// Package backend 定义上游协议适配器；契约类型与 internal/plugin 保持一致。
package backend

import (
	plugin "github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// Backend 是 internal/backend 对插件契约 Backend 的别名，保持历史包名引用可用。
type Backend = plugin.Backend

// UpstreamEvent 是插件观测事件的别名。
type UpstreamEvent = plugin.UpstreamEvent
