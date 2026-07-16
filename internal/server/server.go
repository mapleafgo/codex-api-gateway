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
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	aconstant "github.com/anthropics/anthropic-sdk-go/shared/constant"
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
	var entries []model.Entry

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
				entries = append(entries, model.Entry{
					ID: m.ID, Object: model.ObjectModel,
					Created: parseCreated(m.CreatedAt, s.startedAt),
					OwnedBy: "anthropic",
				})
			}
		}
	}

	// 2) 本地 model_map 别名
	for _, name := range s.cfg.Models() {
		if seen[name] {
			continue
		}
		seen[name] = true
		entries = append(entries, model.Entry{
			ID: name, Object: model.ObjectModel, Created: s.startedAt, OwnedBy: "codex-api-gateway",
		})
	}

	resp := model.ListResponse{Object: model.ObjectList, Data: entries}
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("写出模型列表响应失败", "error", err)
	}
}

// parseCreated 将 Anthropic RFC3339 时间戳转为 Unix 秒；解析失败时回退到 fallback。
func parseCreated(rfc3339 string, fallback int64) int64 {
	if rfc3339 == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return fallback
	}
	return t.Unix()
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		slog.Warn("拒绝响应请求", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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
		slog.Group("input_item_type_counts", inputItemTypeCountAttrs(req.Input.OfInputItemList)...))
	// 逐条打印 input item 类型，用于诊断 Codex 发来的对话历史结构
	for i := range req.Input.OfInputItemList {
		it := &req.Input.OfInputItemList[i]
		role := ""
		if it.OfMessage != nil {
			role = string(it.OfMessage.Role)
		}
		slog.Debug("响应请求输入项", "index", i, "type", itemType(it), "role", role)
	}

	ordered := s.cfg.OrderedSources()
	if len(ordered) > 0 {
		if _, _, err := s.buildAnthropicRequest(body, ordered[0]); err != nil {
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
	// Codex TUI 只渲染 reasoning_summary_* 事件，reasoning_text.* 事件不会被显示。
	// 模型 catalog 的 default_reasoning_summary 常为 "none"，导致 effort 已开启时
	// 用户仍看不到思考。只要 effort 已开启（非 none），就强制使用 summary 事件格式。
	if shouldSummarizeReasoning(req) {
		conv.SetSummarized(true)
	}

	var evCount int
	var executedReq *oairesponses.ResponseNewParams
	sourceName, execErr := s.sch.ExecutePrepared(r.Context(), func(src config.Source) (*anthropic.MessageNewParams, error) {
		reqForSource, anthReq, err := s.buildAnthropicRequest(body, src)
		if err != nil {
			return nil, err
		}
		executedReq = reqForSource
		conv.SetCustomToolNames(convert.FreeformToolNames(reqForSource))
		sysLen := 0
		for _, b := range anthReq.System {
			sysLen += len(b.Text)
		}
		thinkingOn := anthReq.Thinking.OfEnabled != nil || anthReq.Thinking.OfAdaptive != nil
		slog.Info("请求转换完成",
			"source", src.Name,
			"model", string(anthReq.Model),
			"max_tokens", anthReq.MaxTokens,
			"messages", len(anthReq.Messages),
			"system_bytes", sysLen,
			"thinking", thinkingOn,
			"tools", len(anthReq.Tools))
		return anthReq, nil
	}, func(ev *anthropic.MessageStreamEventUnion) error {
		evCount++
		blkType := ""
		if ev.Type == anContentBlockStart {
			blkType = ev.ContentBlock.Type
		}
		slog.Debug("收到上游流事件", "event_index", evCount, "event_type", ev.Type, "block_type", blkType)
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
		slog.Info("响应请求完成", "response_id", id, "status", status, "source", sourceName, "upstream_events", evCount, "output_types", types)
		trailing, _ := conv.Feed(&anthropic.MessageStreamEventUnion{Type: anMessageStop})
		for _, e := range trailing {
			writeSSE(w, e)
		}
		flusher.Flush()
	} else if conv.Done() && !conv.Failed() {
		slog.Warn("上游流终态后读取失败",
			"response_id", id,
			"source", sourceName,
			"error", execErr)
	} else {
		slog.Error("响应请求失败", "response_id", id, "status", "failed", "source", sourceName, "error", execErr)
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
		if len(items) == 0 {
			if executedReq != nil {
				items = collectOutput(executedReq)
			} else {
				items = collectOutput(req)
			}
		}
		if executedReq != nil {
			s.sess.SaveResponse(id, sourceName, executedReq, items)
		} else {
			s.sess.SaveResponse(id, sourceName, req, items)
		}
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

func (s *Server) buildAnthropicRequest(body []byte, src config.Source) (*oairesponses.ResponseNewParams, *anthropic.MessageNewParams, error) {
	req, err := convert.DecodeResponseNewParams(body)
	if err != nil {
		return nil, nil, err
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
	anthReq, err := convert.ToAnthropic(req, s.cfg, prevItems...)
	if err != nil {
		return nil, nil, err
	}
	return req, anthReq, nil
}

func writeSSE(w io.Writer, e model.SSEEvent) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, e.Data)
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

// collectOutput collects function_call/reasoning items from the request's input
// for session storage fallback.
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
