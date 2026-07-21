// Package chatstreamconv 将 OpenAI Chat Completions SSE chunk 转为 Responses SSE 事件。
package chatstreamconv

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

var (
	evResponseCreated            = string(oaconstant.ValueOf[oaconstant.ResponseCreated]())
	evResponseInProgress         = string(oaconstant.ValueOf[oaconstant.ResponseInProgress]())
	evResponseCompleted          = string(oaconstant.ValueOf[oaconstant.ResponseCompleted]())
	evResponseFailed             = string(oaconstant.ValueOf[oaconstant.ResponseFailed]())
	evOutputItemAdded            = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemAdded]())
	evOutputItemDone             = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemDone]())
	evContentPartAdded           = string(oaconstant.ValueOf[oaconstant.ResponseContentPartAdded]())
	evContentPartDone            = string(oaconstant.ValueOf[oaconstant.ResponseContentPartDone]())
	evOutputTextDelta            = string(oaconstant.ValueOf[oaconstant.ResponseOutputTextDelta]())
	evOutputTextDone             = string(oaconstant.ValueOf[oaconstant.ResponseOutputTextDone]())
	evFunctionCallArgumentsDelta = string(oaconstant.ValueOf[oaconstant.ResponseFunctionCallArgumentsDelta]())
	evFunctionCallArgumentsDone  = string(oaconstant.ValueOf[oaconstant.ResponseFunctionCallArgumentsDone]())
)

// Converter 将 Chat chunk 流转为 Responses SSE。
type Converter struct {
	respID      string
	model       string
	clientModel string
	seq         int64
	createdAt   int64
	started     bool
	completed   bool
	failed      bool
	stopReason  string
	usage       *model.ResponseUsage
	outputItems []model.OutputItem
	echo        model.ResponseObjectParams

	// 文本 message item
	msgItemID   string
	msgIndex    int
	msgOpen     bool
	contentBuf  strings.Builder
	contentOpen bool

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
}

// chatChunk 是 Chat Completions 流式 chunk 的精简视图。
type chatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
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
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// New 返回空转换器。
func New() *Converter {
	return &Converter{tools: map[int]*toolAccum{}}
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
		c.usage = &model.ResponseUsage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}
	}
	// 空 choices 的 usage 末包：只更新 usage，不 panic
	if len(chunk.Choices) == 0 {
		return out, nil
	}
	ch := chunk.Choices[0]
	if ch.Delta.Content != "" {
		out = append(out, c.feedText(ch.Delta.Content)...)
	}
	for _, tc := range ch.Delta.ToolCalls {
		out = append(out, c.feedToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)...)
	}
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		c.stopReason = *ch.FinishReason
		out = append(out, c.closeOpenItems()...)
		out = append(out, c.complete()...)
		c.completed = true
	}
	return out, nil
}

// FeedDone 在收到 data: [DONE] 时调用；若尚未 completed 则补终态。
func (c *Converter) FeedDone() []model.SSEEvent {
	if c.completed {
		return nil
	}
	if !c.started {
		// 空流：至少产出 created + completed
		var out []model.SSEEvent
		out = append(out, c.openResponse(chatChunk{ID: "chatcmpl-empty"})...)
		c.started = true
		out = append(out, c.complete()...)
		c.completed = true
		return out
	}
	var out []model.SSEEvent
	out = append(out, c.closeOpenItems()...)
	out = append(out, c.complete()...)
	c.completed = true
	return out
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

func (c *Converter) feedText(delta string) []model.SSEEvent {
	// 若已开 tool，先不混文本；MVP 简化：文本在 tool 前
	var out []model.SSEEvent
	out = append(out, c.ensureMessageOpen()...)
	c.contentBuf.WriteString(delta)
	out = append(out, model.MarshalEvent(evOutputTextDelta, model.OutputTextDeltaEvent{
		Type: evOutputTextDelta, SequenceNumber: c.nextSeq(),
		OutputIndex: c.msgIndex, ContentIndex: 0, ItemID: c.msgItemID, Delta: delta,
	}))
	return out
}

func (c *Converter) feedToolCall(index int, id, name, args string) []model.SSEEvent {
	var out []model.SSEEvent
	// 开 tool 前关闭文本 message
	if c.msgOpen && !toolAllClosed(c.tools) {
		out = append(out, c.closeMessage()...)
	}
	acc := c.tools[index]
	if acc == nil {
		acc = &toolAccum{outIdx: len(c.outputItems)}
		c.tools[index] = acc
	}
	if id != "" {
		acc.id = id
	}
	if name != "" {
		acc.name = name
	}
	if args != "" {
		acc.args.WriteString(args)
	}
	if !acc.opened && (acc.id != "" || acc.name != "") {
		if acc.id == "" {
			acc.id = fmt.Sprintf("call_%d", index)
		}
		acc.itemID = acc.id
		acc.opened = true
		item := model.OutputItem{
			Type:      "function_call",
			ID:        acc.itemID,
			CallID:    acc.id,
			Name:      acc.name,
			Arguments: "",
			Status:    "in_progress",
		}
		c.outputItems = append(c.outputItems, item)
		out = append(out, model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
			Type: evOutputItemAdded, SequenceNumber: c.nextSeq(), OutputIndex: acc.outIdx, Item: item,
		}))
	}
	if args != "" && acc.opened {
		out = append(out, model.MarshalEvent(evFunctionCallArgumentsDelta, map[string]any{
			"type":            evFunctionCallArgumentsDelta,
			"sequence_number": c.nextSeq(),
			"output_index":    acc.outIdx,
			"item_id":         acc.itemID,
			"delta":           args,
		}))
	}
	return out
}

func toolAllClosed(m map[int]*toolAccum) bool {
	if len(m) == 0 {
		return true
	}
	for _, t := range m {
		if t.opened && !t.closed {
			return false
		}
	}
	return true
}

func (c *Converter) closeMessage() []model.SSEEvent {
	if !c.msgOpen {
		return nil
	}
	text := c.contentBuf.String()
	var out []model.SSEEvent
	if c.contentOpen {
		out = append(out, model.MarshalEvent(evOutputTextDone, model.OutputTextDoneEvent{
			Type: evOutputTextDone, SequenceNumber: c.nextSeq(),
			OutputIndex: c.msgIndex, ContentIndex: 0, ItemID: c.msgItemID, Text: text,
		}))
		out = append(out, model.MarshalEvent(evContentPartDone, map[string]any{
			"type":            evContentPartDone,
			"sequence_number": c.nextSeq(),
			"output_index":    c.msgIndex,
			"content_index":   0,
			"item_id":         c.msgItemID,
			"part":            model.ContentPartOut{Type: "output_text", Text: text},
		}))
		c.contentOpen = false
	}
	item := model.OutputItem{
		Type:   "message",
		ID:     c.msgItemID,
		Status: "completed",
		Role:   "assistant",
		Content: []model.OutputText{{
			Type: "output_text", Text: text,
		}},
	}
	if c.msgIndex < len(c.outputItems) {
		c.outputItems[c.msgIndex] = item
	}
	out = append(out, model.MarshalEvent(evOutputItemDone, map[string]any{
		"type":            evOutputItemDone,
		"sequence_number": c.nextSeq(),
		"output_index":    c.msgIndex,
		"item":            item,
	}))
	c.msgOpen = false
	return out
}

func (c *Converter) closeOpenItems() []model.SSEEvent {
	var out []model.SSEEvent
	if c.msgOpen {
		out = append(out, c.closeMessage()...)
	}
	// 关闭 tool calls（按 index 排序可选，map 遍历即可）
	for _, acc := range c.tools {
		if !acc.opened || acc.closed {
			continue
		}
		args := acc.args.String()
		out = append(out, model.MarshalEvent(evFunctionCallArgumentsDone, map[string]any{
			"type":            evFunctionCallArgumentsDone,
			"sequence_number": c.nextSeq(),
			"output_index":    acc.outIdx,
			"item_id":         acc.itemID,
			"arguments":       args,
		}))
		item := model.OutputItem{
			Type:      "function_call",
			ID:        acc.itemID,
			CallID:    acc.id,
			Name:      acc.name,
			Arguments: args,
			Status:    "completed",
		}
		if acc.outIdx < len(c.outputItems) {
			c.outputItems[acc.outIdx] = item
		}
		out = append(out, model.MarshalEvent(evOutputItemDone, map[string]any{
			"type":            evOutputItemDone,
			"sequence_number": c.nextSeq(),
			"output_index":    acc.outIdx,
			"item":            item,
		}))
		acc.closed = true
	}
	return out
}

func (c *Converter) complete() []model.SSEEvent {
	status := model.ResponseStatusCompleted
	if c.stopReason == "content_filter" {
		// 与 Anthropic 路径尽量对齐：content_filter 仍 completed + incomplete_details 可后补；MVP 用 completed
		slog.Debug("chatstreamconv: content_filter finish_reason")
	}
	resp := model.NewResponseObject(c.respID, status, c.model, c.createdAt, c.echo)
	resp.Output = c.outputItems
	resp.CompletedAt = time.Now().Unix()
	resp.Usage = c.usage
	return []model.SSEEvent{
		model.MarshalEvent(evResponseCompleted, model.TerminalResponseEvent{
			Type: evResponseCompleted, SequenceNumber: c.nextSeq(), Response: resp,
		}),
	}
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
