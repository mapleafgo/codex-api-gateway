package backend

import (
	"context"
	"errors"
	"strings"
)

// isClientCanceled 判断 err 是否由请求 ctx 取消引起（客户端断开）。
// 首字节超时会取消子 ctx，但父 ctx 仍有效，故须同时检查父 ctx.Err()。
func isClientCanceled(ctx context.Context, err error) bool {
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
