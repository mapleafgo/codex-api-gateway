package backend

import (
	"errors"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// TestIsClientErrorExcludesRateLimit 429/408 是传输可用性信号，
// 不得按"请求非法"处理（否则限流源永不降级、整轮不重试）。
func TestIsClientErrorExcludesRateLimit(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"upstream 400: bad request", true},
		{"upstream 401: unauthorized", true},
		{"anthropic upstream 404: not found", true},
		{"upstream 429: rate limited", false},
		{"upstream 408: timeout", false},
		{"upstream 500: boom", false},
	}
	for _, tc := range cases {
		if got := IsClientError(errors.New(tc.err)); got != tc.want {
			t.Errorf("IsClientError(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestEventGate_BuffersUntilContent 状态/终态事件先缓冲，首个内容事件触发 flush，
// 且缓冲顺序保持 created → in_progress → content。
func TestEventGate_BuffersUntilContent(t *testing.T) {
	var got []string
	g := NewEventGate(func(ev model.SSEEvent) error {
		got = append(got, ev.Type)
		return nil
	})
	if err := g.Send(model.SSEEvent{Type: evResponseCreated}); err != nil {
		t.Fatal(err)
	}
	if err := g.Send(model.SSEEvent{Type: evResponseInProgress}); err != nil {
		t.Fatal(err)
	}
	if g.HasContent() {
		t.Fatal("status events must not count as content")
	}
	if len(got) != 0 {
		t.Fatalf("status events must be buffered, got %v", got)
	}
	if err := g.Send(model.SSEEvent{Type: "response.output_text.delta"}); err != nil {
		t.Fatal(err)
	}
	if !g.HasContent() {
		t.Fatal("content event must count as content")
	}
	want := []string{evResponseCreated, evResponseInProgress, "response.output_text.delta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v want %v", got, want)
	}
	// flush 后终态事件直发
	if err := g.Send(model.SSEEvent{Type: evResponseCompleted}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[3] != evResponseCompleted {
		t.Fatalf("after flush terminal must pass through, got %v", got)
	}
}

// TestEventGate_NoContentNotFlushed 空响应流（仅状态 + 终态事件）不触发 flush，
// scheduler 据此判定本源无内容、返回 ErrEmptyResponse 触发 failover。
func TestEventGate_NoContentNotFlushed(t *testing.T) {
	var got []string
	g := NewEventGate(func(ev model.SSEEvent) error {
		got = append(got, ev.Type)
		return nil
	})
	for _, et := range []string{evResponseCreated, evResponseInProgress, evResponseCompleted} {
		if err := g.Send(model.SSEEvent{Type: et}); err != nil {
			t.Fatal(err)
		}
	}
	if g.HasContent() {
		t.Fatal("status+terminal only must not count as content")
	}
	if len(got) != 0 {
		t.Fatalf("must not emit any event, got %v", got)
	}
}

// TestEventGate_BufferOverflow 超过缓冲上限的非内容事件返回错误，避免异常上游拖垮内存。
func TestEventGate_BufferOverflow(t *testing.T) {
	g := NewEventGate(func(model.SSEEvent) error { return nil })
	for i := 0; i < maxBufferedEvents; i++ {
		if err := g.Send(model.SSEEvent{Type: evResponseCreated}); err != nil {
			t.Fatalf("buffering event %d: %v", i, err)
		}
	}
	if err := g.Send(model.SSEEvent{Type: evResponseInProgress}); err == nil {
		t.Fatal("must reject event beyond maxBufferedEvents")
	}
}

// TestEventGate_StructuralEventNotContent 输出结构事件（如 output_item.added）不携带内容
// delta，不应触发内容锁定；白名单判定保证新增状态事件默认按非内容缓冲。
func TestEventGate_StructuralEventNotContent(t *testing.T) {
	var got []string
	g := NewEventGate(func(ev model.SSEEvent) error {
		got = append(got, ev.Type)
		return nil
	})
	if err := g.Send(model.SSEEvent{Type: evOutputItemAdded}); err != nil {
		t.Fatal(err)
	}
	if g.HasContent() {
		t.Fatal("output_item.added must not trigger content lock")
	}
	if len(got) != 0 {
		t.Fatalf("structural event must be buffered, got %v", got)
	}
}

// TestEventGate_ErrorEventFlushes 明确错误终态（response.failed）不能作为空响应缓冲，
// 必须先补发已缓冲状态事件，错误必须透传给客户端。
func TestEventGate_ErrorEventFlushes(t *testing.T) {
	var got []string
	g := NewEventGate(func(ev model.SSEEvent) error {
		got = append(got, ev.Type)
		return nil
	})
	_ = g.Send(model.SSEEvent{Type: evResponseCreated})
	if err := g.Send(model.SSEEvent{Type: evResponseFailed}); err != nil {
		t.Fatal(err)
	}
	if g.HasContent() {
		t.Fatal("error terminal event must not count as content")
	}
	if !g.SawTerminalFailure() {
		t.Fatal("error terminal event must mark failure")
	}
	want := []string{evResponseCreated, evResponseFailed}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v want %v", got, want)
	}
}

// TestEventGate_ReasoningSummaryEventsAreContent concise summary 模式的
// response.reasoning_summary_text.delta 也是真实内容事件，不能当作空响应缓冲。
func TestEventGate_ReasoningSummaryEventsAreContent(t *testing.T) {
	var got []string
	g := NewEventGate(func(ev model.SSEEvent) error {
		got = append(got, ev.Type)
		return nil
	})
	_ = g.Send(model.SSEEvent{Type: evResponseCreated})
	if err := g.Send(model.SSEEvent{Type: "response.reasoning_summary_text.delta"}); err != nil {
		t.Fatal(err)
	}
	if !g.HasContent() {
		t.Fatal("reasoning summary delta must count as content")
	}
	if got[0] != evResponseCreated || got[1] != "response.reasoning_summary_text.delta" {
		t.Fatalf("events=%v", got)
	}
}

// TestEventGate_RefusalEventsAreContent refusal 输出是可见内容，不能触发空响应 failover。
func TestEventGate_RefusalEventsAreContent(t *testing.T) {
	g := NewEventGate(func(model.SSEEvent) error { return nil })
	_ = g.Send(model.SSEEvent{Type: "response.refusal.delta"})
	if !g.HasContent() {
		t.Fatal("refusal delta must count as content")
	}
}

// TestEventGate_ToolSearchAddedIsContent tool_search_call 没有专门 delta 事件，
// response.output_item.added 携带完整 arguments，必须算内容，否则合法工具响应会被当空响应丢。
func TestEventGate_ToolSearchAddedIsContent(t *testing.T) {
	g := NewEventGate(func(model.SSEEvent) error { return nil })
	data := `{"type":"response.output_item.added","output_index":0,"item":{"type":"tool_search_call","id":"tsc_1","call_id":"call_1","status":"in_progress","execution":"client"}}`
	if err := g.Send(model.SSEEvent{Type: evOutputItemAdded, Data: []byte(data)}); err != nil {
		t.Fatal(err)
	}
	if !g.HasContent() {
		t.Fatal("tool_search_call output_item.added must count as content")
	}
}
