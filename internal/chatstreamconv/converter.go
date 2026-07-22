// Package chatstreamconv 将 OpenAI Chat Completions SSE chunk 转为 Responses SSE 事件。
package chatstreamconv

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

var (
	evResponseCreated                 = string(oaconstant.ValueOf[oaconstant.ResponseCreated]())
	evResponseInProgress              = string(oaconstant.ValueOf[oaconstant.ResponseInProgress]())
	evResponseCompleted               = string(oaconstant.ValueOf[oaconstant.ResponseCompleted]())
	evResponseIncomplete              = string(oaconstant.ValueOf[oaconstant.ResponseIncomplete]())
	evResponseFailed                  = string(oaconstant.ValueOf[oaconstant.ResponseFailed]())
	evOutputItemAdded                 = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemAdded]())
	evOutputItemDone                  = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemDone]())
	evContentPartAdded                = string(oaconstant.ValueOf[oaconstant.ResponseContentPartAdded]())
	evContentPartDone                 = string(oaconstant.ValueOf[oaconstant.ResponseContentPartDone]())
	evOutputTextDelta                 = string(oaconstant.ValueOf[oaconstant.ResponseOutputTextDelta]())
	evOutputTextDone                  = string(oaconstant.ValueOf[oaconstant.ResponseOutputTextDone]())
	evFunctionCallArgumentsDelta      = string(oaconstant.ValueOf[oaconstant.ResponseFunctionCallArgumentsDelta]())
	evFunctionCallArgumentsDone       = string(oaconstant.ValueOf[oaconstant.ResponseFunctionCallArgumentsDone]())
	evCustomToolCallInputDelta        = string(oaconstant.ValueOf[oaconstant.ResponseCustomToolCallInputDelta]())
	evCustomToolCallInputDone         = string(oaconstant.ValueOf[oaconstant.ResponseCustomToolCallInputDone]())
	evRefusalDelta                    = string(oaconstant.ValueOf[oaconstant.ResponseRefusalDelta]())
	evRefusalDone                     = string(oaconstant.ValueOf[oaconstant.ResponseRefusalDone]())
	evWebSearchCallInProgress         = string(oaconstant.ValueOf[oaconstant.ResponseWebSearchCallInProgress]())
	evWebSearchCallSearching          = string(oaconstant.ValueOf[oaconstant.ResponseWebSearchCallSearching]())
	evWebSearchCallCompleted          = string(oaconstant.ValueOf[oaconstant.ResponseWebSearchCallCompleted]())
	evCodeInterpreterCallInProgress   = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallInProgress]())
	evCodeInterpreterCallInterpreting = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallInterpreting]())
	evCodeInterpreterCallCodeDelta    = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallCodeDelta]())
	evCodeInterpreterCallCodeDone     = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallCodeDone]())
	evCodeInterpreterCallCompleted    = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallCompleted]())
	evMcpCallInProgress               = string(oaconstant.ValueOf[oaconstant.ResponseMcpCallInProgress]())
	evMcpCallArgumentsDelta           = string(oaconstant.ValueOf[oaconstant.ResponseMcpCallArgumentsDelta]())
	evMcpCallArgumentsDone            = string(oaconstant.ValueOf[oaconstant.ResponseMcpCallArgumentsDone]())
	evMcpCallCompleted                = string(oaconstant.ValueOf[oaconstant.ResponseMcpCallCompleted]())
)

const refusalFallback = "I can't help with that."

// Converter 将 Chat chunk 流转为 Responses SSE。
type Converter struct {
	respID      string
	model       string
	clientModel string
	seq         int64
	createdAt   int64
	started     bool
	streamEnded bool // 已见 finish_reason（尚未必发终态 SSE）
	completed   bool // 已发出 terminal SSE
	failed      bool
	stopReason  string
	usage       *model.ResponseUsage
	outputItems []model.OutputItem
	echo        model.ResponseObjectParams

	// freeform 工具名（shell/apply_patch/custom）；tool_search 单独分支
	freeformNames map[string]struct{}

	// 文本 message item
	msgItemID   string
	msgIndex    int
	msgOpen     bool
	contentBuf  strings.Builder
	contentOpen bool

	// Chat delta.refusal 累积；content_filter 时映射为 Responses refusal item
	refusalBuf strings.Builder
	// contentLogprobs 累积本条 assistant 文本的 Chat token logprobs（用于 done / content part）。
	contentLogprobs []model.TokenLogprob

	// tool_calls 按 index 累积
	tools map[int]*toolAccum
}

type toolAccum struct {
	id, name string
	args     strings.Builder
	itemID   string
	outIdx   int
	opened   bool
	closed   bool
	kind     toolKind // function | custom | tool_search
}

type toolKind int

const (
	kindFunction toolKind = iota
	kindCustom
	kindToolSearch
	kindWebSearch
	kindCodeInterpreter
	kindMCP
)

// chatChunk 是 Chat Completions 流式 chunk 的精简视图。
type chatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"` // string | content part array | null
			Refusal   string          `json:"refusal"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
		// Logprobs 与 delta 同包；官方 shape: content/refusal 数组。
		Logprobs *struct {
			Content []chatTokenLogprob `json:"content"`
			Refusal []chatTokenLogprob `json:"refusal"`
		} `json:"logprobs"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		// 官方 stream usage 可带 details；映射到 Responses usage 的 cache/reasoning 字段。
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// chatTokenLogprob 是 Chat Completions 流式 token logprob 子集（忽略 bytes）。
type chatTokenLogprob struct {
	Token       string  `json:"token"`
	Logprob     float64 `json:"logprob"`
	TopLogprobs []struct {
		Token   string  `json:"token"`
		Logprob float64 `json:"logprob"`
	} `json:"top_logprobs"`
}

func mapChatTokenLogprobs(in []chatTokenLogprob) []model.TokenLogprob {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.TokenLogprob, 0, len(in))
	for _, t := range in {
		item := model.TokenLogprob{Token: t.Token, Logprob: t.Logprob}
		if len(t.TopLogprobs) > 0 {
			item.TopLogprobs = make([]model.TopTokenLogprob, 0, len(t.TopLogprobs))
			for _, top := range t.TopLogprobs {
				item.TopLogprobs = append(item.TopLogprobs, model.TopTokenLogprob{
					Token: top.Token, Logprob: top.Logprob,
				})
			}
		}
		out = append(out, item)
	}
	return out
}

// deltaContentText 解析 Chat delta.content：官方多为 string，兼容上游 content part 数组。
func deltaContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		slog.Debug("chatstreamconv: 无法解析 delta.content", "raw", string(raw))
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "", "text", "output_text", "input_text":
			b.WriteString(p.Text)
		default:
			// 非文本 part（image 等）在 Chat 收口内忽略
		}
	}
	return b.String()
}

// New 返回空转换器。
func New() *Converter {
	return &Converter{
		tools:         map[int]*toolAccum{},
		freeformNames: map[string]struct{}{},
	}
}

// SetFreeformNames 登记 freeform 工具名（来自 chatconvert.ChatRequest.FreeformNames）。
// shell/apply_patch 始终按 freeform 处理；custom 名须登记，否则会误判为 function_call。
func (c *Converter) SetFreeformNames(names map[string]struct{}) {
	if names == nil {
		return
	}
	for n := range names {
		c.freeformNames[n] = struct{}{}
	}
}

func (c *Converter) nextSeq() int64 { c.seq++; return c.seq }

// RespID 返回响应 ID。
func (c *Converter) RespID() string { return c.respID }

// Done 是否已终态。
func (c *Converter) Done() bool { return c.completed }

// Failed 是否失败终态。
func (c *Converter) Failed() bool { return c.failed }

// Seq 当前序号。
func (c *Converter) Seq() int64 { return c.seq }

// StopReason 停止原因。
func (c *Converter) StopReason() string { return c.stopReason }

// Usage token 用量。
func (c *Converter) Usage() *model.ResponseUsage { return c.usage }

// OutputItems 已累积输出项。
func (c *Converter) OutputItems() []model.OutputItem { return c.outputItems }

// SetClientModel 设置客户端模型别名。
func (c *Converter) SetClientModel(m string) { c.clientModel = m }

// SetEcho 设置 request echo 字段。
func (c *Converter) SetEcho(p model.ResponseObjectParams) { c.echo = p }

// Feed 处理一个 data 行的 JSON chunk（不含 data: 前缀）。
//
// 官方 include_usage 顺序是：finish_reason 包 → 空 choices 的 usage 末包 → [DONE]。
// 因此 finish_reason 只关闭 open items 并标记 streamEnded，终态 SSE 延后到 FeedDone
// （或本 chunk 已带 usage 时立即发出），避免终态 usage 恒为 0。
func (c *Converter) Feed(data []byte) ([]model.SSEEvent, error) {
	if c.completed {
		return nil, nil
	}
	var chunk chatChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("chatstreamconv: bad chunk: %w", err)
	}
	var out []model.SSEEvent
	if !c.started {
		out = append(out, c.openResponse(chunk)...)
		c.started = true
	}
	if chunk.Usage != nil {
		u := &model.ResponseUsage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}
		// Chat usage details → 官方 Responses details + a 路径 cache_read 兼容字段。
		if d := chunk.Usage.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
			u.CacheReadInputTokens = d.CachedTokens
			u.InputTokensDetails = &model.ResponseUsageInputDetails{CachedTokens: d.CachedTokens}
		}
		if d := chunk.Usage.CompletionTokensDetails; d != nil && d.ReasoningTokens > 0 {
			u.OutputTokensDetails = &model.ResponseUsageOutputDetails{ReasoningTokens: d.ReasoningTokens}
		}
		c.usage = u
	}
	// 已见 finish_reason 后，只接受 usage 末包（空 choices）
	if c.streamEnded {
		return out, nil
	}
	if len(chunk.Choices) == 0 {
		return out, nil
	}
	ch := chunk.Choices[0]
	if ch.Delta.Refusal != "" {
		c.refusalBuf.WriteString(ch.Delta.Refusal)
	}
	var lps []model.TokenLogprob
	if ch.Logprobs != nil {
		lps = mapChatTokenLogprobs(ch.Logprobs.Content)
	}
	if content := deltaContentText(ch.Delta.Content); content != "" || len(lps) > 0 {
		// 部分上游可能拆成「先 content 后 logprobs」两包；有 logprobs 无 content 时仅累积概率。
		out = append(out, c.feedText(content, lps)...)
	}
	for _, tc := range ch.Delta.ToolCalls {
		out = append(out, c.feedToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)...)
	}
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		c.stopReason = *ch.FinishReason
		c.streamEnded = true
		if c.stopReason == "content_filter" {
			// 与 Anthropic refusal 对齐：丢弃半截文本/工具，只保留 refusal item
			out = append(out, c.prepareRefusalOutput()...)
		} else {
			out = append(out, c.closeOpenItems()...)
		}
		// 同包已带 usage 时直接终态；否则等 usage 末包或 FeedDone
		if c.usage != nil {
			out = append(out, c.emitTerminal()...)
		}
	}
	return out, nil
}

// FeedDone 在收到 data: [DONE] 时调用；若尚未 completed 则补终态。
// 正常路径：finish_reason 已关闭 items，此处带上可能刚到达的 usage 发出 terminal。
func (c *Converter) FeedDone() []model.SSEEvent {
	if c.completed {
		return nil
	}
	if !c.started {
		var out []model.SSEEvent
		out = append(out, c.openResponse(chatChunk{ID: "chatcmpl-empty"})...)
		c.started = true
		return append(out, c.emitTerminal()...)
	}
	var out []model.SSEEvent
	if !c.streamEnded {
		out = append(out, c.closeOpenItems()...)
		c.streamEnded = true
	}
	return append(out, c.emitTerminal()...)
}

func (c *Converter) openResponse(chunk chatChunk) []model.SSEEvent {
	if chunk.ID != "" {
		c.respID = chunk.ID
	} else {
		c.respID = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	if c.clientModel != "" {
		c.model = c.clientModel
	} else if chunk.Model != "" {
		c.model = chunk.Model
	} else {
		c.model = "unknown"
	}
	c.createdAt = time.Now().Unix()
	resp := model.NewResponseObject(c.respID, model.ResponseStatusInProgress, c.model, c.createdAt, c.echo)
	return []model.SSEEvent{
		model.MarshalEvent(evResponseCreated, model.TerminalResponseEvent{
			Type: evResponseCreated, SequenceNumber: c.nextSeq(), Response: resp,
		}),
		model.MarshalEvent(evResponseInProgress, model.TerminalResponseEvent{
			Type: evResponseInProgress, SequenceNumber: c.nextSeq(), Response: resp,
		}),
	}
}

func (c *Converter) classifyTool(name string) toolKind {
	switch name {
	case "tool_search":
		return kindToolSearch
	case "shell", "apply_patch":
		return kindCustom
	case "web_search":
		return kindWebSearch
	case "code_interpreter":
		return kindCodeInterpreter
	}
	if strings.HasPrefix(name, "mcp__") {
		return kindMCP
	}
	if _, ok := c.freeformNames[name]; ok {
		return kindCustom
	}
	return kindFunction
}

func (c *Converter) ensureMessageOpen() []model.SSEEvent {
	if c.msgOpen {
		return nil
	}
	c.msgOpen = true
	c.msgIndex = len(c.outputItems)
	c.msgItemID = fmt.Sprintf("msg_%s_%d", c.respID, c.msgIndex)
	item := model.OutputItem{
		Type:   "message",
		ID:     c.msgItemID,
		Status: "in_progress",
		Role:   "assistant",
	}
	c.outputItems = append(c.outputItems, item)
	var out []model.SSEEvent
	out = append(out, model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
		Type: evOutputItemAdded, SequenceNumber: c.nextSeq(), OutputIndex: c.msgIndex, Item: item,
	}))
	out = append(out, model.MarshalEvent(evContentPartAdded, map[string]any{
		"type":            evContentPartAdded,
		"sequence_number": c.nextSeq(),
		"output_index":    c.msgIndex,
		"content_index":   0,
		"item_id":         c.msgItemID,
		"part":            model.ContentPartOut{Type: "output_text", Text: ""},
	}))
	c.contentOpen = true
	return out
}

func (c *Converter) feedText(delta string, logprobs []model.TokenLogprob) []model.SSEEvent {
	var out []model.SSEEvent
	out = append(out, c.ensureMessageOpen()...)
	if delta != "" {
		c.contentBuf.WriteString(delta)
	}
	if len(logprobs) > 0 {
		c.contentLogprobs = append(c.contentLogprobs, logprobs...)
	}
	// 仅 content 空且无 logprobs 时不发事件；有 logprobs 的空 delta 仍透传（罕见）。
	if delta == "" && len(logprobs) == 0 {
		return out
	}
	ev := model.OutputTextDeltaEvent{
		Type: evOutputTextDelta, SequenceNumber: c.nextSeq(),
		OutputIndex: c.msgIndex, ContentIndex: 0, ItemID: c.msgItemID, Delta: delta,
	}
	if len(logprobs) > 0 {
		ev.Logprobs = logprobs
	}
	out = append(out, model.MarshalEvent(evOutputTextDelta, ev))
	return out
}

func (c *Converter) feedToolCall(index int, id, name, args string) []model.SSEEvent {
	var out []model.SSEEvent
	if c.msgOpen {
		out = append(out, c.closeMessage()...)
	}
	acc := c.tools[index]
	if acc == nil {
		// outIdx 在 open 时再锁定，避免仅 id 分片占位导致后续 index 空洞
		acc = &toolAccum{outIdx: -1, kind: kindFunction}
		c.tools[index] = acc
	}
	if id != "" {
		// open 后若上游补发真实 id，同步 item 上的 call_id（先 name 后 id 的兼容分片）
		if acc.opened && acc.id != "" && acc.id != id {
			acc.id = id
			acc.itemID = id
			if acc.outIdx >= 0 && acc.outIdx < len(c.outputItems) {
				it := c.outputItems[acc.outIdx]
				it.ID = id
				it.CallID = id
				c.outputItems[acc.outIdx] = it
			}
		} else {
			acc.id = id
		}
	}
	if name != "" {
		acc.name = name
		acc.kind = c.classifyTool(name)
	}
	if args != "" {
		acc.args.WriteString(args)
	}
	// 必须先有 name 再 open：兼容上游「先 id、后 name」分片，否则会按空 name 误判 function_call。
	if !acc.opened && acc.name != "" {
		if acc.id == "" {
			acc.id = fmt.Sprintf("call_%d", index)
		}
		acc.kind = c.classifyTool(acc.name)
		acc.outIdx = len(c.outputItems)
		acc.itemID = acc.id
		acc.opened = true
		item := c.buildToolItem(acc)
		c.outputItems = append(c.outputItems, item)
		out = append(out, model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
			Type: evOutputItemAdded, SequenceNumber: c.nextSeq(), OutputIndex: acc.outIdx, Item: item,
		}))
		out = append(out, c.hostedStartEvents(acc)...)
		// open 前已累积的 arguments 作为 function 首包 delta（若适用）
		if acc.kind == kindFunction {
			buffered := acc.args.String()
			if buffered != "" {
				out = append(out, model.MarshalEvent(evFunctionCallArgumentsDelta, model.FunctionCallArgumentsDeltaEvent{
					Type: evFunctionCallArgumentsDelta, SequenceNumber: c.nextSeq(),
					OutputIndex: acc.outIdx, ItemID: acc.itemID, Delta: buffered,
				}))
			}
		}
		return out
	}
	// function 流式 arguments delta；hosted/custom/tool_search 在 stop 一次性给出
	if args != "" && acc.opened && acc.kind == kindFunction {
		out = append(out, model.MarshalEvent(evFunctionCallArgumentsDelta, model.FunctionCallArgumentsDeltaEvent{
			Type: evFunctionCallArgumentsDelta, SequenceNumber: c.nextSeq(),
			OutputIndex: acc.outIdx, ItemID: acc.itemID, Delta: args,
		}))
	}
	return out
}

func (c *Converter) buildToolItem(acc *toolAccum) model.OutputItem {
	switch acc.kind {
	case kindCustom:
		return model.OutputItem{
			Type:   model.ItemTypeCustomToolCall,
			ID:     acc.itemID,
			CallID: acc.id,
			Name:   acc.name,
		}
	case kindToolSearch:
		return model.OutputItem{
			Type:      model.ItemTypeToolSearchCall,
			ID:        acc.itemID,
			CallID:    acc.id,
			Status:    model.ResponseStatusInProgress,
			Execution: "client",
		}
	case kindWebSearch:
		return model.OutputItem{
			Type:   model.ItemTypeWebSearchCall,
			ID:     acc.itemID,
			Status: model.ResponseStatusInProgress,
			Action: &model.WebSearchAction{Type: "search"},
		}
	case kindCodeInterpreter:
		return model.OutputItem{
			Type:   model.ItemTypeCodeInterpreterCall,
			ID:     acc.itemID,
			Status: model.ResponseStatusInProgress,
		}
	case kindMCP:
		server, tool, _ := parseMCPName(acc.name)
		return model.OutputItem{
			Type:        model.ItemTypeMcpCall,
			ID:          acc.itemID,
			Status:      model.ResponseStatusInProgress,
			ServerLabel: server,
			Name:        tool,
		}
	default:
		ns, name := toolcatalog.SplitToolName(acc.name)
		return model.OutputItem{
			Type:      model.ItemTypeFunctionCall,
			ID:        acc.itemID,
			Status:    model.ResponseStatusInProgress,
			CallID:    acc.id,
			Name:      name,
			Namespace: ns,
		}
	}
}

func parseMCPName(flat string) (server, tool string, ok bool) {
	const p = "mcp__"
	if !strings.HasPrefix(flat, p) {
		return "", "", false
	}
	rest := strings.TrimPrefix(flat, p)
	idx := strings.LastIndex(rest, "__")
	if idx <= 0 {
		return "", "", false
	}
	return rest[:idx], rest[idx+2:], true
}

func (c *Converter) hostedStartEvents(acc *toolAccum) []model.SSEEvent {
	switch acc.kind {
	case kindWebSearch:
		return []model.SSEEvent{
			model.MarshalEvent(evWebSearchCallInProgress, model.WebSearchCallEvent{
				Type: evWebSearchCallInProgress, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, ItemID: acc.itemID,
			}),
			model.MarshalEvent(evWebSearchCallSearching, model.WebSearchCallEvent{
				Type: evWebSearchCallSearching, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, ItemID: acc.itemID,
			}),
		}
	case kindCodeInterpreter:
		return []model.SSEEvent{
			model.MarshalEvent(evCodeInterpreterCallInProgress, model.CodeInterpreterCallEvent{
				Type: evCodeInterpreterCallInProgress, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, ItemID: acc.itemID,
			}),
		}
	case kindMCP:
		return []model.SSEEvent{
			model.MarshalEvent(evMcpCallInProgress, model.McpCallEvent{
				Type: evMcpCallInProgress, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, ItemID: acc.itemID,
			}),
		}
	default:
		return nil
	}
}

func (c *Converter) closeMessage() []model.SSEEvent {
	if !c.msgOpen {
		return nil
	}
	text := c.contentBuf.String()
	lps := c.contentLogprobs
	var out []model.SSEEvent
	if c.contentOpen {
		doneEv := model.OutputTextDoneEvent{
			Type: evOutputTextDone, SequenceNumber: c.nextSeq(),
			OutputIndex: c.msgIndex, ContentIndex: 0, ItemID: c.msgItemID, Text: text,
		}
		if len(lps) > 0 {
			doneEv.Logprobs = lps
		}
		out = append(out, model.MarshalEvent(evOutputTextDone, doneEv))
		out = append(out, model.MarshalEvent(evContentPartDone, model.ContentPartDoneEvent{
			Type: evContentPartDone, SequenceNumber: c.nextSeq(),
			OutputIndex: c.msgIndex, ContentIndex: 0, ItemID: c.msgItemID,
			Part: model.ContentPartOut{Type: "output_text", Text: text, Logprobs: lps},
		}))
		c.contentOpen = false
	}
	item := model.OutputItem{
		Type:   "message",
		ID:     c.msgItemID,
		Status: "completed",
		Role:   "assistant",
		Content: []model.OutputText{{
			Type: "output_text", Text: text, Logprobs: lps,
		}},
	}
	if c.msgIndex < len(c.outputItems) {
		c.outputItems[c.msgIndex] = item
	}
	out = append(out, model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
		Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
		OutputIndex: c.msgIndex, Item: item,
	}))
	c.msgOpen = false
	c.contentBuf.Reset()
	c.contentLogprobs = nil
	return out
}

func (c *Converter) closeOpenItems() []model.SSEEvent {
	var out []model.SSEEvent
	if c.msgOpen {
		out = append(out, c.closeMessage()...)
	}
	// 按 index 升序关闭，保证 output 顺序稳定
	maxIdx := -1
	for i := range c.tools {
		if i > maxIdx {
			maxIdx = i
		}
	}
	for i := 0; i <= maxIdx; i++ {
		acc := c.tools[i]
		if acc == nil || !acc.opened || acc.closed {
			continue
		}
		out = append(out, c.closeTool(acc)...)
	}
	return out
}

func (c *Converter) closeTool(acc *toolAccum) []model.SSEEvent {
	rawArgs := acc.args.String()
	var out []model.SSEEvent
	switch acc.kind {
	case kindCustom:
		input := toolcatalog.SanitizeClientToolInput(acc.name, true, rawArgs)
		item := model.OutputItem{
			Type:   model.ItemTypeCustomToolCall,
			ID:     acc.itemID,
			CallID: acc.id,
			Name:   acc.name,
			Input:  input,
		}
		if acc.outIdx < len(c.outputItems) {
			c.outputItems[acc.outIdx] = item
		}
		if input != "" {
			out = append(out, model.MarshalEvent(evCustomToolCallInputDelta, model.CustomToolCallInputDeltaEvent{
				Type: evCustomToolCallInputDelta, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, ItemID: acc.itemID, Delta: input,
			}))
		}
		out = append(out, model.MarshalEvent(evCustomToolCallInputDone, model.CustomToolCallInputDoneEvent{
			Type: evCustomToolCallInputDone, SequenceNumber: c.nextSeq(),
			OutputIndex: acc.outIdx, ItemID: acc.itemID, Input: input,
		}))
		out = append(out, model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: acc.outIdx, Item: item,
		}))
	case kindToolSearch:
		item := model.OutputItem{
			Type:      model.ItemTypeToolSearchCall,
			ID:        acc.itemID,
			CallID:    acc.id,
			Status:    model.ResponseStatusCompleted,
			Execution: "client",
			Arguments: rawArgs,
		}
		if acc.outIdx < len(c.outputItems) {
			c.outputItems[acc.outIdx] = item
		}
		out = append(out, model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: acc.outIdx, Item: item,
		}))
	case kindWebSearch:
		query := jsonStringField(rawArgs, "query")
		item := model.OutputItem{
			Type:   model.ItemTypeWebSearchCall,
			ID:     acc.itemID,
			Status: model.ResponseStatusCompleted,
			Action: &model.WebSearchAction{Type: "search", Query: query},
		}
		if acc.outIdx < len(c.outputItems) {
			c.outputItems[acc.outIdx] = item
		}
		// Chat 无 server result：completed 且 sources 空（lossy）
		out = append(out,
			model.MarshalEvent(evWebSearchCallCompleted, model.WebSearchCallEvent{
				Type: evWebSearchCallCompleted, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, ItemID: acc.itemID,
			}),
			model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
				Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, Item: item,
			}),
		)
	case kindCodeInterpreter:
		code := jsonStringField(rawArgs, "code")
		item := model.OutputItem{
			Type:    model.ItemTypeCodeInterpreterCall,
			ID:      acc.itemID,
			Status:  model.ResponseStatusCompleted,
			Code:    code,
			Outputs: []model.CodeInterpreterOutput{},
		}
		if acc.outIdx < len(c.outputItems) {
			c.outputItems[acc.outIdx] = item
		}
		if code != "" {
			out = append(out,
				model.MarshalEvent(evCodeInterpreterCallCodeDelta, model.CodeInterpreterCallCodeDeltaEvent{
					Type: evCodeInterpreterCallCodeDelta, SequenceNumber: c.nextSeq(),
					OutputIndex: acc.outIdx, ItemID: acc.itemID, Delta: code,
				}),
				model.MarshalEvent(evCodeInterpreterCallCodeDone, model.CodeInterpreterCallCodeDoneEvent{
					Type: evCodeInterpreterCallCodeDone, SequenceNumber: c.nextSeq(),
					OutputIndex: acc.outIdx, ItemID: acc.itemID, Code: code,
				}),
			)
		}
		out = append(out,
			model.MarshalEvent(evCodeInterpreterCallInterpreting, model.CodeInterpreterCallEvent{
				Type: evCodeInterpreterCallInterpreting, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, ItemID: acc.itemID,
			}),
			model.MarshalEvent(evCodeInterpreterCallCompleted, model.CodeInterpreterCallEvent{
				Type: evCodeInterpreterCallCompleted, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, ItemID: acc.itemID,
			}),
			model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
				Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, Item: item,
			}),
		)
	case kindMCP:
		server, tool, _ := parseMCPName(acc.name)
		args := toolcatalog.SanitizeClientToolInput(acc.name, false, rawArgs)
		item := model.OutputItem{
			Type:        model.ItemTypeMcpCall,
			ID:          acc.itemID,
			Status:      model.ResponseStatusCompleted,
			ServerLabel: server,
			Name:        tool,
			Arguments:   args,
			Output:      "", // Chat 无 server MCP result
		}
		if acc.outIdx < len(c.outputItems) {
			c.outputItems[acc.outIdx] = item
		}
		if args != "" {
			out = append(out,
				model.MarshalEvent(evMcpCallArgumentsDelta, model.McpCallArgumentsDeltaEvent{
					Type: evMcpCallArgumentsDelta, SequenceNumber: c.nextSeq(),
					OutputIndex: acc.outIdx, ItemID: acc.itemID, Delta: args,
				}),
				model.MarshalEvent(evMcpCallArgumentsDone, model.McpCallArgumentsDoneEvent{
					Type: evMcpCallArgumentsDone, SequenceNumber: c.nextSeq(),
					OutputIndex: acc.outIdx, ItemID: acc.itemID, Arguments: args,
				}),
			)
		}
		out = append(out,
			model.MarshalEvent(evMcpCallCompleted, model.McpCallEvent{
				Type: evMcpCallCompleted, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, ItemID: acc.itemID,
			}),
			model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
				Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
				OutputIndex: acc.outIdx, Item: item,
			}),
		)
	default:
		args := toolcatalog.SanitizeClientToolInput(acc.name, false, rawArgs)
		ns, name := toolcatalog.SplitToolName(acc.name)
		item := model.OutputItem{
			Type:      model.ItemTypeFunctionCall,
			ID:        acc.itemID,
			CallID:    acc.id,
			Name:      name,
			Namespace: ns,
			Arguments: args,
			Status:    model.ResponseStatusCompleted,
		}
		if acc.outIdx < len(c.outputItems) {
			c.outputItems[acc.outIdx] = item
		}
		out = append(out, model.MarshalEvent(evFunctionCallArgumentsDone, model.FunctionCallArgumentsDoneEvent{
			Type: evFunctionCallArgumentsDone, SequenceNumber: c.nextSeq(),
			OutputIndex: acc.outIdx, ItemID: acc.itemID, Arguments: args,
		}))
		out = append(out, model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: acc.outIdx, Item: item,
		}))
	}
	acc.closed = true
	return out
}

// emitTerminal 发出 response.completed / incomplete，并标记 completed。
func (c *Converter) emitTerminal() []model.SSEEvent {
	if c.completed {
		return nil
	}
	var out []model.SSEEvent
	// content_filter 且尚未 prepare（例如仅 FeedDone、无 finish 包）时补 refusal
	if c.stopReason == "content_filter" && !hasRefusalOutput(c.outputItems) {
		out = append(out, c.prepareRefusalOutput()...)
	}
	c.completed = true
	status, incompleteReason := statusForFinish(c.stopReason)
	if c.stopReason == "content_filter" {
		slog.Info("chatstreamconv: content_filter → incomplete + refusal",
			"response_id", c.respID)
	}
	resp := model.NewResponseObject(c.respID, status, c.model, c.createdAt, c.echo)
	resp.Output = c.outputItems
	if len(resp.Output) == 0 {
		resp.Output = []model.OutputItem{}
	}
	if status == model.ResponseStatusCompleted {
		resp.CompletedAt = time.Now().Unix()
	} else {
		resp.CompletedAt = time.Now().Unix()
	}
	resp.Usage = c.usage
	if incompleteReason != "" {
		resp.IncompleteDetails = &model.IncompleteDetails{Reason: incompleteReason}
	}
	eventType := evResponseCompleted
	if status == model.ResponseStatusIncomplete {
		eventType = evResponseIncomplete
	}
	out = append(out, model.MarshalEvent(eventType, model.TerminalResponseEvent{
		Type: eventType, SequenceNumber: c.nextSeq(), Response: resp,
	}))
	return out
}

func hasRefusalOutput(items []model.OutputItem) bool {
	for _, it := range items {
		for _, p := range it.Content {
			if p.Type == model.ContentTypeRefusal {
				return true
			}
		}
	}
	return false
}

// prepareRefusalOutput 清空半截输出并产出 refusal 事件链（对齐 streamconv.emitRefusalEvents）。
func (c *Converter) prepareRefusalOutput() []model.SSEEvent {
	// 丢弃未完成文本 / tool
	c.msgOpen = false
	c.contentOpen = false
	c.contentBuf.Reset()
	c.contentLogprobs = nil
	c.tools = map[int]*toolAccum{}
	c.outputItems = nil

	text := strings.TrimSpace(c.refusalBuf.String())
	if text == "" {
		text = refusalFallback
	}
	idx := 0
	itemID := fmt.Sprintf("msg_%s_refusal", c.respID)
	refusal := text
	refusalPart := model.OutputText{Type: model.ContentTypeRefusal, Refusal: &refusal}
	empty := ""
	addedPart := model.ContentPartOut{Type: model.ContentTypeRefusal, Refusal: &empty}
	donePart := model.ContentPartOut{Type: model.ContentTypeRefusal, Refusal: &refusal}
	addedItem := model.OutputItem{
		Type: model.ItemTypeMessage, ID: itemID, Role: "assistant",
		Status: model.ResponseStatusInProgress, Content: []model.OutputText{},
	}
	doneItem := model.OutputItem{
		Type: model.ItemTypeMessage, ID: itemID, Role: "assistant",
		Status: model.ResponseStatusCompleted, Content: []model.OutputText{refusalPart},
	}
	c.outputItems = []model.OutputItem{doneItem}
	return []model.SSEEvent{
		model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
			Type: evOutputItemAdded, SequenceNumber: c.nextSeq(), OutputIndex: idx, Item: addedItem,
		}),
		model.MarshalEvent(evContentPartAdded, model.ContentPartAddedEvent{
			Type: evContentPartAdded, SequenceNumber: c.nextSeq(), OutputIndex: idx, ContentIndex: 0,
			ItemID: itemID, Part: addedPart,
		}),
		model.MarshalEvent(evRefusalDelta, model.RefusalDeltaEvent{
			Type: evRefusalDelta, SequenceNumber: c.nextSeq(), OutputIndex: idx, ContentIndex: 0,
			ItemID: itemID, Delta: text,
		}),
		model.MarshalEvent(evRefusalDone, model.RefusalDoneEvent{
			Type: evRefusalDone, SequenceNumber: c.nextSeq(), OutputIndex: idx, ContentIndex: 0,
			ItemID: itemID, Refusal: text,
		}),
		model.MarshalEvent(evContentPartDone, model.ContentPartDoneEvent{
			Type: evContentPartDone, SequenceNumber: c.nextSeq(), OutputIndex: idx, ContentIndex: 0,
			ItemID: itemID, Part: donePart,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(), OutputIndex: idx, Item: doneItem,
		}),
	}
}

func statusForFinish(reason string) (status, incompleteReason string) {
	switch reason {
	case "length":
		return model.ResponseStatusIncomplete, model.IncompleteReasonMaxOutputTokens
	case "content_filter":
		return model.ResponseStatusIncomplete, model.IncompleteReasonContentFilter
	default:
		// stop / tool_calls / 空
		return model.ResponseStatusCompleted, ""
	}
}

func jsonStringField(raw, key string) string {
	if raw == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// Fail 标记失败并产出 response.failed（流中断时由 Backend 调用）。
func (c *Converter) Fail(msg string) []model.SSEEvent {
	if c.completed {
		return nil
	}
	if !c.started {
		c.openResponse(chatChunk{ID: "resp_failed"})
		c.started = true
	}
	c.failed = true
	c.completed = true
	resp := model.NewResponseObject(c.respID, model.ResponseStatusFailed, c.model, c.createdAt, c.echo)
	resp.Error = &model.ResponseError{Message: msg}
	return []model.SSEEvent{
		model.MarshalEvent(evResponseFailed, model.TerminalResponseEvent{
			Type: evResponseFailed, SequenceNumber: c.nextSeq(), Response: resp,
		}),
	}
}
