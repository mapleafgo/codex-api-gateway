package toolcatalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

func TestDeclareFunction(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfFunction: &oairesponses.FunctionToolParam{
		Name: "f", Parameters: map[string]any{"type": "object"}, Description: oparam.NewOpt("d"),
	}})
	if err != nil {
		t.Fatalf("Declare error: %v", err)
	}
	if len(decls) != 1 || decls[0].OfTool == nil || decls[0].OfTool.Name != "f" {
		t.Fatalf("expected single ToolParam 'f', got %+v", decls)
	}
}

func TestDeclareCustomIsFreeform(t *testing.T) {
	decls, _ := Declare(oairesponses.ToolUnionParam{OfCustom: &oairesponses.CustomToolParam{Name: "c"}})
	if decls[0].OfTool.Type != anthropic.ToolTypeCustom {
		t.Fatalf("custom tool must set ToolTypeCustom")
	}
}

func TestDeclareNamespacePrefixesNames(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfNamespace: &oairesponses.NamespaceToolParam{
		Name:  "ns",
		Tools: []oairesponses.NamespaceToolToolUnionParam{{OfFunction: &oairesponses.NamespaceToolToolFunctionParam{Name: "f"}}},
	}})
	if err != nil {
		t.Fatalf("Declare error: %v", err)
	}
	if len(decls) != 1 || decls[0].OfTool.Name != "ns__f" {
		t.Fatalf("namespace name not prefixed: %+v", decls)
	}
}

func TestDeclareWebSearchMapsAllowedDomains(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfWebSearch: &oairesponses.WebSearchToolParam{
		Filters: oairesponses.WebSearchToolFiltersParam{AllowedDomains: []string{"a.com"}},
	}})
	if err != nil {
		t.Fatalf("Declare error: %v", err)
	}
	if decls[0].OfWebSearchTool20250305 == nil || len(decls[0].OfWebSearchTool20250305.AllowedDomains) != 1 {
		t.Fatalf("web_search not mapped: %+v", decls)
	}
}

func TestDeclareWebSearchPreviewNoDomains(t *testing.T) {
	decls, _ := Declare(oairesponses.ToolUnionParam{OfWebSearchPreview: &oairesponses.WebSearchPreviewToolParam{}})
	if decls[0].OfWebSearchTool20250305 == nil || len(decls[0].OfWebSearchTool20250305.AllowedDomains) != 0 {
		t.Fatalf("web_search_preview must map to empty-domain server tool")
	}
}

func TestDeclareCodeInterpreterMapsToCodeExecution(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfCodeInterpreter: &oairesponses.ToolCodeInterpreterParam{}})
	if err != nil {
		t.Fatalf("code_interpreter must not fail fast: %v", err)
	}
	if decls[0].OfCodeExecutionTool20250522 == nil {
		t.Fatalf("code_interpreter not mapped to code_execution: %+v", decls)
	}
}

func TestDeclareUnsupportedErrors(t *testing.T) {
	if _, err := Declare(oairesponses.ToolUnionParam{}); err == nil {
		t.Fatal("expected error for unsupported tool")
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
