package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSourcePlugins_DescriptorJSON 验证 /admin/api/source-plugins 返回注册插件的
// 描述符 JSON：稳定 ID 排序、schema/动作可见、绝不含明文凭据。
func TestSourcePlugins_DescriptorJSON(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := newServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/api/source-plugins")
	if err != nil {
		t.Fatalf("GET source-plugins: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var plugins []sourcePluginView
	if err := json.NewDecoder(resp.Body).Decode(&plugins); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(plugins) != 4 {
		t.Fatalf("plugins = %d, want 4", len(plugins))
	}
	// 按 ID 升序。
	wantOrder := []string{"anthropic", "github-copilot", "openai-chat", "openai-responses"}
	for i, p := range plugins {
		if p.ID != wantOrder[i] {
			t.Fatalf("plugins[%d].id = %q, want %q", i, p.ID, wantOrder[i])
		}
		if p.Title == "" {
			t.Fatalf("plugins[%d] missing title", i)
		}
	}
	// Copilot 的扩展动作可见，且 schema 含 github_token 描述但不含值。
	copilot := plugins[1]
	hasDeviceAction := false
	for _, a := range copilot.Actions {
		if a.ID == "device-flow" && len(a.Routes) >= 3 {
			hasDeviceAction = true
		}
	}
	if !hasDeviceAction {
		t.Fatalf("copilot missing device-flow action with routes: %+v", copilot.Actions)
	}
	hasTokenField := false
	for _, f := range copilot.Schema {
		if f.Name == "github_token" && f.Sensitive && f.Target == "options" {
			hasTokenField = true
		}
	}
	if !hasTokenField {
		t.Fatalf("copilot schema missing sensitive github_token: %+v", copilot.Schema)
	}
}

// TestSourcePlugins_NoCredentials 验证响应文本不含明文凭据或 Authorization。
func TestSourcePlugins_NoCredentials(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := newServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/api/source-plugins")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := new(strings.Builder)
	_, _ = io.Copy(body, resp.Body)
	for _, banned := range []string{"gho_", "authorization"} {
		if strings.Contains(strings.ToLower(body.String()), strings.ToLower(banned)) {
			t.Fatalf("source-plugins 响应含敏感串 %q", banned)
		}
	}
}

// TestSourcePlugins_NoRegistry 验证 Registry 为 nil 时返回空列表而非 500。
func TestSourcePlugins_NoRegistry(t *testing.T) {
	deps, _ := newTestDeps(t)
	deps.Registry = nil
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := newServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/api/source-plugins")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// TestSourcePlugins_RejectsNonGET 验证仅允许 GET。
func TestSourcePlugins_RejectsNonGET(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := newServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/source-plugins", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
