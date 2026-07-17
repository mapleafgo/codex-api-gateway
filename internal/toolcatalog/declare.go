package toolcatalog

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	aparam "github.com/anthropics/anthropic-sdk-go/packages/param"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// Declare 把一个 OpenAI ToolUnionParam 映射为 Anthropic tool 声明。
// 返回的切片追加到 MessageNewParams.Tools；namespace tool 展开为多个声明。
// 不支持的变体返回错误（调用方 fail-fast）。
func Declare(t oairesponses.ToolUnionParam) ([]anthropic.ToolUnionParam, error) {
	switch {
	case t.OfFunction != nil:
		fn := t.OfFunction
		return []anthropic.ToolUnionParam{ClientTool(fn.Name, schemaFromAny(fn.Parameters), optionalString(fn.Description), false)}, nil
	case t.OfCustom != nil:
		c := t.OfCustom
		return []anthropic.ToolUnionParam{ClientTool(c.Name, freeformInputSchema(), optionalString(c.Description), true)}, nil
	case t.OfApplyPatch != nil:
		return []anthropic.ToolUnionParam{ClientTool("apply_patch", applyPatchInputSchema(), nil, true)}, nil
	case t.OfShell != nil:
		return []anthropic.ToolUnionParam{ClientTool("shell", freeformInputSchema(), nil, true)}, nil
	case t.OfLocalShell != nil:
		return []anthropic.ToolUnionParam{ClientTool("shell", freeformInputSchema(), nil, true)}, nil
	case t.OfToolSearch != nil:
		s := t.OfToolSearch
		return []anthropic.ToolUnionParam{ClientTool("tool_search", schemaFromAny(s.Parameters), optionalString(s.Description), false)}, nil
	case t.OfNamespace != nil:
		ns := t.OfNamespace
		out := make([]anthropic.ToolUnionParam, 0, len(ns.Tools))
		for _, nested := range ns.Tools {
			switch {
			case nested.OfFunction != nil:
				fn := nested.OfFunction
				out = append(out, ClientTool(ToolName(ns.Name, fn.Name), schemaFromAny(fn.Parameters), optionalString(fn.Description), false))
			case nested.OfCustom != nil:
				c := nested.OfCustom
				out = append(out, ClientTool(ToolName(ns.Name, c.Name), freeformInputSchema(), optionalString(c.Description), true))
			default:
				return nil, fmt.Errorf("unsupported namespace tool: Anthropic backend has no safe equivalent")
			}
		}
		return out, nil
	case t.OfCodeInterpreter != nil:
		// container（file_ids / memory_limit / 显式 cntr_xxx）无 Anthropic 等价，丢弃。
		// Anthropic code execution 无状态单次执行、无 container 概念（已知损失）。
		// Name 由 SDK default 为 code_execution，无需显式设。
		return []anthropic.ToolUnionParam{{OfCodeExecutionTool20250522: &anthropic.CodeExecutionTool20250522Param{}}}, nil
	case t.OfWebSearch != nil:
		return []anthropic.ToolUnionParam{{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
			AllowedDomains: t.OfWebSearch.Filters.AllowedDomains,
		}}}, nil
	case t.OfWebSearchPreview != nil:
		return []anthropic.ToolUnionParam{{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{}}}, nil
	default:
		return nil, fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", openaiToolType(t))
	}
}

// ClientTool 构造一个 Anthropic client tool（ToolParam）。
// custom=true 标记为 freeform custom tool（apply_patch / shell / custom）。
// 被 Declare 与 convert 的 structured-output 注入共用。
func ClientTool(name string, schema map[string]any, description *string, custom bool) anthropic.ToolUnionParam {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	tool := &anthropic.ToolParam{
		Name:        name,
		InputSchema: toInputSchema(schema),
	}
	if description != nil {
		tool.Description = aparam.NewOpt(*description)
	}
	if custom {
		tool.Type = anthropic.ToolTypeCustom
	}
	return anthropic.ToolUnionParam{OfTool: tool}
}

// ToolName 返回 namespace 工具的转换后名（namespace 为空时原样返回）。
func ToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "__" + name
}

func optionalString(v oparam.Opt[string]) *string {
	if !v.Valid() {
		return nil
	}
	return &v.Value
}

func schemaFromAny(v any) map[string]any {
	s, _ := v.(map[string]any)
	return s
}

func freeformInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{"type": "string"},
		},
		"required": []string{"input"},
	}
}

func applyPatchInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operation": map[string]any{"type": "string", "enum": []string{"create_file", "delete_file", "update_file"}},
			"path":      map[string]any{"type": "string"},
			"diff":      map[string]any{"type": "string"},
		},
		"required": []string{"operation", "path"},
	}
}

func toInputSchema(schema map[string]any) anthropic.ToolInputSchemaParam {
	props, _ := schema["properties"].(map[string]any)
	var required []string
	switch r := schema["required"].(type) {
	case []string:
		required = r
	case []any:
		required = make([]string, 0, len(r))
		for _, item := range r {
			if s, ok := item.(string); ok {
				required = append(required, s)
			}
		}
	}
	return anthropic.ToolInputSchemaParam{Properties: props, Required: required}
}
