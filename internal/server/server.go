// Package server wires config, session store, scheduler, and HTTP handlers
// into a single /v1/responses endpoint that translates OpenAI Responses API
// requests to Anthropic Messages streams and back.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	return &Server{
		cfg:       cfg,
		sess:      store.New(cfg.Session.MaxEntries, time.Duration(cfg.Session.TTL)),
		sch:       scheduler.New(cfg),
		startedAt: time.Now().Unix(),
	}
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 收集所有模型 ID（上游 + 本地 model_map 别名），去重。
	seen := make(map[string]bool)
	var entries []model.Entry

	// 1) 上游模型列表
	if body, err := s.sch.ListModels(r.Context()); err != nil {
		log.Printf("[server] /v1/models upstream failed: %v", err)
	} else {
		defer body.Close()
		var am model.AnthropicModelsResponse
		if err := json.NewDecoder(body).Decode(&am); err != nil {
			log.Printf("[server] /v1/models parse upstream: %v", err)
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
		log.Printf("[server] /v1/models encode response: %v", err)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
		return
	}
	req, err := convert.DecodeResponseNewParams(body)
	if err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	prevID := ""
	if req.PreviousResponseID.Valid() {
		prevID = req.PreviousResponseID.Value
	}
	log.Printf("[server] POST /v1/responses model=%s inputItems=%d inputStrLen=%d instructionsLen=%d reasoning.effort=%s reasoning.summary=%s prev=%q",
		req.Model, len(req.Input.OfInputItemList), len(req.Input.OfString.Value), len(req.Instructions.Value),
		req.Reasoning.Effort, req.Reasoning.Summary, prevID)
	// 逐条打印 input item 类型，用于诊断 Codex 发来的对话历史结构
	for i := range req.Input.OfInputItemList {
		it := &req.Input.OfInputItemList[i]
		role := ""
		if it.OfMessage != nil {
			role = string(it.OfMessage.Role)
		}
		log.Printf("[server]   input[%d] type=%s role=%s", i, itemType(it), role)
	}

	ordered := s.cfg.OrderedSources()
	if len(ordered) > 0 {
		if _, _, err := s.buildAnthropicRequest(body, ordered[0]); err != nil {
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
		sysLen := 0
		for _, b := range anthReq.System {
			sysLen += len(b.Text)
		}
		thinkingOn := anthReq.Thinking.OfEnabled != nil || anthReq.Thinking.OfAdaptive != nil
		log.Printf("[server] converted source=%s model=%s max_tokens=%d messages=%d systemLen=%dB thinking=%v tools=%d",
			src.Name, anthReq.Model, anthReq.MaxTokens, len(anthReq.Messages), sysLen, thinkingOn, len(anthReq.Tools))
		return anthReq, nil
	}, func(ev *anthropic.MessageStreamEventUnion) error {
		evCount++
		blkType := ""
		if ev.Type == anContentBlockStart {
			blkType = ev.ContentBlock.Type
		}
		log.Printf("[server] upstream event #%d type=%s blk=%s", evCount, ev.Type, blkType)
		out, _ := conv.Feed(ev)
		for _, e := range out {
			log.Printf("[server]   -> sse %s", e.Type)
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
		log.Printf("[server] done resp_id=%s status=completed upstreamEvents=%d outputTypes=%v", id, evCount, types)
		trailing, _ := conv.Feed(&anthropic.MessageStreamEventUnion{Type: anMessageStop})
		for _, e := range trailing {
			writeSSE(w, e)
		}
		flusher.Flush()
	} else {
		log.Printf("[server] done resp_id=%s status=failed err=%v", id, execErr)
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

	items := conv.OutputItems()
	if len(items) == 0 {
		if executedReq != nil {
			items = collectOutput(executedReq)
		} else {
			items = collectOutput(req)
		}
	}
	s.sess.Save(id, sourceName, items)
}

func (s *Server) buildAnthropicRequest(body []byte, src config.Source) (*oairesponses.ResponseNewParams, *anthropic.MessageNewParams, error) {
	req, err := convert.DecodeResponseNewParams(body)
	if err != nil {
		return nil, nil, err
	}
	prevItems := s.sess.Enrich(req, src.Name)
	log.Printf("[server] enrich source=%s prevItems=%d", src.Name, len(prevItems))
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
	// Echo tool_choice if any variant is set.
	if req.ToolChoice.OfToolChoiceMode.Valid() ||
		req.ToolChoice.OfAllowedTools != nil ||
		req.ToolChoice.OfFunctionTool != nil ||
		req.ToolChoice.OfHostedTool != nil ||
		req.ToolChoice.OfMcpTool != nil ||
		req.ToolChoice.OfCustomTool != nil {
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
	return "unknown"
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
