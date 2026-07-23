# OpenAI Responses 上游透传后端 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在现有 Responses 客户端路径上，新增 `backend_type: r` 的 OpenAI Responses 兼容上游（仅流式、最小改写透传），与 `a`/`c` 混排故障转移；含 T2 出站 model 别名回写与 r 源 WARN 收口。

**Architecture:** `scheduler.ExecuteGeneric` 按源选择 `backend.Backend`；新增 `ResponsesBackend`：`PrepareUpstreamBody`（map 改 model + `stream=true`）→ `responsesclient.Stream` → `ScanSSE` → 可选 T2 model 回写 → `model.SSEEvent`。配置 wire 短码 `r`；管理页/观测/试拉 models 对齐 Chat 路径。

**Tech Stack:** Go、标准库 `net/http` + `bufio` + `encoding/json`、`log/slog`、`testing` + `httptest`。不新增第三方依赖。

**Spec:** `docs/superpowers/specs/2026-07-23-openai-responses-passthrough-design.md`（含 2026-07-23 自审修订：T2 / WARN / ScanSSE）

## Global Constraints

- 客户端仅 `/v1/responses` + `/v1/models`；不新增对外路由
- Responses 上游 **仅流式**（强制 body `stream: true`）；不实现非流式完成体
- `backend_type` wire：`a` | `c` | `r`（空→`a`）；metrics/观测禁止空串
- `responsesclient` 路径拼接：`base + "/responses"`（已含则不重复）；**禁止**写死唯一合法 `.../v1/responses` 配置形态
- 请求：**语义透传**（map→Marshal），非字节保真
- 出站 data：默认原样；**仅 T2** 回写顶层/`response` 内 `model` 为客户端请求 model
- ScanSSE：`event` Type 必须非空才 `onEvent`；Scanner 缓冲起始 **1MiB**，上限 **16MiB**
- 空流不合成 `response.created`/`completed`
- 中途失败不强制补 `response.failed`
- 配置含启用中的 `r` 源时，`warnDroppedOrIgnoredParams` 不对 r 可透传字段误报「数据被丢弃」
- Holder 原子配置；热路径不阻塞 metrics
- 日志 `slog`；注释/commit 中文；标识符英文
- TDD：每 task 先失败测试 → 实现 → 通过 → commit
- 分层：`responsesclient` 不做路由；`backend` 不做选源；admin 不直接改运行时

## File Structure

| 文件 | 责任 | 改动 |
|---|---|---|
| `internal/config/config.go` | `BackendOpenAIResponses`、Normalize、注释 | 改 |
| `internal/config/backend_type_test.go` | 表驱动补 `r` | 改 |
| `config.example.yaml` | 注释示例源 `r` | 改 |
| `internal/responsesclient/client.go` | URL / Stream / ScanSSE / ListModels | 新建 |
| `internal/responsesclient/client_test.go` | URL/mock 流/4xx/大缓冲 | 新建 |
| `internal/backend/responses.go` | ResponsesBackend + Prepare + T2 + usage | 新建 |
| `internal/backend/responses_test.go` | 改写/透传/空流/T2/usage | 新建 |
| `internal/backend/backend.go` | UpstreamEvent 注释 `a\|c\|r` | 改 |
| `internal/scheduler/scheduler.go` | 持有 responsesBackend、backendFor、ListModels | 改 |
| `internal/scheduler/scheduler_test.go` | r 分发（可选混排） | 改 |
| `internal/server/server.go` | 预检 r + warn 收口 | 改 |
| `internal/server/server_test.go` | r 集成 + warn 行为 | 改 |
| `internal/admin/admin.go` | upstream-models 接受 `r` | 改 |
| `internal/admin/assets/index.html` | options / 文案 / A\|C\|R | 改 |
| `internal/admin/admin_test.go` | r 校验 | 改 |
| `internal/metrics/metrics.go` | 注释 BackendType 含 r | 改 |
| `README.md` / `docs/protocol-coverage.md` | 使用说明 + 透传专节 | 改 |
| spec 状态行 | 实现后改为已实现 | 改 |

---

### Task 1: config — `backend_type: r`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/backend_type_test.go`
- Modify: `config.example.yaml`

**Interfaces:**
- Produces: `const BackendOpenAIResponses = "r"`；`NormalizeBackendType` 接受 `r`；非法文案 `allowed: a, c, r`

- [ ] **Step 1: 扩展失败测试**

在 `TestNormalizeBackendType` 的 cases 增加：

```go
{"r", "r", true},
{" r ", "r", true},
```

`TestValidateAcceptsBoth` 改名为 `TestValidateAcceptsKnownBackendTypes`，cases 含 `"r"`。

- [ ] **Step 2: 运行确认当前失败（r 被拒）**

Run: `go test ./internal/config/ -run 'NormalizeBackendType|ValidateAccepts' -count=1`

Expected: FAIL（`r` invalid）

- [ ] **Step 3: 实现**

```go
// backend_type: 'a' = Anthropic Messages, 'c' = OpenAI Chat Completions (only streaming),
// 'r' = OpenAI Responses (passthrough, only streaming)
const (
	BackendAnthropic       = "a"
	BackendOpenAIChat      = "c"
	BackendOpenAIResponses = "r"
)

func NormalizeBackendType(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", BackendAnthropic:
		return BackendAnthropic, nil
	case BackendOpenAIChat:
		return BackendOpenAIChat, nil
	case BackendOpenAIResponses:
		return BackendOpenAIResponses, nil
	default:
		return "", fmt.Errorf("config: invalid backend_type %q (allowed: a, c, r)", s)
	}
}
```

`config.example.yaml` 在 Chat 示例旁增加注释源：

```yaml
#  - name: openai-responses
#    base_url: https://api.openai.com/v1
#    api_key: ${OPENAI_API_KEY}
#    backend_type: r
#    model_map: { gpt-5: gpt-5 }
#    default_model: gpt-5
```

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/config/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/config/ config.example.yaml
git commit -m "feat(config): backend_type 支持 r（OpenAI Responses 透传）"
```

---

### Task 2: `responsesclient` — URL + Stream + ScanSSE + ListModels

**Files:**
- Create: `internal/responsesclient/client.go`
- Create: `internal/responsesclient/client_test.go`

**Interfaces:**
- Produces:
  - `type Client struct { HTTP *http.Client }`
  - `func New() *Client`
  - `func (c *Client) Stream(ctx, baseURL, apiKey string, body []byte) (io.ReadCloser, error)`
  - `func (c *Client) ListModels(ctx, baseURL, apiKey string) ([]ModelInfo, error)`
  - `type ModelInfo struct { ID, DisplayName string }`
  - `func ScanSSE(r io.Reader, onEvent func(eventType string, data []byte) error) error`
  - 内部：`responsesURL` / `modelsURL`（测试可导出小写测试或测通过 Stream 的 URL）

- [ ] **Step 1: 写失败测试（URL + 4xx + SSE）**

```go
package responsesclient_test

// 包名可用 responsesclient 同包测试以便测 unexported URL，或导出测试辅助。
// 推荐 package responsesclient（白盒）测 responsesURL / ScanSSE。

func TestResponsesURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/responses"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/responses"},
		{"https://x/v1/responses", "https://x/v1/responses"},
		{"https://x/v1/responses/", "https://x/v1/responses"},
	}
	for _, tc := range cases {
		if got := responsesURL(tc.in); got != tc.want {
			t.Fatalf("in=%q got=%q want=%q", tc.in, got, tc.want)
		}
	}
}

func TestStreamUpstreamError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Fatalf("auth=%s", r.Header.Get("Authorization"))
		}
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer ts.Close()
	_, err := New().Stream(context.Background(), ts.URL+"/v1", "k", []byte(`{"stream":true}`))
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err=%v", err)
	}
}

func TestScanSSE_EventAndTypeFallback(t *testing.T) {
	raw := strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"r1"}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	var types []string
	err := ScanSSE(strings.NewReader(raw), func(et string, data []byte) error {
		types = append(types, et)
		if !json.Valid(data) {
			t.Fatalf("invalid data %s", data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 2 || types[0] != "response.output_text.delta" || types[1] != "response.completed" {
		t.Fatalf("types=%v", types)
	}
}

func TestScanSSE_SkipEmptyType(t *testing.T) {
	raw := "data: {\"foo\":1}\n\n"
	n := 0
	_ = ScanSSE(strings.NewReader(raw), func(et string, data []byte) error {
		n++
		return nil
	})
	if n != 0 {
		t.Fatalf("expected skip, n=%d", n)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/responsesclient/ -count=1`

Expected: FAIL（包不存在）

- [ ] **Step 3: 实现 `client.go`（核心逻辑）**

要点（完整实现写入文件）：

```go
package responsesclient

// responsesURL / modelsURL：TrimRight "/"；HasSuffix 则返回，否则 + "/responses" 或 "/models"
// Stream：POST、Content-Type application/json、Authorization Bearer、Accept text/event-stream
//  ≥400：读 body trunc 500，return fmt.Errorf("upstream %d: %s", code, snippet)
// ListModels：对齐 chatclient 解析 data[]
// ScanSSE：
//   scanner := bufio.NewScanner(r)
//   buf := make([]byte, 0, 1024*1024)
//   scanner.Buffer(buf, 16*1024*1024)
//   累积 event/data，空行 flush
//   data == "[DONE]" → return nil（结束）
//   eventType 空则 json 取 "type"；仍空 slog.Debug 跳过
//   onEvent(eventType, dataBytes)
```

多行 `data:` 用 `\n` 拼接（SSE 规范）。

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/responsesclient/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/responsesclient/
git commit -m "feat(responsesclient): Responses 上游 HTTP 与 SSE 扫描"
```

---

### Task 3: `ResponsesBackend` — Prepare + Execute + T2 + usage

**Files:**
- Create: `internal/backend/responses.go`
- Create: `internal/backend/responses_test.go`
- Modify: `internal/backend/backend.go`（注释 BackendType `a | c | r`）

**Interfaces:**
- Produces:
  - `type ResponsesBackend struct { Client *responsesclient.Client }`
  - `func NewResponses() *ResponsesBackend`
  - `func PrepareUpstreamBody(raw []byte, src *config.Source) (body []byte, clientModel, resolved string, err error)`
  - `func rewriteClientModel(data []byte, clientModel string) []byte`（可 unexported）
  - `func (b *ResponsesBackend) Execute(...) error` 实现 `Backend`

- [ ] **Step 1: 写失败测试**

```go
func TestPrepareUpstreamBody_ModelMapAndStream(t *testing.T) {
	src := config.Source{
		ModelMap: map[string]string{"gpt-5": "o3"},
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
	// foo 保留
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
		io.WriteString(w, "event: response.created\n")
		io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_u","model":"o3"}}`+"\n\n")
		io.WriteString(w, "event: response.completed\n")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_u","model":"o3","usage":{"input_tokens":3,"output_tokens":4}}}`+"\n\n")
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
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/backend/ -run Responses|PrepareUpstream|RewriteClient -count=1`

- [ ] **Step 3: 实现 `responses.go`**

结构对齐 `chat.go`：

1. `PrepareUpstreamBody`：map 规则见 spec §6.2  
2. `Execute`：
   - prepare → Stream → ScanSSE  
   - 首帧 `onEvent` 前记 TTFB、locked  
   - 每帧：`data = rewriteClientModel(data, clientModel)`；`onEvent(SSEEvent{Type: et, Data: data})`  
   - 观测：若 et 为 `response.completed` / `response.incomplete` / `response.failed`，尽力 parse `response.usage`  
   - 空流 / 取消 / 失败分支对齐 ChatBackend（`isClientCanceled`、`StatusCodeFromErr`、`errSummary`）  
   - `BackendType: config.BackendOpenAIResponses`  
3. `rewriteClientModel`：spec §6.5  

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/backend/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/backend/
git commit -m "feat(backend): ResponsesBackend 透传与 T2 model 回写"
```

---

### Task 4: scheduler — 注册 r 后端

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/scheduler/scheduler_test.go`（按需）

**Interfaces:**
- Consumes: `backend.NewResponses()`、`config.BackendOpenAIResponses`
- Produces: `Scheduler.responsesBackend *backend.ResponsesBackend`；`backendFor` 三分支；`ListUpstreamModels` r 走 responsesclient

- [ ] **Step 1: 改 New / backendFor / ListUpstreamModels**

```go
// Scheduler 字段增加：
responsesBackend *backend.ResponsesBackend

// New 中：
responsesBackend: backend.NewResponses(),

func (s *Scheduler) backendFor(src *config.Source) backend.Backend {
	bt, err := config.NormalizeBackendType(src.BackendType)
	if err != nil {
		return s.anthropicBackend
	}
	switch bt {
	case config.BackendOpenAIChat:
		return s.chatBackend
	case config.BackendOpenAIResponses:
		return s.responsesBackend
	default:
		return s.anthropicBackend
	}
}

// ListUpstreamModels：bt == r 时与 c 相同逻辑，改为：
//   ms, err := s.responsesBackend.Client.ListModels(...)
// 或 c 与 r 共用 Bearer ListModels（r 用 responsesBackend.Client）
```

`UpstreamEvent` / scheduler 注释中 `a | c` 改为 `a | c | r`。

- [ ] **Step 2: 最小测试**

若已有 mock 混排测，复制一份 `BackendType: r` 的源指向返回 Responses SSE 的 httptest，断言 completed。否则在 `backend` 集成已覆盖时，至少编译期保证 `backendFor` 分支存在（可用导出测试或 `trySourceGeneric` 间接测）。

推荐新增 `TestBackendFor_Responses`：若 `backendFor` 未导出，测 `ListUpstreamModels` + 执行路径。

- [ ] **Step 3: 测试**

Run: `go test ./internal/scheduler/ -count=1`

- [ ] **Step 4: Commit**

```bash
git add internal/scheduler/
git commit -m "feat(scheduler): 调度支持 backend_type=r"
```

---

### Task 5: server — 预检 + WARN 收口 + 集成测

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`

**Interfaces:**
- Produces: `hasEnabledResponsesBackend(sources []config.Source) bool`（可 unexported）
- 修改：`handleResponses` 预检；`warnDroppedOrIgnoredParams`

- [ ] **Step 1: 预检**

现有：

```go
if bt == config.BackendAnthropic {
	// ToAnthropic dry-run
}
```

扩展：

```go
switch bt {
case config.BackendAnthropic:
	// 保持现有 ToAnthropic
case config.BackendOpenAIResponses:
	if _, _, _, err := backend.PrepareUpstreamBody(body, &first); err != nil {
		slog.Warn("预转换响应请求失败", "source", first.Name, "backend_type", bt, "error", err)
		http.Error(w, "convert: "+err.Error(), http.StatusBadRequest)
		return
	}
}
// c：无转换预检
```

注意 import `backend` 包（server 已用 scheduler，避免循环：`backend` 不 import server，OK）。

- [ ] **Step 2: WARN 收口**

在 `warnDroppedOrIgnoredParams` 开头：

```go
if hasEnabledResponsesBackend(sources) {
	// r 可透传：跳过会误报「丢弃」的分支。
	// 仍可对 previous_response_id 打 INFO：网关不代补会话，字段透传上游。
	if req.PreviousResponseID.Value != "" {
		slog.Info("previous_response_id 将透传上游；网关不代补会话历史",
			"field", "previous_response_id",
			"previous_response_id", req.PreviousResponseID.Value,
			"impact", "backend_type=r 时原样转发；网关无 session store")
	}
	return // 或仅执行「仍与 r 无关」的检查（若有）
}
```

`hasEnabledResponsesBackend`：

```go
func hasEnabledResponsesBackend(sources []config.Source) bool {
	for _, src := range sources {
		if src.Disabled {
			continue
		}
		bt, err := config.NormalizeBackendType(src.BackendType)
		if err == nil && bt == config.BackendOpenAIResponses {
			return true
		}
	}
	return false
}
```

无 r 时完整保留现有 a/c WARN。

- [ ] **Step 3: 集成测试**

仿 `TestChat...`：

```go
func TestResponsesPassthroughBackend(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		// 断言 body stream=true、model 已映射
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if m["stream"] != true {
			t.Fatalf("stream=%v", m["stream"])
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "event: response.completed\n")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","model":"upstream-m","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Sources: []config.Source{{
			Name: "resp", BaseURL: upstream.URL + "/v1", APIKey: "k",
			BackendType: config.BackendOpenAIResponses,
			ModelMap:    map[string]string{"gpt-5": "upstream-m"},
		}},
	}
	// 规范化 BackendType、构造 Server/httptest 与现有测试相同
	body := `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`
	// POST /v1/responses，读 body，断言含 response.completed 且 model 为 gpt-5（T2）
}
```

可选：`TestWarnSkippedWhenResponsesSource` 用 `slog` test handler 或仅文档化手动测——若难测 slog，至少单测 `hasEnabledResponsesBackend`。

- [ ] **Step 4: 测试**

Run: `go test ./internal/server/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/server/
git commit -m "feat(server): r 预检与 warn 收口及透传集成测"
```

---

### Task 6: admin + metrics UI

**Files:**
- Modify: `internal/admin/admin.go`（`handleUpstreamModels`：`r` 与 `c` 同 ListModels 路径）
- Modify: `internal/admin/assets/index.html`（options、i18n、`backendTypeLabel` → A/C/R）
- Modify: `internal/admin/admin_test.go`（接受 r）
- Modify: `internal/metrics/metrics.go`（注释）

- [ ] **Step 1: admin API**

```go
if bt == config.BackendOpenAIChat || bt == config.BackendOpenAIResponses {
	// Bearer ListModels：c 用 chatclient；r 用 responsesclient.New().ListModels
}
```

- [ ] **Step 2: index.html**

- 中英文：`backendResponses: 'OpenAI Responses'`
- 所有 `opts=[{v:'a',...},{v:'c',...}]` 增加 `{v:'r', l:t('backendResponses')}`
- `addSource` fields options 同理
- `backendTypeLabel`：

```js
if (x === 'c') return 'C';
if (x === 'r') return 'R';
return 'A';
```

- `backendTypeTitle` 对应全文案

- [ ] **Step 3: 测试 + Commit**

Run: `go test ./internal/admin/ -count=1`

```bash
git add internal/admin/ internal/metrics/
git commit -m "feat(admin): 管理页与试拉支持 backend_type=r"
```

---

### Task 7: 文档 + 收尾

**Files:**
- Modify: `README.md`（与 Chat 节并列 Responses 透传）
- Modify: `docs/protocol-coverage.md`（新增 `backend_type: r` 专节，日期更新）
- Modify: spec 状态 → `已实现`

README 最小段落：

```markdown
### OpenAI Responses 透传上游（backend_type: r）

当上游本身提供 OpenAI Responses API 时，配置 `backend_type: r`：网关映射 model、强制 stream，
并将上游 SSE 转回客户端（出站 response.model 回写为客户端模型别名）。可与 a/c 混排。
```

protocol-coverage 专节：passthrough / T2 / 无 session store 产品边界 / 与 a/c 矩阵不共享。

- [ ] **Step 1: 写文档**

- [ ] **Step 2: 全量检查**

Run: `task check`  
若无 task：`gofmt -w ... && go test ./...`

- [ ] **Step 3: Commit**

```bash
git add README.md docs/ protocol-coverage 相关 spec
git commit -m "docs: Responses 透传上游说明与覆盖矩阵"
```

---

## Spec coverage checklist（自审）

| Spec 要求 | Task |
|---|---|
| `BackendOpenAIResponses=r` + Normalize | 1 |
| responsesclient URL/Stream/ListModels/ScanSSE/1MiB | 2 |
| PrepareUpstreamBody map 透传 | 3 |
| 空流不合成终态 | 3 |
| T2 model 回写 | 3 |
| usage → UpstreamEvent | 3 |
| backendFor / ListModels r | 4 |
| 混排 failover（沿用 ExecuteGeneric） | 4 |
| 首源预检 r | 5 |
| warn 收口 has r | 5 |
| 集成测 | 5 |
| admin UI + 试拉 | 6 |
| README + protocol-coverage | 7 |
| 不代写 response.failed | 3（行为） |
| 客户端始终 SSE | 5 集成（现网路径） |

## Placeholder scan

无 TBD/TODO；函数名与 spec 一致：`PrepareUpstreamBody`、`ScanSSE`、`rewriteClientModel`、`hasEnabledResponsesBackend`、`BackendOpenAIResponses`。

## Type consistency

- wire 短码恒为 `"r"` / `config.BackendOpenAIResponses`
- `UpstreamEvent.BackendType` string 必填 `"r"`
- `model.SSEEvent{Type, Data json.RawMessage}` 不变
