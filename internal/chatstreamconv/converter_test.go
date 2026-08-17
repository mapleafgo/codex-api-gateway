package chatstreamconv

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
)

func evTypes(t *testing.T, data []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	typ, _ := m["type"].(string)
	return typ
}

// evTypeIs 判断 SSE 事件类型是否为 want。
// 本环境 Go 工具链（nodwarf5）对部分字符串字面量常量合并会损坏静态字节，
// 直接 `typ == "..."` 可能误判；通过运行时构造同一字符串可绕过损坏常量。
func evTypeIs(typ, want string) bool {
	return typ == string([]byte(want))
}

func TestTextStream(t *testing.T) {
	c := New()
	c.SetClientModel("gpt-4o")
	var all []string
	evs, err := c.Feed([]byte(`{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"He"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		all = append(all, evTypes(t, e.Data))
	}
	evs, _ = c.Feed([]byte(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"llo"}}]}`))
	for _, e := range evs {
		all = append(all, evTypes(t, e.Data))
	}
	evs, _ = c.Feed([]byte(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	for _, e := range evs {
		all = append(all, evTypes(t, e.Data))
	}
	has := func(want string) bool {
		for _, s := range all {
			if s == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{
		"response.created",
		"response.output_text.delta",
		"response.completed",
	} {
		if !has(want) {
			t.Fatalf("missing %s in %v", want, all)
		}
	}
	if !c.Done() {
		t.Fatal("expected Done")
	}
	if u := c.Usage(); u == nil || u.InputTokens != 3 || u.OutputTokens != 2 {
		t.Fatalf("usage=%+v", u)
	}
	if c.RespID() != "chatcmpl-1" {
		t.Fatalf("respID=%q", c.RespID())
	}
}

// TestMessageItemAddedContentNotNull 锁定 c 路径 message item 在
// output_item.added 事件中的 content 字段必须是数组（非 null）。
// nil content 序列化为 "content":null 会让 Codex serde 反序列化失败，
// active_item 不被设置，表现为 "OutputTextDelta without active item"。
func TestMessageItemAddedContentNotNull(t *testing.T) {
	c := New()
	c.SetClientModel("gpt-4o")
	evs, err := c.Feed([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if !evTypeIs(evTypes(t, e.Data), "response.output_item.added") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Data, &m); err != nil {
			t.Fatalf("unmarshal output_item.added: %v: %s", err, e.Data)
		}
		item, _ := m["item"].(map[string]any)
		if item == nil {
			t.Fatalf("output_item.added missing item: %s", e.Data)
		}
		arr, ok := item["content"].([]any)
		if !ok {
			t.Fatalf("content must be a JSON array (got %T): %s", item["content"], e.Data)
		}
		if strings.Contains(string(e.Data), `"content":null`) {
			t.Fatalf("content marshalled as null: %s", e.Data)
		}
		if len(arr) != 0 {
			t.Fatalf("content want empty [] at added, got %v: %s", arr, e.Data)
		}
		return
	}
	t.Fatalf("no output_item.added event found in %d events", len(evs))
}

func TestEmptyChoicesUsageChunk(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	if _, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"content":"x"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Feed([]byte(`{"id":"c1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)); err != nil {
		t.Fatalf("empty choices should not fail: %v", err)
	}
	c.FeedDone()
	if u := c.Usage(); u == nil || u.TotalTokens != 2 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestToolCallStream(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var types []string
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			types = append(types, evTypes(t, e.Data))
		}
	}
	for _, e := range c.FeedDone() {
		types = append(types, evTypes(t, e.Data))
	}
	wantAny := []string{
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.completed",
	}
	for _, w := range wantAny {
		found := false
		for _, typ := range types {
			if typ == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %s in %v", w, types)
		}
	}
	if c.StopReason() != "tool_calls" {
		t.Fatalf("stop=%q", c.StopReason())
	}
	items := c.OutputItems()
	var foundFC bool
	for _, it := range items {
		if it.Type == "function_call" {
			foundFC = true
			if it.Name != "get_weather" {
				t.Fatalf("name=%q", it.Name)
			}
			if it.Arguments != `{"city":"Paris"}` {
				t.Fatalf("args=%q", it.Arguments)
			}
		}
	}
	if !foundFC {
		t.Fatalf("items=%+v", items)
	}
}

func TestShellCallItemStream(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var all []model.SSEEvent
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_s","type":"function","function":{"name":"shell","arguments":""}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"input\":\"ls\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	all = append(all, c.FeedDone()...)
	for _, e := range all {
		typ := evTypes(t, e.Data)
		if typ == "response.function_call_arguments.delta" {
			t.Fatalf("shell must not emit function arguments delta")
		}
	}
	item := doneItemFromEvents(t, all, "custom_tool_call")
	if item["name"] != "shell" || item["input"] != "ls" {
		t.Fatalf("shell custom_tool_call name=%v input=%v want shell/ls", item["name"], item["input"])
	}
}

// doneItemFromEvents 从 Feed/FeedDone 事件流中取出 output_item.done 的 item 并断言 type。
func doneItemFromEvents(t *testing.T, events []model.SSEEvent, wantType string) map[string]any {
	t.Helper()
	for _, e := range events {
		if evTypes(t, e.Data) != "response.output_item.done" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Data, &m); err != nil {
			t.Fatalf("unmarshal done: %v", err)
		}
		item, _ := m["item"].(map[string]any)
		if item != nil && item["type"] == wantType {
			return item
		}
	}
	t.Fatalf("no output_item.done with type %s, events=%v", wantType, events)
	return nil
}

func TestLocalShellCallItemStream(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var all []model.SSEEvent
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ls","type":"function","function":{"name":"local_shell","arguments":"{\"input\":\"pwd\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	all = append(all, c.FeedDone()...)
	item := doneItemFromEvents(t, all, "custom_tool_call")
	if item["name"] != "local_shell" || item["input"] != "pwd" {
		t.Fatalf("local_shell custom_tool_call name=%v input=%v want local_shell/pwd", item["name"], item["input"])
	}
}

func TestApplyPatchCallItemStream(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var all []model.SSEEvent
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ap","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** Add File: a.txt\\n+hi\\n*** End Patch\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	all = append(all, c.FeedDone()...)
	item := doneItemFromEvents(t, all, "custom_tool_call")
	wantInput := "*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch"
	if item["name"] != "apply_patch" || item["input"] != wantInput {
		t.Fatalf("apply_patch custom_tool_call name=%v input=%v", item["name"], item["input"])
	}
}

func TestToolSearchArgumentsSanitized(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var all []model.SSEEvent
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ts","type":"function","function":{"name":"tool_search","arguments":"{\"queries\":[\"1.0\"]}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	all = append(all, c.FeedDone()...)
	item := doneItemFromEvents(t, all, "tool_search_call")
	args, _ := item["arguments"].(string)
	if strings.Contains(args, "1.0") {
		t.Fatalf("tool_search arguments not sanitized: %q", args)
	}
}

func TestChatCustomToolInputStream(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	c.SetFreeformNames(map[string]struct{}{"parse": {}})
	var types []string
	var deltas []string
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_c","type":"custom","custom":{"name":"parse"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"custom":{"input":"hel"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"custom":{"input":"lo"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			types = append(types, evTypes(t, e.Data))
			var ev struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal(e.Data, &ev); err != nil {
				t.Fatal(err)
			}
			if evTypeIs(ev.Type, evCustomToolCallInputDelta) {
				deltas = append(deltas, ev.Delta)
			}
		}
	}
	for _, e := range c.FeedDone() {
		types = append(types, evTypes(t, e.Data))
	}
	if strings.Join(deltas, "") != "hello" {
		t.Fatalf("custom deltas=%v want hello", deltas)
	}
	hasDone := false
	for _, typ := range types {
		if evTypeIs(typ, evCustomToolCallInputDone) {
			hasDone = true
		}
		if evTypeIs(typ, evFunctionCallArgumentsDelta) {
			t.Fatalf("custom must not emit function arguments delta: %v", types)
		}
	}
	if !hasDone {
		t.Fatalf("missing custom input done: %v", types)
	}
	var found bool
	for _, it := range c.OutputItems() {
		if it.Type == "custom_tool_call" && it.Name == "parse" && it.Input == "hello" {
			found = true
		}
	}
	if !found {
		t.Fatalf("items=%+v", c.OutputItems())
	}
}

func TestChatCustomToolInputSingleChunk(t *testing.T) {
	c := New()
	c.SetFreeformNames(map[string]struct{}{"parse": {}})
	raw := `{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"custom","custom":{"name":"parse","input":"abc"}}]},"finish_reason":"tool_calls"}]}`
	evs, err := c.Feed([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range append(evs, c.FeedDone()...) {
		var ev struct {
			Type  string `json:"type"`
			Input string `json:"input"`
		}
		if err := json.Unmarshal(e.Data, &ev); err != nil {
			t.Fatal(err)
		}
		if evTypeIs(ev.Type, evCustomToolCallInputDone) && ev.Input == "abc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing custom input done with input abc")
	}
}

func TestFinishReasonLengthIncomplete(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var types []string
	evs, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"content":"partial"},"finish_reason":"length"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		types = append(types, evTypes(t, e.Data))
	}
	for _, e := range c.FeedDone() {
		types = append(types, evTypes(t, e.Data))
	}
	found := false
	for _, typ := range types {
		if typ == "response.incomplete" {
			found = true
		}
		if typ == "response.completed" {
			t.Fatal("length must not complete")
		}
	}
	if !found {
		t.Fatalf("want incomplete, got %v", types)
	}
}

// TestFinishReasonFunctionCallMapsToCompleted 兼容上游把工具终态写作 function_call。
func TestFinishReasonFunctionCallMapsToCompleted(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var types []string
	evs, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		types = append(types, evTypes(t, e.Data))
	}
	evs, err = c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"function_call"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		types = append(types, evTypes(t, e.Data))
	}
	for _, e := range c.FeedDone() {
		types = append(types, evTypes(t, e.Data))
	}
	if !c.Done() {
		t.Fatalf("expected Done, got %v", types)
	}
	if c.Usage() == nil || c.Usage().OutputTokens != 2 {
		t.Fatalf("usage not propagated: %v", c.Usage())
	}
}

func TestFinishReasonContentFilter(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var types []string
	evs, _ := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"content_filter"}]}`))
	for _, e := range evs {
		types = append(types, evTypes(t, e.Data))
	}
	for _, e := range c.FeedDone() {
		types = append(types, evTypes(t, e.Data))
	}
	found := false
	for _, typ := range types {
		if typ == "response.incomplete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want incomplete for content_filter, got %v", types)
	}
}

func TestFeedDoneWithoutFinishReason(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"content":"hi"}}]}`))
	evs := c.FeedDone()
	var hasCompleted bool
	for _, e := range evs {
		if evTypes(t, e.Data) == "response.completed" {
			hasCompleted = true
		}
	}
	if !hasCompleted {
		t.Fatalf("expected completed from FeedDone, got %d events", len(evs))
	}
	if !c.Done() {
		t.Fatal("expected Done after FeedDone")
	}
}

// TestUsageChunkAfterFinishReason 覆盖官方流顺序：
// finish_reason 包 → 空 choices 的 usage 末包 → [DONE]。
// include_usage 时 usage 在 finish 之后；终态 response 与 Converter.Usage 都必须带上 token。
func TestUsageChunkAfterFinishReason(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var all []string
	feed := func(raw string) {
		t.Helper()
		evs, err := c.Feed([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			all = append(all, evTypes(t, e.Data))
		}
	}
	feed(`{"id":"c1","choices":[{"delta":{"content":"hi"}}]}`)
	feed(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`)
	// 官方：finish 后才来空 choices + usage
	feed(`{"id":"c1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`)
	doneEvs := c.FeedDone()
	for _, e := range doneEvs {
		all = append(all, evTypes(t, e.Data))
	}
	if u := c.Usage(); u == nil || u.InputTokens != 10 || u.OutputTokens != 4 || u.TotalTokens != 14 {
		t.Fatalf("Usage after late usage chunk: %+v", u)
	}
	// 终态事件应携带 usage（在 FeedDone 发出，或 usage 包触发补全后的终态）
	var terminal string
	var terminalUsage any
	scan := func(evs interface { /* */
	}) {
	}
	_ = scan
	// re-feed from scratch to inspect terminal payload
	c2 := New()
	c2.SetClientModel("m")
	var terminalData []byte
	for _, raw := range []string{
		`{"id":"c1","choices":[{"delta":{"content":"hi"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`,
	} {
		evs, err := c2.Feed([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			typ := evTypes(t, e.Data)
			if typ == "response.completed" || typ == "response.incomplete" {
				terminalData = e.Data
			}
		}
	}
	for _, e := range c2.FeedDone() {
		typ := evTypes(t, e.Data)
		if typ == "response.completed" || typ == "response.incomplete" {
			terminalData = e.Data
		}
	}
	if terminalData == nil {
		t.Fatalf("no terminal event, types so far from first run: %v", all)
	}
	var m map[string]any
	if err := json.Unmarshal(terminalData, &m); err != nil {
		t.Fatal(err)
	}
	resp, _ := m["response"].(map[string]any)
	if resp == nil {
		t.Fatalf("no response in terminal: %s", terminalData)
	}
	u, _ := resp["usage"].(map[string]any)
	if u == nil {
		t.Fatalf("terminal response missing usage: %s", terminalData)
	}
	if int(u["input_tokens"].(float64)) != 10 || int(u["output_tokens"].(float64)) != 4 {
		t.Fatalf("terminal usage=%v", u)
	}
	_ = terminal
	_ = terminalUsage
}

// TestChoiceUsageFallback 兼容 Moonshot 等把 usage 放在 choice 上的兼容端点。
func TestChoiceUsageFallback(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	evs, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"content":"hi"},"finish_reason":"stop","usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var terminal []byte
	for _, e := range evs {
		if typ := evTypes(t, e.Data); typ == "response.completed" || typ == "response.incomplete" {
			terminal = e.Data
		}
	}
	for _, e := range c.FeedDone() {
		if typ := evTypes(t, e.Data); typ == "response.completed" || typ == "response.incomplete" {
			terminal = e.Data
		}
	}
	if u := c.Usage(); u == nil || u.InputTokens != 7 || u.OutputTokens != 3 || u.TotalTokens != 10 {
		t.Fatalf("choice usage not mapped: %+v", u)
	}
	if terminal == nil {
		t.Fatalf("want terminal event, got %d events", len(evs))
	}
}

func TestContentFilterEmitsRefusalChain(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var types []string
	feed := func(raw string) {
		t.Helper()
		evs, err := c.Feed([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			types = append(types, evTypes(t, e.Data))
		}
	}
	feed(`{"id":"c1","choices":[{"delta":{"refusal":"I cannot help with that."}}]}`)
	feed(`{"id":"c1","choices":[{"delta":{},"finish_reason":"content_filter"}]}`)
	for _, e := range c.FeedDone() {
		types = append(types, evTypes(t, e.Data))
	}
	want := []string{
		"response.refusal.delta",
		"response.refusal.done",
		"response.incomplete",
	}
	for _, w := range want {
		found := false
		for _, typ := range types {
			if typ == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %s in %v", w, types)
		}
	}
	var hasRefusalItem bool
	for _, it := range c.OutputItems() {
		if it.Type == "message" {
			for _, part := range it.Content {
				if part.Type == "refusal" {
					hasRefusalItem = true
					if part.Refusal == nil || *part.Refusal == "" {
						t.Fatalf("empty refusal text: %+v", part)
					}
				}
			}
		}
	}
	if !hasRefusalItem {
		t.Fatalf("output items missing refusal: %+v", c.OutputItems())
	}
}

func TestContentFilterFallbackRefusalText(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"content_filter"}]}`))
	c.FeedDone()
	var text string
	for _, it := range c.OutputItems() {
		for _, part := range it.Content {
			if part.Type == "refusal" && part.Refusal != nil {
				text = *part.Refusal
			}
		}
	}
	if text == "" {
		t.Fatalf("expected fallback refusal text, items=%+v", c.OutputItems())
	}
}

func TestWebSearchOutboundShape(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var types []string
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"ws1","type":"function","function":{"name":"web_search","arguments":""}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"go\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			types = append(types, evTypes(t, e.Data))
		}
	}
	for _, e := range c.FeedDone() {
		types = append(types, evTypes(t, e.Data))
	}
	for _, w := range []string{"response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed"} {
		ok := false
		for _, typ := range types {
			if typ == w {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("missing %s in %v", w, types)
		}
	}
	for _, it := range c.OutputItems() {
		if it.Type == "web_search_call" {
			if it.Action == nil || it.Action.Query != "go" {
				t.Fatalf("item=%+v", it)
			}
			return
		}
	}
	t.Fatalf("no web_search_call item: %+v", c.OutputItems())
}

func TestMCPOutboundShape(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var types []string
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"m1","type":"function","function":{"name":"mcp__fetch__get","arguments":"{\"x\":1}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		evs, _ := c.Feed([]byte(ch))
		for _, e := range evs {
			types = append(types, evTypes(t, e.Data))
		}
	}
	for _, e := range c.FeedDone() {
		types = append(types, evTypes(t, e.Data))
	}
	// Chat 无 server MCP 执行：mcp__* 必须回成 function_call 让客户端执行。
	// 旧逻辑发已 completed 的空 mcp_call，客户端不会调工具。
	for _, w := range []string{"response.function_call_arguments.delta", "response.function_call_arguments.done"} {
		ok := false
		for _, typ := range types {
			if typ == w {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("missing %s in %v", w, types)
		}
	}
	for _, it := range c.OutputItems() {
		if it.Type == "function_call" {
			if it.Namespace != "mcp__fetch" || it.Name != "get" || it.CallID != "m1" {
				t.Fatalf("item=%+v", it)
			}
			return
		}
	}
	t.Fatalf("no function_call item %+v", c.OutputItems())
}

func TestDeclaredNameOverridesSplit(t *testing.T) {
	c := New()
	c.SetDeclaredNames(map[string]toolcatalog.Identity{
		"a__b":      {OpenAIType: "function", Name: "a__b"},
		"ns__parse": {OpenAIType: "custom", Namespace: "ns", Name: "parse", Freeform: true},
	})
	c.SetFreeformNames(map[string]struct{}{"ns__parse": {}})
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"m1","type":"function","function":{"name":"a__b","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":1,"id":"m2","type":"custom","custom":{"name":"ns__parse","input":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		_, _ = c.Feed([]byte(ch))
	}
	_ = c.FeedDone()
	got := map[string]model.OutputItem{}
	for _, it := range c.OutputItems() {
		got[it.Type+"|"+it.Name] = it
	}
	fn, ok := got["function_call|a__b"]
	if !ok || fn.Namespace != "" {
		t.Fatalf("declared plain name must not be split: %+v", got)
	}
	ct, ok := got["custom_tool_call|parse"]
	if !ok || ct.Namespace != "ns" {
		t.Fatalf("namespaced custom must resolve from declaration: %+v", got)
	}
}

func TestToolCallNameArrivesAfterID(t *testing.T) {
	// 兼容上游常见分片：先 id，后 name/arguments。
	// 仅有 id 时若立即 open，会按空 name 误判 function_call，output_item.added 类型错误。
	c := New()
	c.SetClientModel("m")
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_s","type":"function","function":{}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"shell","arguments":"{\"input\":\"pwd\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var addedTypes []string
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if evTypes(t, e.Data) != "response.output_item.added" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatal(err)
			}
			item, _ := m["item"].(map[string]any)
			typ, _ := item["type"].(string)
			addedTypes = append(addedTypes, typ)
		}
	}
	for _, e := range c.FeedDone() {
		_ = e
	}
	if len(addedTypes) != 1 || addedTypes[0] != "custom_tool_call" {
		t.Fatalf("output_item.added types=%v want [custom_tool_call]", addedTypes)
	}
	var found bool
	for _, it := range c.OutputItems() {
		if it.Type == "custom_tool_call" && it.CallID == "call_s" {
			found = true
		}
		if it.Type == "function_call" {
			t.Fatalf("unexpected function_call item: %+v", it)
		}
	}
	if !found {
		t.Fatalf("items=%+v", c.OutputItems())
	}
}

func TestDeltaContentArrayTextParts(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	// content 为数组时须拼 text，不能 Feed 失败
	evs, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"role":"assistant","content":[{"type":"text","text":"He"},{"type":"text","text":"llo"}]}}]}`))
	if err != nil {
		t.Fatalf("array content should parse: %v", err)
	}
	evs2, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = evs
	_ = evs2
	for _, e := range c.FeedDone() {
		_ = e
	}
	var text string
	for _, it := range c.OutputItems() {
		if it.Type == "message" {
			for _, p := range it.Content {
				if p.Type == "output_text" {
					text += p.Text
				}
			}
		}
	}
	if text != "Hello" {
		t.Fatalf("text=%q items=%+v", text, c.OutputItems())
	}
}

func TestUsageCachedTokensMapped(t *testing.T) {
	c := New()
	// finish + usage 同包，含 prompt_tokens_details / completion_tokens_details
	raw := `{"id":"c1","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":20}}}`
	if _, err := c.Feed([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	c.FeedDone()
	u := c.Usage()
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.InputTokens != 100 || u.OutputTokens != 50 || u.TotalTokens != 150 {
		t.Fatalf("base usage=%+v", u)
	}
	if u.CacheReadInputTokens != 80 {
		t.Fatalf("CacheReadInputTokens=%d want 80", u.CacheReadInputTokens)
	}
	if u.InputTokensDetails == nil || u.InputTokensDetails.CachedTokens != 80 {
		t.Fatalf("InputTokensDetails=%+v", u.InputTokensDetails)
	}
	if u.OutputTokensDetails == nil || u.OutputTokensDetails.ReasoningTokens != 20 {
		t.Fatalf("OutputTokensDetails=%+v", u.OutputTokensDetails)
	}
}

func TestUsageDeepSeekCacheTokensMapped(t *testing.T) {
	c := New()
	raw := `{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}`
	if _, err := c.Feed([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	u := c.Usage()
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.CacheReadInputTokens != 80 {
		t.Fatalf("CacheReadInputTokens=%d want 80", u.CacheReadInputTokens)
	}
	if u.InputTokensDetails == nil || u.InputTokensDetails.CachedTokens != 80 {
		t.Fatalf("InputTokensDetails=%+v", u.InputTokensDetails)
	}
}

func TestUsageTotalTokensFallbackWhenMissing(t *testing.T) {
	c := New()
	raw := `{"id":"c1","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":80}}}`
	if _, err := c.Feed([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	u := c.Usage()
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.InputTokens != 120 || u.OutputTokens != 30 || u.TotalTokens != 150 {
		t.Fatalf("usage=%+v want input 120 output 30 total 150", u)
	}
}

func TestUsageDeepSeekCacheHitSurvivesEmptyDetails(t *testing.T) {
	c := New()
	raw := `{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_cache_hit_tokens":80,"prompt_tokens_details":{}}}`
	if _, err := c.Feed([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if got := c.Usage().CacheReadInputTokens; got != 80 {
		t.Fatalf("CacheReadInputTokens=%d want 80", got)
	}
}

func TestUsageDetailsCachedTokensOverrideDeepSeekCacheHit(t *testing.T) {
	c := New()
	raw := `{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_cache_hit_tokens":80,"prompt_tokens_details":{"cached_tokens":60}}}`
	if _, err := c.Feed([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if got := c.Usage().CacheReadInputTokens; got != 60 {
		t.Fatalf("CacheReadInputTokens=%d want 60", got)
	}
}

func TestUsageCacheWriteTokensMapped(t *testing.T) {
	c := New()
	raw := `{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":30}}}`
	if _, err := c.Feed([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	u := c.Usage()
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.CacheCreationInputTokens != 30 {
		t.Fatalf("CacheCreationInputTokens=%d want 30", u.CacheCreationInputTokens)
	}
	if u.InputTokensDetails == nil || u.InputTokensDetails.CacheWriteTokens != 30 {
		t.Fatalf("InputTokensDetails=%+v", u.InputTokensDetails)
	}
}

func TestLogprobsOnTextDelta(t *testing.T) {
	c := New()
	evs, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"role":"assistant","content":"Hi"},"logprobs":{"content":[{"token":"Hi","logprob":-0.1,"top_logprobs":[{"token":"Hi","logprob":-0.1},{"token":"Hello","logprob":-1.2}]}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, e := range evs {
		joined += string(e.Data)
	}
	if !strings.Contains(joined, `"logprobs"`) || !strings.Contains(joined, `"token":"Hi"`) {
		t.Fatalf("delta should carry logprobs: %s", joined)
	}
	evs, err = c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	joined = ""
	for _, e := range evs {
		joined += string(e.Data)
	}
	if !strings.Contains(joined, "response.output_text.done") {
		t.Fatalf("expect done events: %s", joined)
	}
	if !strings.Contains(joined, `"logprobs"`) {
		t.Fatalf("done should carry accumulated logprobs: %s", joined)
	}
}

func TestLogprobsOnlyChunkAccumulates(t *testing.T) {
	c := New()
	// first: content without logprobs
	if _, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"role":"assistant","content":"Hi"}}]}`)); err != nil {
		t.Fatal(err)
	}
	// second: logprobs only (empty content)
	evs, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"logprobs":{"content":[{"token":"Hi","logprob":-0.2}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, e := range evs {
		joined += string(e.Data)
	}
	if !strings.Contains(joined, `"logprobs"`) {
		t.Fatalf("logprobs-only chunk should emit delta with logprobs: %s", joined)
	}
	evs, err = c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	joined = ""
	for _, e := range evs {
		joined += string(e.Data)
	}
	if !strings.Contains(joined, "response.output_text.done") || !strings.Contains(joined, `"logprobs"`) {
		t.Fatalf("done should keep accumulated logprobs: %s", joined)
	}
}

func TestCodeInterpreterOutboundFallsBackToFunctionCall(t *testing.T) {
	c := New()
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"ci1","type":"function","function":{"name":"code_interpreter","arguments":""}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"code\":\"print(1)\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var joined string
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			joined += string(e.Data)
		}
	}
	if !strings.Contains(joined, "function_call") {
		t.Fatalf("want function_call fallback events: %s", joined)
	}
	if strings.Contains(joined, "code_interpreter_call") {
		t.Fatalf("must not emit code_interpreter_call: %s", joined)
	}
	if !strings.Contains(joined, "print(1)") {
		t.Fatalf("want args in events: %s", joined)
	}
}

func TestToolSearchOutboundShape(t *testing.T) {
	c := New()
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"ts1","type":"function","function":{"name":"tool_search","arguments":""}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var joined string
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			joined += string(e.Data)
		}
	}
	if !strings.Contains(joined, "tool_search_call") {
		t.Fatalf("want tool_search_call: %s", joined)
	}
}

func TestMultiIndexParallelToolCalls(t *testing.T) {
	c := New()
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"fa","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"fb","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var joined string
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			joined += string(e.Data)
		}
	}
	if !strings.Contains(joined, `"name":"fa"`) || !strings.Contains(joined, `"name":"fb"`) {
		t.Fatalf("want both tools: %s", joined)
	}
	// two function_call items
	if strings.Count(joined, `"type":"function_call"`) < 2 {
		t.Fatalf("want >=2 function_call items: %s", joined)
	}
}

func TestFailEmitsResponseFailed(t *testing.T) {
	c := New()
	_, _ = c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"role":"assistant","content":"x"}}]}`))
	evs := c.Fail("upstream reset")
	joined := ""
	for _, e := range evs {
		joined += string(e.Data)
	}
	if !strings.Contains(joined, "response.failed") {
		t.Fatalf("want failed: %s", joined)
	}
	if !c.Failed() || !c.Done() {
		t.Fatal("converter should be failed+done")
	}
}

// TestReasoningContentBeforeText 验证 DeepSeek 等厂商的 delta.reasoning_content
// 先于 content 出现时，映射为 Responses reasoning item + reasoning_text 事件链。
func TestReasoningContentBeforeText(t *testing.T) {
	c := New()
	c.SetClientModel("deepseek-reasoner")
	var types []string
	var joined string
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"role":"assistant","reasoning_content":"先想"}}]}`,
		`{"id":"c1","choices":[{"delta":{"reasoning_content":"一步"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"答案"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			types = append(types, evTypes(t, e.Data))
			joined += string(e.Data)
		}
	}
	has := func(want string) bool {
		for _, s := range types {
			if s == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{
		"response.output_item.added",
		"response.reasoning_text.delta",
		"response.reasoning_text.done",
		"response.output_item.done",
		"response.output_text.delta",
		"response.completed",
	} {
		if !has(want) {
			t.Fatalf("missing %s in %v", want, types)
		}
	}
	if !strings.Contains(joined, `"type":"reasoning"`) {
		t.Fatalf("want reasoning item: %s", joined)
	}
	if !strings.Contains(joined, `"type":"summary_text"`) || !strings.Contains(joined, "先想一步") {
		t.Fatalf("want summary_text with full reasoning: %s", joined)
	}
	// reasoning 应先于 message 文本
	rsDelta := -1
	textDelta := -1
	for i, s := range types {
		if s == "response.reasoning_text.delta" && rsDelta < 0 {
			rsDelta = i
		}
		if s == "response.output_text.delta" && textDelta < 0 {
			textDelta = i
		}
	}
	if rsDelta < 0 || textDelta < 0 || rsDelta >= textDelta {
		t.Fatalf("reasoning delta should precede text delta: rs=%d text=%d types=%v", rsDelta, textDelta, types)
	}
}

// TestThinkTagReasoningExtraction 验证 content 中的字面 <think>...</think> 思维链
// 被抽取为 reasoning item（与 reasoning_content 同构），且不泄漏进 output_text。
func TestThinkTagReasoningExtraction(t *testing.T) {
	c := New()
	c.SetClientModel("deepseek-v4-flash")
	var types []string
	var outText, reasonText string
	collect := func(evs []model.SSEEvent) {
		for _, e := range evs {
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatal(err)
			}
			typ, _ := m["type"].(string)
			types = append(types, typ)
			switch typ {
			case "response.output_text.delta":
				outText += m["delta"].(string)
			case "response.reasoning_text.delta":
				reasonText += m["delta"].(string)
			}
		}
	}
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"role":"assistant","content":"<think>先想一步</think>答案是 42"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		collect(evs)
	}
	if !strings.Contains(reasonText, "先想一步") {
		t.Fatalf("want reasoning text from <think>, got %q", reasonText)
	}
	if strings.Contains(reasonText, "答案是 42") {
		t.Fatalf("answer must not leak into reasoning: %q", reasonText)
	}
	if !strings.Contains(outText, "答案是 42") {
		t.Fatalf("want answer in output_text, got %q", outText)
	}
	if strings.Contains(outText, "先想一步") || strings.Contains(outText, "<think>") || strings.Contains(outText, "</think>") {
		t.Fatalf("think block must not leak into output_text: %q", outText)
	}
	rsIdx, txIdx := -1, -1
	for i, s := range types {
		if s == "response.reasoning_text.delta" && rsIdx < 0 {
			rsIdx = i
		}
		if s == "response.output_text.delta" && txIdx < 0 {
			txIdx = i
		}
	}
	if rsIdx < 0 || txIdx < 0 || rsIdx >= txIdx {
		t.Fatalf("reasoning delta must precede text delta: rs=%d tx=%d", rsIdx, txIdx)
	}
}

// TestThinkTagSplitAcrossChunks 验证被 chunk 边界截断的 <think>/</think> 标签
// 仍能正确拼回：思考内容进 reasoning，标签外答案进 output_text。
func TestThinkTagSplitAcrossChunks(t *testing.T) {
	c := New()
	c.SetClientModel("deepseek-v4-flash")
	var outText, reasonText string
	collect := func(evs []model.SSEEvent) {
		for _, e := range evs {
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatal(err)
			}
			switch m["type"].(string) {
			case "response.output_text.delta":
				outText += m["delta"].(string)
			case "response.reasoning_text.delta":
				reasonText += m["delta"].(string)
			}
		}
	}
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"content":"<thi"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"nk>rea"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"son"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"ing<"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"/think>a"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"nswer"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		collect(evs)
	}
	if reasonText != "reasoning" {
		t.Fatalf("want reasoning='reasoning', got %q", reasonText)
	}
	if outText != "answer" {
		t.Fatalf("want output_text='answer', got %q", outText)
	}
}

// TestThinkTagWithLeadingText 验证 正文 + <think> + 正文 三段顺序：
// 首段正文先作为 message，思考作为 reasoning，末段正文作为新的 message。
func TestThinkTagWithLeadingText(t *testing.T) {
	c := New()
	c.SetClientModel("deepseek-v4-flash")
	var outText, reasonText string
	collect := func(evs []model.SSEEvent) {
		for _, e := range evs {
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatal(err)
			}
			switch m["type"].(string) {
			case "response.output_text.delta":
				outText += m["delta"].(string)
			case "response.reasoning_text.delta":
				reasonText += m["delta"].(string)
			}
		}
	}
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"content":"before<think>mid</think>after"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		collect(evs)
	}
	if reasonText != "mid" {
		t.Fatalf("want reasoning='mid', got %q", reasonText)
	}
	if outText != "beforeafter" {
		t.Fatalf("want output_text='beforeafter', got %q", outText)
	}
}

// TestThinkTagCloseTagWithTrailingSpace 验证闭合标签 '>' 前的空白（如 </think >）
// 被容忍：思考内容仍进 reasoning，标签之后的正文进 output_text，不丢内容。
func TestThinkTagCloseTagWithTrailingSpace(t *testing.T) {
	c := New()
	c.SetClientModel("deepseek-v4-flash")
	var outText, reasonText string
	collect := func(evs []model.SSEEvent) {
		for _, e := range evs {
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatal(err)
			}
			switch m["type"].(string) {
			case "response.output_text.delta":
				outText += m["delta"].(string)
			case "response.reasoning_text.delta":
				reasonText += m["delta"].(string)
			}
		}
	}
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"content":"<think>reason</think >answer"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		collect(evs)
	}
	if reasonText != "reason" {
		t.Fatalf("want reasoning='reason', got %q", reasonText)
	}
	if outText != "answer" {
		t.Fatalf("want output_text='answer', got %q", outText)
	}
}

// TestThinkTagTruncatedOpenTagAtEnd 验证流在开标签中途截断时，残留的半个标签
// 被丢弃而非泄漏进 output_text。
func TestThinkTagTruncatedOpenTagAtEnd(t *testing.T) {
	c := New()
	c.SetClientModel("deepseek-v4-flash")
	var outText string
	collect := func(evs []model.SSEEvent) {
		for _, e := range evs {
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatal(err)
			}
			if m["type"].(string) == "response.output_text.delta" {
				outText += m["delta"].(string)
			}
		}
	}
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"content":"before<thi"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		collect(evs)
	}
	collect(c.FeedDone())
	if strings.Contains(outText, "<thi") || strings.Contains(outText, "<think") {
		t.Fatalf("truncated open tag must not leak into output_text: %q", outText)
	}
	if outText != "before" {
		t.Fatalf("want output_text='before', got %q", outText)
	}
}

// TestThinkTagUnterminatedAtEnd 验证流在思考块内结束（无闭标签）时：已下发的思考文本
// 仍由 closeReasoning 收尾为 reasoning item，残留的半个闭标签前缀被丢弃并 WARN。
func TestThinkTagUnterminatedAtEnd(t *testing.T) {
	var warns bytes.Buffer
	c := New()
	c.SetLogger(slog.New(slog.NewTextHandler(&warns, &slog.HandlerOptions{Level: slog.LevelWarn})))
	c.SetClientModel("deepseek-v4-flash")
	var outText, reasonText string
	collect := func(evs []model.SSEEvent) {
		for _, e := range evs {
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatal(err)
			}
			switch m["type"].(string) {
			case "response.output_text.delta":
				outText += m["delta"].(string)
			case "response.reasoning_text.delta":
				reasonText += m["delta"].(string)
			}
		}
	}
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"content":"<think>deep thought"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		collect(evs)
	}
	collect(c.FeedDone())
	if reasonText != "deep thought" {
		t.Fatalf("want reasoning='deep thought', got %q", reasonText)
	}
	if outText != "" {
		t.Fatalf("want empty output_text, got %q", outText)
	}
	if !strings.Contains(warns.String(), "unterminated <think> block") {
		t.Fatalf("want WARN for unterminated think block, got: %q", warns.String())
	}
}

// TestReasoningContentAliasField 兼容部分上游用 delta.reasoning 而非 reasoning_content。
func TestReasoningContentAliasField(t *testing.T) {
	c := New()
	evs, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"role":"assistant","reasoning":"alias"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if evTypes(t, e.Data) == "response.reasoning_text.delta" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want reasoning_text.delta for delta.reasoning, got %v", evs)
	}
	evs, _ = c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`))
	joined := ""
	for _, e := range evs {
		joined += string(e.Data)
	}
	if !strings.Contains(joined, "alias") {
		t.Fatalf("want closed reasoning text: %s", joined)
	}
}

// TestReasoningTextAliasField 兼容 llama.cpp 等端点用 delta.reasoning_text 的别名。
func TestReasoningTextAliasField(t *testing.T) {
	c := New()
	evs, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{"role":"assistant","reasoning_text":"text-alias"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if evTypes(t, e.Data) == "response.reasoning_text.delta" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want reasoning_text.delta for delta.reasoning_text, got %v", evs)
	}
	evs, _ = c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`))
	joined := ""
	for _, e := range evs {
		joined += string(e.Data)
	}
	if !strings.Contains(joined, "text-alias") {
		t.Fatalf("want closed reasoning text: %s", joined)
	}
}

// TestReasoningThenToolCalls 工具调用前的 reasoning 必须先关闭，再开 function_call。
func TestReasoningThenToolCalls(t *testing.T) {
	c := New()
	var types []string
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"role":"assistant","reasoning_content":"要用工具"}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			types = append(types, evTypes(t, e.Data))
		}
	}
	rsDone := -1
	fnItemIdx := -1
	addedCount := 0
	for i, s := range types {
		if s == "response.reasoning_text.done" && rsDone < 0 {
			rsDone = i
		}
		if s == "response.output_item.added" {
			addedCount++
			if addedCount == 2 {
				fnItemIdx = i
			}
		}
	}
	if rsDone < 0 {
		t.Fatalf("missing reasoning_text.done in %v", types)
	}
	if fnItemIdx < 0 {
		t.Fatalf("missing function output_item.added in %v", types)
	}
	if rsDone >= fnItemIdx {
		t.Fatalf("reasoning must close before function item: rsDone=%d fnItem=%d types=%v", rsDone, fnItemIdx, types)
	}
}
