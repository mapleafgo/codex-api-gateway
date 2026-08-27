package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// newTestPlugin 构造带注入 GitHub mock 的 Plugin，轮询间隔压到毫秒级。
func newTestPlugin(t *testing.T, gh http.HandlerFunc) *Plugin {
	t.Helper()
	p := New()
	if gh != nil {
		srv := httptest.NewServer(gh)
		t.Cleanup(srv.Close)
		p.auth = newAuthManager(
			NewAuthClient(srv.Client(), srv.URL+"/login/device/code", srv.URL+"/token"),
			plugin.AdminCallbacks{},
		)
	}
	p.auth.minInterval = 10 * time.Millisecond
	return p
}

func invokeStart(t *testing.T, p *Plugin, body string) (plugin.ActionResult, authStatus) {
	t.Helper()
	res, _ := p.InvokeAction(context.Background(), plugin.ActionRequest{
		ActionID: "device-flow",
		RouteID:  "start",
		Method:   http.MethodPost,
		Body:     []byte(body),
	})
	var st authStatus
	if res.Data != nil {
		raw, _ := json.Marshal(res.Data)
		_ = json.Unmarshal(raw, &st)
	}
	return res, st
}

func invokeStatus(p *Plugin) authStatus {
	res, _ := p.InvokeAction(context.Background(), plugin.ActionRequest{
		ActionID: "device-flow",
		RouteID:  "status",
		Method:   http.MethodGet,
	})
	var st authStatus
	if res.Data != nil {
		raw, _ := json.Marshal(res.Data)
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

func invokeCancel(p *Plugin) (plugin.ActionResult, authStatus) {
	res, _ := p.InvokeAction(context.Background(), plugin.ActionRequest{
		ActionID: "device-flow",
		RouteID:  "cancel",
		Method:   http.MethodPost,
	})
	var st authStatus
	if res.Data != nil {
		raw, _ := json.Marshal(res.Data)
		_ = json.Unmarshal(raw, &st)
	}
	return res, st
}

func waitAuthState(t *testing.T, p *Plugin, state string) authStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := invokeStatus(p); st.State == state {
			return st
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("auth state did not reach %q", state)
	return authStatus{}
}

func waitManagerState(t *testing.T, p *Plugin, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.auth.mu.Lock()
		var cur string
		if p.auth.active != nil {
			cur = p.auth.active.state
		}
		p.auth.mu.Unlock()
		if cur == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("manager state did not reach %q", state)
}

func noopCallbacks() plugin.AdminCallbacks {
	return plugin.AdminCallbacks{
		Snapshot: func() *config.Config { return &config.Config{} },
		Write:    func(c *config.Config) error { return nil },
	}
}

func TestDeviceFlowRejectsManualToken(t *testing.T) {
	p := newTestPlugin(t, nil)
	res, _ := invokeStart(t, p, `{"source":{"name":"copilot","backend_type":"g","github_token":"ghp_manual"}}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
}

func TestDeviceFlowRejectsNonCopilotBackend(t *testing.T) {
	p := newTestPlugin(t, nil)
	res, _ := invokeStart(t, p, `{"source":{"name":"s1","backend_type":"a","base_url":"https://example.com"}}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
}

func TestDeviceFlowRejectsMissingName(t *testing.T) {
	p := newTestPlugin(t, nil)
	res, _ := invokeStart(t, p, `{"source":{"name":"  ","backend_type":"g"}}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
}

func TestDeviceFlowStatusAwaitingUser(t *testing.T) {
	p := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":5}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	res, st := invokeStart(t, p, `{"source":{"name":"copilot","backend_type":"g"}}`)
	if res.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", res.Code)
	}
	got := waitAuthState(t, p, "awaiting_user")
	if st.UserCode == "" || st.VerificationURI == "" {
		t.Fatalf("start missing user code/uri: %+v", st)
	}
	if got.UserCode != "ABCD-1234" || got.VerificationURI != "https://github.com/login/device" {
		t.Fatalf("status user code/uri: %+v", got)
	}
	if got.IntervalSeconds <= 0 {
		t.Fatalf("interval_seconds = %d", got.IntervalSeconds)
	}
	raw, _ := json.Marshal(res.Data)
	for _, secret := range []string{"dc-1", "device_code"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("start response leaks %q", secret)
		}
	}
	_, _ = invokeCancel(p)
}

func TestDeviceFlowCancel(t *testing.T) {
	p := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	if res, _ := invokeStart(t, p, `{"source":{"name":"cancel-me","backend_type":"g"}}`); res.Code != http.StatusOK {
		t.Fatalf("start = %d", res.Code)
	}
	_ = waitAuthState(t, p, "awaiting_user")
	res, st := invokeCancel(p)
	if res.Code != http.StatusOK || st.State != "cancelled" {
		t.Fatalf("cancel: code=%d state=%q", res.Code, st.State)
	}
	if res, _ := invokeStart(t, p, `{"source":{"name":"again","backend_type":"g"}}`); res.Code != http.StatusOK {
		t.Fatalf("restart = %d", res.Code)
	}
	_, _ = invokeCancel(p)
}

func TestDeviceFlowSuccessSavesNewSource(t *testing.T) {
	p := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_newtoken"}`))
	}))
	var saved atomic.Value
	p.InjectCallbacks(plugin.AdminCallbacks{
		Snapshot: func() *config.Config {
			if v := saved.Load(); v != nil {
				return v.(*config.Config)
			}
			return &config.Config{}
		},
		Write: func(c *config.Config) error {
			saved.Store(c)
			return nil
		},
	})
	res, _ := invokeStart(t, p, `{"source":{"name":"copilot","backend_type":"g","default_model":"gpt-5.3-codex"}}`)
	if res.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", res.Code)
	}
	st := waitAuthState(t, p, "authorized")
	if st.State != "authorized" {
		t.Fatalf("state = %q", st.State)
	}
	cfg := saved.Load().(*config.Config)
	if len(cfg.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(cfg.Sources))
	}
	got := cfg.Sources[0]
	if got.Name != "copilot" || got.Backend != plugin.BackendGitHubCopilot {
		t.Fatalf("saved source = %+v", got)
	}
	if tok, _ := got.Options["github_token"].(string); tok != "ghu_newtoken" {
		t.Fatalf("saved options github_token = %q, want ghu_newtoken", tok)
	}
}

func TestDeviceFlowPendingThenSuccess(t *testing.T) {
	var calls int
	p := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_after_pending"}`))
	}))
	p.InjectCallbacks(noopCallbacks())
	invokeStart(t, p, `{"source":{"name":"pending","backend_type":"g"}}`)
	if st := waitAuthState(t, p, "authorized"); st.State != "authorized" {
		t.Fatalf("pending flow state = %q", st.State)
	}
}

func TestDeviceFlowStatusNeverExposesToken(t *testing.T) {
	p := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_topsecret"}`))
	}))
	p.InjectCallbacks(noopCallbacks())
	invokeStart(t, p, `{"source":{"name":"secret-src","backend_type":"g"}}`)
	_ = waitAuthState(t, p, "authorized")
	st := invokeStatus(p)
	raw, _ := json.Marshal(st)
	for _, secret := range []string{"ghu_topsecret", "dc-1", "device_code", "access_token"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("status response leaks %q", secret)
		}
	}
	if st.State != "authorized" {
		t.Fatalf("state = %q", st.State)
	}
}

func TestDeviceFlowIdleStatus(t *testing.T) {
	p := newTestPlugin(t, nil)
	if st := invokeStatus(p); st.State != "idle" {
		t.Fatalf("idle state = %q", st.State)
	}
}

func TestDeviceFlowSaveFailureErrorState(t *testing.T) {
	p := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_savefail"}`))
	}))
	p.InjectCallbacks(plugin.AdminCallbacks{
		Snapshot: func() *config.Config { return &config.Config{} },
		Write:    func(c *config.Config) error { return errors.New("disk full") },
	})
	invokeStart(t, p, `{"source":{"name":"savefail","backend_type":"g"}}`)
	st := waitAuthState(t, p, "error")
	if !strings.Contains(st.Error, "disk full") {
		t.Fatalf("save failure error = %q", st.Error)
	}
}

func TestDeviceFlowDenied(t *testing.T) {
	p := newTestPlugin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}))
	p.InjectCallbacks(noopCallbacks())
	invokeStart(t, p, `{"source":{"name":"denied","backend_type":"g"}}`)
	if st := waitAuthState(t, p, "error"); !strings.Contains(st.Error, "cancelled") {
		t.Fatalf("denied error = %q", st.Error)
	}
}
