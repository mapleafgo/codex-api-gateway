package toolcatalog

import (
	"fmt"
	"log/slog"
	"strings"

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
		return []anthropic.ToolUnionParam{ClientTool(c.Name, FreeformInputSchema(), optionalString(c.Description), true)}, nil
	case t.OfApplyPatch != nil:
		// Codex 的 apply_patch 只消费 V4A 文本：声明 freeform {"input":...}，
		// 回程 custom_tool_call.input 才能直接交给客户端执行。历史回灌虽按
		// 客户端原始 operation/path/diff 直接回填，但声明必须驱动模型输出文本。
		return []anthropic.ToolUnionParam{ClientTool("apply_patch", FreeformInputSchema(), nil, true)}, nil
	case t.OfShell != nil:
		return []anthropic.ToolUnionParam{ClientTool("shell", FreeformInputSchema(), nil, true)}, nil
	case t.OfLocalShell != nil:
		return []anthropic.ToolUnionParam{ClientTool("shell", FreeformInputSchema(), nil, true)}, nil
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
				out = append(out, ClientTool(ToolName(ns.Name, c.Name), FreeformInputSchema(), optionalString(c.Description), true))
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
		// MCP 由 Codex 客户端本地执行：allowed_tools 展开为扁平
		// mcp__<server>__<tool> 标准 tool 声明（与 c 路径一致），
		// 不再注入 Anthropic beta mcp_servers/mcp_toolset。
		return mcpClientToolDecls(t.OfMcp), nil
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

// mcpClientToolDecls 把 MCP allowed_tools 展开为扁平 client tool 声明。
// filter / 空列表形态不展开（工具经 tool_search 动态提供）。
func mcpClientToolDecls(m *oairesponses.ToolMcpParam) []anthropic.ToolUnionParam {
	if m == nil || m.ServerLabel == "" {
		return nil
	}
	if HasMCPConnectionFields(m) {
		slog.Debug("MCP 连接字段由 Codex 客户端本地使用，网关不注入上游",
			"server_label", m.ServerLabel)
	}
	if m.AllowedTools.OfMcpToolFilter != nil {
		slog.Debug("mcp allowed_tools filter 不展开为 Anthropic tool 声明",
			"server_label", m.ServerLabel)
		return nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(m.AllowedTools.OfMcpAllowedTools))
	for _, name := range m.AllowedTools.OfMcpAllowedTools {
		if name == "" {
			continue
		}
		out = append(out, ClientTool(ToolName("mcp__"+m.ServerLabel, name), nil, nil, false))
	}
	return out
}

// HasMCPConnectionFields 报告 type:mcp 是否携带客户端连接信息（server_url /
// authorization / headers / require_approval / connector_id / tunnel_id）。
// 这些字段只对 Codex 本地连接有意义，网关声明时不需要也不应注入上游。
func HasMCPConnectionFields(m *oairesponses.ToolMcpParam) bool {
	if m == nil {
		return false
	}
	return m.ConnectorID != "" ||
		m.ServerURL.Valid() ||
		m.Authorization.Valid() ||
		len(m.Headers) > 0 ||
		m.TunnelID.Valid() ||
		m.RequireApproval.OfMcpToolApprovalSetting.Valid() ||
		m.RequireApproval.OfMcpToolApprovalFilter != nil
}

// webSearchTool 构造 Anthropic web_search_20260209。
// 部分 Anthropic 兼容后端（如 DeepSeek）只接受 20250305/20260209 版本，
// 不识别 20260318，故统一使用 20260209 保证声明可被解析。
// city/country/region/timezone 来自 OpenAI user_location（两侧均为 param.Opt[string] 但包不同，按值拷贝）。
// Anthropic 无 search_context_size 字段，调用方对非空值自行 WARN。
func webSearchTool(allowed []string, city, country, region, timezone oparam.Opt[string]) anthropic.ToolUnionParam {
	p := &anthropic.WebSearchTool20260209Param{AllowedDomains: allowed}
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
	return anthropic.ToolUnionParam{OfWebSearchTool20260209: p}
}

// ClientTool 构造一个 Anthropic client tool（ToolParam）。
// custom=true 标记为 freeform custom tool（shell / custom）。
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

// StripToolType 清空 client tool（OfTool）的 type 字段，server tool 保持不变。
// DeepSeek 等 Anthropic 兼容端点的 serde 只接受缺省形态的工具声明，
// 显式 type:"custom" 会 400（实测 "unknown variant `custom`"）。
func StripToolType(tools []anthropic.ToolUnionParam) {
	for i := range tools {
		if tools[i].OfTool != nil {
			tools[i].OfTool.Type = ""
		}
	}
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

// FreeformInputSchema 返回 freeform 工具（shell/custom）的通用 input schema。
func FreeformInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{"type": "string"},
		},
		"required":             []string{"input"},
		"additionalProperties": false,
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
	// 解析 $defs 引用，使 schema 自包含。部分 Anthropic 兼容后端（如 Grok）的
	// schema validator 不支持 $ref / $defs 引用解析，直接返回 400。
	if defs, ok := schema["$defs"].(map[string]any); ok {
		props = resolveRefs(props, defs).(map[string]any)
	}
	return anthropic.ToolInputSchemaParam{Properties: props, Required: required}
}

// resolveRefs 递归遍历任意 JSON 结构，将 {"$ref": "#/$defs/<name>"} 替换为
// $defs 中对应的定义，并使替换后的结果也继续展开，支持嵌套引用。
func resolveRefs(v any, defs map[string]any) any {
	switch m := v.(type) {
	case map[string]any:
		if ref, ok := m["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			name := ref[len("#/$defs/"):]
			if def, ok := defs[name]; ok && def != nil {
				return resolveRefs(def, defs)
			}
		}
		out := make(map[string]any, len(m))
		for k, child := range m {
			out[k] = resolveRefs(child, defs)
		}
		return out
	case []any:
		out := make([]any, len(m))
		for i, child := range m {
			out[i] = resolveRefs(child, defs)
		}
		return out
	default:
		return v
	}
}

// SplitToolName splits a namespaced tool name into namespace and base name.
func SplitToolName(name string) (namespace, base string) {
	// 用 LastIndex：Codex MCP 命名空间本身带 mcp__ 前缀（如 mcp__browser），
	// 扁平名 mcp__browser__click 必须拆成 ns=mcp__browser / name=click。
	idx := strings.LastIndex(name, "__")
	if idx < 0 {
		return "", name
	}
	return name[:idx], name[idx+2:]
}
