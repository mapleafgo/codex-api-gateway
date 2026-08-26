package openaichat

import (
	"context"

	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/chatclient"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// Plugin 是 OpenAI Chat Completions 源插件（仅流式）。
type Plugin struct {
	b *backend.ChatBackend
	c *chatclient.Client
}

// New 构造 OpenAI Chat 源插件。
func New() *Plugin {
	return &Plugin{b: backend.NewChat(), c: chatclient.New()}
}

// Descriptor 返回插件的唯一身份与能力声明。
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:           plugin.BackendOpenAIChat,
		Title:        "OpenAI Chat Completions",
		Summary:      "将 Responses 请求转换为 Chat Completions 流式协议",
		Capabilities: []plugin.Capability{plugin.CapabilityChatCompletions},
		Streaming:    plugin.StreamingConverted,
	}
}

// ValidateSource 校验 OpenAI Chat 源配置。当前阶段无专属必填项。
func (p *Plugin) ValidateSource(config.Source) error { return nil }

// Backend 返回协议适配后端。
func (p *Plugin) Backend() plugin.Backend { return p.b }

// ListModels 拉取 Chat 兼容 /v1/models 目录。
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
