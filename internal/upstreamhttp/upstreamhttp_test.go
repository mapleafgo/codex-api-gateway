package upstreamhttp

import (
	"strings"
	"testing"
)

func TestEndpointURL(t *testing.T) {
	cases := []struct {
		base, suffix, want string
	}{
		{"https://api.openai.com/v1", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://x/v1/responses", "/responses", "https://x/v1/responses"},
		{"https://x/v1/responses/", "/responses", "https://x/v1/responses"},
		{"https://api.openai.com/v1", "/models", "https://api.openai.com/v1/models"},
		{"https://api.openai.com/v1/models", "/models", "https://api.openai.com/v1/models"},
	}
	for _, tc := range cases {
		if got := EndpointURL(tc.base, tc.suffix); got != tc.want {
			t.Errorf("EndpointURL(%q, %q) = %q, want %q", tc.base, tc.suffix, got, tc.want)
		}
	}
}

func TestTruncForLog(t *testing.T) {
	if got := TruncForLog([]byte("short"), 500); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("x", 600)
	got := TruncForLog([]byte(long), 500)
	if !strings.HasPrefix(got, strings.Repeat("x", 500)) || !strings.Contains(got, "+100 bytes") {
		t.Fatalf("got %q", got)
	}
}

func TestNewSSEScannerLargeLine(t *testing.T) {
	// 单行超过 bufio.Scanner 默认 64KiB 上限时不得断流。
	line := "data: " + strings.Repeat("x", 200*1024)
	sc := NewSSEScanner(strings.NewReader(line + "\n"))
	if !sc.Scan() {
		t.Fatalf("scan failed: %v", sc.Err())
	}
	if sc.Text() != line {
		t.Fatal("line truncated")
	}
}
