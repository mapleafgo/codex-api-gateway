package chatstreamconv

import (
	"encoding/json"
	"testing"
)

func typesOf(t *testing.T, evs []interface { /* placeholder */
}) {
}

func evTypes(t *testing.T, data []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	typ, _ := m["type"].(string)
	return typ
}

func feedAll(t *testing.T, chunks ...string) *Converter {
	t.Helper()
	c := New()
	c.SetClientModel("gpt-4o")
	for _, ch := range chunks {
		if _, err := c.Feed([]byte(ch)); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}
	return c
}

func TestTextStream(t *testing.T) {
	c := New()
	c.SetClientModel("gpt-4o")
	var all []string
	collect := func(evs interface{}) {}
	_ = collect
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
	// OpenAI include_usage 末包：choices 为空
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
	add := func(evs []interface{}) {}
	_ = add
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
	if len(items) == 0 {
		t.Fatal("no output items")
	}
	// last function_call should have full args
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
