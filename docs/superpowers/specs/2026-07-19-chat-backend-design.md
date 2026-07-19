# OpenAI Chat Completions 后端源设计

> 日期：2026-07-19
> 状态：待审阅
> 方案：适配器模式（方案 B）

## 1. 背景与目标

当前 codex-api-gateway 仅支持 Anthropic Messages API 作为后端源。客户端发 OpenAI Responses API 请求，网关转换为 Anthropic 格式转发。

本设计新增 OpenAI Chat Completions API 作为可选后端源，实现：
- 客户端仍发 Responses API 请求（无感知）
- 按 source 配置区分后端类型（Anthropic / OpenAI Chat）
- 全量特性覆盖：文本对话、function calling、reasoning、structured output、多模态
- 仅流式（stream=true），与 Responses API 行为对齐

## 2. 架构概览

```
客户端 (Responses API)
       │
       ▼
   server.handleResponses
       │
       ▼
   scheduler.ExecuteGeneric
       │
       ├─ backend_type=""|"anthropic" ──→ AnthropicBackend ──→ anthropic client ──→ streamconv
       │
       └─ backend_type="openai_chat"  ──→ ChatBackend     ──→ chatclient     ──→ chatstreamconv
       │
       ▼
   Responses SSE events → 客户端
```

## 3. Backend 接口

```go
// internal/backend/backend.go

package backend

import (
    "context"
    "io"

    "github.com/mapleafgo/codex-api-gateway/internal/config"
)

// StreamEvent 是后端流式事件的抽象，由各实现填充。
// AnthropicBackend 用 *anthropic.MessageStreamEventUnion，
// ChatBackend 用 *chatclient.ChatCompletionChunk。
type StreamEvent interface{}

// EventConverter 将后端原生事件转为 Responses SSE 事件。
type EventConverter interface {
    // Feed 处理一个上游事件，返回 0~N 个 Responses SSE 事件。
    Feed(ev StreamEvent) []ResponseSSEEvent
}

// ResponseSSEEvent 是转换后的 Responses SSE 事件。
type ResponseSSEEvent struct {
    Type       string
    SequenceNo int
    Data       any // TerminalResponseEvent / OutputItemAdded / etc.
}

// Backend 是后端源的抽象接口。
type Backend interface {
    // Execute 发送请求到上游并流式返回事件。
    // rawBody 是客户端原始 Responses API 请求体。
    // src 是目标后端源配置。
    // onEvent 收到每个转换后的 Responses SSE 事件时回调。
    // onUpstream 上游请求结束时回调（观测）。
    Execute(
        ctx context.Context,
        rawBody []byte,
        src config.Source,
        onEvent func(ResponseSSEEvent) error,
        onUpstream func(UpstreamEvent),
    ) (sourceName string, err error)
}

// UpstreamEvent 描述单次上游尝试的观测数据（与 scheduler.UpstreamEvent 对齐）。
type UpstreamEvent struct {
    SourceName    string
    Model         string
    ResolvedModel string
    StartedAt     time.Time
    Duration      time.Duration
    Status        string
    Code          int
    InputTokens   int
    OutputTokens  int
    Error         string
    Attempt       int
}
```

## 4. 模块布局

### 4.1 internal/backend/（新包）

Backend 接口定义 + AnthropicBackend 实现（从 scheduler 重构）。

**AnthropicBackend** 封装现有逻辑：
- 调用 `convert.ToAnthropic` 构建请求
- 调用 `anthropicclient.Client.Stream` 发起流式请求
- 用 `streamconv.Converter.Feed` 转换每个事件
- 复用现有 MCP injection 逻辑

### 4.2 internal/chatclient/（新包）

OpenAI Chat Completions HTTP 客户端，类比 `internal/anthropic/client.go`。

```go
package chatclient

type Client struct { HTTP *http.Client }

// Stream POST /v1/chat/completions，stream=true，返回 SSE body。
func (c *Client) Stream(ctx context.Context, endpoint, apiKey string, req *ChatRequest) (io.ReadCloser, error)

// ListModels GET /v1/models，返回上游模型列表。
func (c *Client) ListModels(ctx context.Context, endpoint, apiKey string) ([]ModelInfo, error)
```

关键实现细节：
- 端点拼接：`base_url` + `/v1/chat/completions`（若已含后缀则不重复）
- 认证头：`Authorization: Bearer <api_key>`（OpenAI 标准格式）
- 请求体：直接 marshal `ChatRequest`（`stream: true` 已在结构体中）
- 响应校验：HTTP 4xx/5xx → 返回错误；content-type 非 SSE → 返回错误
- 错误格式：解析 `{error: {message, type, code}}`
- SSE 扫描：复用 `anthropicclient.ScanEvents` 的模式（逐行读 `data:` 前缀）

### 4.3 internal/chatconvert/（新包）

Responses API → Chat Completions 请求转换。

```go
package chatconvert

// ToChat 将 Responses 请求转为 Chat Completions 请求。
func ToChat(req *oairesponses.ResponseNewParams, cfg *config.Config, src *config.Source) (*ChatRequest, error)
```

### 4.4 internal/chatstreamconv/（新包）

Chat SSE → Responses SSE 流式转换。

```go
package chatstreamconv

type Converter struct { /* 状态机 */ }

func New() *Converter

// Feed 处理一个 ChatCompletionChunk，返回 Responses SSE 事件。
func (c *Converter) Feed(chunk *ChatCompletionChunk) []ResponseSSEEvent

// RespID 返回响应 ID（首个 chunk 的 id）。
func (c *Converter) RespID() string

// Usage 返回最终 usage 统计。
func (c *Converter) Usage() *Usage

// Done 是否已收到 [DONE] 或 finish_reason。
func (c *Converter) Done() bool

// StopReason 返回停止原因。
func (c *Converter) StopReason() string
```

## 5. 请求转换规则（chatconvert）

### 5.1 消息转换

| Responses 输入 item | Chat message |
|---|---|
| `message(role=user)` | `{role: "user", content: [...]}` |
| `message(role=assistant)` | `{role: "assistant", content, tool_calls}` |
| `message(role=system/developer)` | `{role: "system", content}` |
| `function_call` | `{role: "assistant", tool_calls: [{id, type:"function", function:{name, arguments}}]}` |
| `function_call_output` | `{role: "tool", tool_call_id, content}` |
| `reasoning` | **丢弃**（Chat API 无 thinking block） |
| `compaction` | 合并到 system message |
| `input_image` (url/data) | `{type: "image_url", image_url: {url}}` |
| `input_image` (file_id) | 丢弃 + WARN（无 OpenAI Files 凭据） |
| `input_file` | 降级为 `{type: "text", text: "[file: ...]"}` |
| `custom_tool_call` | 转为 function_call（name 加 `_custom` 后缀） |
| `custom_tool_call_output` | 转为 tool message |
| `mcp_*` | 丢弃 + WARN（Chat API 无 MCP 协议） |
| `shell_call` / `apply_patch_call` | 转为 function_call |
| `local_shell_call` | 转为 function_call |

### 5.2 工具定义转换

| Responses tool | Chat tool |
|---|---|
| `function` | `{type: "function", function: {name, description, parameters}}` |
| `custom` | `{type: "function", function: {name: name+"_custom", description, parameters}}` |
| `web_search` | `{type: "function", function: {name: "web_search", ...}}` （兼容后端支持时） |
| `mcp` | 丢弃 + WARN |
| `namespace` | 展开子工具，每个子工具独立转为 function |

### 5.3 参数映射

| Responses 参数 | Chat 参数 |
|---|---|
| `model` | `model`（经 ModelMap 解析） |
| `temperature` | `temperature` |
| `top_p` | `top_p` |
| `max_output_tokens` | `max_completion_tokens` |
| `reasoning.effort` | `reasoning_effort` |
| `reasoning.summary` | 丢弃（Chat API 无 thinking summary） |
| `instructions` | 合并到首条 system message |
| `tools` | `tools` |
| `tool_choice` | `tool_choice` |
| `response_format` | `response_format`（json_schema 直传） |
| `stream` | 固定 `true` |
| `stream_options` | `{include_usage: true}` |
| `metadata` | 丢弃 |

### 5.4 tool_choice 转换

| Responses tool_choice | Chat tool_choice |
|---|---|
| `"none"` | `"none"` |
| `"auto"` | `"auto"` |
| `"required"` | `"required"` |
| `{type: "function", function: {name}}` | `{type: "function", function: {name}}` |
| `{type: "custom", ...}` | `{type: "function", function: {name: name+"_custom"}}` |
| `{type: "web_search"}` | `"required"`（近似） |
| `{type: "mcp", ...}` | 丢弃，降级 `"auto"` |

## 6. 流式转换规则（chatstreamconv）

### 6.1 Chat SSE 流格式

每个 chunk 结构：
```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion.chunk",
  "created": 1234567890,
  "model": "gpt-4o",
  "choices": [{
    "index": 0,
    "delta": {"role": "assistant", "content": "Hello"},
    "finish_reason": null
  }],
  "usage": null
}
```

流终止：`data: [DONE]`

### 6.2 Responses SSE 事件映射

| Chat chunk 特征 | Responses SSE 事件 |
|---|---|
| 首个 delta.role=assistant | `response.created` + `response.output_item.added` + `response.content_part.added` |
| delta.content 非空 | `response.content_part.delta` |
| finish_reason="stop"/"length" | `response.content_part.done` + `response.output_item.done` |
| finish_reason="tool_calls" | 开始新的 function_call output item |
| 最后一个 chunk (含 usage) | `response.completed` |

### 6.3 工具调用流式转换

Chat 工具调用通过 delta.tool_calls 增量传输：

```
chunk1: delta.tool_calls = [{index:0, id:"call_xxx", function:{name:"get_weather", arguments:""}}]
chunk2: delta.tool_calls = [{index:0, function:{arguments:'{"location":'}}]
chunk3: delta.tool_calls = [{index:0, function:{arguments:'"Paris"'}}]
chunk4: delta.tool_calls = [{index:0, function:{arguments:'}'}}]
```

转换为 Responses 事件：
```
response.output_item.added (type=function_call, id=call_xxx, name=get_weather)
response.content_part.added (type=function_call_arguments)
response.content_part.delta (arguments='{"location":')
response.content_part.delta (arguments='"Paris"')
response.content_part.delta (arguments='}')
response.content_part.done
response.output_item.done
```

### 6.4 finish_reason 映射

| Chat finish_reason | Responses stop_reason |
|---|---|
| `"stop"` | `"stop"` |
| `"length"` | `"max_tokens"` |
| `"tool_calls"` | `"tool_use"` |
| `"content_filter"` | `"content_filter"` |

### 6.5 usage 处理

请求时带 `stream_options: {include_usage: true}`。
最后一个 chunk（finish_reason 非 null 后）携带 usage：
```json
{
  "usage": {
    "prompt_tokens": 100,
    "completion_tokens": 50,
    "total_tokens": 150
  }
}
```

映射到 Responses usage：
- `prompt_tokens` → `input_tokens`
- `completion_tokens` → `output_tokens`

### 6.6 Converter 状态机

```go
type Converter struct {
    respID       string
    seq          int
    outputItems  []model.OutputItem
    usage        *Usage
    contentBuf   strings.Builder    // 累积当前 text content
    toolCalls    map[int]*toolCallAccum  // 累积 tool call arguments
    currentRole  string
    started      bool
    done         bool
    stopReason   string
}

type toolCallAccum struct {
    id        string
    name      string
    arguments strings.Builder
}
```

状态流转：
1. **IDLE** → 收到首个 chunk（delta.role） → **STREAMING**
2. **STREAMING** → 收到 delta.content/tool_calls → 产出 delta 事件
3. **STREAMING** → 收到 finish_reason → **DONE** → 产出 completed 事件
4. **DONE** → 忽略后续 chunk

## 7. 配置变更

### 7.1 config.Source 新增字段

```go
type Source struct {
    Name          string            `yaml:"name"`
    BaseURL       string            `yaml:"base_url"`
    APIKey        string            `yaml:"api_key,omitempty"`
    BackendType   string            `yaml:"backend_type,omitempty"` // 新增
    ModelMap      map[string]string `yaml:"model_map,omitempty"`
    DefaultModel  string            `yaml:"default_model,omitempty"`
    Breaker       *BreakerCfg       `yaml:"breaker,omitempty"`
    OriginalIndex int               `yaml:"-"`
}
```

`BackendType` 取值：
- `""` 或 `"anthropic"` → AnthropicBackend（默认，向后兼容）
- `"openai_chat"` → ChatBackend
- 其他 → config.Load 报错

### 7.2 config.yaml 示例

```yaml
sources:
  # Anthropic 源（默认）
  - name: anthropic-main
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    # backend_type 缺省 = "anthropic"

  # OpenAI Chat 兼容源
  - name: openai-compat
    base_url: https://api.openai.com
    api_key: ${OPENAI_API_KEY}
    backend_type: openai_chat
    model_map:
      gpt-4o: gpt-4o
      claude-sonnet: gpt-4o    # 别名映射
```

## 8. Scheduler 改动

### 8.1 新增 ExecuteGeneric

```go
// ExecuteGeneric 根据 source.BackendType 选择 Backend 执行。
// rawBody 是客户端原始请求体（各 Backend 内部自行解析）。
func (s *Scheduler) ExecuteGeneric(
    ctx context.Context,
    rawBody []byte,
    onEvent func(backend.ResponseSSEEvent) error,
    onUpstream func(backend.UpstreamEvent),
) (string, error)
```

内部逻辑：
1. 遍历 runtimeSeq() 的 sources
2. 检查 breaker
3. 根据 source.BackendType 选择对应的 Backend 实例
4. 调用 Backend.Execute(ctx, rawBody, src, onEvent, onUpstream)
5. 失败时 failover 到下一个 source

### 8.2 Backend 实例管理

Scheduler 持有两种 Backend 实例：
- `anthropicBackend *backend.AnthropicBackend`（从现有代码重构）
- `chatBackend *backend.ChatBackend`（新增）

```go
func (s *Scheduler) backendFor(src *config.Source) backend.Backend {
    switch src.BackendType {
    case "openai_chat":
        return s.chatBackend
    default:
        return s.anthropicBackend
    }
}
```

### 8.3 向后兼容

现有 `ExecutePrepared` 方法保持不变（Anthropic 专用）。
server 层统一走 `ExecuteGeneric`，内部自动选择 Backend。
旧配置（无 backend_type）默认走 AnthropicBackend，行为不变。

## 9. Server 改动

handleResponses 中：

```go
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
    // ... 现有请求解析逻辑不变 ...

    // 统一走 ExecuteGeneric
    sourceName, execErr := s.sch.ExecuteGeneric(r.Context(), body,
        func(ev backend.ResponseSSEEvent) error {
            writeSSE(w, ev)
            flusher.Flush()
            return nil
        },
        s.recordUpstream(),
    )
    // ... 现有完成/错误处理逻辑不变 ...
}
```

## 10. Metrics 改动

`metrics.RequestEvent` 新增字段：

```go
type RequestEvent struct {
    // ... 现有字段 ...
    BackendType string `json:"backend_type,omitempty"` // "anthropic" | "openai_chat"
}
```

观测台可按后端类型筛选和聚合。

## 11. 降级与限制

### 11.1 不支持的特性（降级处理）

| 特性 | 降级方式 |
|---|---|
| MCP 工具 | 丢弃 + WARN |
| reasoning.summary | 丢弃（Chat API 无 thinking block） |
| thinking block 回灌 | 丢弃（Chat API 不支持） |
| input_image file_id | 丢弃 + WARN（无 Files 凭据） |
| input_file | 降级为文本描述 |
| connector_id / tunnel_id | 不适用（Anthropic 专有） |

### 11.2 错误处理

- HTTP 4xx/5xx → 解析 `{error: {message}}` 返回给客户端
- 非 SSE content-type → 返回错误
- 流中断 → 返回 `response.failed` 事件
- 所有 source 失败 → 返回 503

## 12. 测试策略

| 测试类型 | 覆盖范围 |
|---|---|
| 单元测试 | chatconvert.ToChat（各 item 类型转换） |
| 单元测试 | chatstreamconv.Converter（各 chunk 序列 → 事件序列） |
| 单元测试 | chatclient（HTTP mock） |
| 集成测试 | 端到端 Responses → Chat → Responses SSE |
| 表驱动 | tool_choice 各变体映射 |
| 表驱动 | finish_reason 映射 |
| 表驱动 | 多模态内容转换 |

## 13. 实施顺序

1. config.Source 新增 BackendType + 校验
2. internal/chatclient（HTTP 客户端）
3. internal/chatconvert（请求转换）
4. internal/chatstreamconv（SSE 转换）
5. internal/backend（接口 + AnthropicBackend 重构 + ChatBackend）
6. scheduler.ExecuteGeneric
7. server 统一走 ExecuteGeneric
8. metrics BackendType 字段
9. 测试
10. config.example.yaml 更新
11. README 更新
