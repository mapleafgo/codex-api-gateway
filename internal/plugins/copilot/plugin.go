package copilot

import (
	"context"

	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	copilotpkg "github.com/mapleafgo/codex-api-gateway/internal/copilot"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// Plugin 是 GitHub Copilot 源插件：endpoint 发现、模型目录、Device Flow 认证
// 与按模型能力的 r/a/c 协议路由全部归属 internal/copilot 单一归属包。
type Plugin struct {
	b *copilotpkg.Backend
}

// New 构造 Copilot 源插件，组合已有的三个 Backend 做委托。
func New() *Plugin {
	return &Plugin{
		b: copilotpkg.NewBackend(backend.NewResponses(), backend.NewAnthropic(), backend.NewChat()),
	}
}

// Descriptor 返回插件的唯一身份与能力声明。
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:           plugin.BackendGitHubCopilot,
		Title:        "GitHub Copilot",
		Summary:      "通过 GitHub Copilot API 按模型能力委托 Responses / Messages / Chat 协议",
		Capabilities: []plugin.Capability{plugin.CapabilityResponsesPassthrough, plugin.CapabilityAnthropicMessages, plugin.CapabilityChatCompletions},
		Streaming:    plugin.StreamingConverted,
	}
}

// ValidateSource 校验 Copilot 源配置：必须携带 github_token。
func (p *Plugin) ValidateSource(src config.Source) error {
	if src.GithubToken == "" {
		return plugin.ErrMissingGithubToken
	}
	return nil
}

// Backend 返回协议适配后端。
func (p *Plugin) Backend() plugin.Backend { return p.b }

// ListModels 拉取 Copilot 筛选后的模型目录。
func (p *Plugin) ListModels(ctx context.Context, src config.Source) ([]plugin.Model, error) {
	ms, err := p.b.ListModels(ctx, src)
	if err != nil {
		return nil, err
	}
	out := make([]plugin.Model, 0, len(ms))
	for _, m := range ms {
		out = append(out, plugin.Model{ID: m.ID})
	}
	return out, nil
}
