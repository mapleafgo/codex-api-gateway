package anthropic

import (
	"context"

	anthropicclient "github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
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
		Schema: []plugin.Field{
			{
				Name: "default_max_tokens", Label: "Default Max Tokens", Type: plugin.FieldTypeInteger,
				Target:      plugin.FieldTargetOption,
				Description: "Default max_output_tokens when the client does not set one",
			},
			{
				Name: "cache_enabled", Label: "Prompt Cache Enabled", Type: plugin.FieldTypeBoolean,
				Default: true, Target: plugin.FieldTargetOption,
				Description: "Whether to inject Anthropic prompt cache breakpoints",
			},
		},
	}
}

// ValidateSource 校验 Anthropic 源配置。专属选项在 US1 引入后实施。
func (p *Plugin) ValidateSource(config.Source) error { return nil }

// Backend 返回带 options 归一化的适配后端。
func (p *Plugin) Backend() plugin.Backend { return anthropicOptionsBackend{p.b} }

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

// anthropicOptionsBackend 在委托前把 source options 归一化为 cfg.Anthropic，
// 使共享转换层（convert.ToAnthropic）可读 per-source 的 default_max_tokens 与
// cache_enabled，加载路径无需改动。
type anthropicOptionsBackend struct {
	inner plugin.Backend
}

func (n anthropicOptionsBackend) Execute(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	cfg *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(plugin.UpstreamEvent),
	attempt int,
) error {
	if cfg != nil && len(src.Options) > 0 {
		merged := *cfg
		if tok := intOption(src.Options["default_max_tokens"]); tok > 0 {
			merged.Anthropic.DefaultMaxTokens = tok
		}
		if v, ok := src.Options["cache_enabled"].(bool); ok {
			enabled := v
			merged.Anthropic.CacheEnabled = &enabled
		}
		cfg = &merged
	}
	return n.inner.Execute(ctx, rawBody, src, cfg, onEvent, onUpstream, attempt)
}

// intOption 兼容 koanf/yaml 解出的 int 或 float64 整数。
func intOption(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	default:
		return 0
	}
}

var _ plugin.Backend = anthropicOptionsBackend{}
