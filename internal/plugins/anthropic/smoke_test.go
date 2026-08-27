package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// TestSmokeExecuteAnthropicConversion 验证 Anthropic 插件把 Messages SSE 转成 Responses SSE。
func TestSmokeExecuteAnthropicConversion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-20250514\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	p := New()
	var types []string
	err := p.Backend().Execute(
		context.Background(),
		[]byte(`{"model":"claude-sonnet-4-20250514","input":"hi","stream":true}`),
		config.Source{Name: "a1", BaseURL: upstream.URL, APIKey: "k", Backend: "anthropic"},
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
	has := map[string]bool{}
	for _, tp := range types {
		has[tp] = true
	}
	if !has["response.created"] || !has["response.completed"] {
		t.Fatalf("missing response.created or response.completed in events %v", types)
	}
}
