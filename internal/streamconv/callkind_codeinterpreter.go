package streamconv

import (
	"fmt"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// codeInterpreterCallKind 把 Anthropic server_tool_use(code_execution) 映射为
// code_interpreter_call。
//
// 事件链：output_item.added(code_interpreter_call, in_progress) → in_progress
// → interpreting → code.delta/done（if code）。block stop 不产 done（在 result block
// 完成）。result（code_execution_tool_result）由 handleCallResult +
// codeInterpreterCallKind.handleResult 驱动完成（completed + outputs(logs)）。
// container_id 由网关合成（Anthropic 无 container，已知损失）。
type codeInterpreterCallKind struct{}

func (codeInterpreterCallKind) itemType() string      { return model.ItemTypeCodeInterpreterCall }
func (codeInterpreterCallKind) idPrefix() string      { return "ci" }
func (codeInterpreterCallKind) tracksToolUseID() bool { return true }

func (codeInterpreterCallKind) buildItem(itemIdx int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem {
	return model.OutputItem{
		Type:        model.ItemTypeCodeInterpreterCall,
		ID:          itemID,
		Status:      model.ResponseStatusInProgress,
		ContainerID: fmt.Sprintf("ci_container_%d", itemIdx),
		Code:        extractCodeExecutionCode(ev.ContentBlock.Input),
		Outputs:     []model.CodeInterpreterOutput{},
	}
}

func (codeInterpreterCallKind) startEvents(c *Converter, itemIdx int, itemID string) []model.SSEEvent {
	code := c.outputItems[itemIdx].Code
	out := []model.SSEEvent{
		model.MarshalEvent(evCodeInterpreterCallInProgress, model.CodeInterpreterCallEvent{
			Type: evCodeInterpreterCallInProgress, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, ItemID: itemID,
		}),
		model.MarshalEvent(evCodeInterpreterCallInterpreting, model.CodeInterpreterCallEvent{
			Type: evCodeInterpreterCallInterpreting, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, ItemID: itemID,
		}),
	}
	if code != "" {
		out = append(out,
			model.MarshalEvent(evCodeInterpreterCallCodeDelta, model.CodeInterpreterCallCodeDeltaEvent{
				Type: evCodeInterpreterCallCodeDelta, SequenceNumber: c.nextSeq(),
				OutputIndex: itemIdx, ItemID: itemID, Delta: code,
			}),
			model.MarshalEvent(evCodeInterpreterCallCodeDone, model.CodeInterpreterCallCodeDoneEvent{
				Type: evCodeInterpreterCallCodeDone, SequenceNumber: c.nextSeq(),
				OutputIndex: itemIdx, ItemID: itemID, Code: code,
			}),
		)
	}
	return out
}

func (codeInterpreterCallKind) consumeDelta(c *Converter, st *callState, partial string) []model.SSEEvent {
	return nil
}

func (codeInterpreterCallKind) finish(c *Converter, st *callState, args string) (model.OutputItem, []model.SSEEvent) {
	// code_interpreter 在 result block（code_execution_tool_result）完成。
	return c.outputItems[st.itemIdx], nil
}

func (codeInterpreterCallKind) handleResult(c *Converter, ev *anthropic.MessageStreamEventUnion, itemIdx int) []model.SSEEvent {
	if itemIdx >= len(c.outputItems) {
		return nil
	}
	itemID := fmt.Sprintf("ci_%d", itemIdx)
	rc := ev.ContentBlock.Content
	logs := foldExecutionLogs(rc.Stdout, rc.Stderr)
	c.outputItems[itemIdx].Status = model.ResponseStatusCompleted
	if logs != "" {
		c.outputItems[itemIdx].Outputs = []model.CodeInterpreterOutput{{Type: "logs", Logs: logs}}
	}
	for _, out := range rc.Content.OfContent {
		if out.FileID != "" {
			slog.Warn("丢弃 code execution 生成的文件（无 OpenAI files url 凭据）",
				"response_id", c.respID, "file_id", out.FileID)
		}
	}
	return []model.SSEEvent{
		model.MarshalEvent(evCodeInterpreterCallCompleted, model.CodeInterpreterCallEvent{
			Type: evCodeInterpreterCallCompleted, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, ItemID: itemID,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: itemIdx, Item: c.outputItems[itemIdx],
		}),
	}
}
