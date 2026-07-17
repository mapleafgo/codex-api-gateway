package toolcatalog

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestServerToolByAnthropicName(t *testing.T) {
	id, ok := ServerToolByAnthropicName("web_search")
	if !ok || id.Name != "web_search" {
		t.Fatalf("web_search must be registered: %+v ok=%v", id, ok)
	}
	id, ok = ServerToolByAnthropicName("code_execution")
	if !ok || id.OpenAIType != "code_interpreter" {
		t.Fatalf("code_execution must be registered: %+v ok=%v", id, ok)
	}
}

func TestApplyCacheControlRecognizedVariants(t *testing.T) {
	cc := anthropic.CacheControlEphemeralParam{TTL: anthropic.CacheControlEphemeralTTLTTL5m}

	tool := anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: "f"}}
	if !ApplyCacheControl(&tool, cc) || tool.OfTool.CacheControl.TTL != cc.TTL {
		t.Fatalf("OfTool cache_control not applied")
	}

	ws := anthropic.ToolUnionParam{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{}}
	if !ApplyCacheControl(&ws, cc) {
		t.Fatalf("OfWebSearchTool20250305 cache_control not applied")
	}

	ce := anthropic.ToolUnionParam{OfCodeExecutionTool20250522: &anthropic.CodeExecutionTool20250522Param{}}
	if !ApplyCacheControl(&ce, cc) {
		t.Fatalf("OfCodeExecutionTool20250522 cache_control not applied")
	}
}

func TestApplyCacheControlUnknownReturnsFalse(t *testing.T) {
	var empty anthropic.ToolUnionParam // 所有变体 nil
	if ApplyCacheControl(&empty, anthropic.CacheControlEphemeralParam{}) {
		t.Fatal("unknown variant must return false")
	}
}
