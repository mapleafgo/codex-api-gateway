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
