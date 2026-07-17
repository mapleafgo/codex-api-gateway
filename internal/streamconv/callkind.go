package streamconv

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// callKind 描述一种回程 call（来自 tool_use / server_tool_use / mcp_tool_use block）
// 如何映射为 OpenAI Responses call item 及其事件链。通用流水线（handleCallStart /
// handleCallDelta / handleCallStop / handleCallResult）按策略驱动，消除「每 call 一个
// 特例 handler」——新增 call 类型只改 dispatchCallKind 注册表，不再加 handler。
//
// 差异轴仅 4 个，全部封装在策略里：
//   - itemType / idPrefix
//   - 承载字段（arguments / input / action / code / server_label+name）—— buildItem
//   - arguments 流式（delta.done / 一次性 done 给 / 状态事件 / 无）—— consumeDelta + finish
//   - result 处理（无 / *_tool_result）—— handleResult
type callKind interface {
	itemType() string       // function_call / tool_search_call / web_search_call / ...
	idPrefix() string       // fc / tsc / ws / ci / mcp_
	tracksToolUseID() bool  // hosted call 关联 result block 用（注册 byToolUseID）

	// buildItem 从 content_block_start 构建初始 OutputItem（status=in_progress）。
	buildItem(itemIdx int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem
	// startEvents 在 output_item.added 之后产出 call 事件链（in_progress / searching / ...）。
	startEvents(c *Converter, itemIdx int, itemID string) []model.SSEEvent
	// consumeDelta 处理流式 arguments delta（input_json_delta），产出 delta 事件。
	// 非流式 call（如 tool_search）返回 nil。
	consumeDelta(c *Converter, st *callState, partial string) []model.SSEEvent
	// finish 在 content_block_stop 完成 item（填 arguments/code/output 等）+ 产出
	// done 事件链（arguments.done / completed / ...）。返回更新后的 item 与事件。
	finish(c *Converter, st *callState, args string) (model.OutputItem, []model.SSEEvent)
	// handleResult 处理该 call 的 result block（client call 返回 nil）。
	handleResult(c *Converter, ev *anthropic.MessageStreamEventUnion, itemIdx int) []model.SSEEvent
}

// callState 追踪一个进行中的通用 call block 的状态（content_block_start 到 stop）。
type callState struct {
	kind       callKind
	itemIdx    int
	itemID     string
	callID     string // Anthropic tool_use id（关联 result）
	name       string
	argBuilder *strings.Builder
}

// toolSearchName 是 tool_search 在 Anthropic 侧的 tool name，由 toolcatalog.Declare
// 固定为 "tool_search"（declare.go）。回程 dispatch 按此硬匹配。
const toolSearchName = "tool_search"

// dispatchCallKind 按 content_block 类型 + name 选 callKind。
// 未识别返回 nil：调用方回退到旧 handler（迁移过渡期）或 skip。
//
// 迁移进度：
//   - S2：function 走通用流水线；custom / tool_search 仍回退旧 handleToolUseStart
//   - S3/S4：custom / tool_search 迁入
//   - S5/S6：server_tool_use / mcp_tool_use 迁入
func (c *Converter) dispatchCallKind(ev *anthropic.MessageStreamEventUnion) callKind {
	if ev.ContentBlock.Type == anBlockToolUse {
		name := ev.ContentBlock.Name
		// custom 工具（含 shell/apply_patch，由 customToolNames 标记）与 tool_search
		// 暂仍走旧 handler；其余 tool_use 走通用 function 流水线。
		if !c.customToolNames[name] && name != toolSearchName {
			return functionCallKind{}
		}
	}
	return nil
}
