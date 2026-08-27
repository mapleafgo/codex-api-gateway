package plugin

import (
	"context"
	"log/slog"
	"strings"

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

// AdminCallbacks 是插件执行管理动作时可用的配置回调（由共享 admin 注入）。
type AdminCallbacks struct {
	// Snapshot 返回当前配置快照，插件用于冲突检查与构造新配置。
	Snapshot func() *config.Config
	// Write 持久化一个完整的新配置并触发热重载；返回错误时旧配置保留。
	Write func(*config.Config) error
}

// CallbackInjector 是插件可选实现：共享 admin 组装完成后注入配置回调。
// 插件用此接口读写全局配置（如 Device Flow 成功后写入凭据），
// 无须直接依赖共享 admin 包。
type CallbackInjector interface {
	InjectCallbacks(cb AdminCallbacks)
}

var reservedHeaders = map[string]bool{
	"content-type":      true,
	"authorization":     true,
	"accept":            true,
	"x-api-key":         true,
	"anthropic-version": true,
	"anthropic-beta":    true,
}

// IsReservedHeader 判断 header 名是否为保留头（不可被 source.headers 覆盖）。
func IsReservedHeader(k string) bool {
	return reservedHeaders[strings.ToLower(k)]
}

// SanitizeSourceHeaders 过滤自定义 header：去掉保留头与空白键，空结果返回 nil。
func SanitizeSourceHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if IsReservedHeader(k) {
			slog.Debug("跳过保留自定义 header", "header", k, "impact", "由网管统一管理")
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
