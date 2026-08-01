package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestScanEvents(t *testing.T) {
	body := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"
	var got []anthropic.MessageStreamEventUnion
	err := ScanEvents(context.Background(), strings.NewReader(body), func(ev *anthropic.MessageStreamEventUnion) error {
		got = append(got, *ev)
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].Delta.Text != "hi" {
		t.Fatalf("bad events: %+v", got)
	}
}

func TestScanEventsError(t *testing.T) {
	body := `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"
	var got []anthropic.MessageStreamEventUnion
	err := ScanEvents(context.Background(), strings.NewReader(body), func(ev *anthropic.MessageStreamEventUnion) error {
		got = append(got, *ev)
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].Type != "error" || got[0].Delta.Text != "Overloaded" {
		t.Fatalf("error event not parsed correctly: %+v", got)
	}
}

func TestScanEventsRejectsMalformedJSON(t *testing.T) {
	body := "data: {not-json}\n\n"
	err := ScanEvents(context.Background(), strings.NewReader(body), func(ev *anthropic.MessageStreamEventUnion) error {
		t.Fatalf("callback should not be called for malformed JSON: %+v", ev)
		return nil
	})
	if err == nil {
		t.Fatalf("expected malformed SSE JSON to return an error")
	}
}

// TestStreamAuthHeaders verifies both credential headers are sent, so the
// request authenticates against the official Anthropic API (x-api-key) and
// Anthropic-compatible gateways (Authorization: Bearer) alike.
func TestStreamAuthHeaders(t *testing.T) {
	var gotAPIKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rc, statusCode, err := New().Stream(context.Background(), srv.URL, "test-key-123", &anthropic.MessageNewParams{}, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	rc.Close()
	if statusCode != http.StatusOK {
		t.Fatalf("status=%d want %d", statusCode, http.StatusOK)
	}

	if gotAPIKey != "test-key-123" {
		t.Errorf("x-api-key = %q, want test-key-123", gotAPIKey)
	}
	if gotAuth != "Bearer test-key-123" {
		t.Errorf("Authorization = %q, want \"Bearer test-key-123\"", gotAuth)
	}
}

// TestInjectStream verifies stream:true is injected and that numeric fidelity
// (e.g. max_tokens) is preserved via json.Number (no float64 suffix).
func TestInjectStream(t *testing.T) {
	out, err := injectStream([]byte(`{"max_tokens":4096,"model":"glm-5.2"}`))
	if err != nil {
		t.Fatalf("injectStream: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := m["stream"].(bool); !ok || !v {
		t.Errorf("stream = %v, want true", m["stream"])
	}
	if !strings.Contains(string(out), `"max_tokens":4096`) {
		t.Errorf("max_tokens not preserved as integer literal: %s", out)
	}
}

// TestMessagesURL verifies base_url (gateway root, without the API path) gets
// /v1/messages appended, while an endpoint already ending in that path is left
// as-is so the suffix is never doubled.
func TestMessagesURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"https://open.bigmodel.cn/api/anthropic", "https://open.bigmodel.cn/api/anthropic/v1/messages"},
		{"https://api.anthropic.com/", "https://api.anthropic.com/v1/messages"},            // 尾部斜杠去重
		{"https://api.anthropic.com/v1/messages", "https://api.anthropic.com/v1/messages"}, // 已含后缀不重复
	}
	for _, c := range cases {
		if got := messagesURL(c.in); got != c.want {
			t.Errorf("messagesURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStreamExtendedCacheTTLBetaOn1h 验证 cache_control TTL=1h 时
// anthropic-beta 自动包含 extended-cache-ttl-2025-04-11。
func TestStreamExtendedCacheTTLBetaOn1h(t *testing.T) {
	var gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req := &anthropic.MessageNewParams{
		CacheControl: anthropic.CacheControlEphemeralParam{
			Type: "ephemeral",
			TTL:  anthropic.CacheControlEphemeralTTLTTL1h,
		},
	}
	rc, _, err := New().Stream(context.Background(), srv.URL, "k", req, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	rc.Close()
	if !strings.Contains(gotBeta, ExtendedCacheTTLBetaHeader) {
		t.Fatalf("anthropic-beta missing extended-cache-ttl: %q", gotBeta)
	}
}

// TestStreamNoExtendedCacheTTLOn5m 验证 5m 路径不带 extended-cache-ttl beta。
func TestStreamNoExtendedCacheTTLOn5m(t *testing.T) {
	var gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req := &anthropic.MessageNewParams{
		CacheControl: anthropic.CacheControlEphemeralParam{
			Type: "ephemeral",
			TTL:  anthropic.CacheControlEphemeralTTLTTL5m,
		},
	}
	rc, _, err := New().Stream(context.Background(), srv.URL, "k", req, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	rc.Close()
	if strings.Contains(gotBeta, ExtendedCacheTTLBetaHeader) {
		t.Fatalf("5m must not set extended-cache-ttl: %q", gotBeta)
	}
}
