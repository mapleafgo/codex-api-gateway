package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOutputTextDeltaMarshalClean(t *testing.T) {
	ev := OutputTextDeltaEvent{
		Type: "response.output_text.delta", SequenceNumber: 3,
		OutputIndex: 1, ContentIndex: 0, ItemID: "msg_1", Delta: "hi",
	}
	b, _ := json.Marshal(ev)
	s := string(b)
	for _, bad := range []string{"logprobs", "refusal", "code", "param"} {
		if strings.Contains(s, bad) {
			t.Fatalf("unexpected field %q in %s", bad, s)
		}
	}
	if !strings.Contains(s, `"delta":"hi"`) || !strings.Contains(s, `"sequence_number":3`) {
		t.Fatalf("missing expected fields: %s", s)
	}
}

func TestResponseObjectMarshalHasRequired(t *testing.T) {
	temp := 0.7
	maxTok := int64(4096)
	obj := NewResponseObject("resp_1", "completed", "gpt-5", 100, ResponseObjectParams{
		Temperature:     &temp,
		MaxOutputTokens: &maxTok,
	})
	ev := TerminalResponseEvent{
		Type: "response.completed", SequenceNumber: 9, Response: obj,
	}
	b, _ := json.Marshal(ev)
	s := string(b)
	for _, field := range []string{`"object":"response"`, `"output":[`, `"created_at":`, `"id":"resp_1"`, `"sequence_number":9`} {
		if !strings.Contains(s, field) {
			t.Fatalf("missing %q in %s", field, s)
		}
	}
}

func TestOutputItemMarshalClean(t *testing.T) {
	item := OutputItem{
		Type: "message", ID: "msg_0", Role: "assistant",
		Content: []OutputText{{Type: "output_text", Text: "hi"}},
	}
	b, _ := json.Marshal(item)
	s := string(b)
	for _, bad := range []string{"call_id", "arguments", "summary", "encrypted_content"} {
		if strings.Contains(s, bad) {
			t.Fatalf("unexpected field %q in %s", bad, s)
		}
	}
}
