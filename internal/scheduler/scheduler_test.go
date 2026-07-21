package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicclient "github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/breaker"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// --- helpers --------------------------------------------------------------

// makeSource creates a config.Source with an OriginalIndex assigned.
func makeSource(name, baseURL string, idx int) config.Source {
	return config.Source{Name: name, BaseURL: baseURL, OriginalIndex: idx}
}

// goodSSE writes valid Anthropic SSE events to the response writer.
func goodSSE(w http.ResponseWriter) {
	w.Header().Set("content-type", "text/event-stream")
	io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\"}}\n\n")
	io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	w.(http.Flusher).Flush()
}

// err500 returns a 500 status.
func err500(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
}

// testBackoff is a near-zero backoff sequence for fast tests.
var testBackoff = []time.Duration{
	1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond,
}

// --- existing tests (adapted for new config) ------------------------------

func TestFailoverOnUpstreamError(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(err500))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodSSE(w)
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
	var sawStart bool
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error {
			if ev.Type == "message_start" {
				sawStart = true
			}
			return nil
		}, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !sawStart {
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
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("want ErrAllSourcesFailed, got %v", err)
	}
}

func TestLockedSourceNoSwitch(t *testing.T) {
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\"}}\n\n")
		w.(http.Flusher).Flush()
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer flaky.Close()

	var goodCalled atomic.Bool
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalled.Store(true)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{
			makeSource("flaky", flaky.URL, 0),
			makeSource("good", good.URL, 1),
		},
	}
	s := New(cfg)
	var eventCount int
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error {
			eventCount++
			return nil
		}, nil)
	if err == nil {
		t.Fatalf("expected mid-stream error to propagate, got nil")
	}
	if errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("locked source error should propagate directly, not ErrAllSourcesFailed")
	}
	if eventCount == 0 {
		t.Fatalf("should have received at least one event from flaky source")
	}
	if goodCalled.Load() {
		t.Fatalf("good source should NOT be called after lock")
	}
}

func TestSlowFirstByteLongStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		time.Sleep(50 * time.Millisecond)
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\"}}\n\n")
		w.(http.Flusher).Flush()
		for i := 0; i < 5; i++ {
			time.Sleep(40 * time.Millisecond)
			io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(100 * time.Millisecond),
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{
			makeSource("slow", srv.URL, 0),
		},
	}
	s := New(cfg)
	var eventCount int
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error {
			eventCount++
			return nil
		}, nil)
	if err != nil {
		t.Fatalf("expected stream to complete without truncation, got error: %v", err)
	}
	if eventCount < 7 {
		t.Fatalf("expected at least 7 events, got %d", eventCount)
	}
}

func TestModelMapResolvedBeforeStream(t *testing.T) {
	var seenModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		seenModel = body.Model

		goodSSE(w)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1,
		},
		Sources: []config.Source{
			{Name: "up", BaseURL: upstream.URL, OriginalIndex: 0,
				ModelMap: map[string]string{
					"gpt-5": "claude-sonnet-4-20250514",
				}},
		},
	}
	s := New(cfg)
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "gpt-5", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if seenModel != "claude-sonnet-4-20250514" {
		t.Fatalf("upstream should see mapped model %q, got %q", "claude-sonnet-4-20250514", seenModel)
	}
}

// --- new retry + sequence tests (Task 2) ---------------------------------

func TestRetryOnAllFail(t *testing.T) {
	var totalCalls atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalCalls.Add(1)
		err500(w, r)
	}))
	defer bad.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 100, // high so it doesn't degrade within this test
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       2, // initial + 2 retries = 3 rounds
		},
		Sources: []config.Source{makeSource("bad", bad.URL, 0)},
	}
	s := New(cfg)
	s.backoff = testBackoff
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("want ErrAllSourcesFailed, got %v", err)
	}
	// 1 source * 3 rounds (initial + 2 retries) = 3 calls
	if got := totalCalls.Load(); got != 3 {
		t.Fatalf("expected 3 total calls (initial + 2 retries), got %d", got)
	}
}

func TestNoRetryWhenMaxRetriesZero(t *testing.T) {
	var totalCalls atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalCalls.Add(1)
		err500(w, r)
	}))
	defer bad.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 100,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       0, // no retry
		},
		Sources: []config.Source{makeSource("bad", bad.URL, 0)},
	}
	s := New(cfg)
	s.backoff = testBackoff
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("want ErrAllSourcesFailed, got %v", err)
	}
	if got := totalCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call (no retry), got %d", got)
	}
}

func TestRetryCtxCancel(t *testing.T) {
	var totalCalls atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalCalls.Add(1)
		err500(w, r)
	}))
	defer bad.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 100,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       -1, // infinite retry
		},
		Sources: []config.Source{makeSource("bad", bad.URL, 0)},
	}

	// Use a long backoff so the ctx cancel fires during sleep.
	s := New(cfg)
	s.backoff = []time.Duration{10 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first round fails and backoff sleep begins.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := s.Execute(ctx, &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	// Should have had at least 1 call (the first round), but not been stuck.
	if got := totalCalls.Load(); got < 1 {
		t.Fatalf("expected at least 1 call before cancel, got %d", got)
	}
}

func TestDegradeMovesSourceToEnd(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(err500))
	defer bad.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 3, // degrade after 3 consecutive failures
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

	// Send 3 requests: each tries A first (500), failovers to B (success).
	// After 3 failures on A, A is degraded -> moved to end.
	for i := 0; i < 3; i++ {
		err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
			func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
		if err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}

	// Verify runtimeOrder: B should be first, A second.
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
	// Use a flip-flop server that fails for the first 3 calls then succeeds.
	var phase atomic.Int32 // 0=fail, 1=succeed
	flipFlop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 0 {
			err500(w, r)
			return
		}
		goodSSE(w)
	}))
	defer flipFlop.Close()

	bad2 := httptest.NewServer(http.HandlerFunc(err500))
	defer bad2.Close()

	// A has DegradeThreshold=3 (degrades after 3 failures).
	// B has DegradeThreshold=100 (effectively never degrades in this test).
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
			{Name: "A", BaseURL: flipFlop.URL, OriginalIndex: 0,
				Breaker: &config.BreakerCfg{DegradeThreshold: 3}},
			{Name: "B", BaseURL: bad2.URL, OriginalIndex: 1,
				Breaker: &config.BreakerCfg{DegradeThreshold: 100}},
		},
	}
	s := New(cfg)

	// Phase 0: A fails 3 times -> degraded -> moved to end.
	// B also fails each time but never degrades (threshold=100).
	for i := 0; i < 3; i++ {
		s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
			func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
	}

	// Verify A degraded and moved to end.
	s.ordMu.RLock()
	if s.order[0].name != "B" || s.order[1].name != "A" {
		s.ordMu.RUnlock()
		t.Fatalf("after degrade, expected [B, A], got [%s, %s]", s.order[0].name, s.order[1].name)
	}
	s.ordMu.RUnlock()

	// Phase 1: A succeeds. B still fails. Request tries B first (fails), then A (succeeds).
	// A's success transitions degraded->normal, which restores it to originalIndex=0.
	phase.Store(1)
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
	if err != nil {
		t.Fatalf("execute should succeed via A: %v", err)
	}

	// Verify A is restored to position 0.
	s.ordMu.RLock()
	defer s.ordMu.RUnlock()
	if s.order[0].name != "A" {
		t.Fatalf("after recovery, expected A first (originalIndex=0), got %s", s.order[0].name)
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
		goodSSE(w)
	}))
	defer goodCounted.Close()

	// DegradeThreshold=1: first failure -> degraded, second -> circuitOpen.
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

	// Drive A to circuitOpen manually via breaker API.
	bkA := s.breakerFor(&cfg.Sources[0])
	bkA.RecordFailure() // normal -> degraded (degradeCount=1)
	bkA.RecordFailure() // degraded -> circuitOpen (degradeCount=2)
	if bkA.State() != breaker.CircuitOpen {
		t.Fatalf("expected A circuitOpen, got %s", bkA.State())
	}

	// Execute: A should be skipped (Allow()=false), B should serve.
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
	if err != nil {
		t.Fatalf("execute should succeed via B: %v", err)
	}
	if got := aCalls.Load(); got != 0 {
		t.Fatalf("circuitOpen source A should NOT be called, got %d", got)
	}
	if got := bCalls.Load(); got != 1 {
		t.Fatalf("B should be called once, got %d", got)
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
			Cooldown:         config.Duration(3 * time.Millisecond), // short cooldown
			HalfOpenProbes:   1,
			MaxRetries:       3,
		},
		Sources: []config.Source{
			makeSource("A", bad.URL, 0),
			makeSource("B", bad.URL, 1),
		},
	}
	s := New(cfg)
	// Use backoff long enough to exceed cooldown (3ms) so halfOpen transitions occur.
	s.backoff = []time.Duration{
		5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond,
	}

	// Drive both sources to circuitOpen manually.
	bkA := s.breakerFor(&cfg.Sources[0])
	bkB := s.breakerFor(&cfg.Sources[1])
	bkA.RecordFailure() // degraded
	bkA.RecordFailure() // circuitOpen
	bkB.RecordFailure() // degraded
	bkB.RecordFailure() // circuitOpen

	if bkA.State() != breaker.CircuitOpen {
		t.Fatalf("expected A circuitOpen, got %s", bkA.State())
	}
	if bkB.State() != breaker.CircuitOpen {
		t.Fatalf("expected B circuitOpen, got %s", bkB.State())
	}

	// Execute with MaxRetries=3.
	// Round 0: both circuitOpen, Allow()=false, all skipped -> no success.
	// Backoff 5ms > cooldown 3ms -> Allow() transitions to halfOpen.
	// Round 1: Allow() -> halfOpen -> trySource -> 500 -> RecordFailure -> circuitOpen.
	// Backoff 5ms > cooldown 3ms -> Allow() transitions to halfOpen.
	// Round 2: same pattern.
	// Backoff 5ms > cooldown 3ms -> halfOpen.
	// Round 3: same pattern. attempt=3 == mr=3 -> break.
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("want ErrAllSourcesFailed, got %v", err)
	}

	// After cooldown expires, sources go halfOpen and get tried (and fail).
	if got := totalCalls.Load(); got == 0 {
		t.Fatalf("expected some upstream calls after halfOpen transitions, got 0")
	}
}

// TestWatchdogFiresRecordsFailure (I1) verifies the critical watchdog
// timeout -> failover path:
//  1. Source A holds the connection open beyond FirstByteTimeout.
//  2. The watchdog timer (time.AfterFunc) fires, cancelling fbCtx.
//  3. client.Stream returns a context error.
//  4. Since the PARENT ctx is not cancelled (ctx.Err()==nil), RecordFailure
//     is still called on A's breaker.
//  5. Failover to source B, which immediately succeeds.
func TestWatchdogFiresRecordsFailure(t *testing.T) {
	var aCalls atomic.Int64
	// Source A: sleeps well past FirstByteTimeout before responding.
	// The fbCtx cancel from the watchdog aborts the HTTP request client-side.
	slowA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		time.Sleep(500 * time.Millisecond)
		goodSSE(w) // too late — client already gone
	}))
	defer slowA.Close()

	goodB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodSSE(w)
	}))
	defer goodB.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(50 * time.Millisecond),
			DegradeThreshold: 1, // single failure -> degraded (observable)
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
		},
		Sources: []config.Source{
			makeSource("A", slowA.URL, 0),
			makeSource("B", goodB.URL, 1),
		},
	}
	s := New(cfg)

	var sawStart bool
	err := s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
		func(ev *anthropic.MessageStreamEventUnion) error {
			if ev.Type == "message_start" {
				sawStart = true
			}
			return nil
		}, nil)
	if err != nil {
		t.Fatalf("execute should succeed via B after watchdog failover: %v", err)
	}
	if !sawStart {
		t.Fatalf("should have streamed from B after watchdog failover")
	}

	// A was called at least once (the timed-out attempt).
	if got := aCalls.Load(); got < 1 {
		t.Fatalf("A should have been called at least once, got %d", got)
	}

	// A's breaker should have left Normal state, proving RecordFailure was
	// called despite the watchdog (not the parent ctx) cancelling fbCtx.
	bkA := s.breakerFor(&cfg.Sources[0])
	if bkA.State() == breaker.Normal {
		t.Fatalf("A breaker should have left normal state after watchdog timeout, got %s", bkA.State())
	}
}

// TestConcurrentExecuteRuntimeOrderStable (spec S10) verifies that concurrent
// requests do not corrupt runtimeOrder. Multiple goroutines call Execute
// simultaneously against a flip-flop source (triggering degrade/recover state
// transitions) and stable sources. The test asserts no panic, order slice
// length is unchanged, and all sources remain present.
// This test also validates the F1 fix (State()/DegradeCount() locking) under
// the race detector.
func TestConcurrentExecuteRuntimeOrderStable(t *testing.T) {
	// Flip-flop server: alternates between failure and success to trigger
	// breaker state transitions (degrade/recover/circuitOpen/halfOpen).
	var phase atomic.Int64
	flipFlop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Add(1)%2 == 0 {
			err500(w, r)
			return
		}
		goodSSE(w)
	}))
	defer flipFlop.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodSSE(w)
	}))
	defer good.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(2 * time.Second),
			DegradeThreshold: 2,
			RecoverThreshold: 1,
			Cooldown:         config.Duration(5 * time.Millisecond),
			HalfOpenProbes:   1,
			MaxRetries:       0,
		},
		Sources: []config.Source{
			makeSource("A", flipFlop.URL, 0),
			makeSource("B", good.URL, 1),
			makeSource("C", good.URL, 2),
		},
	}
	s := New(cfg)
	s.backoff = testBackoff

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			// Execute may succeed or fail; we only care that it doesn't
			// panic or corrupt shared state.
			_ = s.Execute(context.Background(), &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil,
				func(ev *anthropic.MessageStreamEventUnion) error { return nil }, nil)
		}()
	}
	wg.Wait()

	// Verify runtimeOrder integrity: length unchanged, all sources present.
	s.ordMu.RLock()
	defer s.ordMu.RUnlock()
	if len(s.order) != 3 {
		t.Fatalf("runtimeOrder length changed: expected 3, got %d", len(s.order))
	}
	seen := map[string]bool{}
	for _, e := range s.order {
		seen[e.name] = true
	}
	for _, name := range []string{"A", "B", "C"} {
		if !seen[name] {
			t.Fatalf("source %q missing from runtimeOrder after concurrent execution", name)
		}
	}
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

// TestOnUpstreamUsage 验证 scheduler 从 Anthropic SSE 流中提取 usage 并通过
// OnUpstream 回调上报。观测台的输入/输出 token、缓存创建/命中依赖这一路径，
// 缺失会导致管理页统计恒为 0（曾经的实际问题）。
func TestOnUpstreamUsage(t *testing.T) {
	// 构造一次典型流：message_start 携带 input / cache_* 初值，
	// message_delta 携带累计 output_tokens，message_stop 收尾。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\",\"usage\":{\"input_tokens\":123,\"output_tokens\":0,\"cache_read_input_tokens\":45,\"cache_creation_input_tokens\":6}}}\n\n")
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
	_, err := s.ExecutePrepared(context.Background(),
		func(_ config.Source) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
			return &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil, nil
		},
		func(ev *anthropic.MessageStreamEventUnion) error { return nil },
		func(ev UpstreamEvent) { got = append(got, ev) },
	)
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
	if ev.InputTokens != 123 || ev.OutputTokens != 89 ||
		ev.CacheRead != 45 || ev.CacheCreate != 6 {
		t.Fatalf("usage mismatch: in=%d out=%d cache_read=%d cache_create=%d",
			ev.InputTokens, ev.OutputTokens, ev.CacheRead, ev.CacheCreate)
	}
}

// TestLockedStreamClientCancelNotRecordedAsFailed 验证：源已锁定且上游事件已吐完，
// 随后因客户端断开（ctx cancel）导致读尾失败时，OnUpstream 不得记为 failed。
// 这对应生产日志里「上游流终态 + context canceled」污染失败率的问题。
func TestLockedStreamClientCancelNotRecordedAsFailed(t *testing.T) {
	released := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
		// 故意不关 body：模拟客户端在终态后断开导致读尾 context canceled。
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
		_, err := s.ExecutePrepared(ctx,
			func(_ config.Source) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
				return &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil, nil
			},
			func(ev *anthropic.MessageStreamEventUnion) error {
				events++
				if events >= 2 {
					// 终态事件已处理完，取消客户端上下文。
					cancel()
				}
				return nil
			},
			func(ev UpstreamEvent) { got = append(got, ev) },
		)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled (propagate disconnect), got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for ExecutePrepared")
	}

	if len(got) != 1 {
		t.Fatalf("want 1 upstream event, got %d: %+v", len(got), got)
	}
	// message_stop 已处理：业务完成，应记 completed；不得记 failed。
	if got[0].Status != "completed" {
		t.Fatalf("upstream status = %q, want completed (post-terminal client cancel)", got[0].Status)
	}
	if got[0].Code != 200 {
		t.Fatalf("upstream code = %d, want 200 (stream was established)", got[0].Code)
	}
}

// TestOnUpstreamTTFB 验证锁定成功后 OnUpstream 携带非零 TTFB，
// 且 TTFB 不大于整次 Duration（首字节不可能晚于结束）。
func TestOnUpstreamTTFB(t *testing.T) {
	// 故意延迟首字节，便于断言 TTFB 至少达到该量级。
	delay := 80 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\"}}\n\n")
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
	_, err := s.ExecutePrepared(context.Background(),
		func(_ config.Source) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
			return &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil, nil
		},
		func(ev *anthropic.MessageStreamEventUnion) error { return nil },
		func(ev UpstreamEvent) { got = append(got, ev) },
	)
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
	if ev.TTFB < delay {
		t.Fatalf("TTFB = %v, want >= %v", ev.TTFB, delay)
	}
	if ev.TTFB > ev.Duration {
		t.Fatalf("TTFB %v > Duration %v", ev.TTFB, ev.Duration)
	}
}

// TestOnUpstreamTTFBZeroOnConnectFail 验证建连失败时 TTFB 为 0。
func TestOnUpstreamTTFBZeroOnConnectFail(t *testing.T) {
	// 立刻关闭的 listener 地址：建连失败
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(time.Second),
			DegradeThreshold: 5, Cooldown: config.Duration(time.Minute), HalfOpenProbes: 1},
		Sources: []config.Source{makeSource("bad", "http://"+addr, 0)},
	}
	s := New(cfg)

	var got []UpstreamEvent
	_, err = s.ExecutePrepared(context.Background(),
		func(_ config.Source) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
			return &anthropic.MessageNewParams{Model: "x", MaxTokens: 64}, nil, nil
		},
		func(ev *anthropic.MessageStreamEventUnion) error { return nil },
		func(ev UpstreamEvent) { got = append(got, ev) },
	)
	if err == nil {
		t.Fatal("expected connect error")
	}
	if len(got) == 0 {
		t.Fatal("want at least 1 upstream failed event")
	}
	for _, ev := range got {
		if ev.TTFB != 0 {
			t.Fatalf("failed attempt TTFB = %v, want 0", ev.TTFB)
		}
	}
}
