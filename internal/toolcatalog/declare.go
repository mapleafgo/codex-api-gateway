package toolcatalog

import (
	"fmt"
	"log/slog"

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
		// Codex apply_patch 是 freeform V4A 文本工具（非 structured operation/path/diff）。
		// 声明 structured schema 会诱导上游产出 JSON，回程/客户端校验双双失败。
		desc := ApplyPatchDescription()
		return []anthropic.ToolUnionParam{ClientTool("apply_patch", freeformInputSchema(), &desc, true)}, nil
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
		ci := t.OfCodeInterpreter
		if ci.Container.OfString.Valid() && ci.Container.OfString.Value != "" {
			slog.Warn("丢弃 code_interpreter.container 显式 container_id（Anthropic code_execution 无 container），对应数据被丢弃",
				"field", "container",
				"container_id", ci.Container.OfString.Value,
				"impact", "不会挂载 OpenAI container 文件或状态")
		} else if ci.Container.OfCodeInterpreterToolAuto != nil {
			auto := ci.Container.OfCodeInterpreterToolAuto
			nFiles := 0
			if auto != nil {
				nFiles = len(auto.FileIDs)
			}
			slog.Warn("丢弃 code_interpreter.container auto 配置（file_ids/memory_limit 无 Anthropic 等价），对应数据被丢弃",
				"field", "container",
				"file_ids", nFiles,
				"impact", "不会向 code_execution 注入上传文件或内存上限")
		}
		return []anthropic.ToolUnionParam{{OfCodeExecutionTool20250522: &anthropic.CodeExecutionTool20250522Param{}}}, nil
	case t.OfMcp != nil:
		// MCP 是 beta server tool，不产出标准 ToolUnionParam；
		// 其请求定义由 convert.collectMCP 产出 MCPInjection，client 注入。
		return nil, nil
	case t.OfWebSearch != nil:
		ws := t.OfWebSearch
		if ws.SearchContextSize != "" {
			slog.Warn("忽略 web_search.search_context_size（Anthropic web_search 无等价字段），对应数据被丢弃",
				"field", "search_context_size",
				"value", string(ws.SearchContextSize),
				"impact", "不会调整 Anthropic 搜索上下文规模")
		}
		return []anthropic.ToolUnionParam{
			webSearchTool(ws.Filters.AllowedDomains, ws.UserLocation.City, ws.UserLocation.Country, ws.UserLocation.Region, ws.UserLocation.Timezone),
		}, nil
	case t.OfWebSearchPreview != nil:
		wp := t.OfWebSearchPreview
		if wp.SearchContextSize != "" {
			slog.Warn("忽略 web_search_preview.search_context_size（Anthropic web_search 无等价字段），对应数据被丢弃",
				"field", "search_context_size",
				"value", string(wp.SearchContextSize),
				"impact", "不会调整 Anthropic 搜索上下文规模")
		}
		return []anthropic.ToolUnionParam{
			webSearchTool(nil, wp.UserLocation.City, wp.UserLocation.Country, wp.UserLocation.Region, wp.UserLocation.Timezone),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", openaiToolType(t))
	}
}

// webSearchTool 构造 Anthropic web_search_20250305。
// city/country/region/timezone 来自 OpenAI user_location（两侧均为 param.Opt[string] 但包不同，按值拷贝）。
// Anthropic 无 search_context_size 字段，调用方对非空值自行 WARN。
func webSearchTool(allowed []string, city, country, region, timezone oparam.Opt[string]) anthropic.ToolUnionParam {
	p := &anthropic.WebSearchTool20250305Param{AllowedDomains: allowed}
	if city.Valid() || country.Valid() || region.Valid() || timezone.Valid() {
		loc := anthropic.UserLocationParam{}
		if city.Valid() {
			loc.City = aparam.NewOpt(city.Value)
		}
		if country.Valid() {
			loc.Country = aparam.NewOpt(country.Value)
		}
		if region.Valid() {
			loc.Region = aparam.NewOpt(region.Value)
		}
		if timezone.Valid() {
			loc.Timezone = aparam.NewOpt(timezone.Value)
		}
		p.UserLocation = loc
	}
	return anthropic.ToolUnionParam{OfWebSearchTool20250305: p}
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
