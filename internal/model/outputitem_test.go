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

// TestToolSearchCallItemMarshalsRequiredFields 锁定 tool_search_call wire
// 的必填字段 id/call_id/arguments/execution/status/type，且不带 name
// （tool_search_call 无 name 字段，与 function_call 区分）。
func TestToolSearchCallItemMarshalsRequiredFields(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeToolSearchCall, ID: "tsc_0", CallID: "call_ts",
		Arguments: `{"query":"fetch"}`, Execution: "client", Status: ResponseStatusCompleted,
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for _, key := range []string{"type", "id", "call_id", "arguments", "execution", "status"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("tool_search_call wire missing required key %q: %s", key, raw)
		}
	}
	if got["type"] != "tool_search_call" {
		t.Fatalf("bad type: %v", got["type"])
	}
	// arguments 必须是 JSON object（map），不是 string——Codex serde 反序列化成
	// SearchToolCallParams struct，string 会导致 parse 失败 → tool_search 返回空。
	if _, ok := got["arguments"].(map[string]any); !ok {
		t.Fatalf("tool_search_call arguments must be JSON object (not string), got %T: %s", got["arguments"], raw)
	}
	if got["execution"] != "client" {
		t.Fatalf("bad execution: %v", got["execution"])
	}
	// tool_search_call 无 name 字段（默认别名 marshal 会带 name:""，必须排除）
	if _, ok := got["name"]; ok {
		t.Fatalf("tool_search_call wire must not emit name: %s", raw)
	}
}

// TestToolSearchCallItemEmptyFieldsStillEmitRequiredKeys 锁定即使 arguments
// 为空也必须输出键（dedicated MarshalJSON 分支无 omitempty，对齐 mcp_call）。
func TestToolSearchCallItemEmptyFieldsStillEmitRequiredKeys(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeToolSearchCall, ID: "tsc_1", CallID: "call_ts2",
		Execution: "client", Status: ResponseStatusInProgress,
		// Arguments 故意留空
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for _, key := range []string{"id", "call_id", "arguments", "execution"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("tool_search_call wire missing required key %q when empty: %s", key, raw)
		}
	}
}

// TestReasoningItemMarshalsRequiredSummary 锁定 reasoning item 的 summary 字段
// 必须始终输出（即使空/nil）—— OpenAI wire api:"required"，且 Codex 的
// ResponseItem Reasoning 变体 summary 是无 #[serde(default)] 的 required Vec，
// 缺失会导致 serde 解析失败、output_item.added 被丢弃、active_item 不被设置，
// 表现为 Codex 日志 "ReasoningSummaryPartAdded without active item" ERROR。
func TestReasoningItemMarshalsRequiredSummary(t *testing.T) {
	cases := []struct {
		name    string
		summary []OutputText
	}{
		{"empty slice", []OutputText{}},
		{"nil", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := OutputItem{
				Type: ItemTypeReasoning, ID: "rs_0", Status: ResponseStatusInProgress,
				Summary: tc.summary,
			}
			raw, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			_ = json.Unmarshal(raw, &got)
			arr, ok := got["summary"].([]any)
			if !ok {
				t.Fatalf("summary must be a JSON array (got %T): %s", got["summary"], raw)
			}
			if len(arr) != 0 {
				t.Fatalf("summary want empty [], got %v: %s", arr, raw)
			}
			if bytes.Contains(raw, []byte(`"summary":null`)) {
				t.Fatalf("summary marshalled as null: %s", raw)
			}
		})
	}
}
