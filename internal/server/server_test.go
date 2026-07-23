package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestResponsesRequestLogIncludesInputDiagnostics(t *testing.T) {
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

// TestResponsesCompletedEmittedOnce proves the C1 fix end-to-end: the backend
// already sends message_stop and the handler also feeds a trailing synthetic
// message_stop, yet response.completed must appear EXACTLY once in the output.
// The count (not Contains) is what would have caught the original bug.
// TestPreviousResponseIDEmitsWarn 确认网关无 session store 时对 previous_response_id
// 输出 WARN，而不是静默忽略（Codex 主路径不传此字段；其它客户端可能依赖链式会话）。
// TestServiceTierEmitsInfo：service_tier 路径差异（Chat 透传 / a 忽略）须有可观测日志。
// TestIncludeSatisfiedNoWarn：默认已满足的 include 项（encrypted_content / web_search sources 等）不 WARN。
func TestIncludeSatisfiedNoWarn(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_inc\",\"model\":\"claude\"}}\n\n")
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

	body := `{"model":"gpt-5","include":["reasoning.encrypted_content","web_search_call.action.sources","code_interpreter_call.outputs"],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if strings.Contains(logs.String(), "include") {
		t.Fatalf("satisfied include items must not WARN, logs:\n%s", logs.String())
	}
}

// TestIncludeUnsupportedWarns：file_search / logprobs 等无等价 include 仍 WARN。
func TestIncludeUnsupportedWarns(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_inc2\",\"model\":\"claude\"}}\n\n")
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

	body := `{"model":"gpt-5","include":["file_search_call.results","message.output_text.logprobs","reasoning.encrypted_content"],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	got := logs.String()
	if !strings.Contains(got, "file_search_call.results") || !strings.Contains(got, "message.output_text.logprobs") {
		t.Fatalf("expected WARN for unsupported include items, logs:\n%s", got)
	}
	// encrypted_content 已满足，不应出现在 values 里导致误导（允许整体 message 含 encrypted 字样在 impact 中）
	if strings.Contains(got, `"values"`) && strings.Contains(got, "reasoning.encrypted_content") && !strings.Contains(got, "file_search") {
		// ok
	}
}

func TestIncludeLogprobsChatNoWarn(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-t\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-t\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{
			Name: "chat", BaseURL: upstream.URL + "/v1", APIKey: "k", BackendType: config.BackendOpenAIChat,
		}},
	}
	srv := New(cfg)
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-5","include":["message.output_text.logprobs"],"top_logprobs":2,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if strings.Contains(logs.String(), "message.output_text.logprobs") {
		t.Fatalf("Chat source should not WARN logprobs include, logs:\n%s", logs.String())
	}
}

func TestServiceTierEmitsInfo(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_st\",\"model\":\"claude\"}}\n\n")
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
		strings.NewReader(`{"model":"gpt-5","service_tier":"priority","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	got := logs.String()
	if !strings.Contains(got, "service_tier") {
		t.Fatalf("expected INFO for service_tier, logs:\n%s", got)
	}
}

func TestPreviousResponseIDEmitsWarn(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_prev\",\"model\":\"claude\"}}\n\n")
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
		strings.NewReader(`{"model":"gpt-5","previous_response_id":"resp_hist_1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	got := logs.String()
	if !strings.Contains(got, "previous_response_id") || !strings.Contains(got, "resp_hist_1") {
		t.Fatalf("expected WARN for previous_response_id, logs:\n%s", got)
	}
}

// TestPromptCacheFieldsEmitDebug：OpenAI prompt_cache_* 对 Anthropic 无意义，
// 网关已自主 cache_control，属可控协议差异 → DEBUG（不刷 WARN）。
func TestPromptCacheFieldsEmitDebug(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_pc\",\"model\":\"claude\"}}\n\n")
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

	body := `{"model":"gpt-5","prompt_cache_key":"bucket-1","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"prompt_cache_retention":"24h","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	got := logs.String()
	for _, field := range []string{"prompt_cache_key", "prompt_cache_options", "prompt_cache_retention"} {
		if !strings.Contains(got, field) {
			t.Fatalf("expected DEBUG for %s, logs:\n%s", field, got)
		}
	}
}

// TestPromptCacheFieldsNoWarn：默认 WARN 级别下不应出现 prompt_cache_* 噪音。
func TestPromptCacheFieldsNoWarn(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_pcw\",\"model\":\"claude\"}}\n\n")
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

	body := `{"model":"gpt-5","prompt_cache_key":"bucket-1","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"prompt_cache_retention":"24h","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	got := logs.String()
	for _, field := range []string{"prompt_cache_key", "prompt_cache_options", "prompt_cache_retention"} {
		if strings.Contains(got, field) {
			t.Fatalf("%s must not appear at WARN level; logs:\n%s", field, got)
		}
	}
}

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

// TestModelsEndpointBaseInstructionsFromConfig 锁定基线指令链路：
// Load 读取 config 同级 base_instructions.md 写入 cfg.BaseInstructions，
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
	// supported_reasoning_levels 覆盖 Anthropic 全档：none/low/medium/high/xhigh/max
	if srl, ok := m0["supported_reasoning_levels"]; !ok {
		t.Fatal("supported_reasoning_levels 缺失")
	} else {
		s := string(srl)
		for _, e := range []string{"none", "low", "medium", "high", "xhigh", "max"} {
			if !strings.Contains(s, e) {
				t.Fatalf("supported_reasoning_levels 应含 %s, got: %v", e, s)
			}
		}
	}
	// truncation_policy.limit 对齐官方固定 10000（工具输出截断阈值，不随 context_window 变化）
	if tp, ok := m0["truncation_policy"]; !ok || !strings.Contains(string(tp), "10000") {
		t.Fatalf("truncation_policy.limit 应为 10000, got: %v (present=%v)", string(m0["truncation_policy"]), ok)
	}
	// display_name 应为 slug 的大写形式
	if dn, ok := m0["display_name"]; !ok || strings.TrimSpace(string(dn)) != `"GPT-5"` {
		t.Fatalf(`display_name 应为 "GPT-5", got: %v (present=%v)`, string(m0["display_name"]), ok)
	}
	// base_instructions 默认为空（config 同级无 base_instructions.md 时，沿用 Codex 内置指令）
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

// TestResponsesPostTerminalClientCancelTreatedAsCompleted 验证：上游已吐出
// message_stop（converter 已终态），随后客户端断开导致 context canceled 时，
// 不得 ERROR「响应请求失败」、不得把 client 指标记为 failed；应视为 completed。
func TestResponsesPostTerminalClientCancelTreatedAsCompleted(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_cancel\",\"model\":\"claude\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"type\":\"message_delta\",\"stop_reason\":\"end_turn\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
		// 保持连接一会儿，给客户端取消制造读尾 race。
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("content-type", "application/json")

	// 读到 completed 后立即断开客户端，触发读尾 context canceled。
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	buf := make([]byte, 4096)
	var body strings.Builder
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			body.Write(buf[:n])
			if strings.Contains(body.String(), "event: response.completed\n") {
				cancel()
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	_ = resp.Body.Close()

	// 等待 handleResponses goroutine 结束写日志。
	srv.WaitForHandlers()

	got := logs.String()
	if strings.Contains(got, "响应请求失败") {
		t.Fatalf("post-terminal client cancel must not ERROR as 响应请求失败. logs:\n%s", got)
	}
	if strings.Contains(got, "上游流终态后读取失败") {
		t.Fatalf("post-terminal client cancel must not WARN 上游流终态后读取失败. logs:\n%s", got)
	}

	// 指标：client 侧应 completed，不得 failed。
	// 先 Stop 强制 drain channel。
	snap := srv.Metrics().Snapshot()
	// 事件可能还在 channel；再等一轮。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap = srv.Metrics().Snapshot()
		if len(snap.Recent) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var sawClientCompleted bool
	for _, rec := range snap.Recent {
		if rec.Kind == "client" && rec.Status == model.ResponseStatusFailed {
			t.Fatalf("client metrics must not be failed after terminal stream: %+v", rec)
		}
		if rec.Kind == "client" && rec.Status == model.ResponseStatusCompleted {
			sawClientCompleted = true
		}
	}
	if !sawClientCompleted {
		t.Fatalf("want client completed metric after post-terminal cancel, recent=%+v logs=\n%s", snap.Recent, got)
	}
}

// TestResponsesMidStreamClientCancelNoFailedEvent 验证：流未终态时客户端断开，
// 不向已断开连接硬写 response.failed，也不把请求记成 ERROR「响应请求失败」。
func TestResponsesMidStreamClientCancelNoFailedEvent(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	block := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_mid\",\"model\":\"claude\"}}\n\n")
		flusher.Flush()
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(block)

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("content-type", "application/json")

	type result struct {
		resp *http.Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		ch <- result{resp, err}
	}()

	// 等上游事件开始流出后取消。
	time.Sleep(150 * time.Millisecond)
	cancel()

	res := <-ch
	if res.err == nil {
		// 尽量读一点再关，触发服务端写路径感知断开。
		_, _ = io.CopyN(io.Discard, res.resp.Body, 256)
		_ = res.resp.Body.Close()
	}

	// 等待 handleResponses goroutine 完成所有 slog 写入，消除与 logs.String() 的竞争。
	srv.WaitForHandlers()

	got := logs.String()
	if strings.Contains(got, `"level":"ERROR"`) && strings.Contains(got, "响应请求失败") {
		t.Fatalf("mid-stream client cancel must not ERROR 响应请求失败. logs:\n%s", got)
	}
	snap := srv.Metrics().Snapshot()
	for _, rec := range snap.Recent {
		if rec.Kind == "client" && rec.Status == model.ResponseStatusFailed {
			t.Fatalf("mid-stream cancel must not record client failed: %+v\nlogs:\n%s", rec, got)
		}
	}
}

func TestResponsesChatBackendEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-t\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-t\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second), MaxRetries: 0, DegradeThreshold: 3, CircuitInterval: config.Duration(time.Minute), HalfOpenProbes: 1},
		Sources: []config.Source{{
			Name: "chat", BaseURL: upstream.URL + "/v1", APIKey: "k", BackendType: config.BackendOpenAIChat,
		}},
	}
	// validate normalizes backend type
	bt, err := config.NormalizeBackendType(cfg.Sources[0].BackendType)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sources[0].BackendType = bt

	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "response.created") || !strings.Contains(s, "response.completed") {
		t.Fatalf("unexpected SSE body: %s", s)
	}
	if !strings.Contains(s, "ok") {
		t.Fatalf("missing content in body: %s", s)
	}
}

func TestModelsEndpointSupportsSearchOverride(t *testing.T) {
	off := false
	ctxWindow := int64(100000)
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: "http://127.0.0.1:0"}},
		ModelOverrides: map[string]config.ModelOverride{
			"no-search": {
				ContextWindow:      &ctxWindow,
				SupportsSearchTool: &off,
			},
		},
		ModelSlugOrder: []string{"no-search"},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var ml model.CodexModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&ml); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(ml.Models) != 1 {
		t.Fatalf("models=%d", len(ml.Models))
	}
	m0 := ml.Models[0]
	if m0.SupportsSearchTool {
		t.Fatalf("supports_search_tool should be false when override false")
	}
	if m0.WebSearchToolType != "" {
		t.Fatalf("web_search_tool_type should clear when search disabled, got %q", m0.WebSearchToolType)
	}
}

func TestResponsesRejectsOversizedBody(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerCfg{MaxBodyMB: 1}, // 1 MiB
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: "http://127.0.0.1:1"}},
	}
	// 补默认，对齐生产 Load 路径。
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Validate 会把 MaxBodyMB=1 保留；若写 0 会变 32。这里强制 1。
	cfg.Server.MaxBodyMB = 1

	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 构造略大于 1 MiB 的 JSON body
	big := strings.Repeat("x", 1<<20+64)
	payload := `{"model":"gpt-5","input":"` + big + `","stream":true}`
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 413; body=%s", resp.StatusCode, body)
	}
}
