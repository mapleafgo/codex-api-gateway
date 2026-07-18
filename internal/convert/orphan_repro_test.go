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
