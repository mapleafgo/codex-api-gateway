package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

func TestPrepareUpstreamBody_ModelMapAndStream(t *testing.T) {
	src := config.Source{
		ModelMap:     map[string]string{"gpt-5": "o3"},
		DefaultModel: "fallback",
	}
	raw := []byte(`{"model":"gpt-5","stream":false,"input":"hi","foo":{"bar":1}}`)
	body, client, resolved, err := PrepareUpstreamBody(raw, &src)
	if err != nil {
		t.Fatal(err)
	}
	if client != "gpt-5" || resolved != "o3" {
		t.Fatalf("client=%s resolved=%s", client, resolved)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "o3" {
		t.Fatalf("model=%v", m["model"])
	}
	if m["stream"] != true {
		t.Fatalf("stream=%v", m["stream"])
	}
	if _, ok := m["foo"]; !ok {
		t.Fatal("lost foo")
	}
}

func TestRewriteClientModel_T2(t *testing.T) {
	in := []byte(`{"type":"response.completed","response":{"id":"r1","model":"o3","usage":{"input_tokens":1,"output_tokens":2}}}`)
	out := rewriteClientModel(in, "gpt-5")
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	resp := m["response"].(map[string]any)
	if resp["model"] != "gpt-5" {
		t.Fatalf("model=%v", resp["model"])
	}
	if resp["id"] != "r1" {
		t.Fatal("id changed")
	}
}

func TestResponsesBackend_EmptyStreamNoSynthetic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		// 无 data 帧
	}))
	defer ts.Close()
	b := NewResponses()
	var events int
	err := b.Execute(context.Background(),
		[]byte(`{"model":"m","input":[]}`),
		config.Source{Name: "r1", BaseURL: ts.URL + "/v1", APIKey: "k", BackendType: "r"},
		nil,
		func(e model.SSEEvent) error { events++; return nil },
		func(ev UpstreamEvent) {
			if ev.BackendType != config.BackendOpenAIResponses {
				t.Fatalf("bt=%s", ev.BackendType)
			}
			if ev.Status != "failed" {
				t.Fatalf("status=%s", ev.Status)
			}
		},
		1,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if events != 0 {
		t.Fatalf("synthetic events=%d", events)
	}
}

func TestResponsesBackend_PassthroughSSE(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\n")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_u","model":"o3"}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_u","model":"o3","usage":{"input_tokens":3,"output_tokens":4}}}`+"\n\n")
	}))
	defer ts.Close()
	b := NewResponses()
	var got []model.SSEEvent
	var up UpstreamEvent
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-5","input":[]}`),
		config.Source{Name: "r1", BaseURL: ts.URL + "/v1", APIKey: "k",
			ModelMap: map[string]string{"gpt-5": "o3"}},
		nil,
		func(e model.SSEEvent) error { got = append(got, e); return nil },
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("events=%d", len(got))
	}
	// T2: completed 内 model 应为 gpt-5
	if !bytes.Contains(got[len(got)-1].Data, []byte(`"model":"gpt-5"`)) {
		t.Fatalf("data=%s", got[len(got)-1].Data)
	}
	if up.InputTokens != 3 || up.OutputTokens != 4 {
		t.Fatalf("tokens in=%d out=%d", up.InputTokens, up.OutputTokens)
	}
	if up.Status != "completed" || up.BackendType != "r" {
		t.Fatalf("up=%+v", up)
	}
}

func TestResponsesBackend_CancelAfterTerminalIsCompleted(t *testing.T) {
	released := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"r1","model":"o3","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
		fl.Flush()
		// 保持连接，直到客户端取消或测试结束
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	defer ts.Close()
	defer close(released)

	b := NewResponses()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var up UpstreamEvent
	var sawEvent bool
	err := b.Execute(ctx,
		[]byte(`{"model":"gpt-5","input":[]}`),
		config.Source{Name: "r1", BaseURL: ts.URL + "/v1", APIKey: "k",
			ModelMap: map[string]string{"gpt-5": "o3"}},
		nil,
		func(e model.SSEEvent) error {
			sawEvent = true
			cancel()
			// 返回 ctx 错误，保证 isClientCanceled 可识别（不依赖 body 关闭形态）
			return ctx.Err()
		},
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if !sawEvent {
		t.Fatal("expected at least one event")
	}
	// 可能返回 ctx 取消错误；Upstream 状态必须 completed
	_ = err
	if up.Status != "completed" {
		t.Fatalf("status=%s want completed (err=%v)", up.Status, err)
	}
	if up.BackendType != "r" {
		t.Fatalf("backend_type=%s", up.BackendType)
	}
}
