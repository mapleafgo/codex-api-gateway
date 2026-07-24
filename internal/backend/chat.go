package backend

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/chatclient"
	"github.com/mapleafgo/codex-api-gateway/internal/chatconvert"
	"github.com/mapleafgo/codex-api-gateway/internal/chatstreamconv"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/convert"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// ChatBackend 将 Responses 请求转到 OpenAI Chat Completions 兼容上游（仅流式）。
type ChatBackend struct {
	Client *chatclient.Client
}

// NewChat 构造 ChatBackend。
func NewChat() *ChatBackend {
	return &ChatBackend{Client: chatclient.New()}
}

// Execute 实现 Backend。
func (b *ChatBackend) Execute(
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
		"backend_type", config.BackendOpenAIChat,
		"attempt", attempt)
	req, err := convert.DecodeResponseNewParams(rawBody)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	clientModel := string(req.Model)
	resolved := resolveModel(&src, clientModel)

	chatReq, err := chatconvert.ToChat(req, resolved)
	if err != nil {
		return fmt.Errorf("tochat: %w", err)
	}
	body, err := chatconvert.Marshal(chatReq)
	if err != nil {
		return fmt.Errorf("marshal chat: %w", err)
	}

	log.Info("Chat 请求转换完成",
		"model", clientModel,
		"resolved_model", resolved,
		"messages", len(chatReq.Messages),
		"tools", len(chatReq.Tools))

	stream, err := b.Client.Stream(ctx, src.BaseURL, src.APIKey, body)
	if err != nil {
		log.Warn("Chat 上游建连失败", "elapsed", time.Since(start).String(), "error", err)
		if onUpstream != nil {
			onUpstream(UpstreamEvent{
				SourceName: src.Name, Model: clientModel, ResolvedModel: resolved,
				StartedAt: start, Duration: time.Since(start),
				Status: "failed", Code: StatusCodeFromErr(err), Error: errSummary(err), Attempt: attempt,
				BackendType: config.BackendOpenAIChat,
			})
		}
		return err
	}
	defer stream.Close()

	conv := chatstreamconv.New()
	conv.SetEcho(convert.EchoFromRequest(req))
	conv.SetClientModel(clientModel)
	conv.SetFreeformNames(chatReq.FreeformNames)

	var ttfb time.Duration
	locked := false
	scanErr := chatclient.ScanEvents(stream, func(data []byte) error {
		if !locked {
			locked = true
			ttfb = time.Since(start)
			log.Info("Chat 上游首字节到达", "ttfb", ttfb.String())
		}
		evs, err := conv.Feed(data)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if err := onEvent(e); err != nil {
				return err
			}
		}
		return nil
	})
	// [DONE] 路径：ScanEvents 在 [DONE] 处 break；补 FeedDone。
	// 仅当已收到过 data chunk（locked）时才 FeedDone。
	// OpenCode/OpenRouter 常见：先发 `: OPENROUTER PROCESSING` 注释，随后仅
	// `data: [DONE]` 或空 body 关闭。若此时 FeedDone，会合成 created+completed，
	// scheduler 误判已锁定，客户端先看到空成功，server 再 ERROR
	// "upstream returned no events"。
	if locked {
		if scanErr == nil {
			for _, e := range conv.FeedDone() {
				if err := onEvent(e); err != nil {
					scanErr = err
					break
				}
			}
		} else if !conv.Done() {
			for _, e := range conv.Fail(scanErr.Error()) {
				_ = onEvent(e)
			}
		}
	}

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
			if conv.Done() {
				status = "completed"
			} else {
				status = "canceled"
			}
		} else {
			status = "failed"
			if sc := StatusCodeFromErr(scanErr); sc != 0 {
				code = sc
			}
			errText = errSummary(scanErr)
		}
	}
	var inTok, outTok, cacheRead, cacheCreate int
	if u := conv.Usage(); u != nil {
		inTok, outTok = u.InputTokens, u.OutputTokens
		cacheRead = u.CacheReadInputTokens
		cacheCreate = u.CacheCreationInputTokens
	}
	level := slog.LevelInfo
	if status == "failed" {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, "Chat 上游流结束",
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
			CacheRead:   cacheRead,
			CacheCreate: cacheCreate,
			BackendType: config.BackendOpenAIChat,
		})
	}
	if !locked {
		return scanErr
	}
	return scanErr
}

// 确保 config 常量可用
var _ = config.BackendOpenAIChat
