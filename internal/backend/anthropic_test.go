package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// TestAnthropicBackend_ReportsCacheUsage 回归：message_start 带 cache_read/create，
// message_delta 只刷新 output 时，观测事件仍应保留 cache_*（与旧 scheduler.mergeUsage 一致）。
func TestAnthropicBackend_ReportsCacheUsage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"message_start","message":{"id":"m1","model":"claude","usage":{"input_tokens":100,"output_tokens":0,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
		io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":100,"output_tokens":5}}`+"\n\n")
		io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer ts.Close()

	b := NewAnthropic()
	var up UpstreamEvent
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-5","input":"hi","stream":true}`),
		config.Source{Name: "a1", BaseURL: ts.URL, APIKey: "k", BackendType: "a"},
		&config.Config{},
		func(ev model.SSEEvent) error { return nil },
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if up.CacheRead != 80 || up.CacheCreate != 20 {
		t.Fatalf("cache usage lost: read=%d create=%d (want 80/20); full=%+v", up.CacheRead, up.CacheCreate, up)
	}
	if up.InputTokens != 100 || up.OutputTokens != 5 {
		t.Fatalf("token usage: in=%d out=%d", up.InputTokens, up.OutputTokens)
	}
}
