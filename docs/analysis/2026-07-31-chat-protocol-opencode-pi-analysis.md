# Chat 协议处理对照分析：opencode / pi vs codex-api-gateway

> 日期：2026-07-31
> 范围：`backend_type: c`（Responses → Chat Completions → Responses SSE）
> 参考：opencode `packages/llm/src/protocols/openai-chat.ts` + `shared.ts` + `utils/tool-schema.ts`
> 说明：本报告保留 pi 行为仅作历史对照；实现依据只有 opencode `packages/llm` 新协议引擎，
> `packages/opencode/src/provider/transform.ts` 旧 AI SDK 层与 pi 实现不再作为参照（旧行为已删除）。

## 1. 目标与边界

本报告对照 opencode 与 pi 对 OpenAI Chat 协议的处理方式，梳理我们 `internal/chatconvert` + `internal/chatstreamconv` + `internal/backend/chat.go` 的差异。

> 更新（2026-07-31 第三轮，opencode 逐帧对齐）：图片透传 `image_url`、工具结果图片聚合独立 user 消息、
> 时序 system 更新折 `<system-update>` user、工具 schema 完整投影（anyOf 展平 + 强制 object/additionalProperties）、
> `reasoning_effort` 任意值透传、assistant `content:null` / tool `content:""` 均已落地；剩余仅 P2 产品边界项（R1/R10/M4）登记不处理。

沿用仓库两条硬约束：

- 网关是纯协议转换层，只做 wire 对齐，不替上游判能力（形状透传，结果归上游）。
- 只有协议无法映射时才允许转换错误 / WARN 丢弃 / 矩阵登记降级。

## 2. 参考实现摘要

### 2.1 opencode

- `openai-chat.ts` 是 opencode 的通用 Chat 协议适配器，`openai-compatible-chat.ts` 仅复用同一协议，不改行为。
- 请求 body 字段：`model`、`messages`、`tools`、`tool_choice`、`stream`、`stream_options.include_usage`、`store`、`reasoning_effort`、`max_tokens`、`temperature`、`top_p`、`frequency_penalty`、`presence_penalty`、`seed`、`stop`。
- 消息 schema 只有 `system` / `user` / `assistant` / `tool`，没有 `developer`；`assistant` 支持 `reasoning_content`。
- `request.system`（如 instructions）是唯一的 `role=system`；时序 `Message.system` 更新用 `wrapSystemUpdate`
  包成 `<system-update>\n<XML 转义文本>\n</system-update>` 折入 user（前一条 user 或独立 user 消息）。
- user 图片转 `{type:"image_url",image_url:{url}}`；工具结果中的图片收集后追加独立 user 消息；
  纯文本 user 仍是 string，含图片才用 content part 数组。
- 工具 schema 在出站前做 OpenAI 投影：展平 `anyOf`、去 null schema、强制 `additionalProperties:false`。
- 流式按 `tool_calls[].index` 聚合；`id`/`name` 缺失时报错；`finish_reason` 到达时立即解析全部 arguments。
- usage 把 `prompt_tokens` 当作包含 cache 的总量，派生出 non-cached input。

### 2.2 pi

> 仅作历史对照，不作为实现依据（用户明确“旧的我们就不要了”）。

- `openai-completions.ts` 面向 OpenAI 兼容 Chat，额外带大量厂商兼容开关。
- 请求侧：developer/system 按模型能力选择；工具结果中的图片转 `image_url` 追加为 user 消息；工具调用 id 归一化到 Chat 允许字符/长度；无活动工具但有工具历史时仍发 `tools: []`。
- 消息规范化：孤儿 tool call 自动补 `No result provided`；`stopReason=error/aborted` 的 assistant 整条跳过；assistant 无文本且无 tool_calls 时跳过。
- 流式回程：支持 `reasoning_content` / `reasoning` / `reasoning_text` 三别名；顶层无 usage 时读 `choice.usage`；支持 Chat `custom` 工具增量（grammar 场景）。

## 3. 对照清单

编号用于后续逐项过。状态列含义：`一致`（我们已对齐）、`可做`（建议候选）、`边界`（产品边界外，先登记）。

### 3.1 请求构造

| # | 差异点 | opencode | pi | 我们 | 状态 |
|---|---|---|---|---|---|
| R1 | `frequency_penalty` / `presence_penalty` / `seed` / `stop` | body 支持并下发（正常路径无人设置） | 不发送 | Responses 请求无对应顶层字段，无法透传；矩阵已登记为 Chat-only | 边界 |
| R2 | `top_k` | Chat 协议不发送（仅 Anthropic/Bedrock 用） | 不发送 | 矩阵声称 Chat 同名透传，但 `ChatRequest` 无该字段，SDK `ResponseNewParams` 也无 `TopK` | 已修正（D1） |
| R3 | 工具 schema 投影 | 出站前 `ToolSchemaProjection.openAI` 展平 anyOf / 去 null / 强制 additionalProperties | 原样透传 parameters | 完整投影：anyOf record 变体展平进 properties、递归去 null、强制 `type=object` + `additionalProperties=false` | 已实现（完整对齐 opencode） |
| R4 | `developer` 角色 | 会话中折 `<system-update>` user；仅 `request.system` 是 system | 按模型能力选择 developer/system | `instructions` 是唯一 system；developer/system item 折 `<system-update>` user | 已实现 |
| R5 | user 图片 | `image_url` 支持 | 支持并按模型能力降级占位 | `input_image` 真传 `image_url` part；`input_file` / 仅 `file_id` 报错 | 已实现（完整对齐 opencode） |
| R6 | tool result 图片 | 图片收集后追加 user 消息 | 同上 | 文本留 `role=tool`，图片收集为后续独立 user `image_url` 消息 | 已实现（完整对齐 opencode） |
| R7 | 空 assistant 消息 | 发送 `content:null` | 无内容且无 tool_calls 时跳过 | 工具环/reasoning-only assistant wire `content:null`；纯空文本仍跳过 | 已实现 |
| R8 | tool_call_id 归一化 | 原样 | 归一化长度/非法字符 | 原样透传（移除出站归一化） | 已实现（对齐 opencode） |
| R9 | 有工具历史但无活动工具时 `tools` | 不发 | 发 `tools: []`（Anthropic 代理要求） | 仅工具历史场景发 `tools: []` | 已实现（对齐 pi） |
| R10 | Chat `custom` 工具 + grammar | 不支持 | `type:custom` + grammar | 统一 function 化 | 边界 |
| R11 | `reasoning_effort` 校验 | 枚举校验，拒绝 `max` / 非枚举（`openai-options.ts:5`） | 按模型能力 | 任意值原样透传，不拒绝 | 用户决策（更宽松，不替上游裁判） |

### 3.2 消息与工具环

| # | 差异点 | opencode | pi | 我们 | 状态 |
|---|---|---|---|---|---|
| M1 | 孤儿 tool call 补结果 | 无专门处理 | 自动插 `No result provided` | `ensureChatToolPaired` 补占位 + 重排 | 一致 |
| M2 | error/aborted assistant 跳过 | 无 | 整条跳过 | Responses 输入无 stopReason 概念，不适用 | 边界 |
| M3 | 时序 system 更新 | 包 `<system-update>` 折 user 并合并 | 不做 | `<system-update>\n...\n</system-update>`（XML 转义），折入前一条 user 或独立 user，按原时序 | 已实现（完整对齐 opencode） |
| M4 | tool 结果后接 user 时插 assistant | 无 | `requiresAssistantAfterToolResult` 兼容开关 | 无 | 边界 |
| M5 | 历史 reasoning 回灌 | assistant `reasoning_content` | 支持 signature / reasoning_details | assistant `reasoning_content`（无 signature/encrypted） | 一致 |
| M6 | compaction 历史 | 独立 user `<conversation-checkpoint>`（明文 summary/recent） | 无对应处理 | 独立 user `<compaction>` 密文标记文本 | 已实现（载体对齐 user 文本） |
| M7 | compaction_trigger / mcp_list_tools 历史 | 无此类型；AdditionalTools 不转文本 | 折 `<system-update>` 文本（旧实现） | trigger 丢弃（请求控制信号）；mcp_list_tools 不转文本（工具声明保留） | 已实现（对齐 Codex/opencode） |

### 3.3 流式回程

| # | 差异点 | opencode | pi | 我们 | 状态 |
|---|---|---|---|---|---|
| S1 | reasoning 字段别名 | 仅 `reasoning_content` | `reasoning_content` / `reasoning` / `reasoning_text` | 三别名齐备 | 已实现 |
| S2 | `choice.usage` 兜底 | 无 | 顶层无 usage 时读 choice.usage | 顶层优先，choice.usage 兜底 | 已实现 |
| S3 | tool delta 缺 id/name | 缺失即报错 | 先建空块后补 | id 合成 `call_{index}`，name 到齐再 open | 决策：保留宽容（有测试覆盖“先 id 后 name”分片，收紧会破坏真实上游兼容） |
| S4 | tool arguments 校验 | finish 时严格 parse，失败即 error | 流式 parseStreamingJson | 原样透传 + `SanitizeClientToolInput` 可控修复 | 一致（按历史约定） |
| S5 | finish_reason 映射 | `function_call` / `tool_calls` 都映射 tool-calls | 同 opencode 语义 | `function_call` 显式归 completed | 已实现 |
| S6 | usage 语义 | prompt 总量派 non-cached input | 同 | `prompt_tokens` 总量作为 `input_tokens`，缓存明细走 details | 已确认（对齐 opencode/Responses 官方） |
| S7 | 空流/终态锁定 | `onHalt` 保证工具事件先于终态 | 无 finish_reason 时报错 | locked + FeedDone 条件，避免空流误锁 | 一致 |
| S8 | logprobs | 不支持 | 不支持 | 支持 content logprobs | 我们更强 |
| S9 | `stream_options.include_obfuscation` | 不支持 | 不支持 | 支持透传 | 我们更强 |

### 3.4 配置与矩阵

| # | 差异点 | 说明 |
|---|---|---|
| D1 | `docs/protocol-coverage.md` Chat 请求矩阵 `top_k` 行 | 已改为 `none`：客户端 Responses 无来源，Chat 请求体亦无对应字段 |
| D2 | `developer` 折 `<system-update>` user | 与 opencode `lowerMessages` 一致：仅 `instructions` 是 `role=system` |
| D3 | 多模态 / custom grammar / deferred tools | 已登记为产品边界，本次不改 |

## 4. 差异点明细

### R2 top_k 文档错误

`docs/protocol-coverage.md:271` 写着 `top_k → top_k → supported`，但：

- `internal/chatconvert/request.go` 的 `ChatRequest` 没有 `TopK` 字段（`request.go:18`）；
- openai-go v3.42.0 `ResponseNewParams` 只有 `Temperature` / `TopP`，没有 `TopK`（`response.go:25728-25738`）；
- opencode 的 Chat body 也不发 `top_k`，只有 Anthropic/Bedrock 路径使用。

结论：客户端 Responses 请求不存在 `top_k` 来源，矩阵行应删除或改为 `none / 无客户端来源`。
已处理：`docs/protocol-coverage.md` 该行改为 `none` 并注明原因。

### R1 frequency/presence/seed/stop

opencode 在 `GenerationOptions` 声明了这四个字段（`options.ts:79-81`），`openai-chat.ts:364-367` 从 `generation` 透传；但正常 Chat 路径没有任何调用方设置它们，HTTP body overlay 也在 denylist（`http.ts:34-45`），实际请求不会携带。pi 的 `openai-completions.ts` 完全没有这四个字段（grep 无 `frequency_penalty` / `presence_penalty` / `seed` / `stop`）。因此两者都没有内部固定默认值，我们保持不发送；如需支持只能走源级默认参数。

### R3 工具 schema 投影

opencode `ToolSchemaProjection.openAI`（`tool-schema.ts:48-62`）会把：

- `anyOf` 中非 null 变体合并为 `properties`；
- 外层强制 `type: object` + `additionalProperties: false`；
- 递归移除 `anyOf` 中的 null 类型。

我们 `toolUnionToChat` 对 function / namespace 工具原样透传 `parameters`。严格上游可能对 null anyOf / 缺少 type 报 400。投影是 wire 归一，不是替上游判能力。
已处理：`normalizeToolSchema` 完整复刻 opencode `ToolSchemaProjection.openAI`：
- 顶层强制 `type: "object"`；
- 顶层 `anyOf` 的 record 变体展平进 `properties` 并强制 `additionalProperties: false`；
- 递归移除 `anyOf` / `type` 数组中的 null 变体，单个 anyOf 变体并入父级；
- function / namespace function / tool_search 均套用。

### R7 空 assistant 消息

- pi `convertMessages` 对无文本且无 tool_calls 的 assistant 直接跳过（`openai-completions.ts:1183-1198`）。
- 我们 `convertEasyMessage` 对空文本 assistant 仍返回消息（`request.go:626-635`），`convertOutputMessage` 也无条件返回（`request.go:697-708`）。

风险：严格 Chat 上游可能对空 assistant 400。候选修复：空文本且无 tool_calls 的 assistant 跳过，或统一 `content: null`。
已处理：`ChatMessage.MarshalJSON` 对 assistant 显式输出 `content:null`（工具环 / reasoning-only），
tool 空输出显式 `content:""`；纯空文本且无 tool_calls / reasoning 的 assistant 仍跳过，不产生空消息。

### R8 tool_call_id 归一化

pi `normalizeToolCallId`（`openai-completions.ts:1006-1028`）会把 Responses 生成的超长 / 含 `|` 等非法字符的 id 截断为 Chat 允许的 `[a-zA-Z0-9_-]`（<=40 字符），并保留 item 级唯一性。

我们 `appendToolCall` 原样透传历史 call id。风险：部分 Chat 上游（如 JD/DeepSeek）对 id 字符/长度敏感。
已处理（用户决策）：移除 `normalizeToolCallIDs`，assistant.tool_calls 与 role=tool 的 ToolCallID 一律原样透传（对齐 opencode）；归一化会改写客户端可见 call_id，不再做。

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

opencode 把 `prompt_tokens` 视为含 cache 的总量，`inputTokens` 直接透传总量并派生 non-cached；pi 的 `parseChunkUsage` 内部把 `input` 扣成 non-cached，但 `cacheRead`/`cacheWrite` 单独保留，`totalTokens = input + output + cacheRead + cacheWrite` 仍等于 `prompt + completion`。

Responses 官方 `input_tokens` 同样是含缓存的总量，`input_tokens_details.cached_tokens / cache_write_tokens` 表达明细。因此保持现状：`InputTokens = prompt_tokens`，缓存写入 `InputTokensDetails` + `CacheReadInputTokens` 兼容字段，不扣减；同时补 `total_tokens` 缺失兜底（`prompt + completion`，同 opencode/pi）。

### R9 工具历史与空 tools

pi 在无活动工具但消息含工具历史（assistant toolCall / toolResult）时发 `tools: []`，原因是 Anthropic 经 LiteLLM/proxy 的 Chat 上游要求该字段存在；opencode 直接不发。我们按 pi 落地：`ToChat` 检测最终 `Tools` 为空且消息含工具历史时置 `sendEmptyTools`，`MarshalJSON` 显式输出 `"tools":[]`；无工具历史时仍省略 `tools`（对齐 opencode）。

## 5. 建议批次

### P0 文档与低风险正确性（已完成）

- D1：修正 `top_k` 矩阵行。
- S1：`chatstreamconv` 增加 `reasoning_text` 别名。
- S2：`chatstreamconv` 增加 `choice.usage` 兜底。
- R7：空 assistant 消息跳过（需确认是否影响客户端回灌语义）。
- S5：`function_call` finish_reason 显式识别。

### P1 兼容性增强（已完成，第三轮按 opencode 完整对齐）

- R8：tool_call_id 原样透传（移除出站归一化，对齐 opencode）。
- R3：工具 schema 完整投影（anyOf 展平 + 强制 object/additionalProperties）。
- M3：developer/system 时序更新折 `<system-update>` user；compaction 折独立 user `<compaction>` 密文文本（opencode 的 compaction 是 user `<conversation-checkpoint>`，不是 system update）。
- 字段收口：`compaction_trigger` 丢弃（请求控制信号，Codex 明确不保留）；`mcp_list_tools` 不转模型文本（opencode 无此类型，Codex 不把 AdditionalTools 转消息，工具经 ToolSpec/请求 tools 声明）。
- R5/R6：`input_image` 真传 `image_url`；工具结果 / code_interpreter 图片聚合独立 user 消息。
- R4/R7：developer 折 user；assistant `content:null`、tool `content:""`；reasoning-only 也发 assistant。
- reasoning effort：任意值原样透传，不拒绝（`max` 也透传）。
- 字段裁剪：`prompt_cache_options`（mode/ttl）Chat 请求体无顶层槽位，删除透传、DEBUG 丢弃；
  `prompt_cache_key` / `max_tokens` + `max_completion_tokens` 双写保留（兼容端需要）。

### S6/R9（已完成）

- S6：确认 usage 语义保持 `input_tokens` 总量（opencode/Responses 官方），缓存明细走 details；补 `total_tokens` 缺失兜底。
- R9：无活动工具但有工具历史时发 `tools: []`（对齐 pi 的 Anthropic 代理兼容）；无工具历史仍省略。

### P2 产品边界外

- R1：frequency_penalty 等如需支持，只能走源配置默认值，不能从 Responses 客户端透传。
- R10：Chat `custom` + grammar。
- M4：`requiresAssistantAfterToolResult` 兼容开关。

## 6. 待确认问题

1. ~~R3 工具 schema 投影：是否接受 wire 归一，还是严格保持原样透传？~~ 已按 opencode 完整投影落地。
2. ~~R7 空 assistant 跳过：历史回灌中空 assistant 是否允许丢弃？~~ 已落地（空文本且无 tool_calls 才跳，不影响工具环）。
3. ~~R8 id 归一化：是否接受网关返回的 call_id 与原客户端 id 不一致（后续以网关返回为准）？~~ 已改：原样透传（对齐 opencode，不再归一化）。
4. ~~S6 usage：Responses `InputTokens` 是否需要从 `prompt_tokens` 扣掉 cache，还是保持总量？~~ 已确认：保持总量（同 opencode / Responses 官方 `input_tokens` 语义），不扣减；缓存明细走 `input_tokens_details` + `cache_read_input_tokens`。
5. ~~本次范围：只做 P0，还是 P0+P1 一起？~~ 已按 P0+P1 完成。
