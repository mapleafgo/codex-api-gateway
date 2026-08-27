// Package admin 提供网关的 H5 管理页：挂载根路径返回单页前端，
// 以及 /admin/api/* 一组 JSON 接口用于读取指标、读取/修改配置。
//
// 与 API 隔离：所有 handler 在独立 goroutine 中由 HTTP server 调度，
// 且外层包了 recoverMiddleware，单次 panic 不会影响其他请求，
// 更不会影响 /v1/* 的转发路径。
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/copilot"
	"github.com/mapleafgo/codex-api-gateway/internal/health"
	"github.com/mapleafgo/codex-api-gateway/internal/metrics"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// SourceHealthView 是管理页展示的单源运行时回退等级。
type SourceHealthView struct {
	Name         string `json:"name"`
	State        string `json:"state"`         // normal | degraded | circuitOpen | halfOpen
	DegradeCount int    `json:"degrade_count"` // 0/1/2 量级
	Priority     int    `json:"priority"`      // 运行时优先级，1=最高
	Disabled     bool   `json:"disabled"`      // 配置级人工停用
}

// Deps 是 Mount 需要的依赖。main 组装时传入。
type Deps struct {
	Holder  *config.Holder
	Metrics *metrics.Collector
	CfgPath string // config.yaml 的绝对路径（用于写回）
	Version string // 构建版本号，由 CI 通过 -ldflags 注入
	// ReloadFromDisk 在写回 config.yaml 后调用：让 configwatch 重新 Load。
	// 必须同步完成——调用方（如 handleSetSourceDisabled）依赖 reload 后 holder 已更新。
	// 若 configwatch 未启用，传 nil 即可（写回不立即生效，需重启）。
	ReloadFromDisk func()
	// ModelsFetcher 按源名拉取上游 /v1/models 列表，供管理页编辑模型映射时选用。
	// 若未提供（nil），对应接口返回 501。
	ModelsFetcher func(ctx context.Context, sourceName string) ([]anthropic.ModelInfo, error)
	// ValidateConfig 在保存前做完整配置校验（含插件级 SourceValidator）。
	// nil 时只做基础字段校验（cfg.Validate）。返回错误时保存中止，旧配置继续服务。
	ValidateConfig func(*config.Config) error
	// SourceHealth 返回各源运行时健康态。nil 时 snapshot 不附带 sources_health。
	SourceHealth func() []SourceHealthView
	// PromoteSource 手动将源提升回 normal。nil 时 promote 接口 501。
	PromoteSource func(name string) error
	// SyncModelCatalog 在配置写盘并热重载成功后同步 $CODEX_HOME/models.json。
	// nil 时跳过；返回错误时接口报错但 config.yaml 已保存。
	SyncModelCatalog func() error
	// Registry 是已注册源插件注册表，供 health 探测按 backend 分发到插件 HealthProbe。
	// nil 时 health 退回旧 HTTP /v1/models 探针。
	Registry *plugin.Registry
}

type handler struct {
	deps Deps
	// copilot 只服务管理页旁路的目录/连通性探测，不进入 /v1/* 转发路径。
	// auth 唯一活跃 Copilot Device Flow 会话（管理页旁路）。
	auth *copilotAuthManager
	// writeMu 序列化配置写回，避免并发保存互相覆盖。
	writeMu sync.Mutex
}

// Mount 把管理页与 JSON API 挂载到 mux 的 / 与 /admin/api/* 路径。
// 已存在的 /v1/* 路由不受影响（由调用方先注册）。
func Mount(mux *http.ServeMux, deps Deps) {
	h := &handler{
		deps: deps,
	}
	h.auth = newCopilotAuthManager(
		copilot.NewAuthClient(nil, "", ""),
		func() *config.Config { return h.deps.Holder.Current() },
		h.saveCopilotSource,
	)
	// 用 recoverMiddleware 包装，handler 内 panic 不会拖垮整个进程。
	wrap := func(name string, fn http.HandlerFunc) http.HandlerFunc {
		return recoverMiddleware(name, fn)
	}
	mux.HandleFunc("/", wrap("index", h.handleIndex))
	mux.HandleFunc("/favicon.ico", wrap("favicon", h.handleFavicon))
	mux.HandleFunc("/admin/api/metrics", wrap("metrics", h.handleMetrics))
	mux.HandleFunc("/admin/api/config", wrap("config", h.handleConfig))
	mux.HandleFunc("/admin/api/config/reload", wrap("reload", h.handleReload))
	mux.HandleFunc("/admin/api/guidance", wrap("guidance", h.handleGuidance))
	mux.HandleFunc("/admin/api/events", wrap("events", h.handleEvents))
	mux.HandleFunc("/admin/api/models", wrap("models", h.handleModels))
	mux.HandleFunc("/admin/api/upstream-models", wrap("upstream-models", h.handleUpstreamModels))
	mux.HandleFunc("/admin/api/sources/promote", wrap("promote-source", h.handlePromoteSource))
	mux.HandleFunc("/admin/api/sources/disabled", wrap("source-disabled", h.handleSetSourceDisabled))
	mux.HandleFunc("/admin/api/sources/test", wrap("source-test", h.handleSourceTest))
	mux.HandleFunc("/admin/api/sources/reorder", wrap("source-reorder", h.handleReorderSources))
	mux.HandleFunc("/admin/api/sources", wrap("source-add", h.handleAddSource))
	mux.HandleFunc("/admin/api/sources/delete", wrap("source-delete", h.handleDeleteSource))
	mux.HandleFunc("/admin/api/copilot/auth/start", wrap("copilot-auth-start", h.handleCopilotAuthStart))
	mux.HandleFunc("/admin/api/copilot/auth/status", wrap("copilot-auth-status", h.handleCopilotAuthStatus))
	mux.HandleFunc("/admin/api/copilot/auth/cancel", wrap("copilot-auth-cancel", h.handleCopilotAuthCancel))
	mux.HandleFunc("/admin/api/models/reorder", wrap("model-reorder", h.handleReorderModels))
	mux.HandleFunc("/admin/api/models/add", wrap("model-add", h.handleAddModel))
	mux.HandleFunc("/admin/api/models/delete", wrap("model-delete", h.handleDeleteModel))
	mux.HandleFunc("/admin/api/version", wrap("version", h.handleVersion))
}

// recoverMiddleware 捕获 handler panic，记录日志后返回 500。
// 关键：panic 不会传播到上层 http server，避免影响其他请求。
func recoverMiddleware(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("管理接口 panic",
					"endpoint", name, "path", r.URL.Path, "method", r.Method,
					"recover", rec, "elapsed", time.Since(start).String())
				writeJSON(w, http.StatusInternalServerError, errorBody{
					Error: "internal panic", Detail: fmt.Sprintf("%v", rec),
				})
			}
		}()
		next(w, r)
	}
}

type errorBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

type okBody struct {
	OK bool `json:"ok"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("管理接口写 JSON 失败", "error", err)
	}
}

// handleIndex 在根路径返回 H5 单页（嵌入的 index.html）。
// 任何非 /admin/api/ 前缀且未匹配到 /v1/ 的 GET 请求都落到这里。
func (h *handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 仅精确 "/" 返回页面，其余一律 404（避免吃掉未知路径）。
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	if _, err := w.Write(indexHTML); err != nil {
		slog.Warn("写出管理页失败", "error", err)
	}
}

// handleFavicon 返回内嵌的 favicon（共享 logo.png），与托盘共用同一份资源。
func (h *handler) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("content-type", "image/png")
	w.Header().Set("cache-control", "public, max-age=86400")
	if _, err := w.Write(faviconBytes); err != nil {
		slog.Warn("写出 favicon 失败", "error", err)
	}
}

// handleMetrics 返回 metrics snapshot。
func (h *handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	if h.deps.Metrics == nil {
		body := map[string]any{"disabled": true}
		if hs := h.sourcesHealth(); hs != nil {
			body["sources_health"] = hs
		}
		writeJSON(w, http.StatusOK, body)
		return
	}
	writeJSON(w, http.StatusOK, h.metricsSnapshotBody())
}

// handleEvents 是 SSE 推送端点：每 3s 推送一次 metrics snapshot。
// 客户端用 EventSource 订阅，避免轮询。
// 任一 handler panic 不影响本端点（外层有 recoverMiddleware）。
func (h *handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no") // nginx 透传
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 立即推一次，避免页面空白
	writeSSEEvent(w, "snapshot", h.snapshotJSON())
	flusher.Flush()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			writeSSEEvent(w, "snapshot", h.snapshotJSON())
			flusher.Flush()
		}
	}
}

// snapshotJSON 返回 metrics snapshot 的 JSON 字节，附带 sources_health。
func (h *handler) snapshotJSON() []byte {
	b, err := json.Marshal(h.metricsSnapshotBody())
	if err != nil {
		return []byte(`{"error":"marshal"}`)
	}
	return b
}

func (h *handler) metricsSnapshotBody() map[string]any {
	body := map[string]any{}
	if h.deps.Metrics == nil {
		body["disabled"] = true
	} else {
		snap := h.deps.Metrics.Snapshot()
		raw, err := json.Marshal(snap)
		if err != nil {
			body["error"] = "marshal"
			return body
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			body["error"] = "unmarshal"
			return body
		}
	}
	if hs := h.sourcesHealth(); hs != nil {
		body["sources_health"] = hs
	}
	return body
}

func (h *handler) sourcesHealth() []SourceHealthView {
	if h.deps.SourceHealth == nil {
		return nil
	}
	hs := h.deps.SourceHealth()
	if hs == nil {
		return []SourceHealthView{}
	}
	return hs
}

// handlePromoteSource POST {name} 手动将源提升回 normal。
func (h *handler) handlePromoteSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	if h.deps.PromoteSource == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "promote not available"})
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing name"})
		return
	}
	if err := h.deps.PromoteSource(name); err != nil {
		slog.Warn("管理页手动提升源失败", "source", name, "error", err)
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "promote failed", Detail: err.Error()})
		return
	}
	// 成功：scheduler.PromoteSource 已记 Info；此处补管理入口维度
	slog.Info("管理页手动提升源成功", "source", name)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": name,
		"health": h.sourcesHealth(),
	})
}

func writeSSEEvent(w io.Writer, event string, data []byte) {
	// data 内不含换行即可；snapshot JSON 是单行。
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		slog.Warn("管理 SSE 写出失败", "event", event, "error", err)
	}
}

// handleReload 手动触发从磁盘 reload 配置。
func (h *handler) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	if h.deps.ReloadFromDisk == nil {
		writeJSON(w, http.StatusOK, okBody{OK: false})
		return
	}
	h.deps.ReloadFromDisk()
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

// handleUpstreamModels 按连接参数试拉上游 /v1/models（允许未落盘配置）。
// 统一经 Registry 分发到对应插件的 ModelCatalog，不再为任何具体源写分支。
func (h *handler) handleUpstreamModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in struct {
		BaseURL     string         `json:"base_url"`
		APIKey      string         `json:"api_key"`
		Options     map[string]any `json:"options"`
		Name        string         `json:"name"`
		Backend     string         `json:"backend"`
		BackendType string         `json:"backend_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	backend := in.Backend
	if backend == "" && in.BackendType != "" {
		if id, ok := config.BackendTypeToID(in.BackendType); ok {
			backend = id
		}
	}
	if backend == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing backend"})
		return
	}
	if h.deps.Registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "plugin registry not available"})
		return
	}
	p, ok := h.deps.Registry.Get(backend)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "unknown backend", Detail: backend})
		return
	}
	catalog, ok := p.(plugin.ModelCatalog)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "backend does not support model listing"})
		return
	}
	src := config.Source{
		Name:    strings.TrimSpace(in.Name),
		Backend: backend,
		BaseURL: strings.TrimSpace(in.BaseURL),
		APIKey:  strings.TrimSpace(in.APIKey),
		Options: in.Options,
	}
	if src.Options == nil {
		src.Options = map[string]any{}
	}
	// 敏感 options 复用：草稿为空时从同名已保存源继承保留值。
	if saved := h.currentSourceByName(src.Name); saved != nil {
		if src.BaseURL == "" {
			src.BaseURL = saved.BaseURL
		}
		for k, v := range saved.Options {
			if _, exists := src.Options[k]; !exists {
				src.Options[k] = v
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	models, err := catalog.ListModels(ctx, src)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody{Error: "fetch upstream models", Detail: err.Error()})
		return
	}
	out := make([]map[string]string, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]string{"id": m.ID, "display_name": m.DisplayName})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// handleModels 按源名拉取该源的上游 /v1/models 列表。
// GET /admin/api/models?source=<name>，成功返回 { source, models: [{id, display_name}] }；
// source 未提供或 fetcher 缺失分别返回 400 / 501。
func (h *handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	if h.deps.ModelsFetcher == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "models fetcher not configured"})
		return
	}
	source := r.URL.Query().Get("source")
	if source == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing source param"})
		return
	}
	// 上游拉取设 10s 超时，避免管理页长时间挂起。
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	models, err := h.deps.ModelsFetcher(ctx, source)
	if err != nil {
		slog.Warn("管理页拉取上游模型列表失败", "source", source, "error", err)
		writeJSON(w, http.StatusBadGateway, errorBody{Error: "fetch upstream models", Detail: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": source,
		"models": models,
	})
}

// adminConfigView 是 GET /admin/api/config 返回的视图。
// 仅暴露管理页需要编辑的字段，api_key 明文展示（按用户要求）。
type adminConfigView struct {
	Server    serverView      `json:"server"`
	Logging   loggingView     `json:"logging"`
	Breaker   breakerView     `json:"breaker"`
	Anthropic anthropicView   `json:"anthropic"`
	Sources   []sourceView    `json:"sources"`
	Models    []modelViewItem `json:"models"`
}

type serverView struct {
	Listen            string `json:"listen"`
	MaxBodyMB         int    `json:"max_body_mb"`
	ReadHeaderTimeout string `json:"read_header_timeout"`
}
type loggingView struct {
	Level      string `json:"level"`
	Format     string `json:"format"`
	File       string `json:"file"`
	MaxSizeMB  int    `json:"max_size_mb"`
	MaxBackups int    `json:"max_backups"`
}
type breakerView struct {
	FirstByteTimeout          string `json:"first_byte_timeout"`
	RequestTimeout            string `json:"request_timeout"`
	DegradeThreshold          int    `json:"degrade_threshold"`
	DegradeInterval           string `json:"degrade_interval"`
	DegradedRecoveryThreshold int    `json:"degraded_recovery_threshold"`
	CircuitInterval           string `json:"circuit_interval"`
	CircuitRecoveryThreshold  int    `json:"circuit_recovery_threshold"`
	Recovery                  string `json:"recovery"`
	MaxRetries                int    `json:"max_retries"`
}
type anthropicView struct {
	DefaultMaxTokens int  `json:"default_max_tokens"`
	CacheEnabled     bool `json:"cache_enabled"`
}
type sourceView struct {
	Name              string            `json:"name"`
	BaseURL           string            `json:"base_url"`
	APIKey            string            `json:"api_key"`
	Backend           string            `json:"backend"`
	Options           map[string]any    `json:"options,omitempty"`
	GithubToken       string            `json:"github_token,omitempty"`
	BackendType       string            `json:"backend_type"`
	ModelMap          map[string]string `json:"model_map"`
	DefaultModel      string            `json:"default_model"`
	Breaker           *breakerView      `json:"breaker,omitempty"`
	Disabled          bool              `json:"disabled"`
	Headers           map[string]string `json:"headers,omitempty"`
	SupportsWebSearch *bool             `json:"supports_web_search,omitempty"`
}

// redactOptions 去掉 options 中的敏感键，避免管理页回显 token 类凭据。
// 保存时 buildConfigFromInput / handleAddSource 会从当前快照恢复被脱敏的键。
func redactOptions(opts map[string]any) map[string]any {
	if len(opts) == 0 {
		return nil
	}
	out := make(map[string]any, len(opts))
	for k, v := range opts {
		if isSensitiveOption(k) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isSensitiveOption 判定 option 键是否为凭据类，管理页保存时需保留占位但不回显值。
func isSensitiveOption(key string) bool {
	switch key {
	case "github_token", "api_key", "token":
		return true
	}
	return false
}

// modelViewItem 是有序列表中的单个模型项（顺序 = /v1/models Priority）。
type modelViewItem struct {
	Slug                        string `json:"slug"`
	ContextWindow               *int64 `json:"context_window,omitempty"`
	AcceptsImage                *bool  `json:"accepts_image,omitempty"`
	SupportsImageDetailOriginal *bool  `json:"supports_image_detail_original,omitempty"`
}

// adminConfigInput 是 POST /admin/api/config 接收的视图，与 adminConfigView 同构。
// 全量覆盖式更新：前端必须把完整配置 POST 回来（简化语义，避免增量合并）。
type adminConfigInput struct {
	Server    serverView      `json:"server"`
	Logging   loggingView     `json:"logging"`
	Breaker   breakerView     `json:"breaker"`
	Anthropic anthropicView   `json:"anthropic"`
	Sources   []sourceView    `json:"sources"`
	Models    []modelViewItem `json:"models"`
}

func (h *handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getConfig(w, r)
	case http.MethodPost:
		h.postConfig(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
	}
}

func (h *handler) getConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := h.deps.Holder.Current()
	view := adminConfigView{
		Server: serverView{
			Listen:            cfg.Server.Listen,
			MaxBodyMB:         cfg.Server.MaxBodyMB,
			ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeout).String(),
		},
		Logging: loggingView{
			Level: cfg.Logging.Level, Format: cfg.Logging.Format, File: cfg.Logging.File,
			MaxSizeMB: cfg.Logging.MaxSizeMB, MaxBackups: cfg.Logging.MaxBackups,
		},
		Breaker: breakerView{
			FirstByteTimeout:          time.Duration(cfg.Breaker.FirstByteTimeout).String(),
			RequestTimeout:            time.Duration(cfg.Breaker.RequestTimeout).String(),
			DegradeThreshold:          cfg.Breaker.DegradeThreshold,
			DegradeInterval:           time.Duration(cfg.Breaker.DegradeInterval).String(),
			DegradedRecoveryThreshold: cfg.Breaker.DegradedRecoveryThreshold,
			CircuitInterval:           time.Duration(cfg.Breaker.CircuitInterval).String(),
			CircuitRecoveryThreshold:  cfg.Breaker.CircuitRecoveryThreshold,
			Recovery:                  cfg.Breaker.Recovery,
			MaxRetries:                cfg.Breaker.MaxRetries,
		},
		Anthropic: anthropicView{
			DefaultMaxTokens: cfg.Anthropic.DefaultMaxTokens,
			CacheEnabled:     cfg.Anthropic.CacheEnabledValue(),
		},
		Sources: make([]sourceView, 0, len(cfg.Sources)),
		Models:  make([]modelViewItem, 0, len(cfg.ModelOverrides)),
	}
	for _, src := range cfg.Sources {
		backend := src.Backend
		if backend == "" {
			if bt, err := config.NormalizeBackendType(src.BackendType); err == nil {
				if id, ok := config.BackendTypeToID(bt); ok {
					backend = id
				}
			}
		}
		shortCode := ""
		if bt, ok := config.BackendIDToType(backend); ok {
			shortCode = bt
		}
		sv := sourceView{
			Name: src.Name, BaseURL: src.BaseURL, APIKey: src.APIKey,
			Backend:     backend,
			BackendType: shortCode,
			Options:     redactOptions(src.Options),
			ModelMap:    src.ModelMap, DefaultModel: src.DefaultModel,
			Disabled:          src.Disabled,
			Headers:           src.Headers,
			SupportsWebSearch: src.SupportsWebSearch,
		}
		if src.Breaker != nil {
			sv.Breaker = &breakerView{
				FirstByteTimeout:          time.Duration(src.Breaker.FirstByteTimeout).String(),
				RequestTimeout:            time.Duration(src.Breaker.RequestTimeout).String(),
				DegradeThreshold:          src.Breaker.DegradeThreshold,
				DegradeInterval:           time.Duration(src.Breaker.DegradeInterval).String(),
				DegradedRecoveryThreshold: src.Breaker.DegradedRecoveryThreshold,
				CircuitInterval:           time.Duration(src.Breaker.CircuitInterval).String(),
				CircuitRecoveryThreshold:  src.Breaker.CircuitRecoveryThreshold,
				Recovery:                  src.Breaker.Recovery,
			}
		}
		view.Sources = append(view.Sources, sv)
	}
	for _, slug := range cfg.ConfiguredModelSlugs() {
		override := cfg.ModelOverrides[slug]
		view.Models = append(view.Models, modelViewItem{
			Slug:                        slug,
			ContextWindow:               override.ContextWindow,
			AcceptsImage:                override.AcceptsImage,
			SupportsImageDetailOriginal: override.SupportsImageDetailOriginal,
		})
	}
	writeJSON(w, http.StatusOK, view)
}

// adminBodyLimit 管理接口 POST body 上限。旁路不走 /v1 的 MaxBytesReader，
// 自己兜底防误传超大 payload 撑内存。
const adminBodyLimit = 1 << 20 // 1 MiB

func (h *handler) postConfig(w http.ResponseWriter, r *http.Request) {
	var in adminConfigInput
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, adminBodyLimit))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "read body", Detail: err.Error()})
		return
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON", Detail: err.Error()})
		return
	}
	cfg := buildConfigFromInput(in, h.deps.Holder.Current())
	var validateErr error
	if h.deps.ValidateConfig != nil {
		validateErr = h.deps.ValidateConfig(cfg)
	} else {
		validateErr = cfg.Validate()
	}
	if validateErr != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "config invalid", Detail: validateErr.Error()})
		return
	}
	if err := h.writeConfigYAML(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write config", Detail: err.Error()})
		return
	}
	slog.Info("管理页保存配置成功", "path", h.deps.CfgPath)
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

// handleSetSourceDisabled POST {name, disabled}：即时写盘并热重载，切换单源停用态。
// 只改目标源的 disabled，其余配置保持 holder 当前快照，避免管理页脏编辑被意外覆盖。
func (h *handler) handleSetSourceDisabled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in struct {
		Name     string `json:"name"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing name"})
		return
	}

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	cur := h.deps.Holder.Current()
	// 浅拷贝 Config：Sources 做深拷贝（逐元素值拷贝），其余引用字段（ModelOverrides
	// map、ModelSlugOrder slice）共享但本方法仅修改 Sources[i].Disabled，不触碰它们。
	// 若未来 Validate() 增加修改 ModelOverrides 的逻辑，需对 map 做显式拷贝。
	next := *cur
	next.Sources = make([]config.Source, len(cur.Sources))
	copy(next.Sources, cur.Sources)
	found := false
	for i := range next.Sources {
		if next.Sources[i].Name == name {
			next.Sources[i].Disabled = in.Disabled
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "unknown source", Detail: name})
		return
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "config invalid", Detail: err.Error()})
		return
	}
	if err := h.writeConfigYAMLLocked(&next); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write config", Detail: err.Error()})
		return
	}
	slog.Info("管理页切换源停用态", "source", name, "disabled", in.Disabled)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": name, "disabled": in.Disabled,
		"health": h.sourcesHealth(),
	})
}

// handleSourceTest POST {base_url, api_key, options, backend}：对上游做连通性探测。
// 统一经 Registry 分发到对应插件的 HealthProbe，不为任何具体源写分支。
func (h *handler) handleSourceTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in struct {
		BaseURL     string         `json:"base_url"`
		APIKey      string         `json:"api_key"`
		Name        string         `json:"name"`
		Backend     string         `json:"backend"`
		BackendType string         `json:"backend_type"`
		Options     map[string]any `json:"options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	backend := strings.TrimSpace(in.Backend)
	if backend == "" && in.BackendType != "" {
		if id, ok := config.BackendTypeToID(in.BackendType); ok {
			backend = id
		}
	}
	if backend == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid backend"})
		return
	}
	opts := in.Options
	if opts == nil {
		opts = map[string]any{}
	}
	src := config.Source{
		Name:    "__test__",
		BaseURL: strings.TrimSpace(in.BaseURL),
		APIKey:  in.APIKey,
		Backend: backend,
		Options: opts,
	}
	// 草稿敏感 options 复用：从同名已保存源继承保留值。
	if saved := h.currentSourceByName(strings.TrimSpace(in.Name)); saved != nil {
		if src.BaseURL == "" {
			src.BaseURL = saved.BaseURL
		}
		for k, v := range saved.Options {
			if _, exists := src.Options[k]; !exists {
				src.Options[k] = v
			}
		}
	}
	checker := health.NewWithRegistry(health.DefaultConfig(), h.deps.Registry)
	result := checker.CheckSource(r.Context(), src)

	// 转换为前端期望的格式
	status := string(result.Status)
	isOK := result.Success
	if status == string(health.StatusDegraded) {
		status = "reachable"
	}
	if status == string(health.StatusOperational) {
		status = "reachable"
	}

	var httpStatus *int
	if result.HTTPStatus != 0 {
		httpStatus = &result.HTTPStatus
	}
	writeJSON(w, http.StatusOK, sourceTestResult{
		OK:           isOK,
		Status:       status,
		Message:      result.Message,
		HTTPStatus:   httpStatus,
		ResponseTime: result.ResponseTimeMs,
	})
}

func (h *handler) currentSourceByName(name string) *config.Source {
	if name == "" {
		return nil
	}
	cur := h.deps.Holder.Current()
	for i := range cur.Sources {
		if cur.Sources[i].Name == name {
			return &cur.Sources[i]
		}
	}
	return nil
}

type sourceTestResult struct {
	OK           bool   `json:"ok"`
	Status       string `json:"status"` // "reachable" | "unreachable"
	Message      string `json:"message"`
	HTTPStatus   *int   `json:"http_status,omitempty"`
	ResponseTime int64  `json:"response_time_ms"`
}

// handleReorderSources POST {from, to}：将源从 from 位置移到 to 位置（插入式），写盘并热重载。
func (h *handler) handleReorderSources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in struct {
		From int `json:"from"`
		To   int `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	cur := h.deps.Holder.Current()
	if in.From < 0 || in.From >= len(cur.Sources) || in.To < 0 || in.To >= len(cur.Sources) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid position"})
		return
	}
	next := *cur
	next.Sources = make([]config.Source, len(cur.Sources))
	copy(next.Sources, cur.Sources)
	item := next.Sources[in.From]
	next.Sources = append(next.Sources[:in.From], next.Sources[in.From+1:]...)
	next.Sources = append(next.Sources[:in.To], append([]config.Source{item}, next.Sources[in.To:]...)...)
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "config invalid", Detail: err.Error()})
		return
	}
	if err := h.writeConfigYAMLLocked(&next); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write config", Detail: err.Error()})
		return
	}
	slog.Info("管理页调整源顺序", "from", in.From, "to", in.To)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "health": h.sourcesHealth(),
	})
}

// handleReorderModels POST {from, to}：将模型从 from 位置移到 to 位置（插入式），写盘并热重载。
func (h *handler) handleReorderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in struct {
		From int `json:"from"`
		To   int `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	cur := h.deps.Holder.Current()
	if in.From < 0 || in.From >= len(cur.ModelSlugOrder) || in.To < 0 || in.To >= len(cur.ModelSlugOrder) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid position"})
		return
	}
	next := *cur
	next.ModelSlugOrder = make([]string, len(cur.ModelSlugOrder))
	copy(next.ModelSlugOrder, cur.ModelSlugOrder)
	item := next.ModelSlugOrder[in.From]
	next.ModelSlugOrder = append(next.ModelSlugOrder[:in.From], next.ModelSlugOrder[in.From+1:]...)
	next.ModelSlugOrder = append(next.ModelSlugOrder[:in.To], append([]string{item}, next.ModelSlugOrder[in.To:]...)...)
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "config invalid", Detail: err.Error()})
		return
	}
	if err := h.writeConfigYAMLLocked(&next); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write config", Detail: err.Error()})
		return
	}
	slog.Info("管理页调整模型顺序", "from", in.From, "to", in.To)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
	})
}

// handleAddSource POST 追加一个供应商到 Sources 末尾，基于 holder 快照写盘，不合并前端脏字段。
func (h *handler) handleAddSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in sourceView
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	name := strings.TrimSpace(in.Name)
	baseURL := strings.TrimSpace(in.BaseURL)
	backend := strings.TrimSpace(in.Backend)
	if backend == "" && strings.TrimSpace(in.BackendType) != "" {
		// 兼容旧管理页请求：backend_type 短码。
		norm, err := config.NormalizeBackendType(in.BackendType)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
			return
		}
		if id, ok := config.BackendTypeToID(norm); ok {
			backend = id
		}
	}
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing name"})
		return
	}
	if backend == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing backend"})
		return
	}
	headers := sanitizeSourceHeaders(in.Headers)

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	cur := h.deps.Holder.Current()
	for _, s := range cur.Sources {
		if s.Name == name {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "source exists", Detail: name})
			return
		}
	}
	next := *cur
	next.Sources = make([]config.Source, len(cur.Sources)+1)
	copy(next.Sources, cur.Sources)
	options := in.Options
	// 兼容旧管理页：GithubToken 表单字段归入 options。
	if gt := strings.TrimSpace(in.GithubToken); gt != "" {
		if options == nil {
			options = map[string]any{}
		}
		if _, ok := options["github_token"]; !ok {
			options["github_token"] = gt
		}
	}
	next.Sources[len(cur.Sources)] = config.Source{
		Name:              name,
		BaseURL:           baseURL,
		APIKey:            strings.TrimSpace(in.APIKey),
		Backend:           backend,
		Options:           options,
		ModelMap:          map[string]string{},
		DefaultModel:      strings.TrimSpace(in.DefaultModel),
		Disabled:          in.Disabled,
		Headers:           headers,
		SupportsWebSearch: in.SupportsWebSearch,
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "config invalid", Detail: err.Error()})
		return
	}
	if err := h.writeConfigYAMLLocked(&next); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write config", Detail: err.Error()})
		return
	}
	slog.Info("管理页新增供应商", "name", name)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "health": h.sourcesHealth(),
	})
}

// sanitizeSourceHeaders 规范化前端传入的自定义 header：去掉空白键、过滤保留头。
func sanitizeSourceHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if isReservedHeader(k) {
			slog.Debug("管理页保存时跳过保留自定义 header", "header", k, "impact", "由网关统一管理，不可被 source.headers 覆盖")
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isReservedHeader 判断头名是否为网关管理的保留头（大小写不敏感）。
func isReservedHeader(k string) bool {
	switch strings.ToLower(k) {
	case "content-type", "authorization", "accept", "x-api-key", "anthropic-version", "anthropic-beta":
		return true
	}
	return false
}

// handleDeleteSource POST {name}：按已落盘 name 删除供应商，写盘并热重载。
func (h *handler) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing name"})
		return
	}

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	cur := h.deps.Holder.Current()
	next := *cur
	next.Sources = make([]config.Source, 0, len(cur.Sources))
	found := false
	for _, s := range cur.Sources {
		if s.Name == name {
			found = true
			continue
		}
		next.Sources = append(next.Sources, s)
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "unknown source", Detail: name})
		return
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "config invalid", Detail: err.Error()})
		return
	}
	if err := h.writeConfigYAMLLocked(&next); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write config", Detail: err.Error()})
		return
	}
	slog.Info("管理页删除供应商", "name", name)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "health": h.sourcesHealth(),
	})
}

// handleAddModel POST 追加模型到顺序末尾并写入 overrides，基于 holder 快照写盘。
// 路由为 /admin/api/models/add，避免与上游模型列表 /admin/api/models 冲突。
func (h *handler) handleAddModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in modelViewItem
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing slug"})
		return
	}

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	cur := h.deps.Holder.Current()
	if _, exists := cur.ModelOverrides[slug]; exists {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "model exists", Detail: slug})
		return
	}
	for _, s := range cur.ModelSlugOrder {
		if s == slug {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "model exists", Detail: slug})
			return
		}
	}

	next := *cur
	next.ModelOverrides = make(map[string]config.ModelOverride, len(cur.ModelOverrides)+1)
	for k, v := range cur.ModelOverrides {
		next.ModelOverrides[k] = v
	}
	next.ModelSlugOrder = make([]string, len(cur.ModelSlugOrder), len(cur.ModelSlugOrder)+1)
	copy(next.ModelSlugOrder, cur.ModelSlugOrder)
	next.ModelSlugOrder = append(next.ModelSlugOrder, slug)
	next.ModelOverrides[slug] = config.ModelOverride{
		ContextWindow:               in.ContextWindow,
		AcceptsImage:                in.AcceptsImage,
		SupportsImageDetailOriginal: in.SupportsImageDetailOriginal,
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "config invalid", Detail: err.Error()})
		return
	}
	if err := h.writeConfigYAMLLocked(&next); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write config", Detail: err.Error()})
		return
	}
	slog.Info("管理页新增模型", "slug", slug)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteModel POST {slug}：从顺序与 overrides 中删除模型，写盘并热重载。
func (h *handler) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing slug"})
		return
	}

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	cur := h.deps.Holder.Current()
	_, inOverrides := cur.ModelOverrides[slug]
	inOrder := false
	for _, s := range cur.ModelSlugOrder {
		if s == slug {
			inOrder = true
			break
		}
	}
	if !inOverrides && !inOrder {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "unknown model", Detail: slug})
		return
	}

	next := *cur
	next.ModelOverrides = make(map[string]config.ModelOverride, len(cur.ModelOverrides))
	for k, v := range cur.ModelOverrides {
		if k == slug {
			continue
		}
		next.ModelOverrides[k] = v
	}
	next.ModelSlugOrder = make([]string, 0, len(cur.ModelSlugOrder))
	for _, s := range cur.ModelSlugOrder {
		if s == slug {
			continue
		}
		next.ModelSlugOrder = append(next.ModelSlugOrder, s)
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "config invalid", Detail: err.Error()})
		return
	}
	if err := h.writeConfigYAMLLocked(&next); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write config", Detail: err.Error()})
		return
	}
	slog.Info("管理页删除模型", "slug", slug)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// writeConfigYAML 序列化配置并原子写盘，成功后触发热重载。内部加 writeMu。
func (h *handler) writeConfigYAML(cfg *config.Config) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return h.writeConfigYAMLLocked(cfg)
}

// writeConfigYAMLLocked 假定调用方已持有 writeMu。
func (h *handler) writeConfigYAMLLocked(cfg *config.Config) error {
	out, err := yamlMarshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}
	dir := filepath.Dir(h.deps.CfgPath)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	// 失败路径清理临时文件；rename 成功后源路径已不存在，Remove 是 no-op。
	defer os.Remove(tmpName)
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, h.deps.CfgPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	if h.deps.ReloadFromDisk != nil {
		h.deps.ReloadFromDisk()
	}
	if h.deps.SyncModelCatalog != nil {
		if err := h.deps.SyncModelCatalog(); err != nil {
			slog.Error("管理页保存后同步模型目录失败", "error", err)
			return fmt.Errorf("sync model catalog: %w", err)
		}
	}
	return nil
}

// handleGuidance GET 返回基线指令文本，POST 保存。
// GET 返回 { path, content, exists }
func (h *handler) handleGuidance(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p := h.resolveGuidancePath()
		content := readFileOrNil(p)
		writeJSON(w, http.StatusOK, map[string]any{
			"path":    p,
			"content": content,
			"exists":  content != "",
		})
	case http.MethodPost:
		h.writeMu.Lock()
		defer h.writeMu.Unlock()
		var in struct {
			Content string `json:"content"`
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, adminBodyLimit))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "read body", Detail: err.Error()})
			return
		}
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON", Detail: err.Error()})
			return
		}
		p := h.resolveGuidancePath()
		// 原子写
		dir := filepath.Dir(p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "mkdir", Detail: err.Error()})
			return
		}
		tmp, err := os.CreateTemp(dir, ".guidance-*.tmp")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "create temp", Detail: err.Error()})
			return
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if _, err := tmp.WriteString(in.Content); err != nil {
			_ = tmp.Close()
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write temp", Detail: err.Error()})
			return
		}
		if err := tmp.Close(); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "close temp", Detail: err.Error()})
			return
		}
		if err := os.Rename(tmpName, p); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "rename", Detail: err.Error()})
			return
		}
		// 触发 reload（重新加载 base_instructions_file 内容）
		if h.deps.ReloadFromDisk != nil {
			h.deps.ReloadFromDisk()
		}
		slog.Info("管理页保存基线指令成功", "path", p, "bytes", len(in.Content))
		writeJSON(w, http.StatusOK, okBody{OK: true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
	}
}

// resolveGuidancePath 返回基线指令文件路径：固定与 config.yaml 同级的 base_instructions.md。
func (h *handler) resolveGuidancePath() string {
	return filepath.Join(filepath.Dir(h.deps.CfgPath), config.BaseInstructionsFileName)
}

// readFileOrNil 读文件失败时返回空串（不报错给前端，基线指令文件缺失时为空即可）。
func readFileOrNil(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// handleVersion 返回构建注入的版本号（未注入时为空串）。
// GET /admin/api/version。
func (h *handler) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": h.deps.Version})
}
