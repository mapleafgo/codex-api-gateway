package convert

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// findToolResult 返回 msgs 中 ToolUseID == id 的 tool_result block（没有则 nil）。
func findToolResult(msgs []anthropic.MessageParam, id string) *anthropic.ToolResultBlockParam {
	for i := range msgs {
		for _, b := range msgs[i].Content {
			if b.OfToolResult != nil && b.OfToolResult.ToolUseID == id {
				return b.OfToolResult
			}
		}
	}
	return nil
}

// TestEnsureToolUsePairedMidOrphan：input 含 function_call 但无配对 output，且其后还有
// user message。占位 tool_result 应补到该 user message 的前部，is_error=true。
func TestEnsureToolUsePairedMidOrphan(t *testing.T) {
	buf, restore := captureWarnLogger(t)
	defer restore()

	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"do X"}]},
		{"type":"function_call","call_id":"c1","name":"tool","arguments":"{}"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"nevermind"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}

	tr := findToolResult(out.Messages, "c1")
	if tr == nil {
		t.Fatalf("expected placeholder tool_result for orphan c1, messages=%+v", out.Messages)
	}
	if !tr.IsError.Valid() || !tr.IsError.Value {
		t.Errorf("placeholder tool_result should have is_error=true, got %+v", tr.IsError)
	}
	if len(tr.Content) == 0 || tr.Content[0].OfText == nil || !strings.Contains(tr.Content[0].OfText.Text, "no tool output available") {
		t.Errorf("placeholder text mismatch: %+v", tr.Content)
	}

	// 占位 result 应位于最后一个 user message 的首个 block（原 text 在后）。
	last := out.Messages[len(out.Messages)-1]
	if last.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("last message should be user, got %s", last.Role)
	}
	if last.Content[0].OfToolResult == nil {
		t.Errorf("placeholder result should be the first block of the last user message: %+v", last.Content)
	}

	if !strings.Contains(buf.String(), "补占位 tool_result") {
		t.Errorf("expected WARN about placeholder, got %s", buf.String())
	}
}

// TestEnsureToolUsePairedTailOrphan：orphan tool_use 是最后一条（其后无 user message），
// 应新建 user message 承载占位 result。
func TestEnsureToolUsePairedTailOrphan(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"do X"}]},
		{"type":"function_call","call_id":"c1","name":"tool","arguments":"{}"}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("expected a new tail user message, got role=%s", last.Role)
	}
	if len(last.Content) != 1 || last.Content[0].OfToolResult == nil ||
		last.Content[0].OfToolResult.ToolUseID != "c1" {
		t.Fatalf("tail user message should hold one placeholder result for c1: %+v", last.Content)
	}
}

// TestEnsureToolUsePairedMultipleOrphans：assistant 含多个连续 orphan tool_use，
// 每个都应补到后续 user message 中。
func TestEnsureToolUsePairedMultipleOrphans(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"b","arguments":"{}"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2"} {
		if findToolResult(out.Messages, id) == nil {
			t.Errorf("expected placeholder result for orphan %s", id)
		}
	}
}

// TestEnsureToolUsePairedNormalUntouched：正常配对的 tool_use 不应被补占位，
// 也不应触发 WARN；其 result 是客户端真实 output（非 is_error）。
func TestEnsureToolUsePairedNormalUntouched(t *testing.T) {
	buf, restore := captureWarnLogger(t)
	defer restore()

	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"function_call","call_id":"c1","name":"tool","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "补占位") {
		t.Errorf("should not placehold for a paired tool_use: %s", buf.String())
	}
	tr := findToolResult(out.Messages, "c1")
	if tr == nil {
		t.Fatalf("real tool_result for c1 missing")
	}
	if tr.IsError.Valid() && tr.IsError.Value {
		t.Errorf("paired result should not be is_error: %+v", tr.IsError)
	}
	if len(tr.Content) == 0 || tr.Content[0].OfText == nil || tr.Content[0].OfText.Text != "ok" {
		t.Errorf("paired result text should be the real output: %+v", tr.Content)
	}
}

// TestEnsureToolUsePairedServerToolUnaffected：code_interpreter_call（server_tool_use）
// 在 item 内自合成 server_tool_use + code_execution_tool_result，ensureToolUsePaired
// 只处理普通 OfToolUse，不应误动 server tool 结构。
func TestEnsureToolUsePairedServerToolUnaffected(t *testing.T) {
	buf, restore := captureWarnLogger(t)
	defer restore()

	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"code_interpreter_call","id":"ci_1","code":"print(1)","outputs":[]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "补占位") {
		t.Errorf("server_tool_use must not be placeheld: %s", buf.String())
	}
	var hasServerToolUse, hasCodeExecResult bool
	for i := range out.Messages {
		for _, b := range out.Messages[i].Content {
			if b.OfServerToolUse != nil {
				hasServerToolUse = true
			}
			if b.OfCodeExecutionToolResult != nil {
				hasCodeExecResult = true
			}
		}
	}
	if !hasServerToolUse || !hasCodeExecResult {
		t.Errorf("server_tool_use + code_execution_tool_result pair missing: %+v", out.Messages)
	}
}

// TestCoalesceUserAdjacentBeforeToolResult 复现日志中的 400 场景：
// Codex 回灌的 input 顺序是「user message → function_call → user message → function_call_output」，
// 转换后本会得到 user / assistant(tool_use) / user(text) / user(tool_result) 的排列，
// Grok 兼容后端严格要求 assistant(tool_use) 之后立刻是 user(tool_result)，
// 因此触发 "messages[3] 必须是包含 tool_result 的 user 消息" 400。
// 合并策略把连续 user 合成一条既含 text 又含 tool_result 的 user message，通过。
func TestCoalesceUserAdjacentBeforeToolResult(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]},
		{"type":"function_call","call_id":"c1","name":"tool","arguments":"{}"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"more context"}]},
		{"type":"function_call_output","call_id":"c1","output":"ok"}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// 应当交替：user / assistant(tool_use) / user(text+tool_result)。
	if len(out.Messages) != 3 {
		t.Fatalf("expected 3 alternating messages, got %d: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("msg[0] should be user, got %s", out.Messages[0].Role)
	}
	if out.Messages[1].Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("msg[1] should be assistant, got %s", out.Messages[1].Role)
	}
	last := out.Messages[2]
	if last.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("msg[2] should be user, got %s", last.Role)
	}
	hasText, hasResult := false, false
	for _, b := range last.Content {
		if b.OfText != nil {
			hasText = true
		}
		if b.OfToolResult != nil && b.OfToolResult.ToolUseID == "c1" {
			hasResult = true
		}
	}
	if !hasText || !hasResult {
		t.Fatalf("merged user should contain text+tool_result, got %+v", last.Content)
	}
}

// TestCoalesceAssistantAdjacent：连续两条 assistant（如 reasoning 之后紧跟 assistant text）
// 应合并为单条 assistant，保留原 block 顺序。
func TestCoalesceAssistantAdjacent(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"r1","encrypted_content":"sig","summary":[{"type":"summary_text","text":"think"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}
	],"stream":true}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	assistantCount := 0
	for _, m := range out.Messages {
		if m.Role == anthropic.MessageParamRoleAssistant {
			assistantCount++
		}
	}
	if assistantCount != 1 {
		t.Fatalf("expected a single merged assistant message, got %d: %+v", assistantCount, out.Messages)
	}
}
