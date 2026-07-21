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
	var usageIn, usageOut, cacheRead, cacheCreate int
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
		// usage 粗提
		if ev != nil && ev.Type == anMessageStart {
			usageIn = int(ev.Message.Usage.InputTokens)
			cacheRead = int(ev.Message.Usage.CacheReadInputTokens)
			cacheCreate = int(ev.Message.Usage.CacheCreationInputTokens)
		}
		if ev != nil && ev.Type == anMessageDelta {
			if ev.Usage.OutputTokens > 0 {
				usageOut = int(ev.Usage.OutputTokens)
			}
			if ev.Usage.InputTokens > 0 {
				usageIn = int(ev.Usage.InputTokens)
			}
		}
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
			InputTokens: usageIn, OutputTokens: usageOut,
			CacheRead: cacheRead, CacheCreate: cacheCreate,
			BackendType: config.BackendAnthropic,
		})
	}
	if !locked {
		return scanErr
	}
	return scanErr
}
