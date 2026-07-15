package convert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
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

func TestToolChoiceDroppedWhenNoToolsSurvive(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"web_search_preview"}],"tool_choice":"required","stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 0 {
		t.Fatalf("unsupported hosted tools should be dropped: %+v", out.Tools)
	}
	if out.ToolChoice.OfAuto != nil || out.ToolChoice.OfAny != nil || out.ToolChoice.OfTool != nil || out.ToolChoice.OfNone != nil {
		t.Fatalf("tool_choice should be dropped when no tools survive: %+v", out.ToolChoice)
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
