package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

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
		if sc := StatusCodeFromErr(scanErr); sc != 0 {
			code = sc
		}
		errText = errSummary(scanErr)
		return
	}
	if scanErr == nil {
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
func IsClientError(err error) bool {
	code := StatusCodeFromErr(err)
	if code == 429 || code == 408 {
		return false
	}
	return code >= 400 && code < 500
}
