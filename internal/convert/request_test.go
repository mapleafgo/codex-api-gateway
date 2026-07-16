package convert

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

func mustReq(t *testing.T, body string) *oairesponses.ResponseNewParams {
	t.Helper()
	r, err := DecodeResponseNewParams([]byte(body))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return r
}

func TestTextRequestConverts(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens == 0 {
		t.Fatal("max_tokens default not set")
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("user message not converted: %+v", out.Messages)
	}
}

func TestReasoningEffortMapsToThinking(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"high"},"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{Thinking: config.ThinkingCfg{EffortBudget: map[string]int{"high": 32000}}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking.OfEnabled == nil {
		b, _ := json.Marshal(out)
		t.Fatalf("thinking not set: %s", b)
	}
	if out.Thinking.OfEnabled.BudgetTokens != 32000 {
		t.Fatalf("bad budget: %d", out.Thinking.OfEnabled.BudgetTokens)
	}
}

func TestReasoningEffortNoneDisables(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"none"},"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking.OfDisabled == nil {
		t.Fatalf("effort=none should disable thinking")
	}
}

func TestDeveloperRoleFoldsToSystem(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","instructions":"be brief","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"system"`) {
		t.Fatalf("developer not folded to system: %s", b)
	}
	for _, m := range out.Messages {
		if m.Role == "developer" || m.Role == "system" {
			t.Fatalf("developer/system role leaked into messages")
		}
	}
}

func TestSystemConversionPreservesInstructionRoles(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","instructions":"be brief","input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"top rules"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"developer rules"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.System) != 1 {
		t.Fatalf("expected one system block: %+v", out.System)
	}
	got := out.System[0].Text
	wantParts := []string{
		"<developer>\nbe brief\n</developer>",
		"<system>\ntop rules\n</system>",
		"<developer>\ndeveloper rules\n</developer>",
	}
	last := -1
	for _, part := range wantParts {
		idx := strings.Index(got, part)
		if idx < 0 {
			t.Fatalf("system block missing role-preserved part %q: %q", part, got)
		}
		if idx <= last {
			t.Fatalf("system block parts out of order: %q", got)
		}
		last = idx
	}
}

func TestAssistantPhasePreservedInAnthropicText(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"input_text","text":"I am checking files."}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) < 1 || out.Messages[0].Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("assistant message not converted: %+v", out.Messages)
	}
	text := out.Messages[0].Content[0].OfText
	if text == nil {
		t.Fatalf("assistant phase text marker missing: %+v", out.Messages[0].Content[0])
	}
	if !strings.Contains(text.Text, "<assistant_phase>commentary</assistant_phase>") {
		t.Fatalf("assistant phase not preserved in text: %q", text.Text)
	}
	if !strings.Contains(text.Text, "I am checking files.") {
		t.Fatalf("assistant message text lost: %q", text.Text)
	}
}

func TestAdditionalToolsAndToolSearchItemsConvert(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"tool_search_call","call_id":"ts1","arguments":{"q":"crm"}},
		{"type":"tool_search_output","call_id":"ts1","tools":[{"type":"function","name":"lookup","description":"lookup contact","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}]},
		{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"raw_edit","description":"raw edit"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"use the loaded tools"}]}
	],"tools":[{"type":"tool_search","execution":"client","description":"search deferred tools","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if findTool(out.Tools, "tool_search") == nil {
		t.Fatalf("top-level tool_search not exposed: %+v", out.Tools)
	}
	if findTool(out.Tools, "lookup") == nil {
		t.Fatalf("tool_search_output tools not exposed: %+v", out.Tools)
	}
	raw := findTool(out.Tools, "raw_edit")
	if raw == nil || raw.Type != anthropic.ToolTypeCustom {
		t.Fatalf("additional custom tool not exposed as custom: %+v", out.Tools)
	}
	if len(out.Messages) < 2 || out.Messages[0].Content[0].OfToolUse == nil {
		t.Fatalf("tool_search_call not converted to tool_use: %+v", out.Messages)
	}
	if out.Messages[0].Content[0].OfToolUse.Name != "tool_search" {
		t.Fatalf("tool_search_call uses wrong tool name: %+v", out.Messages[0].Content[0].OfToolUse)
	}
	if out.Messages[1].Content[0].OfToolResult == nil {
		t.Fatalf("tool_search_output not converted to tool_result: %+v", out.Messages)
	}
	if len(out.System) == 0 || !strings.Contains(out.System[0].Text, "<developer_tools>") {
		t.Fatalf("additional_tools context marker missing: %+v", out.System)
	}
}

func TestCompactionItemsPreservedAsSystemContext(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"compaction","encrypted_content":"sealed-context"},
		{"type":"compaction_trigger"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
	],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.System) != 1 {
		t.Fatalf("expected compaction system context: %+v", out.System)
	}
	got := out.System[0].Text
	if !strings.Contains(got, "<compaction>") || !strings.Contains(got, "sealed-context") {
		t.Fatalf("compaction item not preserved: %q", got)
	}
	if !strings.Contains(got, "<compaction_trigger />") {
		t.Fatalf("compaction trigger not preserved: %q", got)
	}
}

func TestUnsupportedInputItemPreservedAsSystemContext(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"mcp_approval_response","approval_request_id":"apr_1","approve":true,"reason":"user approved"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
	],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.System) != 1 {
		t.Fatalf("expected unsupported item system context: %+v", out.System)
	}
	got := out.System[0].Text
	if !strings.Contains(got, "<openai_input_item type=\"mcp_approval_response\">") ||
		!strings.Contains(got, `"approval_request_id":"apr_1"`) {
		t.Fatalf("unsupported item not preserved: %q", got)
	}
}

func TestToolCallsConvert(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"search x"}]},{"type":"function_call","call_id":"c1","name":"search","arguments":"{\"q\":\"x\"}"},{"type":"function_call_output","call_id":"c1","output":"result-x"}],"tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(out.Messages), out.Messages)
	}
	asst := out.Messages[1]
	if asst.Role != anthropic.MessageParamRoleAssistant || len(asst.Content) != 1 || asst.Content[0].OfToolUse == nil {
		t.Fatalf("bad assistant tool_use: %+v", asst)
	}
	if asst.Content[0].OfToolUse.ID != "c1" || asst.Content[0].OfToolUse.Name != "search" {
		t.Fatalf("bad tool_use ids: %+v", asst.Content[0].OfToolUse)
	}
	tr := out.Messages[2]
	if tr.Role != anthropic.MessageParamRoleUser || tr.Content[0].OfToolResult == nil {
		t.Fatalf("bad tool_result: %+v", tr)
	}
	if tr.Content[0].OfToolResult.ToolUseID != "c1" {
		t.Fatalf("bad tool_result tool_use_id: %+v", tr.Content[0].OfToolResult)
	}
	if len(out.Tools) != 1 || out.Tools[0].OfTool.Name != "search" {
		t.Fatalf("bad tools: %+v", out.Tools)
	}
}

func TestCustomToolCallInputAndOutputConvert(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"edit"}]},
		{"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
		{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}
	],"tools":[{"type":"custom","name":"apply_patch"}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(out.Messages), out.Messages)
	}
	toolUse := out.Messages[1].Content[0].OfToolUse
	if toolUse == nil {
		t.Fatalf("custom_tool_call not converted to tool_use: %+v", out.Messages[1])
	}
	if toolUse.ID != "c1" || toolUse.Name != "apply_patch" {
		t.Fatalf("bad custom tool_use ids: %+v", toolUse)
	}
	inputData, err := json.Marshal(toolUse.Input)
	if err != nil {
		t.Fatalf("custom tool input cannot marshal: %v", err)
	}
	var input map[string]string
	if err := json.Unmarshal(inputData, &input); err != nil {
		t.Fatalf("custom tool input is not JSON object: %v", err)
	}
	if input["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom input not wrapped: %+v", input)
	}
	toolResult := out.Messages[2].Content[0].OfToolResult
	if toolResult == nil || toolResult.ToolUseID != "c1" {
		t.Fatalf("custom_tool_call_output not converted: %+v", out.Messages[2])
	}
}

func TestShellCallInputItemConvertsToShellToolUse(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfShellCall: &oairesponses.ResponseInputItemShellCallParam{
					CallID: "call_shell",
					Action: oairesponses.ResponseInputItemShellCallActionParam{
						Commands: []string{"pwd", "go test ./..."},
					},
				},
			}},
		},
	}
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	toolUse := out.Messages[0].Content[0].OfToolUse
	if toolUse == nil || toolUse.Name != "shell" || toolUse.ID != "call_shell" {
		t.Fatalf("bad shell tool_use: %+v", out.Messages[0].Content[0])
	}
	if got := fmt.Sprint(toolUse.Input); !strings.Contains(got, "go test ./...") {
		t.Fatalf("shell input lost commands: %#v", toolUse.Input)
	}
}

func TestLocalShellCallInputItemConvertsToShellToolUse(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfLocalShellCall: &oairesponses.ResponseInputItemLocalShellCallParam{
					ID:     "local_shell_1",
					CallID: "call_local_shell",
					Action: oairesponses.ResponseInputItemLocalShellCallActionParam{
						Command: []string{"go", "test", "./..."},
					},
				},
			}},
		},
	}
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	toolUse := out.Messages[0].Content[0].OfToolUse
	if toolUse == nil || toolUse.Name != "shell" || toolUse.ID != "call_local_shell" {
		t.Fatalf("bad local shell tool_use: %+v", out.Messages[0].Content[0])
	}
	if got := fmt.Sprint(toolUse.Input); !strings.Contains(got, "go test ./...") {
		t.Fatalf("local shell input lost command: %#v", toolUse.Input)
	}
}

func TestApplyPatchCallInputItemConvertsToApplyPatchToolUse(t *testing.T) {
	tests := []struct {
		name      string
		operation oairesponses.ResponseInputItemApplyPatchCallOperationUnionParam
		wantType  string
		wantPath  string
		wantDiff  *string
	}{
		{
			name: "create",
			operation: oairesponses.ResponseInputItemApplyPatchCallOperationUnionParam{
				OfCreateFile: &oairesponses.ResponseInputItemApplyPatchCallOperationCreateFileParam{
					Path: "new.txt", Diff: "*** Add File: new.txt\n+new\n",
				},
			},
			wantType: "create_file", wantPath: "new.txt", wantDiff: stringPtr("*** Add File: new.txt\n+new\n"),
		},
		{
			name: "update",
			operation: oairesponses.ResponseInputItemApplyPatchCallOperationUnionParam{
				OfUpdateFile: &oairesponses.ResponseInputItemApplyPatchCallOperationUpdateFileParam{
					Path: "README.md", Diff: "*** Update File: README.md\n@@\n-old\n+new\n",
				},
			},
			wantType: "update_file", wantPath: "README.md", wantDiff: stringPtr("*** Update File: README.md\n@@\n-old\n+new\n"),
		},
		{
			name: "delete",
			operation: oairesponses.ResponseInputItemApplyPatchCallOperationUnionParam{
				OfDeleteFile: &oairesponses.ResponseInputItemApplyPatchCallOperationDeleteFileParam{Path: "old.txt"},
			},
			wantType: "delete_file", wantPath: "old.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &oairesponses.ResponseNewParams{
				Model: "gpt-5",
				Input: oairesponses.ResponseNewParamsInputUnion{
					OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
						OfApplyPatchCall: &oairesponses.ResponseInputItemApplyPatchCallParam{
							CallID: "call_patch", Status: "completed", Operation: tt.operation,
						},
					}},
				},
			}
			out, err := ToAnthropic(req, &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			toolUse := out.Messages[0].Content[0].OfToolUse
			if toolUse == nil || toolUse.Name != "apply_patch" || toolUse.ID != "call_patch" {
				t.Fatalf("bad apply_patch tool_use: %+v", out.Messages[0].Content[0])
			}
			input, ok := toolUse.Input.(map[string]any)
			if !ok {
				t.Fatalf("apply_patch input type = %T, want object", toolUse.Input)
			}
			if input["operation"] != tt.wantType || input["path"] != tt.wantPath {
				t.Fatalf("apply_patch input = %#v, want operation=%q path=%q", input, tt.wantType, tt.wantPath)
			}
			if tt.wantDiff == nil {
				if _, ok := input["diff"]; ok {
					t.Fatalf("delete apply_patch input must not invent a diff: %#v", input)
				}
			} else if input["diff"] != *tt.wantDiff {
				t.Fatalf("apply_patch diff = %#v, want %q", input["diff"], *tt.wantDiff)
			}
		})
	}
}

func stringPtr(s string) *string {
	return &s
}

func TestShellAndApplyPatchOutputsConvertToToolResults(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{
				{OfShellCallOutput: &oairesponses.ResponseInputItemShellCallOutputParam{
					CallID: "call_shell",
					Output: []oairesponses.ResponseFunctionShellCallOutputContentParam{{
						Stdout: "ok",
						Stderr: "warn",
					}},
				}},
				{OfLocalShellCallOutput: &oairesponses.ResponseInputItemLocalShellCallOutputParam{
					ID:     "call_local_shell",
					Output: "local ok",
				}},
				{OfApplyPatchCallOutput: &oairesponses.ResponseInputItemApplyPatchCallOutputParam{
					CallID: "call_patch",
					Status: "completed",
					Output: oparam.NewOpt("Done"),
				}},
			},
		},
	}
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Content[0].OfToolResult.ToolUseID != "call_shell" {
		t.Fatalf("shell output did not produce tool_result: %+v", out.Messages[0])
	}
	shellText := out.Messages[0].Content[0].OfToolResult.Content[0].OfText.Text
	if !strings.Contains(shellText, "ok") || !strings.Contains(shellText, "warn") {
		t.Fatalf("shell output lost stdout/stderr: %q", shellText)
	}
	if out.Messages[0].Content[1].OfToolResult.ToolUseID != "call_local_shell" {
		t.Fatalf("local shell output did not produce tool_result: %+v", out.Messages[0])
	}
	if out.Messages[0].Content[2].OfToolResult.ToolUseID != "call_patch" {
		t.Fatalf("apply_patch output did not produce tool_result: %+v", out.Messages[0])
	}
	if out.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("tool results should be in user message, got %s", out.Messages[0].Role)
	}
}

func TestStructuredOutputInjectsTool(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"give me json"}]}],"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"v":{"type":"number"}}}}},"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].OfTool.Name != "answer" {
		t.Fatalf("structured output tool not injected: %+v", out.Tools)
	}
	if out.ToolChoice.OfTool == nil || out.ToolChoice.OfTool.Name != "answer" {
		t.Fatalf("bad tool_choice: %+v", out.ToolChoice)
	}
}

func TestJsonObjectFormatInjectsTool(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"give me json"}]}],"text":{"format":{"type":"json_object"}},"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].OfTool.Name != "json_object" {
		t.Fatalf("json_object tool not injected: %+v", out.Tools)
	}
	if out.ToolChoice.OfTool == nil || out.ToolChoice.OfTool.Name != "json_object" {
		t.Fatalf("bad tool_choice: %+v", out.ToolChoice)
	}
}

func TestDefaultMaxTokens(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens != 4096 {
		t.Fatalf("expected default 4096, got %d", out.MaxTokens)
	}
}

func TestThinkingBudgetRaisesMaxTokens(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"high"},"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{Thinking: config.ThinkingCfg{EffortBudget: map[string]int{"high": 32000}}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking.OfEnabled == nil || out.Thinking.OfEnabled.BudgetTokens != 32000 {
		t.Fatalf("bad budget: %+v", out.Thinking)
	}
	if out.MaxTokens <= 32000 {
		t.Fatalf("max_tokens %d must exceed budget 32000", out.MaxTokens)
	}
}

func TestReasoningSummaryConciseSetsDisplay(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"medium","summary":"concise"},"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{Thinking: config.ThinkingCfg{EffortBudget: map[string]int{"medium": 16000}}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking.OfEnabled == nil || out.Thinking.OfEnabled.Display != anthropic.ThinkingConfigEnabledDisplaySummarized {
		t.Fatalf("concise summary should set display=summarized")
	}
}

func TestToolChoiceAuto(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"tool_choice":"auto","stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolChoice.OfAuto == nil {
		t.Fatalf("tool_choice auto not set")
	}
}

func TestToolChoiceRequired(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"tool_choice":"required","stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolChoice.OfAny == nil {
		t.Fatalf("tool_choice required -> any not set")
	}
}

func TestUnsupportedHostedToolChoiceReturnsError(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
			OfHostedTool: &oairesponses.ToolChoiceTypesParam{
				Type: oairesponses.ToolChoiceTypesTypeImageGeneration,
			},
		},
	}
	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported tool_choice") {
		t.Fatalf("expected unsupported tool_choice error, got %v", err)
	}
}

func TestUnsupportedToolDefinitionReturnsError(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{{
			OfImageGeneration: &oairesponses.ToolImageGenerationParam{},
		}},
	}
	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported tool") {
		t.Fatalf("expected unsupported tool error, got %v", err)
	}
}

func TestToolSearchOutputUnsupportedToolReturnsError(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{
				oairesponses.ResponseInputItemParamOfToolSearchOutput([]oairesponses.ToolUnionParam{{
					OfImageGeneration: &oairesponses.ToolImageGenerationParam{},
				}}),
			},
		},
	}
	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported tool") {
		t.Fatalf("expected unsupported tool error, got %v", err)
	}
}

func TestAllowedToolsFiltersAnthropicToolsAndUsesRequiredMode(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{
			{OfFunction: &oairesponses.FunctionToolParam{Name: "keep", Parameters: map[string]any{"type": "object"}}},
			{OfFunction: &oairesponses.FunctionToolParam{Name: "drop", Parameters: map[string]any{"type": "object"}}},
		},
		ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
			OfAllowedTools: &oairesponses.ToolChoiceAllowedParam{
				Mode:  oairesponses.ToolChoiceAllowedModeRequired,
				Tools: []map[string]any{{"type": "function", "name": "keep"}},
			},
		},
	}
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].OfTool.Name != "keep" {
		t.Fatalf("allowed_tools did not filter tools: %+v", out.Tools)
	}
	if out.ToolChoice.OfAny == nil {
		t.Fatalf("required allowed_tools should map to Anthropic any: %+v", out.ToolChoice)
	}
}

func TestAllowedToolsErrorsWhenNoSupportedToolsRemain(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{
			{OfFunction: &oairesponses.FunctionToolParam{Name: "available", Parameters: map[string]any{"type": "object"}}},
		},
		ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
			OfAllowedTools: &oairesponses.ToolChoiceAllowedParam{
				Mode:  oairesponses.ToolChoiceAllowedModeRequired,
				Tools: []map[string]any{{"type": "function", "name": "missing"}},
			},
		},
	}
	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("expected allowed_tools error, got %v", err)
	}
}

func TestAllowedToolsRejectsUnsupportedAllowedEntries(t *testing.T) {
	tests := []struct {
		name string
		tool map[string]any
	}{
		{name: "mcp", tool: map[string]any{"type": "mcp", "server_label": "deepwiki"}},
		{name: "image_generation", tool: map[string]any{"type": "image_generation"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &oairesponses.ResponseNewParams{
				Model: "gpt-5",
				Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
				Tools: []oairesponses.ToolUnionParam{
					{OfFunction: &oairesponses.FunctionToolParam{Name: "keep", Parameters: map[string]any{"type": "object"}}},
				},
				ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
					OfAllowedTools: &oairesponses.ToolChoiceAllowedParam{
						Mode: oairesponses.ToolChoiceAllowedModeRequired,
						Tools: []map[string]any{
							{"type": "function", "name": "keep"},
							tt.tool,
						},
					},
				},
			}
			_, err := ToAnthropic(req, &config.Config{})
			if err == nil || !strings.Contains(err.Error(), "unsupported tool_choice") {
				t.Fatalf("expected unsupported tool_choice error, got %v", err)
			}
		})
	}
}

func TestAllowedToolsRejectsPartialIdentity(t *testing.T) {
	tests := []struct {
		name  string
		entry string
		want  string
	}{
		{name: "missing_type", entry: `{"name":"keep"}`, want: "requires a type"},
		{name: "missing_name", entry: `{"type":"function"}`, want: "requires a name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := mustReq(t, `{
				"model":"gpt-5",
				"input":"hi",
				"tools":[{"type":"function","name":"keep","parameters":{"type":"object"}}],
				"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[`+tt.entry+`]}
			}`)

			_, err := ToAnthropic(req, &config.Config{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected incomplete allowed tool identity error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestAllowedToolsRejectsCrossTypeSameName(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{
			{OfFunction: &oairesponses.FunctionToolParam{Name: "search", Parameters: map[string]any{"type": "object"}}},
			{OfCustom: &oairesponses.CustomToolParam{Name: "search"}},
		},
		ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
			OfAllowedTools: &oairesponses.ToolChoiceAllowedParam{
				Mode:  oairesponses.ToolChoiceAllowedModeAuto,
				Tools: []map[string]any{{"type": "function", "name": "search"}},
			},
		},
	}

	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), `conversion name conflict`) {
		t.Fatalf("expected cross-type name conflict error, got %v", err)
	}
}

func TestStructuredOutputStillValidatesAllowedTools(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{
			{OfFunction: &oairesponses.FunctionToolParam{Name: "available", Parameters: map[string]any{"type": "object"}}},
		},
		Text: oairesponses.ResponseTextConfigParam{
			Format: oairesponses.ResponseFormatTextConfigParamOfJSONSchema("result", map[string]any{"type": "object"}),
		},
		ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
			OfAllowedTools: &oairesponses.ToolChoiceAllowedParam{
				Mode:  oairesponses.ToolChoiceAllowedModeAuto,
				Tools: []map[string]any{{"type": "mcp", "server_label": "unsupported"}},
			},
		},
	}

	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), `unsupported tool_choice`) {
		t.Fatalf("expected structured output to validate unsupported allowed tool, got %v", err)
	}
}

func TestAllowedToolsFiltersFunctionCustomAndShell(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{
			{OfFunction: &oairesponses.FunctionToolParam{Name: "keep_function", Parameters: map[string]any{"type": "object"}}},
			{OfCustom: &oairesponses.CustomToolParam{Name: "keep_custom"}},
			{OfShell: &oairesponses.FunctionShellToolParam{}},
			{OfFunction: &oairesponses.FunctionToolParam{Name: "drop", Parameters: map[string]any{"type": "object"}}},
		},
		ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
			OfAllowedTools: &oairesponses.ToolChoiceAllowedParam{
				Mode: oairesponses.ToolChoiceAllowedModeRequired,
				Tools: []map[string]any{
					{"type": "function", "name": "keep_function"},
					{"type": "custom", "name": "keep_custom"},
					{"type": "shell"},
				},
			},
		},
	}

	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 3 || findTool(out.Tools, "keep_function") == nil || findTool(out.Tools, "keep_custom") == nil || findTool(out.Tools, "shell") == nil {
		t.Fatalf("allowed tools not filtered exactly: %+v", out.Tools)
	}
	if out.ToolChoice.OfAny == nil {
		t.Fatalf("required allowed_tools should map to Anthropic any: %+v", out.ToolChoice)
	}
}

func TestAllowedToolsJSONModesAndParallelToolCalls(t *testing.T) {
	tests := []struct {
		name           string
		mode           string
		parallel       string
		wantAny        bool
		wantDisablePar bool
	}{
		{name: "auto", mode: "auto"},
		{name: "required", mode: "required", wantAny: true},
		{name: "auto_parallel_false", mode: "auto", parallel: `,"parallel_tool_calls":false`, wantDisablePar: true},
		{name: "required_parallel_false", mode: "required", parallel: `,"parallel_tool_calls":false`, wantAny: true, wantDisablePar: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := mustReq(t, `{
				"model":"gpt-5",
				"input":"hi",
				"tools":[
					{"type":"function","name":"keep","parameters":{"type":"object"}},
					{"type":"function","name":"drop","parameters":{"type":"object"}}
				],
				"tool_choice":{"type":"allowed_tools","mode":"`+tt.mode+`","tools":[{"type":"function","name":"keep"}]},
				"stream":true`+tt.parallel+`
			}`)
			out, err := ToAnthropic(req, &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Tools) != 1 || out.Tools[0].OfTool.Name != "keep" {
				t.Fatalf("allowed_tools JSON path did not filter tools: %+v", out.Tools)
			}
			if tt.wantAny {
				if out.ToolChoice.OfAny == nil {
					t.Fatalf("required allowed_tools should map to any: %+v", out.ToolChoice)
				}
			} else if out.ToolChoice.OfAuto == nil {
				t.Fatalf("auto allowed_tools should map to auto: %+v", out.ToolChoice)
			}
			gotDisable := out.ToolChoice.GetDisableParallelToolUse()
			if tt.wantDisablePar {
				if gotDisable == nil || !*gotDisable {
					t.Fatalf("disable_parallel_tool_use not set: %+v", out.ToolChoice)
				}
			} else if gotDisable != nil {
				t.Fatalf("disable_parallel_tool_use should be unset: %+v", out.ToolChoice)
			}
		})
	}
}

func TestAllowedToolsJSONSupportsNamelessTools(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		wantName string
	}{
		{name: "shell", tool: `{"type":"shell"}`, wantName: "shell"},
		{name: "local_shell", tool: `{"type":"local_shell"}`, wantName: "shell"},
		{name: "apply_patch", tool: `{"type":"apply_patch"}`, wantName: "apply_patch"},
		{name: "tool_search", tool: `{"type":"tool_search","execution":"client","parameters":{"type":"object"}}`, wantName: "tool_search"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := mustReq(t, `{
				"model":"gpt-5",
				"input":"hi",
				"tools":[`+tt.tool+`],
				"tool_choice":{"type":"allowed_tools","mode":"required","tools":[`+tt.tool+`]}
			}`)
			out, err := ToAnthropic(req, &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Tools) != 1 || out.Tools[0].OfTool == nil || out.Tools[0].OfTool.Name != tt.wantName {
				t.Fatalf("allowed_tools did not retain %s: %+v", tt.wantName, out.Tools)
			}
		})
	}
}

func TestAllowedToolsJSONSupportsNamespaceChildren(t *testing.T) {
	for _, mode := range []string{"auto", "required"} {
		t.Run(mode, func(t *testing.T) {
			req := mustReq(t, `{
				"model":"gpt-5",
				"input":"hi",
				"tools":[{"type":"namespace","name":"crm","tools":[
					{"type":"function","name":"lookup","parameters":{"type":"object"}},
					{"type":"custom","name":"raw"}
				]}],
				"tool_choice":{"type":"allowed_tools","mode":"`+mode+`","tools":[{"type":"namespace","name":"crm","tools":[
					{"type":"function","name":"lookup"},
					{"type":"custom","name":"raw"}
				]}]}
			}`)
			out, err := ToAnthropic(req, &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Tools) != 2 || out.Tools[0].OfTool.Name != "crm__lookup" || out.Tools[1].OfTool.Name != "crm__raw" {
				t.Fatalf("namespace allowed_tools did not retain flattened children: %+v", out.Tools)
			}
			if mode == "required" && out.ToolChoice.OfAny == nil {
				t.Fatalf("required namespace allowed_tools should map to any: %+v", out.ToolChoice)
			}
			if mode == "auto" && out.ToolChoice.OfAuto == nil {
				t.Fatalf("auto namespace allowed_tools should map to auto: %+v", out.ToolChoice)
			}
		})
	}
}

func TestAllowedToolsJSONRejectsUnknownNamespaceChild(t *testing.T) {
	req := mustReq(t, `{
		"model":"gpt-5",
		"input":"hi",
		"tools":[{"type":"namespace","name":"crm","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"namespace","name":"crm","tools":[{"type":"function","name":"missing"}]}]}
	}`)
	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), `function "missing" in namespace "crm" is not declared`) {
		t.Fatalf("expected explicit unknown namespace child error, got %v", err)
	}
}

func TestParallelToolCallsFalseDisablesAnthropicParallelUse(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"parallel_tool_calls":false,"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolChoice.OfAuto == nil {
		t.Fatalf("parallel_tool_calls=false should set auto tool_choice when no explicit choice: %+v", out.ToolChoice)
	}
	if got := out.ToolChoice.GetDisableParallelToolUse(); got == nil || !*got {
		t.Fatalf("disable_parallel_tool_use not set: %+v", out.ToolChoice)
	}
}

func TestParallelToolCallsFalsePreservesExplicitToolChoice(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"search"},"parallel_tool_calls":false,"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolChoice.OfTool == nil || out.ToolChoice.OfTool.Name != "search" {
		t.Fatalf("explicit tool choice not preserved: %+v", out.ToolChoice)
	}
	if got := out.ToolChoice.GetDisableParallelToolUse(); got == nil || !*got {
		t.Fatalf("disable_parallel_tool_use not set on explicit tool choice: %+v", out.ToolChoice)
	}
}

func TestUnsupportedDeferredToolReturnsError(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"web_search_preview"}],"tool_choice":"required","stream":true}`)
	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported tool") {
		t.Fatalf("expected unsupported tool error, got %v", err)
	}
}

func TestInputFileDataConvertsToAnthropicDocument(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"read this"},{"type":"input_file","filename":"log.pdf","file_data":"data:application/pdf;base64,JVBERi0x"}]}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || len(out.Messages[0].Content) != 2 {
		t.Fatalf("expected text and document blocks: %+v", out.Messages)
	}
	doc := out.Messages[0].Content[1].OfDocument
	if doc == nil {
		t.Fatalf("input_file not converted to document: %+v", out.Messages[0].Content[1])
	}
	if doc.Title.Value != "log.pdf" {
		t.Fatalf("filename not mapped to title: %+v", doc.Title)
	}
	if doc.Source.OfBase64 == nil || doc.Source.OfBase64.Data != "JVBERi0x" {
		t.Fatalf("file_data not mapped to base64 source: %+v", doc.Source)
	}
}

func TestInputFileURLConvertsToAnthropicDocument(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"log.pdf","file_url":"https://example.com/log.pdf"}]}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || len(out.Messages[0].Content) != 1 {
		t.Fatalf("expected one document block: %+v", out.Messages)
	}
	doc := out.Messages[0].Content[0].OfDocument
	if doc == nil {
		t.Fatalf("input_file not converted to document: %+v", out.Messages[0].Content[0])
	}
	if doc.Source.OfURL == nil || doc.Source.OfURL.URL != "https://example.com/log.pdf" {
		t.Fatalf("file_url not mapped to url source: %+v", doc.Source)
	}
}

func TestSystemGetsAnthropicCacheControl(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","instructions":"be brief","input":"hi","stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.System) != 1 {
		t.Fatalf("expected one system block: %+v", out.System)
	}
	if out.System[0].CacheControl.TTL != anthropic.CacheControlEphemeralTTLTTL5m {
		t.Fatalf("system cache_control not set to 5m: %+v", out.System[0].CacheControl)
	}
}

func TestLastToolGetsAnthropicCacheControl(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"first","parameters":{"type":"object"}},{"type":"function","name":"last","parameters":{"type":"object"}}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("expected two tools: %+v", out.Tools)
	}
	if out.Tools[0].OfTool.CacheControl.TTL != "" {
		t.Fatalf("only last tool should carry cache_control: %+v", out.Tools[0].OfTool.CacheControl)
	}
	if out.Tools[1].OfTool.CacheControl.TTL != anthropic.CacheControlEphemeralTTLTTL5m {
		t.Fatalf("last tool cache_control not set to 5m: %+v", out.Tools[1].OfTool.CacheControl)
	}
}

func TestStringInput(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hello world","stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out.Messages))
	}
	if out.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("expected user role")
	}
}

func TestPlaintextThinkingSignatureRoundTrip(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"reasoning","id":"rs_0","summary":[{"type":"summary_text","text":"think"}]}],"stream":true}`)
	prevItems := []model.OutputItem{
		{Type: "reasoning", ID: "rs_0", Signature: "EqQBCg..."},
	}
	out, err := ToAnthropic(req, &config.Config{}, prevItems...)
	if err != nil {
		t.Fatal(err)
	}
	// Find the thinking block in the assistant message.
	found := false
	for _, msg := range out.Messages {
		for _, blk := range msg.Content {
			if blk.OfThinking != nil && blk.OfThinking.Signature == "EqQBCg..." {
				found = true
			}
		}
	}
	if !found {
		b, _ := json.Marshal(out)
		t.Fatalf("thinking block with signature not found: %s", b)
	}
}

func TestToInputSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []any{"command"}, // JSON 反序列化来源是 []any，非 []string
	}
	got := toInputSchema(schema)

	props, ok := got.Properties.(map[string]any)
	if !ok {
		t.Fatalf("Properties = %T, want map[string]any", got.Properties)
	}
	if _, exists := props["command"]; !exists {
		t.Errorf("Properties missing 'command': %#v", props)
	}
	if len(got.Required) != 1 || got.Required[0] != "command" {
		t.Errorf("Required = %v, want [command]", got.Required)
	}

	// 回归：序列化后 input_schema 不得 properties 套 properties（智谱 400 code 1210 根因）。
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"properties":{"properties"`) {
		t.Errorf("input_schema double-wrapped under properties: %s", b)
	}
	if !strings.Contains(string(b), `"type":"object"`) {
		t.Errorf("input_schema missing type=object: %s", b)
	}
}

// disable_response_storage=true 时 Codex 在 input 里带完整对话历史，
// reasoning item 的 encrypted_content 携带 thinking signature。
// 验证 convert 能从 encrypted_content 恢复 thinking block 的 signature。
func TestReasoningEncryptedContentAsSignatureZDR(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"reasoning","id":"rs_0","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"sigZDR"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range out.Messages {
		for _, blk := range msg.Content {
			if blk.OfThinking != nil && blk.OfThinking.Signature == "sigZDR" && blk.OfThinking.Thinking == "think" {
				found = true
			}
		}
	}
	if !found {
		b, _ := json.Marshal(out)
		t.Fatalf("thinking block with ZDR signature not found: %s", b)
	}
}

// redacted thinking（无 summary 文本）的 encrypted_content 应转为 redacted_thinking block。
func TestRedactedThinkingFromEncryptedContent(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"reasoning","id":"rs_0","encrypted_content":"redactedData"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range out.Messages {
		for _, blk := range msg.Content {
			if blk.OfRedactedThinking != nil && blk.OfRedactedThinking.Data == "redactedData" {
				found = true
			}
		}
	}
	if !found {
		b, _ := json.Marshal(out)
		t.Fatalf("redacted_thinking block not found: %s", b)
	}
}
