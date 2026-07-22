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
	"github.com/mapleafgo/codex-api-gateway/internal/chatclient"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/metrics"
)

// SourceHealthView 是管理页展示的单源运行时回退等级。
type SourceHealthView struct {
	Name         string `json:"name"`
	State        string `json:"state"`         // normal | degraded | circuitOpen | halfOpen
	DegradeCount int    `json:"degrade_count"` // 0/1/2 量级
	Priority     int    `json:"priority"`      // 运行时优先级，1=最高
}

// Deps 是 Mount 需要的依赖。main 组装时传入。
type Deps struct {
	Holder  *config.Holder
	Metrics *metrics.Collector
	CfgPath string // config.yaml 的绝对路径（用于写回）
	// ReloadFromDisk 在写回 config.yaml 后调用：让 configwatch 重新 Load。
	// 若 configwatch 未启用，传 nil 即可（写回不立即生效，需重启）。
	ReloadFromDisk func()
	// ModelsFetcher 按源名拉取上游 /v1/models 列表，供管理页编辑模型映射时选用。
	// 若未提供（nil），对应接口返回 501。
	ModelsFetcher func(ctx context.Context, sourceName string) ([]anthropic.ModelInfo, error)
	// SourceHealth 返回各源运行时健康态。nil 时 snapshot 不附带 sources_health。
	SourceHealth func() []SourceHealthView
	// PromoteSource 手动将源提升回 normal。nil 时 promote 接口 501。
	PromoteSource func(name string) error
}

type handler struct {
	deps Deps
	// writeMu 序列化配置写回，避免并发保存互相覆盖。
	writeMu sync.Mutex
}

// Mount 把管理页与 JSON API 挂载到 mux 的 / 与 /admin/api/* 路径。
// 已存在的 /v1/* 路由不受影响（由调用方先注册）。
func Mount(mux *http.ServeMux, deps Deps) {
	h := &handler{deps: deps}
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
	// 只对精确 "/" 与非 /v1、非 /admin/api 的路径返回页面；
	// 不匹配则 404（避免吃掉未知路径）。
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

// handleModels 拉取指定源的上游 /v1/models 列表。

// POST /admin/api/upstream-models
// body: {base_url, api_key, backend_type} — 允许未落盘试拉。
func (h *handler) handleUpstreamModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	var in struct {
		BaseURL     string `json:"base_url"`
		APIKey      string `json:"api_key"`
		BackendType string `json:"backend_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid json", Detail: err.Error()})
		return
	}
	if in.BaseURL == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "missing base_url"})
		return
	}
	bt, err := config.NormalizeBackendType(in.BackendType)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var models []anthropic.ModelInfo
	if bt == config.BackendOpenAIChat {
		ms, err := chatclient.New().ListModels(ctx, in.BaseURL, in.APIKey)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, errorBody{Error: "fetch upstream models", Detail: err.Error()})
			return
		}
		for _, m := range ms {
			models = append(models, anthropic.ModelInfo{ID: m.ID, DisplayName: m.DisplayName})
		}
	} else {
		ms, err := anthropic.New().ListModels(ctx, in.BaseURL, in.APIKey)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, errorBody{Error: "fetch upstream models", Detail: err.Error()})
			return
		}
		models = ms
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// GET /admin/api/models?source=<name>
// 成功返回 { source, models: [{id, display_name}] }。
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
	Server               serverView      `json:"server"`
	Logging              loggingView     `json:"logging"`
	Breaker              breakerView     `json:"breaker"`
	Cache                cacheView       `json:"cache"`
	BaseInstructionsFile string          `json:"base_instructions_file"`
	Sources              []sourceView    `json:"sources"`
	Models               []modelViewItem `json:"models"`
}

type serverView struct {
	Listen string `json:"listen"`
}
type loggingView struct {
	Level  string `json:"level"`
	Format string `json:"format"`
	File   string `json:"file"`
}
type breakerView struct {
	FirstByteTimeout string `json:"first_byte_timeout"`
	Cooldown         string `json:"cooldown"`
	DegradeThreshold int    `json:"degrade_threshold"`
	RecoverThreshold int    `json:"recover_threshold"`
	HalfOpenProbes   int    `json:"half_open_probes"`
	MaxRetries       int    `json:"max_retries"`
	Recovery         string `json:"recovery"`
}
type cacheView struct {
	TTL string `json:"ttl"`
}
type sourceView struct {
	Name         string            `json:"name"`
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key"`
	BackendType  string            `json:"backend_type"`
	ModelMap     map[string]string `json:"model_map"`
	DefaultModel string            `json:"default_model"`
	Breaker      *breakerView      `json:"breaker,omitempty"`
}

// modelViewItem 是有序列表中的单个模型项（顺序 = /v1/models Priority）。
type modelViewItem struct {
	Slug           string `json:"slug"`
	ContextWindow  *int64 `json:"context_window,omitempty"`
	SupportsImage  *bool  `json:"supports_image,omitempty"`
	SupportsSearch *bool  `json:"supports_search,omitempty"`
}

// adminConfigInput 是 POST /admin/api/config 接收的视图，与 adminConfigView 同构。
// 全量覆盖式更新：前端必须把完整配置 POST 回来（简化语义，避免增量合并）。
type adminConfigInput struct {
	Server               serverView      `json:"server"`
	Logging              loggingView     `json:"logging"`
	Breaker              breakerView     `json:"breaker"`
	Cache                cacheView       `json:"cache"`
	BaseInstructionsFile string          `json:"base_instructions_file"`
	Sources              []sourceView    `json:"sources"`
	Models               []modelViewItem `json:"models"`
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
		Server:  serverView{Listen: cfg.Server.Listen},
		Logging: loggingView{Level: cfg.Logging.Level, Format: cfg.Logging.Format, File: cfg.Logging.File},
		Breaker: breakerView{
			FirstByteTimeout: time.Duration(cfg.Breaker.FirstByteTimeout).String(),
			Cooldown:         time.Duration(cfg.Breaker.Cooldown).String(),
			DegradeThreshold: cfg.Breaker.DegradeThreshold,
			RecoverThreshold: cfg.Breaker.RecoverThreshold,
			HalfOpenProbes:   cfg.Breaker.HalfOpenProbes,
			MaxRetries:       cfg.Breaker.MaxRetries,
			Recovery:         cfg.Breaker.Recovery,
		},
		Cache:                cacheView{TTL: cfg.Cache.TTL},
		BaseInstructionsFile: cfg.BaseInstructionsFile,
		Sources:              make([]sourceView, 0, len(cfg.Sources)),
		Models:               make([]modelViewItem, 0, len(cfg.ModelOverrides)),
	}
	for _, src := range cfg.Sources {
		bt, _ := config.NormalizeBackendType(src.BackendType)
		sv := sourceView{
			Name: src.Name, BaseURL: src.BaseURL, APIKey: src.APIKey,
			BackendType: bt,
			ModelMap:    src.ModelMap, DefaultModel: src.DefaultModel,
		}
		if src.Breaker != nil {
			sv.Breaker = &breakerView{
				FirstByteTimeout: time.Duration(src.Breaker.FirstByteTimeout).String(),
				Cooldown:         time.Duration(src.Breaker.Cooldown).String(),
				DegradeThreshold: src.Breaker.DegradeThreshold,
				RecoverThreshold: src.Breaker.RecoverThreshold,
				HalfOpenProbes:   src.Breaker.HalfOpenProbes,
				Recovery:         src.Breaker.Recovery,
			}
		}
		view.Sources = append(view.Sources, sv)
	}
	for _, slug := range cfg.ConfiguredModelSlugs() {
		override := cfg.ModelOverrides[slug]
		view.Models = append(view.Models, modelViewItem{
			Slug:           slug,
			ContextWindow:  override.ContextWindow,
			SupportsImage:  override.SupportsImageDetailOriginal,
			SupportsSearch: override.SupportsSearchTool,
		})
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *handler) postConfig(w http.ResponseWriter, r *http.Request) {
	var in adminConfigInput
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "read body", Detail: err.Error()})
		return
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON", Detail: err.Error()})
		return
	}
	cfg := buildConfigFromInput(in)
	if err := cfg.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "config invalid", Detail: err.Error()})
		return
	}
	// 序列化为 YAML 写回磁盘
	out, err := yamlMarshal(cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "marshal yaml", Detail: err.Error()})
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	// 原子写：临时文件 + rename
	dir := filepath.Dir(h.deps.CfgPath)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "create temp", Detail: err.Error()})
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 失败时清理临时文件
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "write temp", Detail: err.Error()})
		return
	}
	if err := tmp.Close(); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "close temp", Detail: err.Error()})
		return
	}
	if err := os.Rename(tmpName, h.deps.CfgPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "rename", Detail: err.Error()})
		return
	}
	// 触发热重载（configwatch 监听会自动 reload，这里也手动调用确保即时生效）
	if h.deps.ReloadFromDisk != nil {
		h.deps.ReloadFromDisk()
	}
	slog.Info("管理页保存配置成功", "path", h.deps.CfgPath, "bytes", len(out))
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

// handleGuidance GET 返回引导语文本，POST 保存。
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
		body, err := io.ReadAll(r.Body)
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
		slog.Info("管理页保存引导语成功", "path", p, "bytes", len(in.Content))
		writeJSON(w, http.StatusOK, okBody{OK: true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
	}
}

// resolveGuidancePath 返回引导语文件路径：取 cfg.BaseInstructionsFile，
// 为空则默认 base_instructions.md（相对于 config.yaml 所在目录）。
func (h *handler) resolveGuidancePath() string {
	cfg := h.deps.Holder.Current()
	name := cfg.BaseInstructionsFile
	if name == "" {
		name = "base_instructions.md"
	}
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(h.deps.CfgPath), name)
	}
	return p
}

// readFileOrNil 读文件失败时返回空串（不报错给前端，引导语未启用时为空即可）。
func readFileOrNil(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
