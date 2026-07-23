# OpenAI Chat Completions 上游后端设计

> 日期：2026-07-21
> 状态：已实现（MVP + 批次 A/B：Codex freeform 工具环与终态对齐）
> 方案：完整 Backend 适配器（方案 A）
> 取代：`docs/superpowers/specs/2026-07-19-chat-backend-design.md`（该文状态改为已取代）
> 后续：第 3 种 backend（OpenAI Responses 透传 `r`）见 `2026-07-23-openai-responses-passthrough-design.md`（已实现）

## 1. 背景与目标

当前 codex-api-gateway 仅支持 Anthropic Messages API 作为后端源。客户端发送 OpenAI Responses API 请求，网关转换为 Anthropic 格式转发。

本设计新增 **OpenAI Chat Completions 兼容 API** 作为可选后端源，实现：

- 客户端仍只使用 Responses API（`/v1/responses`、`/v1/models`），无感知
- 按 source 配置区分后端类型（Anthropic / OpenAI Chat）
- Anthropic 源与 Chat 源可在同一 `sources` 列表中混排，共享故障转移与熔断
- Chat 上游 **仅流式**（固定 `stream: true`），不实现非流式完成体路径
- 管理页支持配置后端类型，并可对已保存/未落盘源试拉上游模型列表

### 1.1 首期（MVP）范围

| 必须 | 不做（后续可加） |
|---|---|
| 文本多轮 + function calling | 对外暴露 `/v1/chat/completions` |
| 基础采样参数（temperature / top_p / max tokens） | 完整多模态 / structured output |
| 流式 SSE + usage（能拿到则填） | Responses reasoning 与 Chat thinking 的完整等价映射 |
| 混排故障转移 / 熔断 | Codex hosted tools 在 Chat 上游上的真实能力 |
| 管理页 `backend_type` + 观测列 + 试拉 models | 第 3 种 backend（`r`）已由 2026-07-23 透传设计落地 |

### 1.2 非目标

- 不把 Anthropic 路径改成「先转 Chat 再转上游」（与仓库「Responses ↔ Anthropic 直转」原则冲突）
- 不改变对外 `/v1/models` 白名单语义（仍只返回配置 `models` 段）

## 2. 架构概览

```text
客户端 (Responses API)
       │
       ▼
   server.handleResponses
       │  解析 body → ResponseNewParams；写 SSE 头
       ▼
   scheduler.ExecuteGeneric(rawBody, onEvent, onUpstream)
       │  runtimeSeq + breaker + 首字节前 failover
       │  按 source.BackendType 选 Backend
       ├─ a (anthropic) ──→ AnthropicBackend
       │                      convert.ToAnthropic
       │                      → anthropic.Client.Stream
       │                      → streamconv → model.SSEEvent
       └─ c ───────────────→ ChatBackend
                              chatconvert.ToChat
                              → chatclient.Stream
                              → chatstreamconv → model.SSEEvent
       ▼
   Responses SSE → 客户端
```

### 2.1 分层归属（单向依赖）

| 层 | 包 | 职责 |
|---|---|---|
| L0 | `config` / `model` / `breaker` | `BackendType` 字段与校验；Responses wire 不变 |
| L1 | `anthropic`（已有）+ `chatclient`（新） | 各协议 HTTP/SSE 客户端，不感知 Responses |
| L2 | `convert` / `streamconv`（已有）+ `chatconvert` / `chatstreamconv`（新） | 协议双向转换 |
| L2.5 | `backend`（新） | `Backend` 接口 + Anthropic/Chat 实现；组装 L1+L2，产出统一 Responses 事件 |
| L3 | `scheduler` | 选源、熔断、重试、failover；调用 `Backend.Execute`；不写协议转换 |
| L4 | `server` | 唯一 `/v1/*` 入口；走 `ExecuteGeneric`；写 SSE / metrics |
| L5 | `admin` / `metrics` | 配置读写含 `backend_type`；观测字段必有后端类型 |

约束：

- 禁止 `convert` 做路由；禁止 `admin` 直接改运行时
- 热重载仍走：`config.yaml` → `Load` → `holder.Replace` → `scheduler.Reload`
- `/v1/*` 热路径不因 admin/metrics 阻塞；metrics 继续非阻塞投递
- Anthropic 现有语义（thinking / MCP injection / cache 等）落在 `AnthropicBackend` 内，scheduler 不再绑定 Anthropic 类型

### 2.2 包依赖方向

```text
server → scheduler → backend → {convert, streamconv, anthropic}
                              → {chatconvert, chatstreamconv, chatclient}
backend → config, model
scheduler → breaker, config, backend
admin → config（含 BackendType 读写）
```

`chatconvert` 不得 import `scheduler` / `server` / `admin`。

## 3. Backend 接口

### 3.1 接口定义

```go
// internal/backend/backend.go
package backend

// Backend 一次上游尝试：把客户端 Responses 请求转到该协议，再流式产出 Responses 事件。
type Backend interface {
    // Execute 对单个 source 发起流式请求。
    // rawBody：客户端原始 Responses JSON（各实现可按需重解析）。
    // src：当前源快照（含 BackendType / ModelMap / BaseURL / APIKey）。
    // onEvent：每个 Responses SSE 事件；返回 error 表示下游写入失败（应中止）。
    // onUpstream：单次尝试结束观测（可 nil）；成功/失败都调一次。
    // 返回 err：nil=业务流正常结束；非 nil=本源失败（scheduler 决定是否 failover）。
    Execute(
        ctx context.Context,
        rawBody []byte,
        src config.Source,
        onEvent func(model.SSEEvent) error,
        onUpstream func(UpstreamEvent),
    ) error
}
```

### 3.2 设计取舍

- **出站统一用现有 `model.SSEEvent`**，不引入额外的 `ResponseSSEEvent` / `StreamEvent interface{}` 包装层（与 `writeSSE` 一致）。
- **Backend 不负责选源 / 熔断**，只负责「这一个 src」。
- **rawBody 进 Backend**：按源重跑 `ToAnthropic` / `ToChat`（含 model map）。
- 需要配置快照时用 `holder.Current()` 或 Execute 入参，**禁止**把 `*config.Config` 缓存到 Backend 长生命周期对象。

### 3.3 UpstreamEvent

与现有 `scheduler.UpstreamEvent` 字段对齐，并 **必填** `BackendType`（规范化后的 `a` 或 `c`）：

```go
type UpstreamEvent struct {
    SourceName, Model, ResolvedModel string
    StartedAt                        time.Time
    Duration, TTFB                   time.Duration
    Status                           string // completed | failed
    Code                             int
    InputTokens, OutputTokens        int
    CacheRead, CacheCreate           int
    Error                            string
    Attempt                          int
    BackendType                      string // 必填：a | c
}
```

可将 `UpstreamEvent` 定义在 `backend` 包，scheduler 复用或做薄映射，避免 L3 反向依赖 L5。

### 3.4 两个实现

**AnthropicBackend**（从现有 server/scheduler 热路径迁入）

- 依赖：`convert`、`anthropicclient`、`streamconv`
- 流程：Decode → `ToAnthropic` → `Client.Stream` → `streamconv.Feed` → `onEvent`
- 迁入：MCP injection、首字节超时配合、usage/TTFB 观测、错误分类
- 目标：与现网 **bit-compatible**

**ChatBackend**（新）

- 依赖：`chatconvert`、`chatclient`、`chatstreamconv`
- 流程：Decode → `ToChat` → `Client.Stream` → `chatstreamconv.Feed` → `onEvent`
- 鉴权：`Authorization: Bearer <api_key>`
- 仅流式；非 SSE 响应视为本源失败

## 4. 模块布局

| 包 | 职责 | 对标 |
|---|---|---|
| `internal/backend` | 接口 + AnthropicBackend + ChatBackend | — |
| `internal/chatclient` | Chat HTTP + SSE 扫描、错误体、`ListModels` | `internal/anthropic` |
| `internal/chatconvert` | Responses → Chat 请求（MVP 子集） | `internal/convert` |
| `internal/chatstreamconv` | Chat chunk → Responses SSE | `internal/streamconv` |

wire 字面量优先从 `openai-go` / 现有 `model` 常量派生，禁止硬编码散落。

## 5. Scheduler

```go
func (s *Scheduler) ExecuteGeneric(
    ctx context.Context,
    rawBody []byte,
    onEvent func(model.SSEEvent) error,
    onUpstream func(backend.UpstreamEvent),
) (sourceName string, err error)

func (s *Scheduler) backendFor(src config.Source) backend.Backend {
    switch normalizeBackendType(src.BackendType) {
    case config.BackendOpenAIChat: // "c"
        return s.chatBackend
    default: // "a"
        return s.anthropicBackend
    }
}
```

- 持有两个 Backend 实例（无状态或仅 HTTP client）
- 现有 `Execute` / `ExecutePrepared` **内聚到 Generic**，不长期双轨；测试跟迁
- **首字节前 failover**：Backend 在真正写出第一个 `onEvent` 之前返回可重试错误时可切源；一旦 `onEvent` 被调用过则锁定，后续失败不再切源（与现语义一致）
- 混排：`a` 与 `c` 在同一 `runtimeSeq` 中按顺序尝试，允许

## 6. Server

- `handleResponses` 预检：有源、body 可 decode；**不做**「必须能 ToAnthropic」的硬预检（否则纯 Chat 部署会被 Anthropic 转换误杀）
- 可选：对当前优先级第一源做对应 Backend 的 dry-convert，失败 400
- 流式写出收敛：server 不再持有 `streamconv` 状态机；终态事件由各 Backend/converter 产出
- 客户端取消 / 完成收尾策略与现逻辑对齐
- `buildAnthropicRequest` 下沉到 `AnthropicBackend`

## 7. 配置

### 7.1 Source 字段

```go
type Source struct {
    Name          string            `yaml:"name" koanf:"name"`
    BaseURL       string            `yaml:"base_url" koanf:"base_url"`
    APIKey        string            `yaml:"api_key,omitempty" koanf:"api_key"`
    BackendType   string            `yaml:"backend_type,omitempty" koanf:"backend_type"` // 新增
    ModelMap      map[string]string `yaml:"model_map,omitempty" koanf:"model_map"`
    DefaultModel  string            `yaml:"default_model,omitempty" koanf:"default_model"`
    Breaker       *BreakerCfg       `yaml:"breaker,omitempty" koanf:"breaker"`
    OriginalIndex int               `yaml:"-" koanf:"-"`
}
```

### 7.2 枚举（wire 恒为短码）

| 配置值 | 规范化 | Backend |
|---|---|---|
| 缺省 / `""` / `a` | `a` | AnthropicBackend |
| `c` | `c` | ChatBackend |
| 其它 | validate 失败 | — |

```go
const (
    BackendAnthropic  = "a"
    BackendOpenAIChat = "c"
)
```

- 内部、落盘、metrics、观测事件：**始终**写规范化后的 `a` 或 `c`（禁止空串）
- 管理页 UI **反显**人类可读标签（中文/英文），存盘仍为 `a`/`c`；缺省可省略表示 `a`

### 7.3 示例

```yaml
sources:
  - name: anthropic-main
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_KEY}
    # backend_type 省略 = a
    model_map: { gpt-5: claude-sonnet-4-20250514 }
    default_model: claude-sonnet-4-20250514

  - name: openai-compat
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
    backend_type: c
    model_map: { gpt-5: gpt-4o }
    default_model: gpt-4o
```

- `base_url` 约定：填写 **OpenAI SDK 风格服务根**，**不含** `/chat/completions` 或 Anthropic 的 `/v1/messages`；路径由对应 client 拼接
- 环境变量：`CODEX_API_GATEWAY_SOURCES__N__BACKEND_TYPE=c`
- 热重载：整体替换 source 快照，不做字段级 in-place

### 7.4 校验

- `name` / `base_url` 规则不变
- `backend_type` 仅允许 `a` / `c` / 空
- **允许**混排；不因混排失败
- `api_key` 可空策略与现网一致

## 8. chatclient：URL 与协议

### 8.1 路径拼接（禁止写死 `/v1/chat/completions`）

对标 `anthropic.messagesURL`：

```text
endpoint = TrimRight(base_url, "/")
if HasSuffix(endpoint, "/chat/completions") → 原样
else → endpoint + "/chat/completions"

models: 同理拼接 "/models"（已含则不重复）
```

原因：各厂 SDK `base_url` 已包含不同前缀（`/v1`、`/api/paas/v4`、`/api/v3`、`/compatible-mode/v1` 等），硬拼 `/v1/chat/completions` 会破坏智谱/火山/百炼等。

### 8.2 厂商 base_url 参考（配置填写示例）

| 厂商 | 典型 base_url | 实际 POST |
|---|---|---|
| OpenAI | `https://api.openai.com/v1` | `…/v1/chat/completions` |
| DeepSeek | `https://api.deepseek.com` 或 `…/v1` | `…/chat/completions` |
| 智谱 | `https://open.bigmodel.cn/api/paas/v4` | `…/paas/v4/chat/completions` |
| 火山方舟 | `https://ark.cn-beijing.volces.com/api/v3` | `…/api/v3/chat/completions` |
| 阿里百炼 | `https://dashscope.aliyuncs.com/compatible-mode/v1` 或 `{WorkspaceId}.….maas…/compatible-mode/v1` | `…/compatible-mode/v1/chat/completions` |

管理页应对 Chat 源展示简短说明 + 示例，降低配错路径概率。

### 8.3 请求/响应约束

- 仅 `Stream`：body 固定 `stream: true`；建议带 `stream_options: {include_usage: true}`（兼容后端可忽略未知字段）
- 鉴权：`Authorization: Bearer`
- 错误：HTTP 4xx/5xx 解析 `error.message`（失败则 body 截断）
- 非 SSE content-type → 本源错误
- `ListModels`：`GET {base}/models`，供管理页使用

## 9. 转换策略（厂商调研驱动）

### 9.1 原则

1. **协议基准**：官方 OpenAI Chat Completions 流式 SSE
2. **兼容面优先**：保证「OpenAI SDK 换 base_url + api_key」类后端可跑
3. **字段级映射**在实现期用各厂文档 + 表驱动/集成测试固化；本设计锁策略与差异轴，不锁死未验证的细表
4. **仅流式**

建议实现期在 `research/openai-chat-backends/` 落 per-vendor findings（不阻塞本设计批准）。

### 9.2 流式共识

| 项 | 共识 |
|---|---|
| 传输 | SSE，`data: {json}`，结束 `data: [DONE]` |
| 增量 | `choices[0].delta.content` / `delta.tool_calls` |
| usage 末包 | 可能出现 **`choices` 为空** 的 chunk，只带 `usage`——converter **必须容忍** |
| 鉴权 | Bearer |

### 9.3 差异轴（MVP 处理）

| 差异 | MVP 策略 |
|---|---|
| `max_tokens` vs `max_completion_tokens` | 出站优先发 **`max_tokens`**（= Responses `max_output_tokens`）；首版不引入 per-vendor 配置 |
| `reasoning_content` / thinking 扩展 | 流中出现则忽略（DEBUG）；不映射 Responses reasoning item |
| 厂商扩展请求字段 | 不主动发送 |
| tool_calls 分片 / 并行 index | 按 OpenAI 标准按 `index` 累积 |
| 错误体格式 | 尽力解析，否则截断原文 |

### 9.4 MVP 能力边界

**必须支持**

- 文本多轮（system / user / assistant；developer → system）
- `instructions` 并入 system
- function tools + 流式 tool_calls → Responses function_call 事件链
- `function_call` / `function_call_output` 历史回灌
- temperature / top_p / ModelMap / DefaultModel
- `response.created` … `response.completed`（或 failed）；usage 有则填、无则 0

**明确降级**

- 多模态、structured output、MCP、hosted tools、reasoning summary
- Codex 专有 item（shell / apply_patch 等）→ 丢弃 + WARN（重要数据）或 DEBUG（已知缺口）

静默跳过遵循仓库约定：可控协议缺口可静默/DEBUG；重要且不可预期数据丢失必须 WARN。

### 9.5 出站事件

`chatstreamconv` 产出的 event type 字符串必须与现有 `streamconv` / `model` 常量对齐（从 SDK constant 派生），避免 Codex 因事件字面量分叉。

finish_reason 映射与 failed/completed 策略与 Anthropic 路径语义对齐（实现期对照现 `streamconv`）。

## 10. 管理页与上游模型列表

### 10.1 源编辑

- 后端类型选择：Anthropic（`a`）/ OpenAI Chat（`c`）；UI 反显标签，存盘短码
- 新建源默认 `a`
- 保存走现有写盘 + `config.validate`；非法类型拒绝

### 10.2 试拉上游 models

- **允许未落盘**：表单中的 `base_url` + `api_key` + `backend_type` 可直接试拉
- 已保存源也可按 name 试拉
- 管理 API（示意）：`POST /admin/api/upstream-models`（body 含 base_url/api_key/backend_type）和/或 `GET /admin/api/sources/{name}/models`
- 按类型分发：
  - `a` → anthropic `ListModels`
  - `c` → chatclient `ListModels`
- 统一返回 `[{id, display_name?}]` 供 `default_model` / `model_map` 目标下拉
- 失败友好提示，不阻断保存其它字段

### 10.3 对外 `/v1/models`

- **行为不变**：只返回配置 `models` 段显式 slug
- **不**把上游全量模型暴露给 Codex

### 10.4 观测台

- 请求表增加 **后端类型** 列
- 值来自实际尝试/命中源的协议：恒为 `a` 或 `c`
- 全失败：取 **最后一次尝试** 源的类型（仍必有值）
- UI 可显示标签，底层字段仍为短码

## 11. Metrics

```go
BackendType string `json:"backend_type"` // 必填语义：始终 "a" 或 "c"
```

- client 事件：最终成功源类型；全失败用最后一跳源类型
- upstream 事件：每次 try 带该源类型
- 非阻塞投递不变

## 12. 错误与 failover

| 情况 | 行为 |
|---|---|
| 连接失败 / 首字节超时 / 5xx / 非 SSE | 本源 failed；**尚未 onEvent** → 可切下一源 |
| 4xx | 与现 Anthropic 失败分类对齐（默认可计失败并切源，若现网有特例则照搬） |
| 流中途断开 | 已 locked → 不切源；写 failed 或现有中断语义 |
| 客户端取消 | 与现 handleResponses 一致 |
| 所有源失败 | 503 / 现有 all-failed 路径 |

## 13. 测试策略

| 类型 | 覆盖 |
|---|---|
| 单元 | chatconvert：文本/function 回灌/instructions；丢弃 reasoning/mcp 表驱动 |
| 单元 | chatstreamconv：纯文本、tool_calls 分片、空 choices+usage、`[DONE]`、finish_reason |
| 单元 | chatclient：URL 拼接（含智谱/火山/百炼形态）、HTTP mock、错误解析 |
| 单元 | config：`a`/`c`/空/非法；admin view 映射 |
| 集成 | mock Chat 上游 → `/v1/responses` 合法 SSE |
| 集成 | 混排：`a` 失败 → `c` 成功（首字节前） |
| 回归 | Anthropic 路径现有测试在 Backend 迁入后仍绿 |
| admin | 未落盘试拉 models（mock） |

涉及共享状态 / goroutine 的改动跑 `task test-race`；提交前 `task check`。

## 14. 实施顺序

1. `config.Source` 新增 `BackendType` + 校验 + `config.example.yaml` + 测试
2. `internal/chatclient`（URL、Stream、ListModels）
3. `internal/chatconvert` / `internal/chatstreamconv`（MVP）
4. `internal/backend` 接口 + `AnthropicBackend` 迁入（行为对齐）
5. `ChatBackend`
6. `scheduler.ExecuteGeneric` + 混排 failover；旧 API 内聚
7. `server` 统一走 Generic；预检按首源类型
8. `admin`：类型反显、试拉 models、观测台列
9. `metrics` BackendType 必填
10. README / 本设计状态；旧 7/19 草案标为已取代
11. （可选）`research/openai-chat-backends/` findings

## 15. 风险与缓解

| 风险 | 缓解 |
|---|---|
| Anthropic 热路径回归 | 先迁 Backend 再挂 Chat；全量现有测试 + check |
| base_url 配错 | 文档 + 管理页示例；拼接不强制 `/v1` |
| 厂商半兼容 | 规范中心 + 忽略扩展字段；失败可 failover |
| 调度双轨 | 不长期保留 ExecutePrepared 与 Generic 两套逻辑 |
| 转换细节漂移 | 实现期 findings + 表驱动测试固化映射 |

## 16. 已拍板决策记录

| 决策 | 选择 |
|---|---|
| 客户端协议 | 仅 Responses；不新增对外 Chat 入口 |
| 上游形态 | OpenAI Chat Completions 兼容，**仅流式** |
| 架构 | 完整 Backend 适配器（方案 A） |
| `backend_type` wire | 本文落地 `a` / `c`；后续补 `r`（见 2026-07-23 透传设计，已实现） |
| 混排 failover | 允许 |
| 管理页 | 第一版支持类型 + 未落盘试拉 models |
| 观测/metrics | `backend_type` 必有值（`a`\|`c`） |
| 对外 `/v1/models` | 白名单不变；上游列表仅 admin |
| 出站事件类型 | `model.SSEEvent` |
| 转换细节 | 调研驱动，设计锁策略不锁死字段表 |
