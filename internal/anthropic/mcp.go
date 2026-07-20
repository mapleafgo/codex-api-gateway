package anthropic

import (
	"encoding/json"
	"strings"

	asdk "github.com/anthropics/anthropic-sdk-go"
)

// MCPBetaHeader 是 MCP managed connector 所需的 anthropic-beta 值。
const MCPBetaHeader = "mcp-client-2025-11-20"

// MCPServer 描述一个待注入请求体顶层 mcp_servers[] 的 beta server 定义。
type MCPServer struct {
	Type               string `json:"type"` // "url"
	URL                string `json:"url"`
	Name               string `json:"name"`
	AuthorizationToken string `json:"authorization_token,omitempty"`
}

// MCPToolset 描述一个待注入 tools[] 的 mcp_toolset（allowlist 模式）。
type MCPToolset struct {
	MCPServerName string   // server_label
	EnabledTools  []string // allowed_tools 命中项；空表示全启用（default_config.enabled=true）
}

// MCPInjection 汇总一次请求的全部 MCP 定义，由 convert 产出、client 注入。
type MCPInjection struct {
	Servers  []MCPServer
	Toolsets []MCPToolset
	// History 为 true 时，messages 中含 beta mcp_tool_use/result 历史块，
	// 即使没有 mcp_servers 也需要 anthropic-beta: mcp-client-2025-11-20。
	History bool
}

// Empty 报告该 MCPInjection 是否无需注入（nil 或无 server）。
func (m *MCPInjection) Empty() bool { return m == nil || (len(m.Servers) == 0 && !m.History) }

// NeedsBeta 报告是否需要设置 MCP beta header。
func (m *MCPInjection) NeedsBeta() bool {
	return m != nil && (len(m.Servers) > 0 || m.History)
}

// injectMCP 把 mcp_servers（顶层）与 mcp_toolset（tools[] 追加）写入已 marshal 的请求体。
// mcp 为空时原样返回。复用 injectStream 的 map 操作模式，json.Number 保数值精度。
func injectMCP(body []byte, mcp *MCPInjection) ([]byte, error) {
	// 仅 History、无 Servers 时不需要改 body，但调用方仍会靠 NeedsBeta 设 header。
	if mcp == nil || len(mcp.Servers) == 0 {
		return body, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	servers := make([]any, 0, len(mcp.Servers))
	for _, s := range mcp.Servers {
		server := map[string]any{"type": "url", "url": s.URL, "name": s.Name}
		if s.AuthorizationToken != "" {
			server["authorization_token"] = s.AuthorizationToken
		}
		servers = append(servers, server)
	}
	obj["mcp_servers"] = servers

	tools, _ := obj["tools"].([]any)
	for _, ts := range mcp.Toolsets {
		entry := map[string]any{"type": "mcp_toolset", "mcp_server_name": ts.MCPServerName}
		if len(ts.EnabledTools) == 0 {
			// 无 allowlist：默认配置全启用。
			entry["default_config"] = map[string]any{"enabled": true}
		} else {
			cfg := map[string]any{}
			for _, name := range ts.EnabledTools {
				cfg[name] = map[string]any{"enabled": true}
			}
			entry["configs"] = cfg
			entry["default_config"] = map[string]any{"enabled": false}
		}
		tools = append(tools, entry)
	}
	obj["tools"] = tools
	return json.Marshal(obj)
}

// mergeBetaHeader 把 mcp beta 值并入已有 anthropic-beta（逗号分隔），避免覆盖 thinking。
func mergeBetaHeader(existing string) string {
	if existing == "" {
		return MCPBetaHeader
	}
	if strings.Contains(existing, MCPBetaHeader) {
		return existing
	}
	return existing + "," + MCPBetaHeader
}

// synthesizeMCPEvent 把 beta mcp block 的 raw JSON（content_block_start envelope）
// 解析成合成 MessageStreamEventUnion，使 converter 能用标准 ev.ContentBlock 字段
// 消费（Type/ID/Input/Name/ToolUseID）。
//
// mcp_tool_use / mcp_tool_result 是 beta block，标准 MessageStreamEventUnion 无 Of*
// 变体，标准反序列化会丢字段，故由 ScanEvents 在标准 unmarshal 之前探测 envelope
// + 嵌套 content_block.type 并改走本函数。
//
// Input-encoding contract: MCP beta blocks are synthesized into content_block_start
// events; beta fields (server_name/is_error/...) are encoded into ContentBlock.Input
// as a map because the standard union has no slots for them — converter handlers
// decode by the same contract:
//   - mcp_tool_use    → Input: {server_name, name, arguments}
//   - mcp_tool_result → Input: {output, is_error}
func synthesizeMCPEvent(payload []byte) (*asdk.MessageStreamEventUnion, error) {
	var env struct {
		ContentBlock json.RawMessage `json:"content_block"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, err
	}
	var blk struct {
		Type       string          `json:"type"`
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		ServerName string          `json:"server_name"`
		Input      json.RawMessage `json:"input"`
		ToolUseID  string          `json:"tool_use_id"`
		IsError    bool            `json:"is_error"`
		Content    json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(env.ContentBlock, &blk); err != nil {
		return nil, err
	}
	ev := &asdk.MessageStreamEventUnion{Type: "content_block_start"}
	cb := asdk.ContentBlockStartEventContentBlockUnion{Type: blk.Type}
	switch blk.Type {
	case "mcp_tool_use":
		cb.ID = blk.ID
		cb.Name = blk.Name
		cb.Input = map[string]any{
			"server_name": blk.ServerName,
			"name":        blk.Name,
			"arguments":   string(blk.Input),
		}
	case "mcp_tool_result":
		cb.ToolUseID = blk.ToolUseID
		cb.Input = map[string]any{
			"output":   mcpResultText(blk.Content),
			"is_error": blk.IsError,
		}
	}
	ev.ContentBlock = cb
	return ev, nil
}

// mcpResultText 从 mcp_tool_result.content（[]{type,text}）拼出纯文本。
// 解析失败时退化为原始 JSON 文本，避免丢失上游返回的错误信息。
func mcpResultText(content json.RawMessage) string {
	var parts []map[string]any
	if json.Unmarshal(content, &parts) != nil {
		return string(content)
	}
	var b strings.Builder
	for _, p := range parts {
		if t, ok := p["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}
