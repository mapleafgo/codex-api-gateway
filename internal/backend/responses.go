package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/responsesclient"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

var allowedToolsType = string(oaconstant.ValueOf[oaconstant.AllowedTools]())

// ResponsesBackend 将 Responses 请求透传到 OpenAI Responses 上游（仅流式）。
type ResponsesBackend struct {
	Client *responsesclient.Client
}

// NewResponses 构造 ResponsesBackend。
func NewResponses() *ResponsesBackend {
	return &ResponsesBackend{Client: responsesclient.New()}
}

// PrepareUpstreamBody 将客户端 Responses JSON 做最小改写：model 映射 + 强制 stream=true。
// 使用 map 语义透传，保留未知扩展字段。log 用于折算日志的请求级关联，空时退回默认 logger。
func PrepareUpstreamBody(raw []byte, src *config.Source, log *slog.Logger) (body []byte, clientModel, resolved string, err error) {
	m, err := decodeObject(raw)
	if err != nil {
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
	if !src.SupportsWebSearchValue() {
		stripWebSearchTools(m, log)
	}
	rewriteReasoningSummaryToContent(m, log)
	rewritePlaintextAgentMessages(m, log)

	body, err = json.Marshal(m)
	if err != nil {
		return nil, "", "", fmt.Errorf("marshal: %w", err)
	}
	return body, clientModel, resolved, nil
}

// stripWebSearchTools 从 Responses 请求 tools 数组里剥掉 hosted web_search 声明。
// r 直通路径原样透传 tools 给上游；上游若只支持标准 OpenAI Responses（不认识
// web_search hosted tool），声明会导致 400/断流。按源能力控制后，仅在此源不支持
// web_search 时剥除，保留其他工具不变。
func stripWebSearchTools(m map[string]any, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	rawTools, ok := m["tools"].([]any)
	if ok {
		out := make([]any, 0, len(rawTools))
		removed := 0
		for _, raw := range rawTools {
			obj, ok := raw.(map[string]any)
			if ok {
				typ, _ := obj["type"].(string)
				if isWebSearchTypeString(typ) {
					removed++
					continue
				}
			}
			out = append(out, raw)
		}
		if removed > 0 {
			m["tools"] = out
			log.Debug("responses: 源不支持 hosted web_search，剥掉工具声明",
				"removed", removed)
		}
	}
	if neutralizeRawWebSearchToolChoice(m) {
		log.Debug("responses: 源不支持 hosted web_search，清理 tool_choice/allowed_tools")
	}
}

// neutralizeRawWebSearchToolChoice 从透传 JSON 里移除 tool_choice / allowed_tools
// 对 hosted web_search 的引用。true 表示发生了修改。
func neutralizeRawWebSearchToolChoice(m map[string]any) bool {
	rawTC, ok := m["tool_choice"].(map[string]any)
	if !ok {
		return false
	}
	typ, _ := rawTC["type"].(string)
	if isWebSearchTypeString(typ) {
		delete(m, "tool_choice")
		return true
	}
	if typ != allowedToolsType {
		return false
	}
	rawTools, ok := rawTC["tools"].([]any)
	if !ok {
		return false
	}
	out := make([]any, 0, len(rawTools))
	for _, raw := range rawTools {
		entry, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		entryType, _ := entry["type"].(string)
		if isWebSearchTypeString(entryType) {
			continue
		}
		out = append(out, raw)
	}
	if len(out) == len(rawTools) {
		return false
	}
	if len(out) == 0 {
		delete(m, "tool_choice")
		return true
	}
	rawTC["tools"] = out
	return true
}

// rewriteReasoningSummaryToContent 把 OpenAI 标准 reasoning item 的 summary 明文
// 折算进 content（reasoning_text part）：DeepSeek /responses 只支持 plain-text
// content 并把它合并进相邻 assistant message，忽略 summary/encrypted_content；
// 只透传 summary 会触发 "reasoning_text ... must be passed back" 400。
func rewriteReasoningSummaryToContent(m map[string]any, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	input, ok := m["input"].([]any)
	if !ok {
		return
	}
	converted := 0
	textLen := 0
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "reasoning" {
			continue
		}
		if hasReasoningTextContent(item["content"]) {
			continue
		}
		texts := reasoningSummaryTexts(item["summary"])
		if len(texts) == 0 {
			continue
		}
		parts := make([]any, 0, len(texts))
		for _, t := range texts {
			parts = append(parts, map[string]any{"type": "reasoning_text", "text": t})
			textLen += len(t)
		}
		item["content"] = parts
		converted++
	}
	if converted > 0 {
		log.Debug("responses: reasoning summary 折算为 content",
			"model", m["model"],
			"converted", converted,
			"text_len", textLen)
	}
}

func hasReasoningTextContent(v any) bool {
	switch c := v.(type) {
	case string:
		return c != ""
	case []any:
		for _, raw := range c {
			p, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := p["text"].(string); ok && s != "" {
				return true
			}
		}
	}
	return false
}

func reasoningSummaryTexts(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var texts []string
	for _, raw := range arr {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := p["text"].(string); ok && s != "" {
			texts = append(texts, s)
		}
	}
	return texts
}

// rewritePlaintextAgentMessages 把完全明文的 Codex agent_message 就地折成
// 标准 assistant message。Responses 兼容上游不认识 agent_message 扩展时会把
// 初始 NEW_TASK / follow-up MESSAGE 静默丢掉；带 encrypted_content 的消息可能
// 属于原生支持该扩展的上游，保持原样交给上游裁决。
func rewritePlaintextAgentMessages(m map[string]any, log *slog.Logger) {
	input, ok := m["input"].([]any)
	if !ok {
		return
	}
	converted := 0
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || item["type"] != "agent_message" {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok || len(content) == 0 {
			continue
		}
		plaintext := true
		for _, rawContent := range content {
			part, ok := rawContent.(map[string]any)
			if !ok || part["type"] != model.ContentTypeInputText {
				plaintext = false
				break
			}
		}
		if !plaintext {
			continue
		}
		item["type"] = model.ItemTypeMessage
		item["role"] = model.RoleAssistant
		delete(item, "author")
		delete(item, "recipient")
		converted++
	}
	if converted == 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	log.Debug("responses: 明文 agent_message 折为 assistant message",
		"converted", converted,
		"impact", "NEW_TASK/MESSAGE 文本保留，位置不变")
}

// decodeObject 用 UseNumber 解码 JSON 对象，避免 map 重编码时大整数经 float64 丢精度。
func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// rewriteClientModel 按 T2 规则把 data 中顶层/response 内 model 回写为客户端请求 model。
// 未含 model 的帧原样返回；Marshal 失败保留原 Data。
func rewriteClientModel(data []byte, clientModel string) []byte {
	if clientModel == "" {
		return data
	}
	m, err := decodeObject(data)
	if err != nil || m == nil {
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

// rewriteCollabPlaintextArgs 对 output_item.added/done 里的 collaboration
// function_call 注入空 encrypted_function_args 信号，让 Codex 走明文投递
// （DirectPlaintextMessage），避免任务内容被塞进 openai-go 不认识的
// encrypted_content。与 rewriteClientModel 同属最小改写的透传层。
func rewriteCollabPlaintextArgs(data []byte) []byte {
	m, err := decodeObject(data)
	if err != nil || m == nil {
		return data
	}
	rawItem, ok := m["item"]
	if !ok {
		return data
	}
	item, ok := rawItem.(map[string]any)
	if !ok || item["type"] != "function_call" {
		return data
	}
	namespace, _ := item["namespace"].(string)
	name, _ := item["name"].(string)
	if !model.IsPlaintextCollabTool(namespace, name) {
		return data
	}
	// 透传层"结果归上游"：上游已携带有效字段（如真 OpenAI 多 agent 加密编排）
	// 时原样保留，仅在缺失时才注入明文信号，避免替上游改判能力。
	if _, exists := item["encrypted_function_args"]; exists {
		return data
	}
	item["encrypted_function_args"] = []any{}
	out, err := json.Marshal(m)
	if err != nil {
		slog.Debug("responses: rewriteCollabPlaintextArgs marshal failed", "error", err)
		return data
	}
	slog.Debug("responses: collaboration 工具注入明文 encrypted_function_args 信号",
		"tool", name)
	return out
}

// parseUsageFromEvent 尽力从终态事件解析 usage（仅观测，失败返回 0）。
func parseUsageFromEvent(eventType string, data []byte) (inTok, outTok, cacheRead, cacheCreate int, ok bool) {
	switch eventType {
	case evResponseCompleted, evResponseIncomplete, evResponseFailed:
	default:
		return 0, 0, 0, 0, false
	}
	var envelope struct {
		Response struct {
			Usage *struct {
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				InputTokensDetails *struct {
					CachedTokens     int `json:"cached_tokens"`
					CacheWriteTokens int `json:"cache_write_tokens"`
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
		cacheCreate = u.InputTokensDetails.CacheWriteTokens
	}
	if u.CacheReadInputTokens != 0 {
		cacheRead = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != 0 {
		cacheCreate = u.CacheCreationInputTokens
	}
	return inTok, outTok, cacheRead, cacheCreate, true
}

func parseResponseError(data []byte) string {
	var envelope struct {
		Response struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Response.Error == nil {
		return ""
	}
	return envelope.Response.Error.Message
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
	log := logging.FromContext(ctx).With(
		"source", src.Name,
		"backend_type", config.BackendOpenAIResponses,
		"attempt", attempt)
	body, clientModel, resolved, err := PrepareUpstreamBody(rawBody, &src, log)
	if err != nil {
		return err
	}

	log.Info("Responses 透传请求准备完成",
		"model", clientModel,
		"resolved_model", resolved)

	stream, err := b.Client.Stream(ctx, src.BaseURL, src.APIKey, body, src.Headers)
	if err != nil {
		log.Warn("Responses 上游建连失败", "elapsed", time.Since(start).String(), "error", err)
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
	terminalStatus := ""
	terminalError := ""
	var inTok, outTok, cacheRead, cacheCreate int

	scanErr := responsesclient.ScanSSE(stream, func(et string, data []byte) error {
		if !locked {
			locked = true
			ttfb = time.Since(start)
			log.Info("Responses 上游首字节到达", "ttfb", ttfb.String())
		}
		// 先记终态再写出，避免 onEvent 内 cancel 时终态尚未置位。
		switch et {
		case evResponseCompleted:
			terminalStatus = "completed"
		case evResponseIncomplete:
			terminalStatus = "incomplete"
		case evResponseFailed:
			terminalStatus = "failed"
			terminalError = parseResponseError(data)
		}
		data = rewriteClientModel(data, clientModel)
		if et == evOutputItemAdded || et == evOutputItemDone {
			data = rewriteCollabPlaintextArgs(data)
		}
		if err := onEvent(model.SSEEvent{Type: et, Data: data}); err != nil {
			return err
		}
		// 观测：尽力解析 usage，不中断流
		if i, o, cr, cc, ok := parseUsageFromEvent(et, data); ok {
			inTok, outTok, cacheRead, cacheCreate = i, o, cr, cc
		}
		return nil
	})

	initialStatus := terminalStatus
	if initialStatus == "" {
		initialStatus = "completed"
	}
	status, code, errText, scanErr := classifyOutcome(ctx, outcomeInput{
		locked:   locked,
		scanErr:  scanErr,
		terminal: terminalStatus != "",
		status:   initialStatus,
		code:     200,
		errText:  terminalError,
		// 无事件且错误串解析不出状态码时 code 落 0（与历史行为一致）。
		noEventsCode: 0,
	})
	// 上游发了部分事件后干净关流、始终未给出 completed/failed/incomplete 终态：
	// 客户端收到的是截断流，指标不得记 completed。只修观测，不向客户端
	// 合成任何 SSE 终态事件（透传层不代补终态）。
	if locked && scanErr == nil && terminalStatus == "" {
		status = "failed"
		errText = "upstream stream ended without terminal event"
		log.Warn("Responses 上游流缺失终态事件即关闭",
			"model", clientModel,
			"resolved_model", resolved,
			"elapsed", time.Since(start).String())
	}
	level := slog.LevelInfo
	if status == "failed" {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, "Responses 上游流结束",
		"status", status,
		"code", code,
		"error", errText,
		"elapsed", time.Since(start).String(),
		"ttfb", ttfb.String(),
		"input_tokens", inTok,
		"output_tokens", outTok,
		"cache_read_tokens", cacheRead,
		"cache_creation_tokens", cacheCreate)

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
	return scanErr
}
