package plugin

// 内置源插件的稳定 ID，进入配置 backend 字段与观测数据。
// 这些常量由 cmd/server 组装时注册，共享核心只按字符串比较。
const (
	BackendAnthropic       = "anthropic"
	BackendOpenAIChat      = "openai-chat"
	BackendOpenAIResponses = "openai-responses"
	BackendGitHubCopilot   = "github-copilot"
)
