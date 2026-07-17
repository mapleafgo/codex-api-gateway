package streamconv

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// mcpCallKind 把 probe 合成的 mcp_tool_use 映射为 mcp_call。
//
// arguments 在 start 时一次性给出（probe 合成的 Input 含完整 args，不流式 delta）：
// output_item.added(mcp_call, in_progress) → mcp_call.in_progress →
// mcp_call_arguments.delta/done（if args）。block stop 不产 done（在 result 完成）。
// result（mcp_tool_result）由 handleMcpToolResultStart 处理（S7 迁入 handleResult）。
type mcpCallKind struct{}

func (mcpCallKind) itemType() string     { return model.ItemTypeMcpCall }
func (mcpCallKind) idPrefix() string      { return "mcp" }
func (mcpCallKind) tracksToolUseID() bool { return true }

func (mcpCallKind) buildItem(itemIdx int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem {
	serverLabel, toolName, args := decodeMcpUseInput(ev.ContentBlock.Input)
	return model.OutputItem{
		Type:        model.ItemTypeMcpCall,
		ID:          itemID,
		Status:      model.ResponseStatusInProgress,
		ServerLabel: serverLabel,
		Name:        toolName,
		Arguments:   args,
	}
}

func (mcpCallKind) startEvents(c *Converter, itemIdx int, itemID string) []model.SSEEvent {
	out := []model.SSEEvent{
		model.MarshalEvent(evMcpCallInProgress, model.McpCallEvent{
			Type: evMcpCallInProgress, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, ItemID: itemID,
		}),
	}
	if args := c.outputItems[itemIdx].Arguments; args != "" {
		out = append(out,
			model.MarshalEvent(evMcpCallArgumentsDelta, model.McpCallArgumentsDeltaEvent{
				Type: evMcpCallArgumentsDelta, SequenceNumber: c.nextSeq(),
				OutputIndex: itemIdx, ItemID: itemID, Delta: args,
			}),
			model.MarshalEvent(evMcpCallArgumentsDone, model.McpCallArgumentsDoneEvent{
				Type: evMcpCallArgumentsDone, SequenceNumber: c.nextSeq(),
				OutputIndex: itemIdx, ItemID: itemID, Arguments: args,
			}),
		)
	}
	return out
}

func (mcpCallKind) consumeDelta(c *Converter, st *callState, partial string) []model.SSEEvent {
	return nil // args 在 start 一次性给（probe 合成 Input），不流式 delta
}

func (mcpCallKind) finish(c *Converter, st *callState, args string) (model.OutputItem, []model.SSEEvent) {
	// mcp_call 的 arguments.delta/done 已在 startEvents 产出，block stop 无事件。
	return c.outputItems[st.itemIdx], nil
}

func (mcpCallKind) handleResult(c *Converter, ev *anthropic.MessageStreamEventUnion, itemIdx int) []model.SSEEvent {
	return nil // S7 迁入
}
