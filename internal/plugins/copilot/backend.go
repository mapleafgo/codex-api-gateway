package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// Backend 将 Responses 请求按模型能力路由到已有的 r/a/c 转换路径，
// 上游是 GitHub Copilot API（仅流式）。认证参照 Zed：直接用 GitHub OAuth token
// 作为 Bearer，通过 GraphQL 发现 API endpoint，不换 Copilot session token。
type Backend struct {
	responses plugin.Backend
	anthropic plugin.Backend
	chat      plugin.Backend
	Client    *Client
}

// NewBackend 构造 Copilot Backend，组合已有的三个 Backend 做委托。
// 三个 Backend 参数均不可为 nil；返回值可并发执行。
func NewBackend(responses, anthropic, chat plugin.Backend) *Backend {
	return &Backend{
		responses: responses,
		anthropic: anthropic,
		chat:      chat,
		Client:    NewClient(),
	}
}

// routeByEndpoints 按模型 supported_endpoints 以优先级 r > a > c 选择协议路径。
// info 为 nil 或无匹配时返回 "r"（默认，让上游返回结果或错误）。
// 映射："/responses" → r；"/v1/messages" → a；"/chat/completions" → c。
func routeByEndpoints(info *ModelInfo) string {
	if info == nil {
		return "r"
	}
	for _, prefer := range []struct {
		endpoint string
		route    string
	}{
		{"/responses", "r"},
		{"/v1/messages", "a"},
		{"/chat/completions", "c"},
	} {
		for _, ep := range info.SupportedEndpoints {
			if ep == prefer.endpoint {
				return prefer.route
			}
		}
	}
	return "r"
}

// ResolveEndpoint 返回 source 的 Copilot API endpoint。显式 BaseURL 优先；
// 缺省时执行 GraphQL 发现并缓存结果。失败时返回默认 endpoint。
func (b *Backend) ResolveEndpoint(ctx context.Context, src *config.Source) string {
	return b.Client.ResolveEndpoint(ctx, *src)
}

// ListModels 返回按 Zed 条件筛选后的 Copilot 模型目录。结果来自 per-source
// TTL 缓存，模型按 ID 排序以保证管理页输出稳定；目录拉取失败时返回错误。
func (b *Backend) ListModels(ctx context.Context, src config.Source) ([]ModelInfo, error) {
	return b.Client.ListModels(ctx, src)
}

// Execute 实现 Backend 接口：按模型能力路由并委托已有的 r/a/c 后端。
func (b *Backend) Execute(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	cfg *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(plugin.UpstreamEvent),
	attempt int,
) error {
	log := logging.FromContext(ctx).With(
		"source", src.Name,
		"backend", plugin.BackendGitHubCopilot,
		"attempt", attempt)

	token := copilotToken(src)
	if token == "" {
		return fmt.Errorf("copilot: source %q missing github_token", src.Name)
	}

	clientModel, resolved, err := extractModel(rawBody, &src)
	if err != nil {
		return fmt.Errorf("copilot: decode model: %w", err)
	}

	directoryStart := time.Now()
	directory, merr := b.Client.Directory(ctx, src)
	directoryElapsed := time.Since(directoryStart)
	endpoint := directory.Endpoint

	var info *ModelInfo
	if merr != nil {
		log.Warn("Copilot 模型目录拉取失败，回退默认路由 r",
			"error", merr, "endpoint", endpoint, "elapsed", directoryElapsed.String())
	} else {
		for i := range directory.Models {
			if directory.Models[i].ID == resolved {
				info = &directory.Models[i]
				break
			}
		}
	}
	route := routeByEndpoints(info)

	delegateSrc := src
	delegateSrc.BaseURL = endpoint
	delegateSrc.APIKey = token
	delegateSrc.Headers = mergeHeaders(src.Headers, Headers())
	// Copilot /responses 为原生 OpenAI Responses 兼容端点，接受原生 reasoning
	// 形态；委托时关闭共享 r 路径的兼容折算，工具调用 id 命名空间归一化在本包完成。
	delegateSrc.ResponsesCompatFold = boolPtr(false)

	log.Info("Copilot 请求路由决策",
		"model", clientModel,
		"resolved_model", resolved,
		"route", route,
		"endpoint", endpoint,
		"models_cached", len(directory.Models),
		"directory_elapsed", directoryElapsed.String())

	reportUpstream := func(ev plugin.UpstreamEvent) {
		if onUpstream == nil {
			return
		}
		ev.Backend = plugin.BackendGitHubCopilot
		onUpstream(ev)
	}

	switch route {
	case "r":
		body, err := normalizeInputIDs(rawBody, log)
		if err != nil {
			return fmt.Errorf("copilot: normalize input ids: %w", err)
		}
		return b.responses.Execute(ctx, body, delegateSrc, cfg, onEvent, reportUpstream, attempt)
	case "a":
		if bearer, ok := b.anthropic.(plugin.BearerOnlyBackend); ok {
			return bearer.ExecuteWithAuthorization(ctx, rawBody, delegateSrc, cfg, onEvent, reportUpstream, attempt)
		}
		return b.anthropic.Execute(ctx, rawBody, delegateSrc, cfg, onEvent, reportUpstream, attempt)
	case "c":
		return b.chat.Execute(ctx, rawBody, delegateSrc, cfg, onEvent, reportUpstream, attempt)
	default:
		return fmt.Errorf("copilot: unexpected route %q", route)
	}
}

// extractModel 从 Responses JSON 中提取 model 并按 source 的 ModelMap/DefaultModel 解析。
func extractModel(raw []byte, src *config.Source) (clientModel, resolved string, err error) {
	var top struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", "", fmt.Errorf("decode: %w", err)
	}
	clientModel = top.Model
	resolved = plugin.ResolveModel(src, clientModel)
	if clientModel == "" {
		clientModel = resolved
	}
	return clientModel, resolved, nil
}

// mergeHeaders 合并用户自定义 header 与 Copilot 必需 header。
// Copilot 必需 header 优先（不可被 source.headers 覆盖）。
func mergeHeaders(user, copilotHeaders map[string]string) map[string]string {
	out := make(map[string]string, len(user)+len(copilotHeaders))
	for k, v := range user {
		out[k] = v
	}
	for k, v := range copilotHeaders {
		for existing := range out {
			if strings.EqualFold(existing, k) {
				delete(out, existing)
			}
		}
		out[k] = v
	}
	return out
}

// boolPtr 返回指向 b 的指针，用于在委托时设置源级开关。
func boolPtr(b bool) *bool { return &b }

// normalizeInputIDs 把历史消息里 function_call / custom_tool_call 的 id 归一化为
// Copilot /responses 端点要求的 fc_ / ctc_ 前缀；call_id 关联 tool result 保持原样。
// 在委托给共享 Responses 后端前完成，使共享层不感知 Copilot 命名空间约定。
func normalizeInputIDs(raw []byte, log *slog.Logger) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return raw, nil // 解析失败时原样透传，交由下游解码报错
	}
	input, ok := m["input"].([]any)
	if !ok {
		return raw, nil
	}
	converted := 0
	for _, entry := range input {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		var prefix string
		switch typ {
		case model.ItemTypeFunctionCall:
			prefix = "fc_"
		case model.ItemTypeCustomToolCall:
			prefix = "ctc_"
		default:
			continue
		}
		id, _ := item["id"].(string)
		if id == "" || strings.HasPrefix(id, prefix) {
			continue
		}
		item["id"] = prefix + id
		converted++
	}
	if converted > 0 && log != nil {
		log.Debug("copilot: 归一化工具调用 id 前缀", "converted", converted)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw, nil
	}
	return out, nil
}
