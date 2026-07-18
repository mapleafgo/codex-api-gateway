package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

func TestShouldSummarizeReasoning(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "empty", body: `{"model":"gpt-5","input":"hi"}`, want: false},
		{name: "none", body: `{"model":"gpt-5","input":"hi","reasoning":{"effort":"none"}}`, want: false},
		{name: "concise", body: `{"model":"gpt-5","input":"hi","reasoning":{"summary":"concise"}}`, want: true},
		{name: "effort_without_summary", body: `{"model":"gpt-5","input":"hi","reasoning":{"effort":"medium"}}`, want: false},
		{name: "effort_plus_concise", body: `{"model":"gpt-5","input":"hi","reasoning":{"effort":"medium","summary":"concise"}}`, want: true},
		{name: "effort_none_concise", body: `{"model":"gpt-5","input":"hi","reasoning":{"effort":"none","summary":"concise"}}`, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req oairesponses.ResponseNewParams
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := shouldSummarizeReasoning(&req); got != tc.want {
				t.Fatalf("shouldSummarizeReasoning() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResponsesEndpointStreamsSSE(t *testing.T) {
	// fake upstream that emits one text delta
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "response.created") {
		t.Fatalf("missing response.created: %s", body)
	}
	if !strings.Contains(string(body), "response.output_text.delta") {
		t.Fatalf("missing output_text.delta: %s", body)
	}
	if !strings.Contains(string(body), "response.completed") {
		t.Fatalf("missing response.completed: %s", body)
	}
}

func TestResponsesRequestLogIncludesStorageDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_log\",\"model\":\"claude\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","input":[{"type":"reasoning","id":"rs_0","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"sigZDR"},{"type":"message","role":"user","content":[{"type":"input_text","text":"search x"}]},{"type":"function_call","call_id":"c1","name":"search","arguments":"{\"q\":\"x\"}"},{"type":"function_call_output","call_id":"c1","output":"result-x"}],"tools":[{"type":"function","name":"search","parameters":{"type":"object"}}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	got := logs.String()
	for _, want := range []string{
		`"previous_response_id_present":false`,
		`"store_explicit":false`,
		`"store_effective":true`,
		`"input_item_type_counts"`,
		`"message":1`,
		`"reasoning":1`,
		`"function_call":1`,
		`"function_call_output":1`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("request log missing %s. logs:\n%s", want, got)
		}
	}
}

func TestResponsesStoreFalseSkipsSessionSave(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_store_false\",\"model\":\"claude\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: response.completed\n") {
		t.Fatalf("missing response.completed: %s", body)
	}
	if !strings.Contains(string(body), `"store":false`) {
		t.Fatalf("response should echo store=false. body:\n%s", body)
	}
	if _, ok := srv.sess.Get("m_store_false"); ok {
		t.Fatalf("store=false must not save response output in session store. body:\n%s", body)
	}
}

func TestResponsesStoreFalseSkipsPreviousResponseEnrich(t *testing.T) {
	requests := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- string(body)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_no_enrich\",\"model\":\"claude\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"fresh\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	srv.sess.Save("resp_cached", "up", []model.OutputItem{
		{
			Type:    "message",
			Role:    "assistant",
			Content: []model.OutputText{{Type: "output_text", Text: "cached-answer"}},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","store":false,"previous_response_id":"resp_cached","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"new question"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: response.completed\n") {
		t.Fatalf("missing response.completed: %s", body)
	}

	select {
	case upstreamBody := <-requests:
		if strings.Contains(upstreamBody, "cached-answer") {
			t.Fatalf("store=false must not enrich previous_response_id into upstream request: %s", upstreamBody)
		}
	case <-time.After(time.Second):
		t.Fatalf("upstream did not receive request")
	}
}

func TestResponsesPreviousResponseIDReplaysInputAndOutputContext(t *testing.T) {
	requests := make(chan string, 2)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- string(body)
		id := "m_context1"
		text := "Paris"
		if calls.Add(1) == 2 {
			id = "m_context2"
			text = "About 2.1 million"
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `data: {"type":"message_start","message":{"id":"`+id+`","model":"claude"}}`+"\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`+"\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp1, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the capital of France?"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("round1 post: %v", err)
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	<-requests

	resp2, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","previous_response_id":"m_context1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"And its population?"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("round2 post: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	select {
	case upstreamBody := <-requests:
		for _, want := range []string{"What is the capital of France?", "Paris", "And its population?"} {
			if !strings.Contains(upstreamBody, want) {
				t.Fatalf("round2 upstream request missing %q: %s", want, upstreamBody)
			}
		}
	case <-time.After(time.Second):
		t.Fatalf("upstream did not receive round2 request")
	}
}

// TestResponsesCompletedEmittedOnce proves the C1 fix end-to-end: the backend
// already sends message_stop and the handler also feeds a trailing synthetic
// message_stop, yet response.completed must appear EXACTLY once in the output.
// The count (not Contains) is what would have caught the original bug.
func TestResponsesCompletedEmittedOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"type\":\"message_delta\",\"stop_reason\":\"end_turn\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Count actual SSE event lines — NOT the raw substring, which would also
	// match the "type":"response.completed" field inside the JSON data.
	if got := strings.Count(string(body), "event: response.completed\n"); got != 1 {
		t.Fatalf("expected SSE event response.completed exactly once, got %d. body:\n%s", got, body)
	}
	// A well-formed error path must not have fired.
	if strings.Contains(string(body), "event: response.failed\n") {
		t.Fatalf("unexpected response.failed on success path. body:\n%s", body)
	}
	// The completion must carry the upstream id, not a synthetic resp_<nano>.
	if !strings.Contains(string(body), `"id":"m1"`) {
		t.Fatalf("expected response.completed to carry upstream id m1. body:\n%s", body)
	}
}

// TestModelsEndpointBaseInstructionsFromConfig 锁定 base_instructions_file 加载链路：
// config.yaml 配置 base_instructions_file 后，Load 读取文件内容写入 cfg.BaseInstructions，
// /v1/models 返回的每个 ModelInfo.base_instructions 应等于文件内容。
// 取代旧 TestResponsesAppendsGlobalSystemSuffix：base_instructions 经客户端注入，
// 不再在转换层追加 system block。
func TestModelsEndpointBaseInstructionsFromConfig(t *testing.T) {
	const content = "You are a gateway-test agent. <gateway_guidance>read SKILL.md first</gateway_guidance>"
	dir := t.TempDir()
	biPath := filepath.Join(dir, "base_instructions.md")
	if err := os.WriteFile(biPath, []byte(content), 0644); err != nil {
		t.Fatalf("write base_instructions file: %v", err)
	}
	ctxWindow := int64(200000)
	cfg := &config.Config{
		BaseInstructions: content,
		Breaker:          config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources:          []config.Source{{Name: "up", BaseURL: "http://127.0.0.1:0"}},
		ModelOverrides: map[string]config.ModelOverride{
			"gpt-5": {ContextWindow: &ctxWindow},
		},
	}
	srv := New(cfg)
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}
	var raw struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v. body: %s", err, body)
	}
	if len(raw.Models) == 0 {
		t.Fatalf("no models returned. body: %s", body)
	}
	for i, m := range raw.Models {
		bi, ok := m["base_instructions"]
		if !ok {
			t.Fatalf("model[%d] 缺少 base_instructions 字段", i)
		}
		var got string
		if err := json.Unmarshal(bi, &got); err != nil {
			t.Fatalf("model[%d] base_instructions 反序列化失败: %v. raw=%s", i, err, string(bi))
		}
		if got != content {
			t.Errorf("model[%d] base_instructions 不匹配 config 加载内容. got len=%d want len=%d", i, len(got), len(content))
		}
	}
}

// TestResponsesErrorPathEmitsFailedNotCompleted proves I1+I2: when every
// upstream errors (here: the only source points at a server that returns
// 500), the handler emits a response.failed event with a non-empty SSE event
// type and does NOT emit response.completed.
func TestResponsesErrorPathEmitsFailedNotCompleted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "event: response.failed\n") {
		t.Fatalf("expected SSE event type 'response.failed', got:\n%s", body)
	}
	if strings.Contains(string(body), "response.completed") {
		t.Fatalf("response.completed must NOT appear on error path. body:\n%s", body)
	}
	// I2: the SSE event line must not be the empty "event: \n" form.
	if strings.Contains(string(body), "event: \n") {
		t.Fatalf("found empty SSE event type. body:\n%s", body)
	}
}

// TestResponsesMidStreamErrorNoDoubleFailed (I1): when the upstream sends an
// error event mid-stream followed by a connection reset, the gateway must
// emit exactly ONE response.failed — not two. Without the Done() guard in
// server.go, the converter's response.failed (from the mid-stream error event)
// would be followed by a second server-side response.failed (from the read error).
func TestResponsesMidStreamErrorNoDoubleFailed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		io.WriteString(w, `data: {"type":"message_start","message":{"id":"m1","model":"claude"}}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n\n")
		f.Flush()
		// Give the gateway time to read and process both events before resetting.
		time.Sleep(100 * time.Millisecond)
		// Force a TCP RST (SetLinger(0)) so the scanner returns a non-nil error,
		// simulating a connection reset after a mid-stream error event.
		hj := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetLinger(0)
		}
		conn.Close()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	count := strings.Count(string(body), "event: response.failed\n")
	if count != 1 {
		t.Fatalf("expected exactly 1 response.failed (I1: double-failed guard), got %d. body:\n%s", count, body)
	}
}

// TestResponsesServerFailureIsNotPersisted 验证服务端失败响应不能通过 previous_response_id 回放。
func TestResponsesServerFailureIsNotPersisted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Extract response.id from the response.failed event.
	var failedID string
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var m map[string]any
		if json.Unmarshal([]byte(payload), &m) != nil {
			continue
		}
		if m["type"] != "response.failed" {
			continue
		}
		if r, ok := m["response"].(map[string]any); ok {
			if id, ok := r["id"].(string); ok {
				failedID = id
			}
		}
	}
	if failedID == "" {
		t.Fatalf("could not extract response.id from response.failed event. body:\n%s", body)
	}

	if _, ok := srv.sess.Get(failedID); ok {
		t.Fatalf("server-side failed response %q must not be persisted. body:\n%s", failedID, body)
	}
}

func TestModelsEndpointReturnsOnlyConfigured(t *testing.T) {
	// /v1/models 只应返回 config.yaml models.<slug> 显式配置的模型，
	// 既不拉取上游列表，也不暴露 model_map 中的别名。
	ctxWindow := int64(200000)
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{
			{Name: "up", BaseURL: "http://127.0.0.1:0", ModelMap: map[string]string{
				"gpt-5":   "claude-sonnet-4-20250514", // model_map 别名，不应出现在 /v1/models
				"gpt-5.5": "claude-opus-4-20250514",
			}},
		},
		ModelOverrides: map[string]config.ModelOverride{
			"gpt-5":   {ContextWindow: &ctxWindow},
			"gpt-5.5": {ContextWindow: &ctxWindow},
		},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}
	var ml model.CodexModelsResponse
	if err := json.Unmarshal(body, &ml); err != nil {
		t.Fatalf("unmarshal: %v. body: %s", err, body)
	}
	// 仅返回 ModelOverrides 配置的 2 个 slug，不含 model_map 映射的真实模型
	ids := make(map[string]bool)
	for _, m := range ml.Models {
		ids[m.Slug] = true
		if !m.SupportsSearchTool {
			t.Fatalf("model %q supports_search_tool = false, want true", m.Slug)
		}
	}
	for _, want := range []string{"gpt-5", "gpt-5.5"} {
		if !ids[want] {
			t.Fatalf("missing model %q in response: %+v", want, ml.Models)
		}
	}
	for _, forbidden := range []string{"claude-sonnet-4-20250514", "claude-opus-4-20250514"} {
		if ids[forbidden] {
			t.Fatalf("model_map 真实模型 %q 不应出现在 /v1/models: %+v", forbidden, ml.Models)
		}
	}
	if len(ml.Models) != 2 {
		t.Fatalf("expected 2 configured models, got %d: %+v", len(ml.Models), ml.Models)
	}
}

func TestModelsEndpointEmptyWhenNoConfigured(t *testing.T) {
	// 未配置 models.<slug> 时，/v1/models 返回空列表（不拉上游、不暴露 model_map 别名）
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(100 * time.Millisecond)},
		Sources: []config.Source{
			{Name: "dead", BaseURL: "http://127.0.0.1:0", ModelMap: map[string]string{"gpt-5": "claude"}},
		},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}
	var ml model.CodexModelsResponse
	if err := json.Unmarshal(body, &ml); err != nil {
		t.Fatalf("unmarshal: %v. body: %s", err, body)
	}
	if len(ml.Models) != 0 {
		t.Fatalf("expected empty models list, got %+v", ml.Models)
	}
}

func TestModelsEndpointRejectsPost(t *testing.T) {
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "s1", BaseURL: "http://unused"}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/models", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// TestModelsEndpointCodexModelInfoContract 验证 /v1/models 返回的每个 model 对象
// 都包含 Codex serde 反序列化 ModelInfo 所需的全部 key。
func TestModelsEndpointCodexModelInfoContract(t *testing.T) {
	// Codex 的 ModelInfo（codex-rs/protocol/src/openai_models.rs）中，
	// 凡是未标注 #[serde(default)] 的字段（含 Option<T>），JSON key 必须存在，
	// 否则 serde_json::from_slice::<ModelsResponse> 直接失败。
	requiredKeys := []string{
		"slug",
		"display_name",
		"description",
		"supported_reasoning_levels",
		"shell_type",
		"visibility",
		"supported_in_api",
		"priority",
		"availability_nux",
		"upgrade",
		"base_instructions",
		"supports_reasoning_summaries",
		"support_verbosity",
		"default_verbosity",
		"truncation_policy",
		"supports_parallel_tool_calls",
		"experimental_supported_tools",
	}

	// 只为 gpt-5 配置 ModelOverrides，/v1/models 仅返回 gpt-5 一条
	ctxWindow := int64(131072)
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{
			{Name: "up", BaseURL: "http://127.0.0.1:0", ModelMap: map[string]string{"gpt-5": "claude"}},
		},
		ModelOverrides: map[string]config.ModelOverride{
			"gpt-5": {ContextWindow: &ctxWindow},
		},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", resp.StatusCode, body)
	}

	var raw struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v. body: %s", err, body)
	}
	if len(raw.Models) == 0 {
		t.Fatalf("no models returned. body: %s", body)
	}
	for i, m := range raw.Models {
		for _, key := range requiredKeys {
			if _, ok := m[key]; !ok {
				var slug json.RawMessage = m["slug"]
				t.Errorf("model[%d] slug=%s 缺少 Codex serde required key %q", i, slug, key)
			}
		}
		// gpt-5 的 context_window 应被 ModelOverrides 覆盖为 131072
		if slugBytes, ok := m["slug"]; ok && strings.Contains(string(slugBytes), "gpt-5") {
			cwBytes, hasCW := m["context_window"]
			if !hasCW {
				t.Fatalf("gpt-5 缺少 context_window（应由 ModelOverrides 补齐）")
			}
			if strings.TrimSpace(string(cwBytes)) != "131072" {
				t.Errorf("gpt-5 context_window = %s, want 131072（来自 ModelOverrides）", strings.TrimSpace(string(cwBytes)))
			}
			// context_window 与 max_context_window 应自动归一为同值
			maxCWBytes, hasMaxCW := m["max_context_window"]
			if !hasMaxCW {
				t.Fatalf("gpt-5 缺少 max_context_window（应由 context_window 归一补齐）")
			}
			if strings.TrimSpace(string(maxCWBytes)) != "131072" {
				t.Errorf("gpt-5 max_context_window = %s, want 131072（归一化）", strings.TrimSpace(string(maxCWBytes)))
			}
		}
	}

	// 网关默认值策略（非 ModelOverrides 字段，应被 codexModelInfo 默认填充）
	m0 := raw.Models[0]
	// apply_patch_tool_type=freeform 启用 apply_patch 工具
	if apt, ok := m0["apply_patch_tool_type"]; !ok || strings.TrimSpace(string(apt)) != `"freeform"` {
		t.Fatalf(`apply_patch_tool_type 应为 "freeform", got: %v (present=%v)`, string(m0["apply_patch_tool_type"]), ok)
	}
	// supports_search_tool=true
	if sst, ok := m0["supports_search_tool"]; !ok || strings.TrimSpace(string(sst)) != "true" {
		t.Fatalf("supports_search_tool 应为 true, got: %v (present=%v)", string(m0["supports_search_tool"]), ok)
	}
	// include_skills_usage_instructions=true
	if isui, ok := m0["include_skills_usage_instructions"]; !ok || strings.TrimSpace(string(isui)) != "true" {
		t.Fatalf("include_skills_usage_instructions 应为 true, got: %v (present=%v)", string(m0["include_skills_usage_instructions"]), ok)
	}
	// web_search_tool_type=text_and_image
	if wst, ok := m0["web_search_tool_type"]; !ok || strings.TrimSpace(string(wst)) != `"text_and_image"` {
		t.Fatalf(`web_search_tool_type 应为 "text_and_image", got: %v (present=%v)`, string(m0["web_search_tool_type"]), ok)
	}
	// input_modalities 包含 text+image
	if im, ok := m0["input_modalities"]; !ok || !strings.Contains(string(im), "image") {
		t.Fatalf("input_modalities 应含 image, got: %v (present=%v)", string(m0["input_modalities"]), ok)
	}
	// supported_reasoning_levels 非空，含 medium
	if srl, ok := m0["supported_reasoning_levels"]; !ok || !strings.Contains(string(srl), "medium") {
		t.Fatalf("supported_reasoning_levels 应含 medium, got: %v (present=%v)", string(m0["supported_reasoning_levels"]), ok)
	}
	// truncation_policy.limit 对齐官方固定 10000（工具输出截断阈值，不随 context_window 变化）
	if tp, ok := m0["truncation_policy"]; !ok || !strings.Contains(string(tp), "10000") {
		t.Fatalf("truncation_policy.limit 应为 10000, got: %v (present=%v)", string(m0["truncation_policy"]), ok)
	}
	// display_name 应为 slug 的大写形式
	if dn, ok := m0["display_name"]; !ok || strings.TrimSpace(string(dn)) != `"GPT-5"` {
		t.Fatalf(`display_name 应为 "GPT-5", got: %v (present=%v)`, string(m0["display_name"]), ok)
	}
	// base_instructions 默认为空（未配置 base_instructions_file 时，沿用 Codex 内置指令）
	if bi, ok := m0["base_instructions"]; !ok || strings.TrimSpace(string(bi)) != `""` {
		t.Fatalf("base_instructions 应为空串, got: %v (present=%v)", string(m0["base_instructions"]), ok)
	}
	// tool_mode 默认注入 "direct"（强制标准工具模式，覆盖 features.code_mode*，避免上游降级）
	if tm, ok := m0["tool_mode"]; !ok || strings.TrimSpace(string(tm)) != `"direct"` {
		t.Fatalf(`tool_mode 应为 "direct", got: %v (present=%v)`, string(m0["tool_mode"]), ok)
	}
	// multi_agent_version 默认注入 "v2"（对齐官方 gpt-5.6 catalog）
	if mav, ok := m0["multi_agent_version"]; !ok || strings.TrimSpace(string(mav)) != `"v2"` {
		t.Fatalf(`multi_agent_version 应为 "v2", got: %v (present=%v)`, string(m0["multi_agent_version"]), ok)
	}
	// comp_hash 默认注入 "3000"（对齐官方 gpt-5.6 catalog 压缩兼容哈希）
	if ch, ok := m0["comp_hash"]; !ok || strings.TrimSpace(string(ch)) != `"3000"` {
		t.Fatalf(`comp_hash 应为 "3000", got: %v (present=%v)`, string(m0["comp_hash"]), ok)
	}
	// use_responses_lite 显式注入 false：压制 Responses Lite（Codex→OpenAI 后端内部
	// 传输优化，第三方上游有害无益）。显式 false 而非省略，防 Codex hardcode/默认开启。
	if url, ok := m0["use_responses_lite"]; !ok || strings.TrimSpace(string(url)) != "false" {
		t.Fatalf("use_responses_lite 应显式为 false, got: %v (present=%v)", string(m0["use_responses_lite"]), ok)
	}
}
