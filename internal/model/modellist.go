package model

// CodexModelsResponse 是 Codex 期望的 /v1/models 响应格式（ModelsResponse）。
// Codex 的 SharedModelsManager 用 serde_json::from_slice::<ModelsResponse> 直接解析，
// 期望 { "models": [ModelInfo] }（不是 OpenAI 的 { data: [] }）。
// 若返回 OpenAI 格式，Codex 解析失败/拿到空 ModelInfo（supports_search_tool 默认 false），
// 导致 tool_search / MCP deferred 不工作。
type CodexModelsResponse struct {
	Models []CodexModelInfo `json:"models"`
}

// CodexModelInfo 对齐 codex-rs/protocol/src/openai_models.rs 的 ModelInfo（0.144.5）。
// 字段语义与 serde 约束（来自源码逐项确认）：
//
// 必填字段（无 #[serde(default)]，JSON key 必须出现）：
//   - slug: 模型 slug（客户端请求时使用的名字）
//   - display_name: 展示名（UI/日志）
//   - description: 模型描述（Option<String>，可为 null）
//   - supported_reasoning_levels: 支持的 reasoning effort 预设列表，元素 {effort,description}；
//     空 [] 合法但表示该模型不支持任何 reasoning 预设
//   - shell_type: shell 执行类型，取值 default/local/unified_exec/disabled/shell_command
//   - visibility: 模型在 picker 的可见性，取值 list/hide/none
//   - supported_in_api: 是否在 API 可用
//   - priority: 排序优先级（数字越小越靠前）
//   - availability_nux / upgrade: Option，null 即可
//   - base_instructions: 基础 system prompt（**非空时会整体替换 Codex 内置 BASE_INSTRUCTIONS**，
//     网关保持空串，命令补丁走 system_suffix 追加而非替换）
//   - supports_reasoning_summaries: 是否接受 Responses API reasoning.summary 参数
//   - support_verbosity / default_verbosity: verbosity 开关与默认值
//   - apply_patch_tool_type: Option<ApplyPatchToolType>，"freeform" 启用 apply_patch 工具，
//     null 则不注册 apply_patch（模型只能靠 shell 绕路改文件）
//   - truncation_policy: 工具输出截断策略 {mode: tokens|bytes, limit}
//   - supports_parallel_tool_calls: 是否支持并行工具调用
//   - experimental_supported_tools: 实验性工具名列表
//
// 可选字段（有 #[serde(default)]，零值省略；网关只在必要时给值）：
//   - context_window / max_context_window: 上下文窗口大小（必须告知，Codex 默认 None）
//   - auto_compact_token_limit: 自动压缩阈值
//   - supports_search_tool: 启用 tool_search + MCP deferred（网关核心，必须 true）
//   - supports_image_detail_original: 是否支持图片识别（原尺寸 detail），默认 false
//   - include_skills_usage_instructions: 注入 skills 使用说明块，默认 true（启用 skill 发现引导）
//   - web_search_tool_type: web search 能力类型 text|text_and_image（仅声明，不自动注册工具）
//   - input_modalities: 支持的输入模态 ["text","image"]
//   - effective_context_window_percent: 输入 token 占窗口百分比
//   - default_reasoning_summary: 默认 reasoning summary auto|none|detailed|concise（省略=auto）
//   - use_responses_lite: Responses Lite 协议路径（*bool，硬编码 false）。该模式是
//     Codex→OpenAI 后端的内部传输优化：启用后 Codex 把工具从顶层 tools 挪进 input 的
//     additional_tools item、清空 instructions、注入 responses-lite header，且不支持
//     namespace 工具。第三方上游非 OpenAI 后端启用有害无益，codexModelInfo 硬编码显式
//     false 压制 lite（防 Codex hardcode/默认开启），不开放 per-slug 覆盖。
//
// 不注入（保持零值）：
//   - base_instructions 非空（避免覆盖 Codex 内置指令）
//   - model_messages（personality 模板，与 system_suffix 冲突）
//   - default_reasoning_level / service_tiers / default_service_tier 等（用 Codex 默认）
//
// used_fallback_model_metadata 是 Codex 内部标记（#[skip_deserializing]），
// 网关设值也会被忽略，故不暴露。
type CodexModelInfo struct {
	// —— 必填字段（无 serde(default)）——
	Slug                       string                `json:"slug"`
	DisplayName                string                `json:"display_name"`
	Description                *string               `json:"description"`
	SupportedReasoningLevels   []CodexReasoningLevel `json:"supported_reasoning_levels"`
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
	UseResponsesLite               *bool    `json:"use_responses_lite,omitempty"`
	AutoReviewModelOverride        *string  `json:"auto_review_model_override,omitempty"`
	ToolMode                       *string  `json:"tool_mode,omitempty"`
	MultiAgentVersion              *string  `json:"multi_agent_version,omitempty"`
}

// CodexReasoningLevel 对应 supported_reasoning_levels 数组元素。
// effort 取值 none/low/medium/high/xhigh；description 为可选展示文案。
type CodexReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description,omitempty"`
}

// CodexTruncationPolicy 对应 Codex ModelInfo.truncation_policy。
type CodexTruncationPolicy struct {
	Mode  string `json:"mode"` // "tokens" | "bytes"
	Limit int64  `json:"limit"`
}
