package openairesponses

import (
	"context"

	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
	"github.com/mapleafgo/codex-api-gateway/internal/responsesclient"
)

// Plugin 是 OpenAI Responses 透传源插件（仅流式）。
type Plugin struct {
	b *backend.ResponsesBackend
	c *responsesclient.Client
}

// New 构造 OpenAI Responses 源插件。
func New() *Plugin {
	return &Plugin{b: backend.NewResponses(), c: responsesclient.New()}
}

// Descriptor 返回插件的唯一身份与能力声明。
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:           plugin.BackendOpenAIResponses,
		Title:        "OpenAI Responses",
		Summary:      "把 Responses 请求原样透传给兼容上游并回传 SSE",
		Capabilities: []plugin.Capability{plugin.CapabilityResponsesPassthrough},
		Streaming:    plugin.StreamingPassthrough,
	}
}

// ValidateSource 校验 Responses 透传源配置。当前阶段无专属必填项。
func (p *Plugin) ValidateSource(config.Source) error { return nil }

// Backend 返回协议适配后端。
func (p *Plugin) Backend() plugin.Backend { return p.b }

// ListModels 拉取 Responses 兼容 /v1/models 目录。
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
