// Package scheduler routes requests across configured upstream sources.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicclient "github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/breaker"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// ErrAllSourcesFailed is returned when no source could serve the request.
var ErrAllSourcesFailed = errors.New("all upstream sources failed")

// defaultBackoff is the fixed production backoff sequence in seconds.
var defaultBackoff = []time.Duration{
	2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second, 10 * time.Second,
}

// orderEntry tracks a source's runtime position and its original config index.
type orderEntry struct {
	name          string
	originalIndex int
}

// Scheduler routes requests across prioritized sources with failover.
type Scheduler struct {
	cfg      *config.Config
	client   *anthropicclient.Client
	breakers map[string]*breaker.Breaker
	order    []orderEntry // runtimeOrder: runtime priority sequence
	bkMu     sync.Mutex
	ordMu    sync.RWMutex
	backoff  []time.Duration // injectable for tests; defaults to defaultBackoff
}

// RequestBuilder builds a source-specific Anthropic request, plus an optional
// MCPInjection for beta mcp_servers/mcp_toolset injection at the client layer.
type RequestBuilder func(src config.Source) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error)

// New builds a Scheduler.
func New(cfg *config.Config) *Scheduler {
	srcs := cfg.OrderedSources()
	order := make([]orderEntry, len(srcs))
	for i, s := range srcs {
		order[i] = orderEntry{name: s.Name, originalIndex: s.OriginalIndex}
	}
	slog.Info("调度器初始化", "sources", len(order),
		"max_retries", cfg.Breaker.MaxRetries,
		"first_byte_timeout", time.Duration(cfg.Breaker.FirstByteTimeout).String())
	return &Scheduler{
		cfg:      cfg,
		client:   anthropicclient.New(),
		breakers: map[string]*breaker.Breaker{},
		order:    order,
		backoff:  defaultBackoff,
	}
}

func (s *Scheduler) breakerFor(src *config.Source) *breaker.Breaker {
	s.bkMu.Lock()
	defer s.bkMu.Unlock()
	b, ok := s.breakers[src.Name]
	if !ok {
		b = breaker.New(s.cfg.BreakerFor(src))
		s.breakers[src.Name] = b
	}
	return b
}

// runtimeSeq returns sources in the current runtime order (runtimeOrder).
func (s *Scheduler) runtimeSeq() []config.Source {
	s.ordMu.RLock()
	defer s.ordMu.RUnlock()
	srcs := s.cfg.OrderedSources()
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

// waitBackoff sleeps for the backoff duration corresponding to the attempt,
// honoring context cancellation. attempt >= len(backoff) clamps to the last value.
func (s *Scheduler) waitBackoff(ctx context.Context, attempt int) error {
	bk := s.backoff
	if attempt >= len(bk) {
		attempt = len(bk) - 1
	}
	d := bk[attempt]
	slog.Info("开始退避等待", "attempt", attempt, "wait", d.String())
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Execute tries sources by runtime priority with failover, and retries the
// entire round with backoff when all sources fail or are circuit-open.
// mcp 是可选的 MCPInjection；非空时透传给 client.Stream 注入 beta mcp_servers/mcp_toolset。
func (s *Scheduler) Execute(ctx context.Context, req *anthropic.MessageNewParams, mcp *anthropicclient.MCPInjection, onEvent func(*anthropic.MessageStreamEventUnion) error) error {
	_, err := s.ExecutePrepared(ctx, func(_ config.Source) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
		return req, mcp, nil
	}, onEvent)
	return err
}

// ExecutePrepared tries sources by runtime priority, building the request for
// each candidate source immediately before it is attempted. It returns the
// source that locked the stream after the first upstream event.
func (s *Scheduler) ExecutePrepared(ctx context.Context, build RequestBuilder, onEvent func(*anthropic.MessageStreamEventUnion) error) (string, error) {
	mr := s.cfg.Breaker.MaxRetries
	start := time.Now()
	var lastErr error
	for attempt := 0; mr == -1 || attempt <= mr; attempt++ {
		sourceName, success, err := s.tryRoundPrepared(ctx, build, onEvent)
		if success {
			slog.Info("上游请求完成", "source", sourceName, "attempts", attempt+1, "elapsed", time.Since(start).String())
			return sourceName, err // nil for clean success; non-nil for mid-stream error on locked source
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
			return "", werr
		}
	}
	if lastErr != nil {
		slog.Error("全部上游源均失败，无可用源", "attempts", 0, "elapsed", time.Since(start).String(), "last_error", lastErr)
		return "", fmt.Errorf("%w (last: %v)", ErrAllSourcesFailed, lastErr)
	}
	slog.Error("全部上游源均失败，无可用源", "attempts", 0, "elapsed", time.Since(start).String())
	return "", ErrAllSourcesFailed
}

func (s *Scheduler) tryRoundPrepared(ctx context.Context, build RequestBuilder, onEvent func(*anthropic.MessageStreamEventUnion) error) (string, bool, error) {
	var lastErr error
	for _, src := range s.runtimeSeq() {
		bk := s.breakerFor(&src)
		if !bk.Allow() {
			slog.Warn("跳过上游源", "source", src.Name, "reason", "breaker_open")
			continue
		}
		req, mcp, err := build(src)
		if err != nil {
			slog.Warn("构建上游请求失败", "source", src.Name, "error", err)
			return "", false, err
		}
		locked, err := s.trySource(ctx, &src, bk, req, mcp, onEvent)
		if locked {
			return src.Name, true, err // propagate mid-stream error if any
		}
		if err != nil {
			lastErr = err
			slog.Warn("上游源请求失败", "source", src.Name, "model", string(req.Model), "error", err)
		}
	}
	return "", false, lastErr
}

func (s *Scheduler) trySource(ctx context.Context, src *config.Source, bk *breaker.Breaker,
	req *anthropic.MessageNewParams, mcp *anthropicclient.MCPInjection, onEvent func(*anthropic.MessageStreamEventUnion) error) (bool, error) {

	timeout := time.Duration(s.cfg.BreakerFor(src).FirstByteTimeout)
	fbCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	timer := time.AfterFunc(timeout, cancel)
	defer timer.Stop()

	// Resolve the model per the selected source's ModelMap before sending upstream.
	resolvedReq := *req
	resolvedReq.Model = anthropic.Model(ResolveModel(src, string(req.Model)))
	sourceStart := time.Now()
	slog.Info("尝试上游源",
		"source", src.Name,
		"endpoint", src.BaseURL,
		"model", string(req.Model),
		"resolved_model", string(resolvedReq.Model))
	body, err := s.client.Stream(fbCtx, src.BaseURL, src.APIKey, &resolvedReq, mcp)
	if err != nil {
		slog.Warn("上游源建连失败",
			"source", src.Name, "elapsed", time.Since(sourceStart).String(), "error", err)
		if ctx.Err() == nil {
			oldState := bk.State()
			newState := bk.RecordFailure()
			s.adjustOrder(src.Name, oldState, newState)
			slog.Warn("记录上游源失败", "source", src.Name, "old_state", oldState, "new_state", newState, "error", err)
		}
		return false, err
	}
	defer body.Close()

	locked := false
	scanErr := anthropicclient.ScanEvents(body, func(ev *anthropic.MessageStreamEventUnion) error {
		if !locked {
			locked = true
			timer.Stop()
			slog.Info("上游首字节到达", "source", src.Name, "ttfb", time.Since(sourceStart).String())
			oldState := bk.State()
			newState := bk.RecordSuccess()
			s.adjustOrder(src.Name, oldState, newState)
			slog.Info("上游源流已锁定", "source", src.Name, "old_state", oldState, "new_state", newState)
		}
		if err := onEvent(ev); err != nil {
			return err
		}
		return nil
	})
	if !locked {
		if ctx.Err() == nil {
			oldState := bk.State()
			newState := bk.RecordFailure()
			s.adjustOrder(src.Name, oldState, newState)
			slog.Warn("上游源未返回事件", "source", src.Name, "old_state", oldState, "new_state", newState, "error", scanErr)
		}
		if scanErr != nil {
			return false, scanErr
		}
		return false, errors.New("upstream returned no events")
	}
	if scanErr != nil {
		slog.Warn("上游流读取失败（已锁定）", "source", src.Name, "elapsed", time.Since(sourceStart).String(), "error", scanErr)
	}
	return true, scanErr
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

// ListModels 从第一个可用的上游源获取模型列表，返回原始 JSON 响应体。
// 按优先级遍历源，首次成功即返回；全部失败时返回 ErrAllSourcesFailed。
func (s *Scheduler) ListModels(ctx context.Context) (io.ReadCloser, error) {
	start := time.Now()
	var lastErr error
	for _, src := range s.runtimeSeq() {
		bk := s.breakerFor(&src)
		if !bk.Allow() {
			slog.Warn("模型列表跳过上游源", "source", src.Name, "reason", "breaker_open")
			continue
		}
		body, err := s.client.ListModels(ctx, src.BaseURL, src.APIKey)
		if err != nil {
			lastErr = err
			slog.Warn("模型列表上游源失败", "source", src.Name, "error", err)
			continue
		}
		slog.Info("模型列表上游源成功", "source", src.Name, "elapsed", time.Since(start).String())
		return body, nil
	}
	if lastErr != nil {
		slog.Error("模型列表全部上游源失败", "elapsed", time.Since(start).String(), "last_error", lastErr)
		return nil, fmt.Errorf("%w (last: %v)", ErrAllSourcesFailed, lastErr)
	}
	slog.Error("模型列表全部上游源失败（无可尝试源）", "elapsed", time.Since(start).String())
	return nil, ErrAllSourcesFailed
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
