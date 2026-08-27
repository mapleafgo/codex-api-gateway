// Package server wires config, scheduler, and HTTP handlers
// into a single /v1/responses endpoint that translates OpenAI Responses API
// requests to Anthropic Messages streams and back.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/convert"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/metrics"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
	"github.com/mapleafgo/codex-api-gateway/internal/scheduler"
	oairesponses "github.com/openai/openai-go/v3/responses"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

// Server wires config, scheduler, and HTTP handlers.
type Server struct {
	holder    *config.Holder
	sch       *scheduler.Scheduler
	metrics   *metrics.Collector
	reg       *plugin.Registry
	handlerWg sync.WaitGroup // 追踪 handleResponses goroutine 生命周期，供测试同步
}

// New builds a Server.
// reg 是已注册源插件注册表，由唯一组装入口注入；调度器按稳定 ID 分发。
func New(cfg *config.Config, reg *plugin.Registry) *Server {
	holder := config.NewHolder(cfg)
	slog.Info("初始化服务组件", "sources", len(cfg.Sources))
	return &Server{
		holder:  holder,
		sch:     scheduler.New(holder, reg),
		metrics: metrics.New(),
		reg:     reg,
	}
}

// Holder 返回内部的 Holder，供 admin 包挂载热重载回调。
func (s *Server) Holder() *config.Holder { return s.holder }

// Metrics 返回内部的 metrics Collector，供 admin 包读取。
func (s *Server) Metrics() *metrics.Collector { return s.metrics }

// Scheduler 返回内部的 Scheduler，供 admin 包触发 Reload。
func (s *Server) Scheduler() *scheduler.Scheduler { return s.sch }

// WaitForHandlers 等待所有 handleResponses goroutine 完成（供测试使用）。
func (s *Server) WaitForHandlers() { s.handlerWg.Wait() }

// ReloadScheduler 让外层（configwatch）通知 scheduler 重建运行时优先级。
func (s *Server) ReloadScheduler() (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("reload scheduler: %v", recovered)
		}
	}()
	s.sch.Reload()
	return nil
}

// Close 停止 metrics consumer，等待已接收事件聚合完成；重复调用安全。
func (s *Server) Close() error {
	if s.metrics != nil {
		s.metrics.Stop()
	}
	return nil
}

// Handler 返回 http.Handler（委托 Mux），供测试直接创建 httptest.Server。
func (s *Server) Handler() http.Handler { return s.Mux() }

// Mux 返回 *http.ServeMux 供 main 挂载额外路由（admin 等）。
// 返回值同时满足 http.Handler 接口；多次调用创建多个独立 mux，建议只调用一次。
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
	resp := s.codexModels(names)
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("写出模型列表响应失败", "error", err)
	} else {
		slog.Info("模型列表响应完成", "models", len(resp.Models))
	}
}

// codexModels 按当前配置构建 Codex 模型目录，保证 /v1/models 与
// models.json 使用同一份模型信息。
func (s *Server) codexModels(names []string) model.CodexModelsResponse {
	infos := make([]model.CodexModelInfo, 0, len(names))
	for i, name := range names {
		info := s.codexModelInfo(name)
		// Codex 按 priority 升序排序（越小越优先、默认选首项），
		// 配置中越靠前的模型分到越小数字，保证客户端按配置顺序选主模型。
		info.Priority = i + 1
		infos = append(infos, info)
	}
	return model.CodexModelsResponse{Models: infos}
}

// ModelCatalogJSON 返回与 /v1/models 完全一致的模型目录 JSON，
// 供管理页/配置同步写入 $CODEX_HOME/models.json。
func (s *Server) ModelCatalogJSON() ([]byte, error) {
	cur := s.holder.Current()
	return json.Marshal(s.codexModels(cur.ConfiguredModelSlugs()))
}

// codexModelInfo 为一个模型 slug 构造 Codex ModelInfo，补齐网关所需的全部字段。
// 字段对齐 codex-rs/protocol/src/openai_models.rs 的 ModelInfo（0.144.5），
// 字段语义与 serde 约束详见 internal/model/modellist.go 的注释。
//
// 网关默认值策略（逐项确认后的结论）：
//
//	必填字段：
//	  - base_instructions：来自 config 同级 base_instructions.md（非空时整体替换 Codex
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
//	  - include_skills_usage_instructions=true（base_instructions 为空时注入，引导 skill 发现；
//	    非空时 false，避免与基线指令重复）
//	  - web_search_tool_type="text_and_image"（声明支持文本+图片 web 搜索）
//	  - input_modalities=["text","image"]（默认按多模态声明）
//	  - default_reasoning_summary="auto"（固定声明默认 summary 模式，不让客户端省略推断）
//	  - tool_mode="direct"：强制标准工具模式（function 工具独立发送、模型直接调用），
//	    覆盖 Codex features.code_mode* 配置。第三方上游（GLM 等）无 code_mode DSL
//	    训练，显式 direct 比依赖客户端默认更稳，避免误走 code_mode 造成降级。
//	  - multi_agent_version="v2"：Codex 多 agent 是纯客户端本地编排（进程内创建
//	    子 thread/session，子 agent 经网关发 /v1/responses 请求），不依赖上游编排
//	    后端。声明 v2 让 spawn_agent/wait_agent 等工具 Direct 可见；不声明默认 V1，
//	    工具在 supports_search_tool=true 时变 Deferred，模型需 tool_search 才发现。
//	  - comp_hash="3000"：对齐 gpt-5.6 catalog 压缩兼容哈希。连续两 turn 不同会
//	    触发 Codex 前置压缩；网关统一注入，避免客户端切模型时误触发。
//	不注入（保持零值）：
//	  - default_reasoning_level / service_tiers / default_service_tier 等
//
// config.yaml models.<slug> 可覆盖 context_window / accepts_image / supports_image_detail_original；
// 其余字段硬编码统一注入。
func (s *Server) codexModelInfo(slug string) model.CodexModelInfo {
	emptyStr := ""
	freeformApplyPatch := "freeform"
	ctxWindow := int64(200000)
	toolMode := "direct"
	multiAgentV2 := "v2"
	compHash := "3000"
	responsesLiteOff := false
	baseInstructions := s.holder.Current().BaseInstructions
	info := model.CodexModelInfo{
		// —— 必填字段 ——
		Slug:                     slug,
		DisplayName:              strings.ToUpper(slug),
		Description:              &emptyStr,
		SupportedReasoningLevels: defaultReasoningLevels(),
		ShellType:                "shell_command",
		Visibility:               "list",
		SupportedInAPI:           true,
		// Priority 由 handleModels 按顺序分配，codexModelInfo 不设固定值.
		AvailabilityNux:            nil,
		Upgrade:                    nil,
		BaseInstructions:           baseInstructions,
		SupportsReasoningSummaries: true,
		DefaultReasoningSummary:    model.ReasoningSummaryAuto,
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
		IncludeSkillsUsageInstructions: baseInstructions == "",
		WebSearchToolType:              "text_and_image",
		InputModalities:                []string{"text", "image"},
		// tool_mode="direct"：强制标准工具调用模式（function 工具独立发送、模型直接
		// 调用），覆盖 Codex features.code_mode* 配置。第三方上游无 code_mode DSL 训练，
		// 显式 direct 避免客户端默认走 code_mode 造成降级。
		ToolMode: &toolMode,
		// multi_agent_version="v2"：Codex 多 agent 完全是客户端本地编排（进程内创建
		// 子 thread/session，子 agent 通过网关发 /v1/responses 请求），不依赖上游编排
		// 后端。声明 v2 让 spawn_agent/wait_agent 等工具 Direct 可见（模型直接调用）；
		// 不声明则默认走 V1（Collab），工具在 supports_search_tool=true 时变 Deferred，
		// 模型需先 tool_search 才能发现，很少主动 spawn。
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
	if baseInstructions != "" {
		info.ModelMessages = &model.CodexModelMessages{InstructionsTemplate: &baseInstructions}
	}

	// 应用 config.yaml models.<slug> 覆盖（仅覆盖显式配置的字段）
	if ov, ok := s.resolveModelOverride(slug); ok {
		applyModelOverride(&info, &ov)
	}
	return info
}

// defaultReasoningLevels 返回网关默认注入的 reasoning effort 预设列表。
// 覆盖 Anthropic output_config.effort 全部档位，并保留 none 关闭思考。
// 空 [] 合法但表示模型不支持 reasoning；不开放 per-slug 覆盖。
func defaultReasoningLevels() []model.CodexReasoningLevel {
	return []model.CodexReasoningLevel{
		{Effort: model.ReasoningEffortNone, Description: "关闭推理"},
		{Effort: model.ReasoningEffortLow, Description: "快速响应，轻量推理"},
		{Effort: model.ReasoningEffortMedium, Description: "平衡模式，常规推理"},
		{Effort: model.ReasoningEffortHigh, Description: "深入推理，适合复杂任务"},
		{Effort: model.ReasoningEffortXhigh, Description: "超高强度推理"},
		{Effort: model.ReasoningEffortMax, Description: "最大强度推理"},
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
// 开放 context_window / accepts_image / supports_image_detail_original；其余能力固定注入。
func applyModelOverride(info *model.CodexModelInfo, ov *config.ModelOverride) {
	// context_window 同时应用到 ContextWindow 与 MaxContextWindow：Codex ModelInfo 协议
	// 要求两个字段，网关场景二者相等，config 只暴露一个 context_window 输入。
	if ov.ContextWindow != nil {
		info.ContextWindow = ov.ContextWindow
		info.MaxContextWindow = ov.ContextWindow
	}
	// AcceptsImage 控制 input_modalities：true/nil 含 image（能识别图片），
	// false 仅 text（Codex 发请求前会 strip 掉历史里的所有图片）。
	if ov.AcceptsImage != nil && !*ov.AcceptsImage {
		info.InputModalities = []string{"text"}
	}
	if ov.SupportsImageDetailOriginal != nil {
		info.SupportsImageDetailOriginal = *ov.SupportsImageDetailOriginal
	}
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handlerWg.Add(1)
	defer s.handlerWg.Done()

	r = r.WithContext(logging.WithRequestID(r.Context(), logging.NewRequestID()))
	log := logging.FromContext(r.Context())
	if r.Method != http.MethodPost {
		log.Warn("拒绝响应请求", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reqStart := time.Now()
	// 防误伤：限制请求体大小，避免超大 body 撑爆本机内存。
	maxBody := s.holder.Current().Server.MaxBodyBytes()
	if maxBody > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// MaxBytesReader 超限时返回 *http.MaxBytesError
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			log.Warn("响应请求体超过上限", "max_body_bytes", maxBody, "error", err)
			http.Error(w, fmt.Sprintf("request body too large (max %d MiB)", maxBody>>20), http.StatusRequestEntityTooLarge)
			return
		}
		log.Warn("读取响应请求体失败", "error", err)
		http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
		return
	}
	const debugBodyLimit = 4096
	bodySnapshot := body
	bodyTruncated := false
	if len(bodySnapshot) > debugBodyLimit {
		bodySnapshot = bodySnapshot[:debugBodyLimit]
		bodyTruncated = true
	}
	log.Debug("响应请求体快照",
		"body_snapshot", string(bodySnapshot),
		"body_bytes", len(body),
		"body_truncated", bodyTruncated)
	req, err := convert.DecodeResponseNewParams(body)
	if err != nil {
		log.Warn("解析响应请求体失败", "error", err)
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Info("收到响应请求",
		"method", r.Method,
		"path", r.URL.Path,
		"query", r.URL.RawQuery,
		"body_bytes", len(body),
		"model", string(req.Model),
		"input_items", len(req.Input.OfInputItemList),
		"input_string_len", len(req.Input.OfString.Value),
		"instructions_len", len(req.Instructions.Value),
		"max_output_tokens_set", req.MaxOutputTokens.Valid(),
		"max_output_tokens", req.MaxOutputTokens.Value,
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
		log.Debug("响应请求输入项", "index", i, "type", itemType(it), "role", role)
	}

	// 本次请求的预检统一使用同一份配置快照，避免热重载窗口里
	// 「零源判断用旧配置、首源预检用新配置」的错位。
	cur := s.holder.Current()
	ordered := cur.OrderedSources()
	if len(ordered) == 0 {
		// 零源配置：进程可启动但无上游可转发。返回 503 引导用户去管理页配置。
		log.Warn("转发请求被拒绝：未配置任何上游源")
		http.Error(w, "no upstream source configured; add one via admin page", http.StatusServiceUnavailable)
		return
	}
	s.warnDroppedOrIgnoredParams(log, req, ordered)

	// 预检：只调用首源插件可选实现的 RequestPreparer。插件不实现时保持
	// 旧行为（Chat 不预检，避免纯 Chat 部署被误杀）；错误直接 400，不建 SSE。
	first := ordered[0]
	if p, ok := s.reg.Get(s.backendIDFor(first)); ok {
		if prep, ok := p.Backend().(plugin.RequestPreparer); ok {
			if err := prep.PrepareRequest(r.Context(), &plugin.PrepareRequestInput{
				RawBody: body,
				Source:  first,
				Config:  cur,
			}); err != nil {
				log.Warn("预检响应请求失败", "source", first.Name, "backend", s.backendIDFor(first), "error", err)
				http.Error(w, "convert: "+err.Error(), http.StatusBadRequest)
				return
			}
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
	var sawTerminal bool
	terminalTypes := map[string]bool{
		string(oaconstant.ValueOf[oaconstant.ResponseCompleted]()):  true,
		string(oaconstant.ValueOf[oaconstant.ResponseFailed]()):     true,
		string(oaconstant.ValueOf[oaconstant.ResponseIncomplete]()): true,
	}
	resolvedModel := string(req.Model)
	backendType := string(plugin.BackendAnthropic)
	var lastUp scheduler.UpstreamEvent

	sourceName, execErr := s.sch.ExecuteGeneric(r.Context(), body,
		func(e model.SSEEvent) error {
			evCount++
			if terminalTypes[e.Type] {
				sawTerminal = true
			}
			log.Debug("写出响应 SSE 事件", "event_type", e.Type)
			if err := writeSSE(w, e); err != nil {
				log.Warn("写出响应 SSE 事件失败", "event_type", e.Type, "error", err)
				return err
			}
			flusher.Flush()
			return nil
		},
		func(ev scheduler.UpstreamEvent) {
			lastUp = ev
			if ev.ResolvedModel != "" {
				resolvedModel = ev.ResolvedModel
			}
			if ev.Backend != "" {
				backendType = ev.Backend
			}
			s.recordUpstream(ev)
		},
	)

	// Backend 已在 SSE 流内写入真实 response id；此处 id 仅用于日志/metrics 关联。
	respID := newResponseID()
	clientCanceled := plugin.IsClientCanceled(r.Context(), execErr)
	serverTimeout := errors.Is(execErr, plugin.ErrUpstreamTimeout)

	status := model.ResponseStatusCompleted
	if lastUp.Status == "incomplete" {
		status = "incomplete"
	}
	code := 200
	errText := ""
	// 上游已给出业务终态后，客户端断开或读尾噪声不得覆盖该终态。
	upstreamSucceeded := lastUp.Status == "completed" || lastUp.Status == "incomplete"
	// 网关侧单笔总时长超时：failed + 504；流内尚未出现终态事件时补发 failed
	// （a/c 后端通常已产出；r 透传不代补上游事件，由这里兜底收口）。
	writeFailedTerminal := func() {
		errResp := model.NewResponseObject(respID, model.ResponseStatusFailed, string(req.Model), time.Now().Unix(), echoFromRequest(req))
		errResp.Output = []model.OutputItem{}
		errResp.Error = failedResponseError(execErr)
		evType := string(oaconstant.ValueOf[oaconstant.ResponseFailed]())
		if err := writeSSE(w, model.MarshalEvent(evType, model.TerminalResponseEvent{
			Type: evType, SequenceNumber: 1, Response: errResp,
		})); err != nil {
			log.Warn("写出失败终态 SSE 事件失败", "event_type", evType, "error", err)
		}
		flusher.Flush()
	}
	if serverTimeout {
		status = model.ResponseStatusFailed
		code = http.StatusGatewayTimeout
		errText = lastUp.Error
		if errText == "" {
			errText = errSummary(execErr)
		}
		if !sawTerminal {
			writeFailedTerminal()
		}
		log.Warn("单笔源请求超时终止",
			"response_id", respID,
			"status", status,
			"source", sourceName,
			"backend", backendType,
			"upstream_events", evCount,
			"elapsed", time.Since(reqStart).String(),
			"reason", "timeout",
			"error", errText)
	} else if lastUp.Status == "failed" && evCount > 0 {
		status = model.ResponseStatusFailed
		code = lastUp.Code
		errText = lastUp.Error
		log.Error("响应请求失败",
			"response_id", respID,
			"status", status,
			"source", sourceName,
			"backend", backendType,
			"upstream_events", evCount,
			"elapsed", time.Since(reqStart).String(),
			"error", errText)
	} else if execErr == nil || upstreamSucceeded {
		if execErr != nil && !clientCanceled {
			// 对齐旧路径：终态后读取失败只 WARN，不当作业务 failed。
			log.Warn("上游流终态后读取失败",
				"response_id", respID,
				"source", sourceName,
				"elapsed", time.Since(reqStart).String(),
				"error", execErr)
		}
		log.Info("响应请求完成",
			"response_id", respID,
			"status", status,
			"source", sourceName,
			"backend", backendType,
			"upstream_events", evCount,
			"cache_read_tokens", lastUp.CacheRead,
			"cache_creation_tokens", lastUp.CacheCreate,
			"elapsed", time.Since(reqStart).String())
	} else if clientCanceled {
		status = "canceled"
		code = 499
		errText = errSummary(execErr)
		log.Info("响应请求被客户端取消", "response_id", respID, "source", sourceName, "backend", backendType, "upstream_events", evCount, "elapsed", time.Since(reqStart).String(), "error", execErr)
	} else {
		status = model.ResponseStatusFailed
		// 流已建立（有事件）时对齐旧语义：默认 200，能解析上游码再覆盖。
		if evCount > 0 {
			code = 200
			if sc := plugin.StatusCodeFromErr(execErr); sc != 0 {
				code = sc
			}
		} else {
			code = clientFailCode(execErr)
		}
		errText = errSummary(execErr)
		log.Error("响应请求失败", "response_id", respID, "status", "failed", "source", sourceName, "backend", backendType, "elapsed", time.Since(reqStart).String(), "error", execErr)
		// 若流尚未写出任何事件，补一条 failed（Backend 通常已写）
		if evCount == 0 {
			writeFailedTerminal()
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
		Backend:       backendType,
	})
}

func writeSSE(w io.Writer, e model.SSEEvent) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, e.Data)
	return err
}

// warnDroppedOrIgnoredParams 对当前不语义映射、后端无等价能力、
// 或 deprecated 的请求字段统一输出 WARN 级别结构化日志，避免静默丢弃。
// 约定见 AGENTS.md「静默跳过与降级处理约定」。
func (s *Server) warnDroppedOrIgnoredParams(log *slog.Logger, req *oairesponses.ResponseNewParams, sources []config.Source) {
	// r 源可形状透传：跳过 a/c 路径「WARN + 丢弃」叙事，避免误报。
	if s.hasEnabledResponsesBackend(sources) {
		if req.PreviousResponseID.Valid() && req.PreviousResponseID.Value != "" {
			log.Info("previous_response_id 将透传上游；网关不代补会话历史",
				"field", "previous_response_id",
				"previous_response_id", req.PreviousResponseID.Value,
				"impact", "backend=openai-responses 时原样转发；网关无 session store")
		}
		return
	}
	// deprecated reasoning.generate_summary：被 reasoning.summary 取代。
	//nolint:staticcheck // 字段被 OpenAI 标记 deprecated，但我们正是要检测它以输出 WARN
	if req.Reasoning.GenerateSummary != "" {
		log.Warn("忽略 deprecated 字段 reasoning.generate_summary（已由 reasoning.summary 取代），对应数据被丢弃",
			"field", "reasoning.generate_summary",
			"value", string(req.Reasoning.GenerateSummary),
			"reasoning_summary", string(req.Reasoning.Summary),
			"impact", "generate_summary 不生效，请改用 reasoning.summary")
	}
	// previous_response_id：网关无 session store，不做 enrich。
	// Codex 主路径不传此字段（客户端自带完整 input 回灌）；其它客户端若依赖链式会话会失效。
	if req.PreviousResponseID.Valid() && req.PreviousResponseID.Value != "" {
		log.Warn("忽略 previous_response_id（网关无 session store，不做历史 enrich），对应数据被丢弃",
			"field", "previous_response_id",
			"previous_response_id", req.PreviousResponseID.Value,
			"impact", "不会按 response_id 补全历史；请在 input 中完整回灌上下文")
	}
	// service_tier：Chat 源（backend=openai-chat）透传；Anthropic 源无等价，仍忽略。
	// 此处 INFO 提示路径差异，避免「全局永不透传」误导（混排 failover 时以实际 Backend 为准）。
	if req.ServiceTier != "" {
		log.Info("service_tier 仅 Chat 源透传，Anthropic 源忽略",
			"field", "service_tier",
			"value", string(req.ServiceTier),
			"impact", "backend=openai-chat 写入 Chat body；backend=anthropic 不传上游")
	}
	// text.verbosity：Chat 源透传；Anthropic 无原生参数。
	if req.Text.Verbosity != "" {
		log.Info("text.verbosity 仅 Chat 源透传，Anthropic 源忽略",
			"field", "text.verbosity",
			"value", string(req.Text.Verbosity),
			"impact", "backend=openai-chat 写入 Chat verbosity；backend=anthropic 不传上游")
	}
	// truncation：Anthropic 无直接等价策略，仅在响应中 echo。
	// truncation 状态为 raw_preserved：值在响应对象中 echo 回显，未被丢弃，
	// 不触发 WARN（AGENTS.md 的静默跳过约定针对丢弃场景，不针对 echo）。

	// include：按 include 项分档处理。
	//   - satisfied：网关默认已发出对应字段（如 web_search sources），
	//     无需额外行为，也不 WARN；
	//   - encrypted_content：已通过 disable_response_storage 路径处理，不 WARN；
	//   - chat_only：仅 Chat 上游可映射（如 message.output_text.logprobs）；
	//   - unsupported：后端无等价能力（file_search、computer 等），WARN + 丢弃。
	if len(req.Include) > 0 {
		satisfied := map[string]bool{
			"reasoning.encrypted_content":    true, // ZDR 路径已处理
			"web_search_call.action.sources": true, // action.sources 默认下发
			"message.input_image.image_url":  true, // 输入 image_url 原样保留
		}
		chatOnly := map[string]bool{
			// Chat 路径 chatstreamconv 映射 token logprobs；需请求 top_logprobs 且上游返回。
			"message.output_text.logprobs": true,
		}
		hasChat := false
		for _, src := range sources {
			if s.sourceHasCapability(src, plugin.CapabilityChatCompletions) {
				hasChat = true
				break
			}
		}
		var unsupported []string
		var chatSkipped []string
		for _, inc := range req.Include {
			s := string(inc)
			if satisfied[s] {
				continue
			}
			if chatOnly[s] {
				if hasChat {
					continue // Chat 源可映射；混排时以实际 Backend 为准
				}
				chatSkipped = append(chatSkipped, s)
				continue
			}
			unsupported = append(unsupported, s)
		}
		if len(chatSkipped) > 0 {
			log.Warn("忽略仅 Chat 源可映射的 include 项（当前无 Chat 上游）",
				"field", "include",
				"values", strings.Join(chatSkipped, ","),
				"impact", "backend=openai-chat 且 top_logprobs 时才有 output_text.logprobs；当前配置无法生效")
		}
		if len(unsupported) > 0 {
			log.Warn("忽略无后端等价能力的 include 项，对应数据被丢弃",
				"field", "include",
				"values", strings.Join(unsupported, ","),
				"impact", "该 include 项不会生效（file_search / computer 等）")
		}
	}
	// metadata：Chat 整表透传；a 路径仅 user_id 进 Anthropic metadata，其余 echo。
	if len(req.Metadata) > 0 {
		nonUserID := 0
		for k := range req.Metadata {
			if k != "user_id" {
				nonUserID++
			}
		}
		if nonUserID > 0 {
			log.Info("metadata 按后端分流（c 整表透传；a 仅 user_id + 响应 echo）",
				"field", "metadata",
				"entries", len(req.Metadata),
				"impact", "backend=openai-chat 写入 Chat metadata；backend=anthropic 仅 metadata.user_id 进上游")
		}
	}
	// prompt_cache_*：Anthropic 用内容 hash + 网关自主 cache_control，不认 OpenAI client key/options/retention。
	if req.PromptCacheKey.Valid() && req.PromptCacheKey.Value != "" {
		// Codex 常发；网关已自主 cache_control，属可控协议差异 → DEBUG 即可。
		// a 路径：网关自主 cache_control；c 路径：chatconvert 丢弃（Chat 无顶层槽位）。
		log.Debug("prompt_cache_key 按后端分流（a 忽略/自主 cache_control；c 丢弃）",
			"field", "prompt_cache_key",
			"impact", "Anthropic 源不按 OpenAI cache key 分桶；Chat 源由 chatconvert 丢弃（Chat 无顶层槽位）")
	}
	if req.PromptCacheOptions.Mode != "" || req.PromptCacheOptions.Ttl != "" {
		// 与 prompt_cache_key 同属可控协议差异，网关已自主 cache_control → DEBUG。
		// a 路径：自主 cache_control；c 路径：Chat 请求体无顶层槽位，chatconvert 丢弃。
		log.Debug("prompt_cache_options 按后端分流（a 忽略/自主 cache_control；c 丢弃）",
			"field", "prompt_cache_options",
			"mode", req.PromptCacheOptions.Mode,
			"ttl", req.PromptCacheOptions.Ttl,
			"impact", "Anthropic 源不按 OpenAI options 调缓存；Chat 请求体无 prompt_cache_options 槽位")
	}
	if req.PromptCacheRetention != "" {
		// deprecated 字段且语义不等价；网关固定 5m TTL，DEBUG 即可。
		log.Debug("忽略 prompt_cache_retention（deprecated；与 Anthropic cache_control 语义不同）",
			"field", "prompt_cache_retention",
			"value", string(req.PromptCacheRetention),
			"impact", "不会按 in_memory/24h 调整上游缓存保留；请用网关 cache_control TTL 配置")
	}
	// prompt：引用 OpenAI prompt template，网关无服务端模板存储。
	if req.Prompt.ID != "" {
		log.Warn("忽略 prompt（网关无 OpenAI prompt 模板存储能力），对应数据被丢弃",
			"field", "prompt",
			"prompt_id", req.Prompt.ID,
			"impact", "模板与变量不会被解析，input 以实际内容为准")
	}
	// background：当前网关只支持同步 SSE。
	if req.Background.Valid() && req.Background.Value {
		log.Warn("忽略 background=true（网关仅支持同步 SSE），请求将按同步处理",
			"field", "background",
			"impact", "请求不会被转为后台执行")
	}
	// conversation：网关无状态，不是 OpenAI Conversation API。
	if req.Conversation.OfString.Valid() || req.Conversation.OfConversationObject != nil {
		log.Warn("忽略 conversation（网关非 OpenAI Conversation API），对应数据被丢弃",
			"field", "conversation",
			"impact", "不会使用 conversation 拉取历史")
	}
	// context_management：OpenAI 服务端自动压缩；网关不做服务端压缩，压缩由 Codex local 承担。
	if len(req.ContextManagement) > 0 {
		types := make([]string, 0, len(req.ContextManagement))
		for _, cm := range req.ContextManagement {
			types = append(types, cm.Type)
		}
		log.Warn("忽略 context_management（服务端自动压缩不适用，压缩由 Codex 客户端 local 完成），对应数据被丢弃",
			"field", "context_management",
			"types", strings.Join(types, ","),
			"impact", "服务端压缩策略不生效；客户端按模型目录 context_window/auto_compact_token_limit 自行压缩")
	}
	// max_tool_calls：Anthropic 无直接请求参数。
	if req.MaxToolCalls.Valid() {
		log.Warn("忽略 max_tool_calls（Anthropic 无等价请求参数，网关不做计数截断），对应数据被丢弃",
			"field", "max_tool_calls",
			"value", req.MaxToolCalls.Value,
			"impact", "工具调用次数不会在网关层被截断")
	}
	// safety_identifier：Chat 透传；Anthropic 无等价。
	if req.SafetyIdentifier.Valid() && req.SafetyIdentifier.Value != "" {
		log.Info("safety_identifier 仅 Chat 源透传，Anthropic 源忽略",
			"field", "safety_identifier",
			"impact", "backend=openai-chat 写入 Chat body；backend=anthropic 不传上游")
	}
	// moderation：Chat 透传；Anthropic 无等价。
	if req.Moderation.Model != "" ||
		req.Moderation.Policy.Input.Mode != "" ||
		req.Moderation.Policy.Output.Mode != "" {
		log.Info("moderation 仅 Chat 源透传，Anthropic 源忽略",
			"field", "moderation",
			"moderation_model", req.Moderation.Model,
			"input_mode", req.Moderation.Policy.Input.Mode,
			"output_mode", req.Moderation.Policy.Output.Mode,
			"impact", "backend=openai-chat 写入 Chat moderation；backend=anthropic 不传上游")
	}
	// stream_options.include_obfuscation：Chat 透传；Anthropic 无等价。
	if req.StreamOptions.IncludeObfuscation.Valid() {
		log.Info("stream_options.include_obfuscation 仅 Chat 源透传，Anthropic 源忽略",
			"field", "stream_options.include_obfuscation",
			"value", req.StreamOptions.IncludeObfuscation.Value,
			"impact", "backend=openai-chat 写入 Chat stream_options；backend=anthropic 不传上游")
	}
	// top_logprobs：Chat 透传（并开 logprobs）；Anthropic 无等价。
	if req.TopLogprobs.Valid() {
		log.Info("top_logprobs 仅 Chat 源透传，Anthropic 源忽略",
			"field", "top_logprobs",
			"value", req.TopLogprobs.Value,
			"impact", "backend=openai-chat 写入 Chat logprobs/top_logprobs；backend=anthropic 不返回 logprobs")
	}
	// deprecated user：OpenAI 已废弃，需决定忽略或映射 metadata。
	if req.User.Valid() && req.User.Value != "" {
		log.Warn("忽略 deprecated 字段 user（OpenAI 已废弃，建议改用 safety_identifier / metadata.user_id），对应数据被丢弃",
			"field", "user",
			"impact", "不会传递给上游（可改用 metadata.user_id）")
	}
}

// echoFromRequest 保留包内调用点，实现统一走 convert.EchoFromRequest。
func echoFromRequest(req *oairesponses.ResponseNewParams) model.ResponseObjectParams {
	return convert.EchoFromRequest(req)
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
	keys := []string{"mcp", "function", "custom", "web_search", "code_interpreter", "apply_patch", "shell", "local_shell", "tool_search", "namespace", "other"}
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

func newResponseID() string {
	return fmt.Sprintf("resp_%d", time.Now().UnixNano())
}

// recordUpstream 把单次上游尝试事件映射为 metrics.RequestEvent
// （Kind=KindUpstream）投递给观测台。每次 trySource 结束时被调用：
// 成功一条 completed，失败一条 failed。
// scheduler 不依赖 metrics 包（分层约束），由 server（L4）做桥接。
func (s *Server) recordUpstream(ev scheduler.UpstreamEvent) {
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
		Backend:       ev.Backend,
	})
}

// backendIDFor 返回源配置声明的稳定 Backend ID；Config v2 下该字段必填。
func (s *Server) backendIDFor(src config.Source) string {
	return src.Backend
}

// sourceHasCapability 按插件描述符能力判断单个源，不再比较源身份短码。
func (s *Server) sourceHasCapability(src config.Source, want plugin.Capability) bool {
	if s.reg == nil {
		return false
	}
	p, ok := s.reg.Get(s.backendIDFor(src))
	if !ok {
		return false
	}
	for _, c := range p.Descriptor().Capabilities {
		if c == want {
			return true
		}
	}
	return false
}

// hasEnabledCapability 判断当前配置是否含启用中的、承载指定能力的源。
func (s *Server) hasEnabledCapability(sources []config.Source, want plugin.Capability) bool {
	for _, src := range sources {
		if src.Disabled {
			continue
		}
		if s.sourceHasCapability(src, want) {
			return true
		}
	}
	return false
}

// hasEnabledResponsesBackend 判断当前配置是否含启用中的 Responses 透传源。
func (s *Server) hasEnabledResponsesBackend(sources []config.Source) bool {
	return s.hasEnabledCapability(sources, plugin.CapabilityResponsesPassthrough)
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
// 解析统一走 plugin.StatusCodeFromErr（支持 a/c/r 三类 client 的错误格式），
// 不在本包维护第二份前缀解析。
func clientFailCode(err error) int {
	if sc := plugin.StatusCodeFromErr(err); sc != 0 {
		return sc
	}
	return 500
}

// failedResponseError 构造 failed 终态的错误对象：保留上游原始诊断信息，上下文
// 超限时补上 Codex 可识别的 error.code，触发其下一轮会话自动压缩。
func failedResponseError(execErr error) *model.ResponseError {
	e := &model.ResponseError{Message: fmt.Sprintf("upstream: %v", execErr)}
	if code := plugin.ContextLengthExceededCode(execErr); code != "" {
		e.Code = code
	}
	return e
}
