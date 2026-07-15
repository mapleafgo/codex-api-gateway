package model

// AnthropicModelsResponse 是 Anthropic 兼容后端 GET /v1/models 的响应格式。
type AnthropicModelsResponse struct {
	Data    []AnthropicModel `json:"data"`
	FirstID string           `json:"first_id"`
	LastID  string           `json:"last_id"`
	HasMore bool             `json:"has_more"`
}

// AnthropicModel 是 Anthropic 模型列表中的单个条目。
type AnthropicModel struct {
	Type        string `json:"type"` // 固定 "model"
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"` // RFC3339
}

// ListResponse 是 OpenAI 兼容的 GET /v1/models 响应格式。
type ListResponse struct {
	Object string  `json:"object"` // 固定 "list"
	Data   []Entry `json:"data"`
}

// Entry 对应 OpenAI Model 对象。
type Entry struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // 固定 "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}
