package admin

// Copilot Device Flow 授权只属于管理页旁路：这里维护进程内唯一活跃会话，
// 成功后通过 handler.saveCopilotSource 复用既有配置写盘与热重载链路。

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
	"github.com/mapleafgo/codex-api-gateway/internal/copilot"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// Device Flow 会话状态。idle 表示当前没有活跃或保留的会话。
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

// authHTTPError 携带建议的 HTTP 状态码，供 handler 直接映射响应。
type authHTTPError struct {
	status int
	msg    string
}

func (e *authHTTPError) Error() string { return e.msg }

func newAuthHTTPError(status int, msg string) *authHTTPError {
	return &authHTTPError{status: status, msg: msg}
}

// copilotAuthStatus 是暴露给管理端的公开状态视图。device code 与 access token
// 永远不会进入这个结构。
type copilotAuthStatus struct {
	State           string `json:"state"`
	UserCode        string `json:"user_code,omitempty"`
	VerificationURI string `json:"verification_uri,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
	SourceName      string `json:"source_name,omitempty"`
	Error           string `json:"error,omitempty"`
}

// authSession 表示一次进行中或已结束的授权尝试。所有可变字段都由 manager 的
// mu 保护；ctx 只用于停止轮询 goroutine。
type authSession struct {
	id         uint64
	state      string
	targetName string
	draft      config.Source // 目标源草稿，GithubToken 始终为空
	flow       *copilot.DeviceFlow
	interval   time.Duration
	publicErr  string
	ctx        context.Context
	cancel     context.CancelFunc
	cancelOnce sync.Once
	done       chan struct{}
}

// copilotAuthManager 维护唯一活跃授权会话，并负责在授权成功后落盘。
type copilotAuthManager struct {
	client *copilot.AuthClient
	// snapshot 返回最新配置快照，用于同名目标冲突检查（FR-007）。
	snapshot func() *config.Config
	// save 在授权成功后写入目标源并热重载；返回错误时进入 error 终态。
	save func(token string, draft config.Source) error

	mu     sync.Mutex
	nextID uint64
	active *authSession
	// minInterval 是轮询间隔下限，通常 1s；测试可调小以加速。
	minInterval time.Duration
}

func newCopilotAuthManager(
	client *copilot.AuthClient,
	snapshot func() *config.Config,
	save func(token string, draft config.Source) error,
) *copilotAuthManager {
	if client == nil {
		client = copilot.NewAuthClient(nil, "", "")
	}
	return &copilotAuthManager{
		client:      client,
		snapshot:    snapshot,
		save:        save,
		minInterval: authMinInterval,
	}
}

func (m *copilotAuthManager) start(ctx context.Context, draft config.Source) (copilotAuthStatus, error) {
	m.mu.Lock()
	if s := m.active; s != nil && authStateActive(s.state) {
		st := m.publicStatusLocked()
		m.mu.Unlock()
		return st, newAuthHTTPError(http.StatusConflict, "an authorization flow is already active")
	}
	if err := m.checkTargetConflict(draft.Name); err != nil {
		m.mu.Unlock()
		return copilotAuthStatus{}, err
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
	// cancel 可能在 device-code 请求期间到达：只有仍是活跃 starting 才推进。
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

func (m *copilotAuthManager) status() copilotAuthStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.publicStatusLocked()
}

func (m *copilotAuthManager) cancel() (copilotAuthStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.active
	if s == nil {
		return copilotAuthStatus{State: authStateIdle}, nil
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

func (m *copilotAuthManager) publicStatusLocked() copilotAuthStatus {
	s := m.active
	if s == nil {
		return copilotAuthStatus{State: authStateIdle}
	}
	out := copilotAuthStatus{
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

func (m *copilotAuthManager) checkTargetConflict(name string) error {
	cfg := m.snapshot()
	if cfg == nil {
		return nil
	}
	for i := range cfg.Sources {
		if cfg.Sources[i].Name == name && cfg.Sources[i].BackendType != config.BackendGitHubCopilot {
			return newAuthHTTPError(http.StatusConflict, fmt.Sprintf("source %q already exists and is not a Copilot source", name))
		}
	}
	return nil
}

// poll 按 GitHub 指示的节奏轮询 access token，直到成功、取消或终态失败。
func (m *copilotAuthManager) poll(s *authSession) {
	defer close(s.done)
	for {
		if !m.waitInterval(s) {
			return // 被取消
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

// waitInterval 等待当前轮询间隔，期间可被 cancel 打断。返回 false 表示已取消。
func (m *copilotAuthManager) waitInterval(s *authSession) bool {
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

// finish 把活跃会话推进到终态。已处于终态的旧会话不会被覆盖。
func (m *copilotAuthManager) finish(s *authSession, state, publicErr string) {
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

func (m *copilotAuthManager) complete(s *authSession, token string) {
	m.mu.Lock()
	if a := m.active; a != s || s.state != authStateAwaitingUser {
		m.mu.Unlock()
		return
	}
	s.state = authStateSaving
	draft := s.draft
	target := s.targetName
	m.mu.Unlock()

	if err := m.save(token, draft); err != nil {
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

// credentialRedact 是兜底防线：即便上游错误文本意外携带凭据片段，也先脱敏再展示。
var credentialRedact = regexp.MustCompile(
	`(?i)\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_\-]+|\b(?:device_code|access_token)[=:][^&\s]+`)

// sanitizeAuthError 把内部错误转成可安全展示的文本。
func sanitizeAuthError(err error) string {
	msg := strings.TrimSpace(err.Error())
	msg = credentialRedact.ReplaceAllString(msg, "[redacted]")
	if msg == "" {
		return "unknown authorization error"
	}
	return msg
}

// handleCopilotAuthStart POST /admin/api/copilot/auth/start
func (h *handler) handleCopilotAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in struct {
		Source sourceView `json:"source"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminBodyLimit)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	draft, err := copilotSourceDraft(in.Source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	status, err := h.auth.start(r.Context(), draft)
	if err != nil {
		var he *authHTTPError
		if errors.As(err, &he) {
			if he.status == http.StatusConflict && status.State != "" && status.State != authStateIdle {
				// 冲突时同时返回当前公开状态，前端可直接续显已有会话。
				body := map[string]any{"error": he.msg}
				if raw, mErr := json.Marshal(status); mErr == nil {
					_ = json.Unmarshal(raw, &body)
				}
				writeJSON(w, he.status, body)
				return
			}
			writeJSON(w, he.status, errorBody{Error: he.msg})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "start authorization failed", Detail: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleCopilotAuthStatus GET /admin/api/copilot/auth/status
func (h *handler) handleCopilotAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, h.auth.status())
}

// handleCopilotAuthCancel POST /admin/api/copilot/auth/cancel
func (h *handler) handleCopilotAuthCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	status, err := h.auth.cancel()
	if err != nil {
		var he *authHTTPError
		if errors.As(err, &he) {
			writeJSON(w, he.status, errorBody{Error: he.msg})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "cancel authorization failed"})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// copilotSourceDraft 校验并规范化授权目标源草稿。github_token 必须为空：
// 凭据只能由 Device Flow 获得，禁止管理端手动注入。
func copilotSourceDraft(in sourceView) (config.Source, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return config.Source{}, errors.New("missing source name")
	}
	norm, err := config.NormalizeBackendType(in.BackendType)
	if err != nil {
		return config.Source{}, err
	}
	if norm != config.BackendGitHubCopilot {
		return config.Source{}, errors.New("source must be backend_type=g")
	}
	if strings.TrimSpace(in.GithubToken) != "" {
		return config.Source{}, errors.New("github_token must be empty for device flow")
	}
	headers := sanitizeSourceHeaders(in.Headers)
	src := config.Source{
		Name:              name,
		BaseURL:           strings.TrimSpace(in.BaseURL),
		APIKey:            strings.TrimSpace(in.APIKey),
		BackendType:       norm,
		ModelMap:          in.ModelMap,
		DefaultModel:      strings.TrimSpace(in.DefaultModel),
		Disabled:          in.Disabled,
		Headers:           headers,
		SupportsWebSearch: in.SupportsWebSearch,
	}
	if in.Breaker != nil {
		b := breakerViewToCfg(*in.Breaker)
		src.Breaker = &b
	}
	return src, nil
}

// saveCopilotSource 把 Device Flow 得到的 token 写入目标 g 源，复用既有配置
// 原子写盘与热重载链路。目标不存在时新增；同名 g 源更新凭据和草稿字段；
// 同名非 g 源在保存前再次拒绝（FR-007）。
func (h *handler) saveCopilotSource(token string, draft config.Source) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	cur := h.deps.Holder.Current()
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
	if draft.Options == nil {
		draft.Options = map[string]any{}
	}
	draft.Options["github_token"] = token
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
	return h.writeConfigYAMLLocked(&next)
}
