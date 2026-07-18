// Package server wires config, session store, scheduler, and HTTP handlers
// into a single /v1/responses endpoint that translates OpenAI Responses API
// requests to Anthropic Messages streams and back.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	aconstant "github.com/anthropics/anthropic-sdk-go/shared/constant"
	anthropicclient "github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/convert"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/scheduler"
	"github.com/mapleafgo/codex-api-gateway/internal/store"
	"github.com/mapleafgo/codex-api-gateway/internal/streamconv"
	oairesponses "github.com/openai/openai-go/v3/responses"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

var (
	anContentBlockStart = string(aconstant.ValueOf[aconstant.ContentBlockStart]())
	anMessageStop       = string(aconstant.ValueOf[aconstant.MessageStop]())
)

// Server wires config, session store, scheduler, and HTTP handlers.
type Server struct {
	cfg       *config.Config
	sess      *store.SessionStore
	sch       *scheduler.Scheduler
	startedAt int64
}

// New builds a Server.
func New(cfg *config.Config) *Server {
	sess := store.New(cfg.Session.MaxBytes, cfg.Session.MaxEntryBytes, time.Duration(cfg.Session.TTL))
	if cfg.Session.Path != "" {
		var err error
		sess, err = store.Open(cfg.Session.Path, cfg.Session.MaxBytes, cfg.Session.MaxEntryBytes, time.Duration(cfg.Session.TTL))
		if err != nil {
			panic(fmt.Sprintf("open session store: %v", err))
		}
	}
	slog.Info("初始化服务组件",
		"session_path", cfg.Session.Path,
		"session_max_bytes", cfg.Session.MaxBytes,
		"session_max_entry_bytes", cfg.Session.MaxEntryBytes,
		"sources", len(cfg.Sources))
	return &Server{
		cfg:       cfg,
		sess:      sess,
		sch:       scheduler.New(cfg),
		startedAt: time.Now().Unix(),
	}
}

// Close releases server resources.
func (s *Server) Close() error {
	return s.sess.Close()
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
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

	// 收集所有模型 ID（上游 + 本地 model_map 别名），去重。
	seen := make(map[string]bool)

	// Codex 期望 /v1/models 返回 { "models": [ModelInfo] }（不是 OpenAI {data:[]}）。
	// Codex 用 serde_json::from_slice::<ModelsResponse> 直接解析，若返回 OpenAI 格式，
	// 解析失败/拿到空 ModelInfo → supports_search_tool 默认 false → MCP deferred 不工作。
	// 故返回 CodexModelsResponse，补全 ModelInfo 能力字段（关键是 supports_search_tool=true）。
	var infos []model.CodexModelInfo

	// 1) 上游模型列表
	if body, err := s.sch.ListModels(r.Context()); err != nil {
		slog.Warn("获取上游模型列表失败", "error", err)
	} else {
		defer body.Close()
		var am model.AnthropicModelsResponse
		if err := json.NewDecoder(body).Decode(&am); err != nil {
			slog.Warn("解析上游模型列表失败", "error", err)
		} else {
			for _, m := range am.Data {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				infos = append(infos, codexModelInfo(m.ID))
			}
		}
	}

	// 2) 本地 model_map 别名
	for _, name := range s.cfg.Models() {
		if seen[name] {
			continue
		}
		seen[name] = true
		infos = append(infos, codexModelInfo(name))
	}

	resp := model.CodexModelsResponse{Models: infos}
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("写出模型列表响应失败", "error", err)
	} else {
		slog.Info("模型列表响应完成", "models", len(infos))
	}
}

// codexModelInfo 为一个模型 slug 构造 Codex ModelInfo，补全能力字段。
// 关键：SupportsSearchTool=true 让 Codex 启用 tool_search + MCP tools deferred。
func codexModelInfo(slug string) model.CodexModelInfo {
	return model.CodexModelInfo{
		Slug:                              slug,
		DisplayName:                       slug,
		Description:                       "",
		SupportedReasoningLevels:          []any{},
		ShellType:                         "shell_command",
		Visibility:                        "list",
		SupportedInAPI:                    true,
		Priority:                          0,
		AvailabilityNux:                   nil,
		Upgrade:                           nil,
		BaseInstructions:                  "",
		SupportsReasoningSummaryParameter: true,
		DefaultReasoningSummary:           "auto",
		SupportVerbosity:                  false,
		DefaultVerbosity:                  nil,
		ApplyPatchToolType:                nil,
		WebSearchToolType:                 "text",
		TruncationPolicy: model.CodexTruncationPolicy{
			Mode: "tokens", Limit: 100000,
		},
		SupportsParallelToolCalls:  true,
		ExperimentalSupportedTools: []string{},
		InputModalities:            []string{"text", "image"},
		SupportsSearchTool:         true,
		UseResponsesLite:           false,
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
	prevID := ""
	prevIDPresent := req.PreviousResponseID.Valid() && req.PreviousResponseID.Value != ""
	if req.PreviousResponseID.Valid() {
		prevID = req.PreviousResponseID.Value
	}
	storeExplicit := req.Store.Valid()
	storeValue := false
	if storeExplicit {
		storeValue = req.Store.Value
	}
	storeEffective := shouldStoreResponse(req)
	slog.Info("收到响应请求",
		"method", r.Method,
		"path", r.URL.Path,
		"model", string(req.Model),
		"input_items", len(req.Input.OfInputItemList),
		"input_string_len", len(req.Input.OfString.Value),
		"instructions_len", len(req.Instructions.Value),
		"reasoning_effort", string(req.Reasoning.Effort),
		"reasoning_summary", string(req.Reasoning.Summary),
		"previous_response_id", prevID,
		"previous_response_id_present", prevIDPresent,
		"store_explicit", storeExplicit,
		"store_value", storeValue,
		"store_effective", storeEffective,
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

	ordered := s.cfg.OrderedSources()
	if len(ordered) > 0 {
		if _, _, _, err := s.buildAnthropicRequest(body, ordered[0]); err != nil {
			slog.Warn("预转换响应请求失败", "source", ordered[0].Name, "error", err)
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

	conv := streamconv.New()
	conv.SetEcho(echoFromRequest(req))
	conv.SetClientModel(string(req.Model))
	conv.SetCustomToolNames(convert.FreeformToolNames(req))
	conv.SetDeclaredServerTools(convert.DeclaredServerTools(req))
	// Codex TUI 只渲染 reasoning_summary_* 事件，reasoning_text.* 事件不会被显示。
	// 模型 catalog 的 default_reasoning_summary 常为 "none"，导致 effort 已开启时
	// 用户仍看不到思考。只要 effort 已开启（非 none），就强制使用 summary 事件格式。
	if shouldSummarizeReasoning(req) {
		conv.SetSummarized(true)
	}

	var evCount int
	var executedReq *oairesponses.ResponseNewParams
	sourceName, execErr := s.sch.ExecutePrepared(r.Context(), func(src config.Source) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
		reqForSource, anthReq, mcp, err := s.buildAnthropicRequest(body, src)
		if err != nil {
			return nil, nil, err
		}
		executedReq = reqForSource
		conv.SetCustomToolNames(convert.FreeformToolNames(reqForSource))
		conv.SetDeclaredServerTools(convert.DeclaredServerTools(reqForSource))
		sysLen := 0
		for _, b := range anthReq.System {
			sysLen += len(b.Text)
		}
		thinkingOn := anthReq.Thinking.OfEnabled != nil || anthReq.Thinking.OfAdaptive != nil
		thinkingBlocks, emptySig, toolUseBlk, toolResultBlk, assistantMsgs, userMsgs := summarizeAnthropicRequest(anthReq)
		slog.Info("请求转换完成",
			"source", src.Name,
			"model", string(anthReq.Model),
			"max_tokens", anthReq.MaxTokens,
			"messages", len(anthReq.Messages),
			"assistant_messages", assistantMsgs,
			"user_messages", userMsgs,
			"system_bytes", sysLen,
			"thinking", thinkingOn,
			"thinking_blocks", thinkingBlocks,
			"thinking_empty_signature", emptySig,
			"tool_use_blocks", toolUseBlk,
			"tool_result_blocks", toolResultBlk,
			"tools", len(anthReq.Tools))
		if emptySig > 0 {
			slog.Warn("回灌的 thinking block 存在空 signature，可能违反 Anthropic thinking round-trip 规则",
				"source", src.Name, "thinking_blocks", thinkingBlocks, "empty_signature", emptySig)
		}
		return anthReq, mcp, nil
	}, func(ev *anthropic.MessageStreamEventUnion) error {
		evCount++
		blkType := ""
		blkName := ""
		if ev.Type == anContentBlockStart {
			blkType = ev.ContentBlock.Type
			blkName = ev.ContentBlock.Name
		}
		slog.Debug("收到上游流事件", "event_index", evCount, "event_type", ev.Type, "block_index", ev.Index, "block_type", blkType, "block_name", blkName)
		out, _ := conv.Feed(ev)
		for _, e := range out {
			slog.Debug("写出响应 SSE 事件", "event_type", e.Type)
			writeSSE(w, e)
		}
		flusher.Flush()
		return nil
	})

	// Compute the response ID once so the error event and session save share
	// the same value (I2: previously two newResponseID() calls produced
	// different IDs when conv.RespID() == "").
	id := conv.RespID()
	if id == "" {
		id = newResponseID()
	}

	if execErr == nil {
		items := conv.OutputItems()
		var types []string
		for _, it := range items {
			types = append(types, it.Type)
		}
		status := model.ResponseStatusCompleted
		if conv.Failed() {
			status = model.ResponseStatusFailed
		}
		var cacheRead, cacheCreate int
		if u := conv.Usage(); u != nil {
			cacheRead = u.CacheReadInputTokens
			cacheCreate = u.CacheCreationInputTokens
		}
		slog.Info("响应请求完成", "response_id", id, "status", status, "source", sourceName, "upstream_events", evCount, "stop_reason", conv.StopReason(), "output_types", types, "cache_read_tokens", cacheRead, "cache_creation_tokens", cacheCreate, "elapsed", time.Since(reqStart).String())
		trailing, _ := conv.Feed(&anthropic.MessageStreamEventUnion{Type: anMessageStop})
		for _, e := range trailing {
			writeSSE(w, e)
		}
		flusher.Flush()
	} else if conv.Done() && !conv.Failed() {
		slog.Warn("上游流终态后读取失败",
			"response_id", id,
			"source", sourceName,
			"elapsed", time.Since(reqStart).String(),
			"error", execErr)
	} else {
		slog.Error("响应请求失败", "response_id", id, "status", "failed", "source", sourceName, "elapsed", time.Since(reqStart).String(), "error", execErr)
		if !conv.Done() {
			// I1: only emit a server-side response.failed if the converter hasn't
			// already emitted one (e.g. via a mid-stream error event). Without this
			// guard, a mid-stream error followed by a connection reset would produce
			// two response.failed events.
			errResp := model.NewResponseObject(id, model.ResponseStatusFailed, "", time.Now().Unix(), echoFromRequest(req))
			errResp.Output = []model.OutputItem{}
			errResp.Error = &model.ResponseError{Message: fmt.Sprintf("upstream: %v", execErr)}
			evType := string(oaconstant.ValueOf[oaconstant.ResponseFailed]())
			writeSSE(w, model.MarshalEvent(evType, model.TerminalResponseEvent{
				Type: evType, SequenceNumber: conv.NextSeq(), Response: errResp,
			}))
			flusher.Flush()
		}
	}

	if shouldStoreResponse(req) && !conv.Failed() && (execErr == nil || conv.Done()) {
		items := conv.OutputItems()
		if !conv.Done() && len(items) == 0 {
			if executedReq != nil {
				items = collectOutput(executedReq)
			} else {
				items = collectOutput(req)
			}
		}
		reqToStore := executedReq
		if reqToStore == nil {
			reqToStore = req
		}
		if conv.Done() && len(items) == 0 {
			reqToStore = nil
		}
		s.sess.SaveResponse(id, sourceName, reqToStore, items)
		slog.Debug("保存会话上下文",
			"response_id", id,
			"source", sourceName,
			"items", len(items),
			"previous_response_id", req.PreviousResponseID.Value,
			"previous_response_id_present", req.PreviousResponseID.Valid() && req.PreviousResponseID.Value != "",
			"store_effective", true)
	} else {
		slog.Debug("跳过会话上下文保存",
			"response_id", id,
			"source", sourceName,
			"previous_response_id", req.PreviousResponseID.Value,
			"previous_response_id_present", req.PreviousResponseID.Valid() && req.PreviousResponseID.Value != "",
			"store", shouldStoreResponse(req))
	}
}

func (s *Server) buildAnthropicRequest(body []byte, src config.Source) (*oairesponses.ResponseNewParams, *anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
	req, err := convert.DecodeResponseNewParams(body)
	if err != nil {
		return nil, nil, nil, err
	}
	var prevItems []model.OutputItem
	if shouldStoreResponse(req) {
		prevItems = s.sess.Enrich(req, src.Name)
		slog.Debug("会话历史回填完成",
			"source", src.Name,
			"previous_response_id", req.PreviousResponseID.Value,
			"previous_response_id_present", req.PreviousResponseID.Valid() && req.PreviousResponseID.Value != "",
			"previous_items", len(prevItems),
			"input_items_after_enrich", len(req.Input.OfInputItemList),
			"input_string_len_after_enrich", len(req.Input.OfString.Value),
			"store_effective", true,
			slog.Group("input_item_type_counts_after_enrich", inputItemTypeCountAttrs(req.Input.OfInputItemList)...))
	} else if req.PreviousResponseID.Valid() && req.PreviousResponseID.Value != "" {
		slog.Warn("跳过 previous_response_id 会话回填",
			"source", src.Name,
			"previous_response_id", req.PreviousResponseID.Value,
			"store", false)
	}
	anthReq, mcp, err := convert.ToAnthropic(req, s.cfg, prevItems...)
	if err != nil {
		return nil, nil, nil, err
	}
	// 模型遵循能力补强：为指令遵循弱的后端（如 GLM）追加 system 尾部提示，
	// 例如强制先调 tool_search 发现 skill。独立 TextBlockParam，不破坏 convert
	// 在前一个 system block 上设置的 cache_control，前缀缓存仍命中。
	if src.SystemSuffix != "" {
		anthReq.System = append(anthReq.System, anthropic.TextBlockParam{Text: src.SystemSuffix})
		// DEBUG：预转换与实际调度各调用一次 buildAnthropicRequest，
		// INFO 会重复，降至 DEBUG；注入是否生效看「请求转换完成」的 system_blocks。
		slog.Debug("注入 system_suffix", "source", src.Name, "suffix_bytes", len(src.SystemSuffix), "system_blocks", len(anthReq.System))
	}
	return req, anthReq, mcp, nil
}

func writeSSE(w io.Writer, e model.SSEEvent) {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, e.Data); err != nil {
		slog.Warn("写出 SSE 事件失败", "event_type", e.Type, "error", err)
	}
}

func shouldSummarizeReasoning(req *oairesponses.ResponseNewParams) bool {
	if string(req.Reasoning.Summary) == model.ReasoningSummaryConcise {
		return true
	}
	return req.Reasoning.Effort != "" && string(req.Reasoning.Effort) != model.ReasoningEffortNone
}

func shouldStoreResponse(req *oairesponses.ResponseNewParams) bool {
	return !req.Store.Valid() || req.Store.Value
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

	// include：仅部分子项可处理，整体只记录不展开。
	if len(req.Include) > 0 {
		// 只对非 encrypted_content 的 include 项 WARN（encrypted_content 已通过
		// disable_response_storage 路径处理，不是丢弃）。
		vals := make([]string, 0, len(req.Include))
		for _, inc := range req.Include {
			if string(inc) == "reasoning.encrypted_content" {
				continue
			}
			vals = append(vals, string(inc))
		}
		if len(vals) > 0 {
			slog.Warn("忽略 include 中除 reasoning.encrypted_content 之外的所有 include 项，对应数据被丢弃",
				"field", "include",
				"values", strings.Join(vals, ","),
				"impact", "除 reasoning.encrypted_content 已通过 disable_response_storage 路径处理外，其余 include 不生效")
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
	// prompt_cache_*：网关自主管理 cache_control。
	if req.PromptCacheKey.Valid() && req.PromptCacheKey.Value != "" {
		slog.Warn("忽略 prompt_cache_key（网关自主管理 cache_control），对应数据被丢弃",
			"field", "prompt_cache_key",
			"impact", "Anthropic 使用内容哈希缓存，不认客户端 key")
	}
	if req.PromptCacheOptions.Mode != "" || req.PromptCacheOptions.Ttl != "" {
		slog.Warn("忽略 prompt_cache_options（网关自主管理 cache_control），对应数据被丢弃",
			"field", "prompt_cache_options",
			"mode", req.PromptCacheOptions.Mode,
			"ttl", req.PromptCacheOptions.Ttl,
			"impact", "OpenAI options 结构对 Anthropic 无意义")
	}
	// deprecated prompt_cache_retention。
	if req.PromptCacheRetention != "" {
		slog.Warn("忽略 deprecated 字段 prompt_cache_retention（与 Anthropic cache_control 语义不同），对应数据被丢弃",
			"field", "prompt_cache_retention",
			"value", string(req.PromptCacheRetention),
			"impact", "retention 策略不生效")
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
	// conversation：本地 store 不是 OpenAI Conversation API。
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
		slog.Warn("忽略 deprecated 字段 user（OpenAI 已废弃，建议改用 safety_identifier / prompt_cache_key），对应数据被丢弃",
			"field", "user",
			"impact", "不会传递给上游（可改用 metadata.user_id）")
	}
}

// echoFromRequest extracts P2 echo fields from the request for response object.
func echoFromRequest(req *oairesponses.ResponseNewParams) model.ResponseObjectParams {
	p := model.ResponseObjectParams{
		Instructions: req.Instructions.Value,
		Truncation:   string(req.Truncation),
	}
	if req.Temperature.Valid() {
		v := req.Temperature.Value
		p.Temperature = &v
	}
	if req.TopP.Valid() {
		v := req.TopP.Value
		p.TopP = &v
	}
	if req.MaxOutputTokens.Valid() {
		v := req.MaxOutputTokens.Value
		p.MaxOutputTokens = &v
	}
	if req.PreviousResponseID.Valid() {
		p.PreviousResponseID = req.PreviousResponseID.Value
	}
	if req.ParallelToolCalls.Valid() {
		v := req.ParallelToolCalls.Value
		p.ParallelToolCalls = &v
	}
	if req.Store.Valid() {
		v := req.Store.Value
		p.Store = &v
	}
	// Echo tool_choice if any variant is set.
	if req.ToolChoice.OfToolChoiceMode.Valid() ||
		req.ToolChoice.OfAllowedTools != nil ||
		req.ToolChoice.OfFunctionTool != nil ||
		req.ToolChoice.OfHostedTool != nil ||
		req.ToolChoice.OfMcpTool != nil ||
		req.ToolChoice.OfCustomTool != nil ||
		req.ToolChoice.OfSpecificApplyPatchToolChoice != nil ||
		req.ToolChoice.OfSpecificShellToolChoice != nil {
		p.ToolChoice = req.ToolChoice
	}
	if req.Reasoning.Effort != "" || req.Reasoning.Summary != "" {
		p.Reasoning = &model.ReasoningEcho{
			Effort:  string(req.Reasoning.Effort),
			Summary: string(req.Reasoning.Summary),
		}
	}
	return p
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
	if it.OfAdditionalTools != nil {
		return model.ItemTypeAdditionalTools
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
		model.ItemTypeAdditionalTools,
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
func summarizeAnthropicRequest(req *anthropic.MessageNewParams) (thinkingBlocks, emptySig, toolUse, toolResult, assistant, user int) {
	for _, msg := range req.Messages {
		switch msg.Role {
		case anthropic.MessageParamRoleAssistant:
			assistant++
		case anthropic.MessageParamRoleUser:
			user++
		}
		for _, b := range msg.Content {
			switch {
			case b.OfThinking != nil:
				thinkingBlocks++
				if b.OfThinking.Signature == "" {
					emptySig++
				}
			case b.OfRedactedThinking != nil:
				thinkingBlocks++
			case b.OfToolUse != nil:
				toolUse++
			case b.OfToolResult != nil:
				toolResult++
			}
		}
	}
	return
}

// collectOutput collects function_call/reasoning items from the request's input
// for session storage fallback.
func collectOutput(req *oairesponses.ResponseNewParams) []model.OutputItem {
	var out []model.OutputItem
	for _, it := range req.Input.OfInputItemList {
		if it.OfFunctionCall != nil {
			fc := it.OfFunctionCall
			out = append(out, model.OutputItem{
				Type: model.ItemTypeFunctionCall, CallID: fc.CallID, Name: fc.Name,
				Arguments: fc.Arguments,
			})
		} else if it.OfCustomToolCall != nil {
			call := it.OfCustomToolCall
			out = append(out, model.OutputItem{
				Type:      model.ItemTypeCustomToolCall,
				ID:        call.ID.Value,
				CallID:    call.CallID,
				Name:      call.Name,
				Input:     call.Input,
				Namespace: call.Namespace.Value,
			})
		} else if it.OfCustomToolCallOutput != nil {
			output := it.OfCustomToolCallOutput
			out = append(out, model.OutputItem{
				Type:   model.ItemTypeCustomToolCallOut,
				ID:     output.ID.Value,
				CallID: output.CallID,
				Output: output.Output.OfString.Value,
			})
		} else if it.OfReasoning != nil {
			r := it.OfReasoning
			item := model.OutputItem{Type: model.ItemTypeReasoning, ID: r.ID}
			for _, s := range r.Summary {
				item.Summary = append(item.Summary, model.OutputText{
					Type: model.ContentTypeSummaryText, Text: s.Text,
				})
			}
			if r.EncryptedContent.Valid() {
				item.EncryptedContent = r.EncryptedContent.Value
			}
			out = append(out, item)
		}
	}
	return out
}

func newResponseID() string {
	return fmt.Sprintf("resp_%d", time.Now().UnixNano())
}
