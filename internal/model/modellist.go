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

// CodexModelInfo 对齐 codex-rs/protocol/src/openai_models.rs 的 ModelInfo
// （对齐 0.144.5，共 37 个外部可反序列化字段；used_fallback_model_metadata
// 被 skip_deserializing 忽略）。
//
// serde 约束（决定 JSON key 是否必须出现）：
//   - 无 #[serde(default)] 的字段（含 Option<T>）：key 必须出现，可为 null。
//     → Go 侧用具名字段 + 指针类型 + 必填值，不用 omitempty。
//   - 有 #[serde(default)] 的字段：Codex 自动补零值，可省略。
//     → Go 侧加 omitempty，零值不出现在 JSON 中。
//
// 关键字段：SupportsSearchTool=true 让 MCP tools 进 deferred + tool_search 工作。
// SupportsReasoningSummaries（0.144.5）/ SupportsReasoningSummaryParameter（main）
// 双写以同时兼容两个版本：旧版把它当必填字段，新版 serde(default) 忽略多余 key。
type CodexModelInfo struct {
	// —— 必填字段（无 serde(default)）——
	Slug                       string                `json:"slug"`
	DisplayName                string                `json:"display_name"`
	Description                *string               `json:"description"`
	SupportedReasoningLevels   []any                 `json:"supported_reasoning_levels"`
	ShellType                  string                `json:"shell_type"`
	Visibility                 string                `json:"visibility"`
	SupportedInAPI             bool                  `json:"supported_in_api"`
	Priority                   int                   `json:"priority"`
	AvailabilityNux            *any                  `json:"availability_nux"`
	Upgrade                    *any                  `json:"upgrade"`
	BaseInstructions           string                `json:"base_instructions"`
	SupportsReasoningSummaries bool                  `json:"supports_reasoning_summaries"`
	SupportVerbosity           bool                  `json:"support_verbosity"`
	DefaultVerbosity           *string               `json:"default_verbosity"`
	ApplyPatchToolType         *string               `json:"apply_patch_tool_type"`
	TruncationPolicy           CodexTruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls  bool                  `json:"supports_parallel_tool_calls"`
	ExperimentalSupportedTools []string              `json:"experimental_supported_tools"`

	// —— 可选字段（有 serde(default)），零值省略 ——
	DefaultReasoningLevel          *string  `json:"default_reasoning_level,omitempty"`
	AdditionalSpeedTiers           []string `json:"additional_speed_tiers,omitempty"`
	ServiceTiers                   []any    `json:"service_tiers,omitempty"`
	DefaultServiceTier             *string  `json:"default_service_tier,omitempty"`
	ModelMessages                  *any     `json:"model_messages,omitempty"`
	IncludeSkillsUsageInstructions bool     `json:"include_skills_usage_instructions,omitempty"`
	DefaultReasoningSummary        string   `json:"default_reasoning_summary,omitempty"`
	WebSearchToolType              string   `json:"web_search_tool_type,omitempty"`
	SupportsImageDetailOriginal    bool     `json:"supports_image_detail_original,omitempty"`
	ContextWindow                  *int64   `json:"context_window,omitempty"`
	MaxContextWindow               *int64   `json:"max_context_window,omitempty"`
	AutoCompactTokenLimit          *int64   `json:"auto_compact_token_limit,omitempty"`
	CompHash                       *string  `json:"comp_hash,omitempty"`
	EffectiveContextWindowPercent  int64    `json:"effective_context_window_percent,omitempty"`
	InputModalities                []string `json:"input_modalities,omitempty"`
	SupportsSearchTool             bool     `json:"supports_search_tool,omitempty"`
	UseResponsesLite               bool     `json:"use_responses_lite,omitempty"`
	AutoReviewModelOverride        *string  `json:"auto_review_model_override,omitempty"`
	ToolMode                       *string  `json:"tool_mode,omitempty"`
	MultiAgentVersion              *string  `json:"multi_agent_version,omitempty"`

	// —— 兼容 main 分支：main 把必填字段重命名为 supports_reasoning_summary_parameter ——
	// 0.144.5 serde 不带 deny_unknown_fields，多余 key 会被静默忽略。
	SupportsReasoningSummaryParameter bool `json:"supports_reasoning_summary_parameter,omitempty"`
}

// CodexTruncationPolicy 对应 Codex ModelInfo.truncation_policy。
type CodexTruncationPolicy struct {
	Mode  string `json:"mode"` // "tokens" | "bytes"
	Limit int64  `json:"limit"`
}
