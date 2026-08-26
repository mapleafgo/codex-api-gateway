package plugin

import (
	"context"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// 敏感值哨兵字符串用于管理页读写的凭据脱敏语义。
const (
	RedactedValue = "__codex_redacted__"
	ClearValue    = "__codex_clear__"
)

// ActionRequest 是管理动作的绑定完成输入；body 已由共享 admin 解析为 JSON。
type ActionRequest struct {
	PluginID   string
	ActionID   string
	RouteID    string
	Method     string
	Source     config.Source
	Body       []byte
	Registered bool
}

// ActionResult 是管理动作的返回；Error 由共享 admin 统一脱敏后输出。
type ActionResult struct {
	Data    any
	Code    int
	Error   string
	Message string
}

// AdminExtension 是源插件暴露给管理页的扩展动作实现。
type AdminExtension interface {
	InvokeAction(ctx context.Context, req ActionRequest) (ActionResult, error)
}
