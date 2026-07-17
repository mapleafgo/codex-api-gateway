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

// CodexModelsResponse 是 Codex 期望的 /v1/models 响应格式（ModelsResponse）。
// Codex 的 SharedModelsManager 用 serde_json::from_slice::<ModelsResponse> 直接解析，
// 期望 { "models": [ModelInfo] }（不是 OpenAI 的 { data: [] }）。
// 若返回 OpenAI 格式，Codex 解析失败/拿到空 ModelInfo（supports_search_tool 默认 false），
// 导致 tool_search / MCP deferred 不工作。
type CodexModelsResponse struct {
	Models []CodexModelInfo `json:"models"`
}

// CodexModelInfo 是 Codex 的 ModelInfo（openai_models.rs）。
// 只含网关能提供的字段；其余字段 Codex serde(default) 补齐。
// 关键字段：SupportsSearchTool=true 让 MCP tools 进 deferred + tool_search 工作。
//
// 字段对齐 codex-rs/protocol/src/openai_models.rs 的 ModelInfo：
// 凡是 Codex 侧未标注 #[serde(default)] 的字段（含 Option<T>），
// 此处的 JSON key 都必须始终出现（omitempty 仅用于 Option 指针的零值省略，
// 但 Codex serde 对 Option<T> 要求 key 存在，可为 null，故用具名字段 + omitempty
// 之外的显式 null 写出，避免 serde_json::from_slice 反序列化失败）。
type CodexModelInfo struct {
	Slug                              string                `json:"slug"`
	DisplayName                       string                `json:"display_name"`
	Description                       string                `json:"description"`
	DefaultReasoningLevel             string                `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels          []any                 `json:"supported_reasoning_levels"`
	ShellType                         string                `json:"shell_type"`
	Visibility                        string                `json:"visibility"`
	SupportedInAPI                    bool                  `json:"supported_in_api"`
	Priority                          int                   `json:"priority"`
	AvailabilityNux                   *any                  `json:"availability_nux"`
	Upgrade                           *any                  `json:"upgrade"`
	BaseInstructions                  string                `json:"base_instructions"`
	SupportsReasoningSummaryParameter bool                  `json:"supports_reasoning_summary_parameter"`
	DefaultReasoningSummary           string                `json:"default_reasoning_summary"`
	SupportVerbosity                  bool                  `json:"support_verbosity"`
	DefaultVerbosity                  *string               `json:"default_verbosity"`
	ApplyPatchToolType                *string               `json:"apply_patch_tool_type"`
	WebSearchToolType                 string                `json:"web_search_tool_type"`
	TruncationPolicy                  CodexTruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls         bool                  `json:"supports_parallel_tool_calls"`
	ExperimentalSupportedTools        []string              `json:"experimental_supported_tools"`
	InputModalities                   []string              `json:"input_modalities"`
	SupportsSearchTool                bool                  `json:"supports_search_tool"`
	UseResponsesLite                  bool                  `json:"use_responses_lite"`
}

// CodexTruncationPolicy 对应 Codex ModelInfo.truncation_policy。
type CodexTruncationPolicy struct {
	Mode  string `json:"mode"` // "tokens" | "bytes"
	Limit int64  `json:"limit"`
}
