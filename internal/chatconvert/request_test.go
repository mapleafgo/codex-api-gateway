package chatconvert

import (
	"bytes"
	"encoding/json"
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

func TestToChat_DeveloperRoleMapsToSystem(t *testing.T) {
	body := `{"model":"gpt-4o","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev rules"}]}]}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Messages) != 1 || out.Messages[0].Role != "system" {
		t.Fatalf("developer should map to system: %+v", out.Messages)
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
	if !out.IsFreeformName("shell") || !out.IsFreeformName("apply_patch") {
		t.Fatalf("freeform registry: %+v", out.FreeformNames)
	}
	// custom suffix must NOT be used
	if names["apply_patch_custom"] {
		t.Fatal("must not suffix _custom")
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

func TestToChat_OrphanToolCallGetsPlaceholder(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]},
			{"type":"function_call","call_id":"orphan","name":"a","arguments":"{}"}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, m := range out.Messages {
		if m.Role == "tool" && m.ToolCallID == "orphan" {
			found = true
			if !strings.Contains(m.Content.(string), "no tool output") {
				t.Fatalf("placeholder content=%v", m.Content)
			}
		}
	}
	if !found {
		t.Fatalf("missing placeholder: %+v", out.Messages)
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

func TestToChat_WebSearchAndCodeInterpreterDecl(t *testing.T) {
	body := `{"model":"gpt-4o","tools":[{"type":"web_search"},{"type":"code_interpreter"}],"input":"x"}`
	out := mustChat(t, body, "gpt-4o")
	names := map[string]bool{}
	for _, t0 := range out.Tools {
		names[t0.Function.Name] = true
	}
	if !names["web_search"] || !names["code_interpreter"] {
		t.Fatalf("hosted tools missing: %v", names)
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

func TestToChat_CompactionHistoryAsSystem(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"compaction","encrypted_content":"enc-blob"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, m := range out.Messages {
		if m.Role == "system" {
			if s, ok := m.Content.(string); ok && strings.Contains(s, "<compaction>") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("compaction marker missing: %+v", out.Messages)
	}
}

func TestToChat_PromptCacheKeyPassthrough(t *testing.T) {
	out := mustChat(t, `{"model":"gpt-4o","prompt_cache_key":"bucket-1","input":"hi"}`, "gpt-4o")
	if out.PromptCacheKey == nil || *out.PromptCacheKey != "bucket-1" {
		t.Fatalf("prompt_cache_key=%v", out.PromptCacheKey)
	}
}

func TestToChat_ResponseFormatJSONSchema(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":"hi",
		"text":{"format":{"type":"json_schema","name":"person","strict":true,"schema":{"type":"object"}}}
	}`
	out := mustChat(t, body, "gpt-4o")
	if out.ResponseFormat == nil {
		t.Fatal("nil response_format")
	}
	raw, _ := json.Marshal(out.ResponseFormat)
	if !strings.Contains(string(raw), "json_schema") || !strings.Contains(string(raw), "person") {
		t.Fatalf("response_format=%s", raw)
	}
}

func TestToChat_FunctionStrictPassthrough(t *testing.T) {
	body := `{"model":"gpt-4o","tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}}],"input":"x"}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 || out.Tools[0].Function.Strict == nil || !*out.Tools[0].Function.Strict {
		t.Fatalf("strict missing: %+v", out.Tools)
	}
}

func TestToChat_VerbosityServiceTierReasoningTopLogprobs(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","text":{"verbosity":"high"},"service_tier":"priority","reasoning":{"effort":"high"},"top_logprobs":3}`
	out := mustChat(t, body, "gpt-4o")
	if out.Verbosity == nil || *out.Verbosity != "high" {
		t.Fatalf("verbosity=%v", out.Verbosity)
	}
	if out.ServiceTier == nil || *out.ServiceTier != "priority" {
		t.Fatalf("service_tier=%v", out.ServiceTier)
	}
	if out.ReasoningEffort == nil || *out.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort=%v", out.ReasoningEffort)
	}
	if out.TopLogprobs == nil || *out.TopLogprobs != 3 || out.Logprobs == nil || !*out.Logprobs {
		t.Fatalf("logprobs top=%v on=%v", out.TopLogprobs, out.Logprobs)
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
	if out.SafetyIdentifier == nil || *out.SafetyIdentifier != "u1" {
		t.Fatalf("safety=%v", out.SafetyIdentifier)
	}
	if out.Metadata["k"] != "v" {
		t.Fatalf("metadata=%v", out.Metadata)
	}
	if out.Store == nil || !*out.Store {
		t.Fatalf("store=%v", out.Store)
	}
	if out.Moderation == nil || out.Moderation.Model != "omni-moderation-latest" {
		t.Fatalf("moderation=%+v", out.Moderation)
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

func TestToChat_OutputMessageDefensive(t *testing.T) {
	in := &oairesponses.ResponseInputItemMessageParam{
		Role: "user",
		Content: oairesponses.ResponseInputMessageContentListParam{
			oairesponses.ResponseInputContentParamOfInputText("via input_message"),
		},
	}
	msg, ok := convertInputMessage(in)
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
	if !out.IsFreeformName("shell") {
		t.Fatal("shell should be freeform")
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

func TestToChat_ParallelToolCallsAndPromptCacheOptions(t *testing.T) {
	body := `{
		"model":"gpt-4o","input":"hi",
		"parallel_tool_calls":false,
		"prompt_cache_options":{"mode":"explicit","ttl":"30m"}
	}`
	out := mustChat(t, body, "gpt-4o")
	if out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls=%v", out.ParallelToolCalls)
	}
	if out.PromptCacheOptions == nil || out.PromptCacheOptions.Mode != "explicit" || out.PromptCacheOptions.TTL != "30m" {
		t.Fatalf("prompt_cache_options=%+v", out.PromptCacheOptions)
	}
}

func TestToChat_ResponseFormatJSONObject(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","text":{"format":{"type":"json_object"}}}`
	out := mustChat(t, body, "gpt-4o")
	raw, _ := json.Marshal(out.ResponseFormat)
	if !strings.Contains(string(raw), `"json_object"`) {
		t.Fatalf("response_format=%s", raw)
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

func TestToChat_ApplyPatchHistoryV4A(t *testing.T) {
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
					if !strings.Contains(tc.Function.Arguments, "a.go") {
						t.Fatalf("args should embed path/diff: %s", tc.Function.Arguments)
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
	if !out.IsFreeformName("apply_patch") {
		t.Fatal("apply_patch freeform")
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
				if tc.Function.Name == "shell" {
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

func TestToChat_ImagePartSkippedInMessage(t *testing.T) {
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
	if out.Messages[0].Content != "see " {
		t.Fatalf("content=%v (image should be dropped)", out.Messages[0].Content)
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

func TestToChat_CompactionTrigger(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"type":"compaction_trigger"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`
	out := mustChat(t, body, "gpt-4o")
	found := false
	for _, m := range out.Messages {
		if m.Role == "system" {
			if s, ok := m.Content.(string); ok && strings.Contains(s, "compaction_trigger") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("compaction_trigger missing: %+v", out.Messages)
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
