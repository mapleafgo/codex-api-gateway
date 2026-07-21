package convert

import (
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// EchoFromRequest 从 Responses 请求提取应回显到 response 对象的 P2 字段。
// streamconv / chatstreamconv 通过 SetEcho 注入，保证 created/completed 等事件
// 带回 instructions、sampling、tool_choice、reasoning 等客户端可见字段。
func EchoFromRequest(req *oairesponses.ResponseNewParams) model.ResponseObjectParams {
	if req == nil {
		return model.ResponseObjectParams{}
	}
	p := model.ResponseObjectParams{
		Instructions: req.Instructions.Value,
		Truncation:   string(req.Truncation),
	}
	if req.Temperature.Valid() {
		v := req.Temperature.Value
		p.Temperature = &v
	}
	if req.TopP.Valid() {
		v := req.TopP.Value
		p.TopP = &v
	}
	if req.MaxOutputTokens.Valid() {
		v := req.MaxOutputTokens.Value
		p.MaxOutputTokens = &v
	}
	if req.ParallelToolCalls.Valid() {
		v := req.ParallelToolCalls.Value
		p.ParallelToolCalls = &v
	}
	if req.Store.Valid() {
		v := req.Store.Value
		p.Store = &v
	}
	// Echo tool_choice if any variant is set.
	if req.ToolChoice.OfToolChoiceMode.Valid() ||
		req.ToolChoice.OfAllowedTools != nil ||
		req.ToolChoice.OfFunctionTool != nil ||
		req.ToolChoice.OfHostedTool != nil ||
		req.ToolChoice.OfMcpTool != nil ||
		req.ToolChoice.OfCustomTool != nil ||
		req.ToolChoice.OfSpecificApplyPatchToolChoice != nil ||
		req.ToolChoice.OfSpecificShellToolChoice != nil {
		p.ToolChoice = req.ToolChoice
	}
	if req.Reasoning.Effort != "" || req.Reasoning.Summary != "" {
		p.Reasoning = &model.ReasoningEcho{
			Effort:  string(req.Reasoning.Effort),
			Summary: string(req.Reasoning.Summary),
		}
	}
	return p
}
