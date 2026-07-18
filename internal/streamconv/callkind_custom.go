package streamconv

import (
	"encoding/json"

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

func (customCallKind) buildItem(_ int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem {
	return model.OutputItem{
		Type:   model.ItemTypeCustomToolCall,
		ID:     itemID,
		Status: "", // custom tool call 不设 in_progress（Codex 客户端按 done 即可渲染）
		CallID: ev.ContentBlock.ID,
		Name:   ev.ContentBlock.Name,
	}
}

func (customCallKind) startEvents(_ *Converter, _ int, _ string) []model.SSEEvent {
	return nil
}

func (customCallKind) consumeDelta(_ *Converter, _ *callState, _ string) []model.SSEEvent {
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

func (customCallKind) handleResult(_ *Converter, _ *anthropic.MessageStreamEventUnion, _ int) []model.SSEEvent {
	return nil
}

// customToolInput 从累积的 custom tool args（{"input": ...}）中解出 input 文本。
// 解析失败时原样返回 raw。
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
