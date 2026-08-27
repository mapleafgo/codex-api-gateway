package openairesponses

import (
	"context"

	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
	"github.com/mapleafgo/codex-api-gateway/internal/responsesclient"
	"github.com/mapleafgo/codex-api-gateway/internal/upstreamhttp"
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
		Schema: []plugin.Field{
			{
				Name: "base_url", Label: "Base URL", Type: plugin.FieldTypeText,
				Required: true, Target: plugin.FieldTargetBaseURL,
				Description: "OpenAI-compatible Responses base URL",
			},
			{
				Name: "api_key", Label: "API Key", Type: plugin.FieldTypePassword,
				Target:      plugin.FieldTargetAPIKey,
				Description: "API key sent as Bearer token",
			},
		},
	}
}

// ValidateSource 校验 Responses 透传源配置。当前阶段无专属必填项。
func (p *Plugin) ValidateSource(config.Source) error { return nil }

// Backend 返回协议适配后端。
func (p *Plugin) Backend() plugin.Backend { return responsesPluginBackend{p.b} }

// responsesPluginBackend 在透传后端上承载 RequestPreparer：预检 dry-run
// PrepareUpstreamBody，不发起上游请求。
type responsesPluginBackend struct {
	*backend.ResponsesBackend
}

func (responsesPluginBackend) PrepareRequest(_ context.Context, req *plugin.PrepareRequestInput) error {
	_, _, _, err := backend.PrepareUpstreamBody(req.RawBody, &req.Source, nil)
	return err
}

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

// Probe 健康探测：GET /v1/models 验证可达性与凭据；404 时降级最小 POST 到 /responses。
func (p *Plugin) Probe(ctx context.Context, src config.Source) plugin.ProbeResult {
	return plugin.HTTPProbe(ctx, src, plugin.ProbeHTTPConfig{
		ModelsURL:       upstreamhttp.ModelsURL(src.BaseURL),
		FallbackPostURL: upstreamhttp.EndpointURL(src.BaseURL, "/responses"),
	})
}

var _ plugin.HealthProbe = (*Plugin)(nil)
