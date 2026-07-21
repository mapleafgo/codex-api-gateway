package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// TestAnthropicBackend_ReportsCacheUsage 回归：message_start 带 cache_read/create，
// message_delta 只刷新 output 时，观测事件仍应保留 cache_*。
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

// TestAnthropicBackend_SetEchoOnCompleted 确保 response.completed 带回 instructions 等 echo 字段。
func TestAnthropicBackend_SetEchoOnCompleted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"message_start","message":{"id":"m_echo","model":"claude"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
		io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":1}}`+"\n\n")
		io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer ts.Close()

	var completed map[string]any
	b := NewAnthropic()
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-5","instructions":"sys-echo","temperature":0.2,"input":"hi","stream":true}`),
		config.Source{Name: "a1", BaseURL: ts.URL, APIKey: "k", BackendType: "a"},
		&config.Config{},
		func(ev model.SSEEvent) error {
			if ev.Type == "response.completed" {
				_ = json.Unmarshal(ev.Data, &completed)
			}
			return nil
		},
		nil,
		1,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if completed == nil {
		t.Fatal("missing response.completed")
	}
	resp, _ := completed["response"].(map[string]any)
	if resp == nil {
		t.Fatalf("no response object: %v", completed)
	}
	if resp["instructions"] != "sys-echo" {
		t.Fatalf("instructions not echoed: %v", resp["instructions"])
	}
	// temperature may be json.Number
	if resp["temperature"] == nil {
		t.Fatalf("temperature not echoed: %+v", resp)
	}
}

// TestAnthropicBackend_MissingMessageStopStillCompletes 上游无 message_stop 时仍应补 completed。
func TestAnthropicBackend_MissingMessageStopStillCompletes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"message_start","message":{"id":"m_nostop","model":"claude"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
		io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":1}}`+"\n\n")
		// intentionally no message_stop
	}))
	defer ts.Close()

	var types []string
	b := NewAnthropic()
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-5","input":"hi","stream":true}`),
		config.Source{Name: "a1", BaseURL: ts.URL, APIKey: "k", BackendType: "a"},
		&config.Config{},
		func(ev model.SSEEvent) error {
			types = append(types, ev.Type)
			return nil
		},
		nil,
		1,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "response.completed") {
		t.Fatalf("expected completed after missing message_stop, types=%v", types)
	}
}

// TestAnthropicBackend_MidStreamEOFCode200 流已建立后 EOF：status=failed 且 code 保持 200（无上游 HTTP 码）。
func TestAnthropicBackend_MidStreamEOFCode200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"message_start","message":{"id":"m_eof","model":"claude"}}`+"\n\n")
		w.(http.Flusher).Flush()
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer ts.Close()

	var up UpstreamEvent
	var types []string
	b := NewAnthropic()
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-5","input":"hi","stream":true}`),
		config.Source{Name: "a1", BaseURL: ts.URL, APIKey: "k", BackendType: "a"},
		&config.Config{},
		func(ev model.SSEEvent) error { types = append(types, ev.Type); return nil },
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if err == nil {
		t.Fatal("expected error on mid-stream EOF")
	}
	if up.Status != "failed" {
		t.Fatalf("status=%q want failed", up.Status)
	}
	if up.Code != 200 {
		t.Fatalf("code=%d want 200 (stream established, no HTTP status in err)", up.Code)
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "response.failed") {
		t.Fatalf("expected response.failed, types=%v", types)
	}
}
