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

func TestInjectMCPEmptyNoop(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	out, err := injectMCP(body, nil)
	if err != nil || string(out) != string(body) {
		t.Fatalf("empty must be noop")
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
