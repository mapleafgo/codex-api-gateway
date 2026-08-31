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
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
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

// TestPrepareUpstreamBody_PreservesInputImage 验证 r 路径透传源对
// input_image 保持原样：URL、data URI、file_id 与 detail 都不改写（007 FR-003）。
func TestPrepareUpstreamBody_PreservesInputImage(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","stream":false,"input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"look"},
		{"type":"input_image","image_url":"https://example.com/a.png?sig=abc#frag","detail":"high"},
		{"type":"input_image","image_url":"data:image/png;base64,AAEC"},
		{"type":"input_image","file_id":"file-abc"}
	]}]}`)
	body, _, _, err := PrepareUpstreamBody(raw, &config.Source{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`https://example.com/a.png?sig=abc#frag`,
		`"detail":"high"`,
		`data:image/png;base64,AAEC`,
		`"file_id":"file-abc"`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("passthrough lost %s in %s", want, body)
		}
	}
}

// TestPrepareUpstreamBody_RewritesPlaintextAgentMessage 验证 r 路径把
// Codex 多 agent 的明文 agent_message 按 wire 等价物恢复为 assistant message。
// Responses 兼容上游不认识 agent_message 扩展时不能静默丢掉初始任务。
func TestPrepareUpstreamBody_RewritesPlaintextAgentMessage(t *testing.T) {
	src := config.Source{Backend: "openai-responses"}
	raw := []byte(`{
		"model":"gpt-5",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"context"}]},
			{
				"type":"agent_message",
				"id":"am_1",
				"author":"/root",
				"recipient":"/root/child",
				"content":[{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\nCHILD_TASK"}]
			}
		]
	}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Input) != 2 {
		t.Fatalf("input length=%d want 2: %s", len(payload.Input), body)
	}
	got := payload.Input[1]
	if got["type"] != "message" || got["role"] != "assistant" {
		t.Fatalf("agent_message must become assistant message, got %#v", got)
	}
	if got["id"] != "am_1" {
		t.Fatalf("id changed: %#v", got)
	}
	if _, ok := got["author"]; ok {
		t.Fatalf("agent-specific author must be removed: %#v", got)
	}
	if _, ok := got["recipient"]; ok {
		t.Fatalf("agent-specific recipient must be removed: %#v", got)
	}
	if !bytes.Contains(body, []byte("CHILD_TASK")) {
		t.Fatalf("task text missing: %s", body)
	}
}

func TestPrepareUpstreamBody_PreservesEncryptedAgentMessage(t *testing.T) {
	src := config.Source{Backend: "openai-responses"}
	raw := []byte(`{
		"model":"gpt-5",
		"input":[
			{
				"type":"agent_message",
				"content":[
					{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},
					{"type":"encrypted_content","encrypted_content":"secret"}
				]
			}
		]
	}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"type":"agent_message"`)) || !bytes.Contains(body, []byte(`"encrypted_content":"secret"`)) {
		t.Fatalf("encrypted agent_message should remain native for capable upstream: %s", body)
	}
}

// TestPrepareUpstreamBody_StripsWebSearchWhenUnsupported 验证 r 路径在源不支持
// hosted web_search 时剥掉 tools 里的 {"type":"web_search"}，保留其他工具。
func TestPrepareUpstreamBody_StripsWebSearchWhenUnsupported(t *testing.T) {
	off := false
	src := config.Source{
		ModelMap:          map[string]string{"gpt-5": "o3"},
		Backend:           "openai-responses",
		SupportsWebSearch: &off,
	}
	raw := []byte(`{"model":"gpt-5","tools":[{"type":"function","name":"f1"},{"type":"web_search"},{"type":"function","name":"f2"}]}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Tools) != 2 {
		t.Fatalf("web_search 未剥除，tools=%v", m.Tools)
	}
	for _, tl := range m.Tools {
		if tl["type"] == "web_search" {
			t.Fatalf("web_search 仍保留: %v", m.Tools)
		}
	}
}

// TestPrepareUpstreamBody_KeepsWebSearchWhenSupported 验证 r 路径在源支持
// hosted web_search 时原样透传，不剥除 web_search 工具。
func TestPrepareUpstreamBody_KeepsWebSearchWhenSupported(t *testing.T) {
	on := true
	src := config.Source{
		ModelMap:          map[string]string{"gpt-5": "o3"},
		Backend:           "openai-responses",
		SupportsWebSearch: &on,
	}
	raw := []byte(`{"model":"gpt-5","tools":[{"type":"web_search"}]}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"web_search"`)) {
		t.Fatalf("支持的源应保留 web_search, got %s", body)
	}
}

// TestPrepareUpstreamBody_NeutralizesToolChoiceWhenUnsupported 验证 r 路径在源
// 不支持 hosted web_search 时，除剥掉 tools 外还清除指向 web_search 的 tool_choice。
func TestPrepareUpstreamBody_NeutralizesToolChoiceWhenUnsupported(t *testing.T) {
	off := false
	src := config.Source{
		ModelMap:          map[string]string{"gpt-5": "o3"},
		Backend:           "openai-responses",
		SupportsWebSearch: &off,
	}
	raw := []byte(`{"model":"gpt-5","tools":[{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["tool_choice"]; ok {
		t.Fatalf("web_search tool_choice 未清除: %s", body)
	}
	if tools, ok := m["tools"].([]any); !ok || len(tools) != 0 {
		t.Fatalf("web_search tools 未剥除: %s", body)
	}
}

// TestPrepareUpstreamBody_FiltersAllowedToolsWhenUnsupported 验证 r 路径剥除
// allowed_tools 里的 web_search 条目，保留其余工具。
func TestPrepareUpstreamBody_FiltersAllowedToolsWhenUnsupported(t *testing.T) {
	off := false
	src := config.Source{
		ModelMap:          map[string]string{"gpt-5": "o3"},
		Backend:           "openai-responses",
		SupportsWebSearch: &off,
	}
	raw := []byte(`{
		"model":"gpt-5",
		"tools":[{"type":"function","name":"f"},{"type":"web_search"}],
		"tool_choice":{
			"type":"allowed_tools",
			"mode":"required",
			"tools":[{"type":"function","name":"f"},{"type":"web_search"}]
		}
	}`)
	body, _, _, err := PrepareUpstreamBody(raw, &src, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice struct {
			Tools []map[string]any `json:"tools"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Tools) != 1 || len(m.ToolChoice.Tools) != 1 {
		t.Fatalf("web_search 未过滤, tools=%v tool_choice=%v", m.Tools, m.ToolChoice.Tools)
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

// TestPrepareUpstreamBody_CompatFoldDisabledSkipsRewrite 验证 ResponsesCompatFold
// 关闭时 r 路径跳过 reasoning summary→content 折算，保持原生 reasoning 形态交给
// 原生 OpenAI Responses 兼容端点（折叠会触发 array too long 400）。工具调用 id
// 归一化由分发型插件在委托前完成，不再由共享后端承担。
func TestPrepareUpstreamBody_CompatFoldDisabledSkipsRewrite(t *testing.T) {
	fold := false
	src := config.Source{ResponsesCompatFold: &fold}
	raw := []byte(`{
		"model":"gpt-5",
		"input":[
			{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"need tool"}]},
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
	reasoning := m.Input[0]
	if reasoning["type"] != "reasoning" {
		t.Fatalf("first item type=%v", reasoning["type"])
	}
	if _, ok := reasoning["content"]; ok {
		t.Fatalf("折叠关闭时不应添加 content 数组: %v", reasoning["content"])
	}
	if _, ok := reasoning["summary"]; !ok {
		t.Fatal("summary 应原样保留")
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

func TestRewriteCollabPlaintextArgs(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		inject bool
	}{
		{
			name:   "spawn_agent collaboration inject",
			in:     `{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_0","call_id":"c1","name":"spawn_agent","namespace":"collaboration","arguments":"{\"message\":\"hi\"}"}}`,
			inject: true,
		},
		{
			name:   "ordinary tool no inject",
			in:     `{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_0","call_id":"c1","name":"get_weather","arguments":"{}"}}`,
			inject: false,
		},
		{
			name:   "non-collab namespace no inject",
			in:     `{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_0","call_id":"c1","name":"spawn_agent","namespace":"other","arguments":"{}"}}`,
			inject: false,
		},
		{
			name:   "non-function item no inject",
			in:     `{"type":"response.output_item.done","item":{"type":"message","id":"m1","role":"assistant","content":[]}}`,
			inject: false,
		},
	}
	for _, tc := range cases {
		out := rewriteCollabPlaintextArgs([]byte(tc.in))
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: unmarshal: %v: %s", tc.name, err, out)
		}
		item, ok := got["item"].(map[string]any)
		if !ok {
			t.Fatalf("%s: item missing: %s", tc.name, out)
		}
		_, present := item["encrypted_function_args"]
		if present != tc.inject {
			t.Fatalf("%s: encrypted_function_args present=%v want=%v: %s", tc.name, present, tc.inject, out)
		}
		if tc.inject {
			arr, ok := item["encrypted_function_args"].([]any)
			if !ok || len(arr) != 0 {
				t.Fatalf("%s: encrypted_function_args want 空数组: %s", tc.name, out)
			}
		}
	}

	// 上游已携带加密参数时原样保留，不做明文信号覆盖（透传层结果归上游）。
	preserved := `{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_0","call_id":"c1","name":"spawn_agent","namespace":"collaboration","encrypted_function_args":["a","b"],"arguments":"{}"}}`
	out := rewriteCollabPlaintextArgs([]byte(preserved))
	if !bytes.Contains(out, []byte(`"encrypted_function_args":["a","b"]`)) {
		t.Fatalf("上游已携带 encrypted_function_args 应原样保留: %s", out)
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
		config.Source{Name: "r1", BaseURL: ts.URL + "/v1", APIKey: "k", Backend: "openai-responses"},
		nil,
		func(e model.SSEEvent) error { events++; return nil },
		func(ev UpstreamEvent) {
			if ev.Backend != plugin.BackendOpenAIResponses {
				t.Fatalf("backend=%s", ev.Backend)
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
	if up.Status != "completed" || up.Backend != plugin.BackendOpenAIResponses {
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
	if up.Backend != plugin.BackendOpenAIResponses {
		t.Fatalf("backend=%s", up.Backend)
	}
}
