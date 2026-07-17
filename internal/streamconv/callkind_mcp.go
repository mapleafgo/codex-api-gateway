package streamconv

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// mcpCallKind 把 probe 合成的 mcp_tool_use 映射为 mcp_call。
//
// arguments 在 start 时一次性给出（probe 合成的 Input 含完整 args，不流式 delta）：
// output_item.added(mcp_call, in_progress) → mcp_call.in_progress →
// mcp_call_arguments.delta/done（if args）。block stop 不产 done（在 result 完成）。
// result（mcp_tool_result）由 handleCallResult + mcpCallKind.handleResult 驱动完成
// （completed/failed + output）。
type mcpCallKind struct{}

func (mcpCallKind) itemType() string      { return model.ItemTypeMcpCall }
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
	if itemIdx >= len(c.outputItems) {
		return nil
	}
	itemID := fmt.Sprintf("mcp_%d", itemIdx)
	output, isError := decodeMcpResultInput(ev.ContentBlock.Input)
	c.outputItems[itemIdx].Output = output
	if isError {
		c.outputItems[itemIdx].Status = model.ResponseStatusFailed
		return []model.SSEEvent{
			model.MarshalEvent(evMcpCallFailed, model.McpCallEvent{
				Type: evMcpCallFailed, SequenceNumber: c.nextSeq(),
				OutputIndex: itemIdx, ItemID: itemID,
			}),
			model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
				Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
				OutputIndex: itemIdx, Item: c.outputItems[itemIdx],
			}),
		}
	}
	c.outputItems[itemIdx].Status = model.ResponseStatusCompleted
	return []model.SSEEvent{
		model.MarshalEvent(evMcpCallCompleted, model.McpCallEvent{
			Type: evMcpCallCompleted, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, ItemID: itemID,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, Item: c.outputItems[itemIdx],
		}),
	}
}

// decodeMcpUseInput 从 probe 合成的 mcp_tool_use Input 中取出 server_name/name/arguments。
// Input 由 synthesizeMCPEvent 编码为 {server_name, name, arguments}。
func decodeMcpUseInput(input any) (serverLabel, name, args string) {
	m, ok := input.(map[string]any)
	if !ok {
		return "", "", ""
	}
	if v, ok := m["server_name"].(string); ok {
		serverLabel = v
	}
	if v, ok := m["name"].(string); ok {
		name = v
	}
	if v, ok := m["arguments"].(string); ok {
		args = v
	}
	return
}

// decodeMcpResultInput 从 mcp_tool_result 的合成 Input map 中取出
// output 文本与 is_error 标志。
// 类型断言失败时返回空值（synthesizeMCPEvent 保证输入为 map[string]any）。
func decodeMcpResultInput(input any) (output string, isError bool) {
	m, ok := input.(map[string]any)
	if !ok {
		return "", false
	}
	if v, ok := m["output"].(string); ok {
		output = v
	}
	if v, ok := m["is_error"].(bool); ok {
		isError = v
	}
	return
}
