package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/metrics"
)

func newTestDeps(t *testing.T) (*Deps, string) {
	t.Helper()
	cfg := &config.Config{
		Server:    config.ServerCfg{Listen: ":0"},
		Logging:   config.LoggingCfg{Level: "info", Format: "text"},
		Anthropic: config.AnthropicCfg{DefaultMaxTokens: 16384, CacheEnabled: ptrBool(true)},
		Sources: []config.Source{
			{Name: "s1", BaseURL: "https://example.com", APIKey: "k1", DefaultModel: "m1"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	holder := config.NewHolder(cfg)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// 写一份初始 yaml 供 reload fallback
	if err := writeInitialYAML(cfgPath, cfg); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	m := metrics.New()
	t.Cleanup(m.Stop)
	reloadCalled := false
	_ = reloadCalled
	deps := &Deps{
		Holder:  holder,
		Metrics: m,
		CfgPath: cfgPath,
		ReloadFromDisk: func() {
			// 简单 reload：从 cfgPath 重新 Load
			if newCfg, err := config.Load(cfgPath); err == nil {
				holder.Replace(newCfg)
			}
		},
	}
	return deps, cfgPath
}

func writeInitialYAML(path string, cfg *config.Config) error {
	out, err := yamlMarshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func TestMetricsEndpoint(t *testing.T) {
	deps, _ := newTestDeps(t)
	deps.Metrics.Record(metrics.RequestEvent{
		Kind:      metrics.KindUpstream,
		StartedAt: time.Now(), Duration: time.Millisecond,
		SourceName: "s1", Model: "m1", Status: "completed",
		InputTokens: 10, OutputTokens: 5, Code: 200,
	})
	// 等待 consumer 处理
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if deps.Metrics.Snapshot().TotalRequests == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/api/metrics")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %v", resp.StatusCode)
	}
	var snap metrics.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.TotalRequests != 1 {
		t.Errorf("TotalRequests = %d", snap.TotalRequests)
	}
}

func TestMetricsDashboardLabels(t *testing.T) {
	html := string(indexHTML)
	for _, want := range []string{
		"cardReq: '上游调用量'",
		"cardReq: 'Upstream calls'",
		"backendCopilot: 'GitHub Copilot'",
		"github_token: src.github_token || ''",
		"github_token: github_token.trim()",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestCopilotSourceCardHidesTokenInput(t *testing.T) {
	html := string(indexHTML)
	if strings.Contains(html, `x-model="src.github_token"`) {
		t.Fatal("index.html still renders github_token input on source card")
	}
	for _, want := range []string{
		`x-text="t('copilotAuth')"`,
		`@click="authorizeSource(src)"`,
		"copilotAuth: '重新授权'",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestCopilotFormHidesUpstreamURL(t *testing.T) {
	html := string(indexHTML)
	for _, want := range []string{
		`<label class="ui-field ui-span-2" x-show="src.backend_type !== 'g'">`,
		"if (this.formIsCopilot() && (f.key === 'base_url' || f.key === 'api_key' || f.key === 'github_token')) return false;",
		"const base_url = v.backend_type === 'g' ? '' : (v.base_url || '').trim();",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestCopilotSelectionStartsDeviceFlowImmediately(t *testing.T) {
	html := string(indexHTML)
	for _, want := range []string{
		"if (value === 'g' && !wasCopilot) { void this.beginFormCopilotAuth(); return; }",
		"async beginFormCopilotAuth() {",
		":disabled=\"formIsCopilot() && f.key === 'name'\"",
		"copilotAuthHint: '选择 GitHub Copilot 后将立即进入 GitHub 设备授权，无需手动填写 Token'",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestAddSourceSupportsCopilot(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(sourceView{
		Name:        "copilot",
		BackendType: config.BackendGitHubCopilot,
		GithubToken: "github-token",
	})
	resp, err := http.Post(srv.URL+"/admin/api/sources", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post source: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cur := deps.Holder.Current()
	if len(cur.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(cur.Sources))
	}
	got := cur.Sources[1]
	if got.BackendType != config.BackendGitHubCopilot || got.GithubToken != "github-token" {
		t.Fatalf("copilot source = %+v", got)
	}
}

func TestAddSourceCopilotIgnoresUpstreamURL(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(sourceView{
		Name:        "copilot",
		BaseURL:     "https://stale.example.com",
		BackendType: config.BackendGitHubCopilot,
		GithubToken: "github-token",
	})
	resp, err := http.Post(srv.URL+"/admin/api/sources", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post source: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := deps.Holder.Current().Sources[1]
	if got.BaseURL != "" {
		t.Fatalf("BaseURL = %q, want empty for endpoint discovery", got.BaseURL)
	}
}

func TestAdminCopilotTokenHiddenAndBlankSavePreserved(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	addBody, _ := json.Marshal(sourceView{
		Name:        "copilot",
		BackendType: config.BackendGitHubCopilot,
		GithubToken: "github-token",
	})
	resp, err := http.Post(srv.URL+"/admin/api/sources", "application/json", bytes.NewReader(addBody))
	if err != nil {
		t.Fatalf("post source: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(srv.URL + "/admin/api/config")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "github-token") {
		t.Fatal("admin config response exposes github token")
	}

	var view adminConfigView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if len(view.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(view.Sources))
	}
	view.Sources[1].GithubToken = ""
	saveBody, _ := json.Marshal(view)
	resp, err = http.Post(srv.URL+"/admin/api/config", "application/json", bytes.NewReader(saveBody))
	if err != nil {
		t.Fatalf("post config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d", resp.StatusCode)
	}
	if got := deps.Holder.Current().Sources[1].GithubToken; got != "github-token" {
		t.Fatalf("GithubToken = %q, want preserved", got)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// GET
	resp, err := http.Get(srv.URL + "/admin/api/config")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var view adminConfigView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Sources) != 1 || view.Sources[0].Name != "s1" {
		t.Fatalf("sources = %+v", view.Sources)
	}
	if view.Anthropic.DefaultMaxTokens != 16384 || !view.Anthropic.CacheEnabled {
		t.Fatalf("anthropic = %+v", view.Anthropic)
	}
	if view.Breaker.RequestTimeout != "2m0s" {
		t.Fatalf("default request_timeout view = %q, want 2m0s (120s)", view.Breaker.RequestTimeout)
	}
	view.Anthropic = anthropicView{
		DefaultMaxTokens: 32768,
		CacheEnabled:     false,
	}
	view.Breaker.RequestTimeout = "45s"
	view.Sources[0].Breaker = &breakerView{RequestTimeout: "30s"}

	// POST：加一个 source
	view.Sources = append(view.Sources, sourceView{
		Name: "s2", BaseURL: "https://two.example.com", APIKey: "k2", DefaultModel: "m2",
		Disabled: true,
	})
	view.Models = []modelViewItem{{Slug: "glm-latest", ContextWindow: ptrInt64(100000)}}
	body, _ := json.Marshal(view)
	resp2, err := http.Post(srv.URL+"/admin/api/config", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp2.StatusCode != 200 {
		t.Fatalf("status = %v", resp2.StatusCode)
	}

	// 验证 holder 已替换
	cur := deps.Holder.Current()
	if len(cur.Sources) != 2 {
		t.Errorf("after save: sources = %d", len(cur.Sources))
	}
	var s2 *config.Source
	for i := range cur.Sources {
		if cur.Sources[i].Name == "s2" {
			s2 = &cur.Sources[i]
			break
		}
	}
	if s2 == nil || !s2.Disabled {
		t.Errorf("s2 disabled not preserved: %+v", s2)
	}
	if len(cur.ModelOverrides) != 1 {
		t.Errorf("models = %v", cur.ModelOverrides)
	}
	if cur.Anthropic.DefaultMaxTokens != 32768 || cur.Anthropic.CacheEnabled == nil ||
		*cur.Anthropic.CacheEnabled {
		t.Errorf("anthropic config not preserved: %+v", cur.Anthropic)
	}
	if cur.Breaker.RequestTimeout != config.Duration(45*time.Second) {
		t.Errorf("after save: global request_timeout = %v, want 45s", cur.Breaker.RequestTimeout)
	}
	if cur.Sources[0].Breaker == nil || cur.Sources[0].Breaker.RequestTimeout != config.Duration(30*time.Second) {
		t.Errorf("after save: per-source request_timeout = %+v, want 30s", cur.Sources[0].Breaker)
	}
}

func TestBuildConfigFromInputPreservesCopilotToken(t *testing.T) {
	cfg := buildConfigFromInput(adminConfigInput{
		Sources: []sourceView{{
			Name:         "copilot",
			BackendType:  config.BackendGitHubCopilot,
			GithubToken:  "github-token",
			DefaultModel: "gpt-5.3-codex",
		}},
	}, nil)
	if cfg.Sources[0].GithubToken != "github-token" {
		t.Fatalf("GithubToken = %q, want github-token", cfg.Sources[0].GithubToken)
	}
}

func TestPanicRecovery(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	// 注入一个会 panic 的端点
	mux.HandleFunc("/admin/api/boom", recoverMiddleware("boom", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	_ = deps
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/boom", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != 500 {
		t.Errorf("status = %v, want 500", resp.StatusCode)
	}
}

func ptrInt64(v int64) *int64 { return &v }
func ptrBool(v bool) *bool    { return &v }

func seedModelSlugs(t *testing.T, deps *Deps) {
	t.Helper()
	cur := deps.Holder.Current()
	next := *cur
	next.ModelOverrides = map[string]config.ModelOverride{
		"a": {},
		"b": {},
	}
	next.ModelSlugOrder = []string{"a", "b"}
	deps.Holder.Replace(&next)
}

func TestModelMutationAPIsSyncModelCatalog(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
		want []string
	}{
		{name: "add", path: "/admin/api/models/add", body: `{"slug":"c"}`, want: []string{"a", "b", "c"}},
		{name: "delete", path: "/admin/api/models/delete", body: `{"slug":"a"}`, want: []string{"b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _ := newTestDeps(t)
			seedModelSlugs(t, deps)
			var syncs [][]string
			deps.SyncModelCatalog = func() error {
				order := append([]string(nil), deps.Holder.Current().ModelSlugOrder...)
				syncs = append(syncs, order)
				return nil
			}

			mux := http.NewServeMux()
			Mount(mux, *deps)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			resp, err := http.Post(srv.URL+tc.path, "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200", resp.StatusCode)
			}
			if len(syncs) == 0 {
				t.Fatal("模型变更后未触发 SyncModelCatalog")
			}
			got := syncs[len(syncs)-1]
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("sync order=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestModelMutationReportsCatalogSyncFailure(t *testing.T) {
	deps, _ := newTestDeps(t)
	seedModelSlugs(t, deps)
	deps.SyncModelCatalog = func() error {
		return fmt.Errorf("disk full")
	}

	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/models/add", "application/json", strings.NewReader(`{"slug":"c"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", resp.StatusCode)
	}
	cur := deps.Holder.Current()
	found := false
	for _, slug := range cur.ModelSlugOrder {
		if slug == "c" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("models.yaml 已保存，但 holder 顺序缺少 c: %v", cur.ModelSlugOrder)
	}
}

func TestAnthropicConfigCard(t *testing.T) {
	html := string(indexHTML)
	for _, want := range []string{
		`t('anthropicParams')`,
		`cfg.anthropic.default_max_tokens`,
		`cfg.anthropic.cache_enabled`,
		`t('loggingParams')`,
		`class="ui-grid-3"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Anthropic config card missing %q", want)
		}
	}
}

// TestYamlMarshalOmitsEmpty 验证管理页保存时空值字段不写入 config.yaml。
// 覆盖 logging.format/file、anthropic 各字段、breaker 各字段、source 的
// api_key/default_model/model_map、顶层 breaker/anthropic/models 为空时整体省略。
func TestYamlMarshalOmitsEmpty(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Server:  config.ServerCfg{Listen: ":9870"},
		Logging: config.LoggingCfg{Level: "info"}, // format/file 空
		Sources: []config.Source{
			{Name: "s1", BaseURL: "https://x"}, // api_key/default_model/model_map 空
		},
	}
	out, err := yamlMarshal(cfg)
	if err != nil {
		t.Fatalf("yamlMarshal: %v", err)
	}
	s := string(out)
	// 应该出现的非空字段
	mustContain := []string{"listen: :9870", "level: info", "name: s1", "base_url: https://x"}
	for _, want := range mustContain {
		if !strings.Contains(s, want) {
			t.Errorf("输出应包含 %q，实际：\n%s", want, s)
		}
	}
	// 空值字段不应出现
	mustNotContain := []string{
		"format:", "file:", "ttl:", "api_key:", "default_model:", "model_map:",
		"first_byte_timeout:", "degrade_threshold:",
		"breaker:", "anthropic:", "models:", "base_instructions_file:",
	}
	for _, unwanted := range mustNotContain {
		if strings.Contains(s, unwanted) {
			t.Errorf("输出不应包含空值字段 %q，实际：\n%s", unwanted, s)
		}
	}
}

// TestUpstreamModelsUnsaved 验证未落盘试拉：POST body 凭证即可拉 models，不依赖已保存源名。
func TestUpstreamModelsUnsaved(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" && r.URL.Path != "/models" {
			// chatclient modelsURL: base+/models
			if !strings.HasSuffix(r.URL.Path, "/models") {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o","display_name":"GPT-4o"}]}`))
	}))
	defer upstream.Close()

	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"base_url":     upstream.URL + "/v1",
		"api_key":      "secret",
		"backend_type": "c",
	})
	resp, err := http.Post(srv.URL+"/admin/api/upstream-models", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out struct {
		Models []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ID != "gpt-4o" {
		t.Fatalf("models=%+v", out.Models)
	}
}

func TestUpstreamModelsAcceptsResponsesBackend(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("auth=%s", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5","display_name":"GPT-5"}]}`))
	}))
	defer upstream.Close()

	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"base_url":     upstream.URL + "/v1",
		"api_key":      "secret",
		"backend_type": "r",
	})
	resp, err := http.Post(srv.URL+"/admin/api/upstream-models", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ID != "gpt-5" {
		t.Fatalf("models=%+v", out.Models)
	}
}

func TestUpstreamModelsCopilotUsesSavedToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer github-token" {
			t.Fatalf("Authorization=%s", r.Header.Get("Authorization"))
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
		BackendType: config.BackendGitHubCopilot, GithubToken: "github-token",
	})
	deps.Holder.Replace(cur)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/upstream-models", "application/json",
		strings.NewReader(`{"name":"copilot","backend_type":"g"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ID != "gpt-5" {
		t.Fatalf("models=%+v", out.Models)
	}
}

func TestUpstreamModelsRejectsInvalidBackendType(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := []byte(`{"base_url":"https://x","backend_type":"openai"}`)
	resp, err := http.Post(srv.URL+"/admin/api/upstream-models", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestSourcesHealthInMetrics(t *testing.T) {
	deps, _ := newTestDeps(t)
	deps.SourceHealth = func() []SourceHealthView {
		return []SourceHealthView{
			{Name: "s1", State: "degraded", DegradeCount: 1, Priority: 1},
		}
	}
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/api/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	hs, ok := body["sources_health"].([]any)
	if !ok || len(hs) != 1 {
		t.Fatalf("sources_health=%v", body["sources_health"])
	}
	row := hs[0].(map[string]any)
	if row["name"] != "s1" || row["state"] != "degraded" {
		t.Fatalf("row=%v", row)
	}
}

func TestPromoteSource(t *testing.T) {
	deps, _ := newTestDeps(t)
	called := ""
	deps.PromoteSource = func(name string) error {
		called = name
		if name == "missing" {
			return fmt.Errorf("unknown")
		}
		return nil
	}
	deps.SourceHealth = func() []SourceHealthView {
		return []SourceHealthView{}
	}
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/sources/promote", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Post(srv.URL+"/admin/api/sources/promote", "application/json",
		strings.NewReader(`{"name":"s1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if called != "s1" {
		t.Fatalf("called=%q", called)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("body=%v", body)
	}
}

func TestSetSourceDisabled(t *testing.T) {
	deps, _ := newTestDeps(t)
	deps.SourceHealth = func() []SourceHealthView {
		cur := deps.Holder.Current()
		out := make([]SourceHealthView, 0, len(cur.Sources))
		for i, s := range cur.Sources {
			out = append(out, SourceHealthView{
				Name: s.Name, State: "normal", Priority: i + 1, Disabled: s.Disabled,
			})
		}
		return out
	}
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// missing name
	resp, err := http.Post(srv.URL+"/admin/api/sources/disabled", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// unknown source
	resp, err = http.Post(srv.URL+"/admin/api/sources/disabled", "application/json",
		strings.NewReader(`{"name":"missing","disabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// disable s1
	resp, err = http.Post(srv.URL+"/admin/api/sources/disabled", "application/json",
		strings.NewReader(`{"name":"s1","disabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["disabled"] != true || body["name"] != "s1" {
		t.Fatalf("body=%v", body)
	}
	if !deps.Holder.Current().Sources[0].Disabled {
		t.Fatal("holder not updated")
	}

	// re-enable
	resp2, err := http.Post(srv.URL+"/admin/api/sources/disabled", "application/json",
		strings.NewReader(`{"name":"s1","disabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("status=%d", resp2.StatusCode)
	}
	if deps.Holder.Current().Sources[0].Disabled {
		t.Fatal("holder still disabled")
	}
}

func TestSourceTest(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)

	// mock /models endpoint for health check (upstreamhttp.ModelsURL appends /models)
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		// reject missing or empty Bearer token
		if auth == "" || auth == "Bearer " || auth == "Bearer" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4"}]}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// missing base_url
	resp, err := http.Post(srv.URL+"/admin/api/sources/test", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// invalid backend_type
	resp, err = http.Post(srv.URL+"/admin/api/sources/test", "application/json",
		strings.NewReader(`{"base_url":"https://example.com","backend_type":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// reachable with valid api_key
	resp, err = http.Post(srv.URL+"/admin/api/sources/test", "application/json",
		strings.NewReader(`{"base_url":"`+srv.URL+`","api_key":"sk-test","backend_type":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("body=%v", body)
	}
	if body["status"] != "reachable" {
		t.Fatalf("status=%v", body["status"])
	}

	// missing api_key should fail (401 from mock)
	resp2, err := http.Post(srv.URL+"/admin/api/sources/test", "application/json",
		strings.NewReader(`{"base_url":"`+srv.URL+`","backend_type":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("status=%d", resp2.StatusCode)
	}
	var body2 map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&body2); err != nil {
		t.Fatal(err)
	}
	if body2["ok"] != false {
		t.Fatalf("expected ok=false without api_key, got body=%v", body2)
	}
}

func TestSourceTestCopilotUsesSavedTokenWithoutBaseURL(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer github-token" {
			t.Fatalf("Authorization=%s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Editor-Version") == "" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Fatalf("missing Copilot headers: %+v", r.Header)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5","model_picker_enabled":true,"capabilities":{"type":"chat"}}]}`))
	}))
	defer upstream.Close()

	deps, _ := newTestDeps(t)
	cur := deps.Holder.Current()
	cur.Sources[0].BackendType = config.BackendGitHubCopilot
	cur.Sources[0].BaseURL = upstream.URL
	cur.Sources[0].GithubToken = "github-token"
	deps.Holder.Replace(cur)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/sources/test", "application/json",
		strings.NewReader(`{"name":"s1","backend_type":"g"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["status"] != "reachable" {
		t.Fatalf("body=%v", body)
	}
}

func TestSourceHeadersRoundTrip(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/sources", "application/json",
		strings.NewReader(`{"name":"h1","base_url":"https://h.example.com","api_key":"k","headers":{"X-Custom":"v1","X-Proxy-Auth":"v2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	cur := deps.Holder.Current()
	if len(cur.Sources) != 2 {
		t.Fatalf("sources=%d", len(cur.Sources))
	}
	h := cur.Sources[1].Headers
	if h["X-Custom"] != "v1" || h["X-Proxy-Auth"] != "v2" {
		t.Fatalf("headers=%v", h)
	}
}

func TestSourceHeadersReservedSkipped(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/sources", "application/json",
		strings.NewReader(`{"name":"h2","base_url":"https://h.example.com","api_key":"k","headers":{"Authorization":"Hijack","X-Foo":"bar"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	cur := deps.Holder.Current()
	h := cur.Sources[1].Headers
	if _, ok := h["Authorization"]; ok {
		t.Fatalf("reserved header should be stripped: %v", h)
	}
	if h["X-Foo"] != "bar" {
		t.Fatalf("custom header missing: %v", h)
	}
}

func TestAddSource(t *testing.T) {
	deps, _ := newTestDeps(t)
	deps.SourceHealth = func() []SourceHealthView {
		return []SourceHealthView{{Name: "s1", State: "normal", Priority: 1}}
	}
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// missing fields
	resp, err := http.Post(srv.URL+"/admin/api/sources", "application/json", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// success
	resp, err = http.Post(srv.URL+"/admin/api/sources", "application/json",
		strings.NewReader(`{"name":"s2","base_url":"https://two.example.com","api_key":"k2","backend_type":"c","default_model":"m2"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	cur := deps.Holder.Current()
	if len(cur.Sources) != 2 {
		t.Fatalf("sources=%d", len(cur.Sources))
	}
	s2 := cur.Sources[1]
	if s2.Name != "s2" || s2.BaseURL != "https://two.example.com" || s2.BackendType != "c" || s2.DefaultModel != "m2" {
		t.Fatalf("s2=%+v", s2)
	}
	if len(s2.ModelMap) != 0 {
		t.Fatalf("model_map=%v", s2.ModelMap)
	}

	// duplicate
	resp2, err := http.Post(srv.URL+"/admin/api/sources", "application/json",
		strings.NewReader(`{"name":"s2","base_url":"https://dup.example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("dup status=%d", resp2.StatusCode)
	}
}

func TestDeleteSource(t *testing.T) {
	deps, _ := newTestDeps(t)
	// seed second source via holder+disk
	cfg := *deps.Holder.Current()
	cfg.Sources = append(append([]config.Source{}, cfg.Sources...), config.Source{
		Name: "s2", BaseURL: "https://two.example.com", APIKey: "k2", BackendType: "a",
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := writeInitialYAML(deps.CfgPath, &cfg); err != nil {
		t.Fatal(err)
	}
	deps.Holder.Replace(&cfg)

	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/sources/delete", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Post(srv.URL+"/admin/api/sources/delete", "application/json",
		strings.NewReader(`{"name":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Post(srv.URL+"/admin/api/sources/delete", "application/json",
		strings.NewReader(`{"name":"s2"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	cur := deps.Holder.Current()
	if len(cur.Sources) != 1 || cur.Sources[0].Name != "s1" {
		t.Fatalf("sources=%+v", cur.Sources)
	}
}

func TestAddModel(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/models/add", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	body := `{"slug":"glm-latest","context_window":200000,"accepts_image":true,"supports_image_detail_original":false}`
	resp, err = http.Post(srv.URL+"/admin/api/models/add", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	cur := deps.Holder.Current()
	if len(cur.ConfiguredModelSlugs()) != 1 || cur.ConfiguredModelSlugs()[0] != "glm-latest" {
		t.Fatalf("slugs=%v", cur.ConfiguredModelSlugs())
	}
	ov, ok := cur.ModelOverrides["glm-latest"]
	if !ok || ov.ContextWindow == nil || *ov.ContextWindow != 200000 {
		t.Fatalf("override=%+v ok=%v", ov, ok)
	}
	if ov.AcceptsImage == nil || !*ov.AcceptsImage {
		t.Fatalf("accepts_image=%v", ov.AcceptsImage)
	}
	if ov.SupportsImageDetailOriginal == nil || *ov.SupportsImageDetailOriginal {
		t.Fatalf("supports_image_detail_original=%v", ov.SupportsImageDetailOriginal)
	}

	resp2, err := http.Post(srv.URL+"/admin/api/models/add", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("dup status=%d", resp2.StatusCode)
	}
}

func TestDeleteModel(t *testing.T) {
	deps, _ := newTestDeps(t)
	cfg := *deps.Holder.Current()
	cfg.ModelOverrides = map[string]config.ModelOverride{
		"glm-latest": {ContextWindow: ptrInt64(100000)},
		"keep-me":    {ContextWindow: ptrInt64(50000)},
	}
	cfg.ModelSlugOrder = []string{"glm-latest", "keep-me"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := writeInitialYAML(deps.CfgPath, &cfg); err != nil {
		t.Fatal(err)
	}
	deps.Holder.Replace(&cfg)

	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/models/delete", "application/json",
		strings.NewReader(`{"slug":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Post(srv.URL+"/admin/api/models/delete", "application/json",
		strings.NewReader(`{"slug":"glm-latest"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	cur := deps.Holder.Current()
	if _, ok := cur.ModelOverrides["glm-latest"]; ok {
		t.Fatal("glm-latest still present")
	}
	if len(cur.ConfiguredModelSlugs()) != 1 || cur.ConfiguredModelSlugs()[0] != "keep-me" {
		t.Fatalf("slugs=%v", cur.ConfiguredModelSlugs())
	}
}

func TestImmediateWriteUIMarkers(t *testing.T) {
	html := string(indexHTML)
	for _, want := range []string{
		"/admin/api/sources",
		"/admin/api/sources/delete",
		"/admin/api/models/add",
		"/admin/api/models/delete",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
