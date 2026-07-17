package anthropic

import (
	"encoding/json"
	"strings"
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
}

// Empty 报告该 MCPInjection 是否无需注入（nil 或无 server）。
func (m *MCPInjection) Empty() bool { return m == nil || len(m.Servers) == 0 }

// injectMCP 把 mcp_servers（顶层）与 mcp_toolset（tools[] 追加）写入已 marshal 的请求体。
// mcp 为空时原样返回。复用 injectStream 的 map 操作模式，json.Number 保数值精度。
func injectMCP(body []byte, mcp *MCPInjection) ([]byte, error) {
	if mcp.Empty() {
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
		servers = append(servers, map[string]any{
			"type": "url", "url": s.URL, "name": s.Name,
			"authorization_token": s.AuthorizationToken,
		})
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
