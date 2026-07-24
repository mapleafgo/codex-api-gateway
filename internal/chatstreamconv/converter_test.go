package chatstreamconv

import (
	"encoding/json"
	"strings"
	"testing"
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

func TestShellCustomToolStream(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	c.SetFreeformNames(map[string]struct{}{"shell": {}})
	var types []string
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
		for _, e := range evs {
			types = append(types, evTypes(t, e.Data))
		}
	}
	for _, e := range c.FeedDone() {
		types = append(types, evTypes(t, e.Data))
	}
	has := false
	for _, typ := range types {
		if typ == "response.custom_tool_call_input.done" {
			has = true
		}
		if typ == "response.function_call_arguments.delta" {
			t.Fatalf("shell must not emit function arguments delta: %v", types)
		}
	}
	if !has {
		t.Fatalf("missing custom input done: %v", types)
	}
	var found bool
	for _, it := range c.OutputItems() {
		if it.Type == "custom_tool_call" && it.Name == "shell" {
			found = true
			if it.Input != "ls" {
				t.Fatalf("input unwrapped want ls got %q", it.Input)
			}
		}
	}
	if !found {
		t.Fatalf("items=%+v", c.OutputItems())
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
	for _, w := range []string{"response.mcp_call.in_progress", "response.mcp_call.completed"} {
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
		if it.Type == "mcp_call" {
			if it.ServerLabel != "fetch" || it.Name != "get" {
				t.Fatalf("item=%+v", it)
			}
			return
		}
	}
	t.Fatalf("no mcp item %+v", c.OutputItems())
}

func TestToolCallNameArrivesAfterID(t *testing.T) {
	// 兼容上游常见分片：先 id，后 name/arguments。
	// 仅有 id 时若立即 open，会按空 name 误判 function_call，output_item.added 类型错误。
	c := New()
	c.SetClientModel("m")
	c.SetFreeformNames(map[string]struct{}{"shell": {}})
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
		if it.Type == "custom_tool_call" && it.Name == "shell" && it.Input == "pwd" {
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

func TestCodeInterpreterOutboundShape(t *testing.T) {
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
	if !strings.Contains(joined, "code_interpreter_call") {
		t.Fatalf("want code_interpreter_call events: %s", joined)
	}
	if !strings.Contains(joined, "print(1)") {
		t.Fatalf("want code in events: %s", joined)
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
