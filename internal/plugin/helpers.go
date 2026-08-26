package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oaconstant "github.com/openai/openai-go/v3/shared/constant"
)

// 事件 wire 字符串，派生自 SDK shared/constant 以防止与规范值漂移。
var (
	evResponseCreated           = string(oaconstant.ValueOf[oaconstant.ResponseCreated]())
	evResponseInProgress        = string(oaconstant.ValueOf[oaconstant.ResponseInProgress]())
	evResponseCompleted         = string(oaconstant.ValueOf[oaconstant.ResponseCompleted]())
	evResponseIncomplete        = string(oaconstant.ValueOf[oaconstant.ResponseIncomplete]())
	evResponseFailed            = string(oaconstant.ValueOf[oaconstant.ResponseFailed]())
	evOutputTextDelta           = string(oaconstant.ValueOf[oaconstant.ResponseOutputTextDelta]())
	evOutputTextDone            = string(oaconstant.ValueOf[oaconstant.ResponseOutputTextDone]())
	evReasoningTextDelta        = string(oaconstant.ValueOf[oaconstant.ResponseReasoningTextDelta]())
	evReasoningTextDone         = string(oaconstant.ValueOf[oaconstant.ResponseReasoningTextDone]())
	evReasoningSummaryTextDelta = string(oaconstant.ValueOf[oaconstant.ResponseReasoningSummaryTextDelta]())
	evReasoningSummaryTextDone  = string(oaconstant.ValueOf[oaconstant.ResponseReasoningSummaryTextDone]())
	evRefusalDelta              = string(oaconstant.ValueOf[oaconstant.ResponseRefusalDelta]())
	evRefusalDone               = string(oaconstant.ValueOf[oaconstant.ResponseRefusalDone]())
	evFunctionCallArgsDelta     = string(oaconstant.ValueOf[oaconstant.ResponseFunctionCallArgumentsDelta]())
	evFunctionCallArgsDone      = string(oaconstant.ValueOf[oaconstant.ResponseFunctionCallArgumentsDone]())
	evCustomToolCallInputDelta  = string(oaconstant.ValueOf[oaconstant.ResponseCustomToolCallInputDelta]())
	evCustomToolCallInputDone   = string(oaconstant.ValueOf[oaconstant.ResponseCustomToolCallInputDone]())
	evWebSearchInProgress       = string(oaconstant.ValueOf[oaconstant.ResponseWebSearchCallInProgress]())
	evWebSearchSearching        = string(oaconstant.ValueOf[oaconstant.ResponseWebSearchCallSearching]())
	evWebSearchCompleted        = string(oaconstant.ValueOf[oaconstant.ResponseWebSearchCallCompleted]())
	evOutputItemAdded           = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemAdded]())
	evOutputItemDone            = string(oaconstant.ValueOf[oaconstant.ResponseOutputItemDone]())
)

// ErrEmptyResponse 表示上游返回了数据（已过首字节）但未产出任何内容事件。
var ErrEmptyResponse = errors.New("upstream returned empty response (no content events)")

// ErrUpstreamTimeout 表示网关侧单笔源请求总时长到点。
var ErrUpstreamTimeout = errors.New("upstream request timeout (per-attempt)")

// MaxBufferedEvents 限制首个内容事件前缓冲的非内容事件数量。
const MaxBufferedEvents = 64

// IsContentEvent 判断事件是否携带上游实际产出内容（白名单判定）。
func IsContentEvent(e model.SSEEvent) bool {
	switch e.Type {
	case evOutputTextDelta, evOutputTextDone,
		evReasoningTextDelta, evReasoningTextDone,
		evReasoningSummaryTextDelta, evReasoningSummaryTextDone,
		evRefusalDelta, evRefusalDone,
		evFunctionCallArgsDelta, evFunctionCallArgsDone,
		evCustomToolCallInputDelta, evCustomToolCallInputDone,
		evWebSearchInProgress, evWebSearchSearching, evWebSearchCompleted:
		return true
	case evOutputItemAdded:
		var probe struct {
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal(e.Data, &probe) == nil && probe.Item.Type == model.ItemTypeToolSearchCall {
			return true
		}
		return false
	default:
		return false
	}
}

// IsBufferableEvent 判断事件是否应作为空响应候选缓冲。
func IsBufferableEvent(e model.SSEEvent) bool {
	if IsContentEvent(e) {
		return false
	}
	switch e.Type {
	case evResponseFailed, evResponseIncomplete:
		return false
	default:
		return true
	}
}

// EventGate 缓冲非内容事件直到首个内容事件/终态失败出现再 flush。
// 非并发安全：必须由同一个流式扫描 goroutine 串行调用。
type EventGate struct {
	buf     []model.SSEEvent
	flushed bool
	content bool
	failure bool
	onEvent func(model.SSEEvent) error
}

// NewEventGate 创建一个事件门控，包装原始 onEvent 回调。
func NewEventGate(onEvent func(model.SSEEvent) error) *EventGate {
	return &EventGate{onEvent: onEvent}
}

// Send 处理单个事件。
func (g *EventGate) Send(e model.SSEEvent) error {
	if g.flushed {
		return g.onEvent(e)
	}
	if IsBufferableEvent(e) {
		if len(g.buf) >= MaxBufferedEvents {
			return fmt.Errorf("event gate: %d buffered non-content events before content", len(g.buf))
		}
		g.buf = append(g.buf, e)
		return nil
	}
	g.flushed = true
	if IsContentEvent(e) {
		g.content = true
	} else {
		g.failure = true
	}
	for _, b := range g.buf {
		if err := g.onEvent(b); err != nil {
			return err
		}
	}
	g.buf = nil
	return g.onEvent(e)
}

// HasContent 报告是否已出现真实内容事件。
func (g *EventGate) HasContent() bool { return g.content }

// SawTerminalFailure 报告是否已出现需要透传客户端的错误/截断终态事件。
func (g *EventGate) SawTerminalFailure() bool { return g.failure }

// OutcomeInput 汇聚上游流扫描结束后终态归类所需的输入。
type OutcomeInput struct {
	Locked       bool
	ScanErr      error
	Terminal     bool
	Status       string
	Code         int
	ErrText      string
	NoEventsCode int
}

// ClassifyOutcome 把上游流扫描结果归类为上报 onUpstream 的终态。
func ClassifyOutcome(ctx context.Context, in OutcomeInput) (status string, code int, errText string, scanErr error) {
	status, code, errText, scanErr = in.Status, in.Code, in.ErrText, in.ScanErr
	if !in.Locked {
		if scanErr == nil {
			scanErr = fmt.Errorf("upstream returned no events")
		}
		status = "failed"
		code = in.NoEventsCode
		if IsServerTimeout(ctx, scanErr) {
			code = 504
			errText = fmt.Sprintf("upstream request timeout: %v", scanErr)
			return
		}
		if sc := StatusCodeFromErr(scanErr); sc != 0 {
			code = sc
		}
		errText = ErrSummary(scanErr)
		return
	}
	if scanErr == nil {
		return
	}
	if IsServerTimeout(ctx, scanErr) {
		status = "failed"
		code = 504
		errText = fmt.Sprintf("upstream request timeout: %v", scanErr)
		return
	}
	if IsClientCanceled(ctx, scanErr) {
		if !in.Terminal {
			status = "canceled"
		}
		return
	}
	status = "failed"
	if sc := StatusCodeFromErr(scanErr); sc != 0 {
		code = sc
	}
	errText = ErrSummary(scanErr)
	return
}

// IsClientCanceled 判断 err 是否由请求 ctx 取消引起。
func IsClientCanceled(ctx context.Context, err error) bool {
	if err == nil || ctx == nil {
		return false
	}
	if ctx.Err() == nil {
		return false
	}
	if errors.Is(context.Cause(ctx), ErrUpstreamTimeout) {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ctx.Err())
}

// ErrSummary 返回错误全文。
func ErrSummary(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// StatusCodeFromErr 从 client 错误串解析上游 HTTP 状态码。
func StatusCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	s := err.Error()
	for _, prefix := range []string{"anthropic upstream ", "upstream "} {
		i := strings.Index(s, prefix)
		if i < 0 {
			continue
		}
		rest := s[i+len(prefix):]
		n := 0
		for _, ch := range rest {
			if ch < '0' || ch > '9' {
				break
			}
			n = n*10 + int(ch-'0')
		}
		if n >= 100 && n <= 599 {
			return n
		}
	}
	return 0
}

// contextOverflowMarkers 是上下文超限的错误文本特征。
var contextOverflowMarkers = []string{
	"context_length_exceeded",
	"context length",
	"context window exceeded",
	"exceeds the context window",
	"maximum context window",
	"max context window",
	"model_context_window_exceeded",
	"longer than the model's context",
	"input is longer than",
	"prompt is too long",
}

// ContextLengthExceededCode 判定错误是否属于上下文超限。
func ContextLengthExceededCode(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ToLower(err.Error())
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(text, marker) {
			return model.ErrorCodeContextLengthExceeded
		}
	}
	return ""
}

// IsClientError reports whether err represents an HTTP 4xx client error.
func IsClientError(err error) bool {
	code := StatusCodeFromErr(err)
	if code == 429 || code == 408 {
		return false
	}
	return code >= 400 && code < 500
}

// IsServerTimeout 判断 err 是否由网关侧单笔总时长超时引起。
func IsServerTimeout(ctx context.Context, err error) bool {
	if ctx == nil || err == nil {
		return false
	}
	return errors.Is(context.Cause(ctx), ErrUpstreamTimeout) || errors.Is(err, ErrUpstreamTimeout)
}
