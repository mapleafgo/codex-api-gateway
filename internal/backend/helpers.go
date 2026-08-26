package backend

import (
	"log/slog"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
	oairesponses "github.com/openai/openai-go/v3/responses"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

// 事件 wire 常量在 internal/plugin 中私有，历史 backend 包仍直接引用，
// 这里保留本地副本；T062 删除 backend 时一并收敛。
var (
	evResponseCreated    = string(oaconstant.ValueOf[oaconstant.ResponseCreated]())
	evResponseInProgress = string(oaconstant.ValueOf[oaconstant.ResponseInProgress]())
	evResponseCompleted  = string(oaconstant.ValueOf[oaconstant.ResponseCompleted]())
	evResponseIncomplete = string(oaconstant.ValueOf[oaconstant.ResponseIncomplete]())
	evResponseFailed     = string(oaconstant.ValueOf[oaconstant.ResponseFailed]())
	evOutputItemAdded    = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemAdded]())
	evOutputItemDone     = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemDone]())
)

// 通用上游结果/事件门控/错误分类 helper 已迁移到 internal/plugin。
// 这里保留别名以兼容历史调用方，最终在 T062 收敛后移除。
var (
	ErrEmptyResponse   = plugin.ErrEmptyResponse
	ErrUpstreamTimeout = plugin.ErrUpstreamTimeout
)

// EventGate 是插件契约类型的别名，兼容历史调用方。
type EventGate = plugin.EventGate

// OutcomeInput 是插件契约类型的别名，兼容历史调用方。
// outcomeInput 是未导出别名，供后端包内部使用。
type OutcomeInput = plugin.OutcomeInput

type outcomeInput = plugin.OutcomeInput

const maxBufferedEvents = plugin.MaxBufferedEvents

// 函数与错误别名，兼容历史调用方。
var (
	NewEventGate              = plugin.NewEventGate
	ClassifyOutcome           = plugin.ClassifyOutcome
	classifyOutcome           = plugin.ClassifyOutcome
	IsClientCanceled          = plugin.IsClientCanceled
	IsServerTimeout           = plugin.IsServerTimeout
	StatusCodeFromErr         = plugin.StatusCodeFromErr
	IsClientError             = plugin.IsClientError
	ContextLengthExceededCode = plugin.ContextLengthExceededCode
	errSummary                = plugin.ErrSummary
)

// stripWebSearchToolsFromParams 从 Responses 请求的工具列表里剥掉 hosted web_search 声明。
func stripWebSearchToolsFromParams(req *oairesponses.ResponseNewParams, log *slog.Logger) {
	if req == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	if len(req.Tools) > 0 {
		out := make([]oairesponses.ToolUnionParam, 0, len(req.Tools))
		removed := 0
		for _, t := range req.Tools {
			if t.OfWebSearch != nil {
				removed++
				continue
			}
			out = append(out, t)
		}
		if removed > 0 {
			req.Tools = out
			log.Debug("backend: 源不支持 hosted web_search，剥掉工具声明", "removed", removed)
		}
	}
	if neutralizeWebSearchToolChoice(&req.ToolChoice) {
		log.Debug("backend: 源不支持 hosted web_search，清理 tool_choice/allowed_tools")
	}
}

// isWebSearchTypeString 判断 tool / tool_choice 的 wire 字符串是否属于 hosted web_search。
func isWebSearchTypeString(t string) bool {
	return t == model.ToolTypeWebSearch ||
		t == string(oairesponses.WebSearchToolTypeWebSearch2025_08_26)
}

// neutralizeWebSearchToolChoice 移除 tool_choice 中对 hosted web_search 的引用。
func neutralizeWebSearchToolChoice(tc *oairesponses.ResponseNewParamsToolChoiceUnion) bool {
	if tc == nil {
		return false
	}
	if hosted := tc.OfHostedTool; hosted != nil && isWebSearchTypeString(string(hosted.Type)) {
		*tc = oairesponses.ResponseNewParamsToolChoiceUnion{}
		return true
	}
	if allowed := tc.OfAllowedTools; allowed != nil {
		out := make([]map[string]any, 0, len(allowed.Tools))
		for _, entry := range allowed.Tools {
			typ, _ := entry["type"].(string)
			if isWebSearchTypeString(typ) {
				continue
			}
			out = append(out, entry)
		}
		if len(out) == len(allowed.Tools) {
			return false
		}
		if len(out) == 0 {
			*tc = oairesponses.ResponseNewParamsToolChoiceUnion{}
			return true
		}
		allowed.Tools = out
		return true
	}
	return false
}
