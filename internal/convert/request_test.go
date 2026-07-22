package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// captureWarnLogger 把 slog 默认 logger 替换为写入 buf 的 JSON handler（含 WARN 级别），
// 返回还原函数。用于验证静默跳过路径是否按约定输出 WARN。
func captureWarnLogger(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf, func() { slog.SetDefault(old) }
}

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
	out, _, err := ToAnthropic(req, &config.Config{})
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

func TestReasoningEffortMapsToOutputConfigEffort(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"high"},"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"thinking":{"type":"enabled"}`) {
		t.Fatalf("thinking not enabled: %s", b)
	}
	if out.OutputConfig.Effort != anthropic.OutputConfigEffortHigh {
		t.Fatalf("expected output_config.effort=high, got %q", out.OutputConfig.Effort)
	}
}

func TestReasoningEffortLowMapsToOutputConfigLow(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"low"},"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.OutputConfig.Effort != anthropic.OutputConfigEffortLow {
		t.Fatalf("expected output_config.effort=low, got %q", out.OutputConfig.Effort)
	}
}

func TestReasoningEffortMaxMapsToOutputConfigMax(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"max"},"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.OutputConfig.Effort != anthropic.OutputConfigEffortMax {
		t.Fatalf("expected output_config.effort=max, got %q", out.OutputConfig.Effort)
	}
}

func TestReasoningEffortNoneDisablesThinking(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"none"},"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking.OfDisabled == nil {
		t.Fatalf("effort=none should disable thinking")
	}
	if out.OutputConfig.Effort != "" {
		t.Fatalf("effort=none should not set output_config.effort, got %q", out.OutputConfig.Effort)
	}
}

func TestReasoningEffortNoneDisables(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"none"},"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking.OfDisabled == nil {
		t.Fatalf("effort=none should disable thinking")
	}
}

func TestDeveloperRoleFoldsToSystem(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","instructions":"be brief","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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

func TestAssistantPhaseNotInjectedIntoAnthropicText(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"input_text","text":"I am checking files."}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) < 1 || out.Messages[0].Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("assistant message not converted: %+v", out.Messages)
	}
	text := out.Messages[0].Content[0].OfText
	if text == nil {
		t.Fatalf("assistant text block missing: %+v", out.Messages[0].Content[0])
	}
	// 注入 <assistant_phase> 标记会导致上游模型模仿该标记，只输出标记而丢失正文。
	// 因此 assistant 消息文本必须原样保留，不得注入 phase 标记。
	if strings.Contains(text.Text, "<assistant_phase>") {
		t.Fatalf("assistant phase marker must not be injected: %q", text.Text)
	}
	if !strings.Contains(text.Text, "I am checking files.") {
		t.Fatalf("assistant message text lost: %q", text.Text)
	}
}

func TestToolSearchItemsConvert(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"tool_search_call","call_id":"ts1","arguments":{"q":"crm"}},
		{"type":"tool_search_output","call_id":"ts1","tools":[{"type":"function","name":"lookup","description":"lookup contact","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"use the loaded tools"}]}
	],"tools":[{"type":"tool_search","execution":"client","description":"search deferred tools","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if findTool(out.Tools, "tool_search") == nil {
		t.Fatalf("top-level tool_search not exposed: %+v", out.Tools)
	}
	if findTool(out.Tools, "lookup") == nil {
		t.Fatalf("tool_search_output tools not exposed: %+v", out.Tools)
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
}

func TestCompactionItemsPreservedAsSystemContext(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"compaction","encrypted_content":"sealed-context"},
		{"type":"compaction_trigger"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
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
		{"type":"image_generation_call","id":"ig_1","status":"completed","result":"base64data"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
	],"stream":true}`)
	buf, restore := captureWarnLogger(t)
	defer restore()
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// 无 Anthropic 等价语义的历史 item 改为 WARN + 丢弃，禁止 raw dump 污染 system。
	for _, s := range out.System {
		if strings.Contains(s.Text, "openai_input_item") {
			t.Fatalf("unsupported history item must not raw-dump: %s", s.Text)
		}
	}
	if !strings.Contains(buf.String(), "image_generation_call") {
		t.Fatalf("expected WARN for image_generation_call, got: %s", buf.String())
	}
}

func TestWebSearchToolMapsToAnthropicServerTool(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com","docs.example.com"]}}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("web_search must not fail fast: %v", err)
	}
	ws := findWebSearchTool(out.Tools)
	if ws == nil {
		b, _ := json.Marshal(out.Tools)
		t.Fatalf("web_search not mapped to Anthropic server tool: %s", b)
	}
	if len(ws.AllowedDomains) != 2 || ws.AllowedDomains[0] != "example.com" || ws.AllowedDomains[1] != "docs.example.com" {
		t.Fatalf("allowed_domains not mapped from filters: %v", ws.AllowedDomains)
	}
}

func TestWebSearchPreviewToolMapsToAnthropicServerTool(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"web_search_preview"}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("web_search_preview must not fail fast: %v", err)
	}
	if findWebSearchTool(out.Tools) == nil {
		t.Fatalf("web_search_preview not mapped: %+v", out.Tools)
	}
}

func TestWebSearchUserLocationViaToAnthropic(t *testing.T) {
	req := mustReq(t, `{
		"model":"gpt-5",
		"input":"hi",
		"tools":[{
			"type":"web_search",
			"filters":{"allowed_domains":["example.com"]},
			"user_location":{"type":"approximate","city":"Shanghai","country":"CN","region":"Shanghai","timezone":"Asia/Shanghai"},
			"search_context_size":"high"
		}],
		"stream":true
	}`)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })

	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var ws *anthropic.WebSearchTool20250305Param
	for _, tool := range out.Tools {
		if tool.OfWebSearchTool20250305 != nil {
			ws = tool.OfWebSearchTool20250305
			break
		}
	}
	if ws == nil {
		t.Fatalf("web_search tool missing: %+v", out.Tools)
	}
	if !ws.UserLocation.City.Valid() || ws.UserLocation.City.Value != "Shanghai" {
		t.Fatalf("user_location not mapped via convert: %+v", ws.UserLocation)
	}
	if len(ws.AllowedDomains) != 1 || ws.AllowedDomains[0] != "example.com" {
		t.Fatalf("allowed_domains: %+v", ws.AllowedDomains)
	}
	if !strings.Contains(logs.String(), "search_context_size") {
		t.Fatalf("expected search_context_size WARN via convert path, logs: %s", logs.String())
	}
}

func findWebSearchTool(tools []anthropic.ToolUnionParam) *anthropic.WebSearchTool20250305Param {
	for _, tool := range tools {
		if tool.OfWebSearchTool20250305 != nil {
			return tool.OfWebSearchTool20250305
		}
	}
	return nil
}

// TestCacheControlAppliedToNonFunctionTool 复现 gap②:最后一个 tool 是
// web_search(OfWebSearchTool20250305)而非 function(OfTool)时,cache_control
// 仍应加到该 tool 上,否则整个 tools 列表缓存丢失。
func TestCacheControlAppliedToNonFunctionTool(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"web_search"}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Tools) == 0 || out.Tools[0].OfWebSearchTool20250305 == nil {
		t.Fatalf("expected web_search tool to be mapped: %+v", out.Tools)
	}
	cc := out.Tools[0].OfWebSearchTool20250305.CacheControl
	if cc.TTL != anthropic.CacheControlEphemeralTTLTTL5m {
		t.Fatalf("cache_control not applied to non-function tool: %+v", cc)
	}
}

// TestSetLastToolCacheControlUnknownVariantNoPanic 防御：最后一个 tool 是
// 未知变体（未来 SDK 新增）时，default 分支只 Warn 不 panic。
func TestSetLastToolCacheControlUnknownVariantNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked on unknown tool variant: %v", r)
		}
	}()
	// 空 ToolUnionParam（所有变体 nil）触发 default 分支
	setLastToolCacheControl([]anthropic.ToolUnionParam{{}}, anthropic.CacheControlEphemeralParam{})
}

// TestOnlyLatestReasoningPreservedAsThinking verifies the gateway trims
// historical reasoning to the most recent item. Anthropic's extended-thinking
// best practice is to carry only the latest thinking block across turns; older
// ones add tokens and noise and push upstream models toward early end_turn.
func TestOnlyLatestReasoningPreservedAsThinking(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"reasoning","id":"rs_old","summary":[{"type":"summary_text","text":"old thinking"}],"encrypted_content":"sigOld"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1"}]},
		{"type":"reasoning","id":"rs_new","summary":[{"type":"summary_text","text":"new thinking"}],"encrypted_content":"sigNew"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a2"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var thinkTexts []string
	for _, msg := range out.Messages {
		for _, b := range msg.Content {
			if b.OfThinking != nil {
				thinkTexts = append(thinkTexts, b.OfThinking.Thinking)
			}
		}
	}
	if len(thinkTexts) != 1 || thinkTexts[0] != "new thinking" {
		t.Fatalf("expected only latest reasoning preserved, got %v", thinkTexts)
	}
}

// TestAssistantOutputTextHistoryPreserved 验证 Codex 回灌的 assistant
// content[].type=output_text 能恢复并转成 Anthropic assistant text。
// 这是"模型看不到上一轮对话"丢上下文的根因回归：openai-go EasyInputMessage
// content 列表不认 output_text，未恢复时正文被静默清空。
func TestAssistantOutputTextHistoryPreserved(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"写大纲"}]},
		{"type":"message","role":"assistant","id":"msg_1","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"一、开场\n二、ERP 系统\n三、物联网架构平台"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"展开第三章"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"第三章：Agent 智能体服务"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"你看不到上一轮吗"}]}
	],"stream":true}`)

	// Decode 后 OfMessage.content 必须已恢复（不是空 list）。
	asst := req.Input.OfInputItemList[1]
	if asst.OfMessage == nil {
		t.Fatal("assistant item not decoded as OfMessage")
	}
	if n := len(asst.OfMessage.Content.OfInputItemContentList); n != 1 {
		t.Fatalf("assistant content parts = %d, want 1 (output_text restored)", n)
	}
	part := asst.OfMessage.Content.OfInputItemContentList[0]
	if part.OfInputText == nil || !strings.Contains(part.OfInputText.Text, "ERP 系统") {
		t.Fatalf("output_text not restored into input_text: %+v", part)
	}

	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// 期望 user / assistant / user / assistant / user，assistant 正文非空。
	if len(out.Messages) != 5 {
		t.Fatalf("want 5 messages, got %d: %+v", len(out.Messages), out.Messages)
	}
	a1 := out.Messages[1]
	if a1.Role != anthropic.MessageParamRoleAssistant || len(a1.Content) == 0 || a1.Content[0].OfText == nil {
		t.Fatalf("assistant[1] missing text: %+v", a1)
	}
	if !strings.Contains(a1.Content[0].OfText.Text, "物联网架构平台") {
		t.Fatalf("assistant history text lost: %q", a1.Content[0].OfText.Text)
	}
	a2 := out.Messages[3]
	if a2.Content[0].OfText == nil || !strings.Contains(a2.Content[0].OfText.Text, "Agent 智能体") {
		t.Fatalf("second assistant history text lost: %+v", a2)
	}
}

// TestAssistantOutputTextWithToolLoop 覆盖"历史 assistant 文本 + tool 循环"
// 的真实 Codex 回灌形状：中间夹 function_call / function_call_output。
func TestAssistantOutputTextWithToolLoop(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"改 slides"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"先看仓库结构"}]},
		{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
		{"type":"function_call_output","call_id":"c1","output":"slides.md"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"看到 slides.md 了"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
	],"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var asstTexts []string
	for _, msg := range out.Messages {
		if msg.Role != anthropic.MessageParamRoleAssistant {
			continue
		}
		for _, b := range msg.Content {
			if b.OfText != nil && b.OfText.Text != "" {
				asstTexts = append(asstTexts, b.OfText.Text)
			}
		}
	}
	if len(asstTexts) < 2 {
		t.Fatalf("want >=2 non-empty assistant texts, got %v (messages=%+v)", asstTexts, out.Messages)
	}
	joined := strings.Join(asstTexts, "\n")
	if !strings.Contains(joined, "先看仓库结构") || !strings.Contains(joined, "看到 slides.md") {
		t.Fatalf("assistant output_text history lost in tool loop: %v", asstTexts)
	}
}

// TestAssistantRefusalHistoryPreserved 验证 output refusal part 折成可见文本，
// 不至于把整条 assistant 历史抹成空消息。
func TestAssistantRefusalHistoryPreserved(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot help with that."}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) < 2 || out.Messages[1].Content[0].OfText == nil {
		t.Fatalf("refusal not converted: %+v", out.Messages)
	}
	if !strings.Contains(out.Messages[1].Content[0].OfText.Text, "I cannot help with that.") {
		t.Fatalf("refusal text lost: %q", out.Messages[1].Content[0].OfText.Text)
	}
}

func TestToolCallsConvert(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"search x"}]},{"type":"function_call","call_id":"c1","name":"search","arguments":"{\"q\":\"x\"}"},{"type":"function_call_output","call_id":"c1","output":"result-x"}],"tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
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

// TestFunctionCallOutputLargeTextPreserved 锁定网关对 function_call_output
// 大文本的完整透传：SKILL.md 全文通过 exec_command 读取后，以 function_call_output
// 形式回灌，网关须把它原样转成 Anthropic tool_result 的 text block，不得截断。
// 这是 skill 加载机制在网关链路上能否工作的关键转译点。
func TestFunctionCallOutputLargeTextPreserved(t *testing.T) {
	// 构造一段 ~8KB 的伪 SKILL.md 全文（真实 skill 正文量级），含多行结构与中文。
	skillBody := strings.Repeat("# SKILL section line 中文内容保持完整\n", 300)
	raw, err := json.Marshal(skillBody)
	if err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"sed -n 1,260p SKILL.md\"}"},{"type":"function_call_output","call_id":"c1","output":%s}],"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}],"stream":true}`,
		string(raw))
	req := mustReq(t, payload)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// tool_result 应在最后一条 user message 里，且文本完整等于 skillBody。
	last := out.Messages[len(out.Messages)-1]
	if last.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("last message role = %v, want user", last.Role)
	}
	var got string
	for _, b := range last.Content {
		if b.OfToolResult != nil && b.OfToolResult.ToolUseID == "c1" {
			for _, c := range b.OfToolResult.Content {
				if c.OfText != nil {
					got += c.OfText.Text
				}
			}
		}
	}
	if got != skillBody {
		t.Fatalf("function_call_output 大文本被截断或篡改: got len=%d want len=%d\nfirst diff at byte %d",
			len(got), len(skillBody), firstDiff(got, skillBody))
	}
}

// TestFunctionCallOutputContentArrayPreserved 锁定 Codex/官方 wire 的
// function_call_output.output 数组形态（input_text/input_image/input_file）。
// 只测 string 会假阳性：SDK 解出数组后 OfString 为空，旧实现静默变成空 tool_result。
func TestFunctionCallOutputContentArrayPreserved(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c1","name":"inspect","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"input_text","text":"result-text"},
			{"type":"input_image","image_url":"https://example.com/shot.png"},
			{"type":"input_file","filename":"note.txt","file_data":"data:text/plain;base64,aGVsbG8="}
		]}
	],"tools":[{"type":"function","name":"inspect","parameters":{"type":"object"}}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tr := findToolResult(out.Messages, "c1")
	if tr == nil {
		t.Fatalf("tool_result missing: %+v", out.Messages)
	}
	if len(tr.Content) < 3 {
		t.Fatalf("want >=3 tool_result parts, got %d: %+v", len(tr.Content), tr.Content)
	}
	if tr.Content[0].OfText == nil || tr.Content[0].OfText.Text != "result-text" {
		t.Fatalf("input_text part lost: %+v", tr.Content[0])
	}
	if tr.Content[1].OfImage == nil {
		t.Fatalf("input_image part not mapped to tool_result image: %+v", tr.Content[1])
	}
	if tr.Content[2].OfDocument == nil {
		t.Fatalf("input_file part not mapped to tool_result document: %+v", tr.Content[2])
	}
}

// TestCustomToolCallOutputContentListPreserved 锁定 custom_tool_call_output.output
// 为 content list 时不得静默变空 tool_result。
func TestCustomToolCallOutputContentListPreserved(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"custom_tool_call","call_id":"c2","name":"shell","input":"ls"},
		{"type":"custom_tool_call_output","call_id":"c2","output":[
			{"type":"input_text","text":"file.txt"},
			{"type":"input_image","image_url":"https://example.com/a.png"}
		]}
	],"tools":[{"type":"custom","name":"shell"}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tr := findToolResult(out.Messages, "c2")
	if tr == nil {
		t.Fatalf("custom tool_result missing: %+v", out.Messages)
	}
	if len(tr.Content) < 2 {
		t.Fatalf("want >=2 parts, got %d: %+v", len(tr.Content), tr.Content)
	}
	if tr.Content[0].OfText == nil || tr.Content[0].OfText.Text != "file.txt" {
		t.Fatalf("custom content list text lost: %+v", tr.Content[0])
	}
	if tr.Content[1].OfImage == nil {
		t.Fatalf("custom content list image lost: %+v", tr.Content[1])
	}
}

// TestFunctionCallOutputImageFileIDWarns 数组里只有 file_id 的图片无法拉取，应 WARN 且不崩溃。
func TestFunctionCallOutputImageFileIDWarns(t *testing.T) {
	buf, restore := captureWarnLogger(t)
	defer restore()
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c1","name":"shot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","file_id":"file-abc"}]}
	],"tools":[{"type":"function","name":"shot","parameters":{"type":"object"}}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tr := findToolResult(out.Messages, "c1")
	if tr == nil {
		t.Fatalf("tool_result missing after file_id-only image: %+v", out.Messages)
	}
	// 无可用内容时仍保留空 text 占位，避免 tool_use 失配。
	if len(tr.Content) != 1 || tr.Content[0].OfText == nil {
		t.Fatalf("want empty text placeholder, got %+v", tr.Content)
	}
	logs := buf.String()
	if !strings.Contains(logs, "file_id") || !strings.Contains(logs, "file-abc") {
		t.Fatalf("expected WARN for tool output image file_id, got: %s", logs)
	}
}

func firstDiff(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func TestCustomToolCallInputAndOutputConvert(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"edit"}]},
		{"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
		{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}
	],"tools":[{"type":"custom","name":"apply_patch"}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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

// TestToolSearchArgumentsInputIsObject 锁定 tool_search_call 的 arguments（Codex
// 回灌历史时通常是 JSON 字符串）必须转成 tool_use input 的 JSON object 形态。
// 若以字符串透传，GLM 会收到 input="..." 而非 object，对它 .get() 直接 500
// （"'str' object has no attribute 'get'"）——S4 修复回程产出 tool_search_call
// 后，请求侧 appendToolSearchCall 这条回灌路径才被触发，暴露本 bug。
func TestToolSearchArgumentsInputPassthrough(t *testing.T) {
	for _, in := range []any{`{"query":"fetch"}`, ""} {
		got := toolSearchArgumentsInput(in)
		raw, isRaw := got.(json.RawMessage)
		if !isRaw {
			t.Fatalf("input %v: want json.RawMessage, got %T", in, got)
		}
		s := string(raw)
		if len(s) == 0 || s[0] != '{' {
			t.Fatalf("input %v: want object, got %q", in, s)
		}
	}
	got := toolSearchArgumentsInput(`{"q":"x"}</seed>`)
	if string(got.(json.RawMessage)) != `{"q":"x"}` {
		t.Fatalf("seed tail got %s", got)
	}
	// 非 object 字符串原样
	got = toolSearchArgumentsInput(`not-json`)
	if s, ok := got.(string); !ok || s != "not-json" {
		t.Fatalf("want string not-json, got %#v", got)
	}
	if s := string(toolSearchArgumentsInput(nil).(json.RawMessage)); s != "{}" {
		t.Fatalf("nil want {}, got %s", s)
	}
	m := map[string]any{"a": 1}
	if toolSearchArgumentsInput(m) == nil {
		t.Fatal("map pass-through")
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
	out, _, err := ToAnthropic(req, &config.Config{})
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

func TestLocalShellCallPreservesEnvInToolUseInput(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfLocalShellCall: &oairesponses.ResponseInputItemLocalShellCallParam{
					ID:     "local_shell_1",
					CallID: "call_local_shell",
					Action: oairesponses.ResponseInputItemLocalShellCallActionParam{
						Command:          []string{"echo", "hi"},
						Env:              map[string]string{"FOO": "bar"},
						WorkingDirectory: oparam.NewOpt("/tmp"),
						TimeoutMs:        oparam.NewOpt(int64(5000)),
						User:             oparam.NewOpt("runner"),
					},
				},
			}},
		},
	}
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	toolUse := out.Messages[0].Content[0].OfToolUse
	if toolUse == nil || toolUse.Name != "shell" {
		t.Fatalf("expected shell tool_use: %+v", out.Messages[0].Content[0])
	}
	in, ok := toolUse.Input.(map[string]any)
	if !ok {
		t.Fatalf("input type %T", toolUse.Input)
	}
	if got := fmt.Sprint(in["input"]); !strings.Contains(got, "echo hi") {
		t.Fatalf("command text lost: %#v", in)
	}
	env, _ := in["env"].(map[string]string)
	if env == nil {
		// JSON round-trip may produce map[string]any
		if raw, ok := in["env"].(map[string]any); ok {
			if raw["FOO"] != "bar" {
				t.Fatalf("env FOO lost: %#v", raw)
			}
		} else {
			t.Fatalf("env missing: %#v", in)
		}
	} else if env["FOO"] != "bar" {
		t.Fatalf("env FOO lost: %#v", env)
	}
	if in["working_directory"] != "/tmp" {
		t.Fatalf("working_directory: %#v", in["working_directory"])
	}
	if fmt.Sprint(in["timeout_ms"]) != "5000" {
		t.Fatalf("timeout_ms: %#v", in["timeout_ms"])
	}
	if in["user"] != "runner" {
		t.Fatalf("user: %#v", in["user"])
	}
}

func TestShellCallRecordsEnvironmentType(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfShellCall: &oairesponses.ResponseInputItemShellCallParam{
					CallID: "call_shell",
					Action: oairesponses.ResponseInputItemShellCallActionParam{
						Commands: []string{"ls"},
					},
					Environment: oairesponses.ResponseInputItemShellCallEnvironmentUnionParam{
						OfLocal: &oairesponses.LocalEnvironmentParam{},
					},
				},
			}},
		},
	}
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	in, ok := out.Messages[0].Content[0].OfToolUse.Input.(map[string]any)
	if !ok {
		t.Fatalf("input type %T", out.Messages[0].Content[0].OfToolUse.Input)
	}
	if in["environment_type"] != "local" {
		t.Fatalf("environment_type: %#v", in["environment_type"])
	}
	if fmt.Sprint(in["input"]) != "ls" {
		t.Fatalf("input text: %#v", in["input"])
	}
}

func TestShellCallPreservesLimitsAndCaller(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfShellCall: &oairesponses.ResponseInputItemShellCallParam{
					CallID: "call_shell",
					Status: "completed",
					Action: oairesponses.ResponseInputItemShellCallActionParam{
						Commands:        []string{"ls", "-la"},
						TimeoutMs:       oparam.NewOpt(int64(1200)),
						MaxOutputLength: oparam.NewOpt(int64(4096)),
					},
					Environment: oairesponses.ResponseInputItemShellCallEnvironmentUnionParam{
						OfLocal: &oairesponses.LocalEnvironmentParam{},
					},
					Caller: oairesponses.ResponseInputItemShellCallCallerUnionParam{
						OfProgram: &oairesponses.ResponseInputItemShellCallCallerProgramParam{
							CallerID: "prog_1",
						},
					},
				},
			}},
		},
	}
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	in, ok := out.Messages[0].Content[0].OfToolUse.Input.(map[string]any)
	if !ok {
		t.Fatalf("input type %T", out.Messages[0].Content[0].OfToolUse.Input)
	}
	if in["environment_type"] != "local" || in["status"] != "completed" {
		t.Fatalf("meta: %#v", in)
	}
	if fmt.Sprint(in["timeout_ms"]) != "1200" || fmt.Sprint(in["max_output_length"]) != "4096" {
		t.Fatalf("limits: %#v", in)
	}
	if in["caller_type"] != "program" || in["caller_id"] != "prog_1" {
		t.Fatalf("caller: %#v", in)
	}
	if got := fmt.Sprint(in["input"]); !strings.Contains(got, "ls") {
		t.Fatalf("commands: %#v", in["input"])
	}
}

func TestShellCallOutputIncludesOutcomeAndStatus(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfShellCallOutput: &oairesponses.ResponseInputItemShellCallOutputParam{
					CallID:          "call_shell",
					Status:          "completed",
					MaxOutputLength: oparam.NewOpt(int64(100)),
					Output: []oairesponses.ResponseFunctionShellCallOutputContentParam{{
						Stdout: "ok\n",
						Stderr: "warn\n",
						Outcome: oairesponses.ResponseFunctionShellCallOutputContentOutcomeUnionParam{
							OfExit: &oairesponses.ResponseFunctionShellCallOutputContentOutcomeExitParam{ExitCode: 0},
						},
					}},
				},
			}},
		},
	}
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	textOut := out.Messages[0].Content[0].OfToolResult.Content[0].OfText.Text
	for _, want := range []string{"[status=completed]", "[max_output_length=100]", "ok", "warn", "[exit_code=0]"} {
		if !strings.Contains(textOut, want) {
			t.Fatalf("missing %q in %q", want, textOut)
		}
	}
}

func TestApplyPatchCallPreservesStatusAndCaller(t *testing.T) {
	// freeform 回灌：status/caller 无 Anthropic 字段，只校验 V4A 正文。
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfApplyPatchCall: &oairesponses.ResponseInputItemApplyPatchCallParam{
					CallID: "call_patch",
					Status: "completed",
					Operation: oairesponses.ResponseInputItemApplyPatchCallOperationUnionParam{
						OfDeleteFile: &oairesponses.ResponseInputItemApplyPatchCallOperationDeleteFileParam{Path: "x.txt"},
					},
					Caller: oairesponses.ResponseInputItemApplyPatchCallCallerUnionParam{
						OfDirect: &oairesponses.ResponseInputItemApplyPatchCallCallerDirectParam{},
					},
				},
			}},
		},
	}
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	in := out.Messages[0].Content[0].OfToolUse.Input.(map[string]any)
	got, _ := in["input"].(string)
	want := "*** Begin Patch\n*** Delete File: x.txt\n*** End Patch"
	if got != want {
		t.Fatalf("apply_patch freeform: %#v want %q", in, want)
	}
}

func TestApplyPatchCallOutputIncludesStatus(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfApplyPatchCallOutput: &oairesponses.ResponseInputItemApplyPatchCallOutputParam{
					CallID: "call_patch",
					Status: "failed",
					Output: oparam.NewOpt("conflict"),
				},
			}},
		},
	}
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	textOut := out.Messages[0].Content[0].OfToolResult.Content[0].OfText.Text
	if !strings.Contains(textOut, "[status=failed]") || !strings.Contains(textOut, "conflict") {
		t.Fatalf("output: %q", textOut)
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
			out, _, err := ToAnthropic(req, &config.Config{})
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
			patch, _ := input["input"].(string)
			if !strings.HasPrefix(patch, "*** Begin Patch\n") || !strings.HasSuffix(patch, "*** End Patch") {
				t.Fatalf("apply_patch freeform markers: %q", patch)
			}
			if !strings.Contains(patch, tt.wantPath) {
				t.Fatalf("apply_patch path missing: %q want %q", patch, tt.wantPath)
			}
			switch tt.wantType {
			case "create_file":
				if !strings.Contains(patch, "*** Add File: ") {
					t.Fatalf("create header missing: %q", patch)
				}
			case "update_file":
				if !strings.Contains(patch, "*** Update File: ") {
					t.Fatalf("update header missing: %q", patch)
				}
			case "delete_file":
				if !strings.Contains(patch, "*** Delete File: ") {
					t.Fatalf("delete header missing: %q", patch)
				}
			}
			if tt.wantDiff != nil && !strings.Contains(patch, strings.TrimSpace(*tt.wantDiff)) {
				// diff 可能被重包进 V4A，至少保留关键片段
				if !strings.Contains(patch, "+new") && !strings.Contains(patch, *tt.wantDiff) {
					t.Fatalf("apply_patch diff lost: %q want %q", patch, *tt.wantDiff)
				}
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
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens != 4096 {
		t.Fatalf("expected default 4096, got %d", out.MaxTokens)
	}
}

func TestOutputConfigEffortDoesNotAlterMaxTokens(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","max_output_tokens":2048,"reasoning":{"effort":"high"},"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.OutputConfig.Effort != anthropic.OutputConfigEffortHigh {
		t.Fatalf("expected effort=high, got %q", out.OutputConfig.Effort)
	}
	if out.MaxTokens != 2048 {
		t.Fatalf("max_tokens should remain 2048, got %d", out.MaxTokens)
	}
}

func TestReasoningSummaryConciseSetsDisplay(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"medium","summary":"concise"},"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"display":"summarized"`) {
		t.Fatalf("concise summary should set display=summarized, got: %s", b)
	}
}

func TestToolChoiceAuto(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"tool_choice":"auto","stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolChoice.OfAuto == nil {
		t.Fatalf("tool_choice auto not set")
	}
}

func TestToolChoiceRequired(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"tool_choice":"required","stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
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
	_, _, err := ToAnthropic(req, &config.Config{})
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
	_, _, err := ToAnthropic(req, &config.Config{})
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
	_, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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
	_, _, err := ToAnthropic(req, &config.Config{})
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
			_, _, err := ToAnthropic(req, &config.Config{})
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

			_, _, err := ToAnthropic(req, &config.Config{})
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

	_, _, err := ToAnthropic(req, &config.Config{})
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

	_, _, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), `unsupported tool_choice`) {
		t.Fatalf("expected structured output to validate unsupported allowed tool, got %v", err)
	}
}

func TestStructuredOutputPrefersSchemaOverAllowedTools(t *testing.T) {
	for _, mode := range []string{"auto", "required"} {
		t.Run(mode, func(t *testing.T) {
			req := mustReq(t, `{
				"model":"gpt-5",
				"input":"hi",
				"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
				"text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"}}},
				"tool_choice":{"type":"allowed_tools","mode":"`+mode+`","tools":[{"type":"function","name":"lookup"}]}
			}`)

			out, _, err := ToAnthropic(req, &config.Config{})
			if err != nil {
				t.Fatalf("expected degrade success, got %v", err)
			}
			if out.ToolChoice.OfTool == nil || out.ToolChoice.OfTool.Name != "result" {
				t.Fatalf("want forced schema tool result, got %+v", out.ToolChoice)
			}
		})
	}
}

func TestStructuredOutputPrefersSchemaOverIncompatibleExplicitToolChoice(t *testing.T) {
	tests := []struct {
		name       string
		tools      string
		toolChoice string
	}{
		{name: "none", toolChoice: `"none"`},
		{name: "auto", toolChoice: `"auto"`},
		{name: "required", toolChoice: `"required"`},
		{name: "function", tools: `[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`, toolChoice: `{"type":"function","name":"lookup"}`},
		{name: "custom", tools: `[{"type":"custom","name":"raw"}]`, toolChoice: `{"type":"custom","name":"raw"}`},
		{name: "apply_patch", tools: `[{"type":"apply_patch"}]`, toolChoice: `{"type":"apply_patch"}`},
		{name: "shell", tools: `[{"type":"shell"}]`, toolChoice: `{"type":"shell"}`},
		{name: "unknown mode", toolChoice: `"unsupported"`},
		{name: "unknown type", toolChoice: `{"type":"unsupported"}`},
		{name: "hosted tool", toolChoice: `{"type":"web_search_preview"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := mustReq(t, `{
				"model":"gpt-5",
				"input":"hi",
				"tools":`+orDefault(tt.tools, "[]")+`,
				"text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"}}},
				"tool_choice":`+tt.toolChoice+`
			}`)

			out, _, err := ToAnthropic(req, &config.Config{})
			if err != nil {
				t.Fatalf("expected degrade success, got %v", err)
			}
			if out.ToolChoice.OfTool == nil || out.ToolChoice.OfTool.Name != "result" {
				t.Fatalf("want forced schema tool result, got %+v", out.ToolChoice)
			}
		})
	}
}

func TestAllowedToolsRejectsUnknownMode(t *testing.T) {
	req := mustReq(t, `{
		"model":"gpt-5",
		"input":"hi",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":{"type":"allowed_tools","mode":"unexpected","tools":[{"type":"function","name":"lookup"}]}
	}`)

	_, _, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), `allowed_tools mode "unexpected"`) {
		t.Fatalf("expected unknown allowed_tools mode error, got %v", err)
	}
}

func TestSpecificToolChoiceRejectsUndeclaredIdentity(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "function_custom_same_name",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"custom","name":"search"}],"tool_choice":{"type":"function","name":"search"}}`,
			want: `function "search" is not declared`,
		},
		{
			name: "function_missing",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"other","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"search"}}`,
			want: `function "search" is not declared`,
		},
		{
			name: "apply_patch_missing",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"other","parameters":{"type":"object"}}],"tool_choice":{"type":"apply_patch"}}`,
			want: `apply_patch "apply_patch" is not declared`,
		},
		{
			name: "shell_missing",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"other","parameters":{"type":"object"}}],"tool_choice":{"type":"shell"}}`,
			want: `shell "shell" is not declared`,
		},
		{
			name: "shell_does_not_match_local_shell",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"local_shell"}],"tool_choice":{"type":"shell"}}`,
			want: `shell "shell" is not declared`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := ToAnthropic(mustReq(t, tt.body), &config.Config{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestSpecificToolChoiceMapsDeclaredIdentity(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "function",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"}}`,
			want: "lookup",
		},
		{
			name: "custom",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"custom","name":"lookup"}],"tool_choice":{"type":"custom","name":"lookup"}}`,
			want: "lookup",
		},
		{
			name: "apply_patch",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"apply_patch"}],"tool_choice":{"type":"apply_patch"}}`,
			want: "apply_patch",
		},
		{
			name: "shell",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"shell"}],"tool_choice":{"type":"shell"}}`,
			want: "shell",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := ToAnthropic(mustReq(t, tt.body), &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if out.ToolChoice.OfTool == nil || out.ToolChoice.OfTool.Name != tt.want {
				t.Fatalf("specific choice = %+v, want %q", out.ToolChoice, tt.want)
			}
		})
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

	out, _, err := ToAnthropic(req, &config.Config{})
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
			out, _, err := ToAnthropic(req, &config.Config{})
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
			out, _, err := ToAnthropic(req, &config.Config{})
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
			out, _, err := ToAnthropic(req, &config.Config{})
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
	_, _, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), `function "missing" in namespace "crm" is not declared`) {
		t.Fatalf("expected explicit unknown namespace child error, got %v", err)
	}
}

func TestNamespaceRejectsUnsupportedChild(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{{
			OfNamespace: &oairesponses.NamespaceToolParam{
				Name:  "crm",
				Tools: []oairesponses.NamespaceToolToolUnionParam{{}},
			},
		}},
	}

	_, _, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported namespace tool") {
		t.Fatalf("expected unsupported namespace child error, got %v", err)
	}
}

func TestDecodeRejectsUnsupportedNamespaceChild(t *testing.T) {
	_, err := DecodeResponseNewParams([]byte(`{
		"model":"gpt-5",
		"input":"hi",
		"tools":[{"type":"namespace","name":"crm","tools":[{"type":"shell"}]}]
	}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported namespace tool type") {
		t.Fatalf("expected unsupported namespace child error, got %v", err)
	}
}

func TestParallelToolCallsFalseDisablesAnthropicParallelUse(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"parallel_tool_calls":false,"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"file_search"}],"tool_choice":"required","stream":true}`)
	_, _, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported tool") {
		t.Fatalf("expected unsupported tool error, got %v", err)
	}
}

func TestInputFileDataConvertsToAnthropicDocument(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"read this"},{"type":"input_file","filename":"log.pdf","file_data":"data:application/pdf;base64,JVBERi0x"}]}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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

// TestInputImageFileIDEmitsWarn 验证 input_image.file_id（无 OpenAI Files 凭据）
// 被静默跳过时按 AGENTS.md 约定输出 WARN。
func TestInputImageFileIDEmitsWarn(t *testing.T) {
	buf, restore := captureWarnLogger(t)
	defer restore()

	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file-abc123"}]}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// file_id 被丢弃后该 message 只有一个空 text 占位 block。
	if len(out.Messages) != 1 {
		t.Fatalf("expected one message: %+v", out.Messages)
	}
	logs := buf.String()
	if !strings.Contains(logs, "input_image.file_id") || !strings.Contains(logs, "file-abc123") {
		t.Fatalf("expected WARN for input_image.file_id, got: %s", logs)
	}
}

// TestInputFileFileIDEmitsWarn 验证 input_file.file_id 被静默跳过时输出 WARN。
func TestInputFileFileIDEmitsWarn(t *testing.T) {
	buf, restore := captureWarnLogger(t)
	defer restore()

	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file-xyz789"}]}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("expected one message: %+v", out.Messages)
	}
	logs := buf.String()
	if !strings.Contains(logs, "input_file.file_id") || !strings.Contains(logs, "file-xyz789") {
		t.Fatalf("expected WARN for input_file.file_id, got: %s", logs)
	}
}

// TestSystemRoleImageDroppedEmitsWarn 验证 system/developer message 中的 image
// 被 Anthropic system（仅文本）丢弃时输出 WARN。
func TestSystemRoleImageDroppedEmitsWarn(t *testing.T) {
	buf, restore := captureWarnLogger(t)
	defer restore()

	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"be brief"},{"type":"input_image","image_url":"https://example.com/img.png"}]}],"stream":true}`)
	if _, _, err := ToAnthropic(req, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "image block") || !strings.Contains(logs, "developer") {
		t.Fatalf("expected WARN for system/developer image drop, got: %s", logs)
	}
}

func TestSystemGetsAnthropicCacheControl(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","instructions":"be brief","input":"hi","stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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

// disable_response_storage=true 时 Codex 在 input 里带完整对话历史，
// reasoning item 的 encrypted_content 携带 thinking signature。
// 验证 convert 能从 encrypted_content 恢复 thinking block 的 signature。
func TestReasoningEncryptedContentAsSignatureZDR(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"reasoning","id":"rs_0","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"sigZDR"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
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
	out, _, err := ToAnthropic(req, &config.Config{})
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

// TestTopLevelCacheControlForMessageHistory 复现 gap①:顶层 cache_control
// 必须设置,Anthropic 才会自动缓存 messages 历史(system/tools 已有显式
// breakpoint,顶层 marker 覆盖到 messages 末尾)。
func TestTopLevelCacheControlForMessageHistory(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.CacheControl.TTL == "" {
		t.Fatalf("top-level cache_control not set; message history won't be cached")
	}
}

// TestCacheControlTTLFromConfig 复现 gap④:TTL 必须从 config.Cache.TTL 读,
// "1h" 时顶层 cache_control 用 1h,默认 5m。
func TestCacheControlTTLFromConfig(t *testing.T) {
	t.Run("default 5m", func(t *testing.T) {
		req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true}`)
		out, _, err := ToAnthropic(req, &config.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if out.CacheControl.TTL != anthropic.CacheControlEphemeralTTLTTL5m {
			t.Fatalf("default TTL want 5m, got %v", out.CacheControl.TTL)
		}
	})
	t.Run("1h from config", func(t *testing.T) {
		req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true}`)
		out, _, err := ToAnthropic(req, &config.Config{Cache: config.CacheCfg{TTL: "1h"}})
		if err != nil {
			t.Fatal(err)
		}
		if out.CacheControl.TTL != anthropic.CacheControlEphemeralTTLTTL1h {
			t.Fatalf("configured TTL want 1h, got %v", out.CacheControl.TTL)
		}
	})
}

// TestCodeInterpreterCallInputReplaysAsServerToolUseAndResult 验证历史
// TestWebSearchCallHistoryReplay 把历史 web_search_call 回放为 Anthropic
// server_tool_use(web_search) + web_search_tool_result，让后端识别 hosted 搜索上下文。
func TestWebSearchCallHistoryReplay(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"search go"}]},
		{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"golang tutorial","sources":[{"type":"url","url":"https://go.dev/doc/"}]}},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"see go.dev"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"more"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// 不得再把 web_search_call 整段塞进 system raw dump。
	for _, s := range out.System {
		if strings.Contains(s.Text, "openai_input_item type=\"web_search_call\"") {
			t.Fatalf("web_search_call must not fall through to raw dump: %s", s.Text)
		}
	}
	var foundUse, foundResult bool
	var useID string
	var foundSourceText bool
	for _, msg := range out.Messages {
		for _, b := range msg.Content {
			if b.OfServerToolUse != nil && b.OfServerToolUse.Name == anthropic.ServerToolUseBlockParamNameWebSearch {
				foundUse = true
				useID = b.OfServerToolUse.ID
				raw, _ := json.Marshal(b.OfServerToolUse.Input)
				if !strings.Contains(string(raw), "golang tutorial") {
					t.Fatalf("server_tool_use input missing query: %s", raw)
				}
			}
			if b.OfWebSearchToolResult != nil {
				foundResult = true
				if b.OfWebSearchToolResult.ToolUseID != "ws_1" {
					t.Fatalf("tool_use_id = %q, want ws_1", b.OfWebSearchToolResult.ToolUseID)
				}
				// 无 Anthropic encrypted_content：result content 必须为空数组。
				if n := len(b.OfWebSearchToolResult.Content.OfWebSearchToolResultBlockItem); n != 0 {
					t.Fatalf("want empty result items, got %d: %+v", n, b.OfWebSearchToolResult.Content.OfWebSearchToolResultBlockItem)
				}
			}
			if b.OfText != nil && strings.Contains(b.OfText.Text, "https://go.dev/doc/") {
				foundSourceText = true
			}
		}
	}
	if !foundUse || !foundResult {
		b, _ := json.Marshal(out.Messages)
		t.Fatalf("missing server_tool_use/result (use=%v result=%v id=%q): %s", foundUse, foundResult, useID, b)
	}
	if !foundSourceText {
		t.Fatal("source URLs should be preserved as visible text alongside empty result")
	}
	if useID != "ws_1" {
		t.Fatalf("server_tool_use id = %q, want ws_1", useID)
	}
}

// TestWebSearchCallHistoryOpenPageLossy 非 search action 折成 query 回放，避免 raw dump。
func TestWebSearchCallHistoryOpenPageLossy(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"web_search_call","id":"ws_2","status":"completed","action":{"type":"open_page","url":"https://example.com/page"}},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"ok"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range out.Messages {
		for _, b := range msg.Content {
			if b.OfServerToolUse != nil && b.OfServerToolUse.Name == anthropic.ServerToolUseBlockParamNameWebSearch {
				found = true
				raw, _ := json.Marshal(b.OfServerToolUse.Input)
				if !strings.Contains(string(raw), "https://example.com/page") {
					t.Fatalf("open_page should fold URL into query/input: %s", raw)
				}
			}
		}
	}
	if !found {
		b, _ := json.Marshal(out.Messages)
		t.Fatalf("open_page web_search_call not converted: %s", b)
	}
}

// code_interpreter_call input item 回放为 Anthropic 历史内容块：
// server_tool_use(code_execution, input={code}) + code_execution_tool_result。
// container_id 必须丢弃（Anthropic code execution 无 container 概念）。
func TestCodeInterpreterCallInputReplaysAsServerToolUseAndResult(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},
		{"type":"code_interpreter_call","id":"ci_1","status":"completed","container_id":"cntr_x","code":"print(2)","outputs":[{"type":"logs","logs":"2\n"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("replay must not fail: %v", err)
	}
	raw, _ := json.Marshal(out.Messages)
	if !strings.Contains(string(raw), `"code_execution"`) {
		t.Fatalf("server_tool_use(code_execution) not replayed: %s", raw)
	}
	if !strings.Contains(string(raw), `"code_execution_result"`) {
		t.Fatalf("code_execution_tool_result not replayed: %s", raw)
	}
	if strings.Contains(string(raw), `"cntr_x"`) {
		t.Fatalf("container_id must be dropped on replay: %s", raw)
	}
	if !strings.Contains(string(raw), `"print(2)"`) {
		t.Fatalf("code text must be preserved in server_tool_use input: %s", raw)
	}
	if !strings.Contains(string(raw), `"2\n"`) {
		t.Fatalf("logs stdout must be preserved in code_execution_tool_result: %s", raw)
	}
}

// TestMcpHistoryListAsDeveloperMarker：list_tools 折 developer marker（lossy）。
// approval_request/response 无审批协议，按丢弃 + WARN 处理，不折 marker。
func TestMcpHistoryListAsDeveloperMarker(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"mcp_list_tools","id":"mcp_lt_1","server_label":"weather","tools":[{"name":"get","description":"d","input_schema":{}}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sys := ""
	for _, b := range out.System {
		sys += b.Text
	}
	for _, want := range []string{
		"mcp_list_tools",
		"weather",
		"<tools>get</tools>",
	} {
		if !strings.Contains(sys, want) {
			t.Fatalf("missing %q in system: %s", want, sys)
		}
	}
}

// TestMcpApprovalHistoryDroppedWithWarn：审批 item 丢弃 + WARN，不写 system。
func TestMcpApprovalHistoryDroppedWithWarn(t *testing.T) {
	buf, restore := captureWarnLogger(t)
	defer restore()
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"mcp_approval_request","id":"mcp_ar_1","server_label":"weather","name":"get","arguments":"{}"},
		{"type":"mcp_approval_response","approval_request_id":"mcp_ar_1","approve":true},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sys := ""
	for _, b := range out.System {
		sys += b.Text
	}
	if strings.Contains(sys, "mcp_approval") {
		t.Fatalf("approval history must not be injected into system: %s", sys)
	}
	logs := buf.String()
	if !strings.Contains(logs, "mcp_approval_request") || !strings.Contains(logs, "mcp_approval_response") {
		t.Fatalf("expected WARN for approval items, got: %s", logs)
	}
}

// TestMcpToolProducesMCPInjection 验证 mcp tool 产出 MCPInjection
// （mcp_servers + toolset allowlist），且 mcp 不进入标准 tools[] 列表。
func TestMcpToolProducesMCPInjection(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"weather","server_url":"https://s.example","allowed_tools":["get"]},{"type":"web_search"}],"stream":true}`)
	out, mcp, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("mcp must not fail fast: %v", err)
	}
	if mcp == nil || len(mcp.Servers) != 1 || mcp.Servers[0].Name != "weather" {
		t.Fatalf("MCPInjection not produced: %+v", mcp)
	}
	if len(mcp.Toolsets) != 1 || len(mcp.Toolsets[0].EnabledTools) != 1 {
		t.Fatalf("toolset allowlist wrong: %+v", mcp.Toolsets)
	}
	// mcp tool 不进标准 tools[]（web_search 进，mcp 不进）
	for _, tool := range out.Tools {
		if tool.OfTool != nil && tool.OfTool.Name == "weather" {
			t.Fatal("mcp must not appear as standard ToolParam")
		}
	}
}

// TestMcpConnectorIDFailsFast 验证 connector_id 是 OpenAI 私有托管设施，
// 不在 Anthropic 标准范围，必须 fail-fast。
func TestMcpConnectorIDFailsFast(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"s","connector_id":"cntr_x"}],"stream":true}`)
	if _, _, err := ToAnthropic(req, &config.Config{}); err == nil {
		t.Fatal("connector_id must fail fast")
	}
}

// TestMcpTunnelIDFailsFast 验证 tunnel_id 是 OpenAI 私有托管设施，
// 不在 Anthropic 标准范围，必须 fail-fast。
func TestMcpTunnelIDFailsFast(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"s","tunnel_id":"tnl_x"}],"stream":true}`)
	if _, _, err := ToAnthropic(req, &config.Config{}); err == nil {
		t.Fatal("tunnel_id must fail fast")
	}
}

// TestMcpAllowedToolsFilterDegradesToAllEnabled 验证 allowed_tools filter 变体
// 不支持精确映射，降级为全启用（EnabledTools 空 → default_config.enabled=true）。
func TestMcpAllowedToolsFilterDegradesToAllEnabled(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"weather","server_url":"https://s.example","allowed_tools":{"read_only":true}}],"stream":true}`)
	_, mcp, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("filter variant must not fail fast: %v", err)
	}
	if mcp == nil || len(mcp.Toolsets) != 1 {
		t.Fatalf("MCPInjection toolset missing: %+v", mcp)
	}
	if len(mcp.Toolsets[0].EnabledTools) != 0 {
		t.Fatalf("filter variant must degrade to empty EnabledTools (all-enabled), got: %v", mcp.Toolsets[0].EnabledTools)
	}
}

// TestMcpHeadersCustomHeaderDiscarded 验证自定义 header（非 Authorization）
// 被丢弃（WARN），但请求仍成功且 token 不受影响。
func TestMcpHeadersCustomHeaderDiscarded(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"weather","server_url":"https://s.example","headers":{"X-Custom":"val"}}],"stream":true}`)
	_, mcp, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("custom header must not fail fast: %v", err)
	}
	if mcp == nil || len(mcp.Servers) != 1 {
		t.Fatalf("MCPInjection not produced: %+v", mcp)
	}
	if mcp.Servers[0].AuthorizationToken != "" {
		t.Fatalf("custom header must not leak into authorization_token: %q", mcp.Servers[0].AuthorizationToken)
	}
}

// TestMcpRequireApprovalNonNeverDegrades 验证 require_approval 非 never 时
// 降级为 never（WARN），请求仍成功。
func TestMcpRequireApprovalNonNeverDegrades(t *testing.T) {
	for _, appr := range []string{"on_failure", "if_referenced"} {
		t.Run(appr, func(t *testing.T) {
			req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"weather","server_url":"https://s.example","require_approval":"`+appr+`"}],"stream":true}`)
			_, _, err := ToAnthropic(req, &config.Config{})
			if err != nil {
				t.Fatalf("require_approval=%s must not fail fast: %v", appr, err)
			}
		})
	}
}

// TestMcpAuthorizationHeaderFallback 验证 authorization 字段为空时，
// 从 headers["Authorization"] 提取 token（去除 "Bearer " 前缀）。
func TestMcpAuthorizationHeaderFallback(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"weather","server_url":"https://s.example","headers":{"Authorization":"Bearer tok-from-header"}}],"stream":true}`)
	_, mcp, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("header fallback must not fail: %v", err)
	}
	if mcp == nil || len(mcp.Servers) != 1 {
		t.Fatalf("MCPInjection not produced: %+v", mcp)
	}
	if mcp.Servers[0].AuthorizationToken != "tok-from-header" {
		t.Fatalf("expected token from header, got: %q", mcp.Servers[0].AuthorizationToken)
	}
}

// TestMcpAuthorizationCollisionWarns 验证 authorization 字段与 headers["Authorization"]
// 同时设置时，headers 值被忽略（WARN），authorization 字段优先。
func TestMcpAuthorizationCollisionWarns(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"weather","server_url":"https://s.example","authorization":"tok-field","headers":{"Authorization":"Bearer tok-header"}}],"stream":true}`)
	_, mcp, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("collision must not fail fast: %v", err)
	}
	if mcp == nil || len(mcp.Servers) != 1 {
		t.Fatalf("MCPInjection not produced: %+v", mcp)
	}
	if mcp.Servers[0].AuthorizationToken != "tok-field" {
		t.Fatalf("authorization field must win over header, got: %q", mcp.Servers[0].AuthorizationToken)
	}
}

// TestMetadataUserIDPassthrough 验证 metadata.user_id 被透传到 Anthropic metadata.user_id。
func TestMetadataUserIDPassthrough(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","metadata":{"user_id":"user-123","other":"ignored"},"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Metadata.UserID.Valid() || out.Metadata.UserID.Value != "user-123" {
		t.Fatalf("metadata.user_id not passed through: %+v", out.Metadata)
	}
}

// TestMetadataAbsentLeavesEmpty 验证无 metadata 时不设置 Anthropic metadata。
func TestMetadataAbsentLeavesEmpty(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Metadata.UserID.Valid() {
		t.Fatalf("unexpected metadata.user_id: %q", out.Metadata.UserID.Value)
	}
}

func TestToolUseInputJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		ok   bool
		want string
	}{
		{"empty object", `{}`, true, `{}`},
		{"clean object", `{"cmd":"ls"}`, true, `{"cmd":"ls"}`},
		{"seed xml tail", "{\"cmd\":\"ls\"}\n</function></seed:tool_call>", true, `{"cmd":"ls"}`},
		{"only garbage", `</function>`, false, ""},
		{"array", `[1,2]`, false, ""},
		{"empty", ``, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := toolUseInputJSON(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v", ok, tc.ok)
			}
			if ok && string(got) != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestFunctionCallArgumentsSeedTailPassthrough(t *testing.T) {
	// Codex seed 尾部杂质：应透传 JSON 前缀，而不是整段改成 {}。
	req := mustReq(t, `{
		"model":"gpt-5",
		"input":[
			{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":\"pwd\"}\n</function></seed:tool_call>"}
		],
		"stream":true
	}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) == 0 {
		t.Fatal("expected tool_use message")
	}
	// 找到 tool_use
	found := false
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.OfToolUse != nil && b.OfToolUse.Name == "shell" {
				found = true
				// Input is any; marshal to check command
				raw, _ := json.Marshal(b.OfToolUse.Input)
				if !strings.Contains(string(raw), "pwd") {
					t.Fatalf("expected command pwd in input, got %s", raw)
				}
			}
		}
	}
	if !found {
		t.Fatalf("tool_use not found: %+v", out.Messages)
	}
}

func TestFunctionCallArgumentsNonObjectAsString(t *testing.T) {
	// 解不出 object 时原串当 string 塞进 input，不跳过、不改 {}。
	req := mustReq(t, `{
		"model":"gpt-5",
		"input":[
			{"type":"function_call","call_id":"c1","name":"shell","arguments":"not-json"}
		],
		"stream":true
	}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.OfToolUse != nil && b.OfToolUse.Name == "shell" {
				found = true
				if s, ok := b.OfToolUse.Input.(string); !ok || s != "not-json" {
					t.Fatalf("want string input not-json, got %#v", b.OfToolUse.Input)
				}
			}
		}
	}
	if !found {
		t.Fatal("expected tool_use")
	}
}
