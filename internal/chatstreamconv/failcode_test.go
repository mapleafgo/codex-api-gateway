package chatstreamconv

import (
	"encoding/json"
	"testing"
)

func TestFailWithCodeSetsErrorCode(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	evs := c.FailWithCode("upstream context error", "context_length_exceeded")
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	var m map[string]any
	if err := json.Unmarshal(evs[0].Data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resp, _ := m["response"].(map[string]any)
	errObj, _ := resp["error"].(map[string]any)
	if errObj["code"] != "context_length_exceeded" {
		t.Fatalf("want error.code=context_length_exceeded, got %v", errObj)
	}
}

func TestFailWithoutCodeKeepsLegacyShape(t *testing.T) {
	c := New()
	c.SetClientModel("m")
	evs := c.Fail("upstream reset")
	var m map[string]any
	if err := json.Unmarshal(evs[0].Data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resp, _ := m["response"].(map[string]any)
	errObj, _ := resp["error"].(map[string]any)
	if _, has := errObj["code"]; has {
		t.Fatalf("legacy Fail must not add error.code, got %v", errObj)
	}
}
