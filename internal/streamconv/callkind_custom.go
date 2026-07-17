package streamconv

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// customCallKind 把 tool_use（customToolNames 命中，含 shell/apply_patch）映射为
// custom_tool_call。input 在 stop 时一次性给出（custom_tool_call.input.delta/done），
// input 经 customToolInput 从累积 args 解析（{"input": ...}）。无 result block。
type customCallKind struct{}

func (customCallKind) itemType() string      { return model.ItemTypeCustomToolCall }
func (customCallKind) idPrefix() string      { return "ctc" }
func (customCallKind) tracksToolUseID() bool { return false }

func (customCallKind) buildItem(itemIdx int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem {
	return model.OutputItem{
		Type:   model.ItemTypeCustomToolCall,
		ID:     itemID,
		Status: "", // custom tool call 不设 in_progress（Codex 客户端按 done 即可渲染）
		CallID: ev.ContentBlock.ID,
		Name:   ev.ContentBlock.Name,
	}
}

func (customCallKind) startEvents(c *Converter, itemIdx int, itemID string) []model.SSEEvent {
	return nil
}

func (customCallKind) consumeDelta(c *Converter, st *callState, partial string) []model.SSEEvent {
	return nil // input 在 stop 一次性给（与旧 handleBlockStop custom 分支一致）
}

func (customCallKind) finish(c *Converter, st *callState, args string) (model.OutputItem, []model.SSEEvent) {
	item := c.outputItems[st.itemIdx]
	input := customToolInput(args)
	item.Input = input
	var evts []model.SSEEvent
	if input != "" {
		evts = append(evts, model.MarshalEvent(evCustomToolCallInputDelta, model.CustomToolCallInputDeltaEvent{
			Type: evCustomToolCallInputDelta, SequenceNumber: c.nextSeq(),
			OutputIndex: st.itemIdx, ItemID: st.itemID, Delta: input,
		}))
	}
	evts = append(evts, model.MarshalEvent(evCustomToolCallInputDone, model.CustomToolCallInputDoneEvent{
		Type: evCustomToolCallInputDone, SequenceNumber: c.nextSeq(),
		OutputIndex: st.itemIdx, ItemID: st.itemID, Input: input,
	}))
	evts = append(evts, model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
		Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
		OutputIndex: st.itemIdx, Item: item,
	}))
	return item, evts
}

func (customCallKind) handleResult(c *Converter, ev *anthropic.MessageStreamEventUnion, itemIdx int) []model.SSEEvent {
	return nil
}
