package copilot

import (
	"context"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// ListModels 拉取 Copilot 筛选后的模型目录，供管理页与协议路由共享。
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
