// Package streamconv converts Anthropic stream events into Responses SSE events.
package streamconv

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	aconstant "github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
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
)

var (
	anMessageStart      = string(aconstant.ValueOf[aconstant.MessageStart]())
	anContentBlockStart = string(aconstant.ValueOf[aconstant.ContentBlockStart]())
	anContentBlockDelta = string(aconstant.ValueOf[aconstant.ContentBlockDelta]())
	anContentBlockStop  = string(aconstant.ValueOf[aconstant.ContentBlockStop]())
	anMessageDelta      = string(aconstant.ValueOf[aconstant.MessageDelta]())
	anMessageStop       = string(aconstant.ValueOf[aconstant.MessageStop]())
	anError             = string(aconstant.ValueOf[aconstant.Error]())

	anBlockText             = string(aconstant.ValueOf[aconstant.Text]())
	anBlockThinking         = string(aconstant.ValueOf[aconstant.Thinking]())
	anBlockRedactedThinking = string(aconstant.ValueOf[aconstant.RedactedThinking]())
	anBlockToolUse          = string(aconstant.ValueOf[aconstant.ToolUse]())

	anDeltaText      = string(aconstant.ValueOf[aconstant.TextDelta]())
	anDeltaThinking  = string(aconstant.ValueOf[aconstant.ThinkingDelta]())
	anDeltaSignature = string(aconstant.ValueOf[aconstant.SignatureDelta]())
	anDeltaInputJSON = string(aconstant.ValueOf[aconstant.InputJSONDelta]())
)

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

	// Accumulators
	textBuilder  strings.Builder
	thinkBuilder strings.Builder
	sigBuilder   strings.Builder

	stopReason string
	usage      *model.ResponseUsage
	completed  bool

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

// Seq returns the current sequence number for use by callers that need to
// emit terminal events outside the converter (e.g. server-side response.failed).
func (c *Converter) Seq() int64 { return c.seq }

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
			out = append(out, c.handleComplete())
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
	}
	return nil
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

func (c *Converter) handleBlockDelta(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
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

	blkIdx := int(ev.Index)
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
	if ev.Usage.OutputTokens > 0 || ev.Usage.InputTokens > 0 {
		c.usage = &model.ResponseUsage{
			InputTokens:  int(ev.Usage.InputTokens),
			OutputTokens: int(ev.Usage.OutputTokens),
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
		return model.ResponseStatusIncomplete, model.IncompleteReasonPauseTurn
	case anthropic.StopReasonRefusal:
		return model.ResponseStatusIncomplete, model.IncompleteReasonRefusal
	case anthropic.StopReasonStopSequence:
		return model.ResponseStatusCompleted, ""
	default:
		if reason == model.IncompleteReasonContentFilter {
			return model.ResponseStatusIncomplete, model.IncompleteReasonContentFilter
		}
		return model.ResponseStatusCompleted, ""
	}
}

func (c *Converter) handleComplete() model.SSEEvent {
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

	return model.MarshalEvent(eventType, model.TerminalResponseEvent{
		Type: eventType, SequenceNumber: c.nextSeq(), Response: resp,
	})
}

func (c *Converter) handleError(ev *anthropic.MessageStreamEventUnion) model.SSEEvent {
	c.completed = true
	msg := "upstream stream error"
	if ev.Delta.Text != "" {
		msg = ev.Delta.Text
	}
	resp := model.NewResponseObject(c.respID, model.ResponseStatusFailed, c.model, c.createdAt, c.echo)
	resp.Output = []model.OutputItem{}
	resp.Error = &model.ResponseError{Message: msg}
	return model.MarshalEvent(evResponseFailed, model.TerminalResponseEvent{
		Type: evResponseFailed, SequenceNumber: c.nextSeq(), Response: resp,
	})
}
