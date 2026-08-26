package anthropic

import (
	"context"

	anthropicclient "github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// Plugin 是 Anthropic Messages 源插件：Responses → Anthropic Messages 转换与
// 流式回写，附带 /v1/models 目录能力。专属配置后续通过 options 承载。
type Plugin struct {
	b *backend.AnthropicBackend
	c *anthropicclient.Client
}

// New 构造 Anthropic 源插件。
func New() *Plugin {
	return &Plugin{b: backend.NewAnthropic(), c: anthropicclient.New()}
}

// Descriptor 返回插件的唯一身份与能力声明。
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:           plugin.BackendAnthropic,
		Title:        "Anthropic Messages",
		Summary:      "将 Responses 请求转换为 Anthropic Messages 流式协议",
		Capabilities: []plugin.Capability{plugin.CapabilityAnthropicMessages},
		Streaming:    plugin.StreamingConverted,
	}
}

// ValidateSource 校验 Anthropic 源配置。专属选项在 US1 引入后实施。
func (p *Plugin) ValidateSource(config.Source) error { return nil }

// Backend 返回协议适配后端。
func (p *Plugin) Backend() plugin.Backend { return p.b }

// ListModels 拉取 Anthropic 兼容 /v1/models 目录。
func (p *Plugin) ListModels(ctx context.Context, src config.Source) ([]plugin.Model, error) {
	ms, err := p.c.ListModels(ctx, src.BaseURL, src.APIKey, src.Headers)
	if err != nil {
		return nil, err
	}
	out := make([]plugin.Model, 0, len(ms))
	for _, m := range ms {
		out = append(out, plugin.Model{ID: m.ID, DisplayName: m.DisplayName})
	}
	return out, nil
}
