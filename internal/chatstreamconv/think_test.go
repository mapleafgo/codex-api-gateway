package chatstreamconv

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// feedAll 依次 Feed 多个 chunk 并追加 FeedDone，返回全部事件。
func feedAll(t *testing.T, c *Converter, chunks ...string) []model.SSEEvent {
	t.Helper()
	var all []model.SSEEvent
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	all = append(all, c.FeedDone()...)
	return all
}

// evTypeOf 不依赖 *testing.T 的事件类型解析（避免 t.Helper 在 nil 上 panic）。
func evTypeOf(data []byte) string {
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	typ, _ := m["type"].(string)
	return typ
}

// doneItems 返回所有 output_item.done 的 item。
func doneItems(events []model.SSEEvent) []map[string]any {
	var items []map[string]any
	for _, e := range events {
		if evTypeOf(e.Data) != "response.output_item.done" {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(e.Data, &m)
		if item, ok := m["item"].(map[string]any); ok {
			items = append(items, item)
		}
	}
	return items
}

// itemText 取出 message 的 content[0].text 或 reasoning 的 summary[0].text。
func itemText(item map[string]any) string {
	if arr, ok := item["content"].([]any); ok && len(arr) > 0 {
		if m, ok := arr[0].(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				return t
			}
		}
	}
	if arr, ok := item["summary"].([]any); ok && len(arr) > 0 {
		if m, ok := arr[0].(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				return t
			}
		}
	}
	return ""
}

func findItem(items []map[string]any, typ string) (map[string]any, bool) {
	for _, it := range items {
		if it["type"] == typ {
			return it, true
		}
	}
	return nil, false
}

func hasThinkLeak(events []model.SSEEvent) bool {
	for _, e := range events {
		if strings.Contains(string(e.Data), "</think>") || strings.Contains(string(e.Data), "<think>") {
			return true
		}
	}
	return false
}

// TestThinkToggleExtract 验证不标准上游以 </think> 同时作为开/闭标签：
// </think> 进入思考，下一个 </think> 退出，标签不泄漏进 output_text。
func TestThinkToggleExtract(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	all := feedAll(t, c,
		`{"id":"c1","choices":[{"delta":{"content":"</think>let me think"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":" hard</think>the answer"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":" is 42"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	if hasThinkLeak(all) {
		t.Fatalf("think tag leaked into events: %s", dumpTypes(all))
	}
	items := doneItems(all)
	reason, ok := findItem(items, string(model.ItemTypeReasoning))
	if !ok {
		t.Fatalf("no reasoning item, items=%v", items)
	}
	if got := itemText(reason); got != "let me think hard" {
		t.Fatalf("reasoning text = %q, want %q", got, "let me think hard")
	}
	msg, ok := findItem(items, string(model.ItemTypeMessage))
	if !ok {
		t.Fatalf("no message item, items=%v", items)
	}
	if got := itemText(msg); got != "the answer is 42" {
		t.Fatalf("message text = %q, want %q", got, "the answer is 42")
	}
}

// TestThinkLoneCloseStripped 验证 content 仅剩孤立闭标签（思考已由 reasoning_content 下发）
// 时被剥离，不开启新思考块、不泄漏。
func TestThinkLoneCloseStripped(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	all := feedAll(t, c,
		`{"id":"c1","choices":[{"delta":{"reasoning_content":"I am thinking"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"</think>final answer"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	if hasThinkLeak(all) {
		t.Fatalf("lone close tag leaked: %s", dumpTypes(all))
	}
	items := doneItems(all)
	// 仅一个 reasoning item，来自 reasoning_content
	reasonCount := 0
	for _, it := range items {
		if it["type"] == string(model.ItemTypeReasoning) {
			reasonCount++
		}
	}
	if reasonCount != 1 {
		t.Fatalf("want exactly 1 reasoning item (from reasoning_content), got %d", reasonCount)
	}
	msg, ok := findItem(items, string(model.ItemTypeMessage))
	if !ok {
		t.Fatalf("no message item, items=%v", items)
	}
	if got := itemText(msg); got != "final answer" {
		t.Fatalf("message text = %q, want %q", got, "final answer")
	}
}

// TestThinkDedupConsecutiveClose 验证 </think></think> 连续同标签去重：
// 仅当作单个分隔，不重新开启思考块。
func TestThinkDedupConsecutiveClose(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	all := feedAll(t, c,
		`{"id":"c1","choices":[{"delta":{"content":"</think>my thought</think></think>answer text"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	if hasThinkLeak(all) {
		t.Fatalf("think tag leaked: %s", dumpTypes(all))
	}
	items := doneItems(all)
	reason, ok := findItem(items, string(model.ItemTypeReasoning))
	if !ok {
		t.Fatalf("no reasoning item, items=%v", items)
	}
	if got := itemText(reason); got != "my thought" {
		t.Fatalf("reasoning text = %q, want %q", got, "my thought")
	}
	msg, ok := findItem(items, string(model.ItemTypeMessage))
	if !ok {
		t.Fatalf("no message item, items=%v", items)
	}
	if got := itemText(msg); got != "answer text" {
		t.Fatalf("message text = %q, want %q", got, "answer text")
	}
}

// TestThinkCrossChunkTag 验证 </think> 标签跨 chunk 流式拼接。
func TestThinkCrossChunkTag(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	all := feedAll(t, c,
		`{"id":"c1","choices":[{"delta":{"content":"</think>rea"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"soning</think>ans"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"wer"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	if hasThinkLeak(all) {
		t.Fatalf("think tag leaked: %s", dumpTypes(all))
	}
	items := doneItems(all)
	reason, ok := findItem(items, string(model.ItemTypeReasoning))
	if !ok {
		t.Fatalf("no reasoning item, items=%v", items)
	}
	if got := itemText(reason); got != "reasoning" {
		t.Fatalf("reasoning text = %q, want %q", got, "reasoning")
	}
	msg, ok := findItem(items, string(model.ItemTypeMessage))
	if !ok {
		t.Fatalf("no message item, items=%v", items)
	}
	if got := itemText(msg); got != "answer" {
		t.Fatalf("message text = %q, want %q", got, "answer")
	}
}

// TestThinkTruncatedAtEnd 验证流末半开 </think> 块（截断）已下发思考保留为 reasoning。
func TestThinkTruncatedAtEnd(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	all := feedAll(t, c,
		`{"id":"c1","choices":[{"delta":{"content":"</think>partial thought"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	if hasThinkLeak(all) {
		t.Fatalf("think tag leaked: %s", dumpTypes(all))
	}
	items := doneItems(all)
	reason, ok := findItem(items, string(model.ItemTypeReasoning))
	if !ok {
		t.Fatalf("no reasoning item, items=%v", items)
	}
	if got := itemText(reason); got != "partial thought" {
		t.Fatalf("reasoning text = %q, want %q", got, "partial thought")
	}
}

// TestThinkPureOutput 验证无标签的纯正文正常作为 output_text，不产生 reasoning item。
func TestThinkPureOutput(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	all := feedAll(t, c,
		`{"id":"c1","choices":[{"delta":{"content":"Hello"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":" world"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	items := doneItems(all)
	reasonCount := 0
	for _, it := range items {
		if it["type"] == string(model.ItemTypeReasoning) {
			reasonCount++
		}
	}
	if reasonCount != 0 {
		t.Fatalf("pure output must not create reasoning item, got %d", reasonCount)
	}
	msg, ok := findItem(items, string(model.ItemTypeMessage))
	if !ok {
		t.Fatalf("no message item, items=%v", items)
	}
	if got := itemText(msg); got != "Hello world" {
		t.Fatalf("message text = %q, want %q", got, "Hello world")
	}
}

// TestThinkStandardOpenTag 验证标准 <think> 开标签同样被抽取。
func TestThinkStandardOpenTag(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	all := feedAll(t, c,
		`{"id":"c1","choices":[{"delta":{"content":"<think>std reason</think>std output"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	if hasThinkLeak(all) {
		t.Fatalf("think tag leaked: %s", dumpTypes(all))
	}
	items := doneItems(all)
	reason, ok := findItem(items, string(model.ItemTypeReasoning))
	if !ok {
		t.Fatalf("no reasoning item, items=%v", items)
	}
	if got := itemText(reason); got != "std reason" {
		t.Fatalf("reasoning text = %q, want %q", got, "std reason")
	}
	msg, ok := findItem(items, string(model.ItemTypeMessage))
	if !ok {
		t.Fatalf("no message item, items=%v", items)
	}
	if got := itemText(msg); got != "std output" {
		t.Fatalf("message text = %q, want %q", got, "std output")
	}
}

// TestThinkCrossChunkDedupBoundary 验证标签跨 chunk 拆分时，闭标签不会被误去重：
// </think>rea</thi | nk>out 应解析为 思考=rea、正文=out（而非把闭标签当开标签重复）。
func TestThinkCrossChunkDedupBoundary(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	all := feedAll(t, c,
		`{"id":"c1","choices":[{"delta":{"content":"</think>rea</thi"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"nk>out"}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	if hasThinkLeak(all) {
		t.Fatalf("think tag leaked: %s", dumpTypes(all))
	}
	items := doneItems(all)
	reason, ok := findItem(items, string(model.ItemTypeReasoning))
	if !ok {
		t.Fatalf("no reasoning item, items=%v", items)
	}
	if got := itemText(reason); got != "rea" {
		t.Fatalf("reasoning text = %q, want %q", got, "rea")
	}
	msg, ok := findItem(items, string(model.ItemTypeMessage))
	if !ok {
		t.Fatalf("no message item, items=%v", items)
	}
	if got := itemText(msg); got != "out" {
		t.Fatalf("message text = %q, want %q", got, "out")
	}
}

func dumpTypes(events []model.SSEEvent) string {
	var ts []string
	for _, e := range events {
		ts = append(ts, evTypeOf(e.Data))
	}
	return strings.Join(ts, ",")
}
