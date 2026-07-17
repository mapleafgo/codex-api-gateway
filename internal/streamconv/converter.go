// Package streamconv converts Anthropic stream events into Responses SSE events.
package streamconv

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	aconstant "github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

// Outbound event type wire strings, derived from SDK shared/constant types
// to prevent divergence from the canonical values.
var (
	evResponseCreated            = string(oaconstant.ValueOf[oaconstant.ResponseCreated]())
	evResponseInProgress         = string(oaconstant.ValueOf[oaconstant.ResponseInProgress]())
	evResponseCompleted          = string(oaconstant.ValueOf[oaconstant.ResponseCompleted]())
	evResponseIncomplete         = string(oaconstant.ValueOf[oaconstant.ResponseIncomplete]())
	evResponseFailed             = string(oaconstant.ValueOf[oaconstant.ResponseFailed]())
	evOutputItemAdded            = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemAdded]())
	evOutputItemDone             = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemDone]())
	evContentPartAdded           = string(oaconstant.ValueOf[oaconstant.ResponseContentPartAdded]())
	evContentPartDone            = string(oaconstant.ValueOf[oaconstant.ResponseContentPartDone]())
	evOutputTextDelta            = string(oaconstant.ValueOf[oaconstant.ResponseOutputTextDelta]())
	evOutputTextDone             = string(oaconstant.ValueOf[oaconstant.ResponseOutputTextDone]())
	evRefusalDelta               = string(oaconstant.ValueOf[oaconstant.ResponseRefusalDelta]())
	evRefusalDone                = string(oaconstant.ValueOf[oaconstant.ResponseRefusalDone]())
	evReasoningTextDelta         = string(oaconstant.ValueOf[oaconstant.ResponseReasoningTextDelta]())
	evReasoningTextDone          = string(oaconstant.ValueOf[oaconstant.ResponseReasoningTextDone]())
	evReasoningSummaryPartAdded  = string(oaconstant.ValueOf[oaconstant.ResponseReasoningSummaryPartAdded]())
	evReasoningSummaryPartDone   = string(oaconstant.ValueOf[oaconstant.ResponseReasoningSummaryPartDone]())
	evReasoningSummaryTextDelta  = string(oaconstant.ValueOf[oaconstant.ResponseReasoningSummaryTextDelta]())
	evReasoningSummaryTextDone   = string(oaconstant.ValueOf[oaconstant.ResponseReasoningSummaryTextDone]())
	evFunctionCallArgumentsDelta = string(oaconstant.ValueOf[oaconstant.ResponseFunctionCallArgumentsDelta]())
	evFunctionCallArgumentsDone  = string(oaconstant.ValueOf[oaconstant.ResponseFunctionCallArgumentsDone]())
	evCustomToolCallInputDelta   = string(oaconstant.ValueOf[oaconstant.ResponseCustomToolCallInputDelta]())
	evCustomToolCallInputDone    = string(oaconstant.ValueOf[oaconstant.ResponseCustomToolCallInputDone]())
	evWebSearchCallInProgress    = string(oaconstant.ValueOf[oaconstant.ResponseWebSearchCallInProgress]())
	evWebSearchCallSearching     = string(oaconstant.ValueOf[oaconstant.ResponseWebSearchCallSearching]())
	evWebSearchCallCompleted     = string(oaconstant.ValueOf[oaconstant.ResponseWebSearchCallCompleted]())

	evCodeInterpreterCallInProgress   = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallInProgress]())
	evCodeInterpreterCallInterpreting = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallInterpreting]())
	evCodeInterpreterCallCodeDelta    = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallCodeDelta]())
	evCodeInterpreterCallCodeDone     = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallCodeDone]())
	evCodeInterpreterCallCompleted    = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallCompleted]())
)

var (
	anMessageStart      = string(aconstant.ValueOf[aconstant.MessageStart]())
	anContentBlockStart = string(aconstant.ValueOf[aconstant.ContentBlockStart]())
	anContentBlockDelta = string(aconstant.ValueOf[aconstant.ContentBlockDelta]())
	anContentBlockStop  = string(aconstant.ValueOf[aconstant.ContentBlockStop]())
	anMessageDelta      = string(aconstant.ValueOf[aconstant.MessageDelta]())
	anMessageStop       = string(aconstant.ValueOf[aconstant.MessageStop]())
	anError             = string(aconstant.ValueOf[aconstant.Error]())

	anBlockText                = string(aconstant.ValueOf[aconstant.Text]())
	anBlockThinking            = string(aconstant.ValueOf[aconstant.Thinking]())
	anBlockRedactedThinking    = string(aconstant.ValueOf[aconstant.RedactedThinking]())
	anBlockToolUse             = string(aconstant.ValueOf[aconstant.ToolUse]())
	anBlockServerToolUse       = string(aconstant.ValueOf[aconstant.ServerToolUse]())
	anBlockWebSearchToolResult = string(aconstant.ValueOf[aconstant.WebSearchToolResult]())

	// tool_result 在 Anthropic 协议中只出现在请求侧，但某些后端会在响应流中
	// 回传它，Responses API 没有对应的响应事件，静默跳过。
	anBlockToolResult = string(aconstant.ValueOf[aconstant.ToolResult]())

	// server-tool result / error block wire strings: no Responses equivalent,
	// skipped gracefully instead of failing the stream.
	anBlockWebFetchToolResult           = string(aconstant.ValueOf[aconstant.WebFetchToolResult]())
	anBlockWebFetchToolResultError      = string(aconstant.ValueOf[aconstant.WebFetchToolResultError]())
	anBlockWebSearchToolResultError     = string(aconstant.ValueOf[aconstant.WebSearchToolResultError]())
	anBlockCodeExecutionToolResult      = string(aconstant.ValueOf[aconstant.CodeExecutionToolResult]())
	anBlockCodeExecutionToolResultError = string(aconstant.ValueOf[aconstant.CodeExecutionToolResultError]())

	anDeltaText      = string(aconstant.ValueOf[aconstant.TextDelta]())
	anDeltaThinking  = string(aconstant.ValueOf[aconstant.ThinkingDelta]())
	anDeltaSignature = string(aconstant.ValueOf[aconstant.SignatureDelta]())
	anDeltaInputJSON = string(aconstant.ValueOf[aconstant.InputJSONDelta]())
)

const refusalFallback = "I can't help with that."

// Converter turns a stream of Anthropic SSE events into Response SSE events.
type Converter struct {
	respID      string
	model       string
	clientModel string
	seq         int64
	createdAt   int64

	itemOrder int // next output item index

	// Text block state
	openText       bool
	textItemIdx    int
	textContentIdx int

	// Thinking block state
	openThinking    bool
	thinkItemIdx    int
	thinkSummaryIdx int
	summarized      bool // thinking display=summarized
	thinkRedacted   bool // current thinking block is redacted

	// Tool call state
	toolCalls       map[int]toolCallState // block index -> output item state
	toolArgBuilders map[int]*strings.Builder
	customToolNames map[string]bool

	// Web search state: Anthropic tool_use id -> output item index.
	webSearchByToolUseID map[string]int

	// Code execution state: Anthropic tool_use id -> output item index.
	codeExecutionByToolUseID map[string]int

	// skippedBlocks tracks block indices for server tools that have no
	// Responses equivalent (web_fetch, code_execution, ...). Their start,
	// delta and stop events are all ignored.
	skippedBlocks map[int]bool

	// Accumulators
	textBuilder  strings.Builder
	thinkBuilder strings.Builder
	sigBuilder   strings.Builder

	stopReason  string
	refusalText string
	usage       *model.ResponseUsage
	completed   bool
	failed      bool

	outputItems []model.OutputItem
	echo        model.ResponseObjectParams
}

// New returns a fresh converter.
func New() *Converter {
	return &Converter{
		toolCalls:       map[int]toolCallState{},
		toolArgBuilders: map[int]*strings.Builder{},
		customToolNames: map[string]bool{
			"apply_patch": true,
			"shell":       true,
		},
		webSearchByToolUseID:     map[string]int{},
		codeExecutionByToolUseID: map[string]int{},
		skippedBlocks:            map[int]bool{},
	}
}

type toolCallState struct {
	itemIdx int
	custom  bool
}

func (c *Converter) nextSeq() int64 { c.seq++; return c.seq }

// RespID returns the upstream message id.
func (c *Converter) RespID() string { return c.respID }

// Done returns true if the converter has already emitted a terminal event
// (response.completed / response.failed). Callers use this to avoid emitting
// a duplicate terminal event after a mid-stream error followed by a read error.
func (c *Converter) Done() bool { return c.completed }

// Failed reports whether the converter emitted a failed terminal response.
func (c *Converter) Failed() bool { return c.failed }

// Seq returns the current sequence number for use by callers that need to
// emit terminal events outside the converter (e.g. server-side response.failed).
func (c *Converter) Seq() int64 { return c.seq }

// StopReason returns the upstream stop reason for diagnostics (empty before
// the message_delta carrying it arrives).
func (c *Converter) StopReason() string { return c.stopReason }

// Usage returns the upstream token usage (including cache hit/creation) for
// diagnostics; nil before the message_delta carrying usage arrives.
func (c *Converter) Usage() *model.ResponseUsage { return c.usage }

// NextSeq advances and returns the next sequence number for caller-emitted events.
func (c *Converter) NextSeq() int64 { return c.nextSeq() }

// OutputItems returns accumulated output items.
func (c *Converter) OutputItems() []model.OutputItem { return c.outputItems }

// SetEcho injects request echo parameters for response object P2 fields.
func (c *Converter) SetEcho(p model.ResponseObjectParams) { c.echo = p }

// SetClientModel keeps Response events on the Codex-facing model alias even
// when the upstream reports its provider-specific model id.
func (c *Converter) SetClientModel(model string) { c.clientModel = model }

// SetSummarized tells the converter to emit reasoning_summary_* events
// instead of reasoning_text.* events for thinking blocks.
func (c *Converter) SetSummarized(v bool) { c.summarized = v }

// SetCustomToolNames marks tool_use names that should be emitted as Responses
// custom_tool_call items instead of function_call items.
func (c *Converter) SetCustomToolNames(names []string) {
	if c.customToolNames == nil {
		c.customToolNames = map[string]bool{}
	}
	for _, name := range names {
		if name != "" {
			c.customToolNames[name] = true
		}
	}
}

// Feed processes one Anthropic event; returns Response SSE events to emit.
func (c *Converter) Feed(ev *anthropic.MessageStreamEventUnion) ([]model.SSEEvent, error) {
	if c.completed {
		return nil, nil
	}

	var out []model.SSEEvent
	switch ev.Type {
	case anMessageStart:
		out = append(out, c.handleMessageStart(ev)...)
	case anContentBlockStart:
		out = append(out, c.handleBlockStart(ev)...)
	case anContentBlockDelta:
		out = append(out, c.handleBlockDelta(ev)...)
	case anContentBlockStop:
		out = append(out, c.handleBlockStop(ev)...)
	case anMessageDelta:
		c.recordStopReason(ev)
	case anMessageStop:
		if !c.completed {
			out = append(out, c.handleComplete()...)
			c.completed = true
		}
	case anError:
		out = append(out, c.handleError(ev))
	}
	return out, nil
}

func (c *Converter) handleMessageStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	c.respID = ev.Message.ID
	c.model = ev.Message.Model
	if c.clientModel != "" {
		c.model = c.clientModel
	}
	c.createdAt = time.Now().Unix()

	resp := model.NewResponseObject(c.respID, model.ResponseStatusInProgress, c.model, c.createdAt, c.echo)
	created := model.MarshalEvent(evResponseCreated, model.TerminalResponseEvent{
		Type: evResponseCreated, SequenceNumber: c.nextSeq(), Response: resp,
	})
	inProgress := model.MarshalEvent(evResponseInProgress, model.TerminalResponseEvent{
		Type: evResponseInProgress, SequenceNumber: c.nextSeq(), Response: resp,
	})
	return []model.SSEEvent{created, inProgress}
}

func (c *Converter) handleBlockStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	switch ev.ContentBlock.Type {
	case anBlockText:
		return c.handleTextStart()
	case anBlockThinking:
		return c.handleThinkingStart(ev, false)
	case anBlockRedactedThinking:
		return c.handleThinkingStart(ev, true)
	case anBlockToolUse:
		return c.handleToolUseStart(ev)
	case anBlockServerToolUse:
		return c.handleServerToolUseStart(ev)
	case anBlockWebSearchToolResult:
		return c.handleWebSearchResultStart(ev)
	case anBlockToolResult:
		// 兼容后端把 web_search_tool_result 误传为 tool_result 的情况：
		// 若该块的 tool_use_id 对应一个已知的 web_search server_tool_use，
		// 则按 web search 结果处理，否则静默跳过。
		if _, ok := c.webSearchByToolUseID[ev.ContentBlock.ToolUseID]; ok {
			slog.Warn("后端将 web_search_tool_result 传为 tool_result，按 web search 结果兼容处理",
				"response_id", c.respID, "tool_use_id", ev.ContentBlock.ToolUseID)
			return c.handleWebSearchResultStart(ev)
		}
		return c.handleSkippedBlockStart(ev)
	case anBlockCodeExecutionToolResult:
		// code_execution_tool_result（非 error）若关联已知 code_execution
		// server_tool_use，则映射为 code_interpreter_call 的 outputs + completed；
		// 否则按 skip 处理（含未关联的孤立 result block）。
		if _, ok := c.codeExecutionByToolUseID[ev.ContentBlock.ToolUseID]; ok {
			return c.handleCodeExecutionResultStart(ev)
		}
		return c.handleSkippedBlockStart(ev)
	case anBlockWebFetchToolResult,
		anBlockWebFetchToolResultError,
		anBlockWebSearchToolResultError,
		anBlockCodeExecutionToolResultError:
		return c.handleSkippedBlockStart(ev)
	}
	return []model.SSEEvent{c.handleUnsupportedBlock(ev)}
}

func (c *Converter) handleUnsupportedBlock(ev *anthropic.MessageStreamEventUnion) model.SSEEvent {
	c.completed = true
	c.failed = true
	blockType := ev.ContentBlock.Type
	if blockType == "" {
		blockType = "unknown"
	}
	slog.Warn("遇到不支持的 Anthropic content block，转为 response.failed",
		"response_id", c.respID, "block_type", blockType, "name", ev.ContentBlock.Name)
	resp := model.NewResponseObject(c.respID, model.ResponseStatusFailed, c.model, c.createdAt, c.echo)
	resp.Output = []model.OutputItem{}
	resp.Error = &model.ResponseError{
		Message: fmt.Sprintf("unsupported Anthropic content block %q", blockType),
	}
	return model.MarshalEvent(evResponseFailed, model.TerminalResponseEvent{
		Type: evResponseFailed, SequenceNumber: c.nextSeq(), Response: resp,
	})
}

func (c *Converter) handleTextStart() []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	c.openText = true
	c.textItemIdx = idx
	c.textContentIdx = 0
	c.textBuilder.Reset()

	itemID := fmt.Sprintf("msg_%d", idx)
	item := model.OutputItem{
		Type: model.ItemTypeMessage, ID: itemID, Role: model.RoleAssistant, Phase: model.AssistantPhaseFinalAnswer, Status: model.ResponseStatusInProgress,
		Content: []model.OutputText{},
	}
	c.outputItems = append(c.outputItems, item)

	var out []model.SSEEvent
	out = append(out, model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
		Type: evOutputItemAdded, SequenceNumber: c.nextSeq(),
		OutputIndex: idx, Item: item,
	}))
	out = append(out, model.MarshalEvent(evContentPartAdded, model.ContentPartAddedEvent{
		Type: evContentPartAdded, SequenceNumber: c.nextSeq(),
		OutputIndex: idx, ContentIndex: c.textContentIdx, ItemID: itemID,
		Part: model.ContentPartOut{Type: model.ContentTypeOutputText},
	}))
	return out
}

func (c *Converter) handleThinkingStart(ev *anthropic.MessageStreamEventUnion, redacted bool) []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	c.openThinking = true
	c.thinkRedacted = redacted
	c.thinkItemIdx = idx
	c.thinkSummaryIdx = 0
	c.thinkBuilder.Reset()
	c.sigBuilder.Reset()
	// GLM 把 thinking signature 放在 content_block_start 的 content_block.signature
	// 字段中，而非像官方 Anthropic API 那样通过 signature_delta 事件流式下发。
	// 在此预取，使两种后端的 signature 都能被正确捕获。
	if !redacted && ev.ContentBlock.Signature != "" {
		c.sigBuilder.WriteString(ev.ContentBlock.Signature)
	}

	itemID := fmt.Sprintf("rs_%d", idx)

	if redacted {
		c.outputItems = append(c.outputItems, model.OutputItem{
			Type: model.ItemTypeReasoning, ID: itemID, Status: model.ResponseStatusInProgress,
			EncryptedContent: ev.ContentBlock.Data,
		})
	} else {
		c.outputItems = append(c.outputItems, model.OutputItem{
			Type: model.ItemTypeReasoning, ID: itemID, Status: model.ResponseStatusInProgress,
			Summary: []model.OutputText{},
		})
	}

	return []model.SSEEvent{model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
		Type: evOutputItemAdded, SequenceNumber: c.nextSeq(),
		OutputIndex: idx, Item: c.outputItems[idx],
	})}
}

func (c *Converter) handleToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	blkIdx := int(ev.Index)
	custom := c.customToolNames[ev.ContentBlock.Name]
	c.toolCalls[blkIdx] = toolCallState{itemIdx: idx, custom: custom}
	c.toolArgBuilders[blkIdx] = &strings.Builder{}

	itemID := fmt.Sprintf("fc_%d", idx)
	itemType := model.ItemTypeFunctionCall
	status := model.ResponseStatusInProgress
	if custom {
		itemID = fmt.Sprintf("ctc_%d", idx)
		itemType = model.ItemTypeCustomToolCall
		status = ""
	}
	item := model.OutputItem{
		Type: itemType, ID: itemID, Status: status,
		CallID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name,
	}
	c.outputItems = append(c.outputItems, item)

	return []model.SSEEvent{model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
		Type: evOutputItemAdded, SequenceNumber: c.nextSeq(),
		OutputIndex: idx, Item: item,
	})}
}

func (c *Converter) handleServerToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	// 只有 catalog 已注册的 hosted server tool 才映射为 Responses item；
	// 其余 server_tool_use（web_fetch、code_execution …批次 0 未注册）无安全
	// Responses 等价物，跳过以保持流继续。
	id, ok := toolcatalog.ServerToolByAnthropicName(ev.ContentBlock.Name)
	if !ok {
		return c.handleSkippedServerToolUseStart(ev)
	}
	switch id.OpenAIType {
	case "web_search":
		return c.handleWebSearchServerToolUseStart(ev)
	case "code_interpreter":
		return c.handleCodeExecutionServerToolUseStart(ev)
	}
	return c.handleSkippedServerToolUseStart(ev)
}

// handleWebSearchServerToolUseStart 把 Anthropic server_tool_use(web_search)
// 映射为 web_search_call item + 事件链（逐字搬迁自原 handleServerToolUseStart
// 的 web_search 分支，行为不变）。
func (c *Converter) handleWebSearchServerToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	itemID := fmt.Sprintf("ws_%d", idx)
	item := model.OutputItem{
		Type:   model.ItemTypeWebSearchCall,
		ID:     itemID,
		Status: model.ResponseStatusInProgress,
		Action: &model.WebSearchAction{Type: "search", Query: extractWebSearchQuery(ev.ContentBlock.Input)},
	}
	c.outputItems = append(c.outputItems, item)
	c.webSearchByToolUseID[ev.ContentBlock.ID] = idx

	return []model.SSEEvent{
		model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
			Type: evOutputItemAdded, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, Item: item,
		}),
		model.MarshalEvent(evWebSearchCallInProgress, model.WebSearchCallEvent{
			Type: evWebSearchCallInProgress, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
		model.MarshalEvent(evWebSearchCallSearching, model.WebSearchCallEvent{
			Type: evWebSearchCallSearching, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
	}
}

// handleCodeExecutionServerToolUseStart 把 Anthropic server_tool_use(code_execution)
// 映射为 code_interpreter_call item + 事件链。
// container_id 由网关合成（Anthropic 无 container，已知损失）。
// input.code 假设为 {"code": "..."}，字段名以 RED/wire 锁定。
func (c *Converter) handleCodeExecutionServerToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	itemID := fmt.Sprintf("ci_%d", idx)
	code := extractCodeExecutionCode(ev.ContentBlock.Input)
	item := model.OutputItem{
		Type:        model.ItemTypeCodeInterpreterCall,
		ID:          itemID,
		Status:      model.ResponseStatusInProgress,
		ContainerID: fmt.Sprintf("ci_container_%d", idx),
		Code:        code,
		Outputs:     []model.CodeInterpreterOutput{},
	}
	c.outputItems = append(c.outputItems, item)
	c.codeExecutionByToolUseID[ev.ContentBlock.ID] = idx

	out := []model.SSEEvent{
		model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
			Type: evOutputItemAdded, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, Item: item,
		}),
		model.MarshalEvent(evCodeInterpreterCallInProgress, model.CodeInterpreterCallEvent{
			Type: evCodeInterpreterCallInProgress, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
		model.MarshalEvent(evCodeInterpreterCallInterpreting, model.CodeInterpreterCallEvent{
			Type: evCodeInterpreterCallInterpreting, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
	}
	if code != "" {
		out = append(out,
			model.MarshalEvent(evCodeInterpreterCallCodeDelta, model.CodeInterpreterCallCodeDeltaEvent{
				Type: evCodeInterpreterCallCodeDelta, SequenceNumber: c.nextSeq(),
				OutputIndex: idx, ItemID: itemID, Delta: code,
			}),
			model.MarshalEvent(evCodeInterpreterCallCodeDone, model.CodeInterpreterCallCodeDoneEvent{
				Type: evCodeInterpreterCallCodeDone, SequenceNumber: c.nextSeq(),
				OutputIndex: idx, ItemID: itemID, Code: code,
			}),
		)
	}
	return out
}

// extractCodeExecutionCode 从 server_tool_use(code_execution) 的 input 取出代码。
// input 是 free-form JSON，假设 {"code": "..."}（spec 第 113 行风险点，RED 锁定）。
func extractCodeExecutionCode(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	if c, ok := m["code"].(string); ok {
		return c
	}
	return ""
}

// handleCodeExecutionResultStart 把 code_execution_tool_result 映射为
// code_interpreter_call 的 outputs（stdout/stderr → logs）+ completed。
// file_id（代码生成的文件）无 url 凭据不可转换，丢弃 + WARN。
func (c *Converter) handleCodeExecutionResultStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx, ok := c.codeExecutionByToolUseID[ev.ContentBlock.ToolUseID]
	if !ok || idx >= len(c.outputItems) {
		return nil // 无关联的 code_execution server_tool_use，交由 handleBlockStart 兜底 skip
	}
	itemID := fmt.Sprintf("ci_%d", idx)
	// code_execution_tool_result 的 stdout/stderr 在 ev.ContentBlock.Content
	// （CodeExecutionToolResultBlockContentUnion），而非顶层或 AsCodeExecutionToolResult。
	// 生成的文件列表在其 Content.OfContent（[]CodeExecutionOutputBlock）。
	rc := ev.ContentBlock.Content
	logs := foldExecutionLogs(rc.Stdout, rc.Stderr)
	c.outputItems[idx].Status = model.ResponseStatusCompleted
	if logs != "" {
		c.outputItems[idx].Outputs = []model.CodeInterpreterOutput{{Type: "logs", Logs: logs}}
	}
	for _, out := range rc.Content.OfContent {
		if out.FileID != "" {
			slog.Warn("丢弃 code execution 生成的文件（无 OpenAI files url 凭据）",
				"response_id", c.respID, "tool_use_id", ev.ContentBlock.ToolUseID, "file_id", out.FileID)
		}
	}
	return []model.SSEEvent{
		model.MarshalEvent(evCodeInterpreterCallCompleted, model.CodeInterpreterCallEvent{
			Type: evCodeInterpreterCallCompleted, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, Item: c.outputItems[idx],
		}),
	}
}

// foldExecutionLogs 把 stdout 与非空 stderr 合并为 logs 文本（OpenAI logs 承载 stdout/stderr）。
func foldExecutionLogs(stdout, stderr string) string {
	var parts []string
	if stdout != "" {
		parts = append(parts, stdout)
	}
	if stderr != "" {
		parts = append(parts, stderr)
	}
	return strings.Join(parts, "\n")
}

func (c *Converter) handleWebSearchResultStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx, ok := c.webSearchByToolUseID[ev.ContentBlock.ToolUseID]
	if !ok || idx >= len(c.outputItems) {
		return nil // no matching web_search server_tool_use; nothing to close
	}
	itemID := fmt.Sprintf("ws_%d", idx)
	c.outputItems[idx].Status = model.ResponseStatusCompleted
	if sources := extractWebSearchSources(ev.ContentBlock.Content); len(sources) > 0 && c.outputItems[idx].Action != nil {
		c.outputItems[idx].Action.Sources = sources
	}
	return []model.SSEEvent{
		model.MarshalEvent(evWebSearchCallCompleted, model.WebSearchCallEvent{
			Type: evWebSearchCallCompleted, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, Item: c.outputItems[idx],
		}),
	}
}

// extractWebSearchSources maps Anthropic web_search_tool_result entries to
// OpenAI web_search_call sources. Only the URL is carried — title and
// encrypted_content have no OpenAI equivalent field.
func extractWebSearchSources(content anthropic.ContentBlockStartEventContentBlockUnionContent) []model.WebSearchSource {
	var out []model.WebSearchSource
	for _, r := range content.OfWebSearchResultBlockArray {
		if r.URL != "" {
			out = append(out, model.WebSearchSource{Type: "url", URL: r.URL})
		}
	}
	return out
}

// handleSkippedServerToolUseStart marks a non-web_search server_tool_use block
// (web_fetch, code_execution, ...) as skipped. The block index is tracked so
// subsequent delta and stop events for this index are also ignored.
func (c *Converter) handleSkippedServerToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	blkIdx := int(ev.Index)
	c.skippedBlocks[blkIdx] = true
	slog.Warn("跳过无 Responses 等价物的 server_tool_use block，对应数据被丢弃",
		"response_id", c.respID, "block_index", blkIdx, "name", ev.ContentBlock.Name)
	return nil
}

// handleSkippedBlockStart marks a content block that has no Responses
// equivalent (tool_result, web_fetch_tool_result, code_execution_tool_result,
// ...) as skipped. The block index is tracked so subsequent delta and stop
// events for this index are also ignored.
func (c *Converter) handleSkippedBlockStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	blkIdx := int(ev.Index)
	c.skippedBlocks[blkIdx] = true
	slog.Warn("跳过无 Responses 等价物的 content block，对应数据被丢弃",
		"response_id", c.respID, "block_index", blkIdx, "block_type", ev.ContentBlock.Type)
	return nil
}

// extractWebSearchQuery pulls the search query out of an Anthropic web_search
// server_tool_use input. The input is a free-form JSON value; the query lives
// under the "query" key.
func extractWebSearchQuery(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	if q, ok := m["query"].(string); ok {
		return q
	}
	return ""
}

func (c *Converter) handleBlockDelta(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	blkIdx := int(ev.Index)
	if c.skippedBlocks[blkIdx] {
		return nil
	}
	switch ev.Delta.Type {
	case anDeltaText:
		if !c.openText {
			return nil
		}
		c.textBuilder.WriteString(ev.Delta.Text)
		return []model.SSEEvent{model.MarshalEvent(evOutputTextDelta, model.OutputTextDeltaEvent{
			Type: evOutputTextDelta, SequenceNumber: c.nextSeq(),
			OutputIndex: c.textItemIdx, ContentIndex: c.textContentIdx,
			ItemID: fmt.Sprintf("msg_%d", c.textItemIdx), Delta: ev.Delta.Text,
		})}
	case anDeltaThinking:
		if !c.openThinking {
			return nil
		}
		c.thinkBuilder.WriteString(ev.Delta.Thinking)
		if c.summarized {
			return nil // deltas emitted as summary events on block stop
		}
		return []model.SSEEvent{model.MarshalEvent(evReasoningTextDelta, model.ReasoningTextDeltaEvent{
			Type: evReasoningTextDelta, SequenceNumber: c.nextSeq(),
			OutputIndex: c.thinkItemIdx, ContentIndex: 0,
			ItemID: fmt.Sprintf("rs_%d", c.thinkItemIdx), Delta: ev.Delta.Thinking,
		})}
	case anDeltaSignature:
		if c.openThinking {
			c.sigBuilder.WriteString(ev.Delta.Signature)
		}
		return nil
	case anDeltaInputJSON:
		blkIdx := int(ev.Index)
		state, ok := c.toolCalls[blkIdx]
		if !ok {
			return nil
		}
		if b, ok := c.toolArgBuilders[blkIdx]; ok {
			b.WriteString(ev.Delta.PartialJSON)
		}
		if state.custom {
			return nil
		}
		return []model.SSEEvent{model.MarshalEvent(evFunctionCallArgumentsDelta, model.FunctionCallArgumentsDeltaEvent{
			Type: evFunctionCallArgumentsDelta, SequenceNumber: c.nextSeq(),
			OutputIndex: state.itemIdx, ItemID: fmt.Sprintf("fc_%d", state.itemIdx),
			Delta: ev.Delta.PartialJSON,
		})}
	}
	return nil
}

func (c *Converter) handleBlockStop(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	var out []model.SSEEvent

	blkIdx := int(ev.Index)
	if c.skippedBlocks[blkIdx] {
		delete(c.skippedBlocks, blkIdx)
		return nil
	}

	if c.openText {
		c.openText = false
		itemID := fmt.Sprintf("msg_%d", c.textItemIdx)
		text := c.textBuilder.String()
		if c.textItemIdx < len(c.outputItems) {
			c.outputItems[c.textItemIdx].Content = []model.OutputText{
				{Type: model.ContentTypeOutputText, Text: text},
			}
		}
		out = append(out, model.MarshalEvent(evOutputTextDone, model.OutputTextDoneEvent{
			Type: evOutputTextDone, SequenceNumber: c.nextSeq(),
			OutputIndex: c.textItemIdx, ContentIndex: c.textContentIdx,
			ItemID: itemID, Text: text,
		}))
		out = append(out, model.MarshalEvent(evContentPartDone, model.ContentPartDoneEvent{
			Type: evContentPartDone, SequenceNumber: c.nextSeq(),
			OutputIndex: c.textItemIdx, ContentIndex: c.textContentIdx,
			ItemID: itemID, Part: model.ContentPartOut{Type: model.ContentTypeOutputText, Text: text},
		}))
		c.outputItems[c.textItemIdx].Status = model.ResponseStatusCompleted
		out = append(out, model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: c.textItemIdx, Item: c.outputItems[c.textItemIdx],
		}))
	}

	if c.openThinking {
		c.openThinking = false
		itemID := fmt.Sprintf("rs_%d", c.thinkItemIdx)
		thinkText := c.thinkBuilder.String()
		sigText := c.sigBuilder.String()
		// handleThinkingStart guarantees the item exists at c.thinkItemIdx,
		// so no bounds check is needed here (consistent with text/tool branches).
		if !c.thinkRedacted {
			c.outputItems[c.thinkItemIdx].Summary = []model.OutputText{
				{Type: model.ContentTypeSummaryText, Text: thinkText},
			}
			c.outputItems[c.thinkItemIdx].Signature = sigText
			// 把 signature 同步到 encrypted_content，使 disable_response_storage=true
			// 时 Codex 能通过标准字段在 ZDR transcript 中保存并回传 thinking signature。
			c.outputItems[c.thinkItemIdx].EncryptedContent = sigText
		}
		if c.summarized && !c.thinkRedacted {
			out = append(out, c.emitSummaryEvents(itemID, thinkText)...)
		} else if !c.thinkRedacted {
			out = append(out, model.MarshalEvent(evReasoningTextDone, model.ReasoningTextDoneEvent{
				Type: evReasoningTextDone, SequenceNumber: c.nextSeq(),
				OutputIndex: c.thinkItemIdx, ItemID: itemID, Text: thinkText,
			}))
		}
		c.outputItems[c.thinkItemIdx].Status = model.ResponseStatusCompleted
		out = append(out, model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: c.thinkItemIdx, Item: c.outputItems[c.thinkItemIdx],
		}))
	}

	if state, ok := c.toolCalls[blkIdx]; ok {
		itemIdx := state.itemIdx
		itemID := fmt.Sprintf("fc_%d", itemIdx)
		args := ""
		if b, ok := c.toolArgBuilders[blkIdx]; ok {
			args = b.String()
		}
		if state.custom {
			itemID = fmt.Sprintf("ctc_%d", itemIdx)
			input := customToolInput(args)
			if itemIdx < len(c.outputItems) {
				c.outputItems[itemIdx].Input = input
			}
			if input != "" {
				out = append(out, model.MarshalEvent(evCustomToolCallInputDelta, model.CustomToolCallInputDeltaEvent{
					Type: evCustomToolCallInputDelta, SequenceNumber: c.nextSeq(),
					OutputIndex: itemIdx, ItemID: itemID, Delta: input,
				}))
			}
			out = append(out, model.MarshalEvent(evCustomToolCallInputDone, model.CustomToolCallInputDoneEvent{
				Type: evCustomToolCallInputDone, SequenceNumber: c.nextSeq(),
				OutputIndex: itemIdx, ItemID: itemID, Input: input,
			}))
			out = append(out, model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
				Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
				OutputIndex: itemIdx, Item: c.outputItems[itemIdx],
			}))
			delete(c.toolCalls, blkIdx)
			delete(c.toolArgBuilders, blkIdx)
			return out
		}
		if itemIdx < len(c.outputItems) {
			c.outputItems[itemIdx].Arguments = args
			c.outputItems[itemIdx].Status = model.ResponseStatusCompleted
		}
		out = append(out, model.MarshalEvent(evFunctionCallArgumentsDone, model.FunctionCallArgumentsDoneEvent{
			Type: evFunctionCallArgumentsDone, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, ItemID: itemID, Arguments: args,
		}))
		out = append(out, model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, Item: c.outputItems[itemIdx],
		}))
		delete(c.toolCalls, blkIdx)
		delete(c.toolArgBuilders, blkIdx)
	}

	return out
}

func customToolInput(raw string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return raw
	}
	if input, ok := obj["input"].(string); ok {
		return input
	}
	return raw
}

func (c *Converter) emitSummaryEvents(itemID, text string) []model.SSEEvent {
	var out []model.SSEEvent
	out = append(out, model.MarshalEvent(evReasoningSummaryPartAdded, model.ReasoningSummaryPartAddedEvent{
		Type: evReasoningSummaryPartAdded, SequenceNumber: c.nextSeq(),
		OutputIndex: c.thinkItemIdx, SummaryIndex: c.thinkSummaryIdx,
		ItemID: itemID, Part: model.SummaryPart{Type: model.ContentTypeSummaryText, Text: ""},
	}))
	if text != "" {
		out = append(out, model.MarshalEvent(evReasoningSummaryTextDelta, model.ReasoningSummaryTextDeltaEvent{
			Type: evReasoningSummaryTextDelta, SequenceNumber: c.nextSeq(),
			OutputIndex: c.thinkItemIdx, SummaryIndex: c.thinkSummaryIdx,
			ItemID: itemID, Delta: text,
		}))
	}
	out = append(out, model.MarshalEvent(evReasoningSummaryTextDone, model.ReasoningSummaryTextDoneEvent{
		Type: evReasoningSummaryTextDone, SequenceNumber: c.nextSeq(),
		OutputIndex: c.thinkItemIdx, SummaryIndex: c.thinkSummaryIdx,
		ItemID: itemID, Text: text,
	}))
	out = append(out, model.MarshalEvent(evReasoningSummaryPartDone, model.ReasoningSummaryPartDoneEvent{
		Type: evReasoningSummaryPartDone, SequenceNumber: c.nextSeq(),
		OutputIndex: c.thinkItemIdx, SummaryIndex: c.thinkSummaryIdx,
		ItemID: itemID, Part: model.SummaryPart{Type: model.ContentTypeSummaryText, Text: text},
	}))
	return out
}

func (c *Converter) recordStopReason(ev *anthropic.MessageStreamEventUnion) {
	c.stopReason = string(ev.Delta.StopReason)
	if ev.Delta.StopReason == anthropic.StopReasonRefusal {
		c.refusalText = ev.Delta.StopDetails.Explanation
		if c.refusalText == "" {
			c.refusalText = refusalFallback
		}
	}
	if ev.Usage.OutputTokens > 0 || ev.Usage.InputTokens > 0 ||
		ev.Usage.CacheReadInputTokens > 0 || ev.Usage.CacheCreationInputTokens > 0 {
		c.usage = &model.ResponseUsage{
			InputTokens:              int(ev.Usage.InputTokens),
			OutputTokens:             int(ev.Usage.OutputTokens),
			CacheReadInputTokens:     int(ev.Usage.CacheReadInputTokens),
			CacheCreationInputTokens: int(ev.Usage.CacheCreationInputTokens),
		}
		c.usage.TotalTokens = c.usage.InputTokens + c.usage.OutputTokens
	}
}

func statusFor(reason string) (status, incompleteReason string) {
	switch anthropic.StopReason(reason) {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonToolUse:
		return model.ResponseStatusCompleted, ""
	case anthropic.StopReasonMaxTokens:
		return model.ResponseStatusIncomplete, model.IncompleteReasonMaxOutputTokens
	case anthropic.StopReasonPauseTurn:
		return model.ResponseStatusIncomplete, ""
	case anthropic.StopReasonRefusal:
		return model.ResponseStatusIncomplete, model.IncompleteReasonContentFilter
	case anthropic.StopReasonStopSequence:
		return model.ResponseStatusCompleted, ""
	default:
		if reason == model.IncompleteReasonContentFilter {
			return model.ResponseStatusIncomplete, model.IncompleteReasonContentFilter
		}
		return model.ResponseStatusCompleted, ""
	}
}

func (c *Converter) handleComplete() []model.SSEEvent {
	var out []model.SSEEvent
	if c.stopReason == string(anthropic.StopReasonRefusal) {
		c.resetOutputForRefusal()
		out = append(out, c.emitRefusalEvents()...)
	}

	status, incompleteReason := statusFor(c.stopReason)

	resp := model.NewResponseObject(c.respID, status, c.model, c.createdAt, c.echo)
	resp.Output = c.outputItems
	if len(resp.Output) == 0 {
		resp.Output = []model.OutputItem{}
	}
	if status == model.ResponseStatusCompleted {
		resp.CompletedAt = time.Now().Unix()
	}
	if c.usage != nil {
		resp.Usage = c.usage
	}
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

func (c *Converter) resetOutputForRefusal() {
	c.itemOrder = 0
	c.openText = false
	c.textItemIdx = 0
	c.textContentIdx = 0
	c.textBuilder.Reset()
	c.openThinking = false
	c.thinkItemIdx = 0
	c.thinkSummaryIdx = 0
	c.thinkBuilder.Reset()
	c.sigBuilder.Reset()
	c.toolCalls = map[int]toolCallState{}
	c.toolArgBuilders = map[int]*strings.Builder{}
	c.outputItems = []model.OutputItem{}
	c.skippedBlocks = map[int]bool{}
}

func (c *Converter) emitRefusalEvents() []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	itemID := fmt.Sprintf("msg_%d", idx)
	refusal := c.refusalText
	refusalPart := model.OutputText{Type: model.ContentTypeRefusal, Refusal: &refusal}
	emptyRefusal := ""
	addedRefusalPart := model.ContentPartOut{Type: model.ContentTypeRefusal, Refusal: &emptyRefusal}
	refusalEventPart := model.ContentPartOut{Type: model.ContentTypeRefusal, Refusal: &refusal}
	addedItem := model.OutputItem{
		Type:    model.ItemTypeMessage,
		ID:      itemID,
		Role:    model.RoleAssistant,
		Phase:   model.AssistantPhaseFinalAnswer,
		Status:  model.ResponseStatusInProgress,
		Content: []model.OutputText{},
	}
	doneItem := model.OutputItem{
		Type:    model.ItemTypeMessage,
		ID:      itemID,
		Role:    model.RoleAssistant,
		Phase:   model.AssistantPhaseFinalAnswer,
		Status:  model.ResponseStatusCompleted,
		Content: []model.OutputText{refusalPart},
	}
	c.outputItems = append(c.outputItems, doneItem)

	return []model.SSEEvent{
		model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
			Type: evOutputItemAdded, SequenceNumber: c.nextSeq(), OutputIndex: idx, Item: addedItem,
		}),
		model.MarshalEvent(evContentPartAdded, model.ContentPartAddedEvent{
			Type: evContentPartAdded, SequenceNumber: c.nextSeq(), OutputIndex: idx, ContentIndex: 0,
			ItemID: itemID, Part: addedRefusalPart,
		}),
		model.MarshalEvent(evRefusalDelta, model.RefusalDeltaEvent{
			Type: evRefusalDelta, SequenceNumber: c.nextSeq(), OutputIndex: idx, ContentIndex: 0,
			ItemID: itemID, Delta: c.refusalText,
		}),
		model.MarshalEvent(evRefusalDone, model.RefusalDoneEvent{
			Type: evRefusalDone, SequenceNumber: c.nextSeq(), OutputIndex: idx, ContentIndex: 0,
			ItemID: itemID, Refusal: c.refusalText,
		}),
		model.MarshalEvent(evContentPartDone, model.ContentPartDoneEvent{
			Type: evContentPartDone, SequenceNumber: c.nextSeq(), OutputIndex: idx, ContentIndex: 0,
			ItemID: itemID, Part: refusalEventPart,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(), OutputIndex: idx, Item: doneItem,
		}),
	}
}

func (c *Converter) handleError(ev *anthropic.MessageStreamEventUnion) model.SSEEvent {
	c.completed = true
	c.failed = true
	msg := "upstream stream error"
	if ev.Delta.Text != "" {
		msg = ev.Delta.Text
	}
	slog.Warn("收到上游 error 事件，转为 response.failed",
		"response_id", c.respID, "message", msg)
	resp := model.NewResponseObject(c.respID, model.ResponseStatusFailed, c.model, c.createdAt, c.echo)
	resp.Output = []model.OutputItem{}
	resp.Error = &model.ResponseError{Message: msg}
	return model.MarshalEvent(evResponseFailed, model.TerminalResponseEvent{
		Type: evResponseFailed, SequenceNumber: c.nextSeq(), Response: resp,
	})
}
