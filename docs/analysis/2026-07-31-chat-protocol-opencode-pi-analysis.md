# Chat 协议处理对照分析：opencode / pi vs codex-api-gateway

> 日期：2026-07-31
> 范围：`backend_type: c`（Responses → Chat Completions → Responses SSE）
> 参考：opencode `packages/llm/src/protocols/openai-chat.ts`、pi `packages/ai/src/api/openai-completions.ts` / `transform-messages.ts`

## 1. 目标与边界

本报告对照 opencode 与 pi 对 OpenAI Chat 协议的处理方式，梳理我们 `internal/chatconvert` + `internal/chatstreamconv` + `internal/backend/chat.go` 的差异。

> 更新（2026-07-31 实现完成）：P0/P1 已按下文批次全部落地并提交，本节“不含实现”不再适用；剩余仅 P2 产品边界项与 S6 待确认项。

沿用仓库两条硬约束：

- 网关是纯协议转换层，只做 wire 对齐，不替上游判能力（形状透传，结果归上游）。
- 只有协议无法映射时才允许转换错误 / WARN 丢弃 / 矩阵登记降级。

## 2. 参考实现摘要

### 2.1 opencode

- `openai-chat.ts` 是 opencode 的通用 Chat 协议适配器，`openai-compatible-chat.ts` 仅复用同一协议，不改行为。
- 请求 body 字段：`model`、`messages`、`tools`、`tool_choice`、`stream`、`stream_options.include_usage`、`store`、`reasoning_effort`、`max_tokens`、`temperature`、`top_p`、`frequency_penalty`、`presence_penalty`、`seed`、`stop`。
- 消息 schema 只有 `system` / `user` / `assistant` / `tool`，没有 `developer`；`assistant` 支持 `reasoning_content`。
- 工具 schema 在出站前做 OpenAI 投影：展平 `anyOf`、去 null schema、强制 `additionalProperties:false`。
- 流式按 `tool_calls[].index` 聚合；`id`/`name` 缺失时报错；`finish_reason` 到达时立即解析全部 arguments。
- usage 把 `prompt_tokens` 当作包含 cache 的总量，派生出 non-cached input。

### 2.2 pi

- `openai-completions.ts` 面向 OpenAI 兼容 Chat，额外带大量厂商兼容开关。
- 请求侧：developer/system 按模型能力选择；工具结果中的图片转 `image_url` 追加为 user 消息；工具调用 id 归一化到 Chat 允许字符/长度；无活动工具但有工具历史时仍发 `tools: []`。
- 消息规范化：孤儿 tool call 自动补 `No result provided`；`stopReason=error/aborted` 的 assistant 整条跳过；assistant 无文本且无 tool_calls 时跳过。
- 流式回程：支持 `reasoning_content` / `reasoning` / `reasoning_text` 三别名；顶层无 usage 时读 `choice.usage`；支持 Chat `custom` 工具增量（grammar 场景）。

## 3. 对照清单

编号用于后续逐项过。状态列含义：`一致`（我们已对齐）、`可做`（建议候选）、`边界`（产品边界外，先登记）。

### 3.1 请求构造

| # | 差异点 | opencode | pi | 我们 | 状态 |
|---|---|---|---|---|---|
| R1 | `frequency_penalty` / `presence_penalty` / `seed` / `stop` | body 支持并下发 | 部分通过 options 下发 | Responses 请求无对应顶层字段，无法透传；矩阵已登记为 Chat-only | 边界 |
| R2 | `top_k` | Chat 协议不发送（仅 Anthropic/Bedrock 用） | 不发送 | 矩阵声称 Chat 同名透传，但 `ChatRequest` 无该字段，SDK `ResponseNewParams` 也无 `TopK` | 已修正（D1） |
| R3 | 工具 schema 投影 | 出站前 `ToolSchemaProjection.openAI` 展平 anyOf / 去 null / 强制 additionalProperties | 原样透传 parameters | 保守投影：递归移除 type/anyOf 中的 null | 已实现 |
| R4 | `developer` 角色 | 无 developer，统一 system | 按模型能力选择 developer/system | `chatRole` 压成 system | 一致 |
| R5 | user 图片 | `image_url` 支持 | 支持并按模型能力降级占位 | 文本占位 `[image input omitted]` | 已实现（降级） |
| R6 | tool result 图片 | 图片收集后追加 user 消息 | 同上 | 文本占位 `[image output omitted]` | 已实现（降级） |
| R7 | 空 assistant 消息 | 发送 `content:null` | 无内容且无 tool_calls 时跳过 | 空文本且无 tool_calls 跳过 | 已实现 |
| R8 | tool_call_id 归一化 | 原样 | 归一化长度/非法字符 | 出站归一化 `[a-zA-Z0-9_-]` <=40 且请求内唯一 | 已实现 |
| R9 | 有工具历史但无活动工具时 `tools` | 不发 | 发 `tools: []`（Anthropic 代理要求） | 不发 | 边界 |
| R10 | Chat `custom` 工具 + grammar | 不支持 | `type:custom` + grammar | 统一 function 化 | 边界 |

### 3.2 消息与工具环

| # | 差异点 | opencode | pi | 我们 | 状态 |
|---|---|---|---|---|---|
| M1 | 孤儿 tool call 补结果 | 无专门处理 | 自动插 `No result provided` | `ensureChatToolPaired` 补占位 + 重排 | 一致 |
| M2 | error/aborted assistant 跳过 | 无 | 整条跳过 | Responses 输入无 stopReason 概念，不适用 | 边界 |
| M3 | 时序 system 更新 | 包 `<system-update>` 折 user 并合并 | 不做 | 相邻 system 合并，紧跟 user 的折入 user；assistant/tool 后保持原位 | 已实现 |
| M4 | tool 结果后接 user 时插 assistant | 无 | `requiresAssistantAfterToolResult` 兼容开关 | 无 | 边界 |
| M5 | 历史 reasoning 回灌 | assistant `reasoning_content` | 支持 signature / reasoning_details | assistant `reasoning_content`（无 signature/encrypted） | 一致 |

### 3.3 流式回程

| # | 差异点 | opencode | pi | 我们 | 状态 |
|---|---|---|---|---|---|
| S1 | reasoning 字段别名 | 仅 `reasoning_content` | `reasoning_content` / `reasoning` / `reasoning_text` | 三别名齐备 | 已实现 |
| S2 | `choice.usage` 兜底 | 无 | 顶层无 usage 时读 choice.usage | 顶层优先，choice.usage 兜底 | 已实现 |
| S3 | tool delta 缺 id/name | 缺失即报错 | 先建空块后补 | id 合成 `call_{index}`，name 到齐再 open | 一致（更宽松） |
| S4 | tool arguments 校验 | finish 时严格 parse，失败即 error | 流式 parseStreamingJson | 原样透传 + `SanitizeClientToolInput` 可控修复 | 一致（按历史约定） |
| S5 | finish_reason 映射 | `function_call` / `tool_calls` 都映射 tool-calls | 同 opencode 语义 | `function_call` 显式归 completed | 已实现 |
| S6 | usage 语义 | prompt 总量派 non-cached input | 同 | 直接把 prompt_tokens 作为 InputTokens，另写 cache 字段 | 待确认 |
| S7 | 空流/终态锁定 | `onHalt` 保证工具事件先于终态 | 无 finish_reason 时报错 | locked + FeedDone 条件，避免空流误锁 | 一致 |
| S8 | logprobs | 不支持 | 不支持 | 支持 content logprobs | 我们更强 |
| S9 | `stream_options.include_obfuscation` | 不支持 | 不支持 | 支持透传 | 我们更强 |

### 3.4 配置与矩阵

| # | 差异点 | 说明 |
|---|---|---|
| D1 | `docs/protocol-coverage.md` Chat 请求矩阵 `top_k` 行 | 已改为 `none`：客户端 Responses 无来源，Chat 请求体亦无对应字段 |
| D2 | `developer→system` | 与 opencode 一致，矩阵已登记，保持 |
| D3 | 多模态 / custom grammar / deferred tools | 已登记为产品边界，本次不改 |

## 4. 差异点明细

### R2 top_k 文档错误

`docs/protocol-coverage.md:271` 写着 `top_k → top_k → supported`，但：

- `internal/chatconvert/request.go` 的 `ChatRequest` 没有 `TopK` 字段（`request.go:18`）；
- openai-go v3.42.0 `ResponseNewParams` 只有 `Temperature` / `TopP`，没有 `TopK`（`response.go:25728-25738`）；
- opencode 的 Chat body 也不发 `top_k`，只有 Anthropic/Bedrock 路径使用。

结论：客户端 Responses 请求不存在 `top_k` 来源，矩阵行应删除或改为 `none / 无客户端来源`。
已处理：`docs/protocol-coverage.md` 该行改为 `none` 并注明原因。

### R3 工具 schema 投影

opencode `ToolSchemaProjection.openAI`（`tool-schema.ts:48-62`）会把：

- `anyOf` 中非 null 变体合并为 `properties`；
- 外层强制 `type: object` + `additionalProperties: false`；
- 递归移除 `anyOf` 中的 null 类型。

我们 `toolUnionToChat`（`request.go:1102`）对 function / namespace 工具原样透传 `parameters`。严格上游可能对 null anyOf / 缺少 type 报 400。这与“形状透传”不完全冲突：投影是 wire 归一，不是替上游判能力。是否做需要确认（见待确认问题）。
已处理：新增 `normalizeToolSchema`，递归移除 `type` 数组与 `anyOf` 中的 null 变体（保守版，不做 additionalProperties 强制与 anyOf 合并）；function / namespace function / tool_search 均套用。

### R7 空 assistant 消息

- pi `convertMessages` 对无文本且无 tool_calls 的 assistant 直接跳过（`openai-completions.ts:1183-1198`）。
- 我们 `convertEasyMessage` 对空文本 assistant 仍返回消息（`request.go:626-635`），`convertOutputMessage` 也无条件返回（`request.go:697-708`）。

风险：严格 Chat 上游可能对空 assistant 400。候选修复：空文本且无 tool_calls 的 assistant 跳过，或统一 `content: null`。
已处理：`convertEasyMessage` / `convertOutputMessage` 对空文本 assistant 返回 `ok=false` 跳过。

### R8 tool_call_id 归一化

pi `normalizeToolCallId`（`openai-completions.ts:1006-1028`）会把 Responses 生成的超长 / 含 `|` 等非法字符的 id 截断为 Chat 允许的 `[a-zA-Z0-9_-]`（<=40 字符），并保留 item 级唯一性。

我们 `appendToolCall`（`request.go:280-294`）原样透传历史 call id。风险：部分 Chat 上游（如 JD/DeepSeek）对 id 字符/长度敏感。候选修复：出站归一化；回程不还原（客户端以网关返回的 id 继续对话）。
已处理：`normalizeToolCallIDs` 在 `convertMessages` 后统一归一化 assistant.tool_calls 与 role=tool 的 ToolCallID，非法字符转 `_`、截断 40、碰撞追加 `_N`；回程不还原。

### S1 reasoning 别名

pi 按 `reasoning_content` / `reasoning` / `reasoning_text` 顺序取第一个非空（`openai-completions.ts:488-491`）。我们只认前两个（`converter.go:128-133`、`converter.go:348`）。`reasoning_text` 是 llama.cpp 等本地兼容端点常见别名，候选补齐。
已处理：`chatChunk` delta 增加 `reasoning_text` 并纳入 `feedReasoning`。

### S2 choice.usage 兜底

pi 在顶层无 `usage` 时读 `choice.usage`（Moonshot 等兼容端点）（`openai-completions.ts:454-456`）。我们 `Feed` 只读 `chunk.Usage`（`converter.go:329`）。候选：`choice.usage` 作为兜底。
已处理：`Feed` 顶层 usage 缺失时读 `choice.Usage` 兜底。

### S5 finish_reason 显式识别

opencode 把 `function_call` / `tool_calls` 都映射为 tool-calls（`openai-chat.ts:378-392`）。我们 `statusForFinish`（`converter.go:1081-1095`）没有 `function_call` 分支，落到 completed。行为上仍是成功终态，影响很小；候选把 `function_call` 显式加入，避免未来误判。
已处理：`statusForFinish` 增加 `function_call` 显式 completed 分支。

### S6 usage 语义

opencode/pi 把 `prompt_tokens` 视为含 cache 的总量，派生 non-cached input；我们直接把 `prompt_tokens` 写进 `InputTokens`，同时写 `CacheReadInputTokens`。Responses 侧 `usage` 字段语义是否要求扣减，需要与客户端契约核对后决定，暂不改。

## 5. 建议批次

### P0 文档与低风险正确性（已完成）

- D1：修正 `top_k` 矩阵行。
- S1：`chatstreamconv` 增加 `reasoning_text` 别名。
- S2：`chatstreamconv` 增加 `choice.usage` 兜底。
- R7：空 assistant 消息跳过（需确认是否影响客户端回灌语义）。
- S5：`function_call` finish_reason 显式识别。

### P1 兼容性增强（已完成）

- R8：tool_call_id 出站归一化（先补测试，再决定是否全量启用）。
- R3：工具 schema 投影（保守版：仅对 null anyOf 做归一；严格模式可后续开）。
- M3：多条 system 消息合并或折 user（可选，低风险）。
- R5/R6：图片降级占位文本（不真传图，避免上游 400）。

### P2 产品边界外

- R1：frequency_penalty 等如需支持，只能走源配置默认值，不能从 Responses 客户端透传。
- R9：Anthropic 代理型 Chat 上游的 `tools: []` 场景。
- R10：Chat `custom` + grammar。
- M4：`requiresAssistantAfterToolResult` 兼容开关。

## 6. 待确认问题

1. ~~R3 工具 schema 投影：是否接受 wire 归一，还是严格保持原样透传？~~ 已按保守投影落地。
2. ~~R7 空 assistant 跳过：历史回灌中空 assistant 是否允许丢弃？~~ 已落地（空文本且无 tool_calls 才跳，不影响工具环）。
3. ~~R8 id 归一化：是否接受网关返回的 call_id 与原客户端 id 不一致（后续以网关返回为准）？~~ 已落地（出站归一化，回程不还原）。
4. S6 usage：Responses `InputTokens` 是否需要从 `prompt_tokens` 扣掉 cache，还是保持总量？（未处理，保持现状）
5. ~~本次范围：只做 P0，还是 P0+P1 一起？~~ 已按 P0+P1 完成。
