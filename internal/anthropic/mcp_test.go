package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInjectMCPAddsServersAndToolset(t *testing.T) {
	body := []byte(`{"model":"x","tools":[{"type":"web_search_20250305"}]}`)
	out, err := injectMCP(body, &MCPInjection{
		Servers:  []MCPServer{{URL: "https://s.example", Name: "weather", AuthorizationToken: "tok"}},
		Toolsets: []MCPToolset{{MCPServerName: "weather", EnabledTools: []string{"get"}}},
	})
	if err != nil {
		t.Fatalf("injectMCP: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	servers := obj["mcp_servers"].([]any)
	s0 := servers[0].(map[string]any)
	if s0["type"] != "url" || s0["name"] != "weather" || s0["authorization_token"] != "tok" {
		t.Fatalf("bad server: %v", s0)
	}
	tools := obj["tools"].([]any)
	ts := tools[1].(map[string]any) // 原有 web_search 在前
	if ts["type"] != "mcp_toolset" || ts["mcp_server_name"] != "weather" {
		t.Fatalf("bad toolset: %v", ts)
	}
}

// TestInjectMCPServerWithoutTokenOmitsKey 验证 server 无 token 时
// 输出 map 不含 authorization_token 键（而非空字符串），避免兼容网关拒绝。
func TestInjectMCPServerWithoutTokenOmitsKey(t *testing.T) {
	body := []byte(`{"model":"x","tools":[]}`)
	out, err := injectMCP(body, &MCPInjection{
		Servers:  []MCPServer{{URL: "https://s.example", Name: "anon"}},
		Toolsets: []MCPToolset{{MCPServerName: "anon"}},
	})
	if err != nil {
		t.Fatalf("injectMCP: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	servers := obj["mcp_servers"].([]any)
	s0 := servers[0].(map[string]any)
	if _, exists := s0["authorization_token"]; exists {
		t.Fatalf("authorization_token must be omitted when empty, got: %v", s0)
	}
}

func TestInjectMCPEmptyNoop(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	out, err := injectMCP(body, nil)
	if err != nil || string(out) != string(body) {
		t.Fatalf("empty must be noop")
	}
}

// TestInjectMCPAllEnabledToolset 验证 EnabledTools 空（filter 降级路径）时，
// injectMCP 写出 default_config.enabled=true（全启用），不产生 configs。
func TestInjectMCPAllEnabledToolset(t *testing.T) {
	body := []byte(`{"model":"x","tools":[]}`)
	out, err := injectMCP(body, &MCPInjection{
		Servers:  []MCPServer{{URL: "https://s.example", Name: "weather"}},
		Toolsets: []MCPToolset{{MCPServerName: "weather"}}, // EnabledTools nil → all-enabled
	})
	if err != nil {
		t.Fatalf("injectMCP: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	tools := obj["tools"].([]any)
	ts := tools[0].(map[string]any)
	dc := ts["default_config"].(map[string]any)
	if dc["enabled"] != true {
		t.Fatalf("empty EnabledTools must produce default_config.enabled=true, got: %v", dc)
	}
	if _, exists := ts["configs"]; exists {
		t.Fatalf("empty EnabledTools must not produce configs, got: %v", ts)
	}
}

// TestInjectMCPRelocatesCacheControlToLastToolset 普通 tools + MCP：
// 旧 function 上的 cache_control 必须清除，末项 mcp_toolset 必须带断点。
func TestInjectMCPRelocatesCacheControlToLastToolset(t *testing.T) {
	body := []byte(`{
		"model":"x",
		"cache_control":{"type":"ephemeral","ttl":"5m"},
		"tools":[{"type":"tool","name":"f","cache_control":{"type":"ephemeral","ttl":"5m"}}]
	}`)
	out, err := injectMCP(body, &MCPInjection{
		Servers:  []MCPServer{{URL: "https://s.example", Name: "weather"}},
		Toolsets: []MCPToolset{{MCPServerName: "weather", EnabledTools: []string{"get"}}},
	})
	if err != nil {
		t.Fatalf("injectMCP: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools := obj["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools len=%d want 2", len(tools))
	}
	first := tools[0].(map[string]any)
	if _, ok := first["cache_control"]; ok {
		t.Fatalf("first tool must not keep cache_control: %v", first)
	}
	last := tools[1].(map[string]any)
	cc, ok := last["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("last tool missing cache_control: %v", last)
	}
	if cc["type"] != "ephemeral" || cc["ttl"] != "5m" {
		t.Fatalf("bad cache_control: %v", cc)
	}
}

// TestInjectMCPOnlyToolsetGetsCacheControl 仅 MCP（初始 tools 空）时
// 末项 mcp_toolset 仍应带 cache_control，TTL 继承顶层 1h。
func TestInjectMCPOnlyToolsetGetsCacheControl(t *testing.T) {
	body := []byte(`{
		"model":"x",
		"cache_control":{"type":"ephemeral","ttl":"1h"},
		"tools":[]
	}`)
	out, err := injectMCP(body, &MCPInjection{
		Servers:  []MCPServer{{URL: "https://s.example", Name: "weather"}},
		Toolsets: []MCPToolset{{MCPServerName: "weather"}},
	})
	if err != nil {
		t.Fatalf("injectMCP: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools := obj["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d want 1", len(tools))
	}
	last := tools[0].(map[string]any)
	cc, ok := last["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("last tool missing cache_control: %v", last)
	}
	if cc["ttl"] != "1h" {
		t.Fatalf("ttl want 1h, got %v", cc["ttl"])
	}
}

func TestMergeBetaHeader(t *testing.T) {
	if got := mergeBetaHeader(""); got != MCPBetaHeader {
		t.Fatalf("empty base: %q", got)
	}
	if got := mergeBetaHeader("interleaved-thinking-2025-05-14"); !strings.Contains(got, MCPBetaHeader) || !strings.Contains(got, "interleaved-thinking") {
		t.Fatalf("must merge: %q", got)
	}
	if got := mergeBetaHeader(MCPBetaHeader); got != MCPBetaHeader {
		t.Fatalf("must dedupe: %q", got)
	}
}

// TestSynthesizeMCPEventToolUse 验证 probe 把 content_block_start envelope 内的
// beta mcp_tool_use 转成合成事件，Input 编码 server_name/name/arguments 三字段。
func TestSynthesizeMCPEventToolUse(t *testing.T) {
	payload := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"mcp_tool_use","id":"toolu_x","name":"get","server_name":"weather","input":{"q":"sf"}}}`)
	ev, err := synthesizeMCPEvent(payload)
	if err != nil {
		t.Fatalf("synthesizeMCPEvent: %v", err)
	}
	if ev.Type != "content_block_start" {
		t.Fatalf("Type=%q want content_block_start", ev.Type)
	}
	cb := ev.ContentBlock
	if cb.Type != "mcp_tool_use" || cb.ID != "toolu_x" || cb.Name != "get" {
		t.Fatalf("bad tool_use header: %+v", cb)
	}
	m, ok := cb.Input.(map[string]any)
	if !ok {
		t.Fatalf("Input not map: %T", cb.Input)
	}
	if m["server_name"] != "weather" || m["name"] != "get" {
		t.Fatalf("bad Input map: %v", m)
	}
	if m["arguments"] != `{"q":"sf"}` {
		t.Fatalf("bad arguments: %v", m["arguments"])
	}
}

// TestSynthesizeMCPEventToolResultOK 验证 mcp_tool_result 的 content[]{text}
// 被折叠进 Input["output"]，is_error=false。
func TestSynthesizeMCPEventToolResultOK(t *testing.T) {
	payload := []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"mcp_tool_result","tool_use_id":"toolu_x","is_error":false,"content":[{"type":"text","text":"sunny"},{"type":"text","text":" day"}]}}`)
	ev, err := synthesizeMCPEvent(payload)
	if err != nil {
		t.Fatalf("synthesizeMCPEvent: %v", err)
	}
	cb := ev.ContentBlock
	if cb.Type != "mcp_tool_result" || cb.ToolUseID != "toolu_x" {
		t.Fatalf("bad tool_result header: %+v", cb)
	}
	m, ok := cb.Input.(map[string]any)
	if !ok {
		t.Fatalf("Input not map: %T", cb.Input)
	}
	if m["output"] != "sunny day" {
		t.Fatalf("output = %q want %q", m["output"], "sunny day")
	}
	if m["is_error"] != false {
		t.Fatalf("is_error = %v want false", m["is_error"])
	}
}

// TestSynthesizeMCPEventToolResultError 验证 is_error=true 时 Input["is_error"]=true。
func TestSynthesizeMCPEventToolResultError(t *testing.T) {
	payload := []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"mcp_tool_result","tool_use_id":"toolu_y","is_error":true,"content":[{"type":"text","text":"boom"}]}}`)
	ev, err := synthesizeMCPEvent(payload)
	if err != nil {
		t.Fatalf("synthesizeMCPEvent: %v", err)
	}
	cb := ev.ContentBlock
	m, ok := cb.Input.(map[string]any)
	if !ok {
		t.Fatalf("Input not map: %T", cb.Input)
	}
	if m["output"] != "boom" {
		t.Fatalf("output = %q want boom", m["output"])
	}
	if m["is_error"] != true {
		t.Fatalf("is_error = %v want true", m["is_error"])
	}
}

// TestSynthesizeMCPEventToolResultInvalidContent 验证 content 非 array 时
// mcpResultText 退化为原始 JSON 文本（不丢错误信息）。
func TestSynthesizeMCPEventToolResultInvalidContent(t *testing.T) {
	payload := []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"mcp_tool_result","tool_use_id":"toolu_z","content":"raw string"}}`)
	ev, err := synthesizeMCPEvent(payload)
	if err != nil {
		t.Fatalf("synthesizeMCPEvent: %v", err)
	}
	m, ok := ev.ContentBlock.Input.(map[string]any)
	if !ok {
		t.Fatalf("Input not map: %T", ev.ContentBlock.Input)
	}
	if m["output"] != `"raw string"` {
		t.Fatalf("fallback output = %q want raw JSON %q", m["output"], `"raw string"`)
	}
}
