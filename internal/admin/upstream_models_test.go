package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// noCatalogPlugin 实现了 SourcePlugin，但不实现 ModelCatalog：试拉 models 应返回 501。
type noCatalogPlugin struct{}

func (noCatalogPlugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{ID: "no-catalog", Title: "No Catalog", Streaming: plugin.StreamingConverted}
}
func (noCatalogPlugin) ValidateSource(config.Source) error { return nil }
func (noCatalogPlugin) Backend() plugin.Backend            { return fakeBackend{} }

// failingCatalogPlugin 的 ListModels 总是报错，用于验证错误响应不泄漏凭据。
type failingCatalogPlugin struct{}

func (failingCatalogPlugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{ID: "fail-catalog", Title: "Fail Catalog", Streaming: plugin.StreamingConverted}
}
func (failingCatalogPlugin) ValidateSource(config.Source) error { return nil }
func (failingCatalogPlugin) Backend() plugin.Backend            { return fakeBackend{} }
func (failingCatalogPlugin) ListModels(context.Context, config.Source) ([]plugin.Model, error) {
	return nil, errors.New("mock catalog broken")
}

// TestUpstreamModels501WithoutCatalog 验证后端未实现 ModelCatalog 时返回 501。
func TestUpstreamModels501WithoutCatalog(t *testing.T) {
	reg, err := plugin.New(noCatalogPlugin{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	deps, _ := newTestDeps(t)
	deps.Registry = reg
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/upstream-models", "application/json",
		strings.NewReader(`{"backend":"no-catalog","base_url":"https://x"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

// TestUpstreamModelsErrorResponseNoCredentialLeak 验证上游拉取失败时响应体不包含明文字符/凭据。
func TestUpstreamModelsErrorResponseNoCredentialLeak(t *testing.T) {
	reg, err := plugin.New(failingCatalogPlugin{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	deps, _ := newTestDeps(t)
	deps.Registry = reg
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	draft := `{"backend":"fail-catalog","base_url":"https://secret.example","api_key":"sk-very-very-secret","options":{"github_token":"gho_topsecret"}}`
	resp, err := http.Post(srv.URL+"/admin/api/upstream-models", "application/json", strings.NewReader(draft))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw := new(strings.Builder)
	_, _ = io.Copy(raw, resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, raw)
	}
	for _, banned := range []string{"sk-very-very-secret", "gho_topsecret", "secret.example"} {
		if strings.Contains(raw.String(), banned) {
			t.Fatalf("502 响应泄漏凭据 %q", banned)
		}
	}
}

// TestUpstreamModelsDraftMergesSavedSensitiveOptions 验证草稿同名保存源时，
// 缺省 base_url 与敏感 options（github_token）从已保存源继承到试拉请求。
func TestUpstreamModelsDraftMergesSavedSensitiveOptions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer saved-gho-token" {
			t.Fatalf("Authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Editor-Version") == "" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Fatalf("missing Copilot headers: %+v", r.Header)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5","model_picker_enabled":true,"capabilities":{"type":"chat"}}]}`))
	}))
	defer upstream.Close()

	deps, _ := newTestDeps(t)
	cur := deps.Holder.Current()
	cur.Sources = append(cur.Sources, config.Source{
		Name: "copilot", BaseURL: upstream.URL,
		Backend: "github-copilot",
		Options: map[string]any{"github_token": "saved-gho-token"},
	})
	deps.Holder.Replace(cur)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 草稿只给 name + backend，不携带 base_url 与 token。
	resp, err := http.Post(srv.URL+"/admin/api/upstream-models",
		"application/json", strings.NewReader(`{"name":"copilot","backend":"github-copilot"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 {
		t.Fatalf("models=%+v", out.Models)
	}
}
