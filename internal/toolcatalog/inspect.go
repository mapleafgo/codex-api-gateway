package toolcatalog

import (
	"encoding/json"
	"fmt"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// Inspect 从一个 OpenAI ToolUnionParam 提取身份。
// namespace tool 展开为多个身份（每个子 tool 一个）；其余变体返回单个身份。
// 不支持的变体返回错误，调用方据此 fail-fast。
func Inspect(t oairesponses.ToolUnionParam) ([]Identity, error) {
	switch {
	case t.OfFunction != nil:
		return []Identity{{OpenAIType: model.ToolTypeFunction, Name: t.OfFunction.Name}}, nil
	case t.OfCustom != nil:
		return []Identity{{OpenAIType: model.ToolTypeCustom, Name: t.OfCustom.Name, Freeform: true}}, nil
	case t.OfApplyPatch != nil:
		return []Identity{{OpenAIType: model.ToolTypeApplyPatch, Name: "apply_patch", Freeform: true}}, nil
	case t.OfShell != nil:
		return []Identity{{OpenAIType: model.ToolTypeShell, Name: "shell", Freeform: true}}, nil
	case t.OfLocalShell != nil:
		return []Identity{{OpenAIType: model.ToolTypeLocalShell, Name: "shell", Freeform: true}}, nil
	case t.OfToolSearch != nil:
		return []Identity{{OpenAIType: model.ToolTypeToolSearch, Name: "tool_search"}}, nil
	case t.OfNamespace != nil:
		ns := t.OfNamespace
		out := make([]Identity, 0, len(ns.Tools))
		for _, nested := range ns.Tools {
			switch {
			case nested.OfFunction != nil:
				out = append(out, Identity{OpenAIType: model.ToolTypeFunction, Namespace: ns.Name, Name: nested.OfFunction.Name})
			case nested.OfCustom != nil:
				out = append(out, Identity{OpenAIType: model.ToolTypeCustom, Namespace: ns.Name, Name: nested.OfCustom.Name, Freeform: true})
			default:
				return nil, fmt.Errorf("unsupported namespace tool: Anthropic backend has no safe equivalent")
			}
		}
		return out, nil
	case t.OfCodeInterpreter != nil:
		return []Identity{{OpenAIType: model.ToolTypeCodeInterpreter, Name: "code_interpreter"}}, nil
	case t.OfMcp != nil:
		return []Identity{{OpenAIType: model.ToolTypeMcp, Name: "mcp"}}, nil
	case t.OfWebSearch != nil, t.OfWebSearchPreview != nil:
		return []Identity{{OpenAIType: model.ToolTypeWebSearch, Name: "web_search"}}, nil
	default:
		return nil, fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", openaiToolType(t))
	}
}

// InspectAllowed 从一个 allowed_tools 条目（弱类型 map）提取身份。
// 与 Inspect 覆盖同一组 tool 类型，但入口是 tool_choice.allowed_tools 的
// `{type, name?, tools?}` 结构，而非强类型 ToolUnionParam。
func InspectAllowed(tool map[string]any) ([]Identity, error) {
	typ, ok := tool["type"].(string)
	if !ok || typ == "" {
		return nil, fmt.Errorf("tool_choice allowed_tools entry requires a type")
	}
	switch typ {
	case model.ToolTypeShell, model.ToolTypeLocalShell:
		return []Identity{{OpenAIType: typ, Name: "shell", Freeform: true}}, nil
	case model.ToolTypeApplyPatch:
		return []Identity{{OpenAIType: typ, Name: "apply_patch", Freeform: true}}, nil
	case model.ToolTypeToolSearch:
		return []Identity{{OpenAIType: typ, Name: "tool_search"}}, nil
	case model.ToolTypeFunction, model.ToolTypeCustom:
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tool_choice allowed_tools entry %q requires a name", typ)
		}
		return []Identity{{OpenAIType: typ, Name: name, Freeform: typ == model.ToolTypeCustom}}, nil
	case model.ToolTypeNamespace:
		return inspectAllowedNamespace(tool)
	default:
		return nil, fmt.Errorf("unsupported tool_choice allowed_tools entry %q: Anthropic backend has no safe equivalent", typ)
	}
}

func inspectAllowedNamespace(tool map[string]any) ([]Identity, error) {
	namespace, _ := tool["name"].(string)
	if namespace == "" {
		return nil, fmt.Errorf("tool_choice allowed_tools namespace requires a name")
	}
	rawTools, _ := tool["tools"].([]any)
	if len(rawTools) == 0 {
		return nil, fmt.Errorf("tool_choice allowed_tools namespace %q requires tools", namespace)
	}
	out := make([]Identity, 0, len(rawTools))
	for _, rawTool := range rawTools {
		nested, ok := rawTool.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool_choice allowed_tools namespace %q has invalid child", namespace)
		}
		typ, _ := nested["type"].(string)
		if typ != model.ToolTypeFunction && typ != model.ToolTypeCustom {
			return nil, fmt.Errorf("unsupported tool_choice allowed_tools namespace %q child type %q", namespace, typ)
		}
		name, _ := nested["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tool_choice allowed_tools namespace %q child %q requires a name", namespace, typ)
		}
		out = append(out, Identity{OpenAIType: typ, Namespace: namespace, Name: name, Freeform: typ == model.ToolTypeCustom})
	}
	return out, nil
}

// openaiToolType 从 ToolUnionParam 取出 type 字符串，用于错误信息。
func openaiToolType(t oairesponses.ToolUnionParam) string {
	if typ := t.GetType(); typ != nil && *typ != "" {
		return *typ
	}
	raw, _ := json.Marshal(t)
	var obj struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &obj)
	if obj.Type != "" {
		return obj.Type
	}
	return "unknown"
}
