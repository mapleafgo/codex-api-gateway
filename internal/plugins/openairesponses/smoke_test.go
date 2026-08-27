package openairesponses

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// TestSmokeExecutePassthrough 验证 Responses 插件把上游 Responses SSE 原样透传。
func TestSmokeExecutePassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()

	p := New()
	var types []string
	err := p.Backend().Execute(
		context.Background(),
		[]byte(`{"model":"gpt-4o","input":"hi","stream":true}`),
		config.Source{Name: "r1", BaseURL: upstream.URL + "/v1", APIKey: "k", Backend: "openai-responses"},
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
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !has[want] {
			t.Fatalf("missing %s in events %v", want, types)
		}
	}
}
