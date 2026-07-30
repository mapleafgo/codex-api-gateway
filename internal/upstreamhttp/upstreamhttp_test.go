package upstreamhttp

import (
	"io"
	"log/slog"
	"net/http"
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

func TestApplyHeadersCustomAndReserved(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "k")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	headers := map[string]string{
		"X-Custom":          "v1",
		"x-custom-second":   "v2",
		"Authorization":     "Hijack",
		"Content-Type":      "hijack",
		"x-api-key":         "hijack",
		"anthropic-version": "hijack",
		"anthropic-beta":    "hijack",
		"Accept":            "hijack",
	}
	ApplyHeaders(req, headers, logger, "s1")
	if req.Header.Get("X-Custom") != "v1" || req.Header.Get("X-Custom-Second") != "v2" {
		t.Fatalf("custom headers not set: %v", req.Header)
	}
	if req.Header.Get("Authorization") != "Bearer secret" {
		t.Fatalf("Authorization overwritten: %s", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type overwritten: %s", req.Header.Get("Content-Type"))
	}
	if req.Header.Get("x-api-key") != "k" {
		t.Fatalf("x-api-key overwritten: %s", req.Header.Get("x-api-key"))
	}
	if req.Header.Get("anthropic-version") != "" || req.Header.Get("anthropic-beta") != "" {
		t.Fatalf("reserved headers set: %v", req.Header)
	}
	if req.Header.Get("Accept") != "" {
		t.Fatalf("Accept overwritten: %s", req.Header.Get("Accept"))
	}
}

func TestApplyHeadersNil(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	ApplyHeaders(req, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "s1")
	if len(req.Header) != 0 {
		t.Fatalf("expected no headers: %v", req.Header)
	}
}
