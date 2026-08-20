package backend

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/convert"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// TestStripWebSearchToolsFromParams 验证 a/c 路径按源能力剥除 web_search 工具声明，
// 仅移除 hosted web_search，其余工具原样保留。
func TestStripWebSearchToolsFromParams(t *testing.T) {
	raw := []byte(`{"model":"g","tools":[
		{"type":"function","name":"f1"},
		{"type":"web_search"},
		{"type":"function","name":"f2"}
	]}`)
	req, err := convert.DecodeResponseNewParams(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Tools) != 3 {
		t.Fatalf("初始 tools=%d", len(req.Tools))
	}
	stripWebSearchToolsFromParams(req, slog.Default())
	// 保留 f1/f2 两个 function，剥掉 web_search。
	if len(req.Tools) != 2 {
		t.Fatalf("剥除后 tools=%d, want 2", len(req.Tools))
	}
	for _, tl := range req.Tools {
		if tl.OfWebSearch != nil {
			t.Fatalf("web_search 仍保留: %+v", tl)
		}
	}
}

// TestStripWebSearchToolsFromParams_NoOpWhenEmpty 验证无工具时剥除为空操作。
func TestStripWebSearchToolsFromParams_NoOpWhenEmpty(t *testing.T) {
	req, err := convert.DecodeResponseNewParams([]byte(`{"model":"g"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	stripWebSearchToolsFromParams(req, nil)
	if len(req.Tools) != 0 {
		t.Fatalf("空工具列表应不变, tools=%v", req.Tools)
	}
}

// TestStripWebSearchToolsFromParams_NeutralizesToolChoice 验证剥除 web_search
// 工具时同步清掉指向 hosted web_search 的 tool_choice，避免泄漏给 Chat/Anthropic。
func TestStripWebSearchToolsFromParams_NeutralizesToolChoice(t *testing.T) {
	req, err := convert.DecodeResponseNewParams([]byte(`{
		"model":"g",
		"tools":[{"type":"web_search"}],
		"tool_choice":{"type":"web_search"}
	}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	stripWebSearchToolsFromParams(req, slog.Default())
	if req.ToolChoice.OfHostedTool != nil || req.ToolChoice.OfAllowedTools != nil ||
		req.ToolChoice.OfToolChoiceMode.Valid() {
		t.Fatalf("web_search tool_choice 未清除: %+v", req.ToolChoice)
	}
}

// TestStripWebSearchToolsFromParams_FiltersAllowedTools 验证 allowed_tools 里的
// web_search 条目被过滤，剩余工具仍保留强制选择语义。
func TestStripWebSearchToolsFromParams_FiltersAllowedTools(t *testing.T) {
	req, err := convert.DecodeResponseNewParams([]byte(`{
		"model":"g",
		"tools":[
			{"type":"function","name":"f"},
			{"type":"web_search"}
		],
		"tool_choice":{
			"type":"allowed_tools",
			"mode":"required",
			"tools":[
				{"type":"function","name":"f"},
				{"type":"web_search"}
			]
		}
	}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	stripWebSearchToolsFromParams(req, slog.Default())
	if req.ToolChoice.OfAllowedTools == nil || len(req.ToolChoice.OfAllowedTools.Tools) != 1 {
		t.Fatalf("allowed_tools 未过滤: %+v", req.ToolChoice)
	}
	if typ, _ := req.ToolChoice.OfAllowedTools.Tools[0]["type"].(string); typ != "function" {
		t.Fatalf("allowed_tools 应只剩 function, got %v", req.ToolChoice.OfAllowedTools.Tools)
	}
}

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

// TestIsServerTimeoutCause 验证 WithTimeoutCause 到点（cause=ErrUpstreamTimeout）
// 才能归类为服务端超时；父 ctx 主动取消（首字节超时/客户端断开）不误判。
func TestIsServerTimeoutCause(t *testing.T) {
	timedOut, cancel := context.WithTimeoutCause(context.Background(), 5*time.Millisecond, ErrUpstreamTimeout)
	defer cancel()
	time.Sleep(20 * time.Millisecond)
	if !IsServerTimeout(timedOut, context.DeadlineExceeded) {
		t.Fatal("deadline-fired ctx should be server timeout")
	}
	if IsServerTimeout(context.Background(), context.DeadlineExceeded) {
		t.Fatal("plain background ctx must not be server timeout")
	}
	if IsServerTimeout(timedOut, nil) {
		t.Fatal("nil err must not be server timeout")
	}
	// 父链主动取消（对应首字节超时/客户端断开）：cause 未置位，不算服务端超时。
	parent, cancelParent := context.WithCancel(context.Background())
	child, cancelChild := context.WithTimeoutCause(parent, time.Hour, ErrUpstreamTimeout)
	defer cancelChild()
	cancelParent()
	if IsServerTimeout(child, context.Canceled) {
		t.Fatal("parent cancel must not be classified as server timeout")
	}
}

// TestIsClientCanceledExcludesServerTimeout 验证服务端超时不被当作客户端取消。
func TestIsClientCanceledExcludesServerTimeout(t *testing.T) {
	timedOut, cancel := context.WithTimeoutCause(context.Background(), 5*time.Millisecond, ErrUpstreamTimeout)
	defer cancel()
	time.Sleep(20 * time.Millisecond)
	if IsClientCanceled(timedOut, context.DeadlineExceeded) {
		t.Fatal("server timeout must not be client-canceled")
	}
	// 普通取消仍按既有语义识别。
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if !IsClientCanceled(cancelled, context.Canceled) {
		t.Fatal("client cancel must still be detected")
	}
}

// TestClassifyOutcomeServerTimeout 验证已锁定场景下服务端超时归类为 failed/504。
func TestClassifyOutcomeServerTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), 5*time.Millisecond, ErrUpstreamTimeout)
	defer cancel()
	time.Sleep(20 * time.Millisecond)
	status, code, errText, scanErr := classifyOutcome(ctx, outcomeInput{
		locked:  true,
		scanErr: context.DeadlineExceeded,
	})
	if status != "failed" || code != 504 {
		t.Fatalf("timeout outcome = %s/%d, want failed/504", status, code)
	}
	if !strings.Contains(errText, "upstream request timeout") {
		t.Fatalf("timeout errText = %q", errText)
	}
	if !errors.Is(scanErr, context.DeadlineExceeded) {
		t.Fatalf("scanErr should stay DeadlineExceeded: %v", scanErr)
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
