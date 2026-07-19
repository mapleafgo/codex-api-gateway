# CodexApiGateway 设计文档

- **日期**: 2026-07-14
- **状态**: 设计已确认，待实现
- **技术栈**: Go

## 1. 概述

CodexApiGateway 是一个面向 **Codex CLI** 的协议适配 + 容灾网关服务。Codex 原生只支持 OpenAI Responses API；本服务作为中间层，把 Codex 的 Response 协议请求转换为 **Anthropic Messages 协议**，转发给一组 Anthropic 兼容后端源，并把响应**流式转回 Response 协议**。通过多源主备 failover + 熔断实现高可用。

一句话：**让 Codex 能用任意 Anthropic 兼容后端（官方 Claude、智谱/Kimi 等的 Anthropic 接口），并在多源间主备容灾。**

## 2. 背景与动机

- Codex CLI（`codex-rs`）原生只支持 OpenAI Responses API，且强制 `stream: true`。
- 用户希望 Codex 能接入 Anthropic 兼容的后端模型。
- 单一后端不稳定，需要多源容灾；但不能多源并发生成（会造成 token 浪费），故采用**优先级主备 + 故障接力（failover）**。

## 3. 范围

### MVP 包含

- 单一 **Response ↔ Anthropic Message** 协议转换（请求 + 流式响应）
- 流式 SSE（Codex 强制 `stream:true`，**无非流式路径**）
- 工具调用（function calling）
- reasoning：Response reasoning ↔ Anthropic `thinking` block（含 `signature` 加密回传）
- 多模态图片输入
- structured output（`text.format` → Anthropic tool-use 约束）
- 会话状态（SessionStore，按 `response_id` enrich 补全有效上下文）
- 多源主备 failover + 熔断

### 非目标（MVP 不做）

- Chat Completion 协议后端（已从范围移除，简化为单协议）
- 非流式响应路径
- 多源并发/竞速生成（避免 token 浪费）
- 配置热加载（启动时加载即可）
- 会话状态持久化（Badger 落盘，重启后可恢复未过期上下文）
- OpenAI Response 内置工具（web_search / file_search / code_interpreter 等，Codex 用自己的工具）

## 4. 总体架构

```
Codex ──POST /v1/responses (stream:true)──► ① 入口层
                                                │
                                                ▼
                                    ② SessionStore enrich
                                    (previous_response_id → 补全 input + output 上下文)
                                                │
                                                ▼
                                    ③ 转换层 Response → Anthropic Message
                                                │
                                                ▼
                                    ④ 调度层 failover + 熔断
                                    (按优先级选健康源, 首字节锁定)
                                                │
                                                ▼
                                    ⑤ AnthropicConnector → POST /v1/messages
                                                │
                              ┌─────────────────┴─────────────────┐
                              ▼                                    ▼
                       Anthropic 官方 Claude              智谱/Kimi 等 Anthropic 兼容
                              │                                    │
                              └────────────────┬───────────────────┘
                                               ▼
                               ③ 流式转换 Anthropic SSE → Response SSE
                                               │
                                               ▼
                                    ② 存本轮有效 input + output → 流式转发 Codex
```

### 一次请求的生命周期

1. Codex 发 `POST /v1/responses`（Response 协议，`stream:true`）
2. 入口层接收
3. **SessionStore enrich**：若带 `previous_response_id`，查回历史有效上下文，补全到当前 input 前面
4. **转换层**：Response 请求 → Anthropic Message 请求
5. **调度层**：按优先级选首个健康源，发起流式请求
6. **首字节前容灾切换窗口**：主源在首字节超时内未返回有效事件 / 报错 → 切备源（此刻 Codex 零字节，无感）
7. 收到首事件 → **锁定该源**，`StreamConverter` 把 Anthropic SSE 转成 Response SSE
8. 流式转发给 Codex；同时 SessionStore 存本轮有效 input + output

## 5. 组件设计

### 5.1 入口层（server）

- HTTP server，暴露 `POST /v1/responses`
- 接收 Response 协议请求，强制按流式处理
- 串联：SessionStore → 转换 → 调度 → Connector → 流式转换 → 转发

### 5.2 协议转换层（convert）

以 **Response 为中心枢纽**（语义最完整），与 Anthropic Message 直接互转，唯一一对转换器。

#### 请求映射（Response → Anthropic Message）

| Response | → Anthropic Message |
|---|---|
| `instructions`（system） | 顶层 `system` |
| `input[].message`（user/assistant） | `messages` |
| `input[].reasoning` | `thinking` block（含 `signature`） |
| `reasoning.effort` | `thinking.budget_tokens`（数值映射表） |
| `input[].function_call` | assistant content 的 `tool_use` block |
| `input[].function_call_output` | user msg 的 `tool_result` block |
| `tools[]` | `tools[]` + `input_schema` |
| `tool_choice` / `parallel_tool_calls` | 直接映射 |
| `text.format`（json_schema） | tool-use 约束（把 schema 包成一个 tool 的 `input_schema`，`tool_choice` 强制调用，拿到结构化 JSON） |
| `input_image` | `image` block（base64 / url） |
| `max_output_tokens` | `max_tokens` |

**要点**：
- `call_id` 对应关系必须保持（Response `function_call.call_id` ↔ Anthropic `tool_use_id` / `tool_result.tool_use_id`），这是工具循环不崩的关键。
- `reasoning.effort`（minimal/low/medium/high）→ `thinking.budget_tokens`（如 1024 / 8000 / 16000 / 32000），全局一张表，可被源覆盖。
- enrich 发生在协议转换**之前**（在 Response 的 items 层面补全），转换器永远收到完整 Response input。

#### 流式映射（Anthropic SSE → Response SSE）

由 `StreamConverter` 状态机处理：

| Anthropic 事件 | → Response 事件 |
|---|---|
| `message_start` | `response.created` + `output_item.added`（message） |
| `content_block_delta`（text_delta） | `response.output_text.delta` |
| `content_block_delta`（thinking_delta） | `response.reasoning.delta` |
| `content_block_start`（tool_use） | `response.output_item.added`（function_call） |
| `content_block_delta`（input_json_delta） | `response.function_call_arguments.delta` |
| `content_block_stop` | `*.done` + `response.output_item.done` |
| `message_delta`（stop_reason + usage） | 记录 stop_reason / usage，映射到收尾 `status`（`end_turn`→completed；`max_tokens` / `stop_sequence`→incomplete） |
| `message_stop` | `response.completed`（带 status + usage） |

**工具调用增量组装**：`tool_use` 的 `id`/`name` 在 `content_block_start`，参数在 `input_json_delta` 分片到来。状态机按 block index 累积，结束时发 `function_call_arguments.done`。

**只支持流式**：转换层没有非流式路径，入口和后端都强制 `stream:true`。

### 5.3 容灾调度层（scheduler）

#### per-source 熔断状态机

```
   closed(正常) ──连续失败达阈值──► open(熔断,拒绝新请求)
        ▲                              │
        │成功                          │ 冷却时间到
        │                         half-open(放 N 个探测请求)
        │                              │
        └────────成功──────────────────┘
                                       失败 ──► open
```

#### 调度流程

1. 从 enrich 后的完整 Response 请求出发
2. 按优先级取首个 `closed`/`half-open` 源；无可用源 → 向 Codex 发 error
3. 转成 Anthropic 协议，`stream:true`，发起请求
4. 等首个有效 SSE 事件（首字节超时 `T`）
   - **超时 / 报错** → 标记该源失败（计数 +1，可能触发熔断）→ 取下一个健康源重来（此刻 Codex 零字节）
   - **收到首事件** → 锁定该源 → `StreamConverter` 转发给 Codex；中途失败只能向 Codex 发 error（LLM 无状态，无法接续）

#### 参数（全部可配置：全局默认 + per-source 覆盖）

| 参数 | 含义 | 建议默认 |
|---|---|---|
| `first_byte_timeout` | 等首个有效事件的超时 | 12s（思考模型 TTFT 长） |
| `failure_threshold` | 连续失败次数触发熔断 | 5 |
| `cooldown` | open 持续时间 | 30s |
| `half_open_probes` | half-open 放行的探测请求数 | 1 |

**全失败**：所有源都 `open` 或逐一试失败 → 向 Codex 发 `response.failed` / error 事件，带原因。

### 5.4 会话状态层（store / SessionStore）

**为什么必须有状态**：Codex 工具循环的第二轮请求往往只发 `previous_response_id` + `function_call_output`，不带上一轮的 `function_call` 和 reasoning。而 Anthropic 后端要求 tool result 前必须紧跟对应的 `tool_use`（最好带 thinking 保证推理连续）。不补全则工具循环第二轮必因上下文残缺失败。

**职责**：按 `response_id` 缓存每轮有效上下文，供后续请求 enrich 补全。

**存储内容**：本轮实际送入模型的 input items，加上本轮 response output items（`message` / `thinking`（含明文 + `signature`）/ `function_call`）。这模拟 OpenAI `previous_response_id` 链式状态，而不是只回填上一轮输出。

**enrich 流程**：请求带 `previous_response_id` → 查回该 id 对应历史上下文 → 补全到当前 input 前面 → 交给转换层。

**thinking `signature` 跨源失效**：`signature` 是后端特定加密签名。failover 切到不同 Anthropic 兼容后端时，旧 signature 不被接受 → 该 thinking 丢弃（不回传），避免 `Invalid signature` 错误。因此 enrich 时需感知「当前目标源」与「历史 reasoning 签名所属源」是否一致。

**存储与过期**：Badger v4 落盘保存上下文，写入时使用 Badger entry TTL；内存 `map` + `container/list` 只维护 LRU/字节预算索引。超过字节预算时按 LRU 删除 Badger key。

### 5.5 AnthropicConnector（anthropic）

- 向 Anthropic 兼容后端发 `POST /v1/messages`（`stream:true`）
- 返回 SSE 流，供 `StreamConverter` 消费
- 管理 per-source 的 base_url / api_key / model_map

## 6. 配置模型（YAML）

```yaml
server:
  listen: ":8383"

session:
  path: data/session
  ttl: 1h
  max_bytes: 67108864
  max_entry_bytes: 2097152

breaker:                       # 全局默认，每个源可覆盖
  first_byte_timeout: 12s
  failure_threshold: 5
  cooldown: 30s
  half_open_probes: 1

thinking:                      # effort → budget_tokens，全局默认可被源覆盖
  effort_budget:
    minimal: 1024
    low: 8000
    medium: 16000
    high: 32000

sources:
  - name: anthropic-official
    priority: 1
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_KEY}
    model_map: { gpt-5: claude-sonnet-4-20250514 }

  - name: glm-anthropic
    priority: 2
    base_url: https://open.bigmodel.cn/api/anthropic
    api_key: ${GLM_KEY}
    model_map: { gpt-5: glm-4.6 }
    breaker: { first_byte_timeout: 20s }   # 覆盖全局
```

- 所有源协议固定为 Anthropic Messages（无需 `protocol` 字段）。
- `model_map`：Response 请求的 `model` → 该后端实际 model。
- `api_key` 支持 `${ENV}` 环境变量插值。
- 启动时加载配置（MVP 不做热加载）。

## 7. 技术栈与项目结构（Go）

```
cmd/server/main.go              # 入口
internal/
  config/                       # YAML 加载 + 校验 + 环境变量插值
  server/                       # HTTP 入口, /v1/responses handler
  convert/                      # Response ↔ Anthropic 双向转换
  stream/                       # Anthropic SSE → Response SSE 状态机
  scheduler/                    # failover + 熔断状态机
  store/                        # SessionStore (enrich)
  anthropic/                    # AnthropicConnector (HTTP client)
```

选 Go 的理由：goroutine + channel + context 的并发原语契合 failover + 流式中继 + 熔断场景；性能与开发速度平衡；单二进制部署。

## 8. 错误处理

| 场景 | 处理 |
|---|---|
| 单源首字节超时 / 报错 | 切下一个健康源（Codex 零字节，无感） |
| 锁定后源中途失败 | 向 Codex 发 error 事件（无法接续） |
| 所有源失败 | 向 Codex 发 `response.failed`，带原因 |
| thinking `signature` 跨源失效 | 该 thinking 丢弃，不回传 |
| 协议转换异常 | 记录结构化日志，向 Codex 发 error |
| `previous_response_id` 找不到历史 | 视为首轮，不补全（降级，不报错） |

## 9. 可观测性

- 结构化日志，每请求一条 trace：选中源、是否切换、熔断状态、耗时。
- 后续可加 metrics（per-source 成功率、延迟、熔断次数）。

## 10. 关键设计决策

1. **单 Anthropic 协议**：从最初的双协议（含 Chat Completion）简化为单协议，转换层只剩一对转换器，流式只剩一套状态机。
2. **只支持流式**：Codex 强制 `stream:true`，非流式路径用不上，去掉以简化。
3. **主备 failover，不并发生成**：避免多源同时生成造成 token 浪费。
4. **首字节锁定**：流式下容灾切换的唯一安全点在「首字节前」；一旦向 Codex 发出字节就锁定源，中途不可切换（LLM 无状态无法接续）。
5. **Response 为转换中心**：Response 协议语义最完整，作为内部中心模型，与 Anthropic 直接互转。
6. **SessionStore 必须有状态**：工具循环第二轮请求缺上下文，必须按 `response_id` enrich 补全有效 input + output 上下文。
7. **thinking `signature` 加密回传**：Anthropic thinking block 自带 `signature`，天然加密回传，对应 Response 的 encrypted reasoning；跨源失效则丢弃。

## 11. 参考实现

- **cc-switch**（`farion1231/cc-switch`）：`src-tauri/src/proxy/providers/transform_codex_chat.rs`（Codex Responses ↔ Chat 转换）、`CodexChatHistoryStore`（会话状态 enrich）、`streaming_codex_chat.rs` 的 `ChatToResponsesState`（流式状态机）、`CodexToolContext`（tool 命名空间映射）。本设计的 SessionStore enrich 思路直接借鉴 cc-switch。
- **协议规范**：Anthropic Messages API、OpenAI Responses API。

## 12. 后续（post-MVP）

- 会话状态持久化已使用 Badger；后续可按运维需要替换为 Redis 等外部存储
- 配置热加载（SIGHUP 或文件 watch）
- metrics + 告警
- Chat Completion 协议后端（若未来需要，按本设计早期的双协议方案扩展）
