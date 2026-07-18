package streamconv

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// lookupHostedCallByResult 按 result block 类型选对应的 byToolUseID map + callKind。
// 返回 (itemIdx, kind)；kind 为 nil 表示该 result block 无关联的 hosted call。
//
// hosted call（web_search/code_interpreter/mcp）在 content_block_start 时注册到
// byToolUseID；其 server_tool_use block 的 content_block_stop 只从 callByBlockIdx 删除，
// byToolUseID map 保留到 result 抵达——真实 SSE 流里 result block 是独立的
// content_block，出现在 server_tool_use stop 之后。tool_result（GLM 方言回传 web search
// 结果）仅在 webSearchByToolUseID 命中时有效，调用方（handleBlockStart）已做该判定。
func (c *Converter) lookupHostedCallByResult(ev *anthropic.MessageStreamEventUnion, toolUseID string) (int, callKind) {
	switch ev.ContentBlock.Type {
	case anBlockWebSearchToolResult, anBlockToolResult:
		idx, ok := c.webSearchByToolUseID[toolUseID]
		if !ok {
			return 0, nil
		}
		return idx, webSearchCallKind{}
	case anBlockCodeExecutionToolResult:
		idx, ok := c.codeExecutionByToolUseID[toolUseID]
		if !ok {
			return 0, nil
		}
		return idx, codeInterpreterCallKind{}
	case anBlockMcpToolResult:
		idx, ok := c.mcpCallByToolUseID[toolUseID]
		if !ok {
			return 0, nil
		}
		return idx, mcpCallKind{}
	default:
		return 0, nil
	}
}

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
	// DEBUG 记录上游发起的每个工具调用（name + 完整 arguments），
	// 便于排查 skill 切片读取、tool_search 误用等模型行为差异。
	slog.Debug("上游工具调用完成",
		"response_id", c.respID,
		"block_index", ev.Index,
		"call_id", st.callID,
		"tool_name", st.name,
		"arguments", args)
	if st.itemIdx < len(c.outputItems) {
		c.outputItems[st.itemIdx] = item
	}
	delete(c.callByBlockIdx, int(ev.Index))
	return evts, true
}

// handleCallResult 处理 result block（web_search_tool_result / code_execution_tool_result /
// mcp_tool_result / GLM 方言 tool_result）：按 block 类型选对应的 byToolUseID map +
// callKind，交给 kind.handleResult 产出 completed/failed + output_item.done。
func (c *Converter) handleCallResult(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	itemIdx, kind := c.lookupHostedCallByResult(ev, ev.ContentBlock.ToolUseID)
	if kind == nil {
		return nil // 无关联的 hosted call
	}
	return kind.handleResult(c, ev, itemIdx)
}
