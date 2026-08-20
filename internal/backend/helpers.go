package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oairesponses "github.com/openai/openai-go/v3/responses"
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

// stripWebSearchToolsFromParams 从 Responses 请求的工具列表里剥掉 hosted web_search 声明。
// a/c 后端把 req.Tools 转成上游格式（Anthropic server tool / Chat function）；
// 若上游只支持标准工具（不认识 hosted web_search），保留会导致上游 400/断流。
// 按源能力（config.Source.SupportsWebSearchValue）决定是否调用；剥除工具的同时
// 同步清理指向 web_search 的 tool_choice / allowed_tools，避免残留引用。
func stripWebSearchToolsFromParams(req *oairesponses.ResponseNewParams, log *slog.Logger) {
	if req == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	if len(req.Tools) > 0 {
		out := make([]oairesponses.ToolUnionParam, 0, len(req.Tools))
		removed := 0
		for _, t := range req.Tools {
			if t.OfWebSearch != nil {
				removed++
				continue
			}
			out = append(out, t)
		}
		if removed > 0 {
			req.Tools = out
			log.Debug("backend: 源不支持 hosted web_search，剥掉工具声明", "removed", removed)
		}
	}
	if neutralizeWebSearchToolChoice(&req.ToolChoice) {
		log.Debug("backend: 源不支持 hosted web_search，清理 tool_choice/allowed_tools")
	}
}

// isWebSearchTypeString 判断 tool / tool_choice 的 wire 字符串是否属于 hosted
// web_search 形态。Codex 实际只发 "web_search"，日期后缀来自 OpenAI SDK 常量。
func isWebSearchTypeString(t string) bool {
	return t == model.ToolTypeWebSearch ||
		t == string(oairesponses.WebSearchToolTypeWebSearch2025_08_26)
}

// neutralizeWebSearchToolChoice 移除 tool_choice 中对 hosted web_search 的引用：
// 直接 hosted 选择清空，allowed_tools 列表过滤；全空时清空整个 tool_choice。
func neutralizeWebSearchToolChoice(tc *oairesponses.ResponseNewParamsToolChoiceUnion) bool {
	if tc == nil {
		return false
	}
	if hosted := tc.OfHostedTool; hosted != nil && isWebSearchTypeString(string(hosted.Type)) {
		*tc = oairesponses.ResponseNewParamsToolChoiceUnion{}
		return true
	}
	if allowed := tc.OfAllowedTools; allowed != nil {
		out := make([]map[string]any, 0, len(allowed.Tools))
		for _, entry := range allowed.Tools {
			typ, _ := entry["type"].(string)
			if isWebSearchTypeString(typ) {
				continue
			}
			out = append(out, entry)
		}
		if len(out) == len(allowed.Tools) {
			return false
		}
		if len(out) == 0 {
			*tc = oairesponses.ResponseNewParamsToolChoiceUnion{}
			return true
		}
		allowed.Tools = out
		return true
	}
	return false
}

// outcomeInput 汇聚各后端上游流扫描结束后终态归类所需的输入。
// status/code/errText 是后端各自的初值，三者差异必须由调用方显式给出，
// 不在 classifyOutcome 内统一：anthropic 起始 code 为建连返回的 upstreamCode，
// chat/responses 起始 code 为 200；responses 的 errText 初值来自终态事件。
type outcomeInput struct {
	locked  bool
	scanErr error
	// terminal 表示业务终态已达成，语义由各后端定义：
	// anthropic 为 sawStop||conv.Done()，chat 为 conv.Done()，
	// responses 为收到 completed/failed/incomplete 终态事件。
	terminal bool
	status   string
	code     int
	errText  string
	// noEventsCode 是「未锁定且错误串解析不出状态码」时的兜底 code：
	// anthropic 保留建连时拿到的 upstreamCode，chat/responses 落 0。
	noEventsCode int
}

// classifyOutcome 把上游流扫描结果归类为上报 onUpstream 的终态。
// 返回的 scanErr 可能被替换：未锁定且无错误时补 "upstream returned no events"。
func classifyOutcome(ctx context.Context, in outcomeInput) (status string, code int, errText string, scanErr error) {
	status, code, errText, scanErr = in.status, in.code, in.errText, in.scanErr
	if !in.locked {
		if scanErr == nil {
			scanErr = fmt.Errorf("upstream returned no events")
		}
		status = "failed"
		code = in.noEventsCode
		if IsServerTimeout(ctx, scanErr) {
			// 未锁定时的单笔超时：同样是网关侧终止，统一 504 归因。
			code = 504
			errText = fmt.Sprintf("upstream request timeout: %v", scanErr)
			return
		}
		if sc := StatusCodeFromErr(scanErr); sc != 0 {
			code = sc
		}
		errText = errSummary(scanErr)
		return
	}
	if scanErr == nil {
		return
	}
	if IsServerTimeout(ctx, scanErr) {
		// 单笔总时长到点：网关侧终止，终态 failed + 504，与客户端取消分道。
		status = "failed"
		code = 504
		errText = fmt.Sprintf("upstream request timeout: %v", scanErr)
		return
	}
	if IsClientCanceled(ctx, scanErr) {
		// 业务终态已达成后客户端才断开：保留初值状态，不算 canceled。
		if !in.terminal {
			status = "canceled"
		}
		return
	}
	status = "failed"
	if sc := StatusCodeFromErr(scanErr); sc != 0 {
		code = sc
	}
	errText = errSummary(scanErr)
	return
}

// IsClientCanceled 判断 err 是否由请求 ctx 取消引起（客户端断开）。
// 首字节超时会取消子 ctx，但父 ctx 仍有效，故须同时检查父 ctx.Err()。
func IsClientCanceled(ctx context.Context, err error) bool {
	if err == nil || ctx == nil {
		return false
	}
	if ctx.Err() == nil {
		return false
	}
	if errors.Is(context.Cause(ctx), ErrUpstreamTimeout) {
		// 网关侧单笔总时长超时不是客户端断开，不得归类为 canceled。
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ctx.Err())
}

func errSummary(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// StatusCodeFromErr 从 client 错误串解析上游 HTTP 状态码。
// 支持 "anthropic upstream %d: ..." 与 chatclient "upstream %d: ..."。
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

// IsClientError reports whether err represents an HTTP 4xx client error
// that indicates the request itself is invalid (no point retrying elsewhere).
// 429/408 被排除：它们是传输可用性信号（限流/超时），必须走正常降级与
// 整轮重试路径，否则持续限流的源永不降级、稳坐优先级第一位。
// 其余 4xx 同样计入 breaker 失败（降级/机会失败），但整轮不重试。
func IsClientError(err error) bool {
	code := StatusCodeFromErr(err)
	if code == 429 || code == 408 {
		return false
	}
	return code >= 400 && code < 500
}

// ErrEmptyResponse 表示上游返回了数据（已过首字节）但未产出任何内容事件。
// 网关不得向客户端发出合成终态，scheduler 可自然 failover 到下一个源。
var ErrEmptyResponse = errors.New("upstream returned empty response (no content events)")

// ErrUpstreamTimeout 表示网关侧单笔源请求总时长到点（breaker.request_timeout）。
// 由 scheduler 通过 context.WithTimeoutCause 注入 cause，各层据此把该结束归因为
// 网关侧超时而非客户端取消；scheduler 返回时会把该哨兵包进 error，供 server 区分。
var ErrUpstreamTimeout = errors.New("upstream request timeout (per-attempt)")

// IsServerTimeout 判断 err 是否由网关侧单笔总时长超时引起。
// 仅当 attempt context 的 deadline 自身到点（cause 为 ErrUpstreamTimeout）时返回
// true；首字节超时通过父 ctx 主动 cancel 触发，不会设置该 cause。
func IsServerTimeout(ctx context.Context, err error) bool {
	if ctx == nil || err == nil {
		return false
	}
	return errors.Is(context.Cause(ctx), ErrUpstreamTimeout) || errors.Is(err, ErrUpstreamTimeout)
}

// maxBufferedEvents 限制首个内容事件前缓冲的非内容事件数量，防止异常上游无界增长。
const maxBufferedEvents = 64

// isContentEvent 判断事件是否携带上游实际产出内容。使用白名单判定：
// 未知事件一律视为非内容事件缓冲，避免新增状态/结构事件被误判为内容而锁定源。
func isContentEvent(e model.SSEEvent) bool {
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
		// tool_search_call 没有专门 delta 事件，arguments 只随 output_item.added/done 携带；
		// 因此该 item 类型的 added 事件按内容处理，避免合法工具响应被当空响应 failover。
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

// isBufferableEvent 判断事件是否应作为“空响应候选”缓冲：既非内容事件，也不是
// 明确携带错误/截断终态的事件（response.failed / response.incomplete 必须立即放行）。
// response.completed 无内容时仍缓冲，以便 run 级空响应 failover 判定。
func isBufferableEvent(e model.SSEEvent) bool {
	if isContentEvent(e) {
		return false
	}
	switch e.Type {
	case evResponseFailed, evResponseIncomplete:
		return false
	default:
		return true
	}
}

// EventGate 缓冲非内容事件（状态/终态），直到出现首个内容事件才 flush 给客户端。
// scheduler 在 a/c 后端使用，统一做空响应兜底：流结束时无内容事件，不锁定源，返回
// ErrEmptyResponse 让请求 failover 到下一个源。
// 解决上游返回 HTTP 200 + 空流（如 deepseek-v4-flash 对超大请求静默放弃）时
// scheduler 误判源锁定、无法 failover 的问题。r 透传后端不用，保持原语义。
// 非并发安全：Send/HasContent/SawTerminalFailure 必须由同一个流式扫描 goroutine 串行调用。
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

// Send 处理单个事件：非内容事件缓冲，首个内容事件或明确错误/截断终态触发 flush 后直发。
func (g *EventGate) Send(e model.SSEEvent) error {
	if g.flushed {
		return g.onEvent(e)
	}
	if isBufferableEvent(e) {
		if len(g.buf) >= maxBufferedEvents {
			return fmt.Errorf("event gate: %d buffered non-content events before content", len(g.buf))
		}
		g.buf = append(g.buf, e)
		return nil
	}
	// 首个内容事件或错误/截断终态：先 flush 缓冲，再发出当前事件。
	// 内容事件才算“已产出内容”；错误/截断终态只是透传给客户端，不能当作空响应兜底已解除。
	g.flushed = true
	if isContentEvent(e) {
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

// HasContent 报告是否已出现真实内容事件，用于 scheduler 决定是否锁定源。
func (g *EventGate) HasContent() bool { return g.content }

// SawTerminalFailure 报告是否已出现需要透传客户端的错误/截断终态事件。
func (g *EventGate) SawTerminalFailure() bool { return g.failure }
