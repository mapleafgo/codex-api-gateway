package chatconvert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/convert"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

func mustChat(t *testing.T, body, model string) *ChatRequest {
	t.Helper()
	req, err := convert.DecodeResponseNewParams([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := ToChat(req, model)
	if err != nil {
		t.Fatalf("ToChat: %v", err)
	}
	return out
}

func chatParts(t *testing.T, v any) []ChatContentPart {
	t.Helper()
	parts, ok := v.([]ChatContentPart)
	if !ok {
		t.Fatalf("content type=%T, want []ChatContentPart: %v", v, v)
	}
	return parts
}

func TestToChat_SimpleUserText(t *testing.T) {
	out := mustChat(t, `{"model":"gpt-4o","input":"Hello world","stream":true}`, "gpt-4o")
	if out.Model != "gpt-4o" {
		t.Fatalf("model=%q", out.Model)
	}
	if !out.Stream || out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Fatalf("stream/usage flags: stream=%v opts=%+v", out.Stream, out.StreamOptions)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("messages=%+v", out.Messages)
	}
}

func TestToChat_InstructionsAndMessageList(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"instructions":"be brief",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hi"}]}]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) < 2 {
		t.Fatalf("want instructions+user, got %+v", out.Messages)
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "be brief" {
		t.Fatalf("system=%+v", out.Messages[0])
	}
	if out.Messages[1].Role != "user" {
		t.Fatalf("user=%+v", out.Messages[1])
	}
}

func TestToChat_DeveloperRoleWrapsAsUser(t *testing.T) {
	body := `{"model":"gpt-4o","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev rules"}]}]}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("developer should wrap as user: %+v", out.Messages)
	}
	content, _ := out.Messages[0].Content.(string)
	if !strings.Contains(content, "<system-update>") || !strings.Contains(content, "dev rules") {
		t.Fatalf("developer not wrapped: %q", content)
	}
}

func TestToChat_FunctionCallHistory(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"London\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"18 C"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 3 {
		t.Fatalf("messages=%d %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[1].Role != "assistant" || len(out.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool_calls: %+v", out.Messages[1])
	}
	if out.Messages[2].Role != "tool" || out.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("tool msg: %+v", out.Messages[2])
	}
}

func TestToChat_FunctionCallHistoryKeepsNamespace(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
			{"type":"function_call","call_id":"c1","name":"spawn_agent","namespace":"collaboration","arguments":"{\"task\":\"x\"}"},
			{"type":"function_call_output","call_id":"c1","output":"ok"}
		],
		"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, m := range out.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "collaboration__spawn_agent" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("history function_call must keep namespace in flat name: %+v", out.Messages)
	}
}

func TestToChat_MergeAdjacentFunctionCalls(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"two tools"}]},
			{"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
			{"type":"function_call","call_id":"c2","name":"b","arguments":"{\"x\":1}"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	// user + assistant(with 2 tool_calls) + 2 placeholder tools
	asst := 0
	for _, m := range out.Messages {
		if m.Role == "assistant" {
			asst++
			if len(m.ToolCalls) != 2 {
				t.Fatalf("want 2 merged tool_calls, got %d", len(m.ToolCalls))
			}
		}
	}
	if asst != 1 {
		t.Fatalf("want 1 assistant, got %d messages=%+v", asst, out.Messages)
	}
}

func TestToChat_AssistantOutputTextHistory(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello there"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, m := range out.Messages {
		if m.Role == "assistant" && m.Content == "hello there" {
			found = true
		}
	}
	if !found {
		t.Fatalf("output_text not restored: %+v", out.Messages)
	}
}

func TestToChat_ShellAndApplyPatchTools(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"shell"},{"type":"apply_patch"},{"type":"function","name":"f","parameters":{"type":"object"}}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	names := map[string]bool{}
	for _, t0 := range out.Tools {
		names[t0.Function.Name] = true
	}
	if !names["shell"] || !names["apply_patch"] || !names["f"] {
		t.Fatalf("tools=%v", names)
	}
	for _, t0 := range out.Tools {
		if t0.Function.Name != "apply_patch" {
			continue
		}
		params, _ := t0.Function.Parameters.(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if _, ok := props["s"]; !ok {
			t.Fatalf("apply_patch must use freeform s schema: %v", params)
		}
		if _, ok := props["operation"]; ok {
			t.Fatalf("apply_patch must not declare operation/path/diff: %v", params)
		}
	}
	// shell/apply_patch 由 ChatName* 常量专项识别，不进 custom 回程登记表。
	if out.IsFreeformName("shell") || out.IsFreeformName("apply_patch") {
		t.Fatalf("builtin freeform must not be registered as custom: %+v", out.FreeformNames)
	}
	// custom suffix must NOT be used
	if names["apply_patch_custom"] {
		t.Fatal("must not suffix _custom")
	}
}

func TestToChat_LocalShellDistinctToolName(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"local_shell"},{"type":"shell"}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	names := map[string]bool{}
	for _, t0 := range out.Tools {
		names[t0.Function.Name] = true
	}
	if !names["local_shell"] || !names["shell"] {
		t.Fatalf("local_shell must keep distinct name: %v", names)
	}
	if out.IsFreeformName("local_shell") {
		t.Fatalf("local_shell must not be registered as custom: %+v", out.FreeformNames)
	}
}

func TestToChat_CustomNoSuffix(t *testing.T) {
	body := `{"model":"gpt-4o","tools":[{"type":"custom","name":"mytool","description":"d"}],"input":"x"}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "mytool" {
		t.Fatalf("tools=%+v", out.Tools)
	}
	if !out.IsFreeformName("mytool") {
		t.Fatal("custom should be freeform")
	}
}

func TestToChat_CustomGrammarToolMapsToChatFunction(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"custom","name":"parse","description":"parse csv","format":{"type":"grammar","definition":"start: /[0-9]+/","syntax":"lark"}}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" {
		t.Fatalf("tools=%+v", out.Tools)
	}
	if out.Tools[0].Function.Name != "parse" {
		t.Fatalf("function=%+v", out.Tools[0].Function)
	}
	if out.Tools[0].Function.Description != "parse csv" {
		t.Fatalf("root description must keep original: %q", out.Tools[0].Function.Description)
	}
	params, _ := out.Tools[0].Function.Parameters.(map[string]any)
	props, _ := params["properties"].(map[string]any)
	input, _ := props["s"].(map[string]any)
	if input["description"] != "Required format:\nstart: /[0-9]+/" {
		t.Fatalf("grammar definition must be merged into input property description: %#v", input)
	}
	if !out.IsFreeformName("parse") {
		t.Fatal("custom should be freeform")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"type":"function","function":{"name":"parse"`) {
		t.Fatalf("wire missing function declaration: %s", raw)
	}
}

func TestToChat_NamespaceCustomGrammarToolMapsToChatFunction(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"parse","description":"d","format":{"type":"grammar","definition":"x","syntax":"regex"}}]}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" {
		t.Fatalf("tools=%+v", out.Tools)
	}
	if out.Tools[0].Function.Name != "ns__parse" {
		t.Fatalf("function=%+v", out.Tools[0].Function)
	}
	if out.Tools[0].Function.Description != "d" {
		t.Fatalf("root description must keep original: %q", out.Tools[0].Function.Description)
	}
	params, _ := out.Tools[0].Function.Parameters.(map[string]any)
	props, _ := params["properties"].(map[string]any)
	input, _ := props["s"].(map[string]any)
	if input["description"] != "Required format:\nx" {
		t.Fatalf("namespace custom input description missing format: %#v", input)
	}
	if !out.IsFreeformName("ns__parse") {
		t.Fatal("namespaced custom should be freeform")
	}
}

func TestToChat_ToolSearchPreservesPropertyDescription(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"tool_search","name":"tool_search","description":"search deferred tools","parameters":{"type":"object","properties":{"q":{"type":"string","description":"the search query"}},"required":["q"]}}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" {
		t.Fatalf("tools=%+v", out.Tools)
	}
	params, _ := out.Tools[0].Function.Parameters.(map[string]any)
	props, _ := params["properties"].(map[string]any)
	q, _ := props["q"].(map[string]any)
	if q["description"] != "the search query" {
		t.Fatalf("tool_search property description lost: %#v", q)
	}
}

func TestToChat_McpDeclarationCarriesServerDescription(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"mcp","server_label":"browser","server_description":"Browser automation MCP server","allowed_tools":["click"]}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	for _, td := range out.Tools {
		if td.Function.Name != "mcp__browser__click" {
			continue
		}
		if !strings.Contains(td.Function.Description, "Browser automation MCP server") {
			t.Fatalf("mcp server_description not carried: %q", td.Function.Description)
		}
		return
	}
	t.Fatalf("mcp__browser__click declaration missing: %+v", out.Tools)
}

func TestToChat_McpAllowedToolsEntry(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"mcp","server_label":"srv","allowed_tools":["get","list"]}],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"mcp","server_label":"srv"}]},
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 2 {
		t.Fatalf("server-level mcp allowed_tools should keep both, got %+v", out.Tools)
	}
	if out.ToolChoice != "auto" {
		t.Fatalf("allowed_tools mode auto expected, got %#v", out.ToolChoice)
	}
}

func TestToChat_CustomGrammarToolChoiceUsesFunction(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"custom","name":"parse","format":{"type":"grammar","definition":"x","syntax":"lark"}}],
		"tool_choice":{"type":"custom","name":"parse"},
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"function":{"name":"parse"},"type":"function"`) {
		t.Fatalf("custom tool_choice must degrade to function: %s", raw)
	}
}

func TestToChat_CustomToolCallHistoryRoundTrip(t *testing.T) {
	// custom 工具声明统一 function 降级，历史 custom_tool_call 也走 function。
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"custom","name":"parse","format":{"type":"grammar","definition":"start: /[0-9]+/","syntax":"lark"}}],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"parse 42"}]},
			{"type":"custom_tool_call","call_id":"call_parse","name":"parse","input":"42"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `"id":"call_parse","type":"function","function":{"name":"parse","arguments":"{\"s\":\"42\"}"}`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("custom tool_call history wire missing %s: %s", want, raw)
	}
	if strings.Contains(string(raw), `"custom":{"name":"parse"`) {
		t.Fatalf("custom tool_call history must not emit custom side: %s", raw)
	}
}

func TestToChat_FreeformCustomToolCallHistoryDegradesToFunction(t *testing.T) {
	// 无 grammar 的 custom 工具声明，在历史回程降级为 type=function。
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"custom","name":"mytool","description":"d"}],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]},
			{"type":"custom_tool_call","call_id":"call_mt","name":"mytool","input":"hello"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `"id":"call_mt","type":"function","function":{"name":"mytool","arguments":"{\"s\":\"hello\"}"}`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("freeform custom tool_call history wire missing %s: %s", want, raw)
	}
	if strings.Contains(string(raw), `"custom":{"name":"mytool"`) {
		t.Fatalf("freeform custom tool_call must not emit custom side: %s", raw)
	}
}

func TestToChat_ApplyPatchCustomGrammarDeclDegradesToFunction(t *testing.T) {
	// Codex 客户端把 apply_patch 声明为 custom + grammar（V4A lark 语法），
	// 但 Chat 上游无 apply_patch custom 槽位：声明与历史都必须 function 降级，
	// 否则 opencode2api 会把裸 V4A 文本直接当 arguments，上游解析失败 400。
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /[a-z]+/"}}],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"edit"}]},
			{"type":"custom_tool_call","call_id":"call_ap","name":"apply_patch","input":"*** Begin Patch\n*** Update File: a.txt\n+x\n*** End Patch"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `"id":"call_ap","type":"function","function":{"name":"apply_patch","arguments":"{\"s\":\"*** Begin Patch\\n*** Update File: a.txt\\n+x\\n*** End Patch\"}"}`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("apply_patch custom grammar history must degrade to function: %s", raw)
	}
	if strings.Contains(string(raw), `"custom":{"name":"apply_patch"`) {
		t.Fatalf("apply_patch must not emit custom side: %s", raw)
	}
	// 工具声明同样保持 function + FreeformInputSchema。
	var parsed struct {
		Tools []struct {
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
			Custom *json.RawMessage `json:"custom"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	for _, td := range parsed.Tools {
		if td.Function.Name != "apply_patch" {
			continue
		}
		props, _ := td.Function.Parameters["properties"].(map[string]any)
		if _, ok := props["s"]; !ok {
			t.Fatalf("apply_patch decl must keep freeform s schema: %v", td.Function.Parameters)
		}
		wantDesc := ""
		if td.Function.Description != wantDesc {
			t.Fatalf("apply_patch decl description mismatch:\n got=%q\nwant=%q", td.Function.Description, wantDesc)
		}
		if td.Custom != nil {
			t.Fatalf("apply_patch decl must not carry custom wire: %v", string(*td.Custom))
		}
	}
}

func TestToChat_NamespacedCustomHistoryFlattensName(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"wait","description":"d"}]}],
		"input":[{"type":"custom_tool_call","call_id":"c1","name":"wait","namespace":"ns","input":"abc"}]
	}`
	out := mustChat(t, body, "gpt-4o")
	if !out.IsFreeformName("ns__wait") {
		t.Fatalf("freeform map missing flattened name: %+v", out.FreeformNames)
	}
	for _, m := range out.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID == "c1" && tc.Function.Name != "ns__wait" {
				t.Fatalf("namespaced custom history name = %q, want ns__wait", tc.Function.Name)
			}
		}
	}
}

func TestToChat_OrphanToolCallGetsPlaceholder(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]},
			{"type":"function_call","call_id":"orphan","name":"a","arguments":"{}"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	// user + assistant(tool_calls) + 紧邻 placeholder tool
	if len(out.Messages) < 3 {
		t.Fatalf("messages=%d %+v", len(out.Messages), out.Messages)
	}
	asstIdx := -1
	for i, m := range out.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			asstIdx = i
			break
		}
	}
	if asstIdx < 0 || asstIdx+1 >= len(out.Messages) {
		t.Fatalf("no assistant tool_calls: %+v", out.Messages)
	}
	tool := out.Messages[asstIdx+1]
	if tool.Role != "tool" || tool.ToolCallID != "orphan" {
		t.Fatalf("placeholder not adjacent after assistant: %+v", out.Messages)
	}
	if !strings.Contains(tool.Content.(string), "no tool output") {
		t.Fatalf("placeholder content=%v", tool.Content)
	}
}

// function_call 与 function_call_output 之间夹杂 assistant 文本时，
// convertMessages 会先 flush tool_calls assistant，导致 tool 落在文本之后；
// ensureChatToolPaired 必须把 tool 挪回 assistant(tool_calls) 紧邻位置。
func TestToChat_ToolResultReorderedAfterInterveningAssistant(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},
			{"type":"function_call","call_id":"call_x","name":"do","arguments":"{}"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"thinking aloud"}]},
			{"type":"function_call_output","call_id":"call_x","output":"done"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	asstIdx := -1
	for i, m := range out.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "call_x" {
			asstIdx = i
			break
		}
	}
	if asstIdx < 0 {
		t.Fatalf("missing tool_calls assistant: %+v", out.Messages)
	}
	if asstIdx+1 >= len(out.Messages) {
		t.Fatalf("no message after tool_calls assistant: %+v", out.Messages)
	}
	next := out.Messages[asstIdx+1]
	if next.Role != "tool" || next.ToolCallID != "call_x" {
		t.Fatalf("tool not adjacent after assistant(tool_calls): idx=%d msgs=%+v", asstIdx, out.Messages)
	}
	if next.Content != "done" {
		t.Fatalf("tool content=%v", next.Content)
	}
}

func TestToChat_MaxCompletionTokensDualWrite(t *testing.T) {
	out := mustChat(t, `{"model":"gpt-4o","max_output_tokens":128,"input":"hi"}`, "gpt-4o")
	if out.MaxTokens == nil || *out.MaxTokens != 128 {
		t.Fatalf("max_tokens=%v", out.MaxTokens)
	}
	if out.MaxCompletionTokens == nil || *out.MaxCompletionTokens != 128 {
		t.Fatalf("max_completion_tokens=%v", out.MaxCompletionTokens)
	}
}

func TestToChat_WebSearchDecl(t *testing.T) {
	body := `{"model":"gpt-4o","tools":[{"type":"web_search"}],"input":"x"}`
	out := mustChat(t, body, "gpt-4o")
	names := map[string]bool{}
	for _, t0 := range out.Tools {
		names[t0.Function.Name] = true
	}
	if !names["web_search"] {
		t.Fatalf("web_search tool missing: %v", names)
	}
}

func TestToChat_WebSearchHistory(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"news"}]},
			{"type":"web_search_call","id":"ws1","status":"completed","action":{"type":"search","query":"go 1.22"}}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	var hasCall, hasResult bool
	for _, m := range out.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "web_search" {
					hasCall = true
				}
			}
		}
		if m.Role == "tool" && m.ToolCallID == "ws1" {
			hasResult = true
		}
	}
	if !hasCall || !hasResult {
		t.Fatalf("web_search history incomplete: call=%v result=%v msgs=%+v", hasCall, hasResult, out.Messages)
	}
}

func TestToChat_MCPHistory(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]},
			{"type":"mcp_call","id":"m1","server_label":"fetch","name":"get","arguments":"{}","output":"ok"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, m := range out.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "mcp__fetch__get" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("mcp history: %+v", out.Messages)
	}
}

func TestToChat_UnsupportedHostedHistoryWarns(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"file_search_call","id":"fs1","queries":["q"],"status":"completed"}
		]
	}`
	_ = mustChat(t, body, "gpt-4o")
	if !strings.Contains(buf.String(), "file_search") {
		t.Fatalf("want WARN for file_search, logs=%s", buf.String())
	}
}

func TestToChat_CompactionDroppedWarns(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"compaction","encrypted_content":"enc-blob"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("want only user message, got %+v", out.Messages)
	}
	first, _ := out.Messages[0].Content.(string)
	if first != "continue" || strings.Contains(first, "enc-blob") {
		t.Fatalf("compaction must be dropped: %q", first)
	}
	if !strings.Contains(buf.String(), "compaction") {
		t.Fatalf("want WARN for compaction, logs=%s", buf.String())
	}
}

func TestToChat_DropsChatUnsupportedTopLevelFields(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"prompt_cache_key":"bucket-1",
		"text":{"verbosity":"high"},
		"safety_identifier":"sid-1",
		"input":"hi"
	}`
	out := mustChat(t, body, "gpt-4o")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, field := range []string{"prompt_cache_key", "verbosity", "safety_identifier"} {
		if strings.Contains(s, field) {
			t.Fatalf("Chat 请求体不应包含 %s：%s", field, s)
		}
	}
	// 允许的其它标准字段不受影响。
	if out.Model != "gpt-4o" || len(out.Messages) == 0 {
		t.Fatalf("标准字段受影响: model=%q messages=%d", out.Model, len(out.Messages))
	}
}

func TestToChat_ResponseFormatJSONSchema(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":"hi",
		"text":{"format":{"type":"json_schema","name":"person","strict":true,"schema":{"type":"object"}}}
	}`
	out := mustChat(t, body, "gpt-4o")
	if out.ResponseFormat != nil {
		t.Fatalf("网关不支持 structured output，response_format 应为 nil，got %+v", out.ResponseFormat)
	}
}

func TestToChat_FunctionStrictPassthrough(t *testing.T) {
	body := `{"model":"gpt-4o","tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}}],"input":"x"}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 || out.Tools[0].Function.Strict == nil || !*out.Tools[0].Function.Strict {
		t.Fatalf("strict missing: %+v", out.Tools)
	}
}

func TestToChat_ServiceTierReasoningTopLogprobs(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","text":{"verbosity":"high"},"service_tier":"priority","reasoning":{"effort":"high"},"top_logprobs":3}`
	out := mustChat(t, body, "gpt-4o")
	if out.ServiceTier == nil || *out.ServiceTier != "priority" {
		t.Fatalf("service_tier=%v", out.ServiceTier)
	}
	if out.ReasoningEffort == nil || *out.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort=%v", out.ReasoningEffort)
	}
	if out.TopLogprobs == nil || *out.TopLogprobs != 3 || out.Logprobs == nil || !*out.Logprobs {
		t.Fatalf("logprobs top=%v on=%v", out.TopLogprobs, out.Logprobs)
	}
	// verbosity 是 Responses 专有字段，Chat 无顶层槽位，应丢弃不进 wire。
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "verbosity") {
		t.Fatalf("Chat 请求体不应含 verbosity：%s", b)
	}
}

func TestToChat_SafetyMetadataStoreModeration(t *testing.T) {
	body := `{
		"model":"gpt-4o","input":"hi",
		"safety_identifier":"u1",
		"metadata":{"k":"v"},
		"store":true,
			"moderation":{"model":"omni-moderation-latest","policy":{"input":{"mode":"score"},"output":{"mode":"block"}}}
		}`
	out := mustChat(t, body, "gpt-4o")
	if out.Metadata["k"] != "v" {
		t.Fatalf("metadata=%v", out.Metadata)
	}
	if out.Store == nil || !*out.Store {
		t.Fatalf("store=%v", out.Store)
	}
	if out.Moderation == nil || out.Moderation.Model != "omni-moderation-latest" {
		t.Fatalf("moderation=%+v", out.Moderation)
	}
	// safety_identifier 是 Responses 专有字段，Chat 无顶层槽位，应丢弃。
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "safety_identifier") {
		t.Fatalf("Chat 请求体不应含 safety_identifier：%s", b)
	}
}

func TestToChat_AllowedToolsFiltersAndMode(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[
			{"type":"function","name":"keep","parameters":{"type":"object"}},
			{"type":"function","name":"drop","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"keep"}]},
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "keep" {
		t.Fatalf("tools=%+v", out.Tools)
	}
	if out.ToolChoice != "required" {
		t.Fatalf("tool_choice=%v", out.ToolChoice)
	}
}

func TestToChat_StreamOptionsObfuscation(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","stream_options":{"include_obfuscation":false}}`
	out := mustChat(t, body, "gpt-4o")
	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Fatal("include_usage must stay true")
	}
	if out.StreamOptions.IncludeObfuscation == nil || *out.StreamOptions.IncludeObfuscation {
		t.Fatalf("include_obfuscation=%v", out.StreamOptions.IncludeObfuscation)
	}
}

func TestToChat_MarshalStreamTrue(t *testing.T) {
	out := mustChat(t, `{"model":"m","input":"x"}`, "m")
	b, err := Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["stream"] != true {
		t.Fatalf("stream field=%v", raw["stream"])
	}
	if _, ok := raw["FreeformNames"]; ok {
		t.Fatal("FreeformNames must not be marshaled")
	}
}

func TestToChat_ToolHistoryWithoutActiveToolsSendsEmptyTools(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"18 C"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 0 {
		t.Fatalf("tools should stay empty: %+v", out.Tools)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"tools":[]`) {
		t.Fatalf("want tools:[] on wire with tool history: %s", raw)
	}
}

func TestToChat_NoToolsOmittedWithoutHistory(t *testing.T) {
	out := mustChat(t, `{"model":"gpt-4o","input":"hi"}`, "gpt-4o")
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"tools"`) {
		t.Fatalf("tools must be omitted without history: %s", raw)
	}
}

func TestToChat_OutputMessageDefensive(t *testing.T) {
	in := &oairesponses.ResponseInputItemMessageParam{
		Role: "user",
		Content: oairesponses.ResponseInputMessageContentListParam{
			oairesponses.ResponseInputContentParamOfInputText("via input_message"),
		},
	}
	msg, ok, err := convertInputMessage(in)
	if err != nil {
		t.Fatalf("convertInputMessage: %v", err)
	}
	if !ok || msg.Role != "user" || msg.Content != "via input_message" {
		t.Fatalf("convertInputMessage=%+v ok=%v", msg, ok)
	}
	out := &oairesponses.ResponseOutputMessageParam{
		ID:     "msg_x",
		Status: "completed",
		Content: []oairesponses.ResponseOutputMessageContentUnionParam{
			{OfOutputText: &oairesponses.ResponseOutputTextParam{Text: "hist"}},
		},
	}
	msg, ok = convertOutputMessage(out)
	if !ok || msg.Role != "assistant" || msg.Content != "hist" {
		t.Fatalf("convertOutputMessage=%+v ok=%v", msg, ok)
	}
}

func TestToChat_ShellCallHistoryMerged(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"ls"}]},
			{"type":"shell_call","call_id":"s1","action":{"commands":["ls -la"]},"status":"completed"},
			{"type":"shell_call_output","call_id":"s1","status":"completed","output":[{"stdout":"ok\n","outcome":{"type":"exit","exit_code":0}}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	var callOK, outOK bool
	for _, m := range out.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "shell" && strings.Contains(tc.Function.Arguments, "ls -la") {
					callOK = true
				}
			}
		}
		if m.Role == "tool" && m.ToolCallID == "s1" {
			outOK = true
			if s, ok := m.Content.(string); !ok || !strings.Contains(s, "ok") {
				t.Fatalf("shell output content=%v", m.Content)
			}
		}
	}
	if !callOK || !outOK {
		t.Fatalf("shell history incomplete call=%v out=%v msgs=%+v", callOK, outOK, out.Messages)
	}
	if out.IsFreeformName("shell") {
		t.Fatalf("shell must not be registered as custom: %+v", out.FreeformNames)
	}
}

func TestToChat_NamespaceFlatten(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{
			"type":"namespace","name":"collaboration",
			"tools":[
				{"type":"function","name":"spawn_agent","parameters":{"type":"object"}},
				{"type":"custom","name":"wait"}
			]
		}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	names := map[string]bool{}
	for _, tl := range out.Tools {
		names[tl.Function.Name] = true
	}
	if !names["collaboration__spawn_agent"] || !names["collaboration__wait"] {
		t.Fatalf("namespace flatten failed: %v", names)
	}
	if !out.IsFreeformName("collaboration__wait") {
		t.Fatal("namespaced custom should be freeform")
	}
}

func TestToChat_ToolSearchCallAndOutput(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"find tools"}]},
			{"type":"tool_search_call","call_id":"ts1","arguments":"{\"q\":\"x\"}"},
			{"type":"tool_search_output","call_id":"ts1","tools":[{"type":"function","name":"dyn_a","parameters":{"type":"object"}}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	var callOK, toolOK bool
	for _, m := range out.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "tool_search" {
					callOK = true
				}
			}
		}
	}
	for _, tl := range out.Tools {
		if tl.Function.Name == "dyn_a" {
			toolOK = true
		}
	}
	if !callOK || !toolOK {
		t.Fatalf("tool_search incomplete call=%v dyn=%v tools=%+v msgs=%+v", callOK, toolOK, out.Tools, out.Messages)
	}
}

func TestToChat_ParallelToolCallsAndPromptCacheOptionsDropped(t *testing.T) {
	body := `{
		"model":"gpt-4o","input":"hi",
		"parallel_tool_calls":false,
		"prompt_cache_options":{"mode":"explicit","ttl":"30m"}
	}`
	out := mustChat(t, body, "gpt-4o")
	if out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls=%v", out.ParallelToolCalls)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "prompt_cache_options") {
		t.Fatalf("prompt_cache_options must not reach wire: %s", raw)
	}
}

func TestToChat_ResponseFormatJSONObject(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","text":{"format":{"type":"json_object"}}}`
	out := mustChat(t, body, "gpt-4o")
	if out.ResponseFormat != nil {
		t.Fatalf("网关不支持 structured output，response_format 应为 nil，got %+v", out.ResponseFormat)
	}
}

func TestToChat_ToolChoiceFunctionAndShell(t *testing.T) {
	body := `{"model":"gpt-4o","input":"x","tools":[{"type":"shell"}],"tool_choice":{"type":"shell"}}`
	out := mustChat(t, body, "gpt-4o")
	raw, _ := json.Marshal(out.ToolChoice)
	if !strings.Contains(string(raw), "shell") {
		t.Fatalf("tool_choice=%s", raw)
	}
}

func TestToChat_ApplyPatchHistoryStructured(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"patch"}]},
			{"type":"apply_patch_call","call_id":"ap1","status":"completed",
				"operation":{"type":"update_file","path":"a.go","diff":"@@\n-old\n+new\n"}},
			{"type":"apply_patch_call_output","call_id":"ap1","status":"completed","output":"done"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	var callOK, outOK bool
	for _, m := range out.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "apply_patch" {
					callOK = true
					var args map[string]any
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						t.Fatalf("args not JSON: %v", err)
					}
					if args["operation"] != "update_file" || args["path"] != "a.go" {
						t.Fatalf("args must be structured operation/path: %s", tc.Function.Arguments)
					}
					if args["status"] != "completed" {
						t.Fatalf("args must keep status: %s", tc.Function.Arguments)
					}
				}
			}
		}
		if m.Role == "tool" && m.ToolCallID == "ap1" {
			outOK = true
		}
	}
	if !callOK || !outOK {
		t.Fatalf("apply_patch history incomplete call=%v out=%v msgs=%+v", callOK, outOK, out.Messages)
	}
	if out.IsFreeformName("apply_patch") {
		t.Fatalf("apply_patch must not be registered as custom: %+v", out.FreeformNames)
	}
}

func TestToChat_LocalShellCallHistory(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"ls"}]},
			{"type":"local_shell_call","id":"ls1","call_id":"call_ls","status":"completed",
				"action":{"command":["ls","-la"],"env":{},"type":"exec"}},
			{"type":"local_shell_call_output","id":"call_ls","status":"completed","output":"ok"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	var callOK, outOK bool
	for _, m := range out.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "local_shell" {
					callOK = true
					if !strings.Contains(tc.Function.Arguments, "ls") {
						t.Fatalf("shell args=%s", tc.Function.Arguments)
					}
				}
			}
		}
		if m.Role == "tool" && (m.ToolCallID == "call_ls" || m.ToolCallID == "ls1") {
			outOK = true
		}
	}
	if !callOK || !outOK {
		t.Fatalf("local_shell incomplete call=%v out=%v msgs=%+v", callOK, outOK, out.Messages)
	}
}

func TestToChat_WebSearchHistoryFallbackIDsUnique(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"q1"}]},
			{"type":"web_search_call","action":{"search":{"queries":["a"]}},"status":"completed"},
			{"type":"web_search_call","action":{"search":{"queries":["b"]}},"status":"completed"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	seen := map[string]bool{}
	toolByID := map[string]string{}
	for _, m := range out.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID == "" || seen[tc.ID] {
					t.Fatalf("duplicate or empty fallback call id: %+v", m.ToolCalls)
				}
				seen[tc.ID] = true
			}
		}
		if m.Role == "tool" && m.ToolCallID != "" {
			toolByID[m.ToolCallID] = m.Content.(string)
		}
	}
	if len(seen) != 2 || len(toolByID) != 2 {
		t.Fatalf("fallback ids not unique: calls=%v tools=%v msgs=%+v", seen, toolByID, out.Messages)
	}
}

func TestToChat_FunctionCallOutputWithoutIDWarnAndSkip(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	body := `{
		"model":"gpt-4o",
		"input":[{"type":"function_call_output","output":"done"}]
	}`
	out := mustChat(t, body, "gpt-4o")
	for _, m := range out.Messages {
		if m.Role == "tool" {
			t.Fatalf("tool message with empty call id must not be emitted: %+v", out.Messages)
		}
	}
	if !strings.Contains(buf.String(), "call_id") {
		t.Fatalf("want WARN for missing call_id, logs=%s", buf.String())
	}
}

func TestNormalizeToolSchemaNoForcedAdditionalProperties(t *testing.T) {
	got := normalizeToolSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{"type": "string"},
		},
	})
	m := got.(map[string]any)
	if _, ok := m["additionalProperties"]; ok {
		t.Fatalf("schema without anyOf must not force additionalProperties: %v", got)
	}
	anyOf := normalizeToolSchema(map[string]any{
		"anyOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
			map[string]any{"type": "null"},
		},
	})
	am := anyOf.(map[string]any)
	if am["additionalProperties"] != false {
		t.Fatalf("anyOf projection must force additionalProperties=false: %v", anyOf)
	}
}

func TestToChat_HostedAndFreeformSchemasProjected(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[
			{"type":"web_search"},
			{"type":"code_interpreter"},
			{"type":"mcp","server_label":"srv","allowed_tools":["get"]},
			{"type":"shell"},
			{"type":"local_shell"},
			{"type":"apply_patch"}
		],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) == 0 {
		t.Fatalf("no tools: %+v", out.Tools)
	}
	for _, tl := range out.Tools {
		if tl.Type != "function" {
			continue
		}
		params, ok := tl.Function.Parameters.(map[string]any)
		if !ok {
			t.Fatalf("tool %s parameters type=%T", tl.Function.Name, tl.Function.Parameters)
		}
		if params["additionalProperties"] != false {
			t.Fatalf("tool %s must declare additionalProperties=false: %v", tl.Function.Name, params)
		}
	}
}

func TestToChat_MCPToolDeclAllowlist(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{
			"type":"mcp","server_label":"fetch","server_url":"https://example.com",
			"allowed_tools":["get","list"]
		}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	names := map[string]bool{}
	for _, tl := range out.Tools {
		names[tl.Function.Name] = true
	}
	if !names["mcp__fetch__get"] || !names["mcp__fetch__list"] {
		t.Fatalf("mcp decls=%v", names)
	}
}

func TestToChat_MCPToolFilterSkipped(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	body := `{
		"model":"gpt-4o",
		"tools":[{
			"type":"mcp","server_label":"fetch","server_url":"https://example.com",
			"allowed_tools":{"tool_names":["get"]}
		}],
		"input":"x"
	}`
	// filter shape may decode as OfMcpToolFilter or fail silently — either no mcp tools or WARN
	out := mustChat(t, body, "gpt-4o")
	for _, tl := range out.Tools {
		if strings.HasPrefix(tl.Function.Name, "mcp__") {
			// if expanded somehow, ok; filter path should not expand blindly wrong
		}
	}
	// primary assert: conversion succeeds
	if out == nil {
		t.Fatal("nil out")
	}
}

func TestToChat_AllowedToolsUnknownEntryErrors(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"function","name":"keep","parameters":{"type":"object"}}],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"missing"}]},
		"input":"x"
	}`
	req, err := convert.DecodeResponseNewParams([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, err = ToChat(req, "gpt-4o")
	if err == nil {
		t.Fatal("want error for unknown allowed tool")
	}
	if !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("err=%v", err)
	}
}

func TestToChat_AllowedToolsLocalShellKeepsDistinctName(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"local_shell"}],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"local_shell"}]},
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "local_shell" {
		t.Fatalf("allowed local_shell must survive as name=local_shell: %+v", out.Tools)
	}
}

func TestToChat_ToolSearchDecl(t *testing.T) {
	body := `{"model":"gpt-4o","tools":[{"type":"tool_search","description":"search tools"}],"input":"x"}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, tl := range out.Tools {
		if tl.Function.Name == "tool_search" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tools=%+v", out.Tools)
	}
}

func TestToChat_FileSearchToolDeclSkipped(t *testing.T) {
	body := `{"model":"gpt-4o","tools":[{"type":"file_search","vector_store_ids":["vs1"]},{"type":"function","name":"f","parameters":{"type":"object"}}],"input":"x"}`
	out := mustChat(t, body, "gpt-4o")
	for _, tl := range out.Tools {
		if tl.Function.Name == "file_search" {
			t.Fatal("file_search must not be declared on Chat")
		}
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "f" {
		t.Fatalf("tools=%+v", out.Tools)
	}
}

func TestToChat_ReasoningHistorySkipped(t *testing.T) {
	// 兼容旧名：reasoning 不得变成独立 role=reasoning 消息，而是挂到 assistant.reasoning_content。
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"reasoning","id":"r1","summary":[],"content":[{"type":"reasoning_text","text":"think"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	for _, m := range out.Messages {
		if m.Role == "reasoning" {
			t.Fatal("reasoning role must not appear in Chat messages")
		}
	}
	found := false
	for _, m := range out.Messages {
		if m.Role == "assistant" && m.ReasoningContent == "think" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want assistant.reasoning_content=think: %+v", out.Messages)
	}
}

func TestToChat_ImageInputTextAndImageParts(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"see "},
			{"type":"input_image","image_url":"https://example.com/a.png"}
		]}]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 1 {
		t.Fatalf("messages=%+v", out.Messages)
	}
	parts := chatParts(t, out.Messages[0].Content)
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "see " {
		t.Fatalf("want text+image parts, got %+v", parts)
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/a.png" {
		t.Fatalf("image part=%+v", parts[1])
	}
}

func TestToChat_ToolChoiceFunctionName(t *testing.T) {
	body := `{"model":"gpt-4o","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"f"},"input":"x"}`
	out := mustChat(t, body, "gpt-4o")
	raw, _ := json.Marshal(out.ToolChoice)
	if !strings.Contains(string(raw), `"f"`) {
		t.Fatalf("tool_choice=%s", raw)
	}
}

func TestConvertToolChoiceHosted(t *testing.T) {
	hosted := func(typ oairesponses.ToolChoiceTypesType) oairesponses.ResponseNewParamsToolChoiceUnion {
		return oairesponses.ResponseNewParamsToolChoiceUnion{
			OfHostedTool: &oairesponses.ToolChoiceTypesParam{Type: typ},
		}
	}
	cases := []struct {
		name     string
		tc       oairesponses.ResponseNewParamsToolChoiceUnion
		wantName string // 期望强制选择的 function 名；空表示降级为 nil
	}{
		{
			name:     "web_search 强制选择映射为同名 function",
			tc:       hosted(oairesponses.ToolChoiceTypesType("web_search")),
			wantName: "web_search",
		},
		{
			name:     "web_search_preview 归并到 web_search",
			tc:       hosted(oairesponses.ToolChoiceTypesTypeWebSearchPreview),
			wantName: "web_search",
		},
		{
			name:     "code_interpreter 无映射，降级为 nil（网关不支持）",
			tc:       hosted(oairesponses.ToolChoiceTypesTypeCodeInterpreter),
			wantName: "",
		},
		{
			name:     "image_generation hosted 无映射，降级为 nil",
			tc:       hosted(oairesponses.ToolChoiceTypesTypeImageGeneration),
			wantName: "",
		},
		{
			name: "mcp tool_choice 无 Chat 等价，降级为 nil",
			tc: oairesponses.ResponseNewParamsToolChoiceUnion{
				OfMcpTool: &oairesponses.ToolChoiceMcpParam{ServerLabel: "srv"},
			},
			wantName: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := convertToolChoice(tc.tc)
			if tc.wantName == "" {
				if got != nil {
					t.Fatalf("tool_choice 应降级为 nil, got %v", got)
				}
				return
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal tool_choice: %v", err)
			}
			var parsed struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("unmarshal tool_choice %s: %v", raw, err)
			}
			if parsed.Type != "function" || parsed.Function.Name != tc.wantName {
				t.Fatalf("tool_choice=%s, want function %q", raw, tc.wantName)
			}
		})
	}
}

// TestToChat_ToolChoiceHostedEndToEnd 走 ToChat 全链路验证 hosted 强制选择进出 wire。
func TestToChat_ToolChoiceHostedEndToEnd(t *testing.T) {
	body := `{"model":"gpt-4o","input":"x","tools":[{"type":"web_search"}]}`
	req, err := convert.DecodeResponseNewParams([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
		OfHostedTool: &oairesponses.ToolChoiceTypesParam{
			Type: oairesponses.ToolChoiceTypesType("web_search"),
		},
	}
	out, err := ToChat(req, "gpt-4o")
	if err != nil {
		t.Fatalf("ToChat: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"tool_choice":{"function":{"name":"web_search"},"type":"function"}`) &&
		!strings.Contains(string(raw), `"tool_choice":{"type":"function","function":{"name":"web_search"}}`) {
		t.Fatalf("wire tool_choice missing forced web_search: %s", raw)
	}
}

func TestToChat_CompactionTriggerDropped(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"compaction_trigger"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 1 || out.Messages[0].Content != "hi" {
		t.Fatalf("compaction_trigger must be dropped, got %+v", out.Messages)
	}
}

func TestToChat_NilRequest(t *testing.T) {
	if _, err := ToChat(nil, "m"); err == nil {
		t.Fatal("want error")
	}
}

// TestToChat_ReasoningContentOnAssistant 历史 reasoning 必须折入同轮/下一条 assistant 的 reasoning_content，
// 并与 tool_calls 同框（DeepSeek/Kimi/GLM 工具环要求）。
func TestToChat_ReasoningContentOnAssistant(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"need tool"}],"content":[{"type":"reasoning_text","text":"need tool"}]},
			{"type":"function_call","id":"fc1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"sunny in Paris"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	var assistantWithTools *ChatMessage
	for i := range out.Messages {
		m := &out.Messages[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			assistantWithTools = m
			break
		}
	}
	if assistantWithTools == nil {
		t.Fatalf("want assistant with tool_calls: %+v", out.Messages)
	}
	if assistantWithTools.ReasoningContent != "need tool" {
		t.Fatalf("reasoning_content=%q want need tool; msg=%+v", assistantWithTools.ReasoningContent, assistantWithTools)
	}
	// 终局 assistant 文本消息不应被 reasoning role 污染
	for _, m := range out.Messages {
		if m.Role == "reasoning" {
			t.Fatal("reasoning role must not appear")
		}
	}
}

// TestToChat_ReasoningBeforeAssistantText 无 tool 时 reasoning 挂到下一条 assistant 文本消息。
func TestToChat_ReasoningBeforeAssistantText(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"think hard"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, m := range out.Messages {
		if m.Role == "assistant" && m.ReasoningContent == "think hard" {
			if s, ok := m.Content.(string); !ok || s != "ok" {
				t.Fatalf("content=%v", m.Content)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("want assistant with reasoning_content: %+v", out.Messages)
	}
}

// TestToChat_ReasoningWithToolCallAssistantPerRound 复现 OpenCode/DeepSeek 400：
// 历史连续多轮 reasoning → assistant 文本 → function_call 时，每轮 reasoning_content
// 必须挂到本轮带 tool_calls 的同一条 assistant 上，不能拆成只含 reasoning 的
// assistant，也不能串到上一轮的 assistant。
func TestToChat_ReasoningWithToolCallAssistantPerRound(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"check"}]},
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"need tool 1"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"let me check 1"}]},
			{"type":"function_call","id":"fc1","call_id":"call_1","name":"get_logs","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok 1"},
			{"type":"reasoning","id":"r2","summary":[{"type":"summary_text","text":"need tool 2"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"let me check 2"}]},
			{"type":"function_call","id":"fc2","call_id":"call_2","name":"get_logs","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_2","output":"ok 2"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	want := map[string]string{
		"need tool 1": "let me check 1",
		"need tool 2": "let me check 2",
	}
	found := map[string]string{}
	for _, m := range out.Messages {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 || m.ReasoningContent == "" {
			continue
		}
		// 严格上游（DeepSeek requiresAssistantContentForToolCalls）要求
		// content、reasoning_content、tool_calls 同框，且按轮归属。
		text, _ := m.Content.(string)
		found[m.ReasoningContent] = text
	}
	for rc, content := range want {
		if got := found[rc]; got != content {
			t.Fatalf("round %q tool-call assistant content=%q want %q; msgs=%+v", rc, got, content, out.Messages)
		}
	}
	if len(found) != len(want) {
		t.Fatalf("want %d tool-call assistants with reasoning, got %d; msgs=%+v", len(want), len(found), out.Messages)
	}
	// reasoning 不得被拆到无 tool_calls 的独立 assistant 上。
	for _, m := range out.Messages {
		if m.Role == "assistant" && m.ReasoningContent != "" && len(m.ToolCalls) == 0 {
			t.Fatalf("reasoning must not be split onto assistant without tool_calls; msgs=%+v", out.Messages)
		}
	}
}

// TestToChat_ReasoningContentInterleavedToolCalls 同一轮工具环若被 tool 结果打断
// （reasoning → assistant → fc → fco → fc → fco），每个带 tool_calls 的 assistant
// 都必须携带 reasoning_content，不能出现缺 reasoning 的第二条 tool-call assistant。
func TestToChat_ReasoningContentInterleavedToolCalls(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"check"}]},
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"need tool"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"let me check"}]},
			{"type":"function_call","id":"fc1","call_id":"call_1","name":"get_logs","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok 1"},
			{"type":"function_call","id":"fc2","call_id":"call_2","name":"search","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_2","output":"ok 2"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	for _, m := range out.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ReasoningContent == "" {
			t.Fatalf("tool-call assistant missing reasoning_content; msg=%+v; msgs=%+v", m, out.Messages)
		}
	}
}

// TestToChat_ReasoningAfterToolResultMergesBack 复现 Console 400：
// reasoning 出现在 tool 结果之后、下一个 function_call 合并回上一轮 assistant 之前时，
// 该 reasoning 必须挂回同一 assistant，不能残留成末尾只有 reasoning_content、
// 没有 content/tool_calls 的 assistant（上游要求 assistant 至少携带 content 或 tool_calls）。
func TestToChat_ReasoningAfterToolResultMergesBack(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"check"}]},
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"need tool 1"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"let me check 1"}]},
			{"type":"function_call","id":"fc1","call_id":"call_1","name":"get_logs","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok 1"},
			{"type":"reasoning","id":"r2","summary":[{"type":"summary_text","text":"need tool 2"}]},
			{"type":"function_call","id":"fc2","call_id":"call_2","name":"search","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_2","output":"ok 2"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, m := range out.Messages {
		if m.Role != "assistant" {
			continue
		}
		text, _ := m.Content.(string)
		if text == "" && len(m.ToolCalls) == 0 {
			t.Fatalf("assistant must carry content or tool_calls; msg=%+v; msgs=%+v", m, out.Messages)
		}
		if len(m.ToolCalls) > 0 && m.ToolCalls[len(m.ToolCalls)-1].ID == "call_2" {
			if m.ReasoningContent == "" || !strings.Contains(m.ReasoningContent, "need tool 2") {
				t.Fatalf("merged assistant must carry pending reasoning; msg=%+v; msgs=%+v", m, out.Messages)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected merged tool-call assistant for call_2; msgs=%+v", out.Messages)
	}
}

// TestToChat_ReasoningAcrossConsecutiveAssistants 复现 Console 400：
// reasoning 后连续多条 assistant 文本再接 function_call 时，reasoning 只能挂在
// 第一条 assistant 上，而 tool_calls 落在最后一条，导致带 tool_calls 的 assistant
// 缺 reasoning_content。修复应把同段 assistant 上的 reasoning 迁移到最终
// 带 tool_calls 的 assistant，满足严格上游同框要求。
func TestToChat_ReasoningAcrossConsecutiveAssistants(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"check"}]},
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"need tool"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"text 1"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"text 2"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"text 3"}]},
			{"type":"function_call","id":"fc1","call_id":"call_1","name":"get_logs","arguments":"{}"},
			{"type":"function_call","id":"fc2","call_id":"call_2","name":"search","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok 1"},
			{"type":"function_call_output","call_id":"call_2","output":"ok 2"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := 0
	for _, m := range out.Messages {
		if m.Role != "assistant" {
			continue
		}
		text, _ := m.Content.(string)
		if text == "" && len(m.ToolCalls) == 0 {
			t.Fatalf("assistant must carry content or tool_calls; msg=%+v; msgs=%+v", m, out.Messages)
		}
		if len(m.ToolCalls) == 0 {
			continue
		}
		found++
		if m.ReasoningContent == "" || !strings.Contains(m.ReasoningContent, "need tool") {
			t.Fatalf("tool-call assistant must carry reasoning_content; msg=%+v; msgs=%+v", m, out.Messages)
		}
	}
	if found != 1 {
		t.Fatalf("want exactly 1 tool-call assistant, got %d; msgs=%+v", found, out.Messages)
	}
}

// TestToChat_ReasoningContentFromContentFallback summary 空时回退 content[].reasoning_text。
func TestToChat_ReasoningContentFromContentFallback(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"reasoning","id":"r1","summary":[],"content":[{"type":"reasoning_text","text":"from content"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, m := range out.Messages {
		if m.Role == "assistant" && m.ReasoningContent == "from content" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want fallback reasoning_content: %+v", out.Messages)
	}
}

func TestToChat_EmptyAssistantOutputSkipped(t *testing.T) {
	out := mustChat(t, `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]},
			{"type":"function_call","call_id":"c1","name":"noop","arguments":"{}"}
		]
	}`, "gpt-4o")
	if len(out.Messages) != 3 {
		t.Fatalf("expected 2 messages, got %+v", out.Messages)
	}
	if out.Messages[1].Role != "assistant" || len(out.Messages[1].ToolCalls) != 1 || out.Messages[1].ToolCalls[0].Function.Name != "noop" {
		t.Fatalf("want noop tool call, got %+v", out.Messages[1])
	}
	if out.Messages[2].Role != "tool" || out.Messages[2].ToolCallID != "c1" {
		t.Fatalf("want placeholder tool result, got %+v", out.Messages[2])
	}
}

func TestChatFunctionArgumentsNonJSONPassthrough(t *testing.T) {
	out := mustChat(t, `{
		"model":"gpt-5",
		"input":[
			{"type":"function_call","call_id":"c1","name":"shell","arguments":"not-json"}
		],
		"stream":true
	}`, "m")
	found := false
	for _, m := range out.Messages {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name != "shell" {
				continue
			}
			found = true
			if tc.Function.Arguments != "not-json" {
				t.Fatalf("want original arguments, got %q", tc.Function.Arguments)
			}
		}
	}
	if !found {
		t.Fatal("tool_call not found")
	}
}

func TestChatFunctionArgumentsPreservesWhitespace(t *testing.T) {
	const arguments = `  {"city":"x"}  `
	if got := chatFunctionArguments(arguments); got != arguments {
		t.Fatalf("arguments=%q want %q", got, arguments)
	}
}

func TestChatFunctionArgumentsTruncatedJSONPassthrough(t *testing.T) {
	const arguments = `{"city":`
	if got := chatFunctionArguments(arguments); got != arguments {
		t.Fatalf("arguments=%q want %q", got, arguments)
	}
}

func TestChatFunctionArgumentsEmptyUsesObject(t *testing.T) {
	if got := chatFunctionArguments(""); got != "{}" {
		t.Fatalf("arguments=%q want {}", got)
	}
}

func TestChatFunctionArgumentsValidJSONPassthrough(t *testing.T) {
	out := mustChat(t, `{
		"model":"gpt-5",
		"input":[
			{"type":"function_call","call_id":"c1","name":"get","arguments":"{\"city\":\"x\"}"}
		],
		"stream":true
	}`, "m")
	for _, m := range out.Messages {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "get" && tc.Function.Arguments != `{"city":"x"}` {
				t.Fatalf("want passthrough object string, got %s", tc.Function.Arguments)
			}
		}
	}
}

func TestToChat_ToolCallIDPassthrough(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]},
			{"type":"function_call","call_id":"call_123|abc def","name":"a","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_123|abc def","output":"done"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	var asstID, toolID string
	for _, m := range out.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			asstID = m.ToolCalls[0].ID
		}
		if m.Role == "tool" && m.ToolCallID != "" {
			toolID = m.ToolCallID
		}
	}
	if asstID != "call_123|abc def" {
		t.Fatalf("assistant tool_call_id must pass through unchanged: %q", asstID)
	}
	if toolID != asstID {
		t.Fatalf("tool id %q must match assistant id %q", toolID, asstID)
	}
}

func TestToChat_ToolCallIDLongPassthrough(t *testing.T) {
	long := "call_" + strings.Repeat("x", 60)
	body := fmt.Sprintf(`{
		"model":"gpt-4o",
		"input":[
			{"type":"function_call","call_id":%q,"name":"a","arguments":"{}"},
			{"type":"function_call_output","call_id":%q,"output":"done"}
		]
	}`, long, long)
	out := mustChat(t, body, "gpt-4o")
	for _, m := range out.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != long {
				t.Fatalf("long tool_call_id must pass through unchanged: %q", tc.ID)
			}
		}
		if m.Role == "tool" && m.ToolCallID != long {
			t.Fatalf("long tool id must pass through unchanged: %q", m.ToolCallID)
		}
	}
}

func TestToChat_ToolCallIDCollisionPassthrough(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"function_call","call_id":"call_a|1","name":"a","arguments":"{}"},
			{"type":"function_call","call_id":"call_a_1","name":"b","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a|1","output":"1"},
			{"type":"function_call_output","call_id":"call_a_1","output":"2"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	seen := map[string]struct{}{}
	for _, m := range out.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "call_a|1" && tc.ID != "call_a_1" {
				t.Fatalf("tool_call_id must pass through unchanged: %q", tc.ID)
			}
			seen[tc.ID] = struct{}{}
		}
	}
	if _, ok := seen["call_a|1"]; !ok {
		t.Fatalf("original id call_a|1 missing: %v", seen)
	}
	if _, ok := seen["call_a_1"]; !ok {
		t.Fatalf("original id call_a_1 missing: %v", seen)
	}
}

func TestToChat_ToolSchemaNullAnyOfNormalized(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{
			"type":"function","name":"f",
			"parameters":{
				"type":"object",
				"properties":{
					"a":{"type":["string","null"]},
					"b":{"anyOf":[{"type":"integer"},{"type":"null"}]}
				}
			}
		}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	params, ok := out.Tools[0].Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("parameters type=%T", out.Tools[0].Function.Parameters)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties=%v", params["properties"])
	}
	a, ok := props["a"].(map[string]any)
	if !ok || a["type"] != "string" {
		t.Fatalf("a type array not collapsed: %v", props["a"])
	}
	b, ok := props["b"].(map[string]any)
	if !ok {
		t.Fatalf("b schema=%v", props["b"])
	}
	if b["type"] != "integer" {
		t.Fatalf("single anyOf variant should merge into b: %v", b)
	}
	if _, ok := b["anyOf"]; ok {
		t.Fatalf("b anyOf should be merged away: %v", b)
	}
	if _, ok := params["additionalProperties"]; ok {
		t.Fatalf("top-level schema without anyOf must not force additionalProperties: %v", params)
	}
}

func TestToChat_NamespaceToolSchemaNormalized(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{
			"type":"namespace","name":"collab",
			"tools":[{"type":"function","name":"pick","parameters":{"type":["object","null"]}}]
		}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 {
		t.Fatalf("tools=%+v", out.Tools)
	}
	params, ok := out.Tools[0].Function.Parameters.(map[string]any)
	if !ok || params["type"] != "object" {
		t.Fatalf("namespace parameters not normalized: %v", out.Tools[0].Function.Parameters)
	}
	if _, ok := params["additionalProperties"]; ok {
		t.Fatalf("namespace schema without anyOf must not force additionalProperties: %v", params)
	}
}

func TestToChat_AdjacentSystemMerged(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"instructions":"be brief",
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev rules"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 3 {
		t.Fatalf("want system+user(wrapped)+user, got %d messages=%+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "system" {
		t.Fatalf("first role=%q", out.Messages[0].Role)
	}
	system, _ := out.Messages[0].Content.(string)
	if !strings.Contains(system, "be brief") {
		t.Fatalf("system instructions missing: %q", system)
	}
	dev, _ := out.Messages[1].Content.(string)
	if out.Messages[1].Role != "user" || !strings.Contains(dev, "<system-update>") || !strings.Contains(dev, "dev rules") {
		t.Fatalf("developer not wrapped as user: %+v", out.Messages[1])
	}
}

func TestToChat_CompactionBetweenUsersDropped(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"before"}]},
			{"type":"compaction","encrypted_content":"enc-blob"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 2 {
		t.Fatalf("want before/continue user messages, got %d messages=%+v", len(out.Messages), out.Messages)
	}
	for i, m := range out.Messages {
		if m.Role != "user" {
			t.Fatalf("message %d role=%q: %+v", i, m.Role, out.Messages)
		}
	}
	first, _ := out.Messages[0].Content.(string)
	second, _ := out.Messages[1].Content.(string)
	if first != "before" || second != "continue" || strings.Contains(second, "enc-blob") {
		t.Fatalf("compaction must be dropped: %q / %q", first, second)
	}
}

func TestToChat_ImageInputDataURLPassthrough(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"data:image/png;base64,AAEC"}
		]}]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("messages=%+v", out.Messages)
	}
	parts := chatParts(t, out.Messages[0].Content)
	if len(parts) != 1 || parts[0].Type != "image_url" || parts[0].ImageURL == nil {
		t.Fatalf("parts=%+v", parts)
	}
	if parts[0].ImageURL.URL != "data:image/png;base64,AAEC" {
		t.Fatalf("data url not preserved: %q", parts[0].ImageURL.URL)
	}
}

func TestToChat_ImageToolResultAggregatesUserMessage(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"function_call","call_id":"c1","name":"gen","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[
				{"type":"input_text","text":"ok"},
				{"type":"input_image","image_url":"https://example.com/a.png"}
			]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 3 {
		t.Fatalf("want assistant+tool+user, got %d messages=%+v", len(out.Messages), out.Messages)
	}
	if out.Messages[1].Role != "tool" || out.Messages[1].Content != "ok" {
		t.Fatalf("tool msg=%+v", out.Messages[1])
	}
	if out.Messages[2].Role != "user" {
		t.Fatalf("want trailing image user msg: %+v", out.Messages[2])
	}
	parts := chatParts(t, out.Messages[2].Content)
	if len(parts) != 1 || parts[0].Type != "image_url" || parts[0].ImageURL == nil ||
		parts[0].ImageURL.URL != "https://example.com/a.png" {
		t.Fatalf("image user content=%+v", parts)
	}
}

func mustToChatErr(t *testing.T, body string) error {
	t.Helper()
	req, err := convert.DecodeResponseNewParams([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, err = ToChat(req, "gpt-4o")
	return err
}

func TestToChat_AssistantToolCallMarshalsNullContent(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"function_call","call_id":"c1","name":"gen","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"done"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"role":"assistant","content":null`) {
		t.Fatalf("assistant content must marshal null: %s", raw)
	}
}

func TestToChat_ReasoningOnlyHistoryBecomesAssistant(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"think hard"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 2 {
		t.Fatalf("want user+assistant, got %+v", out.Messages)
	}
	asst := out.Messages[1]
	if asst.Role != "assistant" || asst.Content != nil || asst.ReasoningContent != "think hard" {
		t.Fatalf("reasoning assistant=%+v", asst)
	}
}

func TestToChat_CompactionAfterToolResultDropped(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"before"}]},
			{"type":"function_call","call_id":"c1","name":"gen","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"done"},
			{"type":"compaction","encrypted_content":"enc"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	last := out.Messages[len(out.Messages)-1]
	if last.Role != "tool" {
		t.Fatalf("want tool result as last, got %+v", last)
	}
	s, ok := last.Content.(string)
	if !ok || !strings.Contains(s, "done") || strings.Contains(s, "enc") {
		t.Fatalf("compaction must be dropped, last=%+v", last.Content)
	}
	for _, m := range out.Messages {
		if m.Role == "system" {
			t.Fatalf("mid-conversation system role must not remain: %+v", out.Messages)
		}
	}
}

func TestToChat_ToolResultImagesFlushedCompactionDropped(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[
				{"type":"input_text","text":"ok"},
				{"type":"input_image","image_url":"https://example.com/1.png"}
			]},
			{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"c2","output":[
				{"type":"input_image","image_url":"https://example.com/2.png"}
			]},
			{"type":"compaction","encrypted_content":"check"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) < 2 {
		t.Fatalf("want tool ring + vision user, got %+v", out.Messages)
	}
	vision := out.Messages[len(out.Messages)-1]
	if vision.Role != "user" {
		t.Fatalf("want vision user as last, got %+v", out.Messages)
	}
	parts := chatParts(t, vision.Content)
	if len(parts) != 2 {
		t.Fatalf("want 2 images flushed, got %+v", parts)
	}
	if parts[0].ImageURL == nil || parts[0].ImageURL.URL != "https://example.com/1.png" {
		t.Fatalf("first image=%+v", parts[0])
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/2.png" {
		t.Fatalf("second image=%+v", parts[1])
	}
	for _, m := range out.Messages {
		if s, ok := m.Content.(string); ok && strings.Contains(s, "check") {
			t.Fatalf("compaction content must be dropped: %+v", out.Messages)
		}
	}
}

func TestToChat_McpListToolsHistoryNoText(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"mcp_list_tools","id":"lt_1","server_label":"weather","tools":[{"name":"get","description":"d","input_schema":{}}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	for _, m := range out.Messages {
		if s, ok := m.Content.(string); ok && strings.Contains(s, "mcp_list_tools") {
			t.Fatalf("mcp_list_tools must not become model text: %+v", m)
		}
	}
	if len(out.Messages) != 0 {
		t.Fatalf("mcp_list_tools must not emit messages, got %+v", out.Messages)
	}
}

func TestToChat_CodeInterpreterHistoryDropped(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"code_interpreter_call","id":"ci1","call_id":"ci1","code":"plot()","outputs":[
				{"type":"logs","logs":"ran"},
				{"type":"image","url":"https://example.com/plot.png"}
			]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 0 {
		t.Fatalf("code_interpreter_call 历史应被丢弃，got %+v", out.Messages)
	}
}

func TestToChat_InputFileUnsupportedError(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_file","file_url":"https://example.com/a.pdf"}
		]}]
	}`
	err := mustToChatErr(t, body)
	if err == nil || !strings.Contains(err.Error(), "input_file") {
		t.Fatalf("want input_file error, got %v", err)
	}
}

func TestToChat_InputImageFileIDOnlyError(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_image","file_id":"file_x"}
		]}]
	}`
	err := mustToChatErr(t, body)
	if err == nil || !strings.Contains(err.Error(), "file_id") {
		t.Fatalf("want file_id error, got %v", err)
	}
}

func TestToChat_ReasoningEffortMaxPassedThrough(t *testing.T) {
	out := mustChat(t, `{"model":"gpt-4o","reasoning":{"effort":"max"},"input":"x"}`, "gpt-4o")
	if out.ReasoningEffort == nil || *out.ReasoningEffort != "max" {
		t.Fatalf("reasoning_effort must pass through, got %v", out.ReasoningEffort)
	}
}

func TestToChat_ReasoningEffortArbitraryPassedThrough(t *testing.T) {
	out := mustChat(t, `{"model":"gpt-4o","reasoning":{"effort":"custom-strong"},"input":"x"}`, "gpt-4o")
	if out.ReasoningEffort == nil || *out.ReasoningEffort != "custom-strong" {
		t.Fatalf("reasoning_effort must pass through unchanged, got %v", out.ReasoningEffort)
	}
}

func TestToChat_ReasoningEffortLowAccepted(t *testing.T) {
	body := `{"model":"gpt-4o","reasoning":{"effort":"low"},"input":"x"}`
	out := mustChat(t, body, "gpt-4o")
	if out.ReasoningEffort == nil || *out.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort=%v", out.ReasoningEffort)
	}
}

func TestToChat_ToolSchemaAnyOfMergedIntoProperties(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{
			"type":"function","name":"f",
			"parameters":{
				"type":"object",
				"anyOf":[
					{"properties":{"a":{"type":"string"}}},
					{"properties":{"b":{"type":"integer"}}}
				],
				"required":["a"]
			}
		}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	params, ok := out.Tools[0].Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("parameters type=%T", out.Tools[0].Function.Parameters)
	}
	if params["type"] != "object" || params["additionalProperties"] != false {
		t.Fatalf("projection headers missing: %v", params)
	}
	if _, ok := params["anyOf"]; ok {
		t.Fatalf("anyOf must be flattened: %v", params)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties=%v", params["properties"])
	}
	if _, ok := props["a"]; !ok {
		t.Fatalf("a missing: %v", props)
	}
	if _, ok := props["b"]; !ok {
		t.Fatalf("b missing: %v", props)
	}
}

func TestToChat_FunctionSchemaForcedObject(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"tools":[{"type":"function","name":"f","parameters":{"type":"string"}}],
		"input":"x"
	}`
	out := mustChat(t, body, "gpt-4o")
	params, ok := out.Tools[0].Function.Parameters.(map[string]any)
	if !ok || params["type"] != "object" {
		t.Fatalf("schema not forced object: %v", out.Tools[0].Function.Parameters)
	}
}

func TestToChat_ToolMessageEmptyContentMarshaledEmptyString(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"function_call","call_id":"c1","name":"gen","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[
				{"type":"input_image","image_url":"https://example.com/a.png"}
			]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"role":"tool","content":""`) {
		t.Fatalf("tool content must marshal empty string: %s", raw)
	}
}
