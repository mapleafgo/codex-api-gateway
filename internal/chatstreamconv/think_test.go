package chatstreamconv

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// 测试用标签：与 converter.go 的 thinkOpenTag/thinkCloseTag 同字节形态，
// 直接复用生产常量避免环境对尖括号字面量的改写。
var (
	thinkOpen  = thinkOpenTag
	thinkClose = thinkCloseTag
)

// feedToEnd 依次 Feed 多个 chunk，追加 finish_reason=stop 包与 FeedDone，返回全部事件。
func feedToEnd(t *testing.T, c *Converter, chunks ...string) []model.SSEEvent {
	t.Helper()
	var all []model.SSEEvent
	for _, ch := range chunks {
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	fin, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, fin...)
	all = append(all, c.FeedDone()...)
	return all
}

// chunkContent 构造只携带 delta.content 的 Chat chunk。
func chunkContent(content string) string {
	return `{"id":"c1","choices":[{"delta":{"content":"` + content + `"}}]}`
}

// evTypeOf 不依赖 *testing.T 的事件类型解析。
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
			if text, ok := m["text"].(string); ok {
				return text
			}
		}
	}
	if arr, ok := item["summary"].([]any); ok && len(arr) > 0 {
		if m, ok := arr[0].(map[string]any); ok {
			if text, ok := m["text"].(string); ok {
				return text
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

func countItems(items []map[string]any, typ string) int {
	n := 0
	for _, it := range items {
		if it["type"] == typ {
			n++
		}
	}
	return n
}

func hasTagLeak(events []model.SSEEvent) bool {
	for _, e := range events {
		s := string(e.Data)
		if strings.Contains(s, thinkOpen) || strings.Contains(s, thinkClose) {
			return true
		}
	}
	return false
}

func dumpTypes(events []model.SSEEvent) string {
	var ts []string
	for _, e := range events {
		ts = append(ts, evTypeOf(e.Data))
	}
	return strings.Join(ts, ",")
}

// TestThinkTagsLangChainStateMachine 按 langchainjs ChatDeepSeek 状态机验证
//
//	thinking/ response 解析，并覆盖  response 双角色（toggle）扩展。
func TestThinkTagsLangChainStateMachine(t *testing.T) {
	cases := []struct {
		name        string
		chunks      []string
		wantReason  string
		wantText    string
		wantReasonN int
		noTagLeak   bool
	}{
		{
			name:        "standard open close one chunk",
			chunks:      []string{chunkContent(thinkOpen + "a" + thinkClose + "b")},
			wantReason:  "a",
			wantText:    "b",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:   "open tag split across chunks",
			chunks: []string{chunkContent("<th"), chunkContent("ink>" + "a" + thinkClose + "b")},
			// "<th" 是 "<think>" 的合法前缀，LangChain 状态机应跨 chunk 暂存，
			// 与第二个分片拼成完整开标签后使 "a" 进入 reasoning。
			wantReason:  "a",
			wantText:    "b",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "close tag split across chunks",
			chunks:      []string{chunkContent(thinkOpen + "a</th"), chunkContent("ink>b")},
			wantReason:  "a",
			wantText:    "b",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "stream ends inside thinking",
			chunks:      []string{chunkContent(thinkOpen + "a")},
			wantReason:  "a",
			wantText:    "",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "close tag toggles open",
			chunks:      []string{chunkContent(thinkClose + "a" + thinkClose + "b")},
			wantReason:  "a",
			wantText:    "b",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "toggle split across chunks",
			chunks:      []string{chunkContent(thinkClose + "a"), chunkContent("b" + thinkClose + "c")},
			wantReason:  "ab",
			wantText:    "c",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "consecutive close tags dedupe",
			chunks:      []string{chunkContent(thinkClose + "a" + thinkClose + thinkClose + "b")},
			wantReason:  "a",
			wantText:    "b",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "consecutive toggle open tags dedupe",
			chunks:      []string{chunkContent(thinkClose + thinkClose + "a")},
			wantReason:  "a",
			wantText:    "",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "consecutive standard open tags dedupe",
			chunks:      []string{chunkContent(thinkOpen + thinkOpen + "a")},
			wantReason:  "a",
			wantText:    "",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "consecutive tags split across chunks dedupe",
			chunks:      []string{chunkContent(thinkClose + "a" + thinkClose), chunkContent(thinkClose + "b")},
			wantReason:  "a",
			wantText:    "b",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "open tag inside thinking stays thought text",
			chunks:      []string{chunkContent(thinkClose + "a" + thinkOpen + "b" + thinkClose + "c")},
			wantReason:  "a" + thinkOpen + "b",
			wantText:    "c",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "close remainder is not re-parsed",
			chunks:      []string{chunkContent(thinkOpen + "a" + thinkClose + thinkOpen + "b")},
			wantReason:  "a",
			wantText:    thinkOpen + "b",
			wantReasonN: 1,
			noTagLeak:   false,
		},
		{
			name: "empty content chunk keeps state",
			chunks: []string{
				chunkContent(thinkClose + "a"),
				`{"id":"c1","choices":[{"delta":{"role":"assistant"}}]}`,
				chunkContent("b" + thinkClose + "c"),
			},
			wantReason:  "ab",
			wantText:    "c",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name: "native reasoning chunk content passes through",
			chunks: []string{
				`{"id":"c1","choices":[{"delta":{"reasoning_content":"r","content":"` + thinkOpen + `x` + thinkClose + `y"}}]}`,
			},
			wantReason:  "r",
			wantText:    thinkOpen + "x" + thinkClose + "y",
			wantReasonN: 1,
			noTagLeak:   false,
		},
		{
			name:        "close tag after native reasoning toggles open",
			chunks:      []string{`{"id":"c1","choices":[{"delta":{"reasoning_content":"r"}}]}`, chunkContent(thinkClose + "x" + thinkClose + "y")},
			wantReason:  "rx",
			wantText:    "y",
			wantReasonN: 1,
			noTagLeak:   true,
		},
		{
			name:        "partial open prefix flushed as text",
			chunks:      []string{chunkContent("abc<thi")},
			wantReason:  "",
			wantText:    "abc<thi",
			wantReasonN: 0,
			noTagLeak:   false,
		},
		{
			name:        "partial close prefix flushed as reasoning",
			chunks:      []string{chunkContent(thinkClose + "a</th")},
			wantReason:  "a</th",
			wantText:    "",
			wantReasonN: 1,
			noTagLeak:   false,
		},
		{
			name:        "whitespace and case variants not recognized",
			chunks:      []string{chunkContent("< think>x</think >y<ThInk>z")},
			wantReason:  "",
			wantText:    "< think>x</think >y<ThInk>z",
			wantReasonN: 0,
			noTagLeak:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			c.SetClientModel("m")
			evs := feedToEnd(t, c, tc.chunks...)
			if tc.noTagLeak && hasTagLeak(evs) {
				t.Fatalf("tag leaked into events: %s", dumpTypes(evs))
			}
			items := doneItems(evs)
			if n := countItems(items, string(model.ItemTypeReasoning)); n != tc.wantReasonN {
				t.Fatalf("reasoning items = %d, want %d: %v", n, tc.wantReasonN, items)
			}
			if r, ok := findItem(items, string(model.ItemTypeReasoning)); ok {
				if got := itemText(r); got != tc.wantReason {
					t.Fatalf("reasoning text = %q, want %q", got, tc.wantReason)
				}
			} else if tc.wantReason != "" {
				t.Fatalf("missing reasoning item, want text %q", tc.wantReason)
			}
			if m, ok := findItem(items, string(model.ItemTypeMessage)); ok {
				if got := itemText(m); got != tc.wantText {
					t.Fatalf("message text = %q, want %q", got, tc.wantText)
				}
			} else if tc.wantText != "" {
				t.Fatalf("missing message item, want text %q", tc.wantText)
			}
		})
	}
}

// TestThinkThenToolCallOrder 验证思维文本先关闭 reasoning，再进入工具 item。
func TestThinkThenToolCallOrder(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var all []model.SSEEvent
	feed := func(ch string) {
		t.Helper()
		evs, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	feed(chunkContent(thinkOpen + "think"))
	feed(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	feed(chunkContent(thinkClose + "after"))
	fin, err := c.Feed([]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, fin...)
	all = append(all, c.FeedDone()...)
	evs := all
	rsDone, fnAdded := -1, -1
	for i, e := range evs {
		switch evTypeOf(e.Data) {
		case "response.reasoning_text.done":
			rsDone = i
		case "response.output_item.added":
			var m map[string]any
			_ = json.Unmarshal(e.Data, &m)
			if it, ok := m["item"].(map[string]any); ok && it["type"] == string(model.ItemTypeFunctionCall) {
				fnAdded = i
			}
		}
	}
	if rsDone < 0 || fnAdded < 0 || rsDone > fnAdded {
		t.Fatalf("want reasoning done before function item, rsDone=%d fnAdded=%d types=%s", rsDone, fnAdded, dumpTypes(evs))
	}
	items := doneItems(evs)
	if r, ok := findItem(items, string(model.ItemTypeReasoning)); !ok || itemText(r) != "think" {
		t.Fatalf("want reasoning text think, got %v", items)
	}
	if m, ok := findItem(items, string(model.ItemTypeMessage)); !ok || itemText(m) != "after" {
		t.Fatalf("want message text after, got %v", items)
	}
}

// TestThinkPartialTagPreservedAcrossEmptyContentChunk 验证残缺标签前缀跨空 content
// 分片（如仅携带 logprobs 的分片）保留，不被误判为安全文本下发，与 LangChain 空 text
// 分片原样透传、不改 buffer 的语义一致。
func TestThinkPartialTagPreservedAcrossEmptyContentChunk(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	var all []model.SSEEvent
	feed := func(j string) {
		t.Helper()
		evs, err := c.Feed([]byte(j))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	// chunk1：以残缺开标签前缀结尾，前段正文先下发，前缀留在 thinkBuf。
	feed(`{"id":"c1","choices":[{"delta":{"content":"abc<thi"}}]}`)
	if c.thinkBuf != "<thi" {
		t.Fatalf("after chunk1 thinkBuf=%q want %q", c.thinkBuf, "<thi")
	}
	// chunk2：仅 logprobs、无 content 的空分片，必须保留残缺前缀。
	feed(`{"id":"c1","choices":[{"delta":{"logprobs":{"content":[{"token":"x","logprob":-0.1,"top_logprobs":[]}]}}}]}`)
	if c.thinkBuf != "<thi" {
		t.Fatalf("empty content chunk must not flush partial tag, thinkBuf=%q", c.thinkBuf)
	}
	// chunk3：补齐标签，流末未闭合。
	feed(`{"id":"c1","choices":[{"delta":{"content":"nk>thought"}}]}`)
	feed(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`)
	all = append(all, c.FeedDone()...)

	if hasTagLeak(all) {
		t.Fatalf("tag leaked: %s", dumpTypes(all))
	}
	items := doneItems(all)
	if m, ok := findItem(items, string(model.ItemTypeMessage)); !ok || itemText(m) != "abc" {
		t.Fatalf("want message text abc, got %v", items)
	}
	if r, ok := findItem(items, string(model.ItemTypeReasoning)); !ok || itemText(r) != "thought" {
		t.Fatalf("want reasoning thought, got %v", items)
	}
}

// TestThinkStateResetOnContentFilter 验证 content_filter 丢弃路径清空思维标签状态。
func TestThinkStateResetOnContentFilter(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	evs := feedToEnd(t, c,
		chunkContent(thinkClose+"thought"+thinkClose),
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
	)
	if c.isThinking || c.thinkBuf != "" {
		t.Fatalf("think state not reset: isThinking=%v thinkBuf=%q", c.isThinking, c.thinkBuf)
	}
	items := doneItems(evs)
	if _, ok := findItem(items, string(model.ItemTypeReasoning)); ok {
		t.Fatalf("content_filter must drop reasoning, got %v", items)
	}
}
