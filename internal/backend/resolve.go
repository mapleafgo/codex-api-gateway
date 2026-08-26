package backend

import (
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// resolveModel 委托共享 plugin.ResolveModel，保持历史包内引用不变。
func resolveModel(src *config.Source, reqModel string) string {
	return plugin.ResolveModel(src, reqModel)
}
