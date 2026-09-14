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
		config.Source{Name: "c1", BaseURL: ts.URL + "/v1", APIKey: "k", Backend: "openai-chat"},
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

// TestChatBackend_CacheReadPropagated 复现：Chat 上游 usage 含 cached_tokens 时，
// onUpstream.CacheRead 必须非 0（此前 chat.go 只填 Input/Output，metrics 缓存命中恒空）。
func TestChatBackend_CacheReadPropagated(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-c\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-c\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"total_tokens\":110,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	b := NewChat()
	var up UpstreamEvent
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-4o","input":"hello","stream":true}`),
		config.Source{Name: "c1", BaseURL: ts.URL + "/v1", APIKey: "k", Backend: "openai-chat"},
		&config.Config{},
		func(model.SSEEvent) error { return nil },
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if up.Status != "completed" {
		t.Fatalf("status=%q", up.Status)
	}
	if up.InputTokens != 100 || up.OutputTokens != 10 {
		t.Fatalf("tokens in=%d out=%d", up.InputTokens, up.OutputTokens)
	}
	if up.CacheRead != 80 {
		t.Fatalf("CacheRead=%d want 80", up.CacheRead)
	}
	if up.CacheCreate != 0 {
		t.Fatalf("CacheCreate=%d want 0 (Chat 无 creation 字段)", up.CacheCreate)
	}
}

// TestChatBackend_ContentFilterIncomplete 复现：上游 finish_reason=content_filter 时，
// onUpstream.Status 必须是语义态 incomplete（此前 chat.go 硬编码 completed，导致
// 日志/metrics 记 completed，而客户端实际收到 response.incomplete）。
func TestChatBackend_ContentFilterIncomplete(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-cf\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-cf\",\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	b := NewChat()
	var types []string
	var up UpstreamEvent
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-4o","input":"hello","stream":true}`),
		config.Source{Name: "c1", BaseURL: ts.URL + "/v1", APIKey: "k", Backend: "openai-chat"},
		&config.Config{},
		func(ev model.SSEEvent) error {
			types = append(types, ev.Type)
			return nil
		},
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	has := map[string]bool{}
	for _, tpe := range types {
		has[tpe] = true
	}
	if !has["response.incomplete"] {
		t.Fatalf("want response.incomplete, got %v", types)
	}
	if has["response.completed"] {
		t.Fatalf("unexpected response.completed in %v", types)
	}
	if up.Status != model.ResponseStatusIncomplete {
		t.Fatalf("onUpstream status=%q want %q", up.Status, model.ResponseStatusIncomplete)
	}
}

// TestChatBackend_EmptyStreamNoSyntheticLock 复现 OpenCode/OpenRouter 空流：
// 仅 SSE 注释或仅 [DONE] 时，不得合成 response.created/completed 锁定源。
func TestChatBackend_EmptyStreamNoSyntheticLock(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"comments_only", ": OPENROUTER PROCESSING\n\n: OPENROUTER PROCESSING\n\n"},
		{"done_only", "data: [DONE]\n\n"},
		{"comments_then_done", ": OPENROUTER PROCESSING\n\ndata: [DONE]\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.body)
			}))
			t.Cleanup(ts.Close)

			b := NewChat()
			var types []string
			err := b.Execute(context.Background(),
				[]byte(`{"model":"gpt-4o","input":"hello","stream":true}`),
				config.Source{Name: "opencode", BaseURL: ts.URL + "/v1", APIKey: "k", Backend: "openai-chat"},
				&config.Config{},
				func(ev model.SSEEvent) error {
					types = append(types, ev.Type)
					return nil
				},
				nil,
				1,
			)
			if err == nil {
				t.Fatal("want error for empty upstream stream")
			}
			if !strings.Contains(err.Error(), "upstream returned no events") {
				t.Fatalf("err=%v", err)
			}
			if len(types) != 0 {
				t.Fatalf("must not emit synthetic SSE events, got %v", types)
			}
		})
	}
}

// TestChatBackend_MultiLineDataFrame 复现「chat 输出不完整」的一个根因：
// 上游按 SSE 规范把一个事件的 JSON 拆到多条 data: 行（接收端应以 "\n" 拼接）。
// 逐行单独解析会把合法 JSON 当截断，该事件内容丢失并报 bad chunk。
func TestChatBackend_MultiLineDataFrame(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-ml\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"")
		io.WriteString(w, ",\"content\":\"line1\\n\\\"quoted\\\" line2\"}}]}\n\n")
		// 这一事件跨两行 data:（换行落在 token 边界，JSON 允许 token 间空白）。
		io.WriteString(w, "data: {\"id\":\"chatcmpl-ml\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"}\n")
		io.WriteString(w, "data: }]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-ml\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	b := NewChat()
	var got strings.Builder
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-4o","input":"hello","stream":true}`),
		config.Source{Name: "ml", BaseURL: ts.URL + "/v1", APIKey: "k", Backend: "openai-chat"},
		&config.Config{},
		func(ev model.SSEEvent) error {
			if ev.Type != "response.output_text.delta" {
				return nil
			}
			var d model.OutputTextDeltaEvent
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return err
			}
			got.WriteString(d.Delta)
			return nil
		},
		nil,
		1,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "line1\n\"quoted\" line2second"
	if got.String() != want {
		t.Fatalf("多行 data 帧内容不完整：got %q, want %q", got.String(), want)
	}
}

// TestChatBackend_ReasoningOnlyThenContent 确保仅含 content 的正常流仍锁定并完成。
func TestChatBackend_CommentThenContent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": OPENROUTER PROCESSING\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	b := NewChat()
	var types []string
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-4o","input":"hello","stream":true}`),
		config.Source{Name: "c1", BaseURL: ts.URL + "/v1", APIKey: "k", Backend: "openai-chat"},
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
