package streamconv

import (
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// handleCallStart 是通用 content_block_start 处理：分配 item → buildItem →
// output_item.added → startEvents。调用方先 dispatchCallKind 拿到非 nil kind 再调用。
func (c *Converter) handleCallStart(ev *anthropic.MessageStreamEventUnion, kind callKind) []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	blkIdx := int(ev.Index)
	itemID := fmt.Sprintf("%s_%d", kind.idPrefix(), idx)

	item := kind.buildItem(idx, itemID, ev)
	c.outputItems = append(c.outputItems, item)
	c.callByBlockIdx[blkIdx] = &callState{
		kind:       kind,
		itemIdx:    idx,
		itemID:     itemID,
		callID:     ev.ContentBlock.ID,
		name:       ev.ContentBlock.Name,
		argBuilder: &strings.Builder{},
	}
	// hosted call（web_search/code_interpreter/mcp）注册 byToolUseID 关联 result block。
	if kind.tracksToolUseID() {
		switch kind.(type) {
		case webSearchCallKind:
			c.webSearchByToolUseID[ev.ContentBlock.ID] = idx
		case codeInterpreterCallKind:
			c.codeExecutionByToolUseID[ev.ContentBlock.ID] = idx
		case mcpCallKind:
			c.mcpCallByToolUseID[ev.ContentBlock.ID] = idx
		}
	}

	out := []model.SSEEvent{model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
		Type: evOutputItemAdded, SequenceNumber: c.nextSeq(),
		OutputIndex: idx, Item: item,
	})}
	out = append(out, kind.startEvents(c, idx, itemID)...)
	return out
}

// handleCallDelta 处理通用 call block 的 input_json_delta。
// 返回 (events, handled)：handled=false 表示该 block 不在通用流水线（调用方走旧路径）。
func (c *Converter) handleCallDelta(ev *anthropic.MessageStreamEventUnion) (events []model.SSEEvent, handled bool) {
	st, ok := c.callByBlockIdx[int(ev.Index)]
	if !ok {
		return nil, false
	}
	st.argBuilder.WriteString(ev.Delta.PartialJSON)
	return st.kind.consumeDelta(c, st, ev.Delta.PartialJSON), true
}

// handleCallStop 处理通用 call block 的 content_block_stop：finish + output_item.done。
// 返回 (events, handled)：handled=false 表示非通用 call block。
func (c *Converter) handleCallStop(ev *anthropic.MessageStreamEventUnion) (events []model.SSEEvent, handled bool) {
	st, ok := c.callByBlockIdx[int(ev.Index)]
	if !ok {
		return nil, false
	}
	args := st.argBuilder.String()
	item, evts := st.kind.finish(c, st, args)
	if st.itemIdx < len(c.outputItems) {
		c.outputItems[st.itemIdx] = item
	}
	delete(c.callByBlockIdx, int(ev.Index))
	return evts, true
}
