package toolcatalog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
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
	if decls[0].OfTool.Type != "" {
		t.Fatalf("client tool 应省略 type 字段，got %q", decls[0].OfTool.Type)
	}
}

func TestDeclareMcpExpandsAllowedTools(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfMcp: &oairesponses.ToolMcpParam{
		ServerLabel: "weather",
		AllowedTools: oairesponses.ToolMcpAllowedToolsUnionParam{
			OfMcpAllowedTools: []string{"get", "list"},
		},
	}})
	if err != nil {
		t.Fatalf("Declare mcp: %v", err)
	}
	if len(decls) != 2 {
		t.Fatalf("expected 2 tool declarations, got %+v", decls)
	}
	if decls[0].OfTool == nil || decls[0].OfTool.Name != "mcp__weather__get" {
		t.Fatalf("bad first declaration: %+v", decls[0])
	}
	if decls[1].OfTool == nil || decls[1].OfTool.Name != "mcp__weather__list" {
		t.Fatalf("bad second declaration: %+v", decls[1])
	}
}

func TestDeclareMcpCarriesServerDescription(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfMcp: &oairesponses.ToolMcpParam{
		ServerLabel:       "browser",
		ServerDescription: oparam.NewOpt("Browser automation MCP server"),
		AllowedTools: oairesponses.ToolMcpAllowedToolsUnionParam{
			OfMcpAllowedTools: []string{"click"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 1 || decls[0].OfTool == nil {
		t.Fatalf("decls=%+v", decls)
	}
	if !decls[0].OfTool.Description.Valid() || decls[0].OfTool.Description.Value != "Browser automation MCP server" {
		t.Fatalf("mcp server_description not carried: %+v", decls[0].OfTool.Description)
	}
}

func TestDeclareMcpFilterDeclaresNothing(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfMcp: &oairesponses.ToolMcpParam{
		ServerLabel: "weather",
		AllowedTools: oairesponses.ToolMcpAllowedToolsUnionParam{
			OfMcpToolFilter: &oairesponses.ToolMcpAllowedToolsMcpToolFilterParam{ReadOnly: oparam.NewOpt(true)},
		},
	}})
	if err != nil {
		t.Fatalf("Declare mcp filter: %v", err)
	}
	if len(decls) != 0 {
		t.Fatalf("filter variant must not expand, got %+v", decls)
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
	if decls[0].OfWebSearchTool20260209 == nil || len(decls[0].OfWebSearchTool20260209.AllowedDomains) != 1 {
		t.Fatalf("web_search not mapped: %+v", decls)
	}
}

func TestDeclareCodeInterpreterUnsupported(t *testing.T) {
	if _, err := Declare(oairesponses.ToolUnionParam{OfCodeInterpreter: &oairesponses.ToolCodeInterpreterParam{}}); err == nil {
		t.Fatal("code_interpreter must fail fast (gateway 不支持)")
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

func TestDeclareFunctionPreservesPropertyDescription(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfFunction: &oairesponses.FunctionToolParam{
		Name:        "get_stock_price",
		Description: oparam.NewOpt("Get the current stock price for a given ticker symbol."),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ticker": map[string]any{
					"type":        "string",
					"description": "The stock ticker symbol, e.g. AAPL for Apple Inc.",
				},
			},
			"required": []string{"ticker"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	props, ok := decls[0].OfTool.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatalf("properties=%T", decls[0].OfTool.InputSchema.Properties)
	}
	ticker, ok := props["ticker"].(map[string]any)
	if !ok {
		t.Fatalf("ticker=%#v", props["ticker"])
	}
	if ticker["description"] != "The stock ticker symbol, e.g. AAPL for Apple Inc." {
		t.Fatalf("property description lost: %#v", ticker)
	}
}

func TestDeclareToolSearchPreservesPropertyDescription(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfToolSearch: &oairesponses.ToolSearchToolParam{
		Description: oparam.NewOpt("search deferred tools"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"q": map[string]any{
					"type":        "string",
					"description": "the search query",
				},
			},
			"required": []string{"q"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	props, ok := decls[0].OfTool.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatalf("properties=%T", decls[0].OfTool.InputSchema.Properties)
	}
	q, ok := props["q"].(map[string]any)
	if !ok {
		t.Fatalf("q=%#v", props["q"])
	}
	if q["description"] != "the search query" {
		t.Fatalf("tool_search property description lost: %#v", q)
	}
}

func TestDeclareNamespaceFunctionPreservesPropertyDescription(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfNamespace: &oairesponses.NamespaceToolParam{
		Name: "ns",
		Tools: []oairesponses.NamespaceToolToolUnionParam{{
			OfFunction: &oairesponses.NamespaceToolToolFunctionParam{
				Name: "lookup",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{
							"type":        "string",
							"description": "contact id",
						},
					},
					"required": []string{"id"},
				},
			},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	props, ok := decls[0].OfTool.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatalf("properties=%T", decls[0].OfTool.InputSchema.Properties)
	}
	id, ok := props["id"].(map[string]any)
	if !ok {
		t.Fatalf("id=%#v", props["id"])
	}
	if id["description"] != "contact id" {
		t.Fatalf("namespace property description lost: %#v", id)
	}
}

func TestDeclareCustomInputDescription(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfCustom: &oairesponses.CustomToolParam{
		Name:        "parse",
		Description: oparam.NewOpt("parse csv"),
		Format: shared.CustomToolInputFormatUnionParam{OfGrammar: &shared.CustomToolInputFormatGrammarParam{
			Definition: "start: /[0-9]+/",
			Syntax:     "lark",
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	props, ok := decls[0].OfTool.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatalf("properties=%T", decls[0].OfTool.InputSchema.Properties)
	}
	input, ok := props["s"].(map[string]any)
	if !ok {
		t.Fatalf("input=%#v", props["s"])
	}
	if input["description"] != "Required format:\nstart: /[0-9]+/" {
		t.Fatalf("freeform input description not carried: %#v", input)
	}
}

func TestDeclareWebSearchMapsUserLocation(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfWebSearch: &oairesponses.WebSearchToolParam{
		Filters: oairesponses.WebSearchToolFiltersParam{AllowedDomains: []string{"example.com"}},
		UserLocation: oairesponses.WebSearchToolUserLocationParam{
			City:     oparam.NewOpt("Shanghai"),
			Country:  oparam.NewOpt("CN"),
			Region:   oparam.NewOpt("Shanghai"),
			Timezone: oparam.NewOpt("Asia/Shanghai"),
		},
	}})
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	ws := decls[0].OfWebSearchTool20260209
	if ws == nil {
		t.Fatal("expected WebSearchTool20260209")
	}
	if !ws.UserLocation.City.Valid() || ws.UserLocation.City.Value != "Shanghai" {
		t.Fatalf("user_location.city not mapped: %+v", ws.UserLocation)
	}
	if !ws.UserLocation.Country.Valid() || ws.UserLocation.Country.Value != "CN" {
		t.Fatalf("user_location.country not mapped: %+v", ws.UserLocation)
	}
	if !ws.UserLocation.Region.Valid() || ws.UserLocation.Region.Value != "Shanghai" {
		t.Fatalf("user_location.region not mapped: %+v", ws.UserLocation)
	}
	if !ws.UserLocation.Timezone.Valid() || ws.UserLocation.Timezone.Value != "Asia/Shanghai" {
		t.Fatalf("user_location.timezone not mapped: %+v", ws.UserLocation)
	}
	if len(ws.AllowedDomains) != 1 || ws.AllowedDomains[0] != "example.com" {
		t.Fatalf("allowed_domains regression: %+v", ws.AllowedDomains)
	}
}

func TestDeclareWebSearchSearchContextSizeDoesNotPanic(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })

	decls, err := Declare(oairesponses.ToolUnionParam{OfWebSearch: &oairesponses.WebSearchToolParam{
		SearchContextSize: oairesponses.WebSearchToolSearchContextSizeHigh,
	}})
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if decls[0].OfWebSearchTool20260209 == nil {
		t.Fatal("expected web search tool")
	}
	got := logs.String()
	if !strings.Contains(got, "search_context_size") || !strings.Contains(got, "high") {
		t.Fatalf("expected WARN for search_context_size, logs: %s", got)
	}
}
