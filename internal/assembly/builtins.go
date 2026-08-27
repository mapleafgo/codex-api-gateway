// Package assembly 是唯一组装入口：把内置源插件组合成不可变 Registry，
// 注入给配置校验、调度、服务编排与管理框架。共享核心不 import 本包。
package assembly

import (
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
	anthropicplugin "github.com/mapleafgo/codex-api-gateway/internal/plugins/anthropic"
	copilotplugin "github.com/mapleafgo/codex-api-gateway/internal/plugins/copilot"
	openaichatplugin "github.com/mapleafgo/codex-api-gateway/internal/plugins/openaichat"
	openairesponsesplugin "github.com/mapleafgo/codex-api-gateway/internal/plugins/openairesponses"
)

// Builtins 返回内置源插件列表。插件在此互相组合（Copilot 委托 r/a/c），
// 后续新增源在此追加，共享调度与服务代码零改动。测试可叠加第三方插件后
// 再交给 plugin.New 构造注册表。
func Builtins() []plugin.SourcePlugin {
	return []plugin.SourcePlugin{
		anthropicplugin.New(),
		openaichatplugin.New(),
		openairesponsesplugin.New(),
		copilotplugin.New(),
	}
}

// NewBuiltins 构造内置源插件注册表，作为 cmd/server 的组装入口。
func NewBuiltins() (*plugin.Registry, error) {
	return plugin.New(Builtins()...)
}
