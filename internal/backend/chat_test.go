package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

func TestChatBackend_TextStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing bearer")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	b := NewChat()
	var types []string
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-4o","input":"hello","stream":true}`),
		config.Source{Name: "c1", BaseURL: ts.URL + "/v1", APIKey: "k", BackendType: "c"},
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
	for _, tpe := range types {
		has[tpe] = true
	}
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !has[want] {
			t.Fatalf("missing %s in %v", want, types)
		}
	}
}
