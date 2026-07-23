package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/responsesclient"
)

// ResponsesBackend 将 Responses 请求透传到 OpenAI Responses 兼容上游（仅流式）。
type ResponsesBackend struct {
	Client *responsesclient.Client
}

// NewResponses 构造 ResponsesBackend。
func NewResponses() *ResponsesBackend {
	return &ResponsesBackend{Client: responsesclient.New()}
}

// PrepareUpstreamBody 将客户端 Responses JSON 做最小改写：model 映射 + 强制 stream=true。
// 使用 map 语义透传，保留未知扩展字段。
func PrepareUpstreamBody(raw []byte, src *config.Source) (body []byte, clientModel, resolved string, err error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "", "", fmt.Errorf("decode: %w", err)
	}
	if m == nil {
		return nil, "", "", fmt.Errorf("decode: body is not a JSON object")
	}

	// model 字段
	if v, ok := m["model"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return nil, "", "", fmt.Errorf("decode: model must be a string")
		}
		clientModel = s
	}
	resolved = resolveModel(src, clientModel)
	if clientModel == "" {
		clientModel = resolved
	}
	m["model"] = resolved
	m["stream"] = true

	body, err = json.Marshal(m)
	if err != nil {
		return nil, "", "", fmt.Errorf("marshal: %w", err)
	}
	return body, clientModel, resolved, nil
}

// rewriteClientModel 按 T2 规则把 data 中顶层/response 内 model 回写为客户端请求 model。
// 未含 model 的帧原样返回；Marshal 失败保留原 Data。
func rewriteClientModel(data []byte, clientModel string) []byte {
	if clientModel == "" {
		return data
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return data
	}
	changed := false
	if v, ok := m["model"]; ok {
		if s, ok := v.(string); ok && s != clientModel {
			m["model"] = clientModel
			changed = true
		}
	}
	if respRaw, ok := m["response"]; ok {
		if resp, ok := respRaw.(map[string]any); ok {
			if v, ok := resp["model"]; ok {
				if s, ok := v.(string); ok && s != clientModel {
					resp["model"] = clientModel
					m["response"] = resp
					changed = true
				}
			}
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(m)
	if err != nil {
		slog.Debug("responses: rewriteClientModel marshal failed", "error", err)
		return data
	}
	return out
}

// parseUsageFromEvent 尽力从终态事件解析 usage（仅观测，失败返回 0）。
func parseUsageFromEvent(eventType string, data []byte) (inTok, outTok, cacheRead, cacheCreate int, ok bool) {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed":
	default:
		return 0, 0, 0, 0, false
	}
	var envelope struct {
		Response struct {
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				InputTokensDetails       *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
				// 兼容部分上游可能暴露的 cache 字段
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Response.Usage == nil {
		return 0, 0, 0, 0, false
	}
	u := envelope.Response.Usage
	inTok, outTok = u.InputTokens, u.OutputTokens
	if u.InputTokensDetails != nil {
		cacheRead = u.InputTokensDetails.CachedTokens
	}
	if u.CacheReadInputTokens != 0 {
		cacheRead = u.CacheReadInputTokens
	}
	cacheCreate = u.CacheCreationInputTokens
	return inTok, outTok, cacheRead, cacheCreate, true
}

// Execute 实现 Backend：透传 Responses 上游 SSE，T2 model 回写，不合成终态。
func (b *ResponsesBackend) Execute(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	_ *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(UpstreamEvent),
	attempt int,
) error {
	start := time.Now()
	body, clientModel, resolved, err := PrepareUpstreamBody(rawBody, &src)
	if err != nil {
		return err
	}

	slog.Info("Responses 透传请求准备完成",
		"source", src.Name,
		"model", clientModel,
		"resolved_model", resolved,
		"backend_type", config.BackendOpenAIResponses)

	stream, err := b.Client.Stream(ctx, src.BaseURL, src.APIKey, body)
	if err != nil {
		if onUpstream != nil {
			onUpstream(UpstreamEvent{
				SourceName: src.Name, Model: clientModel, ResolvedModel: resolved,
				StartedAt: start, Duration: time.Since(start),
				Status: "failed", Code: StatusCodeFromErr(err), Error: errSummary(err), Attempt: attempt,
				BackendType: config.BackendOpenAIResponses,
			})
		}
		return err
	}
	defer stream.Close()

	var ttfb time.Duration
	locked := false
	var inTok, outTok, cacheRead, cacheCreate int

	scanErr := responsesclient.ScanSSE(stream, func(et string, data []byte) error {
		if !locked {
			locked = true
			ttfb = time.Since(start)
			slog.Info("Responses 上游首字节到达", "source", src.Name, "ttfb", ttfb.String())
		}
		data = rewriteClientModel(data, clientModel)
		if err := onEvent(model.SSEEvent{Type: et, Data: data}); err != nil {
			return err
		}
		// 观测：尽力解析 usage，不中断流
		if i, o, cr, cc, ok := parseUsageFromEvent(et, data); ok {
			inTok, outTok, cacheRead, cacheCreate = i, o, cr, cc
		}
		return nil
	})

	status := "completed"
	code := 200
	errText := ""
	if !locked {
		if scanErr == nil {
			scanErr = fmt.Errorf("upstream returned no events")
		}
		status = "failed"
		code = StatusCodeFromErr(scanErr)
		errText = errSummary(scanErr)
	} else if scanErr != nil {
		if isClientCanceled(ctx, scanErr) {
			status = "canceled"
		} else {
			status = "failed"
			if sc := StatusCodeFromErr(scanErr); sc != 0 {
				code = sc
			}
			errText = errSummary(scanErr)
		}
	}

	if onUpstream != nil {
		onUpstream(UpstreamEvent{
			SourceName: src.Name, Model: clientModel, ResolvedModel: resolved,
			StartedAt: start, Duration: time.Since(start), TTFB: ttfb,
			Status: status, Code: code, Error: errText, Attempt: attempt,
			InputTokens: inTok, OutputTokens: outTok,
			CacheRead: cacheRead, CacheCreate: cacheCreate,
			BackendType: config.BackendOpenAIResponses,
		})
	}
	if !locked {
		return scanErr
	}
	return scanErr
}
