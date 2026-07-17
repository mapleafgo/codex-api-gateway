package streamconv

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// webSearchCallKind 把 Anthropic server_tool_use(web_search) 映射为 web_search_call。
//
// 事件链：output_item.added(web_search_call, in_progress) → web_search_call.in_progress
// → web_search_call.searching。block stop 不产 done（web_search 在 result block 完成）。
// result（web_search_tool_result / GLM 方言 tool_result）由 handleCallResult +
// webSearchCallKind.handleResult 驱动完成（completed + sources）。
type webSearchCallKind struct{}

func (webSearchCallKind) itemType() string      { return model.ItemTypeWebSearchCall }
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
	if itemIdx >= len(c.outputItems) {
		return nil
	}
	itemID := fmt.Sprintf("ws_%d", itemIdx)
	c.outputItems[itemIdx].Status = model.ResponseStatusCompleted
	if sources := extractWebSearchSources(ev.ContentBlock.Content); len(sources) > 0 && c.outputItems[itemIdx].Action != nil {
		c.outputItems[itemIdx].Action.Sources = sources
	}
	return []model.SSEEvent{
		model.MarshalEvent(evWebSearchCallCompleted, model.WebSearchCallEvent{
			Type: evWebSearchCallCompleted, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, ItemID: itemID,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, Item: c.outputItems[itemIdx],
		}),
	}
}

// extractWebSearchSources maps Anthropic web_search_tool_result entries to
// OpenAI web_search_call sources. Only the URL is carried — title and
// encrypted_content have no OpenAI equivalent field.
//
// 兼容端（如 GLM web_search_prime）不把结果放进 tool_result block 的 content
// （实测 content 各字段皆空），而是用 text 自述承载 result_summary + link，
// 已由 text 路径透传给客户端。故此处只处理标准 web_search_tool_result 数组，
// 不解析 text——拆 text 违背透传契约，且 link 已对客户端可见。
func extractWebSearchSources(content anthropic.ContentBlockStartEventContentBlockUnionContent) []model.WebSearchSource {
	var out []model.WebSearchSource
	for _, r := range content.OfWebSearchResultBlockArray {
		if r.URL != "" {
			out = append(out, model.WebSearchSource{Type: "url", URL: r.URL})
		}
	}
	return out
}

// extractWebSearchQuery 从 web_search server_tool_use 的 Input 中取出 query。
// Input 是自由 JSON 值，query 在 "query" 键下。
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
