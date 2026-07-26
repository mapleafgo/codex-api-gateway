package backend

import (
	"errors"
	"testing"
)

// TestIsClientErrorExcludesRateLimit 429/408 是传输可用性信号，
// 不得按"请求非法"处理（否则限流源永不降级、整轮不重试）。
func TestIsClientErrorExcludesRateLimit(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"upstream 400: bad request", true},
		{"upstream 401: unauthorized", true},
		{"anthropic upstream 404: not found", true},
		{"upstream 429: rate limited", false},
		{"upstream 408: timeout", false},
		{"upstream 500: boom", false},
	}
	for _, tc := range cases {
		if got := IsClientError(errors.New(tc.err)); got != tc.want {
			t.Errorf("IsClientError(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
