package streamconv

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// functionCallKind 把 tool_use（非 custom、非 tool_search）映射为 function_call。
//
// 事件链：output_item.added(function_call, in_progress) → function_call.arguments.delta
// （每段 input_json_delta）→ function_call.arguments.done（完整 args）→ output_item.done。
// 无 result block（client 执行，结果经 function_call_output 下轮回灌）。
type functionCallKind struct{}

func (functionCallKind) itemType() string     { return model.ItemTypeFunctionCall }
func (functionCallKind) idPrefix() string      { return "fc" }
func (functionCallKind) tracksToolUseID() bool { return false }

func (functionCallKind) buildItem(itemIdx int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem {
	return model.OutputItem{
		Type:   model.ItemTypeFunctionCall,
		ID:     itemID,
		Status: model.ResponseStatusInProgress,
		CallID: ev.ContentBlock.ID,
		Name:   ev.ContentBlock.Name,
	}
}

func (functionCallKind) startEvents(c *Converter, itemIdx int, itemID string) []model.SSEEvent {
	return nil
}

func (functionCallKind) consumeDelta(c *Converter, st *callState, partial string) []model.SSEEvent {
	return []model.SSEEvent{model.MarshalEvent(evFunctionCallArgumentsDelta, model.FunctionCallArgumentsDeltaEvent{
		Type: evFunctionCallArgumentsDelta, SequenceNumber: c.nextSeq(),
		OutputIndex: st.itemIdx, ItemID: st.itemID, Delta: partial,
	})}
}

func (functionCallKind) finish(c *Converter, st *callState, args string) (model.OutputItem, []model.SSEEvent) {
	item := c.outputItems[st.itemIdx]
	item.Arguments = args
	item.Status = model.ResponseStatusCompleted
	evts := []model.SSEEvent{
		model.MarshalEvent(evFunctionCallArgumentsDone, model.FunctionCallArgumentsDoneEvent{
			Type: evFunctionCallArgumentsDone, SequenceNumber: c.nextSeq(),
			OutputIndex: st.itemIdx, ItemID: st.itemID, Arguments: args,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: st.itemIdx, Item: item,
		}),
	}
	return item, evts
}

func (functionCallKind) handleResult(c *Converter, ev *anthropic.MessageStreamEventUnion, itemIdx int) []model.SSEEvent {
	return nil
}
