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
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
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
// 包，保持分层：L3 不反向引用 L5；由 server 层（L4 编排）映射到
// metrics.RequestEvent。
//
// 直接别名 backend.UpstreamEvent（L3 依赖 L2.5 合法）：字段逐一手抄会在
// 新增字段时静默丢数据，别名让两层永远一致。
type UpstreamEvent = backend.UpstreamEvent

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
	cur := s.holder.Current()
	srcs := cur.OrderedSources()
	newOrder := make([]orderEntry, len(srcs))
	for i, src := range srcs {
		newOrder[i] = orderEntry{name: src.Name, originalIndex: src.OriginalIndex}
	}
	s.ordMu.Lock()
	s.order = newOrder
	s.ordMu.Unlock()
	s.bkMu.Lock()
	valid := map[string]bool{}
	for i := range srcs {
		valid[srcs[i].Name] = true
		// 存活源刷新断路器阈值（保留健康状态）：breaker 持有的是创建时的
		// 快照，不刷新则热改熔断参数对已有源静默无效。
		if b, ok := s.breakers[srcs[i].Name]; ok {
			b.UpdateCfg(cur.BreakerFor(&srcs[i]))
		}
	}
	// 清理已不存在的源的 breaker，避免 map 堆积
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
// holder.Replace 与 Reload 之间存在窗口（configwatch 先 Replace 再回调），
// 此时 order 仍是旧配置的：必须按 name 对齐新配置，不能盲信 originalIndex
// ——删源时索引越界 panic，重排时静默选错源。
func (s *Scheduler) runtimeSeq() []config.Source {
	s.ordMu.RLock()
	defer s.ordMu.RUnlock()
	srcs := s.holder.Current().OrderedSources()
	byName := make(map[string]int, len(srcs))
	for i := range srcs {
		byName[srcs[i].Name] = i
	}
	result := make([]config.Source, 0, len(srcs))
	inOrder := make(map[string]bool, len(s.order))
	for _, entry := range s.order {
		if i, ok := byName[entry.name]; ok {
			result = append(result, srcs[i])
			inOrder[entry.name] = true
		}
	}
	// 新配置新增、order 尚未收录的源按配置顺序补到尾部（Reload 后会归位）。
	for i := range srcs {
		if !inOrder[srcs[i].Name] {
			result = append(result, srcs[i])
		}
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
		ms, err := s.chatBackend.Client.ListModels(ctx, src.BaseURL, src.APIKey, src.Headers)
		if err != nil {
			return nil, err
		}
		out := make([]anthropicclient.ModelInfo, 0, len(ms))
		for _, m := range ms {
			out = append(out, anthropicclient.ModelInfo{ID: m.ID, DisplayName: m.DisplayName})
		}
		return out, nil
	case config.BackendOpenAIResponses:
		ms, err := s.responsesBackend.Client.ListModels(ctx, src.BaseURL, src.APIKey, src.Headers)
		if err != nil {
			return nil, err
		}
		out := make([]anthropicclient.ModelInfo, 0, len(ms))
		for _, m := range ms {
			out = append(out, anthropicclient.ModelInfo{ID: m.ID, DisplayName: m.DisplayName})
		}
		return out, nil
	default:
		return s.client.ListModels(ctx, src.BaseURL, src.APIKey, src.Headers)
	}
}

// waitBackoff sleeps a fixed backoff before the next whole-round retry,
// honoring context cancellation (client disconnect aborts the wait).
func (s *Scheduler) waitBackoff(ctx context.Context, attempt int) error {
	logging.FromContext(ctx).Info("开始退避等待", "attempt", attempt, "wait", s.backoff.String())
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
	log := logging.FromContext(ctx)
	cur := s.holder.Current()
	mr := cur.Breaker.MaxRetries
	start := time.Now()
	attemptNo := 0
	var lastErr error
	var lastSource string
	for attempt := 0; ; attempt++ {
		var firstClientErr clientRejection
		sourceName, success, err := s.tryRoundGeneric(ctx, rawBody, onEvent, onUpstream, &attemptNo, &firstClientErr)
		if sourceName != "" {
			lastSource = sourceName
		}
		if success {
			log.Info("上游请求完成", "source", sourceName, "attempts", attempt+1, "elapsed", time.Since(start).String())
			return sourceName, err
		}
		if err != nil {
			lastErr = err
		}
		if firstClientErr.err != nil {
			// 返回并记录首个 4xx 的源与错误：轮内混合失败（A 400、B 503）时
			// lastErr 是 B 的 503，与「4xx 不重试」的结论对不上号。
			log.Warn("上游源返回 4xx 客户端错误，不重试",
				"source", firstClientErr.source, "elapsed", time.Since(start).String(), "error", firstClientErr.err)
			return firstClientErr.source, firstClientErr.err
		}
		// max_retries 由 config 校验保证 >= 0：mr 表示首轮之外的整轮重试次数。
		if attempt >= mr {
			break
		}
		log.Warn("本轮上游源均失败，等待后重试", "attempt", attempt, "max_retries", mr, "last_error", lastErr)
		if werr := s.waitBackoff(ctx, attempt); werr != nil {
			log.Warn("退避等待被取消", "attempt", attempt, "error", werr)
			return lastSource, werr
		}
	}
	if lastErr != nil {
		log.Error("全部上游源均失败，无可用源", "elapsed", time.Since(start).String(), "last_source", lastSource, "last_error", lastErr)
		return lastSource, fmt.Errorf("%w (last: %v)", ErrAllSourcesFailed, lastErr)
	}
	log.Error("全部上游源均失败，无可用源", "elapsed", time.Since(start).String(), "last_source", lastSource)
	return lastSource, ErrAllSourcesFailed
}

// clientRejection 记录一轮尝试中首个 4xx 拒绝的源与错误，
// 供 ExecuteGeneric 的「4xx 不重试」提前返回使用一致的归因。
type clientRejection struct {
	source string
	err    error
}

func (s *Scheduler) tryRoundGeneric(
	ctx context.Context,
	rawBody []byte,
	onEvent func(model.SSEEvent) error,
	onUpstream OnUpstream,
	attemptNo *int,
	firstClientErr *clientRejection,
) (string, bool, error) {
	log := logging.FromContext(ctx)
	var lastErr error
	var lastSource string

	for _, src := range s.runtimeSeq() {
		if src.Disabled {
			log.Debug("跳过上游源", "source", src.Name, "reason", "disabled")
			continue
		}
		// Auto-recover degraded sources that have exceeded degrade_interval.
		s.autoRecoverDegraded(&src)

		bk := s.breakerFor(&src)
		if !bk.Allow() {
			log.Warn("跳过上游源", "source", src.Name, "reason", "breaker_open")
			continue
		}
		*attemptNo++
		locked, err := s.trySourceGeneric(ctx, &src, bk, rawBody, onEvent, onUpstream, *attemptNo)
		if locked {
			return src.Name, true, err
		}
		if err != nil {
			bt, _ := config.NormalizeBackendType(src.BackendType)
			if backend.IsClientError(err) && firstClientErr != nil && firstClientErr.err == nil {
				*firstClientErr = clientRejection{source: src.Name, err: err}
			}
			lastErr = err
			lastSource = src.Name
			log.Warn("上游源请求失败", "source", src.Name, "backend_type", bt, "attempt", *attemptNo, "error", err)
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
	log := logging.FromContext(ctx).With("source", src.Name, "backend_type", bt, "attempt", attemptNo)
	log.Info("尝试上游源", "endpoint", src.BaseURL)

	locked := false
	wrapEvent := func(ev model.SSEEvent) error {
		if !locked {
			locked = true
			timer.Stop()
			oldState, newState := bk.RecordSuccess()
			s.adjustOrder(src.Name, oldState, newState)
			log.Info("上游源流已锁定", "old_state", oldState, "new_state", newState)
		}
		return onEvent(ev)
	}
	// UpstreamEvent 是 backend.UpstreamEvent 的别名，回调直接透传（nil 时不上报）。
	err := s.backendFor(src).Execute(fbCtx, rawBody, *src, s.holder.Current(), wrapEvent, onUpstream, attemptNo)
	if !locked {
		if ctx.Err() == nil {
			if backend.IsClientError(err) {
				log.Warn("上游源拒绝请求 (4xx)，不降级", "error", err)
			} else {
				oldState, newState := bk.RecordFailure()
				s.adjustOrder(src.Name, oldState, newState)
				log.Warn("上游源失败（未锁定）", "old_state", oldState, "new_state", newState, "error", err)
			}
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
