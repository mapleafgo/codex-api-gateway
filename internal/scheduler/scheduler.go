// Package scheduler routes requests across configured upstream sources.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	anthropicclient "github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/breaker"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// ErrAllSourcesFailed is returned when no source could serve the request.
var ErrAllSourcesFailed = errors.New("all upstream sources failed")

// defaultBackoff is the fixed wait between whole-round retries after all
// sources fail in a round. Not configurable; tests may override Scheduler.backoff.
const defaultBackoff = 10 * time.Second

// orderEntry tracks a source's runtime position and its original config index.
type orderEntry struct {
	name          string
	originalIndex int
}

// Scheduler routes requests across prioritized sources with failover.
type Scheduler struct {
	holder           *config.Holder
	client           *anthropicclient.Client
	anthropicBackend *backend.AnthropicBackend
	chatBackend      *backend.ChatBackend
	responsesBackend *backend.ResponsesBackend
	breakers         map[string]*breaker.Breaker
	order            []orderEntry // runtimeOrder: runtime priority sequence
	bkMu             sync.Mutex
	ordMu            sync.RWMutex
	backoff          time.Duration // injectable for tests; defaults to defaultBackoff
}

// UpstreamEvent 描述一次单源上游尝试的观测数据，由 Scheduler 通过
// OnUpstream 回调上报给观测层（L5 metrics）。Scheduler 自身不依赖 metrics
// 包，保持分层：L3 不反向引用 L5。字段语义对齐 metrics.RequestEvent 的
// upstream 子集，由 server 层（L4 编排）在注入回调时做映射。
type UpstreamEvent struct {
	SourceName    string
	Model         string // 客户端请求的模型（可能为别名）
	ResolvedModel string // 实际发给上游的模型（经 ModelMap 解析）
	StartedAt     time.Time
	Duration      time.Duration
	// TTFB 是从开始尝试到收到第一个 SSE 事件的耗时。
	// 未收到首字节（建连失败/首字节超时）时为 0。
	TTFB         time.Duration
	Status       string // "completed" | "failed"
	Code         int    // 200 成功 / 500 失败
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheCreate  int
	Error        string // 失败原因摘要
	Attempt      int    // 该次尝试在客户端请求内的序号（从 1 开始）
	BackendType  string // a | c | r
}

// OnUpstream 是单次上游尝试结束时的回调。nil 时不上报。
// Scheduler 在 trySource 返回前调用：成功一条 completed，失败一条 failed。
type OnUpstream func(UpstreamEvent)

// SourceHealth 是单源运行时健康快照（管理页展示 / 人工提升）。
type SourceHealth struct {
	Name         string `json:"name"`
	State        string `json:"state"`         // normal | degraded | circuitOpen | halfOpen
	DegradeCount int    `json:"degrade_count"` // 0 normal, 1 degraded, 2 circuitOpen 量级
	Priority     int    `json:"priority"`      // 运行时优先级，1=最高
	Disabled     bool   `json:"disabled"`      // 配置级人工停用，不参与调度
}

// New builds a Scheduler.
// cfg 可为 *config.Config 或 *config.Holder（后者用于热重载场景）。
// 为兼容现有调用，若传入 *Config，内部包装为不可替换的 holder。
func New(cfg any) *Scheduler {
	var holder *config.Holder
	switch c := cfg.(type) {
	case *config.Holder:
		holder = c
	case *config.Config:
		holder = config.NewHolder(c)
	default:
		panic(fmt.Sprintf("scheduler.New: 不支持的 cfg 类型 %T", cfg))
	}
	srcs := holder.Current().OrderedSources()
	order := make([]orderEntry, len(srcs))
	for i, s := range srcs {
		order[i] = orderEntry{name: s.Name, originalIndex: s.OriginalIndex}
	}
	cur := holder.Current()
	slog.Info("调度器初始化", "sources", len(order),
		"max_retries", cur.Breaker.MaxRetries,
		"first_byte_timeout", time.Duration(cur.Breaker.FirstByteTimeout).String())
	return &Scheduler{
		holder:           holder,
		client:           anthropicclient.New(),
		anthropicBackend: backend.NewAnthropic(),
		chatBackend:      backend.NewChat(),
		responsesBackend: backend.NewResponses(),
		breakers:         map[string]*breaker.Breaker{},
		order:            order,
		backoff:          defaultBackoff,
	}
}

// Reload 读取 holder 中最新的 Config，重建运行时优先级顺序。
// 热重载时调用：新配置里的源以配置顺序作为新 order，丢弃运行时调整
// （失败源会被 breaker 重新打回 degraded，自然后移）。
func (s *Scheduler) Reload() {
	srcs := s.holder.Current().OrderedSources()
	newOrder := make([]orderEntry, len(srcs))
	for i, src := range srcs {
		newOrder[i] = orderEntry{name: src.Name, originalIndex: src.OriginalIndex}
	}
	s.ordMu.Lock()
	s.order = newOrder
	s.ordMu.Unlock()
	// 清理已不存在的源的 breaker，避免 map 堆积
	s.bkMu.Lock()
	valid := map[string]bool{}
	for _, src := range srcs {
		valid[src.Name] = true
	}
	for name := range s.breakers {
		if !valid[name] {
			delete(s.breakers, name)
		}
	}
	s.bkMu.Unlock()
	slog.Info("调度器配置已重载", "sources", len(newOrder))
}

// SourceHealth 返回当前配置中各源的运行时健康态，按运行时优先级排序。
func (s *Scheduler) SourceHealth() []SourceHealth {
	seq := s.runtimeSeq()
	out := make([]SourceHealth, 0, len(seq))
	for i, src := range seq {
		src := src
		bk := s.breakerFor(&src)
		out = append(out, SourceHealth{
			Name:         src.Name,
			State:        bk.State().String(),
			DegradeCount: bk.DegradeCount(),
			Priority:     i + 1,
			Disabled:     src.Disabled,
		})
	}
	return out
}

// PromoteSource 手动将指定源提升回 normal，并恢复其配置顺序中的运行时优先级。
func (s *Scheduler) PromoteSource(name string) error {
	src, ok := s.sourceByName(name)
	if !ok {
		return fmt.Errorf("scheduler: 未知源 %q", name)
	}
	bk := s.breakerFor(&src)
	old := bk.State()
	newSt := bk.ForceNormal()
	s.adjustOrder(name, old, newSt)
	slog.Info("上游源手动提升为 normal", "source", name, "old_state", old, "new_state", newSt)
	return nil
}

func (s *Scheduler) breakerFor(src *config.Source) *breaker.Breaker {
	s.bkMu.Lock()
	defer s.bkMu.Unlock()
	b, ok := s.breakers[src.Name]
	if !ok {
		b = breaker.New(s.holder.Current().BreakerFor(src))
		s.breakers[src.Name] = b
	}
	return b
}

// runtimeSeq returns sources in the current runtime order (runtimeOrder).
func (s *Scheduler) runtimeSeq() []config.Source {
	s.ordMu.RLock()
	defer s.ordMu.RUnlock()
	srcs := s.holder.Current().OrderedSources()
	result := make([]config.Source, len(s.order))
	for i, entry := range s.order {
		result[i] = srcs[entry.originalIndex]
	}
	return result
}

// moveToEnd moves the named entry to the end of runtimeOrder.
func (s *Scheduler) moveToEnd(name string) {
	s.ordMu.Lock()
	defer s.ordMu.Unlock()
	for i, entry := range s.order {
		if entry.name == name {
			// Move entry at i to the end, shifting others left.
			tmp := s.order[i]
			copy(s.order[i:], s.order[i+1:])
			s.order[len(s.order)-1] = tmp
			return
		}
	}
}

// restoreOriginal moves the named entry back to its originalIndex position.
func (s *Scheduler) restoreOriginal(name string) {
	s.ordMu.Lock()
	defer s.ordMu.Unlock()
	var entry orderEntry
	found := false
	for i, e := range s.order {
		if e.name == name {
			entry = e
			// Remove from current position.
			copy(s.order[i:], s.order[i+1:])
			s.order = s.order[:len(s.order)-1]
			found = true
			break
		}
	}
	if !found {
		return
	}
	// Insert at originalIndex position.
	pos := entry.originalIndex
	if pos > len(s.order) {
		pos = len(s.order)
	}
	s.order = append(s.order, orderEntry{})
	copy(s.order[pos+1:], s.order[pos:])
	s.order[pos] = entry
}

// sourceByName 在当前配置的源列表中按 name 查找，未找到返回 ok=false。
func (s *Scheduler) sourceByName(name string) (config.Source, bool) {
	for _, src := range s.holder.Current().OrderedSources() {
		if src.Name == name {
			return src, true
		}
	}
	return config.Source{}, false
}

// ListUpstreamModels 拉取指定源的上游模型列表，供管理页编辑模型映射时选用。
// 按 backend_type 分发：a → anthropic ListModels；c/r → Bearer ListModels。
// 返回统一 anthropicclient.ModelInfo 形状供管理页复用。
func (s *Scheduler) ListUpstreamModels(ctx context.Context, sourceName string) ([]anthropicclient.ModelInfo, error) {
	src, ok := s.sourceByName(sourceName)
	if !ok {
		return nil, fmt.Errorf("source %q not found", sourceName)
	}
	bt, err := config.NormalizeBackendType(src.BackendType)
	if err != nil {
		return nil, err
	}
	switch bt {
	case config.BackendOpenAIChat:
		ms, err := s.chatBackend.Client.ListModels(ctx, src.BaseURL, src.APIKey)
		if err != nil {
			return nil, err
		}
		out := make([]anthropicclient.ModelInfo, 0, len(ms))
		for _, m := range ms {
			out = append(out, anthropicclient.ModelInfo{ID: m.ID, DisplayName: m.DisplayName})
		}
		return out, nil
	case config.BackendOpenAIResponses:
		ms, err := s.responsesBackend.Client.ListModels(ctx, src.BaseURL, src.APIKey)
		if err != nil {
			return nil, err
		}
		out := make([]anthropicclient.ModelInfo, 0, len(ms))
		for _, m := range ms {
			out = append(out, anthropicclient.ModelInfo{ID: m.ID, DisplayName: m.DisplayName})
		}
		return out, nil
	default:
		return s.client.ListModels(ctx, src.BaseURL, src.APIKey)
	}
}

// waitBackoff sleeps a fixed backoff before the next whole-round retry,
// honoring context cancellation (client disconnect aborts the wait).
func (s *Scheduler) waitBackoff(ctx context.Context, attempt int) error {
	slog.Info("开始退避等待", "attempt", attempt, "wait", s.backoff.String())
	t := time.NewTimer(s.backoff)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ExecuteGeneric 按 runtime 优先级尝试各源，根据 backend_type 选择 Backend。
// rawBody 为客户端 Responses JSON；onEvent 接收已转换的 Responses SSE。
func (s *Scheduler) ExecuteGeneric(
	ctx context.Context,
	rawBody []byte,
	onEvent func(model.SSEEvent) error,
	onUpstream OnUpstream,
) (string, error) {
	cur := s.holder.Current()
	mr := cur.Breaker.MaxRetries
	start := time.Now()
	attemptNo := 0
	var lastErr error
	var lastSource string
	for attempt := 0; mr == -1 || attempt <= mr; attempt++ {
		sourceName, success, err := s.tryRoundGeneric(ctx, rawBody, onEvent, onUpstream, &attemptNo)
		if sourceName != "" {
			lastSource = sourceName
		}
		if success {
			slog.Info("上游请求完成", "source", sourceName, "attempts", attempt+1, "elapsed", time.Since(start).String())
			return sourceName, err
		}
		if err != nil {
			lastErr = err
		}
		if mr == 0 {
			break
		}
		if mr != -1 && attempt == mr {
			break
		}
		slog.Warn("本轮上游源均失败，等待后重试", "attempt", attempt, "max_retries", mr, "last_error", lastErr)
		if werr := s.waitBackoff(ctx, attempt); werr != nil {
			slog.Warn("退避等待被取消", "attempt", attempt, "error", werr)
			return lastSource, werr
		}
	}
	if lastErr != nil {
		slog.Error("全部上游源均失败，无可用源", "elapsed", time.Since(start).String(), "last_error", lastErr)
		return lastSource, fmt.Errorf("%w (last: %v)", ErrAllSourcesFailed, lastErr)
	}
	slog.Error("全部上游源均失败，无可用源", "elapsed", time.Since(start).String())
	return lastSource, ErrAllSourcesFailed
}

func (s *Scheduler) tryRoundGeneric(
	ctx context.Context,
	rawBody []byte,
	onEvent func(model.SSEEvent) error,
	onUpstream OnUpstream,
	attemptNo *int,
) (string, bool, error) {
	var lastErr error
	var lastSource string

	for _, src := range s.runtimeSeq() {
		if src.Disabled {
			slog.Debug("跳过上游源", "source", src.Name, "reason", "disabled")
			continue
		}
		// Auto-recover degraded sources that have exceeded degrade_interval.
		s.autoRecoverDegraded(&src)

		bk := s.breakerFor(&src)
		if !bk.Allow() {
			slog.Warn("跳过上游源", "source", src.Name, "reason", "breaker_open")
			continue
		}
		*attemptNo++
		locked, err := s.trySourceGeneric(ctx, &src, bk, rawBody, onEvent, onUpstream, *attemptNo)
		if locked {
			return src.Name, true, err
		}
		if err != nil {
			bt, _ := config.NormalizeBackendType(src.BackendType)
			lastErr = err
			lastSource = src.Name
			slog.Warn("上游源请求失败", "source", src.Name, "backend_type", bt, "error", err)
		}
	}

	return lastSource, false, lastErr
}

func (s *Scheduler) backendFor(src *config.Source) backend.Backend {
	bt, err := config.NormalizeBackendType(src.BackendType)
	if err != nil {
		return s.anthropicBackend
	}
	switch bt {
	case config.BackendOpenAIChat:
		return s.chatBackend
	case config.BackendOpenAIResponses:
		return s.responsesBackend
	default:
		return s.anthropicBackend
	}
}

// autoRecoverDegraded checks whether src's breaker has been in Degraded state
// long enough to auto-recover. If so, it adjusts the runtime order accordingly.
func (s *Scheduler) autoRecoverDegraded(src *config.Source) {
	bk := s.breakerFor(src)
	oldSt, newSt, recovered := bk.AutoRecover()
	if recovered {
		s.adjustOrder(src.Name, oldSt, newSt)
		slog.Info("上游源 degrade 超时自动恢复", "source", src.Name, "old_state", oldSt, "new_state", newSt)
	}
}

func (s *Scheduler) trySourceGeneric(
	ctx context.Context,
	src *config.Source,
	bk *breaker.Breaker,
	rawBody []byte,
	onEvent func(model.SSEEvent) error,
	onUpstream OnUpstream,
	attemptNo int,
) (bool, error) {
	timeout := time.Duration(s.holder.Current().BreakerFor(src).FirstByteTimeout)
	fbCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(timeout, cancel)
	defer timer.Stop()

	bt, _ := config.NormalizeBackendType(src.BackendType)
	slog.Info("尝试上游源",
		"source", src.Name,
		"endpoint", src.BaseURL,
		"backend_type", bt)

	locked := false
	wrapEvent := func(ev model.SSEEvent) error {
		if !locked {
			locked = true
			timer.Stop()
			oldState := bk.State()
			newState := bk.RecordSuccess()
			s.adjustOrder(src.Name, oldState, newState)
			slog.Info("上游源流已锁定", "source", src.Name, "backend_type", bt, "old_state", oldState, "new_state", newState)
		}
		return onEvent(ev)
	}
	wrapUpstream := func(ev backend.UpstreamEvent) {
		if onUpstream == nil {
			return
		}
		onUpstream(UpstreamEvent{
			SourceName: ev.SourceName, Model: ev.Model, ResolvedModel: ev.ResolvedModel,
			StartedAt: ev.StartedAt, Duration: ev.Duration, TTFB: ev.TTFB,
			Status: ev.Status, Code: ev.Code,
			InputTokens: ev.InputTokens, OutputTokens: ev.OutputTokens,
			CacheRead: ev.CacheRead, CacheCreate: ev.CacheCreate,
			Error: ev.Error, Attempt: ev.Attempt, BackendType: ev.BackendType,
		})
	}

	err := s.backendFor(src).Execute(fbCtx, rawBody, *src, s.holder.Current(), wrapEvent, wrapUpstream, attemptNo)
	if !locked {
		if ctx.Err() == nil {
			oldState := bk.State()
			newState := bk.RecordFailure()
			s.adjustOrder(src.Name, oldState, newState)
			slog.Warn("上游源失败（未锁定）", "source", src.Name, "backend_type", bt, "old_state", oldState, "new_state", newState, "error", err)
		}
		return false, err
	}
	return true, err
}

// adjustOrder modifies the runtime order based on state transitions.
// Only move/restore when the state actually changes:
//   - degraded/circuitOpen (from a less-degraded state) -> moveToEnd
//   - normal (from degraded/halfOpen recovery)           -> restoreOriginal
func (s *Scheduler) adjustOrder(name string, oldState, newState breaker.State) {
	if newState == oldState {
		return // no transition, no order change
	}
	switch newState {
	case breaker.Degraded, breaker.CircuitOpen:
		s.moveToEnd(name)
		slog.Warn("上游源运行优先级后移", "source", name, "old_state", oldState, "new_state", newState)
	case breaker.Normal:
		s.restoreOriginal(name)
		slog.Info("上游源运行优先级恢复", "source", name, "old_state", oldState, "new_state", newState)
	}
}

// ResolveModel maps a Response model name to the source's actual model.
func ResolveModel(src *config.Source, reqModel string) string {
	if m, ok := src.ModelMap[reqModel]; ok {
		return m
	}
	if src.DefaultModel != "" {
		return src.DefaultModel
	}
	return reqModel
}
