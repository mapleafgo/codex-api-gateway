package anthropic

import (
	"context"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// captureBackend 记录委托调用收到的 cfg，用于断言 options 归一化。
type captureBackend struct {
	gotCfg *config.Config
}

func (c *captureBackend) Execute(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	cfg *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(plugin.UpstreamEvent),
	attempt int,
) error {
	c.gotCfg = cfg
	return nil
}

func TestBackendNormalizesOptions(t *testing.T) {
	inner := &captureBackend{}
	w := anthropicOptionsBackend{inner: inner}

	base := &config.Config{}
	src := config.Source{
		Name: "a",
		Options: map[string]any{
			"default_max_tokens": 32768,
			"cache_enabled":      false,
		},
	}
	if err := w.Execute(context.Background(), nil, src, base, nil, nil, 1); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if inner.gotCfg == nil {
		t.Fatal("inner backend not called with cfg")
	}
	if inner.gotCfg.Anthropic.DefaultMaxTokens != 32768 {
		t.Errorf("DefaultMaxTokens = %d, want 32768", inner.gotCfg.Anthropic.DefaultMaxTokens)
	}
	if inner.gotCfg.Anthropic.CacheEnabledValue() {
		t.Error("CacheEnabled should be false")
	}
	// 原始 cfg 不应被修改（深拷贝）。
	if base.Anthropic.DefaultMaxTokens != 0 || base.Anthropic.CacheEnabled != nil {
		t.Errorf("base cfg mutated: %+v", base.Anthropic)
	}
}

func TestBackendKeepsDefaultWhenNoOptions(t *testing.T) {
	inner := &captureBackend{}
	w := anthropicOptionsBackend{inner: inner}
	base := &config.Config{Anthropic: config.AnthropicCfg{DefaultMaxTokens: 100}}
	if err := w.Execute(context.Background(), nil, config.Source{Name: "a"}, base, nil, nil, 1); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if inner.gotCfg == nil || inner.gotCfg.Anthropic.DefaultMaxTokens != 100 {
		t.Errorf("cfg not preserved: %+v", inner.gotCfg)
	}
}
