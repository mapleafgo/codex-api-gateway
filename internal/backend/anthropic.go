package backend

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	aconstant "github.com/anthropics/anthropic-sdk-go/shared/constant"
	anthropicclient "github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/convert"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/streamconv"
)

var (
	anMessageStart = string(aconstant.ValueOf[aconstant.MessageStart]())
	anMessageDelta = string(aconstant.ValueOf[aconstant.MessageDelta]())
	anMessageStop  = string(aconstant.ValueOf[aconstant.MessageStop]())
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
func (b *AnthropicBackend) Execute(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	cfg *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(UpstreamEvent),
	attempt int,
) error {
	start := time.Now()
	req, err := convert.DecodeResponseNewParams(rawBody)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	clientModel := string(req.Model)
	anthReq, mcp, err := convert.ToAnthropic(req, cfg)
	if err != nil {
		return fmt.Errorf("toanthropic: %w", err)
	}
	resolved := resolveModel(&src, clientModel)
	anthReq.Model = anthropic.Model(resolved)

	conv := streamconv.New()
	conv.SetClientModel(clientModel)
	conv.SetCustomToolNames(convert.FreeformToolNames(req))
	conv.SetDeclaredServerTools(convert.DeclaredServerTools(req))
	if string(req.Reasoning.Summary) == model.ReasoningSummaryConcise {
		conv.SetSummarized(true)
	}

	body, err := b.Client.Stream(ctx, src.BaseURL, src.APIKey, anthReq, mcp)
	if err != nil {
		if onUpstream != nil {
			onUpstream(UpstreamEvent{
				SourceName: src.Name, Model: clientModel, ResolvedModel: resolved,
				StartedAt: start, Duration: time.Since(start),
				Status: "failed", Code: 500, Error: err.Error(), Attempt: attempt,
				BackendType: config.BackendAnthropic,
			})
		}
		return err
	}
	defer body.Close()

	var ttfb time.Duration
	locked := false
	var usage anthropic.MessageDeltaUsage
	sawStop := false

	scanErr := anthropicclient.ScanEvents(body, func(ev *anthropic.MessageStreamEventUnion) error {
		if !locked {
			locked = true
			ttfb = time.Since(start)
			slog.Info("Anthropic 上游首字节到达", "source", src.Name, "ttfb", ttfb.String())
		}
		if ev != nil && ev.Type == anMessageStop {
			sawStop = true
		}
		// 与 scheduler.mergeUsage 一致：message_start 取 input/cache_* 初值，
		// message_delta 取累计值（含 cache_* 刷新），按「最后一次非零」合并。
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

	if scanErr != nil && locked && !conv.Done() {
		// 流中途失败：补 response.failed（sequence 延续 converter）
		errResp := model.NewResponseObject(conv.RespID(), model.ResponseStatusFailed, clientModel, time.Now().Unix(), model.ResponseObjectParams{})
		errResp.Output = conv.OutputItems()
		errResp.Error = &model.ResponseError{Message: scanErr.Error()}
		evType := "response.failed"
		_ = onEvent(model.MarshalEvent(evType, model.TerminalResponseEvent{
			Type: evType, SequenceNumber: conv.NextSeq(), Response: errResp,
		}))
	}
	status := "completed"
	code := 200
	errText := ""
	if scanErr != nil {
		if ctx.Err() != nil {
			if sawStop {
				status = "completed"
			} else {
				status = "canceled"
			}
		} else {
			status = "failed"
			code = 500
			errText = scanErr.Error()
		}
	} else if !locked {
		status = "failed"
		code = 500
		scanErr = fmt.Errorf("upstream returned no events")
		errText = scanErr.Error()
	}
	if onUpstream != nil {
		if status == "completed" {
			errText = ""
		}
		onUpstream(UpstreamEvent{
			SourceName: src.Name, Model: clientModel, ResolvedModel: resolved,
			StartedAt: start, Duration: time.Since(start), TTFB: ttfb,
			Status: status, Code: code, Error: errText, Attempt: attempt,
			InputTokens: int(usage.InputTokens), OutputTokens: int(usage.OutputTokens),
			CacheRead: int(usage.CacheReadInputTokens), CacheCreate: int(usage.CacheCreationInputTokens),
			BackendType: config.BackendAnthropic,
		})
	}
	if !locked {
		return scanErr
	}
	return scanErr
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
