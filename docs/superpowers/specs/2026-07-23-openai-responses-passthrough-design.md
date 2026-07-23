# OpenAI Responses 上游透传后端设计

> 日期：2026-07-23
> 状态：已实现（2026-07-23；含 T2 model 回写 / r 源 WARN 收口 / ScanSSE 硬化）
> 方案：完整 Backend 适配器 + 最小改写透传（方案 A + wire 短码 `r`）
> 对齐：`docs/superpowers/specs/2026-07-21-openai-chat-backend-design.md`（Chat 后端方案 A 的第三后端补全）

## 1. 背景与目标

当前 codex-api-gateway 支持两种上游：

| `backend_type` | 上游协议 | 路径 |
|---|---|---|
| `a`（默认） | Anthropic Messages | Responses → Messages → Responses SSE |
| `c` | OpenAI Chat Completions（仅流式） | Responses → Chat → Responses SSE |
| `r` | OpenAI Responses 透传（仅流式） | 最小改写透传 + T2 model 回写 |

Chat 后端设计预留的「第 3 种 backend」由本设计落地：当上游本身提供 **OpenAI Responses API**（官方或任意实现 `/v1/responses` 的代理）时，网关不做协议语义转换，只做 **最小改写后的形状透传**，可与 a/c 源混排调度。

### 1.1 目标

- 客户端仍只使用网关 `/v1/responses` 与 `/v1/models`，无感知
- 按 source 配置 `backend_type: r` 区分 Responses 上游
- `a` / `c` / `r` 可在同一 `sources` 列表中混排，共享故障转移与熔断
- Responses 上游 **仅流式**（固定 `stream: true`）
- 管理页支持配置 `r`、观测列展示、试拉上游 `/models`

### 1.2 透传语义（方案 A）

「直接透传」定义为：

| 项 | 行为 |
|---|---|
| 请求 body | 解析为 JSON object；改写 `model`（`model_map` / `default_model`）；强制 `stream: true`；**其余键语义保留**（map 再 Marshal，**非**请求字节透传） |
| 上游 URL | `base_url` 拼接 `/responses`（已含则不重复） |
| 鉴权 | `Authorization: Bearer <api_key>` |
| 响应 SSE | 解析 `event:` + `data:`，以 `model.SSEEvent` 写出；**默认 Data 原样** |
| 出站 model 别名（**T2，已定**） | 与 a/c 的 `SetClientModel` 对齐：仅把事件 JSON 中面向客户端的 `model` / `response.model` **回写为客户端请求 model**；不改 response id、output、usage 等其它字段 |
| 非目标改写 | 不重写 response id、不补 echo 字段、不归一 item 形状、不做 usage 编造 |
| 客户端契约 | 网关对客户端**始终**写 Responses SSE（与 a/c 相同）；强制上游 `stream: true`，不提供同步 JSON 完成体 |

### 1.3 非目标

- 不新增对外路由（不暴露第二套 `/v1/responses` 旁路）
- 不实现非流式 JSON 完成体路径
- 不做 Responses↔Responses 语义重写或兼容层
- 不把 a/c 路径改成经 r 中转
- 不引入 session store / `previous_response_id` 服务端回填（仍由客户端自带完整 `input`；上游若支持则透传字段，网关不代劳）

### 1.4 架构原则对齐

- **形状透传，结果归上游**（AGENTS.md）：网关不代上游拒绝能力、不编造 failed 终态。
- **Backend 接口边界**：选源 / 熔断 / 首字节前 failover 在 scheduler；协议细节在 Backend。
- **Holder 原子配置**：禁止把 `*config.Config` 缓存到 Backend 长生命周期对象。
- **分层单向依赖**：`responsesclient` 不 import `scheduler` / `server` / `admin`。

## 2. 架构概览

```text
客户端 (Responses API)
       │
       ▼
   server.handleResponses
       │  解析 body；写 SSE 头；可选按首源 dry-check
       ▼
   scheduler.ExecuteGeneric(rawBody, onEvent, onUpstream)
       │  runtimeSeq + breaker + 首字节前 failover
       │  按 source.BackendType 选 Backend
       ├─ a ──→ AnthropicBackend（现有）
       ├─ c ──→ ChatBackend（现有）
       └─ r ──→ ResponsesBackend（新）
                 PrepareUpstreamBody（model 映射 + stream=true）
                 → responsesclient.Stream
                 → ScanSSE → model.SSEEvent（Data 原样）
       ▼
   Responses SSE → 客户端
```

### 2.1 分层归属

| 层 | 包 | 职责 |
|---|---|---|
| L0 | `config` / `model` / `breaker` | `BackendOpenAIResponses = "r"`；校验与规范化 |
| L1 | `internal/responsesclient`（新） | Responses HTTP + SSE 扫描、错误体、`ListModels` |
| L2.5 | `backend.ResponsesBackend`（新） | 最小 body 改写 + 调用 L1 + 产出统一 `model.SSEEvent` |
| L3 | `scheduler` | `backendFor` 三分支；`ListUpstreamModels` 对 `r` 走 `/models` |
| L4 | `server` | `/v1/*` 入口；`r` 首源预检只做可 decode + 可改写，不做 ToAnthropic/ToChat |
| L5 | `admin` / `metrics` | 配置读写、观测 `backend_type=r`、试拉 models |

### 2.2 包依赖方向

```text
server → scheduler → backend → responsesclient
backend → config, model
scheduler → breaker, config, backend
admin → config（含 BackendType 读写）
```

`responsesclient` 不得 import `scheduler` / `server` / `admin` / `convert` / `chatconvert`。

## 3. 配置

### 3.1 常量与规范化

```go
const (
	BackendAnthropic         = "a"
	BackendOpenAIChat        = "c"
	BackendOpenAIResponses   = "r"
)

// NormalizeBackendType："" / "a" → a；"c" → c；"r" → r；其它 error。
// validate 时写回规范化值，保证 holder 快照中 BackendType 非空。
```

错误文案：`invalid backend_type %q (allowed: a, c, r)`（与现有风格一致）。

### 3.2 示例源

```yaml
# config.example.yaml（注释示例）
- name: openai-responses
  base_url: https://api.openai.com/v1
  api_key: ${OPENAI_API_KEY}
  backend_type: r
  model_map: { gpt-5: gpt-5 }
  default_model: gpt-5
```

`base_url` 约定与 Chat 一致：写到 API 根（通常含 `/v1`），由客户端代码拼接资源路径，**禁止**在配置里写死完整 `/v1/responses` 作为唯一合法形式（join 逻辑需兼容已带后缀的 base）。

## 4. Backend 接口（不变）

沿用现有 `backend.Backend`：

```go
Execute(
	ctx context.Context,
	rawBody []byte,
	src config.Source,
	cfg *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(UpstreamEvent),
	attempt int,
) error
```

`UpstreamEvent.BackendType` 对 r 路径 **必填** `"r"`（metrics/观测禁止空串）。

说明：`cfg` 入参对 r 路径可忽略（无 Anthropic cache TTL 等依赖）；接口签名与 a/c 保持一致，不为此缩参。

## 5. `internal/responsesclient`

对标 `internal/chatclient`，职责仅限 HTTP/SSE，不感知 Responses 业务语义。

### 5.1 URL

```text
responsesURL(base):
  trim right "/"
  if suffix "/responses" → base
  else → base + "/responses"

modelsURL(base): 与 chatclient 相同逻辑（suffix "/models" 则不重复）
```

### 5.2 Stream

- `POST` responsesURL，body 为已改写的 JSON 字节
- Headers：
  - `Content-Type: application/json`
  - `Authorization: Bearer <api_key>`
  - `Accept: text/event-stream`
- HTTP ≥400：读 body 截断日志，返回 `fmt.Errorf("upstream %d: %s", code, snippet)`（便于 `StatusCodeFromErr` 复用 chat 前缀 `upstream `）
- 成功：返回 `resp.Body`（caller Close）

**超时模型**：与现有 a/c 一致——`http.Client` 不设覆盖整段 body 的总 Timeout；首字节超时由 scheduler 子 ctx + `first_byte_timeout` 取消。若后续为 streaming 单独设 Transport，保持与 chatclient 同策略，不在本设计引入第三套超时语义。

### 5.3 ListModels

与 chatclient 相同：`GET modelsURL`，Bearer，解析 `data[].id`（可选 `display_name`），供管理页下拉。

### 5.4 ScanSSE

解析标准 SSE 帧：

- 累积 `event:` 与 `data:`（多行 data 按 SSE 规范用 `\n` 拼接）
- 空行结束一帧；调用 `onEvent(eventType string, data []byte)`
- 行以 `:` 开头视为注释，忽略
- 若出现 `data: [DONE]`（部分代理兼容），结束扫描，不把 `[DONE]` 当业务事件
- **`eventType` 解析顺序（必须非空才 `onEvent`）**：
  1. SSE 行 `event:` 非空 → 用之
  2. 否则从 data JSON 顶层 `"type"` 取值
  3. 仍无 → **DEBUG 跳过该帧**，禁止把空 `Type` 交给 `writeSSE`（现实现固定写 `event: %s`，空串会破坏 SSE）
- **Scanner 缓冲**：默认 `bufio.Scanner` 64KiB 不够承载 `response.completed` 全量 output。实现须 `Buffer` 放大，**起始 1MiB**，上限至少 8–16MiB（与仓内 SSE 经验一致）；超限帧记错误并失败本源，不截断 silently 当成功

client 层不做业务 JSON 改写（model 回写在 Backend，见 §6.5）。

## 6. `ResponsesBackend`

### 6.1 流程

1. `PrepareUpstreamBody(rawBody, src)` → `upstreamBody, clientModel, resolvedModel`
2. `slog.Info` 记录 source / model / resolved_model / backend_type=r
3. `Client.Stream(ctx, baseURL, apiKey, upstreamBody)`
4. `ScanSSE`：首帧有效 data → locked + TTFB；每帧 `onEvent(model.SSEEvent{Type, Data})`
5. 结束时 `onUpstream`（tokens 见下）

### 6.2 PrepareUpstreamBody

使用 **`map[string]any`** 而非完整 SDK struct 序列化，避免丢弃 SDK 未建模的扩展字段（透传刚需）。

规则：

1. `json.Unmarshal` 到 `map[string]any`；非 object → error
2. 读取 `model`：
   - 非 string 类型 → error
   - 空串或缺省 → 交给 `resolveModel`（依赖 `default_model` 或原值）
3. `resolved := resolveModel(src, clientModel)`，写回 `body["model"] = resolved`
4. `body["stream"] = true`（无论客户端是否省略或 false）
5. `json.Marshal` 返回

不在此层删除或改写其它键（含 `previous_response_id`、`store`、`include`、tools 等）——形状透传，结果归上游。

### 6.3 空流与终态

与 ChatBackend 空流策略对齐：

- **从未收到 data 帧**（`!locked`）：`scanErr = upstream returned no events`（若尚无 err），`onUpstream` status=failed，**禁止**合成 `response.created`/`completed`
- **已 lock**：
  - 正常 EOF → completed（Code 默认 200）
  - 客户端取消 → 若已见 `response.completed`/`incomplete`（`naturalDone`）则 completed，否则 canceled；对齐 Chat 终态后读尾
  - 读错误 → failed，Code 能解析则带上
- **中途失败不强制补 `response.failed`**：上游已发出的事件原样；透传代理不代写终态。文档与 protocol-coverage 注明：半截流由上游/网络决定。

### 6.4 Usage / Tokens 观测

尽力从已转发的事件中解析（不阻塞写出）：

- 优先：data JSON 且 `type` 为 `response.completed`（或 `response.incomplete` 等带 `response.usage` 的终态）时读取 `response.usage`
- 字段映射到 `UpstreamEvent`：`InputTokens` / `OutputTokens`；cache 相关若存在于 usage 则填 `CacheRead`/`CacheCreate`，否则 0
- 解析失败或未出现：tokens 全 0，不编造

实现可在 Scan 循环中轻量 `json.Unmarshal` 仅用于观测，**不得**因观测失败中断流。

Usage 字段名对齐 OpenAI Responses wire（常见：`input_tokens` / `output_tokens`；cache 细节若存在再映射到 `CacheRead`/`CacheCreate`），映射不到则 0。

### 6.5 出站 model 别名回写（T2）

目标：与 a/c 一致，Codex 看到的始终是**客户端请求 model**（映射前别名），而非上游真实 model id。

规则：

1. `clientModel` = 请求改写前的客户端 model 字符串（空则用 resolve 后值，再空则不回写）
2. 每个将要 `onEvent` 的 data 帧：若 JSON object 含顶层 `model` 字符串，改为 `clientModel`
3. 若含 `response` object 且其中有 `model` 字符串，改为 `clientModel`
4. 仅当至少改动一处时才 `json.Marshal` 替换 `Data`；未含 model 的帧（如纯 delta）**保持原 data 字节**
5. 不修改 `id` / `response.id` / `usage` / `output` / 其它字段
6. Marshal 失败 → DEBUG，**保留原 Data 继续转发**（可观测优先于别名完美）

成功标准中的「data 一致」以本节约定为准：允许 model 回写；其余字段不改语义。

## 7. Scheduler / Server

### 7.1 backendFor

```go
func (s *Scheduler) backendFor(src *config.Source) backend.Backend {
	bt, err := config.NormalizeBackendType(src.BackendType)
	if err != nil {
		return s.anthropicBackend // validate 已拒非法；防御性回落 a
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
```

Scheduler 持有三个无状态（或仅含 HTTP client）Backend 实例：`anthropicBackend` / `chatBackend` / `responsesBackend`。

### 7.2 ListUpstreamModels

`backend_type == r` 与 `c` 相同：Bearer + `/models`，结果映射为现有 `anthropicclient.ModelInfo` 形状供管理页复用。可抽公共 helper 或在 r 分支调用 `responsesBackend.Client.ListModels`（与 c 调用 chatBackend.Client 对称）。

### 7.3 Server 首源预检

与现网代码对齐后扩展（**现网 c 路径没有 ToChat 预检**）：

| 首源类型 | 预检（写 SSE 头之前） |
|---|---|
| `a` | 保持现有 `ToAnthropic` dry-run；失败 400 |
| `c` | **无**转换预检（保持现状） |
| `r` | 可选：`PrepareUpstreamBody` dry-run；失败 400；**不做** ToAnthropic/ToChat |

禁止「全局必须能 ToAnthropic」误杀纯 r / 纯 c 部署。

### 7.3.1 `warnDroppedOrIgnoredParams` 与 r（已定）

现状函数按 a/c 能力输出「丢弃/仅 Chat 透传」类 WARN。对 **r** 许多字段会原样进上游，假 WARN 会误导排障。

**规则（推荐实现，已定）：**

1. 扫描当前配置 `sources`（与现函数已接收的 `sources` 参数一致）。
2. 若存在至少一个 **启用且** `backend_type=r` 的源（`disabled!=true`）：
   - 对 **r 可形状透传** 的字段，**不再**按 a 路径「WARN + 丢弃」叙事输出（包括但不限于：`previous_response_id`、`prompt_cache_*`、`service_tier`、`text.verbosity`、`metadata` 整表、`store`、`moderation`、`top_logprobs`、`stream_options`、多数 `include` 项——一律视为可能由 r 上游消化）。
   - 仍可对 **网关产品明确不做** 的能力保留说明性 DEBUG/INFO（例如网关自身无 session store：**不**承诺按 `previous_response_id` 补历史；字段仍透传，由上游决定）。文案必须写清「透传上游，网关不代补会话」，禁止写「对应数据被丢弃」。
3. 若配置中 **没有** r 源：保持现有 a/c 分支 WARN 逻辑不变。
4. 混排（a+c+r）：以「是否存在 r」为准做收口（有 r 则按上条放宽），避免 failover 到 r 时日志仍说字段已丢弃。

实现时可抽 `hasResponsesBackend(sources) bool`，避免散落判断。

### 7.4 Failover

- 首字节前（未调用 `onEvent`）失败 → 可切下一源（含跨 a/c/r）
- 一旦 lock → 不切源（与现语义一致）

## 8. 管理页 / Metrics / 文档

### 8.1 Admin UI

- 接口类型 options：`a` / `c` / `r`
- 文案：`backendAnthropic` / `backendChat` / `backendResponses`（中：Anthropic / OpenAI Chat / OpenAI Responses；英：同）
- 请求列表短码：`A` / `C` / `R`（缺省当 `a`）
- 试拉 models：body 带 `backend_type: r` 时走 Responses/Chat 同源 ListModels 路径
- 表单默认仍 `a`

### 8.2 Metrics

`RequestEvent.BackendType` / 历史记录支持 `r`；序列化非空。

### 8.3 文档

| 文件 | 改动 |
|---|---|
| `config.example.yaml` | 注释示例 `backend_type: r` |
| `README.md` | 与 Chat 节并列「OpenAI Responses 透传上游」 |
| `docs/protocol-coverage.md` | 新增 **Responses 透传专节（backend_type: r）** |
| 本 spec | 实现后将状态改为「已实现」 |

### 8.4 protocol-coverage 专节要点

- 客户端路径仍为 `/v1/responses`
- 网关保证：`model` 映射、`stream=true`、Bearer、SSE 转发、**出站 model 别名回写（T2）**
- 几乎全部 Responses 请求/事件字段对网关而言为 **`supported`（passthrough）**——语义由上游决定
- 网关产品边界（无 session store、无 background 排队实现等）不变：字段若客户端发送则透传，网关不代为实现；文档不得写成「网关丢弃」
- 与 a/c 矩阵 **不共享** 状态行；三路径并列

## 9. 错误矩阵

| 场景 | locked? | onUpstream.Status | failover |
|---|---|---|---|
| DNS/连接失败 | 否 | failed | 可 |
| HTTP ≥400 | 否 | failed（Code=上游码） | 可 |
| 首字节超时（子 ctx 取消，父仍在） | 否 | failed | 可 |
| body 非 JSON object / model 非法 | 否 | failed | 可（转换错误） |
| 空流结束 | 否 | failed | 可 |
| 流中读错误 | 是 | failed | 否 |
| 客户端断开 | 是 | canceled 或 completed | 否 |
| 正常结束 | 是 | completed | — |

## 10. 测试计划

| 包 | 用例 |
|---|---|
| `config` | `r` 规范化；非法值拒绝；validate 写回 |
| `responsesclient` | URL join（含/不含 `/responses`）；mock 200 流；mock 4xx 错误串含 status |
| `backend` | model_map 生效；stream 强制 true；其余字段保留；SSE 顺序；**T2 model 回写**；空流不合成 completed；usage 解析（有则填）；>64KiB 单帧可扫 |
| `scheduler` | `backendFor` 返回 responsesBackend；ListUpstreamModels r 分支 |
| `server` | r 源集成；含 r 时 warn 不误报丢弃 previous_response_id 等；可选 a/c/r 混排一测 |
| `admin` | Normalize/view 含 r；upstream-models 接受 r |

质量门禁：`task check`（或 `go test ./...` + 格式/vet）；涉及 scheduler 并发处按需 `task test-race`。

## 11. 实现顺序

1. `config`：常量 + Normalize + 测试 + example 注释
2. `responsesclient`：URL / Stream / ScanSSE / ListModels + 测试
3. `backend.ResponsesBackend` + PrepareUpstreamBody + 测试
4. `scheduler`：三实例 + backendFor + ListUpstreamModels
5. `server`：首源预检分支 + `warnDroppedOrIgnoredParams` r 收口 + 集成测
6. `admin` / metrics / i18n 文案
7. `README` + `protocol-coverage` 专节
8. 全量 `task check`；本 spec 状态改为已实现

## 12. 文件结构（预期）

| 文件 | 改动 |
|---|---|
| `internal/config/config.go` | `BackendOpenAIResponses`、Normalize、校验文案 |
| `internal/config/backend_type_test.go` | 表驱动补 r |
| `config.example.yaml` | 示例源 |
| `internal/responsesclient/client.go` | 新建 |
| `internal/responsesclient/client_test.go` | 新建 |
| `internal/backend/responses.go` | 新建 ResponsesBackend |
| `internal/backend/responses_test.go` | 新建 |
| `internal/scheduler/scheduler.go` | 持有 responsesBackend、backendFor、ListModels |
| `internal/server/server.go` | 首源预检 |
| `internal/server/server_test.go` | r 集成 |
| `internal/admin/*` + `assets/index.html` | 选项与展示 |
| `internal/metrics/metrics.go` | 注释/展示兼容 r（字段已是 string） |
| `README.md` / `docs/protocol-coverage.md` | 文档 |

## 13. 风险与明确决策

| 风险 | 决策 |
|---|---|
| SDK 往返丢失扩展字段 | 请求侧用 map 语义透传，不用完整 ResponseNewParams 再 Marshal |
| 请求非字节保真 | 接受；成功标准按语义键保留，不宣称请求 body 字节级一致 |
| 半截流无 failed 事件 | 接受；透传不代写终态（与纯代理一致） |
| 部分上游无 event 行 | 从 data.`type` 回填；仍无则跳过帧，禁止空 Type |
| 大包 completed | Scanner 缓冲 ≥1MiB（上限 8–16MiB） |
| 出站 model 与 a/c 不一致 | **T2**：回写客户端 model（§6.5） |
| a/c 风格 WARN 误杀 r 可透传字段 | 配置含启用中的 r 源时放宽 WARN（§7.3.1） |
| `[DONE]` 非官方 | 兼容结束扫描，不当业务事件 |
| 与 c 的 ListModels 重复 | 允许两 client 各有一份简单实现（YAGNI） |
| wire 短码 | 固定 `r`（responses），与 `a`/`c` 一致单字母 |

## 14. 成功标准

- 配置 `backend_type: r` 的源可被调度，观测显示 `r`/`R`
- 上游收到的 JSON：除 `model` 与 `stream` 外与客户端**语义**一致（map 键保留；允许 Marshal 重排/数字形态变化）
- 客户端 SSE：除 **T2 model 别名回写** 外，data 与上游一致；`event` Type 非空且可被 `writeSSE` 合法写出
- 配置含 r 时，`warnDroppedOrIgnoredParams` 不对 r 可透传字段误报「数据被丢弃」
- 空流不锁定源为「假成功」
- 大包 `response.completed`（>64KiB）可完整扫描，不因默认 Scanner 限制失败
- a/c/r 混排时，首字节前失败可跨类型 failover
- `task check` 通过

---

**附录：与 Chat 后端设计的关系**

本设计是 Chat 设计 §1.1「第 3 种 backend 实现（接口预留）」的落地规格，不取代 Chat 设计文档。接口、调度、观测约定与 2026-07-21 Chat 方案 A 保持一致，仅协议路径不同（透传 vs 转换）。
