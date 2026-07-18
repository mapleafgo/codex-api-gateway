package streamconv

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// toolSearchCallKind 把 tool_use(tool_search) 映射为 tool_search_call。
//
// tool_search 是 OpenAI Responses 的 hosted tool，Anthropic 无等价——请求侧由
// toolcatalog.Declare 降级成普通 tool（name="tool_search"），GLM 调用后由 Codex
// 本地执行搜索（deferred tools 的持有者在 Codex，后端无法搜），故 execution=client。
//
// 事件链：output_item.added(tool_search_call, in_progress) → output_item.done
// （completed + 完整 arguments）。无专门 delta/done 事件（SDK 无
// ResponseToolSearchCallArgumentsDelta/Done），arguments 只随 item 携带。
type toolSearchCallKind struct{}

func (toolSearchCallKind) itemType() string      { return model.ItemTypeToolSearchCall }
func (toolSearchCallKind) idPrefix() string      { return "tsc" }
func (toolSearchCallKind) tracksToolUseID() bool { return false }

func (toolSearchCallKind) buildItem(_ int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem {
	return model.OutputItem{
		Type:      model.ItemTypeToolSearchCall,
		ID:        itemID,
		Status:    model.ResponseStatusInProgress,
		CallID:    ev.ContentBlock.ID,
		Execution: "client",
	}
}

func (toolSearchCallKind) startEvents(_ *Converter, _ int, _ string) []model.SSEEvent {
	return nil
}

func (toolSearchCallKind) consumeDelta(_ *Converter, _ *callState, _ string) []model.SSEEvent {
	return nil // 无专门 delta 事件，arguments 只随 output_item.added/done 携带
}

func (toolSearchCallKind) finish(c *Converter, st *callState, args string) (model.OutputItem, []model.SSEEvent) {
	item := c.outputItems[st.itemIdx]
	item.Arguments = args
	item.Status = model.ResponseStatusCompleted
	evts := []model.SSEEvent{
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: st.itemIdx, Item: item,
		}),
	}
	return item, evts
}

func (toolSearchCallKind) handleResult(_ *Converter, _ *anthropic.MessageStreamEventUnion, _ int) []model.SSEEvent {
	return nil
}
