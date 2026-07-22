# OpenAI Chat Completions 上游后端 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在现有 Responses 客户端路径上，新增 `backend_type: c` 的 OpenAI Chat Completions 兼容上游（仅流式），并按方案 A 抽出 Backend 适配器，使 `a`/`c` 源可混排故障转移。

**Architecture:** `scheduler.ExecuteGeneric` 按源选择 `backend.Backend`；`AnthropicBackend` 承接现网 convert→anthropic→streamconv；`ChatBackend` 走 chatconvert→chatclient→chatstreamconv，统一产出 `model.SSEEvent`。配置 wire 短码 `a`/`c`；管理页反显与试拉 models；metrics/观测必填 backend_type。

**Tech Stack:** Go、`github.com/openai/openai-go/v3`、`github.com/anthropics/anthropic-sdk-go`、`log/slog`、标准 `testing` + `httptest`。

**Spec:** `docs/superpowers/specs/2026-07-21-openai-chat-backend-design.md`

> **完成度收口（2026-07-22）**：Task 1–10 主路径已落地；调度双轨已删除，测试迁 `ExecuteGeneric`；管理页未落盘试拉改走 `POST /admin/api/upstream-models`；混排与 chatclient mock 单测已补。

## Global Constraints

- 客户端仅 `/v1/responses` + `/v1/models`；不新增对外 `/v1/chat/completions`
- Chat 上游 **仅流式**（`stream: true`）；不实现非流式完成体路径
- `backend_type` wire 恒为 `a` | `c`（空→`a`）；metrics/观测禁止空串
- `chatclient` 路径拼接：`base + "/chat/completions"`（已含则不重复），**禁止**写死 `/v1/chat/completions`
- 出站事件用现有 `model.SSEEvent`；字面量对齐 SDK / 现 `streamconv`
- Holder 原子配置；热路径不阻塞 metrics
- 日志 `slog`；注释/commit 中文；标识符英文
- TDD：每 task 先失败测试 → 实现 → 通过 → commit
- 分层：convert/chatconvert 不做路由；admin 不直接改运行时

## File Structure

| 文件 | 责任 | 改动 |
|---|---|---|
| `internal/config/config.go` | Source.BackendType、常量、校验、规范化 | 改 |
| `internal/config/config_test.go` | backend_type 表驱动 | 改 |
| `config.example.yaml` | 示例源 `c` | 改 |
| `internal/chatclient/client.go` | Chat HTTP Stream + ListModels + URL | 新建 |
| `internal/chatclient/client_test.go` | URL/mock 流/错误 | 新建 |
| `internal/chatconvert/request.go` | Responses → Chat 请求 MVP | 新建 |
| `internal/chatconvert/request_test.go` | 表驱动转换 | 新建 |
| `internal/chatstreamconv/converter.go` | Chat chunk → Responses SSE | 新建 |
| `internal/chatstreamconv/converter_test.go` | 文本/tool_calls/usage | 新建 |
| `internal/backend/backend.go` | Backend 接口 + UpstreamEvent | 新建 |
| `internal/backend/anthropic.go` | AnthropicBackend | 新建 |
| `internal/backend/chat.go` | ChatBackend | 新建 |
| `internal/backend/*_test.go` | 后端单测（可 mock client） | 新建 |
| `internal/scheduler/scheduler.go` | ExecuteGeneric、backend 分发 | 改 |
| `internal/scheduler/scheduler_test.go` | 混排/Generic | 改 |
| `internal/server/server.go` | handleResponses 走 Generic | 改 |
| `internal/server/server_test.go` / `integration_test.go` | Chat 上游集成 | 改 |
| `internal/metrics/metrics.go` | RequestEvent.BackendType | 改 |
| `internal/admin/admin.go` / `convert.go` / `assets/index.html` | 类型字段、试拉、观测列 | 改 |
| `internal/admin/admin_test.go` | view 映射 + models API | 改 |
| `README.md` | 使用说明 | 改 |

---

### Task 1: config.BackendType（a/c）

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`（或新建 `backend_type_test.go`）
- Modify: `config.example.yaml`

**Interfaces:**
- Produces:
  - `const BackendAnthropic = "a"`、`BackendOpenAIChat = "c"`
  - `Source.BackendType string`
  - `func NormalizeBackendType(s string) (string, error)` — 空/`a` → `a`；`c` → `c`；其它 error
  - validate 中对每个 source 调用规范化并写回或拒绝非法值

- [ ] **Step 1: 写失败测试**

```go
func TestNormalizeBackendType(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "a", true},
		{"a", "a", true},
		{"c", "c", true},
		{"anthropic", "", false},
		{"openai_chat", "", false},
		{"x", "", false},
	}
	for _, tc := range cases {
		got, err := NormalizeBackendType(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Fatalf("in=%q got=%q err=%v want=%q", tc.in, got, err, tc.want)
			}
		} else if err == nil {
			t.Fatalf("in=%q expected error", tc.in)
		}
	}
}

func TestValidateRejectsUnknownBackendType(t *testing.T) {
	// 构造最小合法 Config，Sources[0].BackendType = "openai_chat"，期望 validate 失败
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/config/ -run 'BackendType|NormalizeBackendType' -count=1`
Expected: FAIL（符号不存在）

- [ ] **Step 3: 实现**

在 `Source` 增加 `BackendType string \`koanf:"backend_type" yaml:"backend_type,omitempty"\``。

```go
const (
	BackendAnthropic  = "a"
	BackendOpenAIChat = "c"
)

// NormalizeBackendType 将配置值规范为 a 或 c；非法值返回 error。
func NormalizeBackendType(s string) (string, error) {
	switch strings.TrimSpace(s) {
	case "", BackendAnthropic:
		return BackendAnthropic, nil
	case BackendOpenAIChat:
		return BackendOpenAIChat, nil
	default:
		return "", fmt.Errorf("invalid backend_type %q (want a or c)", s)
	}
}
```

在 `validate` 的 sources 循环中规范化并写回 `s.BackendType`（或仅校验、读时规范化——**推荐 validate 写回规范化值**，保证 holder 快照非空）。

`config.example.yaml` 增加注释示例源 `backend_type: c`（可注释掉）。

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/config/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/config/ config.example.yaml
git commit -m "feat(config): source.backend_type 支持 a/c 短码"
```

---

### Task 2: chatclient（URL + Stream + ListModels）

**Files:**
- Create: `internal/chatclient/client.go`
- Create: `internal/chatclient/client_test.go`

**Interfaces:**
- Produces:
  - `type Client struct { HTTP *http.Client }`
  - `func New() *Client`
  - `func chatCompletionsURL(endpoint string) string`
  - `func modelsURL(endpoint string) string`
  - `func (c *Client) Stream(ctx, endpoint, apiKey string, body []byte) (io.ReadCloser, error)`
  - `func (c *Client) ListModels(ctx, endpoint, apiKey string) ([]ModelInfo, error)`
  - `type ModelInfo struct { ID string; ... }`

- [ ] **Step 1: 写失败测试（URL 拼接）**

```go
func TestChatCompletionsURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/chat/completions"},
		{"https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4/chat/completions"},
		{"https://ark.cn-beijing.volces.com/api/v3", "https://ark.cn-beijing.volces.com/api/v3/chat/completions"},
		{"https://dashscope.aliyuncs.com/compatible-mode/v1", "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"},
		{"https://api.deepseek.com", "https://api.deepseek.com/chat/completions"},
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
	}
	for _, tc := range cases {
		if got := chatCompletionsURL(tc.in); got != tc.want {
			t.Errorf("chatCompletionsURL(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}
```

另测：Stream 发 Bearer、body 含 `"stream":true`；4xx 返回 error；ListModels 解析 `data[].id`。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/chatclient/ -count=1`
Expected: FAIL（包不存在）

- [ ] **Step 3: 实现最小 client**

- `chatCompletionsURL` / `modelsURL` 按 spec 拼接
- `Stream`：POST JSON body（调用方已 marshal 且 stream=true），Header：`Authorization: Bearer`、`Content-Type: application/json`、`Accept: text/event-stream`
- 成功返回 `resp.Body`（调用方关闭）；>=400 读 body 返回 error
- `ListModels`：GET modelsURL，解析 `{"data":[{"id":"..."}]}`

可参考 `internal/anthropic/client.go` 的 Scan 模式；SSE 扫描可留给 Backend 或在 client 提供 `ScanEvents` 辅助。

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/chatclient/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/chatclient/
git commit -m "feat(chatclient): OpenAI Chat 兼容流式客户端与 ListModels"
```

---

### Task 3: chatconvert（Responses → Chat 请求 MVP）

**Files:**
- Create: `internal/chatconvert/request.go`
- Create: `internal/chatconvert/request_test.go`

**Interfaces:**
- Consumes: `convert.DecodeResponseNewParams`、`config.Source` model map（可复制 scheduler.ResolveModel 逻辑或抽共享）
- Produces:
  - `func ToChat(req *oairesponses.ResponseNewParams, src config.Source) ([]byte, error)`  
    返回可直接 POST 的 JSON（含 `stream:true`、`stream_options.include_usage:true`）
  - 或返回结构体再由 Backend marshal；**推荐返回结构体 + Backend marshal**，测试易断言：
    - `func ToChat(...) (*ChatRequest, error)`

`ChatRequest` 最小字段：`Model, Messages, Tools, ToolChoice, Temperature, TopP, MaxTokens, Stream, StreamOptions`。

- [ ] **Step 1: 写失败测试**

表驱动至少覆盖：
1. 纯 user 文本 → 一条 user message
2. instructions 非空 → 首条 system
3. function_call + function_call_output → assistant tool_calls + role=tool
4. tools function 声明映射
5. model 经 ModelMap 解析（在 ToChat 内用 src，或 Backend 侧 Resolve——**统一在 ToChat/Backend 一处**，推荐 Backend 设 model 前调用与 Anthropic 相同的 `scheduler.ResolveModel`，ToChat 用已解析 model 字符串）

简化接口：

```go
func ToChat(req *oairesponses.ResponseNewParams, model string) (*ChatRequest, error)
```

Backend 负责 `model = ResolveModel(&src, string(req.Model))`。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/chatconvert/ -count=1`

- [ ] **Step 3: 实现 MVP 转换**

- developer → system；reasoning/mcp/image 等按 spec 丢弃 + DEBUG/WARN
- `max_output_tokens` → JSON 字段 `max_tokens`
- 固定 `Stream: true`，`StreamOptions: {IncludeUsage: true}`
- tool_choice：none/auto/required/function

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/chatconvert/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/chatconvert/
git commit -m "feat(chatconvert): Responses 请求转 Chat Completions MVP"
```

---

### Task 4: chatstreamconv（Chat SSE → Responses SSE）

**Files:**
- Create: `internal/chatstreamconv/converter.go`
- Create: `internal/chatstreamconv/converter_test.go`

**Interfaces:**
- Produces:
  - `type Converter struct { ... }`
  - `func New() *Converter`
  - `func (c *Converter) Feed(chunkJSON []byte) ([]model.SSEEvent, error)` 或 Feed 解析后的结构
  - `func (c *Converter) FeedDone() []model.SSEEvent` — 处理 `[DONE]`
  - `RespID() string`、`Done() bool`、`Failed() bool`、`Usage()`、`OutputItems()`、`StopReason() string`  
    尽量对齐 `streamconv.Converter` 观测字段，方便 server 收尾

- [ ] **Step 1: 写失败测试**

1. 纯文本多 chunk → 含 `response.created` 与 text delta 与 `response.completed`
2. tool_calls 分片 arguments 累积 → function_call 事件链
3. 空 `choices` + usage 末包不 panic，usage 写入 completed
4. finish_reason `tool_calls` / `stop`

事件 type 字符串必须与 `internal/model` / 现 streamconv 一致（用 `model.MarshalEvent` 或相同常量）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/chatstreamconv/ -count=1`

- [ ] **Step 3: 实现状态机**

`IDLE → STREAMING → DONE`；toolCalls map[int]*accum；忽略 `reasoning_content`。

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/chatstreamconv/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/chatstreamconv/
git commit -m "feat(chatstreamconv): Chat 流式 chunk 转 Responses SSE"
```

---

### Task 5: backend 接口 + AnthropicBackend 迁入

**Files:**
- Create: `internal/backend/backend.go`
- Create: `internal/backend/anthropic.go`
- Create: `internal/backend/anthropic_test.go`（可选轻量）
- Modify: `internal/scheduler/scheduler.go`（后续 Task 7 全量接线；本 task 可先只建包 + 单测 AnthropicBackend 用 httptest）

**Interfaces:**
- Produces: 见 spec §3 `Backend.Execute(...) error`、`UpstreamEvent`
- AnthropicBackend 依赖：`*anthropicclient.Client`、可选 `*config.Holder` 用于 ToAnthropic(cfg)

**注意：** 现 `convert.ToAnthropic(req, cfg *config.Config)` 需要完整 cfg；Backend.Execute 内 `holder.Current()` 或由 scheduler 注入 holder。

- [ ] **Step 1: 定义接口与 AnthropicBackend 失败测试**

用 httptest 模拟 Anthropic SSE，调用 `AnthropicBackend.Execute`，断言 onEvent 收到至少一条 Responses 事件。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/backend/ -count=1`

- [ ] **Step 3: 实现 AnthropicBackend**

从 `server.handleResponses` + `scheduler.trySource` 抽出：
1. Decode body
2. ToAnthropic + MCP
3. Stream + scan events
4. streamconv.Feed → onEvent
5. 上报 onUpstream（含 BackendType=`a`）

首字节超时：可暂由 scheduler 包一层 context（Task 7），或 Backend 内读 BreakerFor；**推荐 scheduler 传入带 first-byte timeout 的 ctx**，Backend 不重复计时逻辑。

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/backend/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/backend/
git commit -m "feat(backend): Backend 接口与 AnthropicBackend"
```

---

### Task 6: ChatBackend

**Files:**
- Create: `internal/backend/chat.go`
- Create: `internal/backend/chat_test.go`

**Interfaces:**
- Consumes: chatconvert、chatclient、chatstreamconv
- Produces: `ChatBackend` 实现 `Backend`，`BackendType=c`

- [ ] **Step 1: 写失败测试**

httptest 返回 Chat SSE（文本 + [DONE]），Execute 后 onEvent 含 completed 类事件。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/backend/ -run Chat -count=1`

- [ ] **Step 3: 实现 ChatBackend.Execute**

1. Decode Responses body  
2. ResolveModel  
3. ToChat → marshal（确保 stream true）  
4. chatclient.Stream  
5. 逐行 SSE：`data: [DONE]` → FeedDone；否则 Feed(chunk)  
6. onUpstream BackendType=`c`  
7. 第一个 onEvent 前失败返回 error（供 failover）

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/backend/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/backend/chat.go internal/backend/chat_test.go
git commit -m "feat(backend): ChatBackend 流式上游"
```

---

### Task 7: scheduler.ExecuteGeneric + 混排

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Produces:
  - `func (s *Scheduler) ExecuteGeneric(ctx, rawBody []byte, onEvent func(model.SSEEvent) error, onUpstream func(backend.UpstreamEvent)) (sourceName string, err error)`
- `New` 持有 `anthropicBackend`、`chatBackend`
- `backendFor(src)` 按 NormalizeBackendType 选择
- 旧 `Execute`/`ExecutePrepared`：改为内部适配到 Generic，或保留 Anthropic 专用路径但 failover 逻辑只维护一份。**推荐：** tryRound 调用 `backend.Execute`，删除对 anthropic 事件回调的耦合。

- [ ] **Step 1: 写失败测试**

1. 单源 `c` mock Chat 成功  
2. 源1 `a` 建连失败 → 源2 `c` 成功（混排）  
3. 首事件后失败不切源（沿用现语义，若测试已有则改走 Generic）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/scheduler/ -count=1`（部分 FAIL 属预期）

- [ ] **Step 3: 实现 ExecuteGeneric**

复用 runtimeSeq / breaker / max_retries / backoff；`trySource` 改为：

```go
err := s.backendFor(src).Execute(fbCtx, rawBody, src, onEvent, mapUpstream)
```

锁定语义：Backend 或 scheduler 检测「是否已 onEvent」——可在 scheduler 包一层 onEvent 计数。

将 `OnUpstream` 映射到 metrics 时带 BackendType（server 层）。

- [ ] **Step 4: 全包测试通过**

Run: `go test ./internal/scheduler/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/
git commit -m "feat(scheduler): ExecuteGeneric 按 backend_type 分发并支持混排"
```

---

### Task 8: server 接线

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/server/integration_test.go`（按需）

**Interfaces:**
- `handleResponses` 使用 `ExecuteGeneric`；去掉 server 内 streamconv 状态机（由 Backend 产出终态事件）
- 预检：decode 成功即可；勿强制 ToAnthropic
- `recordUpstream` 写入 `BackendType`

- [ ] **Step 1: 调整/新增测试**

现有 Anthropic mock 测试仍绿；新增 Chat mock 上游集成：`backend_type: c` 源 → POST `/v1/responses` 收到 SSE。

- [ ] **Step 2: 运行确认失败（新测试）**

Run: `go test ./internal/server/ -run Chat -count=1`

- [ ] **Step 3: 改 handleResponses**

简化为：读 body → decode 日志 → ExecuteGeneric 写 SSE → 处理取消/错误收尾（若 Backend 已写 completed，避免重复终态——与现逻辑仔细对齐）。

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/server/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/server/
git commit -m "feat(server): /v1/responses 经 ExecuteGeneric 支持 Chat 上游"
```

---

### Task 9: metrics + admin（类型、观测列、试拉 models）

**Files:**
- Modify: `internal/metrics/metrics.go`（及展示用 record 结构）
- Modify: `internal/admin/admin.go`、`convert.go`、`assets/index.html`、测试

**Interfaces:**
- `RequestEvent.BackendType string` 必填语义 `a`|`c`
- `sourceView.BackendType string`
- `POST /admin/api/upstream-models` body: `{base_url, api_key, backend_type}` → `[{id, display_name?}]`
- 观测台 recent 表增加后端类型列

- [ ] **Step 1: 测试**

- config 往返含 `backend_type: c`
- upstream-models：httptest mock Chat `/models` 返回 id 列表（未落盘 body）
- metrics 序列化含 backend_type

- [ ] **Step 2: 实现**

- convert 映射 BackendType
- index.html：源卡片 select；i18n 标签「Anthropic」/「OpenAI Chat」
- 试拉按钮调用 upstream-models
- 观测列表显示 backend_type（可反显标签）

- [ ] **Step 3: 测试通过**

Run: `go test ./internal/admin/ ./internal/metrics/ -count=1`

- [ ] **Step 4: Commit**

```bash
git add internal/admin/ internal/metrics/
git commit -m "feat(admin,metrics): backend_type 配置、观测与试拉 models"
```

---

### Task 10: 文档与门禁

**Files:**
- Modify: `README.md`
- 按需：`docs/protocol-coverage.md` 短注 Chat 上游

- [ ] **Step 1: README** 增加 Chat 源配置示例、base_url 表、仅流式说明

- [ ] **Step 2: 门禁**

Run: `task check`（或 `gofmt` + `go vet` + `go test ./...`）
Expected: 通过

- [ ] **Step 3: Commit**

```bash
git add README.md docs/
git commit -m "docs: 说明 OpenAI Chat 兼容上游配置"
```

---

## Spec Coverage Checklist

| Spec 项 | Task |
|---|---|
| Backend 适配器 A/C | 5–6 |
| backend_type a/c | 1 |
| 仅流式 Chat | 2, 6 |
| base_url 不写死 /v1 | 2 |
| ExecuteGeneric 混排 | 7 |
| server 接线 | 8 |
| metrics 必填类型 | 9 |
| admin 反显 + 未落盘试拉 | 9 |
| 对外 /v1/models 不变 | 8（不改语义） |
| MVP 转换边界 | 3–4 |
| 厂商路径表 | 2 测试 + 10 README |

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-21-openai-chat-backend.md`.

执行选项：
1. **Subagent-Driven（推荐）** — 每 task 新 subagent + 复审
2. **Inline Execution** — 本会话按 executing-plans 连续推进
