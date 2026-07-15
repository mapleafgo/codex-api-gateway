package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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
		{name: "effort", body: `{"model":"gpt-5","input":"hi","reasoning":{"effort":"medium"}}`, want: true},
		{name: "none", body: `{"model":"gpt-5","input":"hi","reasoning":{"effort":"none"}}`, want: false},
		{name: "concise", body: `{"model":"gpt-5","input":"hi","reasoning":{"summary":"concise"}}`, want: true},
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

// TestResponsesErrorPathIDConsistency (I2): when all sources fail before any
// message_start (conv.RespID()==""), the response.id in the response.failed
// event must match the key used for session.Save — otherwise
// previous_response_id can never resolve in a subsequent request.
func TestResponsesErrorPathIDConsistency(t *testing.T) {
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

	// The session store must have an entry under the same ID.
	if _, ok := srv.sess.Get(failedID); !ok {
		t.Fatalf("I2: session store missing entry for response.id %q — ID mismatch between error event and session save. body:\n%s", failedID, body)
	}
}

func TestModelsEndpointMergesUpstreamAndLocal(t *testing.T) {
	// 模拟上游 GET /v1/models 返回 Anthropic 格式的模型列表
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("upstream got method %s, want GET", r.Method)
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{
			"data": [
				{"type":"model","id":"claude-sonnet-4-20250514","display_name":"Claude Sonnet 4","created_at":"2025-05-14T00:00:00Z"},
				{"type":"model","id":"claude-opus-4-20250514","display_name":"Claude Opus 4","created_at":"2025-05-14T00:00:00Z"}
			],
			"has_more": false
		}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{
			{Name: "up", BaseURL: upstream.URL, ModelMap: map[string]string{
				"gpt-5":   "claude-sonnet-4-20250514", // 本地别名，与上游 claude-sonnet-4 重名不冲突
				"gpt-5.5": "claude-opus-4-20250514",
			}},
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
	var ml model.ListResponse
	if err := json.Unmarshal(body, &ml); err != nil {
		t.Fatalf("unmarshal: %v. body: %s", err, body)
	}
	if ml.Object != "list" {
		t.Fatalf("object = %q, want \"list\"", ml.Object)
	}
	// 应包含 4 个模型：2 个上游 + 2 个本地别名
	ids := make(map[string]bool)
	for _, m := range ml.Data {
		if m.Object != "model" {
			t.Fatalf("model %q object = %q, want \"model\"", m.ID, m.Object)
		}
		if m.Created <= 0 {
			t.Fatalf("model %q created = %d, want positive", m.ID, m.Created)
		}
		ids[m.ID] = true
	}
	for _, want := range []string{"claude-sonnet-4-20250514", "claude-opus-4-20250514", "gpt-5", "gpt-5.5"} {
		if !ids[want] {
			t.Fatalf("missing model %q in response: %+v", want, ml.Data)
		}
	}
	if len(ml.Data) != 4 {
		t.Fatalf("expected 4 models (2 upstream + 2 local), got %d: %+v", len(ml.Data), ml.Data)
	}
}

func TestModelsEndpointFallbackLocalOnly(t *testing.T) {
	// 上游不可达时，仅返回本地 model_map 别名
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
	var ml model.ListResponse
	if err := json.Unmarshal(body, &ml); err != nil {
		t.Fatalf("unmarshal: %v. body: %s", err, body)
	}
	if len(ml.Data) != 1 || ml.Data[0].ID != "gpt-5" {
		t.Fatalf("expected only gpt-5, got %+v", ml.Data)
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
