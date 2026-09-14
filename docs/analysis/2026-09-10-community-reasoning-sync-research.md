# 开源社区对 Responses `reasoning` 明文 content 回灌问题的处理调研

> 日期：2026-09-10
> 范围：OpenAI Responses 协议里 `reasoning` 项明文 `content` 的输入校验、openai/codex 的恢复回放路径、主流网关/代理（LiteLLM、new-api、OpenRouter、Portkey 等）的跨厂商转换，以及「保留 / 折叠进 summary / 丢弃」的社区共识。
> 触发故障：codex-api-gateway 的会话同步器把第三方 provider（如 GLM）写入的明文 `reasoning.content` 原样回灌给官方 API，被上游 400 拒绝：
> `[ArrayParam] [input[N].content] [array_above_max_length] Expected an array with maximum length 0, but got an array with length 1`
>
> 来源与工具：GitHub REST API（`search/issues`、`search/code`、`git/trees`）、GitHub raw 文件（openai-openapi / openai-go / openai-codex / litellm / new-api / OpenRouterTeam/docs、Portkey）、curl 官方文档页；所有结论都落在第一步的 primary source 上。

## 1. OpenAI Responses API 对 input `reasoning` item 的约束

### 1.1 官方 schema：`ReasoningItem` 只有一个形态

官方 OpenAPI 仓库 `openai/openai-openapi`（[openapi.yaml#L57315 的 `ReasoningItem`](https://github.com/openai/openai-openapi/blob/main/openapi.yaml#L57315)）：

- `type`：固定 `"reasoning"`；
- `id`：必填，reasoning 内容的唯一标识；
- `summary`：必填，`SummaryTextContent` 数组（`{"type":"summary_text","text":...}`，定义于 [L69040](https://github.com/openai/openai-openapi/blob/main/openapi.yaml#L69040)）；
- `content`：可选，`ReasoningTextContent` 数组（`{"type":"reasoning_text","text":...}`，定义于 [L69058](https://github.com/openai/openai-openapi/blob/main/openapi.yaml#L69058)），描述仅 "Reasoning text content."；
- `encrypted_content`：可选字符串；
- `status`：`in_progress` / `completed` / `incomplete`。

**注意：公开的 openapi.yaml 里没有 `maxItems: 0` 这条约束**（对全部 `maxItems` 做了全文 grep，未出现 0）。也就是说“content 数组最大 0”是 OpenAI 服务端运行时校验器才落实的规则，只能从真实 400 观察到（见第 4 节），SDK 与 spec 本身不体现。

### 1.2 官方 Go SDK 的定义与“入参只能 id + summary”

openai-go（`responses/response.go`）：

- 输出/回灌模型 `ResponseReasoningItem` 的文档直接写：“Be sure to include these items in your `input` to the Responses API for subsequent turns of a conversation” （[L21645-21670](https://github.com/openai/openai-go/blob/main/responses/response.go#L21645)）；
- 入参类型 `ResponseReasoningItemParam` 虽保留 `Content` 字段（[L21756](https://github.com/openai/openai-go/blob/main/responses/response.go#L21756)），但官方提供的与约定一致的构造器 `ResponseInputItemParamOfReasoning(id, summary)` 只有两个参数（[L14762](https://github.com/openai/openai-go/blob/main/responses/response.go#L14762)），**不暴露 content 通道**。

结论：官方对“回灌”的定义就是 `{"type":"reasoning", "id":..., "summary":[summary_text...]}`，必要时叠加 `encrypted_content`；`content` 不参与回灌。

### 1.3 官方指南对 `summary` / `encrypted_content` 的定位

官方 [reasoning 指南](https://developers.openai.com/api/docs/guides/reasoning)原文：

> When you create a response in stateless mode, reasoning items in the response's `output` array include an `encrypted_content` property by default. Stateless mode applies when `store` is `false` or when your organization uses Zero Data Retention (ZDR) ... Reasoning items in the `output` array will include an `encrypted_content` property containing encrypted reasoning tokens that you can pass to future calls.

官方 [conversation-state 指南](https://developers.openai.com/api/docs/guides/conversation-state)：

> The Responses API returns encrypted reasoning items by default. Replaying the complete output keeps reasoning items and assistant `phase` values intact.

即：明文只能短暂存在于输出流；跨轮状态要么是短摘要（`summary`），要么是加密 token（`encrypted_content`），不让明文出去。

## 2. openai/codex（Rust）恢复会话时如何构造 input items

源码来自 `github.com/openai/codex` 主分支（2026-09-10 快照，depth 1）。

### 2.1 恢复 = 全量重建并整体回放

- 入口 `Session::reconstruct_history_from_rollout`（[rollout_reconstruction.rs#L134](https://github.com/openai/codex/blob/main/codex-rs/core/src/session/rollout_reconstruction.rs#L134)）：把 rollout JSONL 恢复为 history 后，后续每个 turn 都把这个 history 组合进 input items 无状态重发。
- 没有“只挑某些 item”或“只发增量”的过滤；`reasoning` 项会**原样以 item 形式回放**（`ResponseItem::Reasoning`）。

### 2.2 codex 自身对明文 `reasoning_text` 的序列化纪律

[models.rs#L1035](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/models.rs#L1035) 的 `ResponseItem::Reasoning`：

```rust
Reasoning {
    id: Option<ResponseItemId>,
    summary: Vec<ReasoningItemReasoningSummary>,
    #[serde(default, skip_serializing_if = "should_serialize_reasoning_content")]
    content: Option<Vec<ReasoningItemContent>>,
    encrypted_content: Option<String>,
    ...
}
```

配套开关 [should_serialize_reasoning_content（L1611）](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/models.rs#L1611) 的语义：

- 只有 content **不包含任何 `ReasoningTextContent` 变体**时才序列化；
- content 中只要有一个 `reasoning_text` 明文，整个 `content` 字段就会在 wire 上被省略。

这就是 openai/codex 对“回灌明文”问题的答案：**明文 reasoning_text 不让它出现在 requests 里**；只保留 `summary` 与 `encrypted_content`。这与我们 sessions.go 的实现方向一致。

### 2.3 “source item”的澄清

协议层 openapi.yaml 的 `ConversationItem` 判别表里没有 `source` 类型；codex 源码也没有生成 `type:"source"` item。它实际上的“来源”概念是：

- `THREAD_SOURCE_KEY = "thread_source"`：放在请求 metadata 里的线程来源标记（[responses_metadata.rs#L49](https://github.com/openai/codex/blob/main/codex-rs/core/src/responses_metadata.rs#L49)）；
- `RetainedInputSource`（`Local` / `Inherited`）：`codex_history` 的续接上下文来源，不是 wire item（[retained_context.rs#L47](https://github.com/openai/codex/blob/main/codex-rs/history/src/retained_context.rs#L47)）。

所以“如何派生 source item type”的咨询是：**不派生；codex 通过 rollout 全量重建历史回放**。真正的跨 provider 约束出在官方侧：`encrypted_content` 必须与当前服务端密钥匹配，第三方签发（GLM 等）的密文与官方 id 在切回 OpenAI 后都会成为新的 400 层（见 3.3、4.2）。

### 2.4 codex 自己的同款 400 记录

- openai/codex#36704（Stale encrypted compaction/reasoning ...）：恢复长线程后先 `invalid_encrypted_content`，清理后再爆 `array_above_max_length`（`input[5].content`）。
- openai/codex#23694 / #24002（0.132.0 回归）：`array_above_max_length` 出现在 resume / remote compaction 路径，用户只能 downgrade 到 0.131.0。
- CLIProxyAPI#5713 抓到同一错误走 `chatgpt.com/backend-api/codex/responses`，说明官网 Codex 入口也执行同样校验。

## 3. 社区网关 / 代理如何把各厂商 thinking 转成 Responses reasoning

### 3.1 LiteLLM（BerriAI/litellm）——最完整的公开对照实现

**Anthropic → Responses（messages 转发 `/responses`）**：

- `litellm/litellm_core_utils/prompt_templates/common_utils.py` 的 `responses_reasoning_items_from_thinking_blocks` 明确：脚本 thinking blocks 折叠成“single summary-only item”，只含 `summary`（`summary_text`）+ 可选 `encrypted_content`，**不带 `content`、不带 `id`**（[common_utils.py#L1945](https://github.com/BerriAI/litellm/blob/main/litellm/litellm_core_utils/prompt_templates/common_utils.py#L1945)）：

  > A run of plain thinking blocks collapses into one summary-only item. No item carries an `id`: the Responses API 404s on any id it did not mint itself and rejects an empty one, while an item without an id is always accepted.

- Responses→Anthropic 回放时也只读 `summary` + `encrypted_content`，把 reasoning item 转回 one Anthropic thinking block（[transformation.py#L170-L193](https://github.com/BerriAI/litellm/blob/main/litellm/llms/anthropic/experimental_pass_through/responses_adapters/transformation.py#L170)）。

**OpenAI Chat Completions → Responses（bridge）**：

- 流式转换把 `reasoning_content` 塞进 `{type:"reasoning", summary:[{type:"summary_text",text}]}` 的 reasoning output item（[streaming_iterator.py#L715-L758](https://github.com/BerriAI/litellm/blob/main/litellm/responses/litellm_completion_transformation/streaming_iterator.py#L715)），**不生成 `reasoning_text content`**。

结论：LiteLLM 是所有已知跨 protocol 转换（Anthropic thinking、Chat reasoning_content）落地为 Responses reasoning 的实现，其形态就是“折叠进 summary + content 不承载”；我没发现任何把明文塞进 `content` 回传的分支。

### 3.2 new-api（QuantumNous/new-api）与 one-api

- new-api 有完整 RelayConvert：`relaykit/relayconvert/reasoning/`（[目录](https://github.com/QuantumNous/new-api/blob/main/relaykit/relayconvert/reasoning/)），Responses→Chat 时先从 protocol 提取 `reasoningIntent` 再应用（[to_oai_chat_req.go#L89-L92](https://github.com/QuantumNous/new-api/blob/main/relaykit/relayconvert/internal/oai_responses/to_oai_chat_req.go#L89)）。
- Responses 输出的 reasoning item dto（`relaykit/dto/openai_response.go`）里只暴露 `Summary []ResponsesReasoningSummaryPart`（[L335](https://github.com/QuantumNous/new-api/blob/main/relaykit/dto/openai_response.go#L335) / [L488](https://github.com/QuantumNous/new-api/blob/main/relaykit/dto/openai_response.go#L488)）。
- 相关公开 PR / issue：
  - [#6392 PR](https://github.com/QuantumNous/new-api/pull/6392)：`recover from invalid reasoning signatures`——上游返回 `thinking_signature_invalid` 时，重试兜底删除 reasoning 的 `encrypted_content`；
  - [#6396](https://github.com/QuantumNous/new-api/issues/6396)：Responses→Chat 转换丢失工具历史的 `reasoning_content`；
  - [#6676](https://github.com/QuantumNous/new-api/pull/6676)：reject malformed reasoning item IDs（对接我们遇到的 400 顶层）。
- one-api（songquanpeng/one-api）无公开 `array_above_max_length` issue（搜索 0 条命中），reasoning 处理主要是 new-api 分支在演进；这里以 new-api 为准。

### 3.3 OpenRouter

- 官方文档仓库 OpenRouterTeam/docs 公开了 Responses reasoning 文档 [api_reference/responses/reasoning.mdx](https://github.com/OpenRouterTeam/docs/blob/main/api_reference/responses/reasoning.mdx)，但内容只讲 `reasoning.effort` / Responses 特性，**没有公布“第三方 thinking → responses reasons”后的明文回写策略**。
- 它是 OpenAI 形状的通过型服务，本身不折叠；我们无法验证其私有后端实现，如实标注局限性。

### 3.4 Portkey / Cloudflare AI Gateway

- Portkey responses 转发（open-ai-base provider）按官方模型把 reasoning item 直接产出 SSE 事件（[createModelResponse.ts#L225](https://github.com/Portkey-AI/gateway/blob/main/src/providers/open-ai-base/createModelResponse.ts#L225)），类型仍带 `content`（[modelResponses.ts#L1591](https://github.com/Portkey-AI/gateway/blob/main/src/types/modelResponses.ts#L1591)、[L1696](https://github.com/Portkey-AI/gateway/blob/main/src/types/modelResponses.ts#L1696)），不做清洗；即“直通”策略。
- Cloudflare AI Gateway 无公开源码、文档也没有 protocols 层面的 `reasoning` item 映射细节，本次无法用 primary source 验证，标注“未能验证”。

## 4. 真实 400 与社区处置（array_above_max_length 集群）

GitHub 搜索 `array_above_max_length`（57 条命中）中与本研究相关的证据：

| 项目 | 编号 | 现象 | 处置 |
|---|---|---|---|
| openai/codex | [#36704](https://github.com/openai/codex/issues/36704) | 长线程恢复：`invalid_encrypted_content` 消失后再 `array_above_max_length`（`input[n].content`） | 用户手工手术 rollout：删陈旧 compaction / 清 content |
| CLIProxyAPI | [#5713](https://github.com/router-for-me/CLIProxyAPI/issues/5713) | Codex Desktop 自动 compact 时历史上 reasoning `content` 非空 → 上游 400 | 等待官方修复 |
| sub2api | [#5491](https://github.com/Wei-Shaw/sub2api/pull/5491) | `array_above_max_length`（`input[N].content`）导致生产板块涌现 | 重试时剔除被上游拒绝的 reasoning content |
| aipmer/codex-switch | [#1](https://github.com/aipmer/codex-switch/issues/1) | 第三方会话切回 OpenAI 触发四层校验：`content` 长度 / `encrypted_content` 签名 / `tool_` 前缀 / 三方 id | **对 reasoning 切除 content、encrypted_content、id，只留 summary**；已实测 185 次 tools 会话恢复 |
| makecindy/cindy | [#2308](https://github.com/makecindy/cindy/issues/2308) | 会话内 GPT↔DeepSeek 切换后持续 400（`array_above_max_length`） | 无法稳定复现，用户上报 |
| oh-my-pi | [#10060](https://github.com/can1357/oh-my-pi/issues/10060) | 同错误 | 未决 |

共性结论：

1. **没有任何项目做到“明文 content 回传仍可用”**——服务端一律以 `array_above_max_length` 拒绝；
2. 社区处置分为两派：**剥除**（sub2api 清 content、codex-switch 连 encrypted_content/id 一起剥）与 **折叠**（LiteLLM 统一进 `summary`）；
3. 谁也绕不开的 constraint 是：**Responses 的 wire 形态里，回传 reasoning 只能是 summary / encrypted_content**。

## 5. 对我们现状修复的评估（sessions.go 方案 vs 主流）

我们方案（`internal/codexsessions/sessions.go`）：

- 明文 `content`（对象 part 或字符串简写）折入 `payload.summary`（`summary_text`），`content` 设 null；
- 已有 `encrypted_content` 的只清明文、密文保留；
- 幂等：summary 已存在同一 text 时不重复 append。

与社区主流对比：

| 维度 | 我们的方案 | 社区主流 | 评价 |
|---|---|---|---|
| content 非空 | 置 null | 全部清掉/序列化时省略（codex `should_serialize_reasoning_content`、sub2api、codex-switch） | 一致 ✔ |
| 明文进 summary | 完整正文折入 `summary_text` | LiteLLM 也是折入 `summary`（但长度受摘要生成策略控制） | 边界一致；我们是“高保真 > 摘要” |
| encrypted_content | 保留 | codex-switch 默认剥、new-api 在 `thinking_signature_invalid` 时剥 | **差异，需登记**：第三方签发的密文切回 OpenAI 后可能触发 `invalid_encrypted_content`（官方 400） |
| id | 不动 | codex-switch 一并剥（官方 `store=false` 只认自己发的 id） | **差异，需免疫**：切回 OpenAI 时第三方 `rs_...` id 也可能被拒 |
| 折叠语义 | summary_text 是模型可见明文 | 社区对 provider 迁移同样用 summary_text | 一致 ✔ |

结论：**主体（折叠进 summary + content=null）与主流 wire 形态一致，是正确方向的修复**；差异集中在两个边界：`encrypted_content` 保留（可能被上游判无效）与 `id` 来源（官方 store=false 只认自己分发）。建议：默认对第三方 reasoning 也做“折 summary + 若 `encrypted_content` 非官方签名则清密文”的配置化处理，并把 id 校验纳入后续会话清洗层；这两个点到时会直接 400，与本次 content 问题同源（见 aipmer/codex-switch#1 的四层矩阵）。

## 6. 调研工具与局限性

- 使用：`curl`（官方文档 / raw.githubusercontent）、`gh api search/issues`、`gh api search/code`、`gh api git/trees`、shallow `git clone`（openai-openapi、openai-go、openai/codex、litellm、Portkey-AI/gateway），以及 OpenRouter 官方 docs 仓库的 mdx 文件。
- 成功率：全部 URL 返回 200；GitHub API 无失败；官方文档页两页均 200。
- 局限：OpenRouter 私有后端、Cloudflare AI Gateway 未公开协议细节，无法用源码验证；`maxItems:0` 不出现在公开 OpenAPI 中，认定依据为 OpenAI 官方 400 文本 + 行业内多次真实复现（均有链接）。
