// Package model contains the lightweight wire structs used for OpenAI
// Responses-compatible outbound payloads.
package model

import (
	oairesponses "github.com/openai/openai-go/v3/responses"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

// Protocol wire values are derived from the OpenAI SDK where it exposes a
// canonical constant type.
var (
	ObjectResponse = string(oaconstant.ValueOf[oaconstant.Response]())
	ObjectList     = string(oaconstant.ValueOf[oaconstant.List]())
	ObjectModel    = string(oaconstant.ValueOf[oaconstant.Model]())

	RoleAssistant = string(oaconstant.ValueOf[oaconstant.Assistant]())
	RoleDeveloper = string(oaconstant.ValueOf[oaconstant.Developer]())
	RoleSystem    = string(oaconstant.ValueOf[oaconstant.System]())
	RoleUser      = string(oaconstant.ValueOf[oaconstant.User]())

	ItemTypeMessage            = string(oaconstant.ValueOf[oaconstant.Message]())
	ItemTypeFunctionCall       = string(oaconstant.ValueOf[oaconstant.FunctionCall]())
	ItemTypeFunctionCallOutput = string(oaconstant.ValueOf[oaconstant.FunctionCallOutput]())
	ItemTypeCustomToolCall     = string(oaconstant.ValueOf[oaconstant.CustomToolCall]())
	ItemTypeCustomToolCallOut  = string(oaconstant.ValueOf[oaconstant.CustomToolCallOutput]())
	ItemTypeToolSearchCall     = string(oaconstant.ValueOf[oaconstant.ToolSearchCall]())
	ItemTypeToolSearchOutput   = string(oaconstant.ValueOf[oaconstant.ToolSearchOutput]())
	ItemTypeAdditionalTools    = string(oaconstant.ValueOf[oaconstant.AdditionalTools]())
	ItemTypeCompaction         = string(oaconstant.ValueOf[oaconstant.Compaction]())
	ItemTypeCompactionTrigger  = string(oaconstant.ValueOf[oaconstant.CompactionTrigger]())
	ItemTypeReasoning          = string(oaconstant.ValueOf[oaconstant.Reasoning]())

	ContentTypeOutputText  = string(oaconstant.ValueOf[oaconstant.OutputText]())
	ContentTypeSummaryText = string(oaconstant.ValueOf[oaconstant.SummaryText]())

	StructuredOutputJSONObjectTool = string(oaconstant.ValueOf[oaconstant.JSONObject]())
)

// Response status and request option values mirror the OpenAI Responses SDK
// enums, with local constants for incomplete reasons that are not SDK enums.
const (
	ResponseStatusCompleted  = string(oairesponses.ResponseStatusCompleted)
	ResponseStatusFailed     = string(oairesponses.ResponseStatusFailed)
	ResponseStatusInProgress = string(oairesponses.ResponseStatusInProgress)
	ResponseStatusIncomplete = string(oairesponses.ResponseStatusIncomplete)

	IncompleteReasonMaxOutputTokens = "max_output_tokens"
	IncompleteReasonContentFilter   = "content_filter"
	IncompleteReasonPauseTurn       = "pause_turn"
	IncompleteReasonRefusal         = "refusal"

	ReasoningSummaryConcise = string(oairesponses.ReasoningSummaryConcise)
	ReasoningEffortNone     = string(oairesponses.ReasoningEffortNone)

	AssistantPhaseCommentary  = string(oairesponses.EasyInputMessagePhaseCommentary)
	AssistantPhaseFinalAnswer = string(oairesponses.EasyInputMessagePhaseFinalAnswer)

	ToolChoiceAuto     = string(oairesponses.ToolChoiceOptionsAuto)
	ToolChoiceRequired = string(oairesponses.ToolChoiceOptionsRequired)
	ToolChoiceNone     = string(oairesponses.ToolChoiceOptionsNone)
)
