package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		Server:  config.ServerCfg{Listen: ":0"},
		Logging: config.LoggingCfg{Level: "info", Format: "text"},
		Cache:   config.CacheCfg{TTL: "5m"},
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
	return writeFile(path, out)
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

	// POST：加一个 source
	view.Sources = append(view.Sources, sourceView{
		Name: "s2", BaseURL: "https://two.example.com", APIKey: "k2", DefaultModel: "m2",
	})
	view.Models = map[string]modelView{"glm-latest": {ContextWindow: ptrInt64(100000)}}
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
	if len(cur.ModelOverrides) != 1 {
		t.Errorf("models = %v", cur.ModelOverrides)
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

// TestYamlMarshalOmitsEmpty 验证管理页保存时空值字段不写入 config.yaml。
// 覆盖 logging.format/file、cache.ttl、breaker 各字段、source 的
// api_key/default_model/model_map、顶层 breaker/cache/models 为空时整体省略。
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
		"first_byte_timeout:", "cooldown:", "degrade_threshold:",
		"breaker:", "cache:", "models:", "base_instructions_file:",
	}
	for _, unwanted := range mustNotContain {
		if strings.Contains(s, unwanted) {
			t.Errorf("输出不应包含空值字段 %q，实际：\n%s", unwanted, s)
		}
	}
}
