package openaichat

import (
	"context"

	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/chatclient"
	"github.com/mapleafgo/codex-api-gateway/internal/chatconvert"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/convert"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
	"github.com/mapleafgo/codex-api-gateway/internal/upstreamhttp"
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
		Schema: []plugin.Field{
			{
				Name: "base_url", Label: "Base URL", Type: plugin.FieldTypeText,
				Required: true, Target: plugin.FieldTargetBaseURL,
				Description: "OpenAI-compatible Chat Completions base URL",
			},
			{
				Name: "api_key", Label: "API Key", Type: plugin.FieldTypePassword,
				Target:      plugin.FieldTargetAPIKey,
				Description: "API key sent as Bearer token",
			},
		},
	}
}

// ValidateSource 校验 OpenAI Chat 源配置。当前阶段无专属必填项。
func (p *Plugin) ValidateSource(config.Source) error { return nil }

// Backend 返回带 RequestPreparer 的协议适配后端。
func (p *Plugin) Backend() plugin.Backend { return chatPluginBackend{p.b} }

// chatPluginBackend 在 Chat 后端上承载 RequestPreparer：预检 dry-run
// Responses -> Chat 转换，不发起上游请求。
type chatPluginBackend struct {
	*backend.ChatBackend
}

func (chatPluginBackend) PrepareRequest(_ context.Context, req *plugin.PrepareRequestInput) error {
	params, err := convert.DecodeResponseNewParams(req.RawBody)
	if err != nil {
		return err
	}
	_, err = chatconvert.ToChat(params, "")
	return err
}

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

// Probe 健康探测：GET /v1/models 验证可达性与凭据；404 时降级最小 POST 到 /chat/completions。
func (p *Plugin) Probe(ctx context.Context, src config.Source) plugin.ProbeResult {
	return plugin.HTTPProbe(ctx, src, plugin.ProbeHTTPConfig{
		ModelsURL:       upstreamhttp.ModelsURL(src.BaseURL),
		FallbackPostURL: upstreamhttp.EndpointURL(src.BaseURL, "/chat/completions"),
	})
}

var _ plugin.HealthProbe = (*Plugin)(nil)
var _ plugin.RequestPreparer = chatPluginBackend{}
