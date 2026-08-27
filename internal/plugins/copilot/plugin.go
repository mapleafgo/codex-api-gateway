package copilot

import (
	"context"
	"errors"
	"net/http"

	"github.com/mapleafgo/codex-api-gateway/internal/backend"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// Plugin 是 GitHub Copilot 源插件：endpoint 发现、模型目录、Device Flow 认证
// 与按模型能力的 r/a/c 协议路由全部归属本包。
type Plugin struct {
	b    *Backend
	auth *authManager
}

// errMissingGithubToken 表示 Copilot 源缺少 github_token 必填凭据。
var errMissingGithubToken = errors.New("copilot: missing github_token")

// New 构造 Copilot 源插件，组合已有的三个 Backend 做委托。
func New() *Plugin {
	p := &Plugin{
		b: NewBackend(backend.NewResponses(), backend.NewAnthropic(), backend.NewChat()),
	}
	p.auth = newAuthManager(NewAuthClient(nil, "", ""), plugin.AdminCallbacks{})
	return p
}

// InjectCallbacks 由共享 admin 在 Mount 时注入配置读写回调。
func (p *Plugin) InjectCallbacks(cb plugin.AdminCallbacks) {
	p.auth.setCallbacks(cb)
}

// InvokeAction 实现 AdminExtension：分发 device-flow 等管理动作。
func (p *Plugin) InvokeAction(ctx context.Context, req plugin.ActionRequest) (plugin.ActionResult, error) {
	switch req.ActionID {
	case "device-flow":
		return p.invokeDeviceFlow(ctx, req)
	}
	return plugin.ActionResult{Code: http.StatusNotFound, Error: "unknown action"}, nil
}

// Descriptor 返回插件的唯一身份与能力声明。
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:           plugin.BackendGitHubCopilot,
		Title:        "GitHub Copilot",
		Summary:      "通过 GitHub Copilot API 按模型能力委托 Responses / Messages / Chat 协议",
		Capabilities: []plugin.Capability{plugin.CapabilityResponsesPassthrough, plugin.CapabilityAnthropicMessages, plugin.CapabilityChatCompletions},
		Streaming:    plugin.StreamingConverted,
		Actions: []plugin.Action{
			{
				ID: "device-flow", Label: "GitHub Device Flow",
				Kind: plugin.ActionKindDeviceCodeStatus,
				Routes: []plugin.ActionRoute{
					{ID: "start", Method: "POST", Path: "/admin/api/copilot/auth/start"},
					{ID: "status", Method: "GET", Path: "/admin/api/copilot/auth/status"},
					{ID: "cancel", Method: "POST", Path: "/admin/api/copilot/auth/cancel"},
				},
			},
		},
		Schema: []plugin.Field{
			{
				Name: "github_token", Label: "GitHub Token", Type: plugin.FieldTypePassword,
				Required: true, Sensitive: true, Target: plugin.FieldTargetOption,
				Description: "GitHub OAuth token used to authenticate against the Copilot API",
			},
		},
	}
}

// ValidateSource 校验 Copilot 源配置：必须携带 github_token。
func (p *Plugin) ValidateSource(src config.Source) error {
	if copilotToken(src) == "" {
		return errMissingGithubToken
	}
	return nil
}

// Backend 返回协议适配后端。
func (p *Plugin) Backend() plugin.Backend { return p.b }

// copilotToken 返回配置中的 GitHub token：只从插件声明的 options 区读取，
// 共享核心不承载任何顶层 github_token 专属字段。
func copilotToken(src config.Source) string {
	if t, _ := src.Options["github_token"].(string); t != "" {
		return t
	}
	return ""
}

var _ plugin.Backend = (*Backend)(nil)
var _ plugin.HealthProbe = (*Plugin)(nil)
var _ plugin.AdminExtension = (*Plugin)(nil)
var _ plugin.CallbackInjector = (*Plugin)(nil)
