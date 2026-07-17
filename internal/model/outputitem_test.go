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

// TestMcpCallItemMarshalsRequiredFields 锁定 mcp_call wire 的必填字段
// server_label/name/arguments 必须始终输出（OpenAI wire api:"required"）。
func TestMcpCallItemMarshalsRequiredFields(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeMcpCall, ID: "mcp_0", Status: ResponseStatusInProgress,
		ServerLabel: "weather", Name: "get_forecast", Arguments: `{"city":"sf"}`,
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for _, key := range []string{"type", "id", "server_label", "name", "arguments"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("mcp_call wire missing required key %q: %s", key, raw)
		}
	}
	if got["type"] != "mcp_call" {
		t.Fatalf("bad type: %v", got["type"])
	}
}

// TestMcpCallItemEmptyFieldsStillEmitRequiredKeys 锁定即使 server_label/name/
// arguments 为空字符串也必须输出键（omitempty 会丢键 → dedicated branch 必须无 omitempty）。
func TestMcpCallItemEmptyFieldsStillEmitRequiredKeys(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeMcpCall, ID: "mcp_1", Status: ResponseStatusInProgress,
		// ServerLabel / Name / Arguments 故意留空
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for _, key := range []string{"server_label", "name", "arguments"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("mcp_call wire missing required key %q when empty: %s", key, raw)
		}
	}
}

// TestMcpCallItemCarriesOutputForFailedState 锁定 failed 状态下错误文本写入
// Output 字段（OutputItem 无 Error 字段，按 Task B1 设计）。
func TestMcpCallItemCarriesOutputForFailedState(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeMcpCall, ID: "mcp_2", Status: ResponseStatusFailed,
		ServerLabel: "weather", Name: "get_forecast", Arguments: "{}",
		Output: "tool connection refused",
	}
	raw, _ := json.Marshal(item)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if got["status"] != "failed" {
		t.Fatalf("bad status: %v", got["status"])
	}
	if got["output"] != "tool connection refused" {
		t.Fatalf("bad output: %v", got["output"])
	}
	// 确保没有 error 字段（OutputItem 无 Error 字段，按设计）
	if _, ok := got["error"]; ok {
		t.Fatalf("mcp_call wire must not emit error field: %s", raw)
	}
}
