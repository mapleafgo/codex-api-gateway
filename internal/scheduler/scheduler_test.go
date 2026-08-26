package scheduler

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/assembly"
	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/breaker"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// newTestRegistry 构造内置源插件注册表，供测试调度器按稳定 ID 分发。
func newTestRegistry() *plugin.Registry {
	reg, err := assembly.NewBuiltins()
	if err != nil {
		panic(err)
	}
	return reg
}

// --- helpers --------------------------------------------------------------

func makeSource(name, baseURL string, idx int) config.Source {
	return config.Source{Name: name, BaseURL: baseURL, OriginalIndex: idx, BackendType: config.BackendAnthropic}
}

func makeChatSource(name, baseURL string, idx int) config.Source {
	return config.Source{Name: name, BaseURL: baseURL, OriginalIndex: idx, BackendType: config.BackendOpenAIChat}
}

func makeCopilotSource(name, baseURL string, idx int) config.Source {
	return config.Source{
		Name: name, BaseURL: baseURL, OriginalIndex: idx,
		BackendType: config.BackendGitHubCopilot, GithubToken: "copilot-token",
	}
}

// goodAnthropicSSE writes minimal Anthropic SSE that streamconv can complete.
func goodAnthropicSSE(w http.ResponseWriter) {
	w.Header().Set("content-type", "text/event-stream")
	io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
	io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
	io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n")
	io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	w.(http.Flusher).Flush()
}

func goodChatSSE(w http.ResponseWriter) {
	w.Header().Set("content-type", "text/event-stream")
	io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n")
	io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n")
	io.WriteString(w, "data: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

func err500(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
}

func err400(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`))
}

var testBackoff = 1 * time.Millisecond

const minimalResponsesBody = `{"model":"x","input":"hi","stream":true}`

func runGeneric(s *Scheduler, onEvent func(model.SSEEvent) error, onUp OnUpstream) (string, error) {
	if onEvent == nil {
		onEvent = func(model.SSEEvent) error { return nil }
	}
	return s.ExecuteGeneric(context.Background(), []byte(minimalResponsesBody), onEvent, onUp)
}

func hangingSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// TestPerAttemptTimeoutFailover 验证单笔总时长到点：挂起源按失败计熔断并换源，
// 且到点时间远早于首字节超时（证明总时长不被首个事件/等待语义抵消）。
func TestPerAttemptTimeoutFailover(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(hangingSSE))
	defer hang.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(5 * time.Second),
			RequestTimeout:            config.Duration(300 * time.Millisecond),
			DegradeThreshold:          1,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			CircuitRecoveryThreshold:  1,
		},
		Sources: []config.Source{makeSource("hang", hang.URL, 0), makeSource("good", good.URL, 1)},
	}
	s := New(cfg, newTestRegistry())
	start := time.Now()
	src, err := runGeneric(s, nil, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ExecuteGeneric err: %v", err)
	}
	if src != "good" {
		t.Fatalf("final source = %q, want good", src)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("per-attempt timeout not applied (elapsed %v, first-byte is 5s)", elapsed)
	}
	if got := s.breakerFor(&cfg.Sources[0]).DegradeCount(); got != 1 {
		t.Fatalf("hang source should count a failure, degrade_count=%d", got)
	}
}

// TestPerAttemptTimeoutWrappedError 验证单一挂起源时返回的 error 携带
// ErrUpstreamTimeout 哨兵，供 server 层区分超时与客户端取消。
func TestPerAttemptTimeoutWrappedError(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(hangingSSE))
	defer hang.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(5 * time.Second),
			RequestTimeout:            config.Duration(300 * time.Millisecond),
			DegradeThreshold:          1,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			CircuitRecoveryThreshold:  1,
		},
		Sources: []config.Source{makeSource("hang", hang.URL, 0)},
	}
	s := New(cfg, newTestRegistry())
	_, err := runGeneric(s, nil, nil)
	if !errors.Is(err, backend.ErrUpstreamTimeout) {
		t.Fatalf("err should wrap ErrUpstreamTimeout, got %v", err)
	}
}

func countEventType(evs []model.SSEEvent, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestFailoverOnUpstreamError(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(err500))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{
			makeSource("bad", bad.URL, 0),
			makeSource("good", good.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	var sawCreated bool
	name, err := runGeneric(s, func(ev model.SSEEvent) error {
		if ev.Type == "response.created" {
			sawCreated = true
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if name != "good" {
		t.Fatalf("source=%q want good", name)
	}
	if !sawCreated {
		t.Fatalf("should have streamed from good source after failover")
	}
}

func TestAllSourcesFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(err500))
	defer bad.Close()
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(time.Second),
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1},
		Sources: []config.Source{makeSource("bad", bad.URL, 0)},
	}
	s := New(cfg, newTestRegistry())
	_, err := runGeneric(s, nil, nil)
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("want ErrAllSourcesFailed, got %v", err)
	}
}

func TestMixAnthropicFailThenChatSuccess(t *testing.T) {
	badA := httptest.NewServer(http.HandlerFunc(err500))
	defer badA.Close()
	goodC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("chat path=%s", r.URL.Path)
		}
		goodChatSSE(w)
	}))
	defer goodC.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{
			makeSource("a-bad", badA.URL, 0),
			makeChatSource("c-good", goodC.URL+"/v1", 1),
		},
	}
	s := New(cfg, newTestRegistry())
	var events []model.SSEEvent
	var ups []UpstreamEvent
	name, err := runGeneric(s, func(ev model.SSEEvent) error {
		events = append(events, ev)
		return nil
	}, func(ev UpstreamEvent) { ups = append(ups, ev) })
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if name != "c-good" {
		t.Fatalf("source=%q want c-good", name)
	}
	if countEventType(events, "response.completed") == 0 {
		// print events for debug
		for _, e := range events {
			t.Logf("event=%q data=%s", e.Type, e.Data)
		}
		t.Fatal("expected response.completed from chat backend")
	}
	// last successful upstream should be c
	var sawC bool
	for _, u := range ups {
		if u.SourceName == "c-good" && u.Backend == plugin.BackendOpenAIChat && u.Status == "completed" {
			sawC = true
		}
	}
	if !sawC {
		t.Fatalf("upstream events=%+v", ups)
	}
}

// TestEmptyChatResponseSwitchesToNext 复现 deepseek-v4-flash 空响应：
// 首个 chat 源返回 HTTP 200 + 正常结束但无内容事件（仅 created/in_progress/completed），
// scheduler 统一兜底：不锁定该源，failover 到下一个源。
func TestEmptyChatResponseSwitchesToNext(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-empty\",\"choices\":[]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer empty.Close()

	goodCalled := atomic.Bool{}
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalled.Store(true)
		goodChatSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{
			makeChatSource("c-empty", empty.URL+"/v1", 0),
			makeChatSource("c-good", good.URL+"/v1", 1),
		},
	}
	s := New(cfg, newTestRegistry())
	var events []model.SSEEvent
	name, err := runGeneric(s, func(ev model.SSEEvent) error {
		events = append(events, ev)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !goodCalled.Load() {
		t.Fatal("empty chat response must failover to next source")
	}
	if name != "c-good" {
		t.Fatalf("source=%q want c-good", name)
	}
	if countEventType(events, "response.output_text.delta") == 0 {
		t.Fatalf("client must receive content from second source")
	}
}

// TestFailureTerminalBeforeContentNoFailover 失败终态（response.failed）已写出给客户端后，
// Backend 即便再返回非 nil error 也不得 failover 到下一源：否则客户端会在一个 failed 终态
// 之后又收到第二个源的 created/completed，形成非法双响应流。
func TestFailureTerminalBeforeContentNoFailover(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		// 先开流（合成 created/in_progress），随后坏 JSON 让 ScanEvents 返回错误，
		// ChatBackend 会补发 response.failed 并返回非 nil error。
		io.WriteString(w, "data: {\"id\":\"chatcmpl-fail\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		io.WriteString(w, "data: {\"bad\":\n\n")
		w.(http.Flusher).Flush()
	}))
	defer failing.Close()

	goodCalled := atomic.Bool{}
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalled.Store(true)
		goodChatSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{
			makeChatSource("c-fail", failing.URL+"/v1", 0),
			makeChatSource("c-good", good.URL+"/v1", 1),
		},
	}
	s := New(cfg, newTestRegistry())
	var events []model.SSEEvent
	name, err := runGeneric(s, func(ev model.SSEEvent) error {
		events = append(events, ev)
		return nil
	}, nil)
	if err == nil {
		t.Fatalf("expected error from failing source")
	}
	if name != "c-fail" {
		t.Fatalf("source=%q want c-fail", name)
	}
	if goodCalled.Load() {
		t.Fatal("failed terminal already delivered must not failover")
	}
	if countEventType(events, "response.failed") != 1 {
		t.Fatalf("client must receive exactly one response.failed; events=%+v", events)
	}
}

func TestStatusOnlySourceSwitchesToNext(t *testing.T) {
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		// 仅发 message_start + message_stop（只合成 created/in_progress/completed，
		// 无任何内容事件）后干净结束：空响应应 failover 到下一源。
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer flaky.Close()

	var goodCalled atomic.Bool
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalled.Store(true)
		goodAnthropicSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{
			makeSource("flaky", flaky.URL, 0),
			makeSource("good", good.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	_, _ = runGeneric(s, func(model.SSEEvent) error { return nil }, nil)
	if !goodCalled.Load() {
		t.Fatal("source with only status events must switch to next source")
	}
}

func TestStatusEventStopsFirstByteWatchdog(t *testing.T) {
	slowContent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
		goodAnthropicSSE(w)
	}))
	defer slowContent.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(50 * time.Millisecond), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{makeSource("slow-content", slowContent.URL, 0)},
	}
	s := New(cfg, newTestRegistry())
	name, err := runGeneric(s, nil, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if name != "slow-content" {
		t.Fatalf("source=%q want slow-content", name)
	}
}

func TestSlowFirstByteLongStream(t *testing.T) {
	// first source times out before first byte; second succeeds
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		goodAnthropicSSE(w)
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer fast.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(50 * time.Millisecond), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{
			makeSource("slow", slow.URL, 0),
			makeSource("fast", fast.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	name, err := runGeneric(s, nil, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if name != "fast" {
		t.Fatalf("want fast, got %s", name)
	}
}

func TestModelMapResolvedBeforeStream(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		// crude extract
		if i := strings.Index(string(b), `"model"`); i >= 0 {
			rest := string(b)[i:]
			// "model":"xxx"
			parts := strings.SplitN(rest, `"`, 5)
			if len(parts) >= 4 {
				gotModel = parts[3]
			}
		}
		goodAnthropicSSE(w)
	}))
	defer srv.Close()

	src := makeSource("m", srv.URL, 0)
	src.ModelMap = map[string]string{"x": "mapped-model"}
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(time.Second),
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1},
		Sources: []config.Source{src},
	}
	s := New(cfg, newTestRegistry())
	if _, err := runGeneric(s, nil, nil); err != nil {
		t.Fatal(err)
	}
	if gotModel != "mapped-model" {
		t.Fatalf("upstream model=%q want mapped-model", gotModel)
	}
}

func TestRetryOnAllFail(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			err500(w, r)
			return
		}
		goodAnthropicSSE(w)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(time.Second), MaxRetries: 5,
			DegradeThreshold: 100, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{makeSource("s", srv.URL, 0)},
	}
	s := New(cfg, newTestRegistry())
	s.backoff = testBackoff
	if _, err := runGeneric(s, nil, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if hits.Load() < 3 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestNoRetryWhenMaxRetriesZero(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		err500(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(time.Second), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{makeSource("s", srv.URL, 0)},
	}
	s := New(cfg, newTestRegistry())
	_, err := runGeneric(s, nil, nil)
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("err=%v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d want 1", hits.Load())
	}
}

func TestRetryCtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(err500))
	defer srv.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(time.Second), MaxRetries: -1,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{makeSource("s", srv.URL, 0)},
	}
	s := New(cfg, newTestRegistry())
	s.backoff = time.Hour // long wait so cancel wins
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := s.ExecuteGeneric(ctx, []byte(minimalResponsesBody), func(model.SSEEvent) error { return nil }, nil)
	if err == nil {
		t.Fatal("want cancel error")
	}
}

func TestDegradeMovesSourceToEnd(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(err500))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeThreshold:          3,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			CircuitRecoveryThreshold:  1,
			MaxRetries:                0,
		},
		Sources: []config.Source{
			makeSource("A", bad.URL, 0),
			makeSource("B", good.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	for i := 0; i < 3; i++ {
		if _, err := runGeneric(s, nil, nil); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	s.ordMu.RLock()
	defer s.ordMu.RUnlock()
	if s.order[0].name != "B" {
		t.Fatalf("after degrade, expected B first, got %s", s.order[0].name)
	}
	if s.order[1].name != "A" {
		t.Fatalf("after degrade, expected A second, got %s", s.order[1].name)
	}
}

func TestRecoverRestoresOriginalPosition(t *testing.T) {
	var phase atomic.Int32
	flipFlop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 0 {
			err500(w, r)
			return
		}
		goodAnthropicSSE(w)
	}))
	defer flipFlop.Close()
	bad2 := httptest.NewServer(http.HandlerFunc(err500))
	defer bad2.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeThreshold:          3,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			CircuitRecoveryThreshold:  1,
			MaxRetries:                0,
		},
		Sources: []config.Source{
			{Name: "A", BaseURL: flipFlop.URL, OriginalIndex: 0, BackendType: config.BackendAnthropic,
				Breaker: &config.BreakerCfg{DegradeThreshold: 3}},
			{Name: "B", BaseURL: bad2.URL, OriginalIndex: 1, BackendType: config.BackendAnthropic,
				Breaker: &config.BreakerCfg{DegradeThreshold: 100}},
		},
	}
	s := New(cfg, newTestRegistry())
	for i := 0; i < 3; i++ {
		_, _ = runGeneric(s, nil, nil)
	}
	s.ordMu.RLock()
	if s.order[0].name != "B" || s.order[1].name != "A" {
		s.ordMu.RUnlock()
		t.Fatalf("after degrade, expected [B, A], got [%s, %s]", s.order[0].name, s.order[1].name)
	}
	s.ordMu.RUnlock()

	phase.Store(1)
	if _, err := runGeneric(s, nil, nil); err != nil {
		t.Fatalf("execute should succeed via A: %v", err)
	}
	s.ordMu.RLock()
	defer s.ordMu.RUnlock()
	if s.order[0].name != "A" {
		t.Fatalf("after recovery, expected A first, got %s", s.order[0].name)
	}
}

// TestSourceHealthPrioritySkipsDisabledAndCircuitOpen 验证运行时优先级计数
// 不把 disabled / circuitOpen 源占位，active 源应从 1 开始连续编号。
func TestSourceHealthPrioritySkipsDisabledAndCircuitOpen(t *testing.T) {
	dis := makeSource("disabled", "https://disabled.example", 0)
	dis.Disabled = true
	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:         config.Duration(time.Second),
			DegradeThreshold:         1,
			CircuitInterval:          config.Duration(time.Minute),
			CircuitRecoveryThreshold: 1,
			MaxRetries:               0,
			Recovery:                 "normal",
		},
		Sources: []config.Source{
			dis,
			makeSource("active", "https://active.example", 1),
			makeSource("open", "https://open.example", 2),
		},
	}
	s := New(cfg, newTestRegistry())
	openSrc, ok := s.sourceByName("open")
	if !ok {
		t.Fatal("missing open")
	}
	bk := s.breakerFor(&openSrc)
	bk.RecordFailure() // threshold=1 -> degraded
	bk.RecordFailure() // degraded -> circuitOpen
	s.adjustOrder("open", breaker.Degraded, breaker.CircuitOpen)

	hs := s.SourceHealth()
	if len(hs) != 3 {
		t.Fatalf("health len=%d", len(hs))
	}
	for _, h := range hs {
		if h.Name == "active" && (h.Priority != 1 || h.Disabled) {
			t.Fatalf("active 源应拿到 priority=1，got %+v", h)
		}
		if h.Name == "disabled" && (h.Priority != 0 || !h.Disabled) {
			t.Fatalf("disabled 源不应参与优先级，got %+v", h)
		}
		if h.Name == "open" && h.Priority != 0 {
			t.Fatalf("circuitOpen 源不应参与优先级，got %+v", h)
		}
	}
}

// TestCircuitOpenRemovedFromRuntimeSeq 验证熔断源不进入运行时候选队列。
func TestCircuitOpenRemovedFromRuntimeSeq(t *testing.T) {
	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:         config.Duration(time.Second),
			DegradeThreshold:         1,
			CircuitInterval:          config.Duration(time.Minute),
			CircuitRecoveryThreshold: 1,
			MaxRetries:               0,
			Recovery:                 "normal",
		},
		Sources: []config.Source{
			makeSource("A", "https://a.example", 0),
			makeSource("B", "https://b.example", 1),
		},
	}
	s := New(cfg, newTestRegistry())
	aSrc, _ := s.sourceByName("A")
	bk := s.breakerFor(&aSrc)
	bk.RecordFailure() // -> degraded
	bk.RecordFailure() // -> circuitOpen
	s.adjustOrder("A", breaker.Degraded, breaker.CircuitOpen)

	for _, src := range s.runtimeSeq() {
		if src.Name == "A" {
			t.Fatalf("circuitOpen 源不应出现在 runtimeSeq，got %v", sourceNames(s.runtimeSeq()))
		}
	}
}

// TestHalfOpenTransitionRestoresPriority 验证 circuitOpen 到期进入 halfOpen 时
// 立即回到原始运行优先级，否则在队尾拿不到探测机会。
func TestHalfOpenTransitionRestoresPriority(t *testing.T) {
	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:         config.Duration(time.Second),
			DegradeThreshold:         1,
			CircuitInterval:          config.Duration(time.Minute),
			CircuitRecoveryThreshold: 1,
			MaxRetries:               0,
			Recovery:                 "normal",
		},
		Sources: []config.Source{
			makeSource("A", "https://a.example", 0),
			makeSource("B", "https://b.example", 1),
		},
	}
	s := New(cfg, newTestRegistry())
	s.moveToEnd("A")
	s.adjustOrder("A", breaker.CircuitOpen, breaker.HalfOpen)
	if s.order[0].name != "A" {
		t.Fatalf("halfOpen 应恢复原始位置 [A, B]，got %v", orderNames(s.order))
	}
}

func TestRestoreOriginalUsesCurrentConfigOrder(t *testing.T) {
	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:         config.Duration(time.Second),
			DegradeThreshold:         1,
			CircuitInterval:          config.Duration(time.Minute),
			CircuitRecoveryThreshold: 1,
			MaxRetries:               0,
			Recovery:                 "normal",
		},
		Sources: []config.Source{
			makeSource("A", "https://a.example", 0),
			makeSource("B", "https://b.example", 1),
		},
	}
	s := New(cfg, newTestRegistry())
	aSrc, _ := s.sourceByName("A")
	bk := s.breakerFor(&aSrc)
	bk.RecordFailure() // threshold=1 -> degraded
	s.adjustOrder("A", breaker.Normal, breaker.Degraded)
	if s.order[0].name != "B" {
		t.Fatalf("setup: A 应已后移，got %v", orderNames(s.order))
	}

	// 模拟 configwatch 已 Replace 但尚未 Reload：当前配置顺序是 B 在前。
	cfg2 := &config.Config{
		Breaker: cfg.Breaker,
		Sources: []config.Source{
			makeSource("B", "https://b.example", 0),
			makeSource("A", "https://a.example", 1),
		},
	}
	s.holder.Replace(cfg2)
	s.restoreOriginal("A")

	if s.order[0].name != "B" || s.order[1].name != "A" {
		t.Fatalf("应按当前配置顺序恢复为 [B, A]，got %v", orderNames(s.order))
	}
}

func orderNames(order []orderEntry) []string {
	out := make([]string, len(order))
	for i := range order {
		out[i] = order[i].name
	}
	return out
}

func TestCircuitOpenSourceSkipped(t *testing.T) {
	var aCalls atomic.Int64
	var bCalls atomic.Int64
	badCounted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		err500(w, r)
	}))
	defer badCounted.Close()
	goodCounted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCalls.Add(1)
		goodAnthropicSSE(w)
	}))
	defer goodCounted.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeThreshold:          1,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			CircuitRecoveryThreshold:  1,
			MaxRetries:                0,
		},
		Sources: []config.Source{
			makeSource("A", badCounted.URL, 0),
			makeSource("B", goodCounted.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	bkA := s.breakerFor(&cfg.Sources[0])
	bkA.RecordFailure()
	bkA.RecordFailure()
	if bkA.State() != breaker.CircuitOpen {
		t.Fatalf("expected A circuitOpen, got %s", bkA.State())
	}
	if _, err := runGeneric(s, nil, nil); err != nil {
		t.Fatalf("execute should succeed via B: %v", err)
	}
	if aCalls.Load() != 0 {
		t.Fatalf("circuitOpen source A should NOT be called, got %d", aCalls.Load())
	}
	if bCalls.Load() != 1 {
		t.Fatalf("B should be called once, got %d", bCalls.Load())
	}
}

func TestAllCircuitOpenTriggersRetry(t *testing.T) {
	var totalCalls atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalCalls.Add(1)
		err500(w, r)
	}))
	defer bad.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeThreshold:          1,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(3 * time.Millisecond),
			CircuitRecoveryThreshold:  1,
			MaxRetries:                3,
		},
		Sources: []config.Source{
			makeSource("A", bad.URL, 0),
			makeSource("B", bad.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	s.backoff = 5 * time.Millisecond
	bkA := s.breakerFor(&cfg.Sources[0])
	bkB := s.breakerFor(&cfg.Sources[1])
	bkA.RecordFailure()
	bkA.RecordFailure()
	bkB.RecordFailure()
	bkB.RecordFailure()
	if bkA.State() != breaker.CircuitOpen || bkB.State() != breaker.CircuitOpen {
		t.Fatalf("want both circuitOpen, got A=%s B=%s", bkA.State(), bkB.State())
	}
	_, err := runGeneric(s, nil, nil)
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("want ErrAllSourcesFailed, got %v", err)
	}
	if totalCalls.Load() == 0 {
		t.Fatalf("expected some upstream calls after halfOpen transitions, got 0")
	}
}

func TestWatchdogFiresRecordsFailure(t *testing.T) {
	// Accept connection but never write → first-byte timeout
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// hold connection open without writing
			time.Sleep(200 * time.Millisecond)
			_ = c.Close()
		}
	}()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(30 * time.Millisecond), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{makeSource("hang", "http://"+ln.Addr().String(), 0)},
	}
	s := New(cfg, newTestRegistry())
	var ups []UpstreamEvent
	_, err = runGeneric(s, nil, func(ev UpstreamEvent) { ups = append(ups, ev) })
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("err=%v", err)
	}
	if len(ups) == 0 || ups[0].Status != "failed" {
		t.Fatalf("ups=%+v", ups)
	}
}

func TestConcurrentExecuteRuntimeOrderStable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second), MaxRetries: 0,
			DegradeThreshold: 100, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{
			makeSource("a", srv.URL, 0),
			makeSource("b", srv.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = runGeneric(s, nil, nil)
		}()
	}
	wg.Wait()
}

func TestOnUpstreamUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"usage\":{\"input_tokens\":123,\"output_tokens\":0,\"cache_read_input_tokens\":45,\"cache_creation_input_tokens\":6}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":123,\"output_tokens\":89,\"cache_read_input_tokens\":45,\"cache_creation_input_tokens\":6}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(time.Second),
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1},
		Sources: []config.Source{makeSource("good", srv.URL, 0)},
	}
	s := New(cfg, newTestRegistry())

	var got []UpstreamEvent
	_, err := runGeneric(s, nil, func(ev UpstreamEvent) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 upstream event, got %d", len(got))
	}
	ev := got[0]
	if ev.Status != "completed" {
		t.Fatalf("status: want completed, got %q", ev.Status)
	}
	if ev.Backend != plugin.BackendAnthropic {
		t.Fatalf("backend=%q", ev.Backend)
	}
	if ev.InputTokens != 123 || ev.OutputTokens != 89 ||
		ev.CacheRead != 45 || ev.CacheCreate != 6 {
		t.Fatalf("usage mismatch: in=%d out=%d cache_read=%d cache_create=%d",
			ev.InputTokens, ev.OutputTokens, ev.CacheRead, ev.CacheCreate)
	}
}

func TestLockedStreamClientCancelNotRecordedAsFailed(t *testing.T) {
	released := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
		select {
		case <-released:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(released)

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:         config.Duration(2 * time.Second),
			DegradeThreshold:         100,
			CircuitInterval:          config.Duration(time.Minute),
			CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{makeSource("up", upstream.URL, 0)},
	}
	s := New(cfg, newTestRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	var got []UpstreamEvent
	var events int
	done := make(chan error, 1)
	go func() {
		_, err := s.ExecuteGeneric(ctx, []byte(minimalResponsesBody),
			func(ev model.SSEEvent) error {
				events++
				if events >= 3 {
					cancel()
				}
				return nil
			},
			func(ev UpstreamEvent) { got = append(got, ev) },
		)
		done <- err
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for ExecuteGeneric")
	}
	if len(got) != 1 {
		t.Fatalf("want 1 upstream, got %+v", got)
	}
	if got[0].Status == "failed" {
		t.Fatalf("client cancel must not be failed: %+v", got[0])
	}
}

func TestOnUpstreamTTFB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		goodAnthropicSSE(w)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1},
		Sources: []config.Source{makeSource("good", srv.URL, 0)},
	}
	s := New(cfg, newTestRegistry())
	var got UpstreamEvent
	_, err := runGeneric(s, nil, func(ev UpstreamEvent) { got = ev })
	if err != nil {
		t.Fatal(err)
	}
	if got.TTFB <= 0 {
		t.Fatalf("TTFB=%v want >0", got.TTFB)
	}
}

func TestOnUpstreamTTFBZeroOnConnectFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(err500))
	defer srv.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(time.Second), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1},
		Sources: []config.Source{makeSource("bad", srv.URL, 0)},
	}
	s := New(cfg, newTestRegistry())
	var got UpstreamEvent
	_, _ = runGeneric(s, nil, func(ev UpstreamEvent) { got = ev })
	if got.TTFB != 0 {
		t.Fatalf("TTFB=%v want 0 on connect fail", got.TTFB)
	}
}

func TestSourceHealthAndPromote(t *testing.T) {
	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(time.Second),
			CircuitInterval:           config.Duration(time.Minute),
			DegradeThreshold:          1,
			DegradedRecoveryThreshold: 1,
			CircuitRecoveryThreshold:  1,
			MaxRetries:                0,
			Recovery:                  "normal",
		},
		Sources: []config.Source{
			{Name: "a", BaseURL: "https://a.example", APIKey: "k", DefaultModel: "m"},
			{Name: "b", BaseURL: "https://b.example", APIKey: "k", DefaultModel: "m"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, newTestRegistry())
	// 强制 a 进入 degraded
	srcA, ok := s.sourceByName("a")
	if !ok {
		t.Fatal("missing a")
	}
	bk := s.breakerFor(&srcA)
	bk.RecordFailure() // threshold=1 -> degraded
	if bk.State() != breaker.Degraded {
		t.Fatalf("want degraded, got %v", bk.State())
	}
	s.adjustOrder("a", breaker.Normal, breaker.Degraded)

	hs := s.SourceHealth()
	if len(hs) != 2 {
		t.Fatalf("health len=%d", len(hs))
	}
	var aHealth SourceHealth
	for _, h := range hs {
		if h.Name == "a" {
			aHealth = h
		}
	}
	if aHealth.State != "degraded" || aHealth.DegradeCount != 1 {
		t.Fatalf("a health=%+v", aHealth)
	}

	if err := s.PromoteSource("a"); err != nil {
		t.Fatal(err)
	}
	if bk.State() != breaker.Normal {
		t.Fatalf("after promote state=%v", bk.State())
	}
	hs2 := s.SourceHealth()
	for _, h := range hs2 {
		if h.Name == "a" && h.State != "normal" {
			t.Fatalf("after promote health=%+v", h)
		}
	}
	if err := s.PromoteSource("missing"); err == nil {
		t.Fatal("want error for unknown source")
	}
}

// TestDisabledSourceSkipped 验证 disabled 源不参与调度，且健康快照带 disabled 标记。
func TestDisabledSourceSkipped(t *testing.T) {
	disabledHits := atomic.Int32{}
	disabledSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		disabledHits.Add(1)
		goodAnthropicSSE(w)
	}))
	defer disabledSrv.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer good.Close()

	dis := makeSource("disabled-src", disabledSrv.URL, 0)
	dis.Disabled = true
	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:         config.Duration(2 * time.Second),
			DegradeThreshold:         5,
			CircuitInterval:          config.Duration(time.Minute),
			CircuitRecoveryThreshold: 1,
			MaxRetries:               0,
		},
		Sources: []config.Source{
			dis,
			makeSource("good", good.URL, 1),
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, newTestRegistry())

	hs := s.SourceHealth()
	if len(hs) != 2 {
		t.Fatalf("health len=%d", len(hs))
	}
	var foundDisabled bool
	for _, h := range hs {
		if h.Name == "disabled-src" {
			foundDisabled = true
			if !h.Disabled {
				t.Fatalf("disabled-src health should mark Disabled=true: %+v", h)
			}
		}
	}
	if !foundDisabled {
		t.Fatal("SourceHealth missing disabled-src")
	}

	name, err := runGeneric(s, nil, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if name != "good" {
		t.Fatalf("source=%q want good", name)
	}
	if disabledHits.Load() != 0 {
		t.Fatalf("disabled source was hit %d times", disabledHits.Load())
	}
}

// TestAllSourcesDisabled 验证全部源停用时返回 ErrAllSourcesFailed，且不命中上游。
func TestAllSourcesDisabled(t *testing.T) {
	hits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		goodAnthropicSSE(w)
	}))
	defer srv.Close()

	a := makeSource("a", srv.URL, 0)
	a.Disabled = true
	b := makeSource("b", srv.URL, 1)
	b.Disabled = true
	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:         config.Duration(2 * time.Second),
			DegradeThreshold:         5,
			CircuitInterval:          config.Duration(time.Minute),
			CircuitRecoveryThreshold: 1,
			MaxRetries:               0,
		},
		Sources: []config.Source{a, b},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, newTestRegistry())
	s.backoff = testBackoff

	_, err := runGeneric(s, nil, nil)
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("err=%v want ErrAllSourcesFailed", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("disabled sources were hit %d times", hits.Load())
	}
}

// --- Auto-recover degraded sources in tryRoundGeneric ---

func TestSchedulerAutoRecoverDegradedSource(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeThreshold:          1,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			DegradeInterval:           config.Duration(30 * time.Millisecond),
			CircuitRecoveryThreshold:  1,
			MaxRetries:                0,
			Recovery:                  "normal",
		},
		Sources: []config.Source{
			makeSource("s1", good.URL, 0),
		},
	}
	s := New(cfg, newTestRegistry())

	// Force s1 into degraded
	src, _ := s.sourceByName("s1")
	bk := s.breakerFor(&src)
	bk.RecordFailure() // -> degraded
	if bk.State() != breaker.Degraded {
		t.Fatalf("setup: want degraded, got %v", bk.State())
	}

	// Manually set degradedAt to 50ms ago (past DegradeInterval of 30ms)
	bk.SetDegradedAt(time.Now().Add(-50 * time.Millisecond))

	// ExecuteGeneric should auto-recover in tryRoundGeneric and succeed
	name, err := runGeneric(s, nil, nil)
	if err != nil {
		t.Fatalf("should succeed after auto-recover: %v", err)
	}
	if name != "s1" {
		t.Fatalf("source=%q want s1", name)
	}
	if bk.State() != breaker.Normal {
		t.Fatalf("breaker should be Normal after auto-recover, got %v", bk.State())
	}
}

func TestSchedulerAutoRecoverDegradedBeforeInterval(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeThreshold:          1,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			DegradeInterval:           config.Duration(30 * time.Second),
			CircuitRecoveryThreshold:  1,
			MaxRetries:                0,
			Recovery:                  "normal",
		},
		Sources: []config.Source{
			makeSource("s1", good.URL, 0),
		},
	}
	s := New(cfg, newTestRegistry())

	src, _ := s.sourceByName("s1")
	bk := s.breakerFor(&src)
	bk.RecordFailure() // -> degraded

	// degradedAt is set to now, so AutoRecover should NOT trigger (30s not elapsed)
	_, _, recovered := bk.AutoRecover()
	if recovered {
		t.Fatal("AutoRecover should not recover immediately")
	}
}

// TestSchedulerDegradedSourceNotStarvedByHealthySource 复现：健康源 A 每轮都
// 锁定成功并提前返回，导致排在后位的降级源 B 永远不被遍历、也不会被评估
// degrade 超时自动恢复，从而一直停留在 degraded 再也不会被调用。
// B 语义下，tryRoundGeneric 每轮开始会先整体评估所有源，把 degrade 超时的
// B 恢复到其原始首位，从而在 A 锁定前就重新获得被尝试的机会。
func TestSchedulerDegradedSourceNotStarvedByHealthySource(t *testing.T) {
	goodA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer goodA.Close()
	goodB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer goodB.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeThreshold:          1,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			DegradeInterval:           config.Duration(30 * time.Millisecond),
			CircuitRecoveryThreshold:  1,
			MaxRetries:                0,
			Recovery:                  "normal",
		},
		Sources: []config.Source{
			makeSource("B", goodB.URL, 0), // 原本居首的降级源
			makeSource("A", goodA.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())

	// 强制 B 进入 degraded 并后移，同时让 degrade 间隔已过期。
	srcB, _ := s.sourceByName("B")
	bkB := s.breakerFor(&srcB)
	bkB.RecordFailure() // threshold=1 -> degraded
	if bkB.State() != breaker.Degraded {
		t.Fatalf("setup: B want degraded, got %v", bkB.State())
	}
	bkB.SetDegradedAt(time.Now().Add(-50 * time.Millisecond))
	s.moveToEnd("B") // 降级后移到队尾，A 排在 B 前

	// 轮次开始先评估：B 超时恢复原始首位，被 A 抢占前已重新获得尝试机会并成功转 normal。
	if _, err := runGeneric(s, nil, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if bkB.State() != breaker.Normal {
		t.Fatalf("B should auto-recover to normal after degrade_interval, got %v", bkB.State())
	}
}

// TestDegradedChanceFailuresMoveToEndAndCircuitOpen 覆盖「机会窗口」语义：
// 降级源在 degrade_interval 超时后恢复到原位置（给机会）；机会内失败一次即
// 重新排到队尾等下一次机会；三次机会都失败后触发熔断 circuitOpen。
func TestDegradedChanceFailuresMoveToEndAndCircuitOpen(t *testing.T) {
	badA := httptest.NewServer(http.HandlerFunc(err500))
	defer badA.Close()
	goodB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer goodB.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeThreshold:          3,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			DegradeInterval:           config.Duration(30 * time.Second),
			CircuitRecoveryThreshold:  1,
			MaxRetries:                0,
			Recovery:                  "normal",
		},
		Sources: []config.Source{
			makeSource("A", badA.URL, 0),
			makeSource("B", goodB.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	aSrc, _ := s.sourceByName("A")
	bkA := s.breakerFor(&aSrc)

	// 三次失败：A normal -> degraded，并被移到队尾。
	for i := 0; i < 3; i++ {
		if _, err := runGeneric(s, nil, nil); err != nil {
			t.Fatalf("degrade round %d: %v", i+1, err)
		}
	}
	if bkA.State() != breaker.Degraded {
		t.Fatalf("setup: A want degraded, got %v", bkA.State())
	}
	seq := s.runtimeSeq()
	if seq[0].Name != "B" || seq[1].Name != "A" {
		t.Fatalf("setup: A 应已移到队尾，got %v", sourceNames(seq))
	}

	// 三次机会：每次超时恢复原位置 -> 机会内失败 -> 重新移到队尾。
	for i := 0; i < 3; i++ {
		bkA.SetDegradedAt(time.Now().Add(-time.Minute))
		s.autoRecoverDegraded(&aSrc)
		seq = s.runtimeSeq()
		if seq[0].Name != "A" || seq[1].Name != "B" {
			t.Fatalf("chance %d: A 应恢复到原位置，got %v", i+1, sourceNames(seq))
		}
		if _, err := runGeneric(s, nil, nil); err != nil {
			t.Fatalf("chance %d: %v", i+1, err)
		}
		seq = s.runtimeSeq()
		if i < 2 {
			if seq[0].Name != "B" || seq[1].Name != "A" {
				t.Fatalf("chance %d: 机会失败后 A 应重新移到队尾，got %v", i+1, sourceNames(seq))
			}
		} else if len(seq) != 1 || seq[0].Name != "B" {
			t.Fatalf("chance %d: 熔断后 A 应移出 runtimeSeq，got %v", i+1, sourceNames(seq))
		}
	}

	if bkA.State() != breaker.CircuitOpen {
		t.Fatalf("三次机会失败后应熔断 circuitOpen，got %v", bkA.State())
	}
}

// TestClientErrorDegradesAndChanceFailuresCircuitOpen 覆盖「4xx 也要降级」：
// 4xx 同样计为 breaker 失败（降级/机会失败），三次机会失败后触发熔断；
// 每轮仍由轮内 failover 换到健康源，整轮不重试语义不变。
func TestClientErrorDegradesAndChanceFailuresCircuitOpen(t *testing.T) {
	badA := httptest.NewServer(http.HandlerFunc(err400))
	defer badA.Close()
	goodB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer goodB.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeThreshold:          3,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			DegradeInterval:           config.Duration(30 * time.Second),
			CircuitRecoveryThreshold:  1,
			MaxRetries:                0,
			Recovery:                  "normal",
		},
		Sources: []config.Source{
			makeSource("A", badA.URL, 0),
			makeSource("B", goodB.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	aSrc, _ := s.sourceByName("A")
	bkA := s.breakerFor(&aSrc)

	// 三次 4xx：A normal -> degraded，并被移到队尾；B 每轮 failover 成功。
	for i := 0; i < 3; i++ {
		if _, err := runGeneric(s, nil, nil); err != nil {
			t.Fatalf("degrade round %d: %v", i+1, err)
		}
	}
	if bkA.State() != breaker.Degraded {
		t.Fatalf("4xx 三次后 A 应降级 degraded，got %v", bkA.State())
	}
	seq := s.runtimeSeq()
	if seq[0].Name != "B" || seq[1].Name != "A" {
		t.Fatalf("setup: A 应已移到队尾，got %v", sourceNames(seq))
	}

	// 三次机会，每次超时恢复原位置 -> 4xx 机会失败 -> 重新移到队尾。
	for i := 0; i < 3; i++ {
		bkA.SetDegradedAt(time.Now().Add(-time.Minute))
		s.autoRecoverDegraded(&aSrc)
		seq = s.runtimeSeq()
		if seq[0].Name != "A" || seq[1].Name != "B" {
			t.Fatalf("chance %d: A 应恢复到原位置，got %v", i+1, sourceNames(seq))
		}
		if _, err := runGeneric(s, nil, nil); err != nil {
			t.Fatalf("chance %d: %v", i+1, err)
		}
		seq = s.runtimeSeq()
		if i < 2 {
			if seq[0].Name != "B" || seq[1].Name != "A" {
				t.Fatalf("chance %d: 4xx 机会失败后 A 应重新移到队尾，got %v", i+1, sourceNames(seq))
			}
		} else if len(seq) != 1 || seq[0].Name != "B" {
			t.Fatalf("chance %d: 熔断后 A 应移出 runtimeSeq，got %v", i+1, sourceNames(seq))
		}
	}

	if bkA.State() != breaker.CircuitOpen {
		t.Fatalf("三次 4xx 机会失败后应熔断 circuitOpen，got %v", bkA.State())
	}
}

func TestListUpstreamModels_Responses(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Fatalf("auth=%s", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"}]}`))
	}))
	defer ts.Close()

	cfg := &config.Config{
		Sources: []config.Source{{
			Name: "r1", BaseURL: ts.URL + "/v1", APIKey: "k",
			BackendType: config.BackendOpenAIResponses, OriginalIndex: 0,
		}},
	}
	s := New(cfg, newTestRegistry())
	ms, err := s.ListUpstreamModels(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].ID != "gpt-5" {
		t.Fatalf("models=%+v", ms)
	}
}

func TestListUpstreamModels_CopilotUsesFilteredCatalog(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path=%s, want /models", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer copilot-token" {
			t.Errorf("Authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Editor-Version") == "" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Errorf("missing Copilot headers: %+v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"visible","model_picker_enabled":true,"capabilities":{"type":"chat"}},
			{"id":"hidden","model_picker_enabled":false,"capabilities":{"type":"chat"}},
			{"id":"embedding","model_picker_enabled":true,"capabilities":{"type":"embedding"}},
			{"id":"pending","model_picker_enabled":true,"capabilities":{"type":"chat"},"policy":{"state":"pending"}},
			{"id":"premium","model_picker_enabled":true,"capabilities":{"type":"chat"},"billing":{"restricted_to":["pro_plus"]}}
		]}`))
	}))
	defer ts.Close()

	cfg := &config.Config{Sources: []config.Source{makeCopilotSource("g1", ts.URL, 0)}}
	s := New(cfg, newTestRegistry())
	models, err := s.ListUpstreamModels(context.Background(), "g1")
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(models))
	for _, m := range models {
		got[m.ID] = true
	}
	for _, id := range []string{"visible", "premium"} {
		if !got[id] {
			t.Errorf("model %q missing from %+v", id, models)
		}
	}
	for _, id := range []string{"hidden", "embedding", "pending"} {
		if got[id] {
			t.Errorf("filtered model %q returned in %+v", id, models)
		}
	}
}

func TestCopilotSourceParticipatesInFailover(t *testing.T) {
	badCopilot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"x","model_picker_enabled":true,"supported_endpoints":["/responses"],"capabilities":{"type":"chat"}}]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badCopilot.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodAnthropicSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second), MaxRetries: 0,
			DegradeThreshold: 5, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
		},
		Sources: []config.Source{
			makeCopilotSource("copilot", badCopilot.URL, 0),
			makeSource("good", good.URL, 1),
		},
	}
	s := New(cfg, newTestRegistry())
	name, err := runGeneric(s, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteGeneric: %v", err)
	}
	if name != "good" {
		t.Fatalf("source=%q, want good", name)
	}
}

// TestRuntimeSeqSurvivesReplaceBeforeReload 覆盖 holder.Replace 与 Reload 之间
// 的窗口：旧 order 不得按 originalIndex 盲索引新配置——删源会越界 panic，
// 重排会静默选错源。runtimeSeq 必须按 name 对齐。
func TestRuntimeSeqSurvivesReplaceBeforeReload(t *testing.T) {
	base := config.BreakerCfg{
		FirstByteTimeout: config.Duration(time.Second),
		DegradeThreshold: 3, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
	}
	holder := config.NewHolder(&config.Config{
		Breaker: base,
		Sources: []config.Source{
			makeSource("a", "http://a", 0),
			makeSource("b", "http://b", 1),
			makeSource("c", "http://c", 2),
		},
	})
	s := New(holder, newTestRegistry())

	// 新配置：删掉 a/c，b 挪到 index 0，新增 d。Replace 后暂不 Reload。
	holder.Replace(&config.Config{
		Breaker: base,
		Sources: []config.Source{
			makeSource("b", "http://b", 0),
			makeSource("d", "http://d", 1),
		},
	})

	seq := s.runtimeSeq()
	if len(seq) != 2 || seq[0].Name != "b" || seq[1].Name != "d" {
		t.Fatalf("runtimeSeq 应按 name 对齐并把新源补到尾部，got %+v", seq)
	}
	hs := s.SourceHealth()
	if len(hs) != 2 || hs[0].Name != "b" || hs[1].Name != "d" {
		t.Fatalf("SourceHealth 应只含新配置中的源，got %+v", hs)
	}
}

// TestReloadRefreshesBreakerCfg 热重载后存活源的 breaker 阈值必须即时生效，
// 不得沿用创建时的配置快照。
func TestReloadRefreshesBreakerCfg(t *testing.T) {
	mk := func(threshold int) *config.Config {
		return &config.Config{
			Breaker: config.BreakerCfg{
				FirstByteTimeout: config.Duration(time.Second),
				DegradeThreshold: threshold, CircuitInterval: config.Duration(time.Minute), CircuitRecoveryThreshold: 1,
			},
			Sources: []config.Source{makeSource("a", "http://a", 0)},
		}
	}
	holder := config.NewHolder(mk(5))
	s := New(holder, newTestRegistry())
	src := holder.Current().OrderedSources()[0]
	bk := s.breakerFor(&src)

	holder.Replace(mk(1))
	s.Reload()

	if _, st := bk.RecordFailure(); st != breaker.Degraded {
		t.Fatalf("阈值热更新为 1 后，单次失败应即降级，got %v", st)
	}
}

// TestBackgroundRecoveryRestoresPriority 覆盖「无请求流量时，后台恢复线程仍会
// 在 degrade_interval 超时后把已降级源恢复到原始优先级位置」。
// B 语义：超时只恢复位置、保留 degraded 状态；只有真实请求成功后才转 normal。
func TestBackgroundRecoveryRestoresPriority(t *testing.T) {
	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout:          config.Duration(2 * time.Second),
			DegradeInterval:           config.Duration(30 * time.Second),
			DegradeThreshold:          1,
			DegradedRecoveryThreshold: 1,
			CircuitInterval:           config.Duration(time.Minute),
			CircuitRecoveryThreshold:  1,
		},
		Sources: []config.Source{
			makeSource("A", "http://a", 0),
			makeSource("B", "http://b", 1),
		},
	}
	s := New(cfg, newTestRegistry())
	defer s.StopRecovery()

	// 强制 A 降级并后移（模拟一次上游失败）。
	aSrc := s.holder.Current().OrderedSources()[0]
	bkA := s.breakerFor(&aSrc)
	oldSt, newSt := bkA.RecordFailure() // threshold=1 -> degraded
	s.adjustOrder("A", oldSt, newSt)
	// 把 degrade 计时拨到过去：degrade_interval(30s) 早已超时。
	bkA.SetDegradedAt(time.Now().Add(-time.Minute))

	seq := s.runtimeSeq()
	if len(seq) != 2 || seq[0].Name != "B" || seq[1].Name != "A" {
		t.Fatalf("setup: A 应已被后移，got %v", sourceNames(seq))
	}

	// 用短轮询周期启动后台恢复线程，验证无任何请求时也能恢复优先级。
	s.recoveryPeriod = 10 * time.Millisecond
	s.StartRecovery()

	// 仅靠后台线程：A 位置应恢复到 position 0，但状态仍保持 degraded。
	deadline := time.After(2 * time.Second)
	for {
		cur := s.runtimeSeq()
		if len(cur) == 2 && cur[0].Name == "A" && cur[1].Name == "B" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("后台线程未把 A 恢复到 position 0：state=%v order=%v",
				bkA.State(), sourceNames(cur))
		case <-time.After(10 * time.Millisecond):
		}
	}
	if bkA.State() != breaker.Degraded {
		t.Fatalf("B 语义：超时只恢复位置，状态应保持 degraded，got %v", bkA.State())
	}

	// 只有真实请求成功后才转回 normal。
	bkA.RecordSuccess() // degraded_recovery_threshold=1 -> degraded -> normal
	if bkA.State() != breaker.Normal {
		t.Fatalf("真实成功后才应转 normal，got %v", bkA.State())
	}
}

func sourceNames(seq []config.Source) []string {
	out := make([]string, 0, len(seq))
	for _, s := range seq {
		out = append(out, s.Name)
	}
	return out
}

// TestRecoveryStopIdempotentAndBeforeStart 覆盖 StartRecovery/StopRecovery 的
// 边界顺序：Stop 先于 Start 时不得启动后台线程，多次 Stop 幂等不 panic。
func TestRecoveryStopIdempotentAndBeforeStart(t *testing.T) {
	cfg := &config.Config{
		Sources: []config.Source{makeSource("a", "http://a", 0)},
	}
	s := New(cfg, newTestRegistry())

	s.StopRecovery() // 先停：后续 Start 不得再启动无法停止的 goroutine
	s.recoveryPeriod = 10 * time.Millisecond
	s.StartRecovery()

	s.StopRecovery()
	s.StopRecovery() // 重复 Stop 必须安全
}
