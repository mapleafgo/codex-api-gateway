package openaichat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// TestSmokeExecuteChatConversion 验证 Chat 插件把 Chat SSE 转成 Responses SSE 并保持事件顺序。
func TestSmokeExecuteChatConversion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := New()
	var types []string
	err := p.Backend().Execute(
		context.Background(),
		[]byte(`{"model":"gpt-4o","input":"hello","stream":true}`),
		config.Source{Name: "c1", BaseURL: upstream.URL + "/v1", APIKey: "k", Backend: "openai-chat"},
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
