package backend

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	aconstant "github.com/anthropics/anthropic-sdk-go/shared/constant"
	anthropicclient "github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/convert"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
	"github.com/mapleafgo/codex-api-gateway/internal/streamconv"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

var (
	anMessageStart = string(aconstant.ValueOf[aconstant.MessageStart]())
	anMessageDelta = string(aconstant.ValueOf[aconstant.MessageDelta]())
	anMessageStop  = string(aconstant.ValueOf[aconstant.MessageStop]())
	anError        = string(aconstant.ValueOf[aconstant.Error]())
)

// AnthropicBackend 将 Responses 请求转到 Anthropic Messages 上游。
type AnthropicBackend struct {
	Client *anthropicclient.Client
}

// NewAnthropic 构造 AnthropicBackend。
func NewAnthropic() *AnthropicBackend {
	return &AnthropicBackend{Client: anthropicclient.New()}
}

// Execute 实现 Backend：convert → anthropic stream → streamconv。
// 行为对齐改造前 server.handleResponses + scheduler.trySource 的 a 路径：
// SetEcho / summarized / tools 声明、usage+cache 合并、message_stop 收尾、
// 客户端取消判定、上游 HTTP 状态码、转换诊断日志。
func (b *AnthropicBackend) Execute(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	cfg *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(UpstreamEvent),
	attempt int,
) error {
	return b.execute(ctx, rawBody, src, cfg, onEvent, onUpstream, attempt, false)
}

// ExecuteWithAuthorization 保持 Execute 的 Responses ↔ Anthropic 转换行为，
// 但上游认证只发送 Authorization: Bearer，并省略 x-api-key/anthropic-version。
// 用于 GitHub Copilot Messages 端点；并发安全性与 Execute 相同。
func (b *AnthropicBackend) ExecuteWithAuthorization(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	cfg *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(UpstreamEvent),
	attempt int,
) error {
	return b.execute(ctx, rawBody, src, cfg, onEvent, onUpstream, attempt, true)
}

func (b *AnthropicBackend) execute(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	cfg *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(UpstreamEvent),
	attempt int,
	bearerOnly bool,
) error {
	start := time.Now()
	log := logging.FromContext(ctx).With(
		"source", src.Name,
		"backend", plugin.BackendAnthropic,
		"attempt", attempt)
	req, err := convert.DecodeResponseNewParams(rawBody)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if !src.SupportsWebSearchValue() {
		stripWebSearchToolsFromParams(req, log)
	}
	clientModel := string(req.Model)
	anthReq, err := convert.ToAnthropic(req, cfg)
	if err != nil {
		return fmt.Errorf("toanthropic: %w", err)
	}
	resolved := resolveModel(&src, clientModel)
	anthReq.Model = anthropic.Model(resolved)
	// Copilot 的 Anthropic 端点要求 enabled thinking 显式携带 budget_tokens，
	// 共享转换层刻意省略它以兼容常规上游；仅在 Copilot 委派时补齐。
	if bearerOnly && src.BackendType == config.BackendGitHubCopilot {
		applyCopilotThinkingBudget(anthReq, req)
	}

	maxTokensSource := "anthropic_default"
	if req.MaxOutputTokens.Valid() && req.MaxOutputTokens.Value > 0 {
		maxTokensSource = "client"
	}
	logAnthropicConverted(log, anthReq, maxTokensSource)

	conv := streamconv.New()
	conv.SetLogger(log)
	conv.SetEcho(convert.EchoFromRequest(req))
	conv.SetClientModel(clientModel)
	conv.SetCustomToolNames(convert.FreeformToolNames(req))
	conv.SetDeclaredServerTools(convert.DeclaredServerTools(req))
	conv.SetDeclaredToolNames(convert.DeclaredToolNames(req))
	// 仅 summary==concise 时上游返回 summarized thinking，才用 reasoning_summary_*。
	if string(req.Reasoning.Summary) == model.ReasoningSummaryConcise {
		conv.SetSummarized(true)
	}

	log.Info("尝试 Anthropic 上游",
		"endpoint", src.BaseURL,
		"model", clientModel,
		"resolved_model", resolved)
	var body io.ReadCloser
	var upstreamCode int
	if bearerOnly {
		body, upstreamCode, err = b.Client.StreamWithAuthorization(ctx, src.BaseURL, src.APIKey, anthReq, src.Headers)
	} else {
		body, upstreamCode, err = b.Client.Stream(ctx, src.BaseURL, src.APIKey, anthReq, src.Headers)
	}
	if err != nil {
		log.Warn("上游源建连失败", "elapsed", time.Since(start).String(), "error", err)
		if onUpstream != nil {
			if upstreamCode == 0 {
				upstreamCode = StatusCodeFromErr(err)
			}
			onUpstream(UpstreamEvent{
				SourceName: src.Name, Model: clientModel, ResolvedModel: resolved,
				StartedAt: start, Duration: time.Since(start),
				Status: "failed", Code: upstreamCode, Error: errSummary(err), Attempt: attempt,
				Backend: plugin.BackendAnthropic,
			})
		}
		return err
	}
	defer body.Close()

	var ttfb time.Duration
	sawFirstByte := false
	locked := false
	var usage anthropic.MessageDeltaUsage
	sawStop := false

	scanErr := anthropicclient.ScanEvents(ctx, body, func(ev *anthropic.MessageStreamEventUnion) error {
		if !sawFirstByte {
			sawFirstByte = true
			ttfb = time.Since(start)
			log.Info("Anthropic 上游首字节到达", "ttfb", ttfb.String())
		}
		if ev != nil && ev.Type == anError && !locked {
			message := ev.Delta.Text
			if message == "" {
				message = "upstream stream error"
			}
			if ev.Delta.Type != "" {
				return fmt.Errorf("anthropic upstream stream error (%s): %s", ev.Delta.Type, message)
			}
			return fmt.Errorf("anthropic upstream stream error: %s", message)
		}
		if !locked {
			locked = true
		}
		if ev != nil && ev.Type == anMessageStop {
			sawStop = true
		}
		mergeAnthropicUsage(&usage, ev)
		out, err := conv.Feed(ev)
		if err != nil {
			return err
		}
		for _, e := range out {
			if err := onEvent(e); err != nil {
				return err
			}
		}
		return nil
	})

	// 收尾安全网：对齐旧 server 成功路径再喂一次 message_stop。
	// 上游未发 message_stop 但干净结束时，仍产出 response.completed。
	if locked && !conv.Done() {
		if scanErr == nil || (IsClientCanceled(ctx, scanErr) && sawStop) {
			trailing, _ := conv.Feed(&anthropic.MessageStreamEventUnion{Type: anMessageStop})
			for _, e := range trailing {
				if err := onEvent(e); err != nil && scanErr == nil {
					scanErr = err
				}
			}
		} else if scanErr != nil && !IsClientCanceled(ctx, scanErr) {
			// 流中途失败：补 response.failed（sequence 延续 converter）
			errResp := model.NewResponseObject(conv.RespID(), model.ResponseStatusFailed, clientModel, time.Now().Unix(), convert.EchoFromRequest(req))
			errResp.Output = conv.OutputItems()
			errResp.Error = &model.ResponseError{Message: scanErr.Error()}
			evType := evResponseFailed
			_ = onEvent(model.MarshalEvent(evType, model.TerminalResponseEvent{
				Type: evType, SequenceNumber: conv.NextSeq(), Response: errResp,
			}))
		}
	}

	if locked && scanErr != nil {
		if IsClientCanceled(ctx, scanErr) {
			if !sawStop && !conv.Done() {
				log.Info("上游流读取因客户端断开中止", "elapsed", time.Since(start).String(), "error", scanErr)
			}
		} else {
			log.Warn("上游流读取失败（已锁定）", "elapsed", time.Since(start).String(), "error", scanErr)
		}
	}

	status, code, errText, scanErr := classifyOutcome(ctx, outcomeInput{
		Locked:   locked,
		ScanErr:  scanErr,
		Terminal: sawStop || conv.Done(),
		Status:   conv.Status(),
		Code:     upstreamCode,
		// 错误串解析不出状态码时保留建连返回的 upstreamCode。
		NoEventsCode: upstreamCode,
	})
	level := slog.LevelInfo
	if status == "failed" {
		level = slog.LevelWarn
	}
	// 优先用 streamconv.Usage（含 cache），与旧 server 成功路径一致；
	// 若 converter 尚未记 usage，回退 mergeAnthropicUsage。
	inTok, outTok := int(usage.InputTokens), int(usage.OutputTokens)
	cacheRead, cacheCreate := int(usage.CacheReadInputTokens), int(usage.CacheCreationInputTokens)
	if u := conv.Usage(); u != nil {
		if u.InputTokens > 0 {
			inTok = u.InputTokens
		}
		if u.OutputTokens > 0 {
			outTok = u.OutputTokens
		}
		if u.CacheReadInputTokens > 0 {
			cacheRead = u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens > 0 {
			cacheCreate = u.CacheCreationInputTokens
		}
	}
	log.Log(ctx, level, "Anthropic 上游流结束",
		"status", status,
		"code", code,
		"error", errText,
		"elapsed", time.Since(start).String(),
		"ttfb", ttfb.String(),
		"input_tokens", inTok,
		"output_tokens", outTok,
		"cache_read", cacheRead,
		"cache_create", cacheCreate)
	if onUpstream != nil {
		if status == "completed" {
			errText = ""
		}
		onUpstream(UpstreamEvent{
			SourceName: src.Name, Model: clientModel, ResolvedModel: resolved,
			StartedAt: start, Duration: time.Since(start), TTFB: ttfb,
			Status: status, Code: code, Error: errText, Attempt: attempt,
			InputTokens: inTok, OutputTokens: outTok,
			CacheRead: cacheRead, CacheCreate: cacheCreate,
			Backend: plugin.BackendAnthropic,
		})
	}
	// 无论是否锁定都原样返回 scanErr：未锁定时供 scheduler 故障转移；
	// 已锁定时（如业务终态后客户端断开）供上层记日志，onUpstream 已按实际终态上报。
	return scanErr
}

func logAnthropicConverted(log *slog.Logger, anthReq *anthropic.MessageNewParams, maxTokensSource string) {
	sysLen := 0
	for _, b := range anthReq.System {
		sysLen += len(b.Text)
	}
	thinkingOn := anthReq.Thinking.OfEnabled != nil || anthReq.Thinking.OfAdaptive != nil
	thinkingBlocks, emptySig, toolUseBlk, toolResultBlk, assistantMsgs, userMsgs := summarizeAnthropicRequest(anthReq)
	log.Info("请求转换完成",
		"model", string(anthReq.Model),
		"max_tokens", anthReq.MaxTokens,
		"max_tokens_source", maxTokensSource,
		"cache_enabled", anthReq.CacheControl.Type != "",
		"messages", len(anthReq.Messages),
		"assistant_messages", assistantMsgs,
		"user_messages", userMsgs,
		"system_bytes", sysLen,
		"thinking", thinkingOn,
		"thinking_blocks", thinkingBlocks,
		"thinking_empty_signature", emptySig,
		"tool_use_blocks", toolUseBlk,
		"tool_result_blocks", toolResultBlk,
		"tools", len(anthReq.Tools))
	if emptySig > 0 {
		log.Warn("回灌的 thinking block 存在空 signature，可能违反 Anthropic thinking round-trip 规则",
			"thinking_blocks", thinkingBlocks, "empty_signature", emptySig)
	}
}

// defaultCopilotThinkingBudget 是 Copilot thinking budget 的内置默认值；
// 合法区间为 ≥1024 且严格小于 max_tokens。
const defaultCopilotThinkingBudget int64 = 8192

// applyCopilotThinkingBudget 为 Copilot /v1/messages 补 thinking budget_tokens。
// Copilot 的 Anthropic 端点对 enabled thinking 报 "budget_tokens: Field required"，
// 而共享转换层 applyReasoning 刻意构造不带 budget 的 {"type":"enabled"}（SDK 零值
// 会序列化为 0 被常规 Anthropic 拒绝）。此处整体替换为带合法 budget 的结构体，
// 保留 summarized display，常规 Anthropic 源行为不受影响。
func applyCopilotThinkingBudget(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) {
	effort := string(req.Reasoning.Effort)
	if effort == "" || effort == model.ReasoningEffortNone {
		return
	}
	budget := defaultCopilotThinkingBudget
	if max := out.MaxTokens; max > 1024 {
		candidate := max - 1024
		if candidate < budget {
			budget = candidate
		}
		if budget < 1024 {
			budget = 1024
		}
	}
	enabled := anthropic.ThinkingConfigEnabledParam{BudgetTokens: budget}
	if string(req.Reasoning.Summary) == model.ReasoningSummaryConcise {
		enabled.Display = anthropic.ThinkingConfigEnabledDisplaySummarized
	}
	out.Thinking = anthropic.ThinkingConfigParamUnion{OfEnabled: &enabled}
}

func summarizeAnthropicRequest(req *anthropic.MessageNewParams) (thinkingBlocks, emptySig, toolUse, toolResult, assistant, user int) {
	for _, msg := range req.Messages {
		switch msg.Role {
		case anthropic.MessageParamRoleAssistant:
			assistant++
		case anthropic.MessageParamRoleUser:
			user++
		}
		for _, b := range msg.Content {
			switch {
			case b.OfThinking != nil:
				thinkingBlocks++
				if b.OfThinking.Signature == "" {
					emptySig++
				}
			case b.OfRedactedThinking != nil:
				thinkingBlocks++
			case b.OfToolUse != nil:
				toolUse++
			case b.OfToolResult != nil:
				toolResult++
			}
		}
	}
	return
}

// mergeAnthropicUsage 合并单个 Anthropic SSE 事件的 usage 到累计视图。
// message_start 用 Message.Usage；message_delta 用事件级 Usage（累计）。
// 取最后一次非零值，避免把 delta 累计值与 start 初值重复相加。
func mergeAnthropicUsage(acc *anthropic.MessageDeltaUsage, ev *anthropic.MessageStreamEventUnion) {
	if acc == nil || ev == nil {
		return
	}
	switch ev.Type {
	case anMessageStart:
		u := ev.Message.Usage
		if u.InputTokens > 0 {
			acc.InputTokens = u.InputTokens
		}
		if u.OutputTokens > 0 {
			acc.OutputTokens = u.OutputTokens
		}
		if u.CacheReadInputTokens > 0 {
			acc.CacheReadInputTokens = u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens > 0 {
			acc.CacheCreationInputTokens = u.CacheCreationInputTokens
		}
	case anMessageDelta:
		u := ev.Usage
		if u.InputTokens > 0 {
			acc.InputTokens = u.InputTokens
		}
		if u.OutputTokens > 0 {
			acc.OutputTokens = u.OutputTokens
		}
		if u.CacheReadInputTokens > 0 {
			acc.CacheReadInputTokens = u.CacheReadInputTokens
		}
		if u.CacheCreationInputTokens > 0 {
			acc.CacheCreationInputTokens = u.CacheCreationInputTokens
		}
	}
}
