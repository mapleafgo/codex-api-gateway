package model

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCodeInterpreterCallItemMarshalsRequiredOutputs(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeCodeInterpreterCall, ID: "ci_0", Status: ResponseStatusInProgress,
		ContainerID: "ci_container_0", Code: "print(1)",
		Outputs: []CodeInterpreterOutput{},
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for _, key := range []string{"type", "id", "status", "container_id", "code", "outputs"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("code_interpreter_call wire missing required key %q: %s", key, raw)
		}
	}
	if got["type"] != "code_interpreter_call" {
		t.Fatalf("bad type: %v", got["type"])
	}
}

func TestCodeInterpreterCallItemCarriesLogsOutput(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeCodeInterpreterCall, ID: "ci_0", Status: ResponseStatusCompleted,
		ContainerID: "ci_container_0", Code: "print(1)",
		Outputs: []CodeInterpreterOutput{{Type: "logs", Logs: "1\n"}},
	}
	raw, _ := json.Marshal(item)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	outputs := got["outputs"].([]any)
	first := outputs[0].(map[string]any)
	if first["type"] != "logs" || first["logs"] != "1\n" {
		t.Fatalf("bad logs output: %v", first)
	}
}

// TestCodeInterpreterCallItemNilOutputsMarshalsAsEmptyArray 锁定 nil Outputs
// 必须序列化为 "outputs":[]（OpenAI wire 要求 outputs 为数组，禁止 null）。
func TestCodeInterpreterCallItemNilOutputsMarshalsAsEmptyArray(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeCodeInterpreterCall, ID: "ci_x", Status: ResponseStatusInProgress,
		// Outputs 字段故意留 nil
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	outputs, ok := got["outputs"].([]any)
	if !ok {
		t.Fatalf("outputs not a JSON array (got %T): %s", got["outputs"], raw)
	}
	if len(outputs) != 0 {
		t.Fatalf("outputs want empty [], got %v: %s", outputs, raw)
	}
	// 同时校验 wire 文本里不含 "null"（防回归）
	if bytes.Contains(raw, []byte(`"outputs":null`)) {
		t.Fatalf("outputs marshalled as null: %s", raw)
	}
}
