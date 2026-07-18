package model

import "encoding/json"

// ResponseObject is the `response` object embedded in created/in_progress/
// completed/incomplete/failed events. Fields use omitempty so we emit exactly
// the P2 fields we can populate, without SDK Response's 35+ zero fields.
type ResponseObject struct {
	ID                 string             `json:"id"`
	Object             string             `json:"object"` // always "response"
	Status             string             `json:"status"`
	Model              string             `json:"model"`
	CreatedAt          int64              `json:"created_at"`
	CompletedAt        int64              `json:"completed_at,omitempty"`
	Output             []OutputItem       `json:"output"`
	Usage              *ResponseUsage     `json:"usage,omitempty"`
	IncompleteDetails  *IncompleteDetails `json:"incomplete_details,omitempty"`
	Error              *ResponseError     `json:"error,omitempty"`
	Instructions       string             `json:"instructions,omitempty"`
	Temperature        *float64           `json:"temperature,omitempty"`
	TopP               *float64           `json:"top_p,omitempty"`
	MaxOutputTokens    *int64             `json:"max_output_tokens,omitempty"`
	ToolChoice         any                `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool              `json:"parallel_tool_calls,omitempty"`
	Reasoning          *ReasoningEcho     `json:"reasoning,omitempty"`
	Truncation         string             `json:"truncation,omitempty"`
	Store              *bool              `json:"store,omitempty"`
}

// MarshalJSON 保证 incomplete 响应始终包含 required 的 incomplete_details 字段。
func (r ResponseObject) MarshalJSON() ([]byte, error) {
	if r.Status != ResponseStatusIncomplete {
		type responseObject ResponseObject
		return json.Marshal(responseObject(r))
	}
	type responseObject ResponseObject
	return json.Marshal(struct {
		responseObject
		IncompleteDetails *IncompleteDetails `json:"incomplete_details"`
	}{
		responseObject:    responseObject(r),
		IncompleteDetails: r.IncompleteDetails,
	})
}

// ResponseError is the error detail embedded in a failed ResponseObject.
type ResponseError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// ResponseUsage reports token usage on terminal response events.
type ResponseUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	TotalTokens              int `json:"total_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// IncompleteDetails describes why a response ended incomplete.
type IncompleteDetails struct {
	Reason string `json:"reason"` // max_output_tokens | content_filter
}

// ResponseObjectParams carries request echo fields into NewResponseObject.
type ResponseObjectParams struct {
	Instructions       string
	Temperature        *float64
	TopP               *float64
	MaxOutputTokens    *int64
	ToolChoice         any
	ParallelToolCalls  *bool
	Reasoning          *ReasoningEcho
	Truncation         string
	Store              *bool
}

// ReasoningEcho echoes the request's reasoning configuration back in the
// response object. Uses omitempty so empty fields are omitted.
type ReasoningEcho struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// NewResponseObject builds a ResponseObject with echoed request fields.
func NewResponseObject(id, status, modelName string, createdAt int64, p ResponseObjectParams) ResponseObject {
	return ResponseObject{
		ID: id, Object: ObjectResponse, Status: status, Model: modelName, CreatedAt: createdAt,
		Output:             []OutputItem{},
		Instructions:       p.Instructions,
		Temperature:        p.Temperature,
		TopP:               p.TopP,
		MaxOutputTokens:    p.MaxOutputTokens,
		ToolChoice:         p.ToolChoice,
		ParallelToolCalls:  p.ParallelToolCalls,
		Reasoning:          p.Reasoning,
		Truncation:         p.Truncation,
		Store:              p.Store,
	}
}
