package plugin

import (
	"errors"
	"testing"
)

// TestStatusCodeFromErrFindsCodeLaterInMessage 全源失败包装串里第一个上游
// 前缀可能没有状态码（all upstream sources failed），必须继续扫描后面的
// "upstream 200" 才能归因 HTTP 200。
func TestStatusCodeFromErrFindsCodeLaterInMessage(t *testing.T) {
	got := StatusCodeFromErr(errors.New("all upstream sources failed (last: upstream 200: quota exceeded)"))
	if got != 200 {
		t.Fatalf("StatusCodeFromErr = %d, want 200", got)
	}
}
