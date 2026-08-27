package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// fakeActionPlugin 是声明一个动作路由的测试插件，用于验证通用动作挂载与冲突检测。
type fakeActionPlugin struct {
	id     plugin.ID
	routes []plugin.ActionRoute
}

func (p *fakeActionPlugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:        p.id,
		Title:     string(p.id),
		Streaming: plugin.StreamingConverted,
		Actions: []plugin.Action{{
			ID: "action", Label: "Action", Routes: p.routes,
		}},
	}
}

func (*fakeActionPlugin) ValidateSource(config.Source) error { return nil }

func (*fakeActionPlugin) Backend() plugin.Backend { return fakeBackend{} }

func (*fakeActionPlugin) InvokeAction(context.Context, plugin.ActionRequest) (plugin.ActionResult, error) {
	return plugin.ActionResult{Code: http.StatusOK}, nil
}

type fakeBackend struct{}

func (fakeBackend) Execute(
	context.Context, []byte, config.Source, *config.Config,
	func(model.SSEEvent) error, func(plugin.UpstreamEvent), int,
) error {
	return nil
}

// TestActionRoutesMounted 验证插件声明的动作路由被逐条挂载到 mux，路径与 http
// method 保持精确匹配；自定义前缀尚未注册不会命中动作。
func TestActionRoutesMounted(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/admin/api/copilot/auth/start"},
		{http.MethodGet, "/admin/api/copilot/auth/status"},
		{http.MethodPost, "/admin/api/copilot/auth/cancel"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		_, pat := mux.Handler(req)
		if pat != tc.path {
			t.Fatalf("%s %s registered pattern=%q, want %q", tc.method, tc.path, pat, tc.path)
		}
	}
}

// TestActionWrongMethodReturns405 验证动作路由按声明方法匹配，错误方法返回 405。
func TestActionWrongMethodReturns405(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/copilot/auth/status", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// TestActionRouteConflictPanics 验证重复动作路径在挂载阶段直接 panic
// （组装期编程错误，不落入运行时分支）。
func TestActionRouteConflictPanics(t *testing.T) {
	shared := plugin.ActionRoute{ID: "start", Method: http.MethodPost, Path: "/admin/api/fake/shared"}
	pA := &fakeActionPlugin{id: "fake-a", routes: []plugin.ActionRoute{shared}}
	pB := &fakeActionPlugin{id: "fake-b", routes: []plugin.ActionRoute{shared}}
	reg, err := plugin.New(pA, pB)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("mountActions should panic on route conflict")
		}
	}()
	h := &handler{deps: Deps{Registry: reg}}
	h.mountActions(http.NewServeMux(), func(string, http.HandlerFunc) http.HandlerFunc { return func(http.ResponseWriter, *http.Request) {} })
}

// TestActionRoutesNoRegistry 验证 Registry 为 nil 时动作路由不挂载，也不 panic。
func TestActionRoutesNoRegistry(t *testing.T) {
	deps, _ := newTestDeps(t)
	deps.Registry = nil
	mux := http.NewServeMux()
	Mount(mux, *deps)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/copilot/auth/start", nil)
	_, pat := mux.Handler(req)
	if pat == "/admin/api/copilot/auth/start" {
		t.Fatal("copilot action route must not be mounted without registry")
	}
}

// TestActionUnknownActionIs404 验证插件对未知 action 返回 404（与共享路由 405 区分）。
func TestActionUnknownActionIs404(t *testing.T) {
	reg := newTestRegistry(t)
	p, ok := reg.Get(string(plugin.BackendGitHubCopilot))
	if !ok {
		t.Fatal("copilot plugin missing from test registry")
	}
	ext, ok := p.(plugin.AdminExtension)
	if !ok {
		t.Fatal("copilot plugin must implement AdminExtension")
	}
	res, err := ext.InvokeAction(context.Background(), plugin.ActionRequest{
		PluginID: string(plugin.BackendGitHubCopilot), ActionID: "not-exist", Method: http.MethodPost,
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", res.Code)
	}
}
