package convert

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// TestCustomToolNotDropped 复现：Codex 的 apply_patch 是 freeform custom tool
// （type=custom，grammar 格式），网关 convertTools 只认 OfFunction 会把它丢掉。
func TestCustomToolNotDropped(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true,"tools":[
		{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}},
		{"type":"custom","name":"apply_patch","description":"edit files","format":{"type":"grammar","syntax":"lark","definition":"start: x"}}
	]}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 {
		b, _ := json.Marshal(out.Tools)
		t.Fatalf("expected both tools converted, got %d: %s", len(out.Tools), b)
	}
	// custom 工具名必须保留为 function 名，模型才能按名字调用
	names := map[string]bool{}
	for _, tl := range out.Tools {
		if tl.OfTool != nil {
			names[tl.OfTool.Name] = true
		}
	}
	if !names["apply_patch"] {
		t.Fatalf("apply_patch tool lost, names=%v", names)
	}
	if tool := findTool(out.Tools, "apply_patch"); tool == nil || tool.Type != anthropic.ToolTypeCustom {
		t.Fatalf("custom tool should be converted as Anthropic custom tool: %+v", tool)
	}
}

func TestApplyPatchToolNotDropped(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true,"tools":[
		{"type":"apply_patch"}
	]}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tool := findTool(out.Tools, "apply_patch")
	if tool == nil {
		t.Fatalf("apply_patch tool lost: %+v", out.Tools)
	}
	if tool.Type != anthropic.ToolTypeCustom {
		t.Fatalf("apply_patch should be exposed as Anthropic custom tool, got %q", tool.Type)
	}
}

func TestShellToolNotDroppedAndCanBeForced(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true,
		"tools":[{"type":"shell"}],
		"tool_choice":{"type":"shell"}
	}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tool := findTool(out.Tools, "shell")
	if tool == nil {
		t.Fatalf("shell tool lost: %+v", out.Tools)
	}
	if tool.Type != anthropic.ToolTypeCustom {
		t.Fatalf("shell should be exposed as Anthropic custom tool, got %q", tool.Type)
	}
	if out.ToolChoice.OfTool == nil || out.ToolChoice.OfTool.Name != "shell" {
		t.Fatalf("shell tool_choice not preserved: %+v", out.ToolChoice)
	}
}

func TestLocalShellToolNotDropped(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true,"tools":[
		{"type":"local_shell"}
	]}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tool := findTool(out.Tools, "shell")
	if tool == nil {
		t.Fatalf("local_shell tool lost: %+v", out.Tools)
	}
	if tool.Type != anthropic.ToolTypeCustom {
		t.Fatalf("local_shell should be exposed as Anthropic custom shell tool, got %q", tool.Type)
	}
}

func TestApplyPatchToolChoiceCanBeForced(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true,
		"tools":[{"type":"apply_patch"}],
		"tool_choice":{"type":"apply_patch"}
	}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolChoice.OfTool == nil || out.ToolChoice.OfTool.Name != "apply_patch" {
		t.Fatalf("apply_patch tool_choice not preserved: %+v", out.ToolChoice)
	}
}

func TestNamespaceFunctionAndCustomToolsNotDropped(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true,"tools":[
		{"type":"namespace","name":"crm","description":"CRM tools","tools":[
			{"type":"function","name":"lookup","description":"lookup contact","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},
			{"type":"custom","name":"raw","description":"raw command"}
		]}
	]}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	lookup := findTool(out.Tools, "crm__lookup")
	if lookup == nil {
		t.Fatalf("namespace function tool lost: %+v", out.Tools)
	}
	props, ok := lookup.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatalf("namespace function schema properties have unexpected type: %+v", lookup.InputSchema.Properties)
	}
	if _, ok := props["id"]; !ok {
		t.Fatalf("namespace function schema not preserved: %+v", lookup.InputSchema)
	}
	raw := findTool(out.Tools, "crm__raw")
	if raw == nil {
		t.Fatalf("namespace custom tool lost: %+v", out.Tools)
	}
	if raw.Type != anthropic.ToolTypeCustom {
		t.Fatalf("namespace custom should be Anthropic custom tool, got %q", raw.Type)
	}
}

// TestFreeformToolNames 提取应作为 freeform 处理的 custom 工具名。
func TestFreeformToolNames(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true,"tools":[
		{"type":"function","name":"shell","parameters":{"type":"object"}},
		{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"x"}},
		{"type":"custom","name":"my_freeform"},
		{"type":"apply_patch"},
		{"type":"shell"},
		{"type":"namespace","name":"crm","description":"CRM tools","tools":[
			{"type":"custom","name":"raw"}
		]}
	]}`)
	got := FreeformToolNames(req)
	want := []string{"apply_patch", "my_freeform", "shell", "crm__raw"}
	for _, name := range want {
		if !contains(got, name) {
			t.Fatalf("missing %q in %v", name, got)
		}
	}
}

func TestFreeformToolNamesFromAdditionalToolsAndToolSearchOutput(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"adhoc"}]},
		{"type":"tool_search_output","tools":[{"type":"custom","name":"loaded_raw"}]}
	],"stream":true}`)
	got := FreeformToolNames(req)
	for _, name := range []string{"adhoc", "loaded_raw"} {
		if !contains(got, name) {
			t.Fatalf("missing %q in %v", name, got)
		}
	}
}

func contains(s []string, t string) bool {
	for _, v := range s {
		if v == t {
			return true
		}
	}
	return false
}

func findTool(tools []anthropic.ToolUnionParam, name string) *anthropic.ToolParam {
	for _, tool := range tools {
		if tool.OfTool != nil && tool.OfTool.Name == name {
			return tool.OfTool
		}
	}
	return nil
}
