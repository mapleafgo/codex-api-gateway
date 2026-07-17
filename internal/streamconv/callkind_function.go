package streamconv

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// functionCallKind 把 tool_use（非 custom、非 tool_search）映射为 function_call。
//
// 事件链：output_item.added(function_call, in_progress) → function_call.arguments.delta
// （每段 input_json_delta）→ function_call.arguments.done（完整 args）→ output_item.done。
// 无 result block（client 执行，结果经 function_call_output 下轮回灌）。
type functionCallKind struct{}

func (functionCallKind) itemType() string      { return model.ItemTypeFunctionCall }
func (functionCallKind) idPrefix() string      { return "fc" }
func (functionCallKind) tracksToolUseID() bool { return false }

func (functionCallKind) buildItem(itemIdx int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem {
	// Codex 的 ToolName 用 namespace + name 两字段（ToolName::new(namespace, name)）。
	// tool_search 发现的 MCP 工具名是 flat "mcp__server__tool"（declare 的 ToolName 拼接）。
	// 若整个塞 name（namespace 空），Codex 构造 ToolName{None, "mcp__server__tool"}，
	// 但 registry 注册的是 ToolName{Some("mcp__server"), "tool"} → 不匹配 → unsupported call。
	// 故按最后一个 "__" 拆成 namespace + name，对齐 Codex registry。
	ns, name := splitToolNameNamespace(ev.ContentBlock.Name)
	return model.OutputItem{
		Type:      model.ItemTypeFunctionCall,
		ID:        itemID,
		Status:    model.ResponseStatusInProgress,
		CallID:    ev.ContentBlock.ID,
		Name:      name,
		Namespace: ns,
	}
}

// splitToolNameNamespace 把 declare 拼接的 flat name（ns__tool）按最后一个 "__"
// 拆成 namespace + name。declare 的 ToolName 用 "__" 拼接（ns + "__" + name），
// ns 本身可能含 "__"（如 "mcp__fetch"），故用 LastIndex 反向拆。
// 无 "__" 的 name 返回空 namespace（普通 function）。
func splitToolNameNamespace(flat string) (namespace, name string) {
	idx := strings.LastIndex(flat, "__")
	if idx <= 0 {
		return "", flat
	}
	return flat[:idx], flat[idx+2:]
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
