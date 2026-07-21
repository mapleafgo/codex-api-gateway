// Package server wires config, scheduler, and HTTP handlers
// into a single /v1/responses endpoint that translates OpenAI Responses API
// requests to Anthropic Messages streams and back.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	aconstant "github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/convert"
	"github.com/mapleafgo/codex-api-gateway/internal/metrics"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/scheduler"
	oairesponses "github.com/openai/openai-go/v3/responses"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

var (
	anContentBlockStart = string(aconstant.ValueOf[aconstant.ContentBlockStart]())
	anMessageStop       = string(aconstant.ValueOf[aconstant.MessageStop]())
)

// Server wires config, scheduler, and HTTP handlers.
type Server struct {
	holder    *config.Holder
	sch       *scheduler.Scheduler
	metrics   *metrics.Collector
	startedAt int64
}

// New builds a Server.
func New(cfg *config.Config) *Server {
	holder := config.NewHolder(cfg)
	slog.Info("初始化服务组件", "sources", len(cfg.Sources))
	return &Server{
		holder:    holder,
		sch:       scheduler.New(holder),
		metrics:   metrics.New(),
		startedAt: time.Now().Unix(),
	}
}

// Holder 返回内部的 Holder，供 admin 包挂载热重载回调。
func (s *Server) Holder() *config.Holder { return s.holder }

// Metrics 返回内部的 metrics Collector，供 admin 包读取。
func (s *Server) Metrics() *metrics.Collector { return s.metrics }

// Scheduler 返回内部的 Scheduler，供 admin 包触发 Reload。
func (s *Server) Scheduler() *scheduler.Scheduler { return s.sch }

// ReloadScheduler 让外层（configwatch）通知 scheduler 重建运行时优先级。
func (s *Server) ReloadScheduler() {
	func() {
		defer func() { _ = recover() }()
		s.sch.Reload()
	}()
}

// Close releases server resources. Currently a no-op; retained as the
// shutdown hook for future resources.
func (s *Server) Close() error {
	if s.metrics != nil {
		s.metrics.Stop()
	}
	return nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/v1/models", s.handleModels)
	return mux
}

// Mux 返回内部 mux 供 main 挂载额外路由（admin 等）。
// 与 Handler() 的区别：返回 *http.ServeMux，便于外部追加路由。
// 多次调用会创建多个独立的 mux，建议只调用一次。
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/v1/models", s.handleModels)
	return mux
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		slog.Warn("拒绝模型列表请求", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 仅返回 config.yaml 中 models.<slug> 显式配置的模型。
	// Codex 期望 /v1/models 返回 { "models": [ModelInfo] }（不是 OpenAI {data:[]}）。
	// Codex 用 serde_json::from_slice::<ModelsResponse> 直接解析，若返回 OpenAI 格式，
	// 解析失败/拿到空 ModelInfo → supports_search_tool 默认 false → tool_search 与 MCP
	// 延迟加载工具（deferred tools）不可用。故返回 CodexModelsResponse，补全 ModelInfo
	// 能力字段（关键是 supports_search_tool=true）。
	cur := s.holder.Current()
	names := cur.ConfiguredModelSlugs()
	infos := make([]model.CodexModelInfo, 0, len(names))
	for _, name := range names {
		infos = append(infos, s.codexModelInfo(name))
	}

	resp := model.CodexModelsResponse{Models: infos}
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("写出模型列表响应失败", "error", err)
	} else {
		slog.Info("模型列表响应完成", "models", len(infos))
	}
}

// codexModelInfo 为一个模型 slug 构造 Codex ModelInfo，补齐网关所需的全部字段。
// 字段对齐 codex-rs/protocol/src/openai_models.rs 的 ModelInfo（0.144.5），
// 字段语义与 serde 约束详见 internal/model/modellist.go 的注释。
//
// 网关默认值策略（逐项确认后的结论）：
//
//	必填字段：
//	  - base_instructions：来自 config base_instructions_file（非空时整体替换 Codex
//	    内置 BASE_INSTRUCTIONS，用于注入网关级指令补强）；为空则沿用 Codex 内置指令。
//	  - apply_patch_tool_type="freeform"：启用 apply_patch 工具，否则只能靠 shell 改文件。
//	  - supports_reasoning_summaries=true：接受 reasoning.summary 参数。
//	必填字段默认值：
//	  - display_name 大写（UI 展示更醒目，如 GPT-5.5）
//	  - truncation_policy.limit = 10000（对齐官方固定值，单次工具输出截断阈值，
//	    与 context_window 是独立维度，不随窗口缩放）
//	可选字段（仅在网关必须告知时给值，其余交给 Codex 默认）：
//	  - supports_search_tool=true（启用 tool_search + MCP deferred tools 懒加载，网关核心）
//	  - context_window（Codex 默认 None，必须告知；同步到 max_context_window）
//	  - include_skills_usage_instructions=true（注入 skills 使用说明块，引导 skill 发现）
//	  - web_search_tool_type="text_and_image"（声明支持文本+图片 web 搜索）
//	  - input_modalities=["text","image"]（默认按多模态声明）
//	  - tool_mode="direct"：强制标准工具模式（function 工具独立发送、模型直接调用），
//	    覆盖 Codex features.code_mode* 配置。第三方上游（GLM 等）无 code_mode DSL
//	    训练，显式 direct 比依赖客户端默认更稳，避免误走 code_mode 造成降级。
//	  - multi_agent_version="v2"：对齐 gpt-5.6 catalog。启用多 agent 协作工具
//	    （spawn_csv/agent_jobs 等），V2 不受 spawn depth 限制。
//	  - comp_hash="3000"：对齐 gpt-5.6 catalog 压缩兼容哈希。连续两 turn 不同会
//	    触发 Codex 前置压缩；网关统一注入，避免客户端切模型时误触发。
//	不注入（保持零值）：
//	  - model_messages
//	  - default_reasoning_level / service_tiers / default_service_tier 等
//
// config.yaml models.<slug> 仅可覆盖 context_window / supports_image；
// 其余字段硬编码统一注入，不开放 per-slug 覆盖。
func (s *Server) codexModelInfo(slug string) model.CodexModelInfo {
	emptyStr := ""
	freeformApplyPatch := "freeform"
	ctxWindow := int64(200000)
	toolMode := "direct"
	multiAgentV2 := "v2"
	compHash := "3000"
	responsesLiteOff := false
	info := model.CodexModelInfo{
		// —— 必填字段 ——
		Slug:                       slug,
		DisplayName:                strings.ToUpper(slug),
		Description:                &emptyStr,
		SupportedReasoningLevels:   defaultReasoningLevels(),
		ShellType:                  "shell_command",
		Visibility:                 "list",
		SupportedInAPI:             true,
		Priority:                   0,
		AvailabilityNux:            nil,
		Upgrade:                    nil,
		BaseInstructions:           s.holder.Current().BaseInstructions,
		SupportsReasoningSummaries: true,
		SupportVerbosity:           false,
		DefaultVerbosity:           nil,
		ApplyPatchToolType:         &freeformApplyPatch,
		TruncationPolicy: model.CodexTruncationPolicy{
			// limit 对齐官方模型 catalog 固定值 10000：这是 shell/exec/mcp resource 等
			// 单次工具调用输出回传模型时的截断阈值，与模型 context_window 独立，不随窗口缩放。
			Mode: "tokens", Limit: 10000,
		},
		SupportsParallelToolCalls:  true,
		ExperimentalSupportedTools: []string{},

		// —— 可选字段（网关必须告知的） ——
		SupportsSearchTool:             true,
		ContextWindow:                  &ctxWindow,
		MaxContextWindow:               &ctxWindow,
		IncludeSkillsUsageInstructions: true,
		WebSearchToolType:              "text_and_image",
		InputModalities:                []string{"text", "image"},
		// tool_mode="direct"：强制标准工具调用模式（function 工具独立发送、模型直接
		// 调用），覆盖 Codex features.code_mode* 配置。第三方上游无 code_mode DSL 训练，
		// 显式 direct 避免客户端默认走 code_mode 造成降级。
		ToolMode:          &toolMode,
		MultiAgentVersion: &multiAgentV2,
		// comp_hash="3000"：对齐官方 gpt-5.6 catalog 的压缩兼容性哈希。
		// 连续两 turn 的 comp_hash 不同时，Codex 会触发 previous-model inline compact；
		// 网关所有模型注入同一值，避免客户端在模型间切换时误触发压缩。
		CompHash: &compHash,
		// use_responses_lite=false：显式压制 Responses Lite（Codex→OpenAI 后端内部传输
		// 优化，第三方上游有害无益）。显式 false 而非省略，防止 Codex hardcode/默认开启
		// lite（如 gpt-5.6 catalog 硬编码 true）。不开放 per-slug 覆盖。
		UseResponsesLite: &responsesLiteOff,
	}

	// 应用 config.yaml models.<slug> 覆盖（仅覆盖显式配置的字段）
	if ov, ok := s.resolveModelOverride(slug); ok {
		applyModelOverride(&info, &ov)
	}
	return info
}

// defaultReasoningLevels 返回网关默认注入的 reasoning effort 预设列表。
// 空 [] 合法但表示模型不支持 reasoning；这里给出常规档位，不开放 per-slug 覆盖。
func defaultReasoningLevels() []model.CodexReasoningLevel {
	return []model.CodexReasoningLevel{
		{Effort: "low", Description: "快速响应，轻量推理"},
		{Effort: "medium", Description: "平衡模式，常规推理"},
		{Effort: "high", Description: "深入推理，适合复杂任务"},
		{Effort: "xhigh", Description: "超高强度推理"},
	}
}

// resolveModelOverride 解析 slug 的 ModelOverride。
// 查找优先级：
//  1. models.<slug> 直接命中（精确覆盖，优先级最高）
//  2. 若 slug 是某 source.model_map 的别名（key），取其映射到的真实上游模型，
//     再查 models.<真实模型>（别名继承真实模型配置）
//  3. 都没命中返回 false。
//
// 别名继承机制让用户只需为真实上游模型（如 glm-5.2）配置一次能力字段，
// model_map 里的别名（如 gpt-5.5→glm-5.2）自动继承，无需重复配置。
func (s *Server) resolveModelOverride(slug string) (config.ModelOverride, bool) {
	cur := s.holder.Current()
	if ov, ok := cur.ModelOverrides[slug]; ok {
		return ov, true
	}
	for _, src := range cur.Sources {
		// mapped 是 model_map 别名指向的真实上游模型 slug。
		if mapped, ok := src.ModelMap[slug]; ok {
			if ov, ok2 := cur.ModelOverrides[mapped]; ok2 {
				return ov, true
			}
		}
	}
	return config.ModelOverride{}, false
}

// applyModelOverride 把 ModelOverride 覆盖到 CodexModelInfo（仅覆盖非 nil 字段）。
// ModelOverride 只暴露 per-model 真实差异（context_window / supports_image），
// 其余能力由 codexModelInfo 硬编码统一注入。
func applyModelOverride(info *model.CodexModelInfo, ov *config.ModelOverride) {
	// context_window 同时应用到 ContextWindow 与 MaxContextWindow：Codex ModelInfo 协议
	// 要求两个字段，网关场景二者相等，config 只暴露一个 context_window 输入。
	if ov.ContextWindow != nil {
		info.ContextWindow = ov.ContextWindow
		info.MaxContextWindow = ov.ContextWindow
	}
	if ov.SupportsImageDetailOriginal != nil {
		info.SupportsImageDetailOriginal = *ov.SupportsImageDetailOriginal
	}
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		slog.Warn("拒绝响应请求", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reqStart := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Warn("读取响应请求体失败", "error", err)
		http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
		return
	}
	req, err := convert.DecodeResponseNewParams(body)
	if err != nil {
		slog.Warn("解析响应请求体失败", "error", err)
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	slog.Info("收到响应请求",
		"method", r.Method,
		"path", r.URL.Path,
		"model", string(req.Model),
		"input_items", len(req.Input.OfInputItemList),
		"input_string_len", len(req.Input.OfString.Value),
		"instructions_len", len(req.Instructions.Value),
		"reasoning_effort", string(req.Reasoning.Effort),
		"reasoning_summary", string(req.Reasoning.Summary),
		slog.Group("input_item_type_counts", inputItemTypeCountAttrs(req.Input.OfInputItemList)...),
		slog.Group("tools", toolSummaryAttrs(req.Tools)...))
	// 逐条打印 input item 类型，用于诊断 Codex 发来的对话历史结构
	for i := range req.Input.OfInputItemList {
		it := &req.Input.OfInputItemList[i]
		role := ""
		if it.OfMessage != nil {
			role = string(it.OfMessage.Role)
		}
		slog.Debug("响应请求输入项", "index", i, "type", itemType(it), "role", role)
	}

	warnDroppedOrIgnoredParams(req)

	ordered := s.holder.Current().OrderedSources()
	if len(ordered) == 0 {
		// 零源配置：进程可启动但无上游可转发。返回 503 引导用户去管理页配置。
		slog.Warn("转发请求被拒绝：未配置任何上游源")
		http.Error(w, "no upstream source configured; add one via admin page", http.StatusServiceUnavailable)
		return
	}
	// 预检：首源为 Anthropic 时 dry-run ToAnthropic，在写 SSE 头前返回 400（对齐旧行为）。
	// 首源为 Chat 时不做 Anthropic 预检，避免纯 Chat 部署被误杀。
	first := ordered[0]
	bt, _ := config.NormalizeBackendType(first.BackendType)
	if bt == config.BackendAnthropic {
		if _, err := convert.DecodeResponseNewParams(body); err != nil {
			slog.Warn("预解析响应请求失败", "source", first.Name, "error", err)
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if _, _, err := convert.ToAnthropic(req, s.holder.Current()); err != nil {
			slog.Warn("预转换响应请求失败", "source", first.Name, "backend_type", bt, "error", err)
			http.Error(w, "convert: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var evCount int
	resolvedModel := string(req.Model)
	backendType := config.BackendAnthropic
	var lastUp scheduler.UpstreamEvent

	sourceName, execErr := s.sch.ExecuteGeneric(r.Context(), body,
		func(e model.SSEEvent) error {
			evCount++
			slog.Debug("写出响应 SSE 事件", "event_type", e.Type)
			writeSSE(w, e)
			flusher.Flush()
			return nil
		},
		func(ev scheduler.UpstreamEvent) {
			lastUp = ev
			if ev.ResolvedModel != "" {
				resolvedModel = ev.ResolvedModel
			}
			if ev.BackendType != "" {
				backendType = ev.BackendType
			}
			s.recordUpstream()(ev)
		},
	)

	// Backend 已在 SSE 流内写入真实 response id；此处 id 仅用于日志/metrics 关联。
	respID := newResponseID()
	clientCanceled := isClientCanceled(r.Context(), execErr)

	status := model.ResponseStatusCompleted
	code := 200
	errText := ""
	// 上游已业务完成（lastUp.Status=completed）后客户端断开：按 completed 收尾。
	upstreamCompleted := lastUp.Status == "completed"
	if execErr == nil || (clientCanceled && upstreamCompleted) {
		slog.Info("响应请求完成",
			"response_id", respID,
			"status", status,
			"source", sourceName,
			"backend_type", backendType,
			"upstream_events", evCount,
			"cache_read_tokens", lastUp.CacheRead,
			"cache_creation_tokens", lastUp.CacheCreate,
			"elapsed", time.Since(reqStart).String())
	} else if clientCanceled {
		status = "canceled"
		code = 499
		errText = errSummary(execErr)
		slog.Info("响应请求被客户端取消", "response_id", respID, "source", sourceName, "backend_type", backendType, "upstream_events", evCount, "elapsed", time.Since(reqStart).String(), "error", execErr)
	} else {
		status = model.ResponseStatusFailed
		code = clientFailCode(execErr)
		errText = errSummary(execErr)
		slog.Error("响应请求失败", "response_id", respID, "status", "failed", "source", sourceName, "backend_type", backendType, "elapsed", time.Since(reqStart).String(), "error", execErr)
		// 若流尚未写出任何事件，补一条 failed（Backend 通常已写）
		if evCount == 0 {
			errResp := model.NewResponseObject(respID, model.ResponseStatusFailed, string(req.Model), time.Now().Unix(), echoFromRequest(req))
			errResp.Output = []model.OutputItem{}
			errResp.Error = &model.ResponseError{Message: fmt.Sprintf("upstream: %v", execErr)}
			evType := string(oaconstant.ValueOf[oaconstant.ResponseFailed]())
			writeSSE(w, model.MarshalEvent(evType, model.TerminalResponseEvent{
				Type: evType, SequenceNumber: 1, Response: errResp,
			}))
			flusher.Flush()
		}
	}

	s.metrics.Record(metrics.RequestEvent{
		Kind:          metrics.KindClient,
		StartedAt:     reqStart,
		Duration:      time.Since(reqStart),
		SourceName:    sourceName,
		Model:         string(req.Model),
		ResolvedModel: resolvedModel,
		Status:        status,
		Code:          code,
		InputTokens:   lastUp.InputTokens,
		OutputTokens:  lastUp.OutputTokens,
		CacheRead:     lastUp.CacheRead,
		CacheCreate:   lastUp.CacheCreate,
		Error:         errText,
		BackendType:   backendType,
	})
}

func writeSSE(w io.Writer, e model.SSEEvent) {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, e.Data); err != nil {
		slog.Warn("写出 SSE 事件失败", "event_type", e.Type, "error", err)
	}
}

// warnDroppedOrIgnoredParams 对当前不语义映射、后端无等价能力、
// 或 deprecated 的请求字段统一输出 WARN 级别结构化日志，避免静默丢弃。
// 约定见 AGENTS.md「静默跳过与降级处理约定」。
func warnDroppedOrIgnoredParams(req *oairesponses.ResponseNewParams) {
	// deprecated reasoning.generate_summary：被 reasoning.summary 取代。
	//nolint:staticcheck // 字段被 OpenAI 标记 deprecated，但我们正是要检测它以输出 WARN
	if req.Reasoning.GenerateSummary != "" {
		slog.Warn("忽略 deprecated 字段 reasoning.generate_summary（已由 reasoning.summary 取代），对应数据被丢弃",
			"field", "reasoning.generate_summary",
			"value", string(req.Reasoning.GenerateSummary),
			"reasoning_summary", string(req.Reasoning.Summary),
			"impact", "generate_summary 不生效，请改用 reasoning.summary")
	}
	// previous_response_id：网关无 session store，不做 enrich。
	// Codex 主路径不传此字段（客户端自带完整 input 回灌）；其它客户端若依赖链式会话会失效。
	if req.PreviousResponseID.Valid() && req.PreviousResponseID.Value != "" {
		slog.Warn("忽略 previous_response_id（网关无 session store，不做历史 enrich），对应数据被丢弃",
			"field", "previous_response_id",
			"previous_response_id", req.PreviousResponseID.Value,
			"impact", "不会按 response_id 补全历史；请在 input 中完整回灌上下文")
	}
	// service_tier：网关不透传，由上游默认处理。
	if req.ServiceTier != "" {
		slog.Warn("忽略 service_tier（网关不透传，由上游按默认处理），对应数据被丢弃",
			"field", "service_tier",
			"value", string(req.ServiceTier),
			"impact", "请求不会指定 OpenAI service tier")
	}
	// text.verbosity：Anthropic 无原生 verbosity 参数。
	if req.Text.Verbosity != "" {
		slog.Warn("忽略 text.verbosity（Anthropic 无原生等价参数），对应数据被丢弃",
			"field", "text.verbosity",
			"value", string(req.Text.Verbosity),
			"impact", "verbosity 不生效")
	}
	// truncation：Anthropic 无直接等价策略，仅在响应中 echo。
	// truncation 状态为 raw_preserved：值在响应对象中 echo 回显，未被丢弃，
	// 不触发 WARN（AGENTS.md 的静默跳过约定针对丢弃场景，不针对 echo）。

	// include：按 include 项分档处理。
	//   - satisfied：网关默认已发出对应字段（如 web_search sources、code_interpreter outputs），
	//     无需额外行为，也不 WARN；
	//   - encrypted_content：已通过 disable_response_storage 路径处理，不 WARN；
	//   - unsupported：后端无等价能力（file_search、logprobs、computer 等），WARN + 丢弃。
	if len(req.Include) > 0 {
		satisfied := map[string]bool{
			"reasoning.encrypted_content":    true, // ZDR 路径已处理
			"web_search_call.action.sources": true, // action.sources 默认下发
			"code_interpreter_call.outputs":  true, // outputs 默认下发
			"message.input_image.image_url":  true, // 输入 image_url 原样保留
		}
		var unsupported []string
		for _, inc := range req.Include {
			if satisfied[string(inc)] {
				continue
			}
			unsupported = append(unsupported, string(inc))
		}
		if len(unsupported) > 0 {
			slog.Warn("忽略无 Anthropic 等价能力的 include 项，对应数据被丢弃",
				"field", "include",
				"values", strings.Join(unsupported, ","),
				"impact", "该 include 项不会生效（file_search / logprobs / computer 等无后端等价）")
		}
	}
	// metadata：Anthropic metadata 仅支持 user_id，整体 echo + 取 user_id 透传。
	if len(req.Metadata) > 0 {
		// 只有存在非 user_id 键值对时才 WARN（user_id 被透传，不是丢弃）。
		nonUserID := 0
		for k := range req.Metadata {
			if k != "user_id" {
				nonUserID++
			}
		}
		if nonUserID > 0 {
			slog.Warn("metadata 整体仅在响应中 echo，Anthropic metadata 只透传 user_id（取自 metadata.user_id）",
				"field", "metadata",
				"entries", len(req.Metadata),
				"impact", "非 user_id 的键值对不会传递给上游")
		}
	}
	// prompt_cache_*：Anthropic 用内容 hash + 网关自主 cache_control，不认 OpenAI client key/options/retention。
	if req.PromptCacheKey.Valid() && req.PromptCacheKey.Value != "" {
		// Codex 常发；网关已自主 cache_control，属可控协议差异 → DEBUG 即可。
		slog.Debug("忽略 prompt_cache_key（Anthropic 不认客户端 cache key，网关已自主设 cache_control）",
			"field", "prompt_cache_key",
			"impact", "不会按 OpenAI prompt_cache_key 分桶缓存；上游缓存由 Anthropic cache_control 控制")
	}
	if req.PromptCacheOptions.Mode != "" || req.PromptCacheOptions.Ttl != "" {
		// 与 prompt_cache_key 同属可控协议差异，网关已自主 cache_control → DEBUG。
		slog.Debug("忽略 prompt_cache_options（OpenAI options 对 Anthropic 无意义，网关已自主设 cache_control）",
			"field", "prompt_cache_options",
			"mode", req.PromptCacheOptions.Mode,
			"ttl", req.PromptCacheOptions.Ttl,
			"impact", "不会按 OpenAI prompt_cache_options 调整缓存策略")
	}
	if req.PromptCacheRetention != "" {
		// deprecated 字段且语义不等价；网关用 cache.ttl 配置，DEBUG 即可。
		slog.Debug("忽略 prompt_cache_retention（deprecated；与 Anthropic cache_control 语义不同）",
			"field", "prompt_cache_retention",
			"value", string(req.PromptCacheRetention),
			"impact", "不会按 in_memory/24h 调整上游缓存保留；请用网关 cache_control TTL 配置")
	}
	// prompt：引用 OpenAI prompt template，网关无服务端模板存储。
	if req.Prompt.ID != "" {
		slog.Warn("忽略 prompt（网关无 OpenAI prompt 模板存储能力），对应数据被丢弃",
			"field", "prompt",
			"prompt_id", req.Prompt.ID,
			"impact", "模板与变量不会被解析，input 以实际内容为准")
	}
	// background：当前网关只支持同步 SSE。
	if req.Background.Valid() && req.Background.Value {
		slog.Warn("忽略 background=true（网关仅支持同步 SSE），请求将按同步处理",
			"field", "background",
			"impact", "请求不会被转为后台执行")
	}
	// conversation：网关无状态，不是 OpenAI Conversation API。
	if req.Conversation.OfString.Valid() || req.Conversation.OfConversationObject != nil {
		slog.Warn("忽略 conversation（网关非 OpenAI Conversation API），对应数据被丢弃",
			"field", "conversation",
			"impact", "不会使用 conversation 拉取历史")
	}
	// context_management：OpenAI 服务端自动压缩，Anthropic 无等价请求参数。
	if len(req.ContextManagement) > 0 {
		types := make([]string, 0, len(req.ContextManagement))
		for _, cm := range req.ContextManagement {
			types = append(types, cm.Type)
		}
		slog.Warn("忽略 context_management（Anthropic 无等价请求参数，网关未实现 compaction），对应数据被丢弃",
			"field", "context_management",
			"types", strings.Join(types, ","),
			"impact", "上下文管理策略不生效")
	}
	// max_tool_calls：Anthropic 无直接请求参数。
	if req.MaxToolCalls.Valid() {
		slog.Warn("忽略 max_tool_calls（Anthropic 无等价请求参数，网关不做计数截断），对应数据被丢弃",
			"field", "max_tool_calls",
			"value", req.MaxToolCalls.Value,
			"impact", "工具调用次数不会在网关层被截断")
	}
	// safety_identifier：后端无等价字段。
	if req.SafetyIdentifier.Valid() && req.SafetyIdentifier.Value != "" {
		slog.Warn("忽略 safety_identifier（Anthropic 后端无等价字段），对应数据被丢弃",
			"field", "safety_identifier",
			"impact", "不会传递给上游")
	}
	// moderation：OpenAI 输入/输出 moderation 配置。
	if req.Moderation.Model != "" ||
		req.Moderation.Policy.Input.Mode != "" ||
		req.Moderation.Policy.Output.Mode != "" {
		slog.Warn("忽略 moderation（Anthropic Messages 无等价参数），对应数据被丢弃",
			"field", "moderation",
			"moderation_model", req.Moderation.Model,
			"input_mode", req.Moderation.Policy.Input.Mode,
			"output_mode", req.Moderation.Policy.Output.Mode,
			"impact", "不会对输入/输出做 OpenAI moderation")
	}
	// stream_options.include_obfuscation：Anthropic streaming 无等价 obfuscation。
	if req.StreamOptions.IncludeObfuscation.Valid() {
		slog.Warn("忽略 stream_options.include_obfuscation（Anthropic streaming 无等价机制），对应数据被丢弃",
			"field", "stream_options.include_obfuscation",
			"value", req.StreamOptions.IncludeObfuscation.Value,
			"impact", "obfuscation 字段不生效")
	}
	// top_logprobs：Anthropic Messages 无 OpenAI output logprobs 等价。
	if req.TopLogprobs.Valid() {
		slog.Warn("忽略 top_logprobs（Anthropic Messages 无 OpenAI output logprobs 等价能力），对应数据被丢弃",
			"field", "top_logprobs",
			"value", req.TopLogprobs.Value,
			"impact", "logprobs 不会返回")
	}
	// deprecated user：OpenAI 已废弃，需决定忽略或映射 metadata。
	if req.User.Valid() && req.User.Value != "" {
		slog.Warn("忽略 deprecated 字段 user（OpenAI 已废弃，建议改用 safety_identifier / metadata.user_id），对应数据被丢弃",
			"field", "user",
			"impact", "不会传递给上游（可改用 metadata.user_id）")
	}
}

// echoFromRequest 保留包内调用点，实现统一走 convert.EchoFromRequest。
func echoFromRequest(req *oairesponses.ResponseNewParams) model.ResponseObjectParams {
	return convert.EchoFromRequest(req)
}

// shouldSummarizeReasoning 与 AnthropicBackend / convert.applyReasoning 对齐：
// 仅 reasoning.summary==concise 时使用 reasoning_summary_* 事件格式。
func shouldSummarizeReasoning(req *oairesponses.ResponseNewParams) bool {
	return string(req.Reasoning.Summary) == model.ReasoningSummaryConcise
}

// itemType 返回 input item 的人类可读类型名称，用于日志。
func itemType(it *oairesponses.ResponseInputItemUnionParam) string {
	if it.OfMessage != nil {
		return model.ItemTypeMessage
	}
	if it.OfReasoning != nil {
		return model.ItemTypeReasoning
	}
	if it.OfFunctionCall != nil {
		return model.ItemTypeFunctionCall
	}
	if it.OfFunctionCallOutput != nil {
		return model.ItemTypeFunctionCallOutput
	}
	if it.OfCustomToolCall != nil {
		return model.ItemTypeCustomToolCall
	}
	if it.OfCustomToolCallOutput != nil {
		return model.ItemTypeCustomToolCallOut
	}
	if it.OfToolSearchCall != nil {
		return model.ItemTypeToolSearchCall
	}
	if it.OfToolSearchOutput != nil {
		return model.ItemTypeToolSearchOutput
	}
	if it.OfCompaction != nil {
		return model.ItemTypeCompaction
	}
	if it.OfCompactionTrigger != nil {
		return model.ItemTypeCompactionTrigger
	}
	if typ := it.GetType(); typ != nil && *typ != "" {
		return *typ
	}
	return "unknown"
}

func inputItemTypeCountAttrs(items []oairesponses.ResponseInputItemUnionParam) []any {
	counts := map[string]int{}
	for i := range items {
		counts[itemType(&items[i])]++
	}
	keys := []string{
		model.ItemTypeMessage,
		model.ItemTypeReasoning,
		model.ItemTypeFunctionCall,
		model.ItemTypeFunctionCallOutput,
		model.ItemTypeCustomToolCall,
		model.ItemTypeCustomToolCallOut,
		model.ItemTypeToolSearchCall,
		model.ItemTypeToolSearchOutput,
		model.ItemTypeCompaction,
		model.ItemTypeCompactionTrigger,
		"unknown",
	}
	attrs := make([]any, 0, len(keys))
	for _, key := range keys {
		if counts[key] > 0 {
			attrs = append(attrs, slog.Int(key, counts[key]))
		}
	}
	return attrs
}

// toolSummaryAttrs 统计请求 tools[] 的类型分布，并展开 mcp tool 的 server 明细
// （label/url/connector_id）与 client tool 名字，用于诊断日志：回答"Codex 发了
// 哪些 tool 类型、有没有 type=mcp、mcp 长什么样、其余 tool 叫什么名字"。
// MCP 链路定位的关键观测点（请求入口）。
func toolSummaryAttrs(tools []oairesponses.ToolUnionParam) []any {
	counts := map[string]int{}
	// toolDetails 收集非 function/custom（即结构化、有明细可打印的）tool 的诊断串：
	// mcp server、tool_search 的 execution、namespace 的子工具列表。
	var toolDetails []string
	var clientToolNames []string
	for _, t := range tools {
		switch {
		case t.OfMcp != nil:
			counts["mcp"]++
			m := t.OfMcp
			serverURL := ""
			if m.ServerURL.Valid() {
				serverURL = m.ServerURL.Value
			}
			toolDetails = append(toolDetails, fmt.Sprintf("mcp label=%s url=%s connector=%s", m.ServerLabel, serverURL, m.ConnectorID))
		case t.OfFunction != nil:
			counts["function"]++
			clientToolNames = append(clientToolNames, t.OfFunction.Name)
		case t.OfCustom != nil:
			counts["custom"]++
			clientToolNames = append(clientToolNames, t.OfCustom.Name)
		case t.OfWebSearch != nil:
			counts["web_search"]++
		case t.OfWebSearchPreview != nil:
			counts["web_search_preview"]++
		case t.OfCodeInterpreter != nil:
			counts["code_interpreter"]++
		case t.OfApplyPatch != nil:
			counts["apply_patch"]++
		case t.OfShell != nil:
			counts["shell"]++
		case t.OfLocalShell != nil:
			counts["local_shell"]++
		case t.OfToolSearch != nil:
			counts["tool_search"]++
			toolDetails = append(toolDetails, fmt.Sprintf("tool_search execution=%s", t.OfToolSearch.Execution))
		case t.OfNamespace != nil:
			counts["namespace"]++
			ns := t.OfNamespace
			var nsChildren []string
			for _, nt := range ns.Tools {
				if nt.OfFunction != nil {
					nsChildren = append(nsChildren, nt.OfFunction.Name)
				} else if nt.OfCustom != nil {
					nsChildren = append(nsChildren, nt.OfCustom.Name)
				}
			}
			toolDetails = append(toolDetails, fmt.Sprintf("namespace=%s children=[%s]", ns.Name, strings.Join(nsChildren, ",")))
		default:
			counts["other"]++
		}
	}
	keys := []string{"mcp", "function", "custom", "web_search", "web_search_preview", "code_interpreter", "apply_patch", "shell", "local_shell", "tool_search", "namespace", "other"}
	attrs := []any{slog.Int("total", len(tools))}
	for _, k := range keys {
		if counts[k] > 0 {
			attrs = append(attrs, slog.Int(k, counts[k]))
		}
	}
	if len(toolDetails) > 0 {
		attrs = append(attrs, slog.String("structured_tools_detail", strings.Join(toolDetails, " | ")))
	}
	if len(clientToolNames) > 0 {
		attrs = append(attrs, slog.String("client_tool_names", strings.Join(clientToolNames, ",")))
	}
	return attrs
}

// summarizeAnthropicRequest counts block/role distribution in a converted
// Anthropic request for diagnostics: reasoning signature health (empty
// signatures violate Anthropic's thinking round-trip rules and can corrupt
// multi-turn thinking context), tool-loop balance, and context volume.

func newResponseID() string {
	return fmt.Sprintf("resp_%d", time.Now().UnixNano())
}

// recordUpstream 返回一个 scheduler.OnUpstream 回调，把单次上游尝试事件
// 映射为 metrics.RequestEvent（Kind=KindUpstream）投递给观测台。
// 每次 trySource 结束时调用：成功一条 completed，失败一条 failed。
// scheduler 不依赖 metrics 包（分层约束），由 server（L4）做桥接。
func (s *Server) recordUpstream() scheduler.OnUpstream {
	return func(ev scheduler.UpstreamEvent) {
		s.metrics.Record(metrics.RequestEvent{
			Kind:          metrics.KindUpstream,
			StartedAt:     ev.StartedAt,
			Duration:      ev.Duration,
			TTFB:          ev.TTFB,
			SourceName:    ev.SourceName,
			Model:         ev.Model,
			ResolvedModel: ev.ResolvedModel,
			Status:        ev.Status,
			Code:          ev.Code,
			InputTokens:   ev.InputTokens,
			OutputTokens:  ev.OutputTokens,
			CacheRead:     ev.CacheRead,
			CacheCreate:   ev.CacheCreate,
			Error:         ev.Error,
			Attempt:       ev.Attempt,
			BackendType:   ev.BackendType,
		})
	}
}

// isClientCanceled 判断 execErr 是否由客户端断开（请求 ctx 取消）引起。
// 与 scheduler.isClientCanceled 同语义；server 不依赖 scheduler 未导出 helper。
func isClientCanceled(ctx context.Context, err error) bool {
	if err == nil || ctx == nil {
		return false
	}
	if ctx.Err() == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ctx.Err())
}

// errSummary 返回上游错误全文，供观测台 tip 展示完整上游返回。
func errSummary(err error) string {
	if err == nil {
		return ""
	}
	// 保留上游完整返回（含 JSON body），观测台 tip 需要全量信息。
	return err.Error()
}

// clientFailCode 从 execErr 解析上游 HTTP 状态码；解析不到（网络错误/取消）回退 500。
// 客户端失败记录展示最终失败原因对应的上游码，便于观测台对齐限流/鉴权等场景。
func clientFailCode(err error) int {
	if sc := statusCodeFromErr(err); sc != 0 {
		return sc
	}
	return 500
}

// statusCodeFromErr 从 anthropic client 错误串解析上游 HTTP 状态码。
// 错误格式固定为 "anthropic upstream %d: ..."；解析失败返回 0。
func statusCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	const prefix = "anthropic upstream "
	s := err.Error()
	i := strings.Index(s, prefix)
	if i < 0 {
		return 0
	}
	rest := s[i+len(prefix):]
	n := 0
	for _, ch := range rest {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int(ch-'0')
	}
	if n >= 100 && n <= 599 {
		return n
	}
	return 0
}
