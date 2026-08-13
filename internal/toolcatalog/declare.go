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
		return []anthropic.ToolUnionParam{ClientTool(fn.Name, schemaFromAny(fn.Parameters), optionalString(fn.Description))}, nil
	case t.OfCustom != nil:
		c := t.OfCustom
		return []anthropic.ToolUnionParam{ClientTool(c.Name, FreeformInputSchema(customInputDescription(c)), optionalString(c.Description))}, nil
	case t.OfApplyPatch != nil:
		// Codex 的 apply_patch 只消费 V4A 文本：声明 freeform {"s":...}，
		// 回程 custom_tool_call.input 才能直接交给客户端执行。历史回灌虽按
		// 客户端原始 operation/path/diff 直接回填，但声明必须驱动模型输出文本。
		return []anthropic.ToolUnionParam{ClientTool("apply_patch", FreeformInputSchema(""), nil)}, nil
	case t.OfShell != nil:
		return []anthropic.ToolUnionParam{ClientTool("shell", FreeformInputSchema(""), nil)}, nil
	case t.OfLocalShell != nil:
		return []anthropic.ToolUnionParam{ClientTool("shell", FreeformInputSchema(""), nil)}, nil
	case t.OfToolSearch != nil:
		s := t.OfToolSearch
		return []anthropic.ToolUnionParam{ClientTool("tool_search", schemaFromAny(s.Parameters), optionalString(s.Description))}, nil
	case t.OfNamespace != nil:
		ns := t.OfNamespace
		out := make([]anthropic.ToolUnionParam, 0, len(ns.Tools))
		for _, nested := range ns.Tools {
			switch {
			case nested.OfFunction != nil:
				fn := nested.OfFunction
				out = append(out, ClientTool(ToolName(ns.Name, fn.Name), schemaFromAny(fn.Parameters), optionalString(fn.Description)))
			case nested.OfCustom != nil:
				c := nested.OfCustom
				out = append(out, ClientTool(ToolName(ns.Name, c.Name), FreeformInputSchema(customInputDescription(c)), optionalString(c.Description)))
			default:
				return nil, fmt.Errorf("unsupported namespace tool: Anthropic backend has no safe equivalent")
			}
		}
		return out, nil
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
		out = append(out, ClientTool(ToolName("mcp__"+m.ServerLabel, name), nil, optionalString(m.ServerDescription)))
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

// ClientTool 构造一个 Anthropic client tool（ToolParam），统一省略 type 字段，
// 与官方自定义工具的缺省形态一致（name + description + input_schema）。
func ClientTool(name string, schema map[string]any, description *string) anthropic.ToolUnionParam {
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
	return anthropic.ToolUnionParam{OfTool: tool}
}

// ToolName 返回 namespace 工具的转换后名（namespace 为空时原样返回）。
func ToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "__" + name
}

// ResolveIdentityFromFlat 从请求声明的扁平工具名映射还原身份。
// 先按 flat 精确匹配；未命中且 flat 不含 "__"（上游可能丢弃了 namespace 前缀，
// 如只返回 spawn_agent 而非 collaboration__spawn_agent）时，在声明的 values 里
// 按 Name 做唯一回退匹配，补回 namespace。name 非唯一（多个 namespace 同名）
// 时不回退，避免猜错。
func ResolveIdentityFromFlat(declared map[string]Identity, flat string) (Identity, bool) {
	if declared == nil {
		return Identity{}, false
	}
	if id, ok := declared[flat]; ok {
		return id, true
	}
	if strings.Contains(flat, "__") {
		return Identity{}, false
	}
	var hit Identity
	found := false
	for _, id := range declared {
		if id.Name == flat {
			if found {
				return Identity{}, false // 同名多 namespace，歧义不回退
			}
			hit = id
			found = true
		}
	}
	return hit, found
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

// customInputDescription 生成 freeform input 字段的 description：
// 只说明该字段必须遵循的格式（format.definition），不携带工具级 description。
func customInputDescription(c *oairesponses.CustomToolParam) string {
	if c == nil {
		return ""
	}
	if g := c.Format.OfGrammar; g != nil && g.Definition != "" {
		return FormatRequirementDescription(g.Definition)
	}
	return ""
}

// FormatRequirementDescription 返回 freeform input 字段的必填格式说明（英文 wire 文本）。
func FormatRequirementDescription(definition string) string {
	return "Required format:\n" + definition
}

// FreeformInputSchema 返回 freeform 工具（shell/custom）的通用 input schema。
// description 为空时使用默认字段描述，保证 input 属性始终携带 description。
func FreeformInputSchema(description string) map[string]any {
	if description == "" {
		description = "The freeform text input to pass to the tool."
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"s": map[string]any{"type": "string", "description": description},
		},
		"required":             []string{"s"},
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
