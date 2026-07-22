package chatconvert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/convert"
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
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" || out.Messages[0].Content != "Hello world" {
		t.Fatalf("messages=%+v", out.Messages)
	}
}

func TestToChat_InstructionsAndMessageList(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"instructions":"You are helpful",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hi"}]}]
	}`
	out := mustChat(t, body, "upstream-model")
	if out.Model != "upstream-model" {
		t.Fatalf("model=%q", out.Model)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages len=%d %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "You are helpful" {
		t.Fatalf("system=%+v", out.Messages[0])
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content != "Hi" {
		t.Fatalf("user=%+v", out.Messages[1])
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
		t.Fatalf("messages len=%d %+v", len(out.Messages), out.Messages)
	}
	asst := out.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant=%+v", asst)
	}
	fc := asst.ToolCalls[0]
	if fc.ID != "call_1" || fc.Function.Name != "get_weather" || fc.Function.Arguments != `{"city":"London"}` {
		t.Fatalf("tool_call=%+v", fc)
	}
	tool := out.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || tool.Content != "18 C" {
		t.Fatalf("tool=%+v", tool)
	}
}

func TestToChat_FunctionToolDecl(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":"hi",
		"tools":[{"type":"function","name":"get_weather","description":"weather","parameters":{"type":"object"}}]
	}`
	out := mustChat(t, body, "gpt-4o")
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools=%+v", out.Tools)
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
}

func TestChatToolArgumentsJSON(t *testing.T) {
	got, ok := chatToolArgumentsJSON(`{"a":1}
</x>`)
	if !ok || got != `{"a":1}` {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if _, ok := chatToolArgumentsJSON(`not`); ok {
		t.Fatal("want fail")
	}
	got, ok = chatToolArgumentsJSON("")
	if !ok || got != "{}" {
		t.Fatalf("empty %q", got)
	}
}

func TestChatCustomInputAsArguments(t *testing.T) {
	got, ok := chatCustomInputAsArguments(`{"x":1}`)
	if !ok || got != `{"x":1}` {
		t.Fatalf("object %q", got)
	}
	got, ok = chatCustomInputAsArguments(`hello`)
	if !ok || !strings.Contains(got, "hello") {
		t.Fatalf("wrap %q", got)
	}
}
