package copilot

import (
	"context"

	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	copilotpkg "github.com/mapleafgo/codex-api-gateway/internal/copilot"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
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
		Schema: []plugin.Field{
			{
				Name: "github_token", Label: "GitHub Token", Type: plugin.FieldTypePassword,
				Required: true, Sensitive: true, Target: plugin.FieldTargetOption,
				Description: "GitHub OAuth token used to authenticate against the Copilot API",
			},
		},
	}
}

// ValidateSource 校验 Copilot 源配置：必须携带 github_token。
func (p *Plugin) ValidateSource(src config.Source) error {
	if copilotToken(src) == "" {
		return plugin.ErrMissingGithubToken
	}
	return nil
}

// Backend 返回协议适配后端。
func (p *Plugin) Backend() plugin.Backend { return normalizeBackend{p.b} }

// ListModels 拉取 Copilot 筛选后的模型目录。
func (p *Plugin) ListModels(ctx context.Context, src config.Source) ([]plugin.Model, error) {
	ms, err := p.b.ListModels(ctx, withCopilotToken(src))
	if err != nil {
		return nil, err
	}
	out := make([]plugin.Model, 0, len(ms))
	for _, m := range ms {
		out = append(out, plugin.Model{ID: m.ID})
	}
	return out, nil
}

// copilotToken 返回配置中的 GitHub token：优先 options.github_token（Config v2），
// 兜底旧的 github_token 顶层字段。
func copilotToken(src config.Source) string {
	if t, _ := src.Options["github_token"].(string); t != "" {
		return t
	}
	return src.GithubToken
}

// withCopilotToken 把 options.github_token 归一化到 src.GithubToken，供内部委托。
func withCopilotToken(src config.Source) config.Source {
	if t, _ := src.Options["github_token"].(string); t != "" {
		src.GithubToken = t
	}
	return src
}

// normalizeBackend 是归一化委托层：执行前把 options.github_token 填进 src，
// 使被委托的 r/a/c 后端只认 src.GithubToken，插件边界内不影响共享核心。
type normalizeBackend struct {
	inner plugin.Backend
}

func (n normalizeBackend) Execute(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	cfg *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(plugin.UpstreamEvent),
	attempt int,
) error {
	return n.inner.Execute(ctx, rawBody, withCopilotToken(src), cfg, onEvent, onUpstream, attempt)
}

var _ plugin.Backend = normalizeBackend{}
