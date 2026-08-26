package plugin

import (
	"context"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// LegacyBackendTypeToID 把旧版单字符 backend_type 短码映射到插件稳定 ID。
// 过渡期配置仍可能用短码；调度器通过此函数解析到 Registry 查找用的全名。
func LegacyBackendTypeToID(bt string) ID {
	switch bt {
	case config.BackendAnthropic:
		return BackendAnthropic
	case config.BackendOpenAIChat:
		return BackendOpenAIChat
	case config.BackendOpenAIResponses:
		return BackendOpenAIResponses
	case config.BackendGitHubCopilot:
		return BackendGitHubCopilot
	default:
		return ID(bt)
	}
}

// BearerOnlyBackend 是可选接口：支持仅 Authorization: Bearer 认证模式的后端
// 可实现此方法。分发型插件（如 Copilot）在委托时按需检查此接口。
type BearerOnlyBackend interface {
	ExecuteWithAuthorization(
		ctx context.Context,
		rawBody []byte,
		src config.Source,
		cfg *config.Config,
		onEvent func(model.SSEEvent) error,
		onUpstream func(UpstreamEvent),
		attempt int,
	) error
}

// 内置源插件的稳定 ID，进入配置 backend 字段与观测数据。
// 这些常量由 cmd/server 组装时注册，共享核心只按字符串比较。
const (
	BackendAnthropic       = "anthropic"
	BackendOpenAIChat      = "openai-chat"
	BackendOpenAIResponses = "openai-responses"
	BackendGitHubCopilot   = "github-copilot"
)
