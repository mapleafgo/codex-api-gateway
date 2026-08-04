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
	if g.IsFlushed() {
		t.Fatal("must not flush before content")
	}
	if err := g.Send(model.SSEEvent{Type: evResponseCreated}); err != nil {
		t.Fatal(err)
	}
	if err := g.Send(model.SSEEvent{Type: evResponseInProgress}); err != nil {
		t.Fatal(err)
	}
	if g.IsFlushed() {
		t.Fatal("status events must not flush the gate")
	}
	if len(got) != 0 {
		t.Fatalf("status events must be buffered, got %v", got)
	}
	if err := g.Send(model.SSEEvent{Type: "response.output_text.delta"}); err != nil {
		t.Fatal(err)
	}
	if !g.IsFlushed() {
		t.Fatal("content event must flush the gate")
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
	if g.IsFlushed() {
		t.Fatal("status+terminal only must not flush")
	}
	if len(got) != 0 {
		t.Fatalf("must not emit any event, got %v", got)
	}
}

// TestEventGate_Flush 强制 flush 用于客户端取消等场景：保持源锁定语义。
func TestEventGate_Flush(t *testing.T) {
	var got []string
	g := NewEventGate(func(ev model.SSEEvent) error {
		got = append(got, ev.Type)
		return nil
	})
	_ = g.Send(model.SSEEvent{Type: evResponseCreated})
	if err := g.Flush(); err != nil {
		t.Fatal(err)
	}
	if !g.IsFlushed() {
		t.Fatal("Flush must set flushed")
	}
	if len(got) != 1 || got[0] != evResponseCreated {
		t.Fatalf("Flush must emit buffered events, got %v", got)
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
	if g.IsFlushed() {
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
	if !g.IsFlushed() {
		t.Fatal("error terminal event must flush the gate")
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
