package toolcatalog

import (
	"testing"

	oairesponses "github.com/openai/openai-go/v3/responses"
)

func TestInspectClientTools(t *testing.T) {
	tests := []struct {
		name string
		tool oairesponses.ToolUnionParam
		want Identity
	}{
		{"function", oairesponses.ToolUnionParam{OfFunction: &oairesponses.FunctionToolParam{Name: "f"}}, Identity{OpenAIType: "function", Name: "f"}},
		{"custom", oairesponses.ToolUnionParam{OfCustom: &oairesponses.CustomToolParam{Name: "c"}}, Identity{OpenAIType: "custom", Name: "c", Freeform: true}},
		{"apply_patch", oairesponses.ToolUnionParam{OfApplyPatch: &oairesponses.ApplyPatchToolParam{}}, Identity{OpenAIType: "apply_patch", Name: "apply_patch", Freeform: true}},
		{"shell", oairesponses.ToolUnionParam{OfShell: &oairesponses.FunctionShellToolParam{}}, Identity{OpenAIType: "shell", Name: "shell", Freeform: true}},
		{"local_shell", oairesponses.ToolUnionParam{OfLocalShell: &oairesponses.ToolLocalShellParam{}}, Identity{OpenAIType: "local_shell", Name: "shell", Freeform: true}},
		{"tool_search", oairesponses.ToolUnionParam{OfToolSearch: &oairesponses.ToolSearchToolParam{}}, Identity{OpenAIType: "tool_search", Name: "tool_search"}},
		{"web_search", oairesponses.ToolUnionParam{OfWebSearch: &oairesponses.WebSearchToolParam{}}, Identity{OpenAIType: "web_search", Name: "web_search"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ids, err := Inspect(tc.tool)
			if err != nil {
				t.Fatalf("Inspect error: %v", err)
			}
			if len(ids) != 1 || !ids[0].Equal(tc.want) {
				t.Fatalf("got %+v, want %+v", ids, tc.want)
			}
		})
	}
}

func TestInspectNamespaceExpands(t *testing.T) {
	tool := oairesponses.ToolUnionParam{OfNamespace: &oairesponses.NamespaceToolParam{
		Name: "ns",
		Tools: []oairesponses.NamespaceToolToolUnionParam{
			{OfFunction: &oairesponses.NamespaceToolToolFunctionParam{Name: "f"}},
			{OfCustom: &oairesponses.CustomToolParam{Name: "c"}},
		},
	}}
	ids, err := Inspect(tool)
	if err != nil {
		t.Fatalf("Inspect error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 identities, got %d", len(ids))
	}
	if ids[0].ConvertedName() != "ns__f" || ids[1].ConvertedName() != "ns__c" {
		t.Fatalf("namespace names wrong: %+v", ids)
	}
	if !ids[1].Freeform {
		t.Fatalf("namespace custom must be freeform")
	}
}

func TestInspectUnsupportedErrors(t *testing.T) {
	_, err := Inspect(oairesponses.ToolUnionParam{})
	if err == nil {
		t.Fatal("expected error for unsupported tool")
	}
}

func TestInspectNamespaceUnsupportedChildErrors(t *testing.T) {
	tool := oairesponses.ToolUnionParam{OfNamespace: &oairesponses.NamespaceToolParam{
		Name:  "ns",
		Tools: []oairesponses.NamespaceToolToolUnionParam{{}}, // 空 child
	}}
	if _, err := Inspect(tool); err == nil {
		t.Fatal("expected error for unsupported namespace child")
	}
}

func TestInspectAllowedTypeStrings(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
		want Identity
	}{
		{"shell", map[string]any{"type": "shell"}, Identity{OpenAIType: "shell", Name: "shell", Freeform: true}},
		{"local_shell", map[string]any{"type": "local_shell"}, Identity{OpenAIType: "local_shell", Name: "shell", Freeform: true}},
		{"apply_patch", map[string]any{"type": "apply_patch"}, Identity{OpenAIType: "apply_patch", Name: "apply_patch", Freeform: true}},
		{"tool_search", map[string]any{"type": "tool_search"}, Identity{OpenAIType: "tool_search", Name: "tool_search"}},
		{"function", map[string]any{"type": "function", "name": "f"}, Identity{OpenAIType: "function", Name: "f"}},
		{"custom", map[string]any{"type": "custom", "name": "c"}, Identity{OpenAIType: "custom", Name: "c", Freeform: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ids, err := InspectAllowed(tc.raw)
			if err != nil {
				t.Fatalf("InspectAllowed error: %v", err)
			}
			if len(ids) != 1 || !ids[0].Equal(tc.want) {
				t.Fatalf("got %+v, want %+v", ids, tc.want)
			}
		})
	}
}

func TestInspectAllowedNamespace(t *testing.T) {
	raw := map[string]any{
		"type": "namespace", "name": "ns",
		"tools": []any{
			map[string]any{"type": "function", "name": "f"},
			map[string]any{"type": "custom", "name": "c"},
		},
	}
	ids, err := InspectAllowed(raw)
	if err != nil {
		t.Fatalf("InspectAllowed error: %v", err)
	}
	if len(ids) != 2 || ids[0].ConvertedName() != "ns__f" || ids[1].ConvertedName() != "ns__c" {
		t.Fatalf("namespace identities wrong: %+v", ids)
	}
}

func TestInspectAllowedErrors(t *testing.T) {
	if _, err := InspectAllowed(map[string]any{}); err == nil {
		t.Fatal("expected error for missing type")
	}
	if _, err := InspectAllowed(map[string]any{"type": "function"}); err == nil {
		t.Fatal("expected error for function without name")
	}
	if _, err := InspectAllowed(map[string]any{"type": "unknown"}); err == nil {
		t.Fatal("expected error for unsupported type")
	}
}
