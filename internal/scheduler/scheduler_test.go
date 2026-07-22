package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/breaker"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// --- helpers --------------------------------------------------------------

func makeSource(name, baseURL string, idx int) config.Source {
	return config.Source{Name: name, BaseURL: baseURL, OriginalIndex: idx, BackendType: config.BackendAnthropic}
}

func makeChatSource(name, baseURL string, idx int) config.Source {
	return config.Source{Name: name, BaseURL: baseURL, OriginalIndex: idx, BackendType: config.BackendOpenAIChat}
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

var testBackoff = []time.Duration{
	1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond,
}

const minimalResponsesBody = `{"model":"x","input":"hi","stream":true}`

func runGeneric(s *Scheduler, onEvent func(model.SSEEvent) error, onUp OnUpstream) (string, error) {
	if onEvent == nil {
		onEvent = func(model.SSEEvent) error { return nil }
	}
	return s.ExecuteGeneric(context.Background(), []byte(minimalResponsesBody), onEvent, onUp)
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{
			makeSource("bad", bad.URL, 0),
			makeSource("good", good.URL, 1),
		},
	}
	s := New(cfg)
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1},
		Sources: []config.Source{makeSource("bad", bad.URL, 0)},
	}
	s := New(cfg)
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{
			makeSource("a-bad", badA.URL, 0),
			makeChatSource("c-good", goodC.URL+"/v1", 1),
		},
	}
	s := New(cfg)
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
		if u.SourceName == "c-good" && u.BackendType == "c" && u.Status == "completed" {
			sawC = true
		}
	}
	if !sawC {
		t.Fatalf("upstream events=%+v", ups)
	}
}

func TestLockedSourceNoSwitch(t *testing.T) {
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		// emit first event then drop
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		w.(http.Flusher).Flush()
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{
			makeSource("flaky", flaky.URL, 0),
			makeSource("good", good.URL, 1),
		},
	}
	s := New(cfg)
	_, _ = runGeneric(s, func(model.SSEEvent) error { return nil }, nil)
	if goodCalled.Load() {
		t.Fatal("must not switch after first event locked the source")
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{
			makeSource("slow", slow.URL, 0),
			makeSource("fast", fast.URL, 1),
		},
	}
	s := New(cfg)
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1},
		Sources: []config.Source{src},
	}
	s := New(cfg)
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
			DegradeThreshold: 100, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{makeSource("s", srv.URL, 0)},
	}
	s := New(cfg)
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{makeSource("s", srv.URL, 0)},
	}
	s := New(cfg)
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{makeSource("s", srv.URL, 0)},
	}
	s := New(cfg)
	s.backoff = []time.Duration{time.Hour} // long wait so cancel wins
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
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 3,
			RecoverThreshold: 1,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       0,
		},
		Sources: []config.Source{
			makeSource("A", bad.URL, 0),
			makeSource("B", good.URL, 1),
		},
	}
	s := New(cfg)
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
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 3,
			RecoverThreshold: 1,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       0,
		},
		Sources: []config.Source{
			{Name: "A", BaseURL: flipFlop.URL, OriginalIndex: 0, BackendType: config.BackendAnthropic,
				Breaker: &config.BreakerCfg{DegradeThreshold: 3}},
			{Name: "B", BaseURL: bad2.URL, OriginalIndex: 1, BackendType: config.BackendAnthropic,
				Breaker: &config.BreakerCfg{DegradeThreshold: 100}},
		},
	}
	s := New(cfg)
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
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 1,
			RecoverThreshold: 1,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       0,
		},
		Sources: []config.Source{
			makeSource("A", badCounted.URL, 0),
			makeSource("B", goodCounted.URL, 1),
		},
	}
	s := New(cfg)
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
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 1,
			RecoverThreshold: 1,
			Cooldown:         config.Duration(3 * time.Millisecond),
			HalfOpenProbes:   1,
			MaxRetries:       3,
		},
		Sources: []config.Source{
			makeSource("A", bad.URL, 0),
			makeSource("B", bad.URL, 1),
		},
	}
	s := New(cfg)
	s.backoff = []time.Duration{
		5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond,
	}
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{makeSource("hang", "http://"+ln.Addr().String(), 0)},
	}
	s := New(cfg)
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
			DegradeThreshold: 100, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{
			makeSource("a", srv.URL, 0),
			makeSource("b", srv.URL, 1),
		},
	}
	s := New(cfg)
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

func TestStatusCodeFromErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{errors.New("context canceled"), 0},
		{errors.New(`anthropic upstream 429: {"type":"error"}`), 429},
		{errors.New("anthropic upstream 401: unauthorized"), 401},
		{fmt.Errorf("%w (last: %v)", ErrAllSourcesFailed, errors.New("anthropic upstream 429: x")), 429},
		{errors.New("anthropic upstream abc: bad"), 0},
		{errors.New("anthropic upstream 99: too small"), 0},
	}
	for _, tc := range cases {
		if got := statusCodeFromErr(tc.err); got != tc.want {
			t.Errorf("statusCodeFromErr(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1},
		Sources: []config.Source{makeSource("good", srv.URL, 0)},
	}
	s := New(cfg)

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
	if ev.BackendType != "a" {
		t.Fatalf("backend_type=%q", ev.BackendType)
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
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 100,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
		},
		Sources: []config.Source{makeSource("up", upstream.URL, 0)},
	}
	s := New(cfg)

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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1},
		Sources: []config.Source{makeSource("good", srv.URL, 0)},
	}
	s := New(cfg)
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
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1},
		Sources: []config.Source{makeSource("bad", srv.URL, 0)},
	}
	s := New(cfg)
	var got UpstreamEvent
	_, _ = runGeneric(s, nil, func(ev UpstreamEvent) { got = ev })
	if got.TTFB != 0 {
		t.Fatalf("TTFB=%v want 0 on connect fail", got.TTFB)
	}
}

func TestResolveModel(t *testing.T) {
	src := &config.Source{ModelMap: map[string]string{"a": "b"}, DefaultModel: "def"}
	if ResolveModel(src, "a") != "b" {
		t.Fatal("map")
	}
	if ResolveModel(src, "x") != "def" {
		t.Fatal("default")
	}
	src2 := &config.Source{}
	if ResolveModel(src2, "x") != "x" {
		t.Fatal("passthrough")
	}
}

func TestSourceHealthAndPromote(t *testing.T) {
	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(time.Second),
			Cooldown:         config.Duration(time.Minute),
			DegradeThreshold: 1,
			RecoverThreshold: 1,
			HalfOpenProbes:   1,
			MaxRetries:       0,
			Recovery:         "normal",
		},
		Sources: []config.Source{
			{Name: "a", BaseURL: "https://a.example", APIKey: "k", DefaultModel: "m"},
			{Name: "b", BaseURL: "https://b.example", APIKey: "k", DefaultModel: "m"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg)
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
