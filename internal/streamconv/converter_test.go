package streamconv

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
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

	for _, typ := range []string{"response.content_part.added", "response.content_part.done"} {
		data := eventData(t, eventByType(t, evs, typ))
		part := data["part"].(map[string]any)
		if got := part["refusal"]; got != "I can't help with that." {
			t.Fatalf("%s should include refusal field, got %#v", typ, part)
		}
		if _, ok := part["text"]; ok {
			t.Fatalf("%s refusal part should not include text field: %#v", typ, part)
		}
	}

	for _, typ := range []string{"response.output_item.added", "response.output_item.done"} {
		data := eventData(t, eventByType(t, evs, typ))
		item := data["item"].(map[string]any)
		content := item["content"].([]any)
		part := content[0].(map[string]any)
		if got := part["refusal"]; got != "I can't help with that." {
			t.Fatalf("%s item content should include refusal field, got %#v", typ, part)
		}
		if _, ok := part["text"]; ok {
			t.Fatalf("%s refusal item content should not include text field: %#v", typ, part)
		}
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
