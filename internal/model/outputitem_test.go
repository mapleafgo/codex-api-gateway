package model

import (
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
