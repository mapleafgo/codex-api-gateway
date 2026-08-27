package copilot

// Device Flow 授权管理器：维护进程内唯一活跃会话，成功后通过注入的配置写回调
// 把 token 落盘并触发热重载。全部逻辑归属本包，共享 admin 不感知 Copilot 细节。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

const (
	authStateIdle         = "idle"
	authStateStarting     = "starting"
	authStateAwaitingUser = "awaiting_user"
	authStateSaving       = "saving"
	authStateAuthorized   = "authorized"
	authStateCancelled    = "cancelled"
	authStateError        = "error"
)

const (
	authDeviceCodeTimeout = 10 * time.Second
	authMinInterval       = time.Second
)

func authStateActive(state string) bool {
	switch state {
	case authStateStarting, authStateAwaitingUser, authStateSaving:
		return true
	}
	return false
}

// authHTTPError 携带建议的 HTTP 状态码。
type authHTTPError struct {
	status int
	msg    string
}

func (e *authHTTPError) Error() string { return e.msg }

func newAuthHTTPError(status int, msg string) *authHTTPError {
	return &authHTTPError{status: status, msg: msg}
}

// authStatus 是暴露给管理端的公开状态视图。device code 与 access token
// 永远不会进入这个结构。
type authStatus struct {
	State           string `json:"state"`
	UserCode        string `json:"user_code,omitempty"`
	VerificationURI string `json:"verification_uri,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
	SourceName      string `json:"source_name,omitempty"`
	Error           string `json:"error,omitempty"`
}

// authSession 表示一次进行中或已结束的授权尝试。
type authSession struct {
	id         uint64
	state      string
	targetName string
	draft      config.Source
	flow       *DeviceFlow
	interval   time.Duration
	publicErr  string
	ctx        context.Context
	cancel     context.CancelFunc
	cancelOnce sync.Once
	done       chan struct{}
}

// authManager 维护唯一活跃授权会话，负责在授权成功后落盘。
type authManager struct {
	client   *AuthClient
	snapshot func() *config.Config
	write    func(*config.Config) error

	mu          sync.Mutex
	nextID      uint64
	active      *authSession
	minInterval time.Duration
}

func newAuthManager(client *AuthClient, cb plugin.AdminCallbacks) *authManager {
	if client == nil {
		client = NewAuthClient(nil, "", "")
	}
	return &authManager{
		client:      client,
		snapshot:    cb.Snapshot,
		write:       cb.Write,
		minInterval: authMinInterval,
	}
}

func (m *authManager) setCallbacks(cb plugin.AdminCallbacks) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot = cb.Snapshot
	m.write = cb.Write
}

func (m *authManager) start(ctx context.Context, draft config.Source) (authStatus, error) {
	m.mu.Lock()
	if s := m.active; s != nil && authStateActive(s.state) {
		st := m.publicStatusLocked()
		m.mu.Unlock()
		return st, newAuthHTTPError(http.StatusConflict, "an authorization flow is already active")
	}
	if err := m.checkTargetConflict(draft.Name); err != nil {
		m.mu.Unlock()
		return authStatus{}, err
	}
	s := &authSession{
		id:         m.nextID + 1,
		state:      authStateStarting,
		targetName: draft.Name,
		draft:      draft,
		interval:   m.minInterval,
		done:       make(chan struct{}),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	m.nextID = s.id
	m.active = s
	m.mu.Unlock()

	codeCtx, codeCancel := context.WithTimeout(ctx, authDeviceCodeTimeout)
	defer codeCancel()
	flow, err := m.client.StartDeviceFlow(codeCtx)
	if err != nil {
		m.finish(s, authStateError, sanitizeAuthError(err))
		close(s.done)
		return m.status(), nil
	}

	m.mu.Lock()
	if a := m.active; a != s || s.state != authStateStarting {
		m.mu.Unlock()
		return m.status(), nil
	}
	s.flow = flow
	s.interval = max(flow.Interval, m.minInterval)
	s.state = authStateAwaitingUser
	st := m.publicStatusLocked()
	go m.poll(s)
	m.mu.Unlock()
	return st, nil
}

func (m *authManager) status() authStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.publicStatusLocked()
}

func (m *authManager) cancel() (authStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.active
	if s == nil {
		return authStatus{State: authStateIdle}, nil
	}
	switch s.state {
	case authStateSaving:
		return m.publicStatusLocked(), newAuthHTTPError(http.StatusConflict, "authorization is already saving")
	case authStateStarting, authStateAwaitingUser:
		s.state = authStateCancelled
		s.cancelOnce.Do(func() {
			if s.cancel != nil {
				s.cancel()
			}
		})
		return m.publicStatusLocked(), nil
	default:
		return m.publicStatusLocked(), nil
	}
}

func (m *authManager) publicStatusLocked() authStatus {
	s := m.active
	if s == nil {
		return authStatus{State: authStateIdle}
	}
	out := authStatus{
		State:      s.state,
		SourceName: s.targetName,
		Error:      s.publicErr,
	}
	if s.state == authStateAwaitingUser && s.flow != nil {
		out.UserCode = s.flow.UserCode
		out.VerificationURI = s.flow.VerificationURI
		if s.interval > 0 {
			out.IntervalSeconds = int((s.interval + 999*time.Millisecond) / time.Second)
		}
	}
	return out
}

func (m *authManager) checkTargetConflict(name string) error {
	if m.snapshot == nil {
		return nil
	}
	cfg := m.snapshot()
	if cfg == nil {
		return nil
	}
	for i := range cfg.Sources {
		if cfg.Sources[i].Name == name && cfg.Sources[i].Backend != plugin.BackendGitHubCopilot {
			return newAuthHTTPError(http.StatusConflict, fmt.Sprintf("source %q already exists and is not a Copilot source", name))
		}
	}
	return nil
}

func (m *authManager) poll(s *authSession) {
	defer close(s.done)
	for {
		if !m.waitInterval(s) {
			return
		}
		token, nextInterval, err := m.client.PollDeviceFlow(s.ctx, s.flow)
		if err != nil {
			m.finish(s, authStateError, sanitizeAuthError(err))
			return
		}
		if token == "" {
			m.mu.Lock()
			if nextInterval > 0 {
				s.interval = max(nextInterval, m.minInterval)
			}
			m.mu.Unlock()
			continue
		}
		m.complete(s, token)
		return
	}
}

func (m *authManager) waitInterval(s *authSession) bool {
	m.mu.Lock()
	interval := s.interval
	m.mu.Unlock()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (m *authManager) finish(s *authSession, state, publicErr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.active; a == s && authStateActive(a.state) {
		a.state = state
		a.publicErr = publicErr
		a.cancelOnce.Do(func() {
			if a.cancel != nil {
				a.cancel()
			}
		})
	}
}

func (m *authManager) complete(s *authSession, token string) {
	m.mu.Lock()
	if a := m.active; a != s || s.state != authStateAwaitingUser {
		m.mu.Unlock()
		return
	}
	s.state = authStateSaving
	draft := s.draft
	target := s.targetName
	m.mu.Unlock()

	if err := m.saveToken(token, draft); err != nil {
		m.finish(s, authStateError, sanitizeAuthError(err))
		slog.Warn("管理页 Copilot 授权保存失败", "source", target, "error", err.Error())
		return
	}
	m.mu.Lock()
	if a := m.active; a == s && s.state == authStateSaving {
		s.state = authStateAuthorized
		s.publicErr = ""
	}
	m.mu.Unlock()
	slog.Info("管理页 Copilot 授权成功", "source", target)
}

// saveToken 把 Device Flow 得到的 token 写入目标源并落盘。
func (m *authManager) saveToken(token string, draft config.Source) error {
	if m.snapshot == nil || m.write == nil {
		return fmt.Errorf("copilot: config callbacks not injected")
	}
	if draft.Options == nil {
		draft.Options = map[string]any{}
	}
	draft.Options["github_token"] = token
	cur := m.snapshot()
	if cur == nil {
		return fmt.Errorf("copilot: config snapshot unavailable")
	}
	next := *cur
	next.Sources = make([]config.Source, len(cur.Sources))
	copy(next.Sources, cur.Sources)
	idx := -1
	for i := range next.Sources {
		if next.Sources[i].Name == draft.Name {
			idx = i
			break
		}
	}
	if idx >= 0 {
		if next.Sources[idx].Backend != plugin.BackendGitHubCopilot {
			return fmt.Errorf("source %q is not a Copilot source", draft.Name)
		}
		next.Sources[idx] = draft
	} else {
		next.Sources = append(next.Sources, draft)
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	return m.write(&next)
}

// credentialRedact 兜底脱敏：即便上游错误文本携带凭据片段也先脱敏。
var credentialRedact = regexp.MustCompile(
	`(?i)\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_\-]+|\b(?:device_code|access_token)[=:][^&\s]+`)

func sanitizeAuthError(err error) string {
	msg := strings.TrimSpace(err.Error())
	msg = credentialRedact.ReplaceAllString(msg, "[redacted]")
	if msg == "" {
		return "unknown authorization error"
	}
	return msg
}

// deviceFlowDraft 是前端 POST /admin/api/copilot/auth/start 请求体中的 source 字段。
type deviceFlowDraft struct {
	Name              string            `json:"name"`
	BaseURL           string            `json:"base_url"`
	APIKey            string            `json:"api_key"`
	BackendType       string            `json:"backend_type"`
	GithubToken       string            `json:"github_token"`
	ModelMap          map[string]string `json:"model_map"`
	DefaultModel      string            `json:"default_model"`
	Disabled          bool              `json:"disabled"`
	Headers           map[string]string `json:"headers"`
	SupportsWebSearch *bool             `json:"supports_web_search,omitempty"`
}

// buildDraft 校验并规范化授权目标源草稿。github_token 必须为空：
// 凭据只能由 Device Flow 获得，禁止管理端手动注入。
func (d deviceFlowDraft) buildDraft() (config.Source, error) {
	name := strings.TrimSpace(d.Name)
	if name == "" {
		return config.Source{}, errors.New("missing source name")
	}
	norm, err := config.NormalizeBackendType(d.BackendType)
	if err != nil {
		return config.Source{}, err
	}
	if norm != config.BackendGitHubCopilot {
		return config.Source{}, errors.New("source must be backend_type=g")
	}
	if strings.TrimSpace(d.GithubToken) != "" {
		return config.Source{}, errors.New("github_token must be empty for device flow")
	}
	return config.Source{
		Name:              name,
		Backend:           plugin.BackendGitHubCopilot,
		BaseURL:           strings.TrimSpace(d.BaseURL),
		APIKey:            strings.TrimSpace(d.APIKey),
		ModelMap:          d.ModelMap,
		DefaultModel:      strings.TrimSpace(d.DefaultModel),
		Disabled:          d.Disabled,
		Headers:           plugin.SanitizeSourceHeaders(d.Headers),
		SupportsWebSearch: d.SupportsWebSearch,
	}, nil
}

// invokeDeviceFlow 是 AdminExtension.InvokeAction 对 device-flow 动作的分发。
func (p *Plugin) invokeDeviceFlow(ctx context.Context, req plugin.ActionRequest) (plugin.ActionResult, error) {
	switch req.RouteID {
	case "start":
		return p.authStart(ctx, req)
	case "status":
		return plugin.ActionResult{Code: http.StatusOK, Data: p.auth.status()}, nil
	case "cancel":
		status, err := p.auth.cancel()
		if err != nil {
			var he *authHTTPError
			if errors.As(err, &he) {
				return plugin.ActionResult{Code: he.status, Error: he.msg}, nil
			}
			return plugin.ActionResult{Code: http.StatusInternalServerError, Error: "cancel authorization failed"}, nil
		}
		return plugin.ActionResult{Code: http.StatusOK, Data: status}, nil
	}
	return plugin.ActionResult{Code: http.StatusNotFound, Error: "unknown route"}, nil
}

func (p *Plugin) authStart(ctx context.Context, req plugin.ActionRequest) (plugin.ActionResult, error) {
	if req.Method != http.MethodPost {
		return plugin.ActionResult{Code: http.StatusMethodNotAllowed, Error: "method not allowed"}, nil
	}
	var in struct {
		Source deviceFlowDraft `json:"source"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return plugin.ActionResult{Code: http.StatusBadRequest, Error: "invalid json", Message: err.Error()}, nil
	}
	draft, err := in.Source.buildDraft()
	if err != nil {
		return plugin.ActionResult{Code: http.StatusBadRequest, Error: err.Error()}, nil
	}
	status, err := p.auth.start(ctx, draft)
	if err != nil {
		var he *authHTTPError
		if errors.As(err, &he) {
			if he.status == http.StatusConflict && status.State != "" && status.State != authStateIdle {
				raw, _ := json.Marshal(status)
				var body map[string]any
				_ = json.Unmarshal(raw, &body)
				body["error"] = he.msg
				return plugin.ActionResult{Code: he.status, Data: body}, nil
			}
			return plugin.ActionResult{Code: he.status, Error: he.msg}, nil
		}
		return plugin.ActionResult{Code: http.StatusInternalServerError, Error: "start authorization failed"}, nil
	}
	return plugin.ActionResult{Code: http.StatusOK, Data: status}, nil
}
