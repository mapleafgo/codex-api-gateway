package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/copilotclient"
)

// newTestAuthHandler 构造带注入 GitHub mock 的管理 handler，轮询间隔压到毫秒级。
func newTestAuthHandler(t *testing.T, gh http.HandlerFunc) (*handler, *Deps) {
	t.Helper()
	deps, _ := newTestDeps(t)
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	authClient := copilotclient.NewAuthClient(srv.Client(), srv.URL+"/login/device/code", srv.URL+"/token")
	h := &handler{deps: *deps, copilot: copilotclient.New()}
	h.auth = newCopilotAuthManager(
		authClient,
		func() *config.Config { return deps.Holder.Current() },
		h.saveCopilotSource,
	)
	h.auth.minInterval = 10 * time.Millisecond
	return h, deps
}

func authStartRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/admin/api/copilot/auth/start", strings.NewReader(body))
}

func authStatusRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/admin/api/copilot/auth/status", nil)
}

func authCancelRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/admin/api/copilot/auth/cancel", nil)
}

func doStart(t *testing.T, h *handler, body string) (*httptest.ResponseRecorder, copilotAuthStatus) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleCopilotAuthStart(rec, authStartRequest(t, body))
	var st copilotAuthStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil && rec.Body.Len() > 0 && rec.Code == http.StatusOK {
		t.Fatalf("json decode: %v (%s)", err, rec.Body.String())
	}
	return rec, st
}

func doStatus(h *handler) copilotAuthStatus {
	rec := httptest.NewRecorder()
	h.handleCopilotAuthStatus(rec, authStatusRequest())
	var st copilotAuthStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	return st
}

func doCancel(h *handler) (*httptest.ResponseRecorder, copilotAuthStatus) {
	rec := httptest.NewRecorder()
	h.handleCopilotAuthCancel(rec, authCancelRequest())
	var st copilotAuthStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	return rec, st
}

// waitAuth 轮询公开状态直到进入 state（测试期轮询间隔已是毫秒级）。
func waitAuth(t *testing.T, h *handler, state string) copilotAuthStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := doStatus(h); st.State == state {
			return st
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("auth state did not reach %q", state)
	return copilotAuthStatus{}
}

func TestCopilotAuthStartRejectsManualToken(t *testing.T) {
	h, _ := newTestAuthHandler(t, nil)
	rec, _ := doStart(t, h, `{"source":{"name":"copilot","backend_type":"g","github_token":"ghp_manual"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCopilotAuthStartRejectsNonCopilotBackend(t *testing.T) {
	h, _ := newTestAuthHandler(t, nil)
	rec, _ := doStart(t, h, `{"source":{"name":"s1","backend_type":"a","base_url":"https://example.com"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCopilotAuthStartRejectsMissingName(t *testing.T) {
	h, _ := newTestAuthHandler(t, nil)
	rec, _ := doStart(t, h, `{"source":{"name":"  ","backend_type":"g"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCopilotAuthStartRejectsNonGCollision(t *testing.T) {
	h, _ := newTestAuthHandler(t, nil)
	// newTestDeps 的初始配置里 s1 是 a 类型
	rec, _ := doStart(t, h, `{"source":{"name":"s1","backend_type":"g"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestCopilotAuthSingleActiveSession(t *testing.T) {
	release := make(chan struct{})
	h, _ := newTestAuthHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			<-release // 第一个会话停留在 starting
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":5}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))

	firstDone := make(chan struct{})
	go func() {
		doStart(t, h, `{"source":{"name":"a1","backend_type":"g"}}`)
		close(firstDone)
	}()
	// 等第一个会话进入 starting，再发起第二个 start
	waitManagerState(t, h, "starting")
	rec2, active := doStart(t, h, `{"source":{"name":"a2","backend_type":"g"}}`)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second start status = %d, want 409", rec2.Code)
	}
	// 契约：409 响应体必须携带当前活跃会话的公开状态
	if active.State != "starting" || active.SourceName != "a1" {
		t.Fatalf("conflict body status = %+v, want active starting session", active)
	}
	close(release)
	<-firstDone
	_ = waitAuth(t, h, "awaiting_user")
	_, _ = doCancel(h)
}

func waitManagerState(t *testing.T, h *handler, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.auth.mu.Lock()
		var cur string
		if h.auth.active != nil {
			cur = h.auth.active.state
		}
		h.auth.mu.Unlock()
		if cur == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("manager state did not reach %q", state)
}

func TestCopilotAuthStatusAwaitingUser(t *testing.T) {
	h, _ := newTestAuthHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":5}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	rec, st := doStart(t, h, `{"source":{"name":"copilot","backend_type":"g"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", rec.Code)
	}
	got := waitAuth(t, h, "awaiting_user")
	if st.UserCode == "" || st.VerificationURI == "" {
		t.Fatalf("start missing user code/uri: %+v", st)
	}
	if got.UserCode != "ABCD-1234" || got.VerificationURI != "https://github.com/login/device" {
		t.Fatalf("status user code/uri: %+v", got)
	}
	if got.IntervalSeconds <= 0 {
		t.Fatalf("interval_seconds = %d", got.IntervalSeconds)
	}
	if got.SourceName != "copilot" {
		t.Fatalf("source_name = %q", got.SourceName)
	}
	raw := rec.Body.String()
	for _, secret := range []string{"dc-1", "device_code"} {
		if strings.Contains(raw, secret) {
			t.Errorf("start response leaks %q", secret)
		}
	}
	_, _ = doCancel(h)
}

func TestCopilotAuthSuccessSavesNewSource(t *testing.T) {
	h, deps := newTestAuthHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_newtoken"}`))
	}))
	rec, _ := doStart(t, h, `{"source":{"name":"copilot","backend_type":"g","default_model":"gpt-5.3-codex"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", rec.Code)
	}
	st := waitAuth(t, h, "authorized")
	if st.State != "authorized" {
		t.Fatalf("state = %q", st.State)
	}
	cfg := deps.Holder.Current()
	if len(cfg.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(cfg.Sources))
	}
	got := cfg.Sources[1]
	if got.Name != "copilot" || got.BackendType != config.BackendGitHubCopilot {
		t.Fatalf("saved source = %+v", got)
	}
	if got.GithubToken != "ghu_newtoken" {
		t.Fatalf("saved token = %q", got.GithubToken)
	}
}

func TestCopilotAuthSuccessUpdatesExistingSource(t *testing.T) {
	deps, _ := newTestDeps(t)
	// 先把一个 g 源写进 holder 与磁盘
	cur := *deps.Holder.Current()
	cur.Sources = append(cur.Sources, config.Source{
		Name: "copilot", BackendType: config.BackendGitHubCopilot,
		GithubToken: "ghu_old", DefaultModel: "gpt-4o", BaseURL: "https://api.githubcopilot.com",
	})
	if err := cur.Validate(); err != nil {
		t.Fatalf("seed validate: %v", err)
	}
	deps.Holder.Replace(&cur)
	if err := writeInitialYAML(deps.CfgPath, &cur); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	h := &handler{deps: *deps, copilot: copilotclient.New()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_newtoken"}`))
	}))
	defer srv.Close()
	h.auth = newCopilotAuthManager(
		copilotclient.NewAuthClient(srv.Client(), srv.URL+"/login/device/code", srv.URL+"/token"),
		func() *config.Config { return deps.Holder.Current() },
		h.saveCopilotSource,
	)
	h.auth.minInterval = 10 * time.Millisecond

	rec, _ := doStart(t, h, `{"source":{"name":"copilot","backend_type":"g","default_model":"gpt-5.3-codex"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", rec.Code)
	}
	_ = waitAuth(t, h, "authorized")
	cfg := deps.Holder.Current()
	if len(cfg.Sources) != 2 {
		t.Fatalf("sources = %d, want 2 (unchanged count)", len(cfg.Sources))
	}
	got := cfg.Sources[1]
	if got.Name != "copilot" || got.DefaultModel != "gpt-5.3-codex" || got.GithubToken != "ghu_newtoken" {
		t.Fatalf("updated source = %+v", got)
	}
}

func TestCopilotAuthCancel(t *testing.T) {
	h, _ := newTestAuthHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	if rec, _ := doStart(t, h, `{"source":{"name":"cancel-me","backend_type":"g"}}`); rec.Code != http.StatusOK {
		t.Fatalf("start = %d", rec.Code)
	}
	_ = waitAuth(t, h, "awaiting_user")
	rec, st := doCancel(h)
	if rec.Code != http.StatusOK || st.State != "cancelled" {
		t.Fatalf("cancel: code=%d state=%q", rec.Code, st.State)
	}
	// 取消后再起新会话成功
	if rec, _ := doStart(t, h, `{"source":{"name":"again","backend_type":"g"}}`); rec.Code != http.StatusOK {
		t.Fatalf("restart = %d", rec.Code)
	}
}

func TestCopilotAuthCancelDuringSavingConflict(t *testing.T) {
	h, _ := newTestAuthHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_conflict"}`))
	}))
	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	h.auth.save = func(token string, draft config.Source) error {
		close(saveStarted)
		<-releaseSave
		return nil
	}
	if rec, _ := doStart(t, h, `{"source":{"name":"conflict","backend_type":"g"}}`); rec.Code != http.StatusOK {
		t.Fatalf("start = %d", rec.Code)
	}
	select {
	case <-saveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("save did not start")
	}
	rec, _ := doCancel(h)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel during saving = %d, want 409", rec.Code)
	}
	close(releaseSave)
	_ = waitAuth(t, h, "authorized")
}

func TestCopilotAuthFailureKeepsExistingSource(t *testing.T) {
	deps, _ := newTestDeps(t)
	cur := *deps.Holder.Current()
	cur.Sources = append(cur.Sources, config.Source{
		Name: "existing-copilot", BackendType: config.BackendGitHubCopilot, GithubToken: "ghu_old",
	})
	if err := cur.Validate(); err != nil {
		t.Fatalf("seed validate: %v", err)
	}
	deps.Holder.Replace(&cur)
	if err := writeInitialYAML(deps.CfgPath, &cur); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}
	h := &handler{deps: *deps, copilot: copilotclient.New()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"expired_token"}`))
	}))
	defer srv.Close()
	h.auth = newCopilotAuthManager(
		copilotclient.NewAuthClient(srv.Client(), srv.URL+"/login/device/code", srv.URL+"/token"),
		func() *config.Config { return deps.Holder.Current() },
		h.saveCopilotSource,
	)
	h.auth.minInterval = 10 * time.Millisecond

	if rec, _ := doStart(t, h, `{"source":{"name":"existing-copilot","backend_type":"g"}}`); rec.Code != http.StatusOK {
		t.Fatalf("start = %d", rec.Code)
	}
	st := waitAuth(t, h, "error")
	if st.Error == "" {
		t.Fatal("expected public error")
	}
	if got := deps.Holder.Current().Sources[1].GithubToken; got != "ghu_old" {
		t.Fatalf("old token changed to %q", got)
	}
}

func TestCopilotAuthSaveFailureKeepsSources(t *testing.T) {
	h, deps := newTestAuthHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_savefail"}`))
	}))
	h.auth.save = func(token string, draft config.Source) error {
		return errors.New("disk full")
	}
	if rec, _ := doStart(t, h, `{"source":{"name":"savefail","backend_type":"g"}}`); rec.Code != http.StatusOK {
		t.Fatalf("start = %d", rec.Code)
	}
	st := waitAuth(t, h, "error")
	if !strings.Contains(st.Error, "disk full") {
		t.Fatalf("save failure error = %q", st.Error)
	}
	if got := deps.Holder.Current(); len(got.Sources) != 1 {
		t.Fatalf("sources = %d, want 1 (unchanged)", len(got.Sources))
	}
	for _, s := range deps.Holder.Current().Sources {
		if strings.Contains(s.GithubToken, "ghu_savefail") {
			t.Errorf("token leaked into existing source %q", s.Name)
		}
	}
}

func TestCopilotAuthDenied(t *testing.T) {
	// access_denied -> error
	h, _ := newTestAuthHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}))
	doStart(t, h, `{"source":{"name":"denied","backend_type":"g"}}`)
	if st := waitAuth(t, h, "error"); !strings.Contains(st.Error, "cancelled") {
		t.Fatalf("denied error = %q", st.Error)
	}
}

func TestCopilotAuthPendingThenSuccess(t *testing.T) {
	// authorization_pending 后继续轮询直到成功
	var calls int
	h, _ := newTestAuthHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	doStart(t, h, `{"source":{"name":"pending","backend_type":"g"}}`)
	if st := waitAuth(t, h, "authorized"); st.State != "authorized" {
		t.Fatalf("pending flow state = %q", st.State)
	}
}

func TestCopilotAuthStatusNeverExposesToken(t *testing.T) {
	h, _ := newTestAuthHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/device/code" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ghu_topsecret"}`))
	}))
	doStart(t, h, `{"source":{"name":"secret-src","backend_type":"g"}}`)
	_ = waitAuth(t, h, "authorized")
	rec := httptest.NewRecorder()
	h.handleCopilotAuthStatus(rec, authStatusRequest())
	raw := rec.Body.String()
	for _, secret := range []string{"ghu_topsecret", "dc-1", "device_code", "access_token"} {
		if strings.Contains(raw, secret) {
			t.Errorf("status response leaks %q", secret)
		}
	}
	st := doStatus(h)
	if st.State != "authorized" {
		t.Fatalf("state = %q", st.State)
	}
}

func TestCopilotAuthIdleStatus(t *testing.T) {
	h, _ := newTestAuthHandler(t, nil)
	if st := doStatus(h); st.State != "idle" {
		t.Fatalf("idle state = %q", st.State)
	}
}
