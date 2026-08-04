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
	body, client, resolved, err := PrepareUpstreamBody(raw, &src, nil)
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

func TestPrepareUpstreamBody_PreservesLargeNumbers(t *testing.T) {
	src := config.Source{
		ModelMap: map[string]string{"gpt-5": "o3"},
	}
	raw := []byte(`{"model":"gpt-5","stream":false,"max_output_tokens":9223372036854775807}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`9223372036854775807`)) {
		t.Fatalf("large integer corrupted by float64 re-marshal: %s", body)
	}
}

// TestPrepareUpstreamBody_ReasoningSummaryToContent 复现 DeepSeek /responses 400：
// Codex 发送 OpenAI 标准 reasoning item（summary 形式），而 DeepSeek 只支持
// plain-text content 并把它合并进相邻 assistant message，summary 会被忽略导致
// "reasoning_text must be passed back"。网关透传时应把 summary 文本折算进
// content（reasoning_text part），保留原始字段只做协议槽位对齐。
func TestPrepareUpstreamBody_ReasoningSummaryToContent(t *testing.T) {
	src := config.Source{ModelMap: map[string]string{"gpt-5": "deepseek-v4-flash"}}
	raw := []byte(`{
		"model":"gpt-5",
		"input":[
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"need tool"},{"type":"summary_text","text":"keep going"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},
			{"type":"function_call","id":"fc1","call_id":"call_1","name":"get_logs","arguments":"{}"}
		]
	}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Input) == 0 {
		t.Fatal("input lost")
	}
	reasoning := m.Input[0]
	if reasoning["type"] != "reasoning" {
		t.Fatalf("first item type=%v", reasoning["type"])
	}
	content, ok := reasoning["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("reasoning content missing: %v", reasoning["content"])
	}
	first := content[0].(map[string]any)
	if first["type"] != "reasoning_text" || first["text"] != "need tool" {
		t.Fatalf("unexpected content part: %v", first)
	}
}

// TestPrepareUpstreamBody_ReasoningContentStringNotOverwritten content 已是 plain-text
// string 时不应被 summary 折算覆盖。
func TestPrepareUpstreamBody_ReasoningContentStringNotOverwritten(t *testing.T) {
	src := config.Source{ModelMap: map[string]string{"gpt-5": "deepseek-v4-flash"}}
	raw := []byte(`{
		"model":"gpt-5",
		"input":[
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"summary text"}],"content":"existing plain text"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
		]
	}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if got := m.Input[0]["content"]; got != "existing plain text" {
		t.Fatalf("content overwritten: %v", got)
	}
}

// TestPrepareUpstreamBody_ReasoningEmptySummaryUntouched summary 为空时不补 content。
func TestPrepareUpstreamBody_ReasoningEmptySummaryUntouched(t *testing.T) {
	src := config.Source{ModelMap: map[string]string{"gpt-5": "deepseek-v4-flash"}}
	raw := []byte(`{
		"model":"gpt-5",
		"input":[
			{"type":"reasoning","id":"r1","summary":[]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
		]
	}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Input[0]["content"]; ok {
		t.Fatalf("empty summary must not add content: %v", m.Input[0]["content"])
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

func TestRewriteClientModel_PreservesLargeNumbers(t *testing.T) {
	in := []byte(`{"type":"response.completed","response":{"id":"r1","model":"o3","created_at":1750000000123456789,"usage":{"input_tokens":9223372036854775807}}}`)
	out := rewriteClientModel(in, "gpt-5")
	for _, want := range []string{`"model":"gpt-5"`, `1750000000123456789`, `9223372036854775807`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("rewriteClientModel 损坏数据，缺少 %s: %s", want, out)
		}
	}
}

func TestParseUsageFromEvent_CacheWriteTokens(t *testing.T) {
	data := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":30}}}}`)
	inTok, outTok, cacheRead, cacheCreate, ok := parseUsageFromEvent("response.completed", data)
	if !ok {
		t.Fatal("usage not parsed")
	}
	if inTok != 100 || outTok != 10 || cacheRead != 60 || cacheCreate != 30 {
		t.Fatalf("usage=%d/%d/%d/%d want 100/10/60/30", inTok, outTok, cacheRead, cacheCreate)
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

func TestResponsesBackend_FailedTerminalIsFailed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.failed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"id":"r1","model":"o3","error":{"message":"quota exceeded"},"usage":{"input_tokens":3,"output_tokens":1}}}`+"\n\n")
	}))
	defer ts.Close()

	b := NewResponses()
	var up UpstreamEvent
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-5","input":[]}`),
		config.Source{Name: "r1", BaseURL: ts.URL + "/v1", APIKey: "k"},
		nil,
		func(model.SSEEvent) error { return nil },
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if up.Status != "failed" || up.Code != http.StatusOK {
		t.Fatalf("status=%s code=%d want failed/200", up.Status, up.Code)
	}
	if up.Error != "quota exceeded" {
		t.Fatalf("error=%q want quota exceeded", up.Error)
	}
	if up.InputTokens != 3 || up.OutputTokens != 1 {
		t.Fatalf("usage=%d/%d want 3/1", up.InputTokens, up.OutputTokens)
	}
}

func TestResponsesBackend_IncompleteTerminalIsIncomplete(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.incomplete\n")
		_, _ = io.WriteString(w, `data: {"type":"response.incomplete","response":{"id":"r1","model":"o3","usage":{"input_tokens":4,"output_tokens":2}}}`+"\n\n")
	}))
	defer ts.Close()

	b := NewResponses()
	var up UpstreamEvent
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-5","input":[]}`),
		config.Source{Name: "r1", BaseURL: ts.URL + "/v1", APIKey: "k"},
		nil,
		func(model.SSEEvent) error { return nil },
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if up.Status != "incomplete" || up.InputTokens != 4 || up.OutputTokens != 2 {
		t.Fatalf("up=%+v want incomplete with usage 4/2", up)
	}
}

func TestResponsesBackend_TruncatedStreamWithoutTerminalIsFailed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		// 只发部分事件后直接关流，不给任何终态事件
		_, _ = io.WriteString(w, "event: response.created\n")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"r1","model":"o3"}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n")
	}))
	defer ts.Close()

	b := NewResponses()
	var up UpstreamEvent
	var events int
	err := b.Execute(context.Background(),
		[]byte(`{"model":"gpt-5","input":[]}`),
		config.Source{Name: "r1", BaseURL: ts.URL + "/v1", APIKey: "k"},
		nil,
		func(model.SSEEvent) error { events++; return nil },
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if up.Status != "failed" {
		t.Fatalf("status=%s want failed for truncated stream", up.Status)
	}
	if up.Error != "upstream stream ended without terminal event" {
		t.Fatalf("error=%q", up.Error)
	}
	// 不代补终态：客户端只应收到上游实际发出的 2 个事件
	if events != 2 {
		t.Fatalf("events=%d want 2 (no synthetic terminal)", events)
	}
}

func TestResponsesBackend_CancelAfterFailedTerminalIsFailed(t *testing.T) {
	released := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.failed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"id":"r1","model":"o3","error":{"message":"failed upstream"}}}`+"\n\n")
		fl.Flush()
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	defer ts.Close()
	defer close(released)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewResponses()
	var up UpstreamEvent
	_ = b.Execute(ctx,
		[]byte(`{"model":"gpt-5","input":[]}`),
		config.Source{Name: "r1", BaseURL: ts.URL + "/v1", APIKey: "k"},
		nil,
		func(model.SSEEvent) error {
			cancel()
			return ctx.Err()
		},
		func(ev UpstreamEvent) { up = ev },
		1,
	)
	if up.Status != "failed" || up.Error != "failed upstream" {
		t.Fatalf("up=%+v want failed terminal after cancel", up)
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
