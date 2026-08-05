package convert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// TestReasoningContentFallbackWhenSummaryEmpty：content[].reasoning_text 有全文、
// summary 为空时，不得误判为 redacted_thinking 而丢掉思考正文。
func TestReasoningContentFallbackWhenSummaryEmpty(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"FULL_THINK"}],"encrypted_content":"sigX"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ans"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}
	],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var foundThinking, foundRedacted bool
	for _, msg := range out.Messages {
		for _, b := range msg.Content {
			if b.OfThinking != nil {
				foundThinking = true
				if b.OfThinking.Thinking != "FULL_THINK" || b.OfThinking.Signature != "sigX" {
					t.Fatalf("thinking not from content: %+v", b.OfThinking)
				}
			}
			if b.OfRedactedThinking != nil {
				foundRedacted = true
			}
		}
	}
	if !foundThinking {
		b, _ := json.Marshal(out.Messages)
		t.Fatalf("want plaintext thinking from content[], got: %s", b)
	}
	if foundRedacted {
		t.Fatal("must not attach redacted_thinking when content has reasoning_text")
	}
}

// TestWebSearchHistoryResultEmptyWithoutEncrypted：无 Anthropic encrypted_content 时
// 只回放 server_tool_use + 空 result 数组，避免官方 API 因 required encrypted 为空而 400。
func TestWebSearchHistoryResultEmptyWithoutEncrypted(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"go","sources":[{"type":"url","url":"https://go.dev/"}]}},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"u"}]}
	],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var useID string
	var result *anthropic.WebSearchToolResultBlockParam
	var resultRole anthropic.MessageParamRole
	for _, msg := range out.Messages {
		for _, b := range msg.Content {
			if b.OfServerToolUse != nil && b.OfServerToolUse.Name == anthropic.ServerToolUseBlockParamNameWebSearch {
				useID = b.OfServerToolUse.ID
			}
			if b.OfWebSearchToolResult != nil {
				result = b.OfWebSearchToolResult
				resultRole = msg.Role
			}
		}
	}
	if useID != "ws_1" {
		t.Fatalf("server_tool_use id=%q", useID)
	}
	if result == nil {
		t.Fatal("missing web_search_tool_result")
	}
	if resultRole != anthropic.MessageParamRoleAssistant {
		t.Fatalf("web_search_tool_result role=%q, want assistant", resultRole)
	}
	if result.ToolUseID != "ws_1" {
		t.Fatalf("tool_use_id=%q", result.ToolUseID)
	}
	// 无 encrypted 时 content 必须是空数组（不是带空 encrypted 的伪 result）。
	if n := len(result.Content.OfWebSearchToolResultBlockItem); n != 0 {
		t.Fatalf("want empty result items without encrypted, got %d: %+v", n, result.Content.OfWebSearchToolResultBlockItem)
	}
}

// TestUnsupportedHistoryItemsDroppedWithWarn：file_search/computer/image_generation/program/item_reference
// 不得再 raw dump 进 system，应 WARN 后丢弃。
func TestUnsupportedHistoryItemsDroppedWithWarn(t *testing.T) {
	cases := []struct {
		name string
		item string
		typ  string
	}{
		{"file_search", `{"type":"file_search_call","id":"fs1","queries":["x"],"status":"completed"}`, "file_search_call"},
		{"computer", `{"type":"computer_call","id":"cu1","call_id":"c1","status":"completed","actions":[{"type":"screenshot"}]}`, "computer_call"},
		{"computer_out", `{"type":"computer_call_output","call_id":"c1","output":{"type":"computer_screenshot","image_url":"https://x/s.png"}}`, "computer_call_output"},
		{"image_gen", `{"type":"image_generation_call","id":"ig1","status":"completed","result":"b64"}`, "image_generation_call"},
		{"program", `{"type":"program","id":"p1"}`, "program"},
		{"item_ref", `{"type":"item_reference","id":"msg_1"}`, "item_reference"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := captureWarnLogger(t)
			defer restore()
			payload := `{"model":"gpt-5","input":[` + tc.item + `,{"type":"message","role":"user","content":[{"type":"input_text","text":"u"}]}],"stream":true}`
			req := mustReq(t, payload)
			out, err := ToAnthropic(req, &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range out.System {
				if strings.Contains(s.Text, "openai_input_item") {
					t.Fatalf("%s must not raw-dump into system: %s", tc.typ, s.Text)
				}
			}
			logs := buf.String()
			if !strings.Contains(logs, tc.typ) {
				t.Fatalf("expected WARN containing %s, got: %s", tc.typ, logs)
			}
		})
	}
}

// TestCodeInterpreterHistoryDropped：code_interpreter_call 整体丢弃（含 image/logs），
// 不再回灌 code_execution block。
func TestCodeInterpreterHistoryDropped(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"code_interpreter_call","id":"ci1","status":"completed","code":"plot()","outputs":[{"type":"image","url":"https://x/y.png"},{"type":"logs","logs":"done"}]}
	],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out.Messages)
	if strings.Contains(string(raw), "code_execution") {
		t.Fatalf("code_interpreter_call history must be dropped: %s", raw)
	}
}

// TestMcpCallHistoryReplay：历史 mcp_call 按扁平名直接回填标准 tool_use + tool_result。
func TestMcpCallHistoryReplay(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"weather"}]},
		{"type":"mcp_call","id":"mcp_1","server_label":"weather","name":"get","arguments":"{\"q\":\"sf\"}","output":"sunny","status":"completed"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"sunny"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}
	],"tools":[{"type":"mcp","server_label":"weather","server_url":"https://mcp.example/sse"}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out.Messages)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "mcp__weather__get") || !strings.Contains(s, "mcp_1") {
		t.Fatalf("flat tool_use name missing from marshaled messages: %s", s)
	}
	if !strings.Contains(s, "tool_result") {
		t.Fatalf("tool_result missing from marshaled messages: %s", s)
	}
	if strings.Contains(s, "mcp_tool_use") || strings.Contains(s, "mcp_tool_result") {
		t.Fatalf("beta mcp blocks must not be emitted: %s", s)
	}
	if !strings.Contains(s, "sunny") {
		t.Fatalf("mcp result output lost: %s", s)
	}
}

// TestMcpCallHistoryMissingOutputGetsPlaceholder：mcp_call 缺 output 时不得
// 静默补空 tool_result，应交由 ensureToolUsePaired 补 is_error 占位 + WARN。
func TestMcpCallHistoryMissingOutputGetsPlaceholder(t *testing.T) {
	buf, restore := captureWarnLogger(t)
	defer restore()
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"weather"}]},
		{"type":"mcp_call","id":"mcp_1","server_label":"weather","name":"get","arguments":"{}","status":"completed"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}
	],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tr := findToolResult(out.Messages, "mcp_1")
	if tr == nil {
		t.Fatalf("want placeholder tool_result for mcp_1, messages=%+v", out.Messages)
	}
	if !tr.IsError.Valid() || !tr.IsError.Value {
		t.Fatalf("placeholder must be is_error: %+v", tr)
	}
	if len(tr.Content) == 0 || tr.Content[0].OfText == nil ||
		!strings.Contains(tr.Content[0].OfText.Text, "no tool output") {
		t.Fatalf("placeholder content=%+v", tr.Content)
	}
	if !strings.Contains(buf.String(), "补占位 tool_result") {
		t.Fatalf("expected WARN for placeholder, got: %s", buf.String())
	}
}
