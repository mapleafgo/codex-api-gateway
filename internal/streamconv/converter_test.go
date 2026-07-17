package streamconv

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
)

func evType(t *testing.T, data json.RawMessage) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("bad sse data: %v", err)
	}
	return m["type"].(string)
}

func eventTypes(evs []model.SSEEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}

func eventData(t *testing.T, ev model.SSEEvent) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode event data: %v", err)
	}
	return data
}

func eventByType(t *testing.T, evs []model.SSEEvent, typ string) model.SSEEvent {
	t.Helper()
	for _, ev := range evs {
		if ev.Type == typ {
			return ev
		}
	}
	t.Fatalf("missing event %s in %v", typ, eventTypes(evs))
	return model.SSEEvent{}
}

func TestConverterTextDelta(t *testing.T) {
	c := New()
	// message_start
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_1", Model: "claude"},
	})
	if len(evs) < 2 {
		t.Fatalf("expected created+in_progress, got %d", len(evs))
	}
	if evType(t, evs[0].Data) != "response.created" {
		t.Fatalf("expected response.created first, got %s", evType(t, evs[0].Data))
	}
	if evType(t, evs[1].Data) != "response.in_progress" {
		t.Fatalf("expected response.in_progress second, got %s", evType(t, evs[1].Data))
	}

	// text block start
	evs, _ = c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "text"},
	})
	hasItemAdded := false
	hasPartAdded := false
	for _, e := range evs {
		ft := evType(t, e.Data)
		if ft == "response.output_item.added" {
			hasItemAdded = true
		}
		if ft == "response.content_part.added" {
			hasPartAdded = true
		}
	}
	if !hasItemAdded || !hasPartAdded {
		t.Fatalf("expected output_item.added + content_part.added, got %+v", evs)
	}

	// text delta
	evs, _ = c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "text_delta", Text: "he"},
	})
	found := false
	for _, e := range evs {
		if evType(t, e.Data) == "response.output_text.delta" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected output_text.delta, got %+v", evs)
	}

	// text block stop
	evs, _ = c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})
	types := make([]string, len(evs))
	for i, e := range evs {
		types[i] = evType(t, e.Data)
	}
	if !contains(types, "response.output_text.done") {
		t.Fatalf("expected output_text.done in %+v", types)
	}
	if !contains(types, "response.content_part.done") {
		t.Fatalf("expected content_part.done in %+v", types)
	}
	if !contains(types, "response.output_item.done") {
		t.Fatalf("expected output_item.done in %+v", types)
	}
}

func TestConverterOutputMessageHasFinalAnswerPhase(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_1", Model: "claude"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "text"},
	})
	if len(evs) == 0 {
		t.Fatalf("expected output_item.added event")
	}
	var added struct {
		Item struct {
			Type  string `json:"type"`
			Role  string `json:"role"`
			Phase string `json:"phase"`
		} `json:"item"`
	}
	if err := json.Unmarshal(evs[0].Data, &added); err != nil {
		t.Fatalf("unmarshal output item: %v", err)
	}
	if added.Item.Type != "message" || added.Item.Role != "assistant" {
		t.Fatalf("expected assistant message item, got %+v", added.Item)
	}
	if added.Item.Phase != "final_answer" {
		t.Fatalf("assistant output phase not set: %+v", added.Item)
	}
	items := c.OutputItems()
	if len(items) != 1 || items[0].Phase != "final_answer" {
		t.Fatalf("stored output item phase not set: %+v", items)
	}
}

func TestConverterUsesClientModelWhenSet(t *testing.T) {
	c := New()
	c.SetClientModel("gpt-5.5")
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_1", Model: "glm-5.2"},
	})
	if len(evs) == 0 {
		t.Fatalf("expected response.created event")
	}
	payload := decodePayload(t, evs[0].Data)
	resp := payload["response"].(map[string]any)
	if resp["model"] != "gpt-5.5" {
		t.Fatalf("response model = %v, want client-facing alias gpt-5.5", resp["model"])
	}
}

func TestConverterThinkingAndToolUse(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	// thinking
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "thinking"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "thinking_delta", Thinking: "hmm"},
	})
	hasReason := false
	for _, e := range evs {
		if evType(t, e.Data) == "response.reasoning_text.delta" {
			hasReason = true
		}
	}
	if !hasReason {
		t.Fatalf("expected reasoning_text.delta, got %+v", evs)
	}
	// tool_use
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "tool_use", ID: "t1", Name: "run"},
	})
	evs, _ = c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 1,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "input_json_delta", PartialJSON: `{"a":1}`},
	})
	hasFC := false
	for _, e := range evs {
		if evType(t, e.Data) == "response.function_call_arguments.delta" {
			hasFC = true
		}
	}
	if !hasFC {
		t.Fatalf("expected function_call_arguments.delta, got %+v", evs)
	}
}

func TestConverterCompletion(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: "end_turn"},
		Usage: anthropic.MessageDeltaUsage{InputTokens: 10, OutputTokens: 5},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	var resp map[string]any
	for _, e := range evs {
		if evType(t, e.Data) == "response.completed" {
			m := decodePayload(t, e.Data)
			resp = m
		}
	}
	if resp == nil {
		t.Fatalf("expected response.completed, got %+v", evs)
	}
	r, _ := resp["response"].(map[string]any)
	if r["status"] != "completed" {
		t.Fatalf("expected status=completed, got %v", r["status"])
	}
	if r["object"] != "response" {
		t.Fatalf("expected object=response, got %v", r["object"])
	}
	u, ok := r["usage"].(map[string]any)
	if !ok {
		t.Fatalf("expected usage in response.completed, got %v", r)
	}
	if u["input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 5 {
		t.Fatalf("expected input=10 output=5, got %v", u)
	}
	if u["total_tokens"].(float64) != 15 {
		t.Fatalf("expected total=15, got %v", u)
	}
	output, ok := r["output"].([]any)
	if !ok {
		t.Fatalf("expected output array, got %v", r["output"])
	}
	_ = output // may be empty if no content blocks
}

func TestConverterCompletedEmittedOnce(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "text"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "text_delta", Text: "hi"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: "end_turn"},
	})

	first, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})
	second, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	count := 0
	for _, e := range append(first, second...) {
		if evType(t, e.Data) == "response.completed" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected response.completed exactly once, got %d", count)
	}

	if got := c.RespID(); got != "m" {
		t.Fatalf("RespID: want %q, got %q", "m", got)
	}
}

func TestConverterOutputItemsFunctionCall(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})

	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "tool_use", ID: "call_abc", Name: "search"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "input_json_delta", PartialJSON: `{"q":"`},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "input_json_delta", PartialJSON: `hello"}`},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})

	items := c.OutputItems()
	if len(items) != 1 {
		t.Fatalf("expected 1 output item, got %d: %+v", len(items), items)
	}
	if items[0].Type != "function_call" {
		t.Fatalf("expected function_call, got %+v", items[0])
	}
	if items[0].CallID != "call_abc" {
		t.Fatalf("expected CallID call_abc, got %q", items[0].CallID)
	}
	if items[0].Name != "search" {
		t.Fatalf("expected Name search, got %q", items[0].Name)
	}
	if items[0].Arguments != `{"q":"hello"}` {
		t.Fatalf("expected Arguments %q, got %q", `{"q":"hello"}`, items[0].Arguments)
	}
}

func TestConverterOutputItemsCustomToolCall(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})

	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "tool_use", ID: "call_patch", Name: "apply_patch"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "input_json_delta", PartialJSON: `{"input":"*** Begin`},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "input_json_delta", PartialJSON: ` Patch"}`},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})

	var types []string
	for _, e := range evs {
		types = append(types, evType(t, e.Data))
	}
	if !contains(types, "response.custom_tool_call_input.delta") {
		t.Fatalf("expected custom_tool_call_input.delta, got %+v", types)
	}
	if !contains(types, "response.custom_tool_call_input.done") {
		t.Fatalf("expected custom_tool_call_input.done, got %+v", types)
	}
	for _, typ := range types {
		if typ == "response.function_call_arguments.done" {
			t.Fatalf("custom tool must not emit function_call_arguments.done: %+v", types)
		}
	}
	items := c.OutputItems()
	if len(items) != 1 {
		t.Fatalf("expected 1 output item, got %d: %+v", len(items), items)
	}
	if items[0].Type != "custom_tool_call" {
		t.Fatalf("expected custom_tool_call, got %+v", items[0])
	}
	if items[0].Input != "*** Begin Patch" {
		t.Fatalf("expected unwrapped input, got %q", items[0].Input)
	}
}

func TestConverterOutputItemsTextAndReasoning(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})

	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "thinking"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "thinking_delta", Thinking: "let me think"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})

	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "text"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 1,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "text_delta", Text: "hello world"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 1,
	})

	items := c.OutputItems()
	if len(items) != 2 {
		t.Fatalf("expected 2 output items, got %d", len(items))
	}
	if items[0].Type != "reasoning" {
		t.Fatalf("expected first item reasoning, got %+v", items[0])
	}
	if len(items[0].Summary) != 1 || items[0].Summary[0].Text != "let me think" {
		t.Fatalf("bad reasoning summary: %+v", items[0].Summary)
	}
	if items[1].Type != "message" {
		t.Fatalf("expected second item message, got %+v", items[1])
	}
	if len(items[1].Content) != 1 || items[1].Content[0].Text != "hello world" {
		t.Fatalf("bad message content: %+v", items[1].Content)
	}
}

func TestSignatureDeltaCaptured(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "thinking"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "thinking_delta", Thinking: "hmm"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "signature_delta", Signature: "EqQBCg..."},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})

	items := c.OutputItems()
	if len(items) != 1 || items[0].Type != "reasoning" {
		t.Fatalf("expected one reasoning item, got %+v", items)
	}
	if items[0].Signature != "EqQBCg..." {
		t.Fatalf("expected signature %q, got %q", "EqQBCg...", items[0].Signature)
	}
	if len(items[0].Summary) != 1 || items[0].Summary[0].Text != "hmm" {
		t.Fatalf("bad thinking summary: %+v", items[0].Summary)
	}
}

func TestErrorEventSurfacesAsFailed(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	ev := &anthropic.MessageStreamEventUnion{Type: "error"}
	ev.Delta.Text = "Overloaded"
	evs, _ := c.Feed(ev)
	if len(evs) != 1 || evs[0].Type != "response.failed" {
		t.Fatalf("expected one response.failed, got %+v", evs)
	}
	// Verify error message is present in the response object.
	m := decodePayload(t, evs[0].Data)
	r, _ := m["response"].(map[string]any)
	if r == nil {
		t.Fatalf("expected response object in payload: %v", m)
	}
	errObj, _ := r["error"].(map[string]any)
	if errObj == nil || errObj["message"] != "Overloaded" {
		t.Fatalf("expected error.message=Overloaded, got %v", r["error"])
	}
}

func TestWebSearchServerToolUseEmitsWebSearchCall(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:  "server_tool_use",
			ID:    "toolu_ws1",
			Name:  "web_search",
			Input: map[string]any{"query": "golang tutorial"},
		},
	})
	added := eventData(t, eventByType(t, evs, "response.output_item.added"))
	item := added["item"].(map[string]any)
	if item["type"] != "web_search_call" {
		t.Fatalf("expected web_search_call item, got %v", item["type"])
	}
	action := item["action"].(map[string]any)
	if action["type"] != "search" || action["query"] != "golang tutorial" {
		t.Fatalf("bad web search action: %v", action)
	}
	eventByType(t, evs, "response.web_search_call.in_progress")
	eventByType(t, evs, "response.web_search_call.searching")

	evs2, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:      "web_search_tool_result",
			ToolUseID: "toolu_ws1",
		},
	})
	eventByType(t, evs2, "response.web_search_call.completed")
	done := eventData(t, eventByType(t, evs2, "response.output_item.done"))
	doneItem := done["item"].(map[string]any)
	if doneItem["status"] != "completed" {
		t.Fatalf("expected web_search_call done status completed, got %v", doneItem["status"])
	}
}

func TestWebSearchResultSurfacesSources(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:  "server_tool_use",
			ID:    "toolu_ws1",
			Name:  "web_search",
			Input: map[string]any{"query": "rust async"},
		},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:      "web_search_tool_result",
			ToolUseID: "toolu_ws1",
			Content: anthropic.ContentBlockStartEventContentBlockUnionContent{
				OfWebSearchResultBlockArray: []anthropic.WebSearchResultBlock{
					{Title: "A", URL: "https://a.example.com"},
					{Title: "B", URL: "https://b.example.com"},
				},
			},
		},
	})
	done := eventData(t, eventByType(t, evs, "response.output_item.done"))
	item := done["item"].(map[string]any)
	action := item["action"].(map[string]any)
	sources, ok := action["sources"].([]any)
	if !ok || len(sources) != 2 {
		t.Fatalf("expected 2 sources, got %v", action["sources"])
	}
	first := sources[0].(map[string]any)
	if first["type"] != "url" || first["url"] != "https://a.example.com" {
		t.Fatalf("bad first source: %v", first)
	}
}

func TestCodeExecutionServerToolUseEmitsCodeInterpreterCall(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_start", Message: anthropic.Message{ID: "m", Model: "x"}})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:  "server_tool_use",
			ID:    "toolu_ci1",
			Name:  "code_execution",
			Input: map[string]any{"code": "print(3)"},
		},
	})
	added := eventData(t, eventByType(t, evs, "response.output_item.added"))
	item := added["item"].(map[string]any)
	if item["type"] != "code_interpreter_call" {
		t.Fatalf("expected code_interpreter_call, got %v", item["type"])
	}
	if item["code"] != "print(3)" {
		t.Fatalf("bad code: %v", item["code"])
	}
	if item["container_id"] == "" {
		t.Fatal("container_id must be synthesized")
	}
	eventByType(t, evs, "response.code_interpreter_call.in_progress")
	eventByType(t, evs, "response.code_interpreter_call.interpreting")
	eventByType(t, evs, "response.code_interpreter_call_code.delta")
	eventByType(t, evs, "response.code_interpreter_call_code.done")

	evs2, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:      "code_execution_tool_result",
			ToolUseID: "toolu_ci1",
			// stdout 在 Content（CodeExecutionToolResultBlockContentUnion），非顶层。
			Content: anthropic.ContentBlockStartEventContentBlockUnionContent{Stdout: "3\n"},
		},
	})
	done := eventData(t, eventByType(t, evs2, "response.output_item.done"))
	doneItem := done["item"].(map[string]any)
	if doneItem["status"] != "completed" {
		t.Fatalf("expected completed, got %v", doneItem["status"])
	}
	outputs := doneItem["outputs"].([]any)
	if outputs[0].(map[string]any)["logs"] != "3\n" {
		t.Fatalf("bad logs output: %v", outputs[0])
	}
}

func TestCodeExecutionResultStderrFoldedIntoLogs(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_start", Message: anthropic.Message{ID: "m", Model: "x"}})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "server_tool_use", ID: "toolu_ci2", Name: "code_execution", Input: map[string]any{"code": "x"},
		},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "code_execution_tool_result", ToolUseID: "toolu_ci2",
			Content: anthropic.ContentBlockStartEventContentBlockUnionContent{Stdout: "out", Stderr: "err"},
		},
	})
	done := eventData(t, eventByType(t, evs, "response.output_item.done"))
	logs := done["item"].(map[string]any)["outputs"].([]any)[0].(map[string]any)["logs"]
	if !strings.Contains(logs.(string), "out") || !strings.Contains(logs.(string), "err") {
		t.Fatalf("stdout+stderr must fold into logs: %v", logs)
	}
}

func TestUnsupportedAnthropicBlockFailsInsteadOfSilentDrop(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_unsupported", Model: "claude-test"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "totally_unknown_block",
		},
	})
	if len(evs) != 1 || evs[0].Type != "response.failed" {
		t.Fatalf("expected response.failed for unsupported block, got %+v", evs)
	}
	data := eventData(t, evs[0])
	if data["type"] != "response.failed" {
		t.Fatalf("failed payload type mismatch: %v", data)
	}
	if _, ok := data["sequence_number"].(float64); !ok {
		t.Fatalf("failed payload should include sequence_number: %v", data)
	}
	response := data["response"].(map[string]any)
	if response["status"] != "failed" {
		t.Fatalf("response status should be failed: %v", response)
	}
	output := response["output"].([]any)
	if len(output) != 0 {
		t.Fatalf("failed response should have empty output: %v", output)
	}
	errObj := response["error"].(map[string]any)
	if !strings.Contains(errObj["message"].(string), "totally_unknown_block") {
		t.Fatalf("failed event should name unsupported block: %v", errObj)
	}
	trailing, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})
	if len(trailing) != 0 {
		t.Fatalf("unsupported block should mark converter complete, got trailing events %+v", trailing)
	}
}

func TestUnsupportedAnthropicBlockSuppressesLaterErrorTerminal(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_unsupported_error", Model: "claude-test"},
	})
	first, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start",
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "totally_unknown_block",
		},
	})
	errEv := &anthropic.MessageStreamEventUnion{Type: "error"}
	errEv.Delta.Text = "upstream failed after unsupported block"
	second, _ := c.Feed(errEv)
	third, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	all := append(append(first, second...), third...)
	terminalCount := 0
	for _, ev := range all {
		switch ev.Type {
		case "response.failed", "response.completed", "response.incomplete":
			terminalCount++
		}
	}
	if terminalCount != 1 {
		t.Fatalf("expected exactly one terminal event, got %d in %v", terminalCount, eventTypes(all))
	}
	if len(second) != 0 || len(third) != 0 {
		t.Fatalf("completed converter should suppress later error/message_stop, got error=%+v stop=%+v", second, third)
	}
}

// TestNonWebSearchServerToolUseSkippedNotFailed verifies that server tools
// without a Responses equivalent (web_fetch, code_execution, ...) are silently
// skipped instead of failing the stream. The block's start, delta, and stop
// events are all ignored, and the stream completes normally.
func TestNonWebSearchServerToolUseSkippedNotFailed(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_skip", Model: "claude-test"},
	})

	// text block before the skipped block — should produce events normally
	textStart, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "text"},
	})
	eventByType(t, textStart, "response.output_item.added")

	// web_fetch server_tool_use — must be skipped silently
	skipStart, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "server_tool_use",
			ID:   "srv_fetch",
			Name: "web_fetch",
		},
	})
	if len(skipStart) != 0 {
		t.Fatalf("web_fetch server_tool_use should produce no events, got %+v", skipStart)
	}

	// delta for the skipped block — must be ignored
	skipDelta, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 1,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "input_json_delta", PartialJSON: "{}"},
	})
	if len(skipDelta) != 0 {
		t.Fatalf("delta for skipped block should produce no events, got %+v", skipDelta)
	}

	// web_fetch_tool_result — must also be skipped
	resultStart, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 2,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "web_fetch_tool_result",
		},
	})
	if len(resultStart) != 0 {
		t.Fatalf("web_fetch_tool_result should produce no events, got %+v", resultStart)
	}

	// stop events for skipped blocks — must be ignored
	for _, idx := range []int64{1, 2} {
		stopEvs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
			Type:  "content_block_stop",
			Index: idx,
		})
		if len(stopEvs) != 0 {
			t.Fatalf("stop for skipped block %d should produce no events, got %+v", idx, stopEvs)
		}
	}

	// text stop — should produce normal text done events
	textStop, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})
	eventByType(t, textStop, "response.output_text.done")

	// message_stop — should complete normally, not fail
	complete, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})
	completed := eventByType(t, complete, "response.completed")
	resp := eventData(t, completed)["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("expected completed status after skipping server tool, got %v", resp["status"])
	}
}

// TestToolResultBlockSkippedNotFailed verifies that a tool_result content
// block echoed back by some upstream backends is silently skipped instead
// of failing the stream.
func TestToolResultBlockSkippedNotFailed(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_tr", Model: "claude-test"},
	})

	// tool_result block echoed by upstream — must be skipped silently
	skipStart, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "tool_result",
		},
	})
	if len(skipStart) != 0 {
		t.Fatalf("tool_result block should produce no events, got %+v", skipStart)
	}

	// stop for the skipped block — must be ignored
	stopEvs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})
	if len(stopEvs) != 0 {
		t.Fatalf("stop for skipped tool_result block should produce no events, got %+v", stopEvs)
	}

	// message_stop — should complete normally, not fail
	complete, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})
	completed := eventByType(t, complete, "response.completed")
	resp := eventData(t, completed)["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("expected completed status after skipping tool_result, got %v", resp["status"])
	}
}

// TestToolResultWithWebSearchToolUseIDCompletesWebSearchCall verifies that
// when a backend misnames web_search_tool_result as tool_result but carries
// the web_search server_tool_use id, the gateway treats it as the web search
// result and completes the web_search_call item.
func TestToolResultWithWebSearchToolUseIDCompletesWebSearchCall(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m_compat", Model: "claude-test"},
	})
	// 先发出 web_search server_tool_use
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:  "server_tool_use",
			ID:    "toolu_ws_compat",
			Name:  "web_search",
			Input: map[string]any{"query": "compat test"},
		},
	})
	// 后端把 web_search_tool_result 传成了 tool_result，但 tool_use_id 指向 web_search
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:      "tool_result",
			ToolUseID: "toolu_ws_compat",
		},
	})
	eventByType(t, evs, "response.web_search_call.completed")
	done := eventData(t, eventByType(t, evs, "response.output_item.done"))
	item := done["item"].(map[string]any)
	if item["status"] != "completed" {
		t.Fatalf("web_search_call should be completed via compatible tool_result, got %v", item["status"])
	}
}

// TestServerToolUseAliasedNameFallsBackToDeclaredServerTool verifies that when a
// compatibility backend returns a server_tool_use whose name is a non-standard
// alias (e.g. GLM's "web_search_prime") missing from the catalog, the converter
// falls back to the sole declared server tool identity and still emits a
// web_search_call instead of dropping the block. The follow-up tool_result
// (carrying the alias tool_use_id) then completes the call via the existing
// tool_result-compatible branch — covering the full GLM wire shape end to end.
func TestServerToolUseAliasedNameFallsBackToDeclaredServerTool(t *testing.T) {
	c := New()
	c.SetDeclaredServerTools([]toolcatalog.Identity{{OpenAIType: "web_search", Name: "web_search"}})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m_glm", Model: "glm-5.2"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:  "server_tool_use",
			ID:    "toolu_ws_glm",
			Name:  "web_search_prime", // GLM 方言名，不在 catalog
			Input: map[string]any{"query": "rust async"},
		},
	})
	added := eventData(t, eventByType(t, evs, "response.output_item.added"))
	item := added["item"].(map[string]any)
	if item["type"] != "web_search_call" {
		t.Fatalf("aliased server_tool_use should fall back to web_search_call, got %v", item["type"])
	}
	action := item["action"].(map[string]any)
	if action["query"] != "rust async" {
		t.Fatalf("query should be carried through fallback: %v", action)
	}
	eventByType(t, evs, "response.web_search_call.searching")

	// 后端以 tool_result（非 web_search_tool_result）回传结果，tool_use_id 指向该 call。
	evs2, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:      "tool_result",
			ToolUseID: "toolu_ws_glm",
		},
	})
	eventByType(t, evs2, "response.web_search_call.completed")
}

// TestServerToolUseAliasedNameSkippedWhenUndeclared verifies that an unrecognized
// server_tool_use name is still dropped (no item emitted) when no server tool was
// declared — the fallback only applies to tools the client actually requested,
// so a backend self-invoked tool we never asked for remains skipped.
func TestServerToolUseAliasedNameSkippedWhenUndeclared(t *testing.T) {
	c := New() // 未声明任何 server tool
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m_undeclared", Model: "x"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "server_tool_use",
			Name: "web_search_prime",
		},
	})
	if len(evs) != 0 {
		t.Fatalf("undeclared aliased server_tool_use should be skipped, got %d events", len(evs))
	}
}

func TestSummarizedEmitsSummaryEvents(t *testing.T) {
	c := New()
	c.SetSummarized(true)
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "thinking"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "thinking_delta", Thinking: "summary text"},
	})
	stopEvs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})

	allEvs := append(evs, stopEvs...)
	types := make([]string, len(allEvs))
	for i, e := range allEvs {
		types[i] = evType(t, e.Data)
	}

	// Must emit reasoning_summary_* events, NOT reasoning_text.*
	if !contains(types, "response.reasoning_summary_part.added") {
		t.Fatalf("expected reasoning_summary_part.added, got %+v", types)
	}
	if !contains(types, "response.reasoning_summary_text.delta") {
		t.Fatalf("expected reasoning_summary_text.delta, got %+v", types)
	}
	if !contains(types, "response.reasoning_summary_text.done") {
		t.Fatalf("expected reasoning_summary_text.done, got %+v", types)
	}
	if !contains(types, "response.reasoning_summary_part.done") {
		t.Fatalf("expected reasoning_summary_part.done, got %+v", types)
	}
	for _, ft := range types {
		if ft == "response.reasoning_text.delta" || ft == "response.reasoning_text.done" {
			t.Fatalf("summarized mode must NOT emit reasoning_text.* events, got %+v", types)
		}
	}

	// Verify reasoning_summary_text.done carries full text (not delta).
	for _, e := range allEvs {
		if evType(t, e.Data) == "response.reasoning_summary_text.done" {
			m := decodePayload(t, e.Data)
			if m["text"] != "summary text" {
				t.Fatalf("expected text field with full summary, got %v", m["text"])
			}
		}
	}
}

func TestRedactedThinkingStoredAsEncrypted(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "redacted_thinking", Data: "ENCRYPTED_DATA"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})

	items := c.OutputItems()
	if len(items) != 1 || items[0].Type != "reasoning" {
		t.Fatalf("expected one reasoning item, got %+v", items)
	}
	if items[0].EncryptedContent != "ENCRYPTED_DATA" {
		t.Fatalf("redacted data not stored: %q", items[0].EncryptedContent)
	}
	hasDone := false
	for _, e := range evs {
		if e.Type == "response.output_item.done" {
			hasDone = true
		}
	}
	if !hasDone {
		t.Fatalf("expected output_item.done for redacted_thinking")
	}
}

// TestRedactedSummarizedNoSummaryEvents verifies Fix B: when summarized mode
// is active and a redacted_thinking block arrives, no reasoning_summary_*
// events are emitted (the guard checks EncryptedContent == "").
func TestRedactedSummarizedNoSummaryEvents(t *testing.T) {
	c := New()
	c.SetSummarized(true)
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "redacted_thinking", Data: "SECRET"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})

	for _, e := range evs {
		ft := evType(t, e.Data)
		if strings.HasPrefix(ft, "response.reasoning_summary") ||
			ft == "response.reasoning_text.delta" ||
			ft == "response.reasoning_text.done" {
			t.Fatalf("redacted+summarized must NOT emit %s, got events: %+v", ft, evs)
		}
	}

	items := c.OutputItems()
	if len(items) != 1 || items[0].EncryptedContent != "SECRET" {
		t.Fatalf("redacted data must still be stored, got %+v", items)
	}
}

func TestSequenceNumberMonotonic(t *testing.T) {
	c := New()
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	var seqs []int64
	for _, e := range evs {
		var m map[string]any
		json.Unmarshal(e.Data, &m)
		if s, ok := m["sequence_number"].(float64); ok {
			seqs = append(seqs, int64(s))
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("sequence_number not monotonic: %v", seqs)
		}
	}
}

func TestTextWirePartsIncludeEmptyAnnotations(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_annotations", Model: "claude-test"},
	})
	textStart, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "text"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "text_delta", Text: "hello"},
	})
	blockStop, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "content_block_stop", Index: 0})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: "end_turn"},
	})
	messageStop, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	events := append(textStart, blockStop...)
	events = append(events, messageStop...)
	for _, eventType := range []string{
		"response.content_part.added",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	} {
		found := false
		for _, event := range events {
			if event.Type != eventType {
				continue
			}
			found = true
			payload := decodePayload(t, event.Data)
			switch eventType {
			case "response.content_part.added", "response.content_part.done":
				assertOutputTextAnnotations(t, eventType, payload["part"].(map[string]any))
			case "response.output_item.done":
				item := payload["item"].(map[string]any)
				assertOutputTextAnnotations(t, eventType, item["content"].([]any)[0].(map[string]any))
			case "response.completed":
				response := payload["response"].(map[string]any)
				item := response["output"].([]any)[0].(map[string]any)
				assertOutputTextAnnotations(t, eventType, item["content"].([]any)[0].(map[string]any))
			}
		}
		if !found {
			t.Fatalf("missing %s event", eventType)
		}
	}
}

func assertOutputTextAnnotations(t *testing.T, eventType string, part map[string]any) {
	t.Helper()
	if part["type"] != "output_text" {
		t.Fatalf("%s part type = %#v, want output_text", eventType, part["type"])
	}
	annotations, ok := part["annotations"].([]any)
	if !ok || len(annotations) != 0 {
		t.Fatalf("%s output_text annotations = %#v, want []", eventType, part["annotations"])
	}
}

func TestMaxTokensIncomplete(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: "max_tokens"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	for _, e := range evs {
		ft := evType(t, e.Data)
		if ft == "response.incomplete" {
			m := decodePayload(t, e.Data)
			r, _ := m["response"].(map[string]any)
			details, _ := r["incomplete_details"].(map[string]any)
			if details == nil || details["reason"] != "max_output_tokens" {
				t.Fatalf("expected incomplete_details.reason=max_output_tokens, got %v", r)
			}
			return
		}
	}
	t.Fatalf("expected response.incomplete for max_tokens")
}

func TestPauseTurnDoesNotEmitInvalidIncompleteReason(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_pause", Model: "claude-test"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: anthropic.StopReasonPauseTurn},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	last := evs[len(evs)-1]
	if last.Type != "response.incomplete" {
		t.Fatalf("expected response.incomplete, got %s", last.Type)
	}
	if strings.Contains(string(last.Data), `"reason":"pause_turn"`) {
		t.Fatalf("pause_turn is not an OpenAI incomplete_details.reason: %s", last.Data)
	}
	payload := decodePayload(t, last.Data)
	response, _ := payload["response"].(map[string]any)
	details, ok := response["incomplete_details"]
	if !ok || details != nil {
		t.Fatalf("pause_turn incomplete response must include incomplete_details:null, got %v", response)
	}
}

func TestRefusalStopReasonEmitsRefusalPartAndContentFilter(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_refusal", Model: "claude-test"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{
			StopReason: anthropic.StopReasonRefusal,
			StopDetails: anthropic.RefusalStopDetails{
				Category:    anthropic.RefusalStopDetailsCategoryCyber,
				Explanation: "I can't help with that.",
			},
		},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	types := eventTypes(evs)
	for _, want := range []string{
		"response.output_item.added",
		"response.content_part.added",
		"response.refusal.delta",
		"response.refusal.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.incomplete",
	} {
		if !slices.Contains(types, want) {
			t.Fatalf("missing %s in %v", want, types)
		}
	}
	last := evs[len(evs)-1]
	if !strings.Contains(string(last.Data), `"reason":"content_filter"`) {
		t.Fatalf("refusal should map to OpenAI content_filter: %s", last.Data)
	}

	addedItem := eventData(t, eventByType(t, evs, "response.output_item.added"))["item"].(map[string]any)
	if got := addedItem["status"]; got != "in_progress" {
		t.Fatalf("added refusal item status = %#v, want in_progress", got)
	}
	addedContent, ok := addedItem["content"].([]any)
	if !ok || len(addedContent) != 0 {
		t.Fatalf("added refusal item content = %#v, want explicit empty array", addedItem["content"])
	}

	addedPart := eventData(t, eventByType(t, evs, "response.content_part.added"))["part"].(map[string]any)
	if got := addedPart["refusal"]; got != "" {
		t.Fatalf("added refusal part = %#v, want empty refusal", got)
	}
	if _, ok := addedPart["text"]; ok {
		t.Fatalf("added refusal part should not include text field: %#v", addedPart)
	}

	delta := eventData(t, eventByType(t, evs, "response.refusal.delta"))
	if got := delta["delta"]; got != "I can't help with that." {
		t.Fatalf("refusal delta = %#v, want final refusal", got)
	}
	if got := delta["content_index"]; got != float64(0) {
		t.Fatalf("refusal delta content_index = %#v, want 0", got)
	}

	for _, typ := range []string{"response.refusal.done", "response.content_part.done"} {
		data := eventData(t, eventByType(t, evs, typ))
		if typ == "response.refusal.done" {
			if got := data["refusal"]; got != "I can't help with that." {
				t.Fatalf("%s refusal = %#v, want final refusal", typ, got)
			}
			if got := data["content_index"]; got != float64(0) {
				t.Fatalf("%s content_index = %#v, want 0", typ, got)
			}
			continue
		}
		part := data["part"].(map[string]any)
		if got := part["refusal"]; got != "I can't help with that." {
			t.Fatalf("%s refusal = %#v, want final refusal", typ, got)
		}
	}

	doneItem := eventData(t, eventByType(t, evs, "response.output_item.done"))["item"].(map[string]any)
	if got := doneItem["status"]; got != "completed" {
		t.Fatalf("done refusal item status = %#v, want completed", got)
	}
	doneContent := doneItem["content"].([]any)
	donePart := doneContent[0].(map[string]any)
	if got := donePart["refusal"]; got != "I can't help with that." {
		t.Fatalf("done refusal item = %#v, want final refusal", got)
	}

	terminal := eventData(t, last)
	response := terminal["response"].(map[string]any)
	output := response["output"].([]any)
	item := output[0].(map[string]any)
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	if got := part["refusal"]; got != "I can't help with that." {
		t.Fatalf("terminal response should include refusal field, got %#v", part)
	}
	if _, ok := part["text"]; ok {
		t.Fatalf("terminal refusal content should not include text field: %#v", part)
	}
}

func TestRefusalWithoutDetailsStillEmitsRefusalEvents(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_empty_refusal", Model: "claude-test"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: anthropic.StopReasonRefusal},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	wantTypes := []string{
		"response.output_item.added",
		"response.content_part.added",
		"response.refusal.delta",
		"response.refusal.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.incomplete",
	}
	if got := eventTypes(evs); !slices.Equal(got, wantTypes) {
		t.Fatalf("unexpected event sequence:\nwant %v\n got %v", wantTypes, got)
	}
	added := eventData(t, eventByType(t, evs, "response.content_part.added"))
	part := added["part"].(map[string]any)
	if got, ok := part["refusal"]; !ok || got != "" {
		t.Fatalf("empty refusal should still be explicit, got %#v", part)
	}
}

func TestRefusalDiscardsPartialTextFromTerminalOutput(t *testing.T) {
	c := New()
	var events []model.SSEEvent
	startEvents, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_partial_refusal", Model: "claude-test"},
	})
	events = append(events, startEvents...)
	blockEvents, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "text"},
	})
	events = append(events, blockEvents...)
	deltaEvents, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "text_delta", Text: "partial text"},
	})
	events = append(events, deltaEvents...)
	stopReasonEvents, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{
			StopReason:  anthropic.StopReasonRefusal,
			StopDetails: anthropic.RefusalStopDetails{Explanation: "I can't help with that."},
		},
	})
	events = append(events, stopReasonEvents...)
	terminalEvents, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})
	events = append(events, terminalEvents...)

	terminal := eventData(t, events[len(events)-1])
	response := terminal["response"].(map[string]any)
	output := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("terminal refusal output = %#v, want only refusal item", output)
	}
	content := output[0].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["refusal"] != "I can't help with that." {
		t.Fatalf("terminal refusal content = %#v", content)
	}
	if strings.Contains(string(events[len(events)-1].Data), "partial text") {
		t.Fatalf("terminal response leaked partial text: %s", events[len(events)-1].Data)
	}
	terminalID := output[0].(map[string]any)["id"]
	for _, typ := range []string{
		"response.output_item.added",
		"response.content_part.added",
		"response.refusal.delta",
		"response.refusal.done",
		"response.content_part.done",
		"response.output_item.done",
	} {
		payload := eventData(t, eventByType(t, events, typ))
		if got := payload["output_index"]; got != float64(0) {
			t.Fatalf("%s output_index = %#v, want 0", typ, got)
		}
		if typ != "response.output_item.added" && typ != "response.output_item.done" {
			if got := payload["content_index"]; got != float64(0) {
				t.Fatalf("%s content_index = %#v, want 0", typ, got)
			}
			if got := payload["item_id"]; got != terminalID {
				t.Fatalf("%s item_id = %#v, want %#v", typ, got, terminalID)
			}
		}
	}
	items := c.OutputItems()
	if len(items) != 1 || len(items[0].Content) != 1 || items[0].Content[0].Refusal == nil {
		t.Fatalf("stored output items = %#v, want only refusal", items)
	}
}

func TestRefusalUsesReadableFallbackInsteadOfCategory(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_refusal_fallback", Model: "claude-test"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{
			StopReason:  anthropic.StopReasonRefusal,
			StopDetails: anthropic.RefusalStopDetails{Category: anthropic.RefusalStopDetailsCategoryCyber},
		},
	})
	events, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	terminal := eventData(t, events[len(events)-1])
	output := terminal["response"].(map[string]any)["output"].([]any)
	part := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["refusal"] != "I can't help with that." {
		t.Fatalf("refusal fallback = %#v, want readable text", part)
	}
	if part["refusal"] == "cyber" {
		t.Fatalf("refusal must not expose category as text: %#v", part)
	}
}

func TestEmptyOutputTextKeepsRequiredTextFields(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "msg_empty_text", Model: "claude-test"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "text"},
	})
	blockStopEvents, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "content_block_stop", Index: 0})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: anthropic.StopReasonEndTurn},
	})
	terminalEvents, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	done := eventData(t, eventByType(t, blockStopEvents, "response.output_item.done"))
	doneItem := done["item"].(map[string]any)
	doneContent := doneItem["content"].([]any)
	donePart := doneContent[0].(map[string]any)
	if got, ok := donePart["text"]; !ok || got != "" {
		t.Fatalf("output_item.done output_text should include empty text, got %#v", donePart)
	}

	terminal := eventData(t, terminalEvents[len(terminalEvents)-1])
	response := terminal["response"].(map[string]any)
	output := response["output"].([]any)
	item := output[0].(map[string]any)
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	if got, ok := part["text"]; !ok || got != "" {
		t.Fatalf("terminal output_text should include empty text, got %#v", part)
	}
}

func TestMcpToolUseEmitsMcpCall(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_start", Message: anthropic.Message{ID: "m", Model: "x"}})
	// 模拟 ScanEvents probe 合成的 mcp_tool_use 事件
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "mcp_tool_use", ID: "toolu_mcp1", Name: "get",
			Input: map[string]any{"server_name": "weather", "name": "get", "arguments": `{"q":"sf"}`},
		},
	})
	added := eventData(t, eventByType(t, evs, "response.output_item.added"))
	item := added["item"].(map[string]any)
	if item["type"] != "mcp_call" || item["server_label"] != "weather" || item["name"] != "get" {
		t.Fatalf("bad mcp_call item: %v", item)
	}
	if item["arguments"] != `{"q":"sf"}` {
		t.Fatalf("bad arguments: %v", item["arguments"])
	}
	eventByType(t, evs, "response.mcp_call.in_progress")
	eventByType(t, evs, "response.mcp_call_arguments.delta")
	eventByType(t, evs, "response.mcp_call_arguments.done")

	evs2, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "mcp_tool_result", ToolUseID: "toolu_mcp1",
			Input: map[string]any{"output": "sunny", "is_error": false},
		},
	})
	done := eventData(t, eventByType(t, evs2, "response.output_item.done"))
	doneItem := done["item"].(map[string]any)
	if doneItem["status"] != "completed" || doneItem["output"] != "sunny" {
		t.Fatalf("bad mcp_call done: %v", doneItem)
	}
}

func TestMcpToolResultErrorEmitsFailed(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_start", Message: anthropic.Message{ID: "m", Model: "x"}})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "mcp_tool_use", ID: "toolu_mcp2", Name: "get",
			Input: map[string]any{"server_name": "w", "name": "get", "arguments": "{}"},
		},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "mcp_tool_result", ToolUseID: "toolu_mcp2",
			Input: map[string]any{"output": "boom", "is_error": true},
		},
	})
	eventByType(t, evs, "response.mcp_call.failed")
	done := eventData(t, eventByType(t, evs, "response.output_item.done"))
	if done["item"].(map[string]any)["status"] != "failed" {
		t.Fatalf("expected failed status")
	}
}

func decodePayload(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("bad sse data: %v", err)
	}
	return m
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// 验证 plaintext thinking 的 signature 同步写入 encrypted_content，
// 使 disable_response_storage=true 时 Codex 能通过标准字段回传。
func TestPlaintextThinkingSetsEncryptedContent(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "thinking"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "thinking_delta", Thinking: "reasoning here"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_delta",
		Index: 0,
		Delta: anthropic.MessageStreamEventUnionDelta{Type: "signature_delta", Signature: "sig123"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_stop",
		Index: 0,
	})

	items := c.OutputItems()
	if len(items) != 1 || items[0].Type != "reasoning" {
		t.Fatalf("expected one reasoning item, got %+v", items)
	}
	if items[0].EncryptedContent != "sig123" {
		t.Fatalf("expected encrypted_content %q, got %q", "sig123", items[0].EncryptedContent)
	}
	if items[0].Signature != "sig123" {
		t.Fatalf("expected signature %q, got %q", "sig123", items[0].Signature)
	}
	if len(items[0].Summary) != 1 || items[0].Summary[0].Text != "reasoning here" {
		t.Fatalf("bad summary: %+v", items[0].Summary)
	}
}

// TestUsageRecordsCacheTokens 复现 gap③:上游 message_delta 的 usage 含
// cache_read_input_tokens / cache_creation_input_tokens,converter 必须透传,
// 否则日志无法观测缓存命中。
func TestUsageRecordsCacheTokens(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: "end_turn"},
		Usage: anthropic.MessageDeltaUsage{
			InputTokens:              50,
			OutputTokens:             10,
			CacheReadInputTokens:     1000,
			CacheCreationInputTokens: 200,
		},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})
	u := c.Usage()
	if u == nil {
		t.Fatal("expected usage after message_delta")
	}
	if u.CacheReadInputTokens != 1000 || u.CacheCreationInputTokens != 200 {
		t.Fatalf("cache tokens not propagated: %+v", u)
	}
}
