package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/assembly"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/configwatch"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// e2eSourcePlugin 是 US4 端到端测试用的自包含插件：只靠注册表接入，
// 共享核心零改动即可完成 /v1/responses 转发与观测身份透传。
type e2eSourcePlugin struct{}

func (e2eSourcePlugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:           "e2e-source",
		Title:        "E2E Source",
		Summary:      "server 端到端扩展性测试源",
		Capabilities: []plugin.Capability{plugin.CapabilityResponsesPassthrough},
		Streaming:    plugin.StreamingPassthrough,
		Schema: []plugin.Field{
			{
				Name: "token", Label: "Token", Type: plugin.FieldTypePassword,
				Required: true, Sensitive: true, Target: plugin.FieldTargetOption,
			},
		},
	}
}

func (e2eSourcePlugin) ValidateSource(src config.Source) error {
	token, _ := src.Options["token"].(string)
	if token == "" {
		return fmt.Errorf("e2e-source: missing required option token")
	}
	return nil
}

func (e2eSourcePlugin) Backend() plugin.Backend { return e2eSourceBackend{} }

// e2eSourceBackend 透传最小 Responses SSE 事件序列，并以 e2e-source 身份上报
// UpstreamEvent，验证观测身份随插件 ID 透传而非被共享核心改写。
type e2eSourceBackend struct{}

func (e2eSourceBackend) Execute(
	_ context.Context,
	_ []byte,
	src config.Source,
	_ *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(plugin.UpstreamEvent),
	_ int,
) error {
	events := []model.SSEEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp-e2e","status":"in_progress"}}`)},
		{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","item_id":"it_1","delta":"hi"}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp-e2e","status":"completed"}}`)},
	}
	for _, ev := range events {
		if err := onEvent(ev); err != nil {
			return err
		}
	}
	if onUpstream != nil {
		onUpstream(plugin.UpstreamEvent{
			SourceName: src.Name,
			Model:      "e2e-model",
			Status:     "completed",
			Backend:    "e2e-source",
		})
	}
	return nil
}

var _ plugin.SourcePlugin = e2eSourcePlugin{}

// newSrvExtensible 用内置插件叠加测试源构造 Server，证明新增源只改注册表即可接入。
func newSrvExtensible(cfg *config.Config) *Server {
	reg, err := plugin.New(append(assembly.Builtins(), e2eSourcePlugin{})...)
	if err != nil {
		panic(err)
	}
	return New(cfg, reg)
}

// TestServerRegistersExternalSourcePluginEndToEnd 验证 US4 端到端路径：
// 注册式 test source 经统一 /v1/responses 入口转发，SSE 流出 completed 终态，
// 且观测历史带该插件自身声明的 backend 身份。
func TestServerRegistersExternalSourcePluginEndToEnd(t *testing.T) {
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{
			Name:    "e2e",
			Backend: "e2e-source",
			Options: map[string]any{"token": "t1"},
		}},
	}
	srv := newSrvExtensible(cfg)
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"e2e-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("SSE 缺少 %s:\n%s", want, got)
		}
	}

	// 观测身份：recordUpstream 应把插件声明的 Backend 原样落到最近历史。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := srv.Metrics().Snapshot()
		for _, rec := range snap.Recent {
			if rec.Kind == "upstream" && rec.Backend == "e2e-source" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("metrics 未出现 e2e-source 身份的上游记录，观测身份被共享核心丢弃或改写")
}

// TestExternalSourceReloadRejectsUnknownBackend 验证热重载路径上插件级校验
// 生效：写入引用未注册 backend 的新配置时，Load 失败并保留旧配置。
func TestExternalSourceReloadRejectsUnknownBackend(t *testing.T) {
	reg, err := plugin.New(append(assembly.Builtins(), e2eSourcePlugin{})...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeE2EYAML(t, path, "e2e-source")

	cfg, err := config.LoadWithValidator(path, reg)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	holder := config.NewHolder(cfg)
	w, err := configwatch.New(path, holder, reg, nil, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// 改成未注册 backend：插件级校验应拒绝本次加载并保留旧配置。
	writeE2EYAML(t, path, "not-registered")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w.LastLoadErr() != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if w.LastLoadErr() == nil {
		t.Fatal("期望未知 backend 被拒绝，LastLoadErr 应为非空")
	}
	if holder.Current().Sources[0].Backend != "e2e-source" {
		t.Errorf("旧配置被覆盖: backend = %q, want e2e-source", holder.Current().Sources[0].Backend)
	}
}

func writeE2EYAML(t *testing.T, path, backend string) {
	t.Helper()
	data := []byte("server:\n  listen: :9999\nsources:\n  - name: e2e\n    backend: " +
		backend + "\n    options:\n      token: t1\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
