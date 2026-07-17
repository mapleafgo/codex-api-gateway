package streamconv

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// webSearchCallKind 把 Anthropic server_tool_use(web_search) 映射为 web_search_call。
//
// 事件链：output_item.added(web_search_call, in_progress) → web_search_call.in_progress
// → web_search_call.searching。block stop 不产 done（web_search 在 result block 完成）。
// result（web_search_tool_result / GLM 方言 tool_result）由 handleWebSearchResultStart
// 处理（S7 迁入 handleResult）。
type webSearchCallKind struct{}

func (webSearchCallKind) itemType() string     { return model.ItemTypeWebSearchCall }
func (webSearchCallKind) idPrefix() string      { return "ws" }
func (webSearchCallKind) tracksToolUseID() bool { return true }

func (webSearchCallKind) buildItem(itemIdx int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem {
	return model.OutputItem{
		Type:   model.ItemTypeWebSearchCall,
		ID:     itemID,
		Status: model.ResponseStatusInProgress,
		Action: &model.WebSearchAction{Type: "search", Query: extractWebSearchQuery(ev.ContentBlock.Input)},
	}
}

func (webSearchCallKind) startEvents(c *Converter, itemIdx int, itemID string) []model.SSEEvent {
	return []model.SSEEvent{
		model.MarshalEvent(evWebSearchCallInProgress, model.WebSearchCallEvent{
			Type: evWebSearchCallInProgress, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, ItemID: itemID,
		}),
		model.MarshalEvent(evWebSearchCallSearching, model.WebSearchCallEvent{
			Type: evWebSearchCallSearching, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, ItemID: itemID,
		}),
	}
}

func (webSearchCallKind) consumeDelta(c *Converter, st *callState, partial string) []model.SSEEvent {
	return nil
}

func (webSearchCallKind) finish(c *Converter, st *callState, args string) (model.OutputItem, []model.SSEEvent) {
	// web_search 在 result block（web_search_tool_result）完成，不在 server_tool_use
	// 的 content_block_stop 完成。故 block stop 返回 item 不变 + 空事件。
	return c.outputItems[st.itemIdx], nil
}

func (webSearchCallKind) handleResult(c *Converter, ev *anthropic.MessageStreamEventUnion, itemIdx int) []model.SSEEvent {
	return nil // S7 迁入
}
