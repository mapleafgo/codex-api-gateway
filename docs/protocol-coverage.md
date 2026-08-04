# Protocol Coverage Matrix

日期: 2026-08-04（a Anthropic + c Chat + r Responses 透传；usage details 已支持，structured output 已移除）

本文是协议覆盖的**唯一真相源（SoT）**。默认表格描述 **Responses → Anthropic Messages（`backend_type: a`）**；`c` / `r` 见专节，**三路径不共享**字段状态行。后续任何协议补齐都必须同步更新本文。

## 状态定义

| 状态 | 含义 | 实现要求 |
|---|---|---|
| `supported` | 有明确 Anthropic 等价能力，且当前网关做语义级转换 | 需要单元测试或集成测试覆盖 |
| `lossy_supported` | 可转换但存在字段或行为损失 | 必须说明损失内容 |
| `raw_preserved` | 暂无语义转换，但原始 JSON 被保存或注入上下文，避免静默丢弃 | 不得对客户端宣称语义支持 |
| `unsupported_by_backend` | Anthropic Messages 无等价能力，不能安全模拟 | 应返回明确错误或在文档中登记不支持 |
| `deferred` | 需要专项设计才能决定语义 | 必须说明后续分析点 |
| `dropped` | 在请求侧无 Anthropic 等价能力，回灌时静默丢弃 + WARN | 必须记录被丢弃内容的类型/标识/影响 |

## 关注面与产品边界

本网关面向 **Codex CLI → 多上游**（`a` / `c` / `r`），不做 OpenAI 全量 Responses 平台，无 session 运行时：

- 客户端**自带完整 `input`** 回灌；网关不做 session store。
- **a**：`previous_response_id` 非空时 WARN + 忽略；Responses ↔ Anthropic **直转**。
- **c / r**：见下列专节；r 可透传 `previous_response_id`，网关仍不代补历史。

### OpenAI Chat Completions 上游（`backend_type: c`）

客户端仍只走 `/v1/responses`。当 source 配置 `backend_type: c` 时，网关经 `chatconvert` → Chat Completions 流式上游 → `chatstreamconv` 回写 Responses SSE。

详细字段级状态见本文专节 **「Chat 后端覆盖矩阵（backend_type: c）」**。摘要：

- **已支持（A+B+透传收口）**：文本多轮、工具环、采样、`function.strict`/`service_tier`/`metadata`/`store`/`moderation`/`reasoning.effort`→`reasoning_effort`、**`reasoning_content` 出站+入站回灌（有损）**、`top_logprobs`（含出站 logprobs）、`stream_options.include_obfuscation`、usage（含 details）、`finish_reason` 终态。
- **明确降级**：reasoning **无 encrypted/signature**（明文 `reasoning_content` 有损映射）；`text.verbosity`/`safety_identifier`/`prompt_cache_key` 为 Responses 专有字段，Chat 无顶层等价，**DEBUG + 丢弃**（2026-08-01）；hosted 为 **function 化有损**；file_search/computer/image_generation 历史 **WARN 跳过**；`compaction` item 密文不可解读，**WARN 丢弃**（Codex 非 OpenAI provider 下走 local 压缩，摘要以明文 user 消息回灌，不走 compaction item）；compaction_trigger / mcp_list_tools 历史 **丢弃**。`input_image` 图片按 opencode 形状透传为 `image_url`；`input_file` / 仅 `file_id` 的图片属协议不可映射，转换报错。
- **与 a 路径关系**：Anthropic 源仍是 Responses↔Messages **直转**；Chat 是并行 Backend，不经 Chat 中枢转 Anthropic。

- Anthropic 无等价能力的字段：明确错误 / WARN + 丢弃 / echo-only，禁止把整段 JSON 灌进 system。


### OpenAI Responses 透传上游（`backend_type: r`）

客户端仍只走 `/v1/responses`。当 source 配置 `backend_type: r` 时，网关对 OpenAI Responses 上游做**最小改写透传**（实现：`backend.ResponsesBackend` + `responsesclient`）：

- **入站**：`map` 语义透传；`model` 经 `model_map`/`default_model` 解析；强制 `stream: true`；`reasoning` item 仅含 `summary` 明文时折算 `content`（`reasoning_text` part）——DeepSeek `/responses` 只支持 plain-text `content` 合并进相邻 assistant，忽略 `summary`，不折算会触发 `reasoning_text must be passed back` 400；其余键原样保留（含 `previous_response_id`、tools、include 等）
- **出站 SSE**：上游 `event` + `data` 转发（无 `event` 时从 JSON `type` 回填；仍空则跳过帧）；**T2** 仅回写顶层/`response.model` 为客户端请求 model；空流不合成终态；中途失败不强制补 `response.failed`
- **取消语义**：已收到 `response.completed` / `response.incomplete` 后客户端取消 → 观测记 `completed`（对齐 Chat 终态后读尾）
- **观测**：尽力解析终态事件 `usage`（`input_tokens` / `output_tokens` / cache 字段若有）；`backend_type` 恒为 `r`（metrics 禁止空串）
- **WARN 收口**：配置含**启用中** r 源时，`warnDroppedOrIgnoredParams` 不对 r 可透传字段误报「数据被丢弃」；`previous_response_id` 打 INFO「透传上游，网关不代补会话」
- **与 a/c 关系**：并行 Backend，**不共享** a/c 字段矩阵；不经 Chat/Anthropic 转换
- **DeepSeek 适配**：DeepSeek 的 Anthropic 兼容端点（`/anthropic/v1/messages`）对显式 `type:"custom"` 工具返回 400（`unknown variant 'custom'`）；a 路径 client tool 已统一省略 type（官方缺省 `name + description + input_schema` 形态），天然兼容，不再需要 `tools_type` 配置

字段级状态不复用 a/c 表：r 路径以「形状透传、结果归上游」为准；几乎全部请求/事件字段对网关为 passthrough，语义由上游决定。

## 架构基础（与 AGENTS.md 对齐）

**形状透传，结果归上游。** 网关是协议转换层，不代上游拒绝能力、不编造 failed 终态。仅「协议不可映射」才允许转换错误或矩阵登记的丢弃。详见仓库根目录 `AGENTS.md`「协议转换职责边界」。

## 收口策略（产品 + 技术）

本节是协议映射的**硬边界**。后续 PR 若扩大边界，必须先改本节再改代码。

### 1. 产品范围内（做）

- Codex CLI → Anthropic 兼容后端的 **Responses ↔ Messages 直转**。
- 客户端自带完整 `input` 回灌；网关无 session store。
- 可语义映射的 tool / content / SSE 生命周期；有损处登记 `lossy_supported` 并说明损失。
- 网关按 `anthropic.cache_enabled` 自主控制 Anthropic `cache_control`，不依赖 OpenAI prompt cache key。MCP 由 Codex 客户端本地执行：网关只声明扁平 `mcp__<server>__<tool>` function，不再注入 beta MCP 配置；`anthropic.cache_ttl=1h` 时带 `extended-cache-ttl-2025-04-11` beta。

### 2. 产品范围外（声明不做）

| 能力 | 处理 |
|---|---|
| `previous_response_id` / Conversation / 本地 `store` 会话回填 | `unsupported_by_backend` 或 echo-only；非空 WARN |
| `background` / `queued` / 非 SSE 同步 JSON 响应体 | 不做 |
| `file_search` / `computer*` / `image_generation` / `programmatic_tool_calling` | 工具声明 fail-fast；历史 item `dropped` |
| `audio*` SSE | 不做 |
| MCP `require_approval` 审批协议 | 降级 never + WARN；审批历史 item `dropped` |
| OpenAI Files 凭据拉取（`file_id`） | WARN + 丢弃 |
| OpenAI moderation / safety_identifier 透传 | WARN + 忽略 |

### 3. 后端/协议限制（无法等价实现）

| 限制 | 处理 |
|---|---|
| Anthropic 无 OpenAI `search_context_size` 字段 | **不得假映射**；请求带该字段时 WARN + 忽略 |
| Anthropic 无 output logprobs / stream obfuscation | WARN + 忽略 |
| Anthropic MCP 仅 `authorization_token` | 非 Bearer 的自定义 headers WARN + 丢弃 |
| code_interpreter 整体（含 container / 生成文件 URL / image） | 网关不支持；a/c 声明 fail-fast，历史/回程静默忽略或 skip |
| 未知 Anthropic server tool（web_fetch 等） | 流式 **WARN + skip**，不 `response.failed` |

### 4. Deprecated 字段（一律丢弃）

下列字段 **禁止** 做兼容映射或注入 system 模拟：

| 字段 | 行为 |
|---|---|
| `reasoning.generate_summary` | WARN + 忽略（只用 `reasoning.summary`） |
| `prompt_cache_retention` | DEBUG + 忽略（不用其推导 TTL） |
| `user`（OpenAI 已废弃） | WARN + 忽略（可用 `metadata.user_id`） |

### 5. lossy 打磨原则

- **优先透传 SDK 两侧均存在的字段**（如 web_search `user_location`）。
- **历史回灌**可把客户端执行元数据折进 `tool_use.input` JSON（lossy 保留线索），不得改成 Anthropic 不认识的顶层字段。
- **不扩大 fail-fast 范围**去「假装严格」；已知无等价继续 WARN/drop 或 fail-fast 与矩阵一致。
- 每改一行矩阵状态，同步本文件变更记录日期子弹。

### 6. 收口内可打磨项（已做尽）

下列在 **不扩产品边界、不假映射** 前提下已做到可折入的最大集：

1. web_search：`user_location` 映射；`search_context_size` WARN + 忽略（无 Anthropic 字段）。
2. shell / local_shell / apply_patch **历史**：env、cwd、timeout、limits、status、caller、exit/timeout outcome 折入 `tool_use.input` 或 tool_result 文本。
3. code_interpreter：**网关不再支持**（声明 fail-fast；历史/回程静默忽略；DeepSeek 等兼容端点不受 code_execution 变体影响）。
4. MCP：string allowlist 已支持；filter / 审批 / 非 Bearer headers 保持降级（硬限制）。

### 7. 收口内不再打磨（硬限制 / 协议天花板）

| 项 | 原因 |
|---|---|
| `search_context_size` 真映射 | Anthropic web_search 无字段 |
| code_interpreter 全功能（container / 生成文件 URL / image 真映射） | 网关整体不支持 code_interpreter |
| MCP 审批协议 / filter AST / 任意 headers | 后端无能力或成本/边界外 |
| custom `format` grammar 完整保留 | Anthropic custom tool 无 OpenAI grammar 等价 |
| structured output 非 tool 模拟 | 无原生 json_schema 强制 |
| reasoning.effort 精确 token | OpenAI effort 非 budget；已改用 `output_config.effort` 语义映射，模型自行决定深度 |
| system/developer 中的 image | Anthropic system 仅文本 |
| 出站专用 `shell_call`/`apply_patch_call` item type | Codex 消费 `custom_tool_call` 已验证 |
| SSE citation 非 web 类 → file_citation | OpenAI 无更细等价 |


### 8. 为何网关不支持 `code_interpreter`

网关对 `code_interpreter` 整体 fail-fast（a/c 声明报错；历史 item 与上游回程静默忽略），
不再做 Anthropic code_execution 映射。原因：

- DeepSeek 等 Anthropic 兼容端点的工具表只接受普通 `name + input_schema` 与
  `web_search` server tool，`code_execution_20250522` 变体不在支持表内（消息侧
  `code_execution_tool_result` 明确 Not Supported），声明即可能 400。
- OpenAI `code_interpreter` 输出的 `image`+`url` 与 Anthropic code_execution 的
  `stdout`/`file_id` 协议不对齐（此前 §8 已登记为硬限制）。
- 移除后请求侧不再产生 code_execution 变体，官方/兼容端点均不会主动调用未声明工具，
  回程（server_tool_use(code_execution) / code_execution_tool_result）按 skip 处理，不中断流。

## 变更记录

### 2026-08-04

- **DeepSeek thinking-mode reasoning 回传修复**：c 路径 `chatconvert` 在连续多条 assistant 文本后接 `function_call` 时，把整个连续 assistant 段合并成一条消息，`content` / `reasoning_content` / `tool_calls` 同框且保持原始顺序，遵守 Z.AI/Console 与 DeepSeek “完整、不重排回传 `reasoning_content`”契约，避免 `reasoning_content ... must be passed back` 400；r 路径 `PrepareUpstreamBody` 把 OpenAI 标准 `summary` 明文折算进 `reasoning_text` content，满足 DeepSeek `/responses` 只认 plain-text `content` 的合并契约。

### 2026-08-01

- **文档对齐与运行修复**：`/v1/models` Priority 改为按配置顺序升序 `1..N`（Codex 按 priority 升序排序，越小越优先）；r 透传 `PrepareUpstreamBody` / `rewriteClientModel` 改用 `json.Number` 保留大整数；`web_search` / `web_search_preview` 矩阵版本行订正为 `20260209`（实际 wire）；README 断路器键名 `cooldown` 订正为 `circuit_interval`。
- **DeepSeek 兼容收口（用户决策）**：a/c 路径不再转换 `text.format`（`json_schema` / `json_object` 统一忽略，`output_config` 仅保留 `effort`）；`code_interpreter` 整体移除（a/c 声明 fail-fast，历史 item 与上游 code_execution 回程静默忽略/skip，不再映射 `code_execution_20250522`）；**网关只接受明文 thinking**：出站 `redacted_thinking` 块跳过（不生成 reasoning item），入站无 summary 文本的 `encrypted_content`（redacted 密文）静默忽略，明文 thinking 的 signature 回灌保留。
- **compaction 行为修正（Codex local 压缩确认）**：`supports_remote_compaction()` 仅对 provider 名为 OpenAI/Azure 成立；本网关面向第三方 provider（如 deepseek）时 Codex 走 **local 压缩**，摘要生成请求与摘要回灌均为普通明文消息，网关按常规转换透传。真实 `compaction` item 的 `encrypted_content` 只对生成它的服务端可解，a/c 路径无法解读，改为 **WARN + 丢弃**（移除原先折独立 user `<compaction>` 密文文本的降级）；`compaction_trigger` 保持丢弃（请求控制信号）。
- **DeepSeek a 端点工具限制**：DeepSeek Anthropic 兼容端点只接受 `web_search_20250305` / `web_search_20260209` server tool，`custom` 工具返回 400；带 21+ 工具的 Codex 请求实测 400 后 failover 到 Chat 源。DeepSeek 改为 `backend_type: r` 透传后全部 200，登记到 r 专节。
- **freeform 单键任意键名解包**：opencode 等 Chat 上游把工具文本包在 `input`/`patch`/`cmd` 等不同键下，Codex 会把整段 JSON 当入参导致校验失败；`SanitizeClientToolInput` 对 freeform 工具改为单键 JSON 对象直接取唯一字符串值，键名不敏感；多键 structured 形态仍走 apply_patch V4A 兜底，r 透传实测返回裸文本不受影响。
- **opencode c 400（Console upstream failed）观测**：2026-08-01 11:25-11:28 opencode2api 上游（Console provider）连续返回 400 `Error from provider (Console): Upstream request failed`，网关 failover 到 grok a 成功（当时策略为整轮不重试不降级；2026-08-04 起 4xx 计入降级/机会失败，整轮仍不重试）；属上游暂时故障，网关侧无转换改动。
- **a 路径 client tool 统一省略 type（用户决策）**：Anthropic client tool 没有独立 custom 类型，`type:"custom"` 是默认值且部分兼容端点（DeepSeek）拒绝显式写；`custom`/`shell`/`apply_patch` 一律转官方缺省形态 `name + description + input_schema`，移除 `tools_type: custom|omit` 配置与 `StripToolType`，server tool 仍带自身变体 type。

### 2026-07-31

- **Chat 字段裁剪（对齐 opencode）**：`prompt_cache_options` 无 Chat 顶层槽位，删除透传、DEBUG 丢弃；`max_output_tokens` 出站保留 `max_tokens` + `max_completion_tokens` 双写（兼容旧上游与新模型）。
- **compaction 类历史收口（Codex/opencode 对照）**：`compaction` 折独立 user `<compaction>` 密文文本（opencode 对应物是 user `<conversation-checkpoint>`，不再走 `<system-update>`；a 路径在 Anthropic 交替约束下并入相邻 user）；`compaction_trigger` 是请求控制信号，Codex 明确丢弃；`mcp_list_tools` 不转模型文本（opencode 无此类型，Codex 不把 AdditionalTools 转消息，工具经 ToolSpec/请求 tools 声明）。
- **透传收口（用户决策）**：`reasoning_effort` 任意值原样透传，不再拒绝 `max`；`tool_call_id` 原样透传，移除出站归一化（对齐 opencode，不改写客户端可见 call_id）。

### 2026-08-01

- **Chat 上游非标准字段丢弃**：`prompt_cache_key` / `text.verbosity` / `safety_identifier` 均为 Responses/Codex 专有、Chat Completions 无顶层等价字段。此前透传会导致严格上游 400（如 `0v0.info` 报「未知请求字段：prompt_cache_key」），故改为 **DEBUG + 丢弃**（同 `prompt_cache_options` 处理）。相关对话式上游若确需这些扩展，由上游自行声明支持，网关不再代发。

### 2026-07-22

- **Responses 透传（`backend_type: r`）**：最小改写透传 + T2 model 回写 + warn 收口。
- **Chat 后端（`backend_type: c`）专节**：补全 Request / Input Item / Tool / 出站 SSE 矩阵；批次 A+B（合并 tool_calls、Codex freeform 工具环、output_text 回灌、`parallel_tool_calls`、`finish_reason` 终态）。
  - **usage 末包时序**：finish_reason 后仍接收空 choices usage；终态延后到 FeedDone，避免 `include_usage` 下 terminal usage 恒 0。
  - **DRY**：`toolcatalog.FreeformInputSchema` / `SplitToolName` 供 a/c 共用。
  - **P2**：content_filter refusal 事件链；orphan tool 占位；`max_tokens`+`max_completion_tokens` 双写。

### 2026-07-20

- **code_interpreter image 硬限制说明**：§8 写明 OpenAI `image`+url 与 Anthropic code_execution（stdout/`file_id`）不对齐，且网关无 Files 托管，入站/出站均无法真映射。

- **收口策略专节**：产品范围 / 范围外 / 后端限制 / deprecated 一律丢弃 / lossy 打磨原则写入 `docs/protocol-coverage.md`。

- **文档对齐实现**
  - Anthropic `stream server_tool_use`：未 catalog 的 server tool（web_fetch 等）与对应 result 为 **WARN + skip**，不是 `response.failed`（修正「仍显式失败」过时表述）。
  - 补充「未知 Input Item 兜底」：`unknownInputItemPart` 仅对 SDK 未登记类型 raw_preserved；已知无等价类型保持 dropped。
- **deferred 全 A 收口**
  - `reasoning.generate_summary` / `text.verbosity` / `context_management` / `max_tool_calls` → `unsupported_by_backend`（WARN + 忽略；不实现语义模拟）。`prompt_cache_retention` 同其它 prompt_cache_* 为 DEBUG + 忽略。
  - `prompt_cache_key` / `prompt_cache_options` / `prompt_cache_retention` 均为 DEBUG + 忽略（网关已自主 cache_control，可控协议差异）。
  - response status `cancelled` → `supported`（metrics `canceled`/499 only；不写 SSE 终态，因对端通常已断）。

- **Codex 主路径 wire 修复**
  - `input` 历史 assistant `content[].type=output_text`/`refusal` 从 raw JSON 归一为 `input_text` 再走 `appendMessage`（原路径被 SDK EasyInputMessage 静默清空）。
  - `function_call_output` / `custom_tool_call_output` 的 `output` 支持 content 数组（`input_text` / `input_image` / `input_file`）→ `tool_result` 多 part；仅 `file_id` 无法拉取时 WARN。
  - `reasoning`：`summary` 为空时回退 `content[].reasoning_text`，避免误判 redacted。
- **hosted server tool 历史回灌**
  - `web_search_call` 历史：`server_tool_use(web_search)` + 空 `web_search_tool_result` + sources URL 折可见文本（Anthropic required `encrypted_content` 无 OpenAI 来源，填空会 400）。
  - `mcp_call` 历史：按扁平名 `mcp__<server>__<tool>` 直接回填标准 `tool_use` + `tool_result`，不重建 beta MCP 块。
  - `mcp_list_tools` 历史：折 developer marker（server + 工具名 + error），lossy 保留可用工具线索。
  - `mcp_approval_request` / `response`：Anthropic 无审批协议，**不实现**，WARN + 丢弃。
  - `code_interpreter_call` 的 image 输出：丢弃 + WARN；logs 保留，并写入可读占位（`image output omitted`）。
- **出站流式**
  - `citations_delta` 除流式 `annotation.added` 外，写入终态 `output_text.annotations`，避免 Codex 只看 final item 时 citation 丢失。
- **无等价历史 item 一律 dropped**
  - `file_search_call` / `computer_call` / `computer_call_output` / `image_generation_call` / `program` / `program_output` / `item_reference` / `additional_tools`：WARN + 丢弃，不再 raw dump 进 system context。
- **请求参数状态订正**
  - `previous_response_id` → `unsupported_by_backend`（非空时 WARN）。
  - `store` → `raw_preserved`（响应对象 echo，无本地存储）。
  - `service_tier` → 非空时 WARN，仍不透传。
  - `include` 分档：已满足项静默；`message.output_text.logprobs` 在配置含 Chat 源时 satisfied（出站由 chatstreamconv 映射）；纯 a 源或 file_search/computer 等仍 WARN + 忽略。
- **表述订正**
  - `output_message` / `input_message`：SDK 三个 `message` discriminator 实测几乎总落到 `OfMessage`；不再宣称「未做分支」的 raw_preserved。
  - `tool_search_output`：入站回灌 supported，出站不生成（搜索由 Codex 本地执行）。
  - 去掉 `response.output_text.annotation.added` 与 `error` 的重复旧行。

### 2026-07-18

- 为所有「静默忽略」的请求参数（deprecated / 无等价能力）补 WARN 结构化日志，见 AGENTS.md「静默跳过与降级处理约定」。
- `metadata.user_id` 透传到 Anthropic `metadata.user_id`；其余键值对仅 echo。
- 流式 `citations_delta` 映射为 `response.output_text.annotation.added`（`web_search_result_location` → `url_citation`，其余 → `file_citation`）。
- 流式上游 `error` 事件同时发出 OpenAI `error` 事件与 `response.failed` 终态。
- 流式 `mid_conversation_system` 块 WARN + 跳过，不中断流。
- 移除 `service_tier_passthrough` 配置与 `applyServiceTier` 逻辑（`service_tier` 不再透传）。
- 移除 `additional_tools` input item 转换分支（网关统一 `use_responses_lite=false`）。
- 网关级指令注入从 `system_suffix` 改为 config 同级 `base_instructions.md`（经 `/v1/models` 由 Codex 客户端注入，prompt cache 更友好）。

## Chat 后端覆盖矩阵（backend_type: c）

日期: 2026-08-01

本节只描述 **Responses → Chat Completions → Responses SSE** 路径（`backend_type: c`）。Anthropic 直转见上文各表；两路径**不共享**字段状态。

### 状态约定

沿用全局状态定义。Chat 路径额外：

- 内建 freeform 工具（`shell` / `local_shell` / `apply_patch`）在 Chat 侧声明为 `type=function` + `parameters={s:string}`；出站回程统一折为 `custom_tool_call`（name=`shell` / `local_shell` / `apply_patch`），input 原样透传，不解析命令/补丁文本。freeform `custom` 工具同样按 `custom_tool_call` 回程（`tool_search` 为 `tool_search_call`）。
- 连续多条 Responses `function_call` / freeform call **必须**合并为一条 Chat `assistant` 消息的 `tool_calls[]`，否则多数 Chat 兼容上游 400。

### 产品边界（c）

| 做 | 不做 |
|---|---|
| Codex 文本 + 客户端 function / freeform agent 工具环 | 对外 `/v1/chat/completions` |
| 流式 SSE only（固定 `stream:true` + `include_usage`） | 非流式 Chat 完成体 |
| 与 a 源混排 failover / 熔断 | hosted **真实** server 执行（Chat 仅 shape） |
| `finish_reason` 终态对齐（stop/tool_calls/length/content_filter） | Chat `reasoning_content` 与 a 路径 thinking **完整等价**（无 encrypted/signature） |
| 出站 `reasoning_content` → Responses reasoning（有损） + 入站回灌 `reasoning_content` | Anthropic 式 `citations_delta` 完整等价（五家 Chat 无官方字段） |
| 文本工具环 | 多模态 image / OpenAI Files |

### Request 参数（c）

| Responses 参数 | Chat 映射 | 状态 | 说明 |
|---|---|---|---|
| `model` | `model` | `supported` | 经 source ModelMap / DefaultModel |
| `input` string | user message | `supported` | |
| `input` item list | `messages` | `lossy_supported` | 见下表；无 Chat 等价 item DEBUG/WARN 跳过 |
| `instructions` | system message（首条） | `supported` | 对齐 opencode：`instructions` 是唯一 `role=system`；会话中的 developer/system item 折 `<system-update>` user（XML 转义，按原时序折入前一条 user 或独立 user 消息） |
| `max_output_tokens` | `max_tokens` + `max_completion_tokens` | `supported` | 双写兼容旧上游与新模型 |
| `temperature` / `top_p` | 同名 | `supported` | |
| `top_k` | `top_k` | `none` | 客户端 Responses 请求无此来源；Chat 请求体亦无对应字段；矩阵原列为 supported 是错误的 |
| `parallel_tool_calls` | `parallel_tool_calls` | `supported` | 直接透传 |
| `tools` | `tools` | `lossy_supported` | function + freeform + hosted function 化（web_search/mcp__*）；file_search/computer/image_generation/code_interpreter 跳过或 fail-fast；**无活动工具但消息含工具历史时显式发 `tools: []`**（对齐 pi 的 Anthropic 代理兼容） |
| `tool_choice` | `tool_choice` | `lossy_supported` | mode + function/custom/shell/apply_patch 名；**allowed_tools 精确过滤** tools 列表 + mode；hosted choice：web_search 映射为同名 function 强制选择，其余（含 code_interpreter）与 mcp/programmatic DEBUG 后忽略 |
| `stream` | 固定 `true` | `supported` | 客户端 stream 与否不影响上游 |
| `stream_options` | `include_usage: true` | `supported` | 网关强制打开 usage 末包 |
| `reasoning.*` | partial | `lossy_supported` | `effort`→`reasoning_effort`（任意值透传，不拒绝）；历史 reasoning 回灌 `message.reasoning_content`（无 encrypted）；不 hardcode 厂商 thinking 开关 |
| `text.format` structured | none | `dropped` | 网关不支持 structured output；`json_schema`/`json_object` 忽略，不写 `response_format` |
| `text.verbosity` | none | `dropped` | Responses 专有字段，Chat 无顶层等价；非空时 **DEBUG + 丢弃**（2026-08-01） |
| `service_tier` | `service_tier` | `supported` | Chat 官方字段透传；a 路径仍忽略 |
| `safety_identifier` | none | `dropped` | Responses 专有字段，Chat 无顶层等价；非空时 **DEBUG + 丢弃**（2026-08-01） |
| `metadata` | `metadata` | `supported` | Chat 整表透传；a 路径仅 user_id + echo |
| `store` | `store` | `supported` | Chat 透传；响应 echo 仍保留 |
| `moderation` | `moderation` | `supported` | model + policy modes 同形透传；a 路径忽略 |
| `reasoning.effort` | `reasoning_effort` | `lossy_supported` | 任意值原样透传（`max` 也透传，不替上游拒绝）；summary/context 无 Chat 顶层等价 |
| `top_logprobs` | `logprobs=true` + `top_logprobs` | `supported` | Chat 透传；a 路径忽略；出站见 SSE 表 |
| `stream_options.include_obfuscation` | 同名 | `supported` | 强制 `include_usage=true` 并透传 obfuscation；a 路径忽略 |
| `previous_response_id` 等平台字段 | none | `unsupported_by_backend` | 与 a 路径同产品边界 |
| `prompt_cache_key` | none | `dropped` | Responses/Codex 专有字段，Chat 无顶层等价；非空时 **DEBUG + 丢弃**（2026-08-01） |
| `prompt_cache_options` | none | `dropped` | Chat 请求体无顶层等价（仅 content part `prompt_cache_breakpoint`）；mode/ttl 非空时 **DEBUG + 丢弃** |
| `prompt_cache_retention` | none | `unsupported_by_backend` | deprecated，不映射 |
| `user`（deprecated） | none | `dropped` | 与 a 一致不映射；请用 safety_identifier / metadata.user_id |

### Input Item（c）

| Responses item | Chat 映射 | 状态 | 说明 |
|---|---|---|---|
| `message` / EasyInputMessage | role + content | `lossy_supported` | `user` 纯文本为 string、含图片为 `[text,image_url]` parts；`developer`/`system` 折 `<system-update>` user；assistant 无文本跳过 |
| `input_message` / `output_message` | 同 message 文本 | `supported` | 防御分支；SDK 实测几乎总落到 EasyInputMessage |
| `function_call` | assistant `tool_calls[]` | `supported` | **相邻 call 合并**到同一 assistant；`id` / `tool_call_id` 原样透传（不归一化） |
| `function_call_output` | role=tool + user image | `supported` | 文本留在 `role=tool`（多段 `\n` 拼接）；图片收集为后续独立 user `image_url` 消息（同 opencode）；含 `input_file` 报错 |
| `custom_tool_call` | assistant tool_calls name 原样 | `supported` | arguments=`{"s":...}` freeform；相邻合并 |
| `custom_tool_call_output` | role=tool | `supported` | |
| `shell_call` / `local_shell_call` | tool_calls name=`shell` / `local_shell` | `lossy_supported` | 命令折 `input`；env/limits 不进 Chat schema；`local_shell` 用独立函数名区分回程 |
| `shell_call_output` / `local_shell_call_output` | role=tool | `lossy_supported` | status/stdout 折文本 |
| `apply_patch_call` | tool_calls name=`apply_patch` | `supported` | operation/path/diff 结构化 JSON 进 function arguments，含 status/caller |
| `apply_patch_call_output` | role=tool | `lossy_supported` | |
| `tool_search_call` | tool_calls name=`tool_search` | `supported` | arguments 原样/对象序列化 |
| `tool_search_output` | 动态 tools + tool 消息 | `lossy_supported` | 注入 function 声明；result 文本为工具名列表 |
| `reasoning` | assistant `reasoning_content` | `lossy_supported` | 明文 summary/content 折入同轮/下一条 assistant；连续 assistant 段合并为一条，`content` / `reasoning_content` / `tool_calls` 同框且不重排；`encrypted_content` 丢弃（DEBUG）；孤立 reasoning 也发 assistant（`content:null` + `reasoning_content`，同 opencode） |
| `web_search_call` 历史 | assistant tool_calls + tool 文本 | `lossy_supported` | query/sources 折文本 |
| `code_interpreter_call` 历史 | none | `dropped` | 网关不支持 code_interpreter；静默忽略 |
| `mcp_call` 历史 | `mcp__server__tool` + tool result | `lossy_supported` | 无审批 |
| computer / file_search / image_generation / program / program_output / item_reference 历史 | none | `dropped` | **WARN** 跳过（`itemType` 显式识别，禁止静默 unknown） |
| `compaction` | none | `dropped` | 密文不可解读（只对生成它的服务端有效），WARN 丢弃；Codex local 压缩以明文摘要 user 消息回灌，不经 compaction item |
| `compaction_trigger` | none | `dropped` | 请求控制信号，Codex 明确丢弃，不发给模型 |

### Tool 声明（c）

| Responses tool | Chat tools[] | 状态 | 说明 |
|---|---|---|---|
| `function` | function | `supported` | name/description/parameters/**strict**；parameters 出站做完整投影（对齐 opencode `ToolSchemaProjection.openAI`：强制 `type=object`，anyOf record 变体展平进 properties，递归去 null；仅 anyOf 展平时强制 `additionalProperties=false`） |
| `custom` | function + freeform parameters | `lossy_supported` | name **不加** `_custom` 后缀；根 description 保持原文，grammar 约束写入 `properties.input.description`（英文） |
| `shell` / `local_shell` | function name=`shell` / `local_shell` freeform | `lossy_supported` | 独立函数名，回程统一折 `custom_tool_call` |
| `apply_patch` | function name=`apply_patch` | `supported` | freeform `parameters={s:string}`；历史按客户端 operation/path/diff 直接回填（见 Input Item）；回程对 structured JSON 输出兜底折 V4A |
| `tool_search` | function name=`tool_search` | `supported` | |
| `namespace` | 展平 `ns__name` function | `lossy_supported` | 仅 function/custom 子项；嵌套 function 同样做 schema 投影 |
| `web_search` / `web_search_preview` | function `web_search` | `lossy_supported` | 无 server 搜索 |
| `code_interpreter` | none | `unsupported_by_backend` | 网关不支持；声明跳过（Debug） |
| `mcp` | `mcp__{server}__{tool}`（allowed_tools 列表） | `lossy_supported` | `server_description` 折入工具 description；连接/审批不注入；filter 不展开 |
| file_search / computer / image_generation / programmatic | none | `unsupported_by_backend` | 声明跳过 |

### 出站 SSE（c → Responses）

| Chat 流 | Responses 事件 | 状态 | 说明 |
|---|---|---|---|
| 首 chunk | `response.created` / `in_progress` | `supported` | |
| `delta.reasoning_content` / `delta.reasoning` | reasoning item + `reasoning_text.delta/done` | `lossy_supported` | 先于 content/tool；终态 `summary:[{summary_text}]`；无 encrypted/signature |
| `delta.content` | message + `output_text.delta` | `supported` | string；兼容 content part 数组（取 text） |
| `choices[].logprobs.content` | `output_text.delta/done.logprobs` | `supported` | 需请求 `top_logprobs` 且上游返回；无 bytes 字段；`include=message.output_text.logprobs` 在 Chat 源不再 WARN |
| `delta.tool_calls` function | `function_call` 链 + arguments delta/done | `supported` | 按 index 累积；**name 到齐再 open**（兼容先 id 后 name；opencode 对缺 id/name 直接报错，我们保留宽容以兼容分片上游） |
| `delta.tool_calls` name=`shell` / `local_shell` / `apply_patch` | `custom_tool_call` + input delta/done | `supported` | 参数在 `output_item.done` 一次给出；`SanitizeClientToolInput` 对单键 JSON 对象按任意键名取值解包；apply_patch 若输出 structured JSON 兜底折 V4A |
| `delta.tool_calls` freeform custom 名 | `custom_tool_call` + input delta/done | `supported` | `SanitizeClientToolInput` 解包/归一 |
| `delta.tool_calls` name=`tool_search` | `tool_search_call` | `supported` | arguments 随 item done |
| `delta.tool_calls` name=`web_search` | `web_search_call` 链 | `lossy_supported` | 无真实 sources |
| `delta.tool_calls` name=`code_interpreter` | `function_call` | `lossy_supported` | 网关不识别 code_interpreter，按普通 function_call 回程 |
| `delta.tool_calls` name=`mcp__*__*` | `function_call`（按声明还原 namespace） | `supported` | Chat 无 server MCP；必须回 client 可执行的 function_call（含 Codex `mcp__*` namespace）；先查请求声明映射，未声明才回退 `__` 拆分 |
| `finish_reason=stop` / `tool_calls` | `response.completed` | `supported` | |
| `finish_reason=length` | `response.incomplete` reason=`max_output_tokens` | `supported` | |
| `finish_reason=content_filter` | `response.incomplete` + refusal 事件链 | `supported` | 累积 `delta.refusal`，缺省用 fallback 文案；清掉半截 text/tool output |
| usage 末包（空 choices） | 填 `usage` | `supported` | `input_tokens` 为含 cache 总量（同 opencode/Responses 官方）；totals + `input_tokens_details.cached_tokens` + `output_tokens_details.reasoning_tokens`；缺 `total_tokens` 时按 prompt+completion 兜底；并写 `cache_read_input_tokens` 兼容 a 路径字段名 |
| `[DONE]` 且无 finish_reason | 补 completed/incomplete | `supported` | FeedDone |
| 流中断 | `response.failed` | `supported` | Backend Fail |

### 已知缺口（c，产品边界外 / 硬限制）

| 项 | 说明 |
|---|---|
| 多模态输入 | `input_image`（URL/data URL）真传 `image_url`；`input_file` / 仅 `file_id` 图片无 Chat 槽位，转换报错（不占位降级） |
| 厂商 `reasoning_content` 无 encrypted/signature | 明文 reasoning 有损（summary 承载全文）；a 路径 signature/encrypted 不在 c 复现 |
| Chat 原生 Anthropic 式 `citations_delta` | 五家主路径无官方等价；联网多走 tool_calls；非标 annotations 本期不做 |
| hosted tools **真实** server 执行 | Chat 仅 function 形状；出站 completed 无真实 sources/logs |
| Chat 原生 `tools[].type=custom` + grammar | freeform 统一 function 化以兼容通用上游；grammar 约束写入 `input` 字段 description（英文），根 description 保持原文 |
| 出站 logprobs `bytes` 字段 | Chat TokenLogprob.bytes 不映射到 Responses（官方 delta logprobs 无 bytes） |
| Responses-only：`max_tool_calls` / `background` / `conversation` / `context_management` / `prompt` 模板 | 无 Chat 等价或产品边界外 |
| Chat-only：`frequency_penalty` / `presence_penalty` / `seed` / `stop` / `n` / `logit_bias` / `prediction` / `audio`/`modalities` / `web_search_options` | Responses 请求无对应顶层字段，无法从客户端映射 |
| orphan tool 配对 | **已补**：缺 output 时 WARN + 占位 `role=tool` |

### 收口内已打磨（2026-07-22）

| 项 | 行为 |
|---|---|
| Chat `reasoning_content` 出站 + 入站回灌 | 出站→Responses reasoning + `reasoning_text.*`；入站折 `assistant.reasoning_content`（工具环同框）；无 encrypted |
| computer/file_search/image_generation 等历史 | 显式 WARN + drop（非 unknown 静默） |
| compaction 历史 | WARN + 丢弃（密文不可解读；Codex local 压缩以明文摘要 user 消息回灌，不走 compaction item） |
| compaction_trigger / mcp_list_tools 历史 | DEBUG 丢弃：前者是请求控制信号（Codex 明确不保留）；后者 opencode 无此类型、Codex 不转文本（工具经 ToolSpec/请求 tools 声明） |
| tool_calls 分片 name 晚到 | 有 name 再 `output_item.added`，避免误判 function |
| `delta.content` 数组 | 解析 text part，不整段 Feed 失败 |
| `developer` role | 会话中折 `<system-update>` user；`instructions` 才是唯一 `role=system`（对齐 opencode） |
| `tool_choice.allowed_tools` | 精确过滤 tools + mode（auto/required） |
| `prompt_cache_key` | 透传到 Chat body（官方字段） |
| `prompt_cache_options` | Chat 无顶层槽位，DEBUG + 丢弃（2026-07-31） |
| structured `text.format` | 忽略（网关不支持 structured output） |
| `function.strict` | → Chat `tools[].function.strict` |
| `text.verbosity` / `service_tier` | → Chat 同名字段透传 |
| `safety_identifier` / `metadata` / `store` / `moderation` | → Chat 同形透传 |
| `reasoning.effort` | → Chat `reasoning_effort`，任意值透传（`max` 也透传） |
| `top_logprobs` | → Chat `logprobs` + `top_logprobs` |
| `stream_options.include_obfuscation` | 透传；始终 `include_usage` |
| usage details | `cached_tokens` + `reasoning_tokens` 出站明细 + `cache_read_input_tokens` 兼容字段 |
| 工具历史无活动工具 | 显式发 `tools: []`（对齐 pi 的 Anthropic 代理兼容；无工具历史仍省略） |
| shell/local_shell/apply_patch 回程统一 custom | `custom_tool_call` 透传（对齐 a 路径）；工具 schema 投影仅在 anyOf 展平时强制 `additionalProperties=false`（对齐 opencode） |
| usage 缺 `total_tokens` | 按 `prompt + completion` 兜底（同 opencode/pi） |
| 出站 token logprobs | Chat `choices[].logprobs` → `response.output_text.delta/done.logprobs` + content part |
| OfInputMessage / OfOutputMessage | chatconvert 防御分支（SDK 极少落点） |
| assistant 无文本 | wire 显式 `content:null`（工具环/reasoning-only，对齐 opencode） |
| tool 空输出 | wire 显式 `content:""` |

### 实现包

| 包 | 职责 |
|---|---|
| `internal/chatconvert` | Responses → Chat 请求 |
| `internal/chatclient` | HTTP SSE 客户端 |
| `internal/chatstreamconv` | Chat chunk → Responses SSE |
| `internal/backend` | `ChatBackend` 组装 |

## 资料来源

- OpenAI API Reference: `https://developers.openai.com/api/reference/resources/responses/methods/create`
- OpenAI SDK: `github.com/openai/openai-go/v3@v3.42.0`
- Anthropic Messages docs: `https://platform.claude.com/docs/en/api/messages`
- Anthropic streaming docs: `https://platform.claude.com/docs/en/build-with-claude/streaming`
- Anthropic SDK: `github.com/anthropics/anthropic-sdk-go@v1.57.0`

## Request Parameters

| OpenAI 参数 | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `model` | `MessageNewParams.Model` | `supported` | 保留 Codex-facing model alias，后端 source 可替换实际模型 |
| `input` string | user text message | `supported` | 转为单条 user text block |
| `input` item list | `messages` / `system` / tool blocks | `lossy_supported` | 仅部分 item 语义支持，详见 Input Item Union |
| `instructions` | top-level `system` | `supported` | 作为 developer 指令段折入 system text |
| `max_output_tokens` | `max_tokens` | `lossy_supported` | 客户端值优先；未设置时使用 `anthropic.default_max_tokens`，默认 16384。**注意**：OpenAI `max_output_tokens` 预算含 reasoning token，Anthropic `max_tokens` 仅限制可见输出（thinking 由 `budget_tokens` 独立控制），两者计数口径不一致 |
| `temperature` | `temperature` | `supported` | 直接映射 |
| `top_p` | `top_p` | `supported` | 直接映射 |
| `parallel_tool_calls` | `disable_parallel_tool_use` 反向映射 | `supported` | `false` 时禁用 Anthropic 并行 tool use |
| `reasoning.effort` | `output_config.effort` + `thinking` | `lossy_supported` | `none`→thinking disabled；`low`/`medium`/`high`/`xhigh`/`max`→同名 Anthropic `output_config.effort`（覆盖官方全部五档）；未知档仅开 thinking 不伪造 effort。兼容后端对不支持的值可静默降级 |
| `reasoning.summary` | thinking display / summary events | `lossy_supported` | `concise` 映射到 summarized 输出 |
| `reasoning.generate_summary` | thinking display | `unsupported_by_backend` | deprecated，被 `reasoning.summary` 取代；非空时 **WARN + 忽略**，不复用 `summary` 路径 |
| `metadata` | response echo + Anthropic `metadata.user_id` | `lossy_supported` | `metadata.user_id` 透传到 Anthropic `metadata.user_id`；其余键值对无 Anthropic 等价能力，仅响应 echo 回显。未透传的键值对触发 WARN |
| `text.format.json_schema` | none | `dropped` | 网关不支持 structured output；忽略，不写 `output_config.format`（DeepSeek 等端点 `output_config` 仅支持 `effort`） |
| `text.format.json_object` | none | `dropped` | 网关不支持 structured output；忽略，不再注入合成工具 |
| `text.verbosity` | none | `unsupported_by_backend` | a 忽略；Chat 见 c 专节 |
| `tools` | `tools` | `lossy_supported` | 仅部分工具类型支持，详见 Tool Union |
| `tool_choice` | `tool_choice` | `lossy_supported` | 仅部分 choice 支持；具体工具选择必须精确匹配声明的 type/name，详见 Tool Choice Union |
| `previous_response_id` | none | `unsupported_by_backend` | 网关无 session store，不做 enrich；Codex 主路径不传此字段（客户端完整回灌 `input`）。若请求携带非空值则 WARN + 忽略 |
| `store` | response echo only | `raw_preserved` | 无本地会话存储/回填；仅在响应对象 echo 请求值 |
| `truncation` | response echo only | `raw_preserved` | Anthropic 无直接等价策略 |
| `include` | partial | `lossy_supported` | 已满足：`reasoning.encrypted_content`、`web_search_call.action.sources`、`message.input_image.image_url`；`message.output_text.logprobs` 仅 Chat 源 satisfied；其余（file_search/computer 等）WARN + 忽略 |
| `prompt_cache_key` | none | `unsupported_by_backend` | Anthropic 用内容 hash 缓存(cache_control)，不认客户端 key；网关已自主设 cache_control；非空时 **DEBUG + 忽略**（Codex 常发，可控协议差异） |
| `prompt_cache_options` | none | `unsupported_by_backend` | 网关已自主在 system/tools/顶层设 cache_control（TTL 可配；MCP toolset inject 后重定位 tools 末项断点；`1h` 带 `extended-cache-ttl-2025-04-11`），OpenAI options 结构对 Anthropic 无意义；mode/ttl 非空时 **DEBUG + 忽略** |
| `prompt_cache_retention` | none | `unsupported_by_backend` | deprecated（in_memory/24h），与 Anthropic cache_control 语义不同；非空时 **DEBUG + 忽略**（不映射 TTL） |
| `prompt` | none | `unsupported_by_backend` | 引用 prompt template 与变量，需服务端模板存储与解析；网关无 OpenAI prompt 存储能力；`prompt.id` 非空时 **WARN + 忽略** |
| `background` | none | `unsupported_by_backend` | 当前网关只支持同步 SSE |
| `conversation` | none | `unsupported_by_backend` | 本地 store 不是 OpenAI Conversation API |
| `context_management` | none | `unsupported_by_backend` | 请求级上下文管理（含 compaction）；OpenAI 服务端压缩，网关不做（压缩由 Codex 客户端 local 完成，明文摘要 user 消息回灌）；非空时 **WARN + 忽略**。历史 `compaction` item 密文不可解读，WARN 丢弃；`compaction_trigger` 丢弃 |
| `max_tool_calls` | none | `unsupported_by_backend` | Anthropic 无直接请求参数；多轮 tool 环由客户端控制，网关单请求内不做计数截断；非空时 **WARN + 忽略** |
| `service_tier` | none | `dropped` | a 忽略；Chat 见 c 专节 |
| `safety_identifier` | none | `unsupported_by_backend` | 后端无等价字段 |
| `moderation` | none | `unsupported_by_backend` | OpenAI 输入/输出 moderation 配置，Anthropic Messages 无等价参数；配置非空时 **WARN + 忽略** |
| `stream_options.include_obfuscation` | none | `unsupported_by_backend` | Anthropic streaming 无等价 obfuscation |
| `top_logprobs` | none | `unsupported_by_backend` | Anthropic Messages 无 OpenAI output logprobs 等价 |
| `user` | deprecated | `unsupported_by_backend` | OpenAI 已废弃字段，建议改用 `safety_identifier`/`prompt_cache_key`/`metadata.user_id`；当前 WARN + 丢弃（不透传给上游） |

## Input Content Union

| OpenAI content | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `input_text` | `text` block | `supported` | 文本语义保留 |
| `output_text`（作为 input 历史 content） | `text` block | `supported` | 非 Input Content 官方成员，但是 Codex 回灌 wire；解码时归一为 `input_text` 再转换 |
| `refusal`（作为 input 历史 content） | `text` block | `lossy_supported` | 折成可见文本（`[refusal] …`），避免整段 assistant 历史被抹掉 |
| `input_image.image_url` | `image` block | `supported` | URL 或 data URI 映射 |
| `input_image.file_id` | none | `unsupported_by_backend` | 网关没有 OpenAI Files 凭据来拉取文件 |
| `input_file.file_data` | `document` block | `supported` | 以 base64/plain text 方式构造 document |
| `input_file.file_url` | `document` block | `supported` | URL document |
| `input_file.file_id` | none | `unsupported_by_backend` | 同 OpenAI Files 限制 |
| `prompt_cache_breakpoint` | none | `unsupported_by_backend` | 网关已自主设 cache_control（system/tools 末尾 + 顶层 automatic；MCP 注入后 tools 末项重定位），不读 OpenAI breakpoint，忽略 |

## Input Item Union

| OpenAI item | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `message` / `EasyInputMessage` | message/system text | `lossy_supported` | system/developer 仅保留文本；图片等非文本内容无法放入 Anthropic system。Codex 回灌 assistant 正文见下「output_text 回灌」行 |
| `message` + history `content[output_text]` | assistant text | `supported` | Codex 回灌主路径；raw 归一后走 `appendMessage` |
| `message` + history `content[refusal]` | assistant text | `lossy_supported` | refusal 折成可见文本 |
| `input_message` | message | `supported` | SDK 三个 `message` discriminator 实测几乎总落到 `OfMessage`；无独立分支需求 |
| `output_message` | assistant text | `supported` | 兜底：若 SDK 解到 `OfOutputMessage` 则转 assistant text；真·Codex wire 通常是 `type=message` + `output_text` |
| `file_search_call` | none | `dropped` | 历史回灌 WARN + 丢弃（不 raw dump）；工具声明阶段 fail-fast |
| `computer_call` | none | `dropped` | 历史回灌 WARN + 丢弃；工具声明 fail-fast |
| `computer_call_output` | none | `dropped` | 同上 |
| `web_search_call`（input 历史） | `server_tool_use` + 空 result + sources 文本 | `lossy_supported` | query→input；`web_search_tool_result` 必须放在 assistant 消息（DeepSeek 400 实测）；无 encrypted 时 result content 空；URL 折可见文本；open_page/find 折 query。出站 stream 见 Output/SSE |
| `function_call` | assistant `tool_use` | `supported` | `arguments` 转 tool input |
| `function_call_output` | user `tool_result` | `supported` | `output` string 或 content 数组（`input_text`/`input_image`/`input_file`）→ tool_result 多 part；仅 `file_id` 无法拉取时 WARN + 丢弃 |
| `tool_search_call` | assistant `tool_use` name=`tool_search` | `supported` | 已有语义分支 |
| `tool_search_output` | dynamic tools + tool_result | `supported` | 工具注入并记录 developer marker |
| `additional_tools` | none | `unsupported_by_backend` | Responses Lite 产物；网关统一 `use_responses_lite=false`，该 item 不会出现，移除转换分支 |
| `reasoning` | `thinking` | `lossy_supported` | 网关只接受明文 thinking：summary 优先，空则回退 content[].reasoning_text；有 encrypted 且有文本→thinking 签名回灌；无文本（redacted 密文）静默忽略；无 encrypted 丢弃 |
| `compaction` | none | `dropped` | 密文不可解读（只对生成它的服务端有效），WARN 丢弃；Codex local 压缩以明文摘要 user 消息回灌，不经 compaction item |
| `image_generation_call` | none | `dropped` | 历史回灌 WARN + 丢弃；工具声明 fail-fast |
| `code_interpreter_call` | none | `dropped` | 网关不支持 code_interpreter；历史回灌静默忽略，不产生 code_execution block |
| `local_shell_call` | assistant `tool_use` name=`shell` | `lossy_supported` | 命令文本 + `env`/`working_directory`/`timeout_ms`/`user`/`status` 折入 tool_use.input；无 Anthropic 原生 shell 协议 |
| `local_shell_call_output` | user `tool_result` | `lossy_supported` | 文本 tool_result；可前缀 `[status=…]`；item.id 作 tool_use_id |
| `shell_call` | assistant `tool_use` name=`shell` | `lossy_supported` | 命令文本 + `environment_type` + `timeout_ms`/`max_output_length`/`status`/`caller_type`/`caller_id` 折入 tool_use.input；无 Anthropic 原生 shell 协议 |
| `shell_call_output` | user `tool_result` | `lossy_supported` | stdout/stderr + `[status]`/`[max_output_length]`/`[exit_code]`/`[timeout]` 折文本；caller 不映射 |
| `apply_patch_call` | assistant `tool_use` name=`apply_patch` | `supported` | operation/path/diff 结构化 JSON 透传；`status`/`caller` 折入 tool_use.input |
| `apply_patch_call_output` | user `tool_result` | `lossy_supported` | `[status=…]` + 可选日志文本；caller 不映射 |
| `mcp_list_tools` | none | `dropped` | opencode 无此类型；Codex 不把 AdditionalTools 转文本（工具经 ToolSpec/请求 tools 声明）；DEBUG 丢弃 |
| `mcp_approval_request` | none | `dropped` | Anthropic 无审批协议；网关不实现，历史回灌 WARN + 丢弃 |
| `mcp_approval_response` | none | `dropped` | Anthropic 无审批协议；网关不实现，历史回灌 WARN + 丢弃 |
| `mcp_call` | `tool_use` name=`mcp__<server>__<tool>` + `tool_result` | `supported` | 扁平名直接回填；error 文本并入 tool_result |
| `custom_tool_call` | assistant custom `tool_use` | `supported` | freeform custom tool 支持 |
| `custom_tool_call_output` | user `tool_result` | `supported` | `output` string 或 content list → tool_result 多 part；仅 `file_id` 无法拉取时 WARN + 丢弃 |
| `compaction_trigger` | none | `dropped` | 请求控制信号，Codex 明确丢弃，不发给模型 |
| `item_reference` | none | `dropped` | 网关无 session store；历史回灌 WARN + 丢弃 |
| `program` | none | `dropped` | 历史回灌 WARN + 丢弃 |
| `program_output` | none | `dropped` | 同上 |

## 未知 Input Item 兜底

| OpenAI item | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| SDK 尚未登记 / `GetType` 未知的 input item | system raw marker `<openai_input_item>` | `raw_preserved` | **仅**作为前向兼容兜底：已知无等价类型（file_search / computer / image_generation / program / item_reference / additional_tools / MCP approval 等）一律 `dropped`（WARN + 不 dump）。未知类型仍注入 system 以免整段历史静默蒸发；与「禁止把已知无等价 JSON 灌 system」不冲突。若产品希望未知也 drop，可改此兜底。 |

## 转换后完整性保证

| 保证项 | 触发条件 | 处理 | 说明 |
|---|---|---|---|
| orphan `tool_use` 兜底 | input 历史含 `function_call`/`custom_tool_call`/`shell`/`apply_patch`/`tool_search_call` 但缺对应 `*_output`（中断后 resume / failover 丢历史 / 客户端 bug） | 补 `is_error=true` 占位 `tool_result` | 避免上游 Anthropic 以 `tool_use without tool_result` 400 拒绝整请求。占位补在该 tool_use 后的首个 user message 前部；无后续 user message 则新建。code_interpreter_call 已整体丢弃，不参与配对。实现见 `internal/convert/request.go` `ensureToolUsePaired`；WARN 暴露该客户端异常 |

## Tool Union

| OpenAI tool | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `function` | client tool | `supported` | JSON schema 转 `input_schema` |
| `file_search` | none | `unsupported_by_backend` | 无 OpenAI vector store 后端；请求时返回明确转换错误 |
| `computer` | none | `unsupported_by_backend` | 需 computer use 执行环境；请求时返回明确转换错误 |
| `computer_use_preview` | none | `unsupported_by_backend` | 同上；请求时返回明确转换错误 |
| `web_search` | Anthropic web search server tool (20260209) | `lossy_supported` | `filters.allowed_domains` → `allowed_domains`；`user_location` → `user_location`；`search_context_size` 无 Anthropic 字段 → WARN + 忽略 |
| `web_search_preview` | Anthropic web search server tool (20260209) | `lossy_supported` | 同 web_search：`user_location` 映射；`search_context_size` WARN + 忽略；preview 无 domains filter |
| `mcp` | 扁平 client tool `mcp__<server>__<tool>` | `lossy_supported` | `allowed_tools: string[]` → 逐个展开为 function 声明，`server_description` 折入工具 description；`allowed_tools: filter`/空列表 → 不声明（经 tool_search 动态提供）；连接字段（server_url/headers/require_approval/connector_id/tunnel_id）不再注入或 fail-fast，交给 Codex 客户端本地连接执行 |
| `code_interpreter` | none | `unsupported_by_backend` | 网关不支持；声明时明确转换错误（fail-fast） |
| `programmatic_tool_calling` | none | `unsupported_by_backend` | 无 Anthropic 等价物；DEBUG + 忽略，透传上游自行决定 |
| `image_generation` | none | `unsupported_by_backend` | Anthropic Messages 不生成 OpenAI image result；请求时返回明确转换错误 |
| `local_shell` | client tool `shell` | `lossy_supported` | 统一省略 type；声明 freeform `shell`；历史元数据见 Input Item（env/cwd/timeout/user/status） |
| `shell` | client tool `shell` | `lossy_supported` | 统一省略 type；声明 freeform `shell`；历史 limits/caller/environment_type 折入 input（见 Input Item）；skills 细节仍 lossy |
| `custom` | client tool | `lossy_supported` | 统一省略 type；freeform `s` 字段 description 只声明必填格式（英文，取 `format.definition`，不含工具级 description），shell/apply_patch 用默认文案；`format.definition` 折入 s 字段 description |
| `namespace` | flattened tool names | `lossy_supported` | namespace 被拼入 tool name；子工具仅支持 `function` / `custom`；回程按请求声明还原 namespace/name，未声明才回退最后一个 `__` 拆分 |
| `tool_search` | client tool `tool_search` | `supported` | 当前按普通 tool 暴露 |
| `apply_patch` | client tool `apply_patch` | `supported` | 统一省略 type；freeform `{s:string}` schema（Codex 只消费 V4A 文本）；历史 operation/path/diff 直接回填；回程 structured→V4A 兜底 |

## Tool Choice Union

| OpenAI choice | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `none` | `tool_choice.none` | `supported` | 直接映射 |
| `auto` | `tool_choice.auto` | `supported` | 直接映射 |
| `required` | `tool_choice.any` | `supported` | Anthropic 使用 `any` |
| `function` | `tool_choice.tool(name)` | `supported` | 仅在声明了相同 type/name 的 function 时映射，否则明确转换失败 |
| `custom` | `tool_choice.tool(name)` | `supported` | 仅在声明了相同 type/name 的 custom 时映射，否则明确转换失败 |
| `apply_patch` | `tool_choice.tool("apply_patch")` | `supported` | 仅在已声明 `apply_patch` 时映射，否则明确转换失败 |
| `shell` | `tool_choice.tool("shell")` | `supported` | 仅在已声明 `shell` 时映射；`local_shell` 不是此 specific choice 的等价声明 |
| `allowed_tools` | filtered tool set + choice mode | `lossy_supported` | 仅支持 `auto`/`required`；每个 allowed 条目按 `type`、namespace、name 与已声明工具精确匹配；MCP 条目按 `server_label`（可选 `name`）展开为扁平工具；未知 mode、转换名冲突和 hosted/未声明 MCP 条目明确报错 |
| hosted tool choice | none | `lossy_supported` | 无 Anthropic 等价 choice：DEBUG 后忽略，声明的工具照常下发，是否调用由上游决定（不代劳拒绝） |
| `mcp` | none | `lossy_supported` | 无等价 MCP choice：DEBUG 后忽略，交上游决定（不代劳拒绝） |
| `programmatic_tool_calling` | none | `unsupported_by_backend` | 无等价 programmatic tool choice；请求时返回明确转换错误 |

## Output Item Union

| OpenAI output item | Anthropic 来源 | 当前状态 | 说明 |
|---|---|---|---|
| `message` | text block | `supported` | 输出 text message |
| `reasoning` | thinking block | `lossy_supported` | 有 summary/文本 + signature 时回灌 thinking；redacted_thinking 不再回灌（静默忽略） |
| `function_call` | `tool_use` | `supported` | 回程 arguments 把整数值 `N.0` 收成整数，避免 Codex serde 失败 |
| `function_call_output` | request replay only | `supported` | 作为 input item 回放（含 content 数组形态） |
| `custom_tool_call` | custom `tool_use` | `supported` | freeform 单键对象按任意键名取值解包（shell/custom/apply_patch），多键 structured 形态 apply_patch 兜底折 V4A |
| `custom_tool_call_output` | request replay only | `supported` | 作为 input item 回放（含 content list 形态） |
| `tool_search_call` | `tool_use` name=`tool_search` | `supported` | `toolSearchCallKind` 产出 `tool_search_call` item（execution=client，arguments 随 done 一次性给出，不流式 delta） |
| `tool_search_output` | request replay only | `supported` | 由 Codex 本地执行 tool_search 后回灌；网关入站注入 tools + tool_result。出站不生成该 item（后端非搜索持有者） |
| `additional_tools` | none | `unsupported_by_backend` | Responses Lite 产物；网关统一 `use_responses_lite=false`，该 item 不会出现 |
| `compaction` | response compact API | `raw_preserved` | 非模型 stream output |
| `file_search_call` | none | `unsupported_by_backend` | 无等价 |
| `web_search_call` | Anthropic server web search | `supported` | server_tool_use + web_search_tool_result 映射，结果 URL 回显为 sources |
| `computer_call` | none | `unsupported_by_backend` | 无等价 |
| `computer_call_output` | none | `unsupported_by_backend` | 无等价 |
| `program` | none | `unsupported_by_backend` | 无等价 |
| `program_output` | none | `unsupported_by_backend` | 无等价 |
| `image_generation_call` | none | `unsupported_by_backend` | 无等价 |
| `code_interpreter_call` | none | `unsupported_by_backend` | 网关不支持 code_interpreter；上游 code_execution 回程按 skip 处理，不产生该 item |
| `local_shell_call` | `custom_tool_call` name=`shell` | `lossy_supported` | 出站以 `custom_tool_call` 形态发出（Codex 实测可消费）；不生成专用 `local_shell_call` item type |
| `local_shell_call_output` | request replay only | `supported` | 不作为 output item 生成；入站历史转 `tool_result` 见 Input Item |
| `shell_call` | `custom_tool_call` name=`shell` | `lossy_supported` | 出站以 `custom_tool_call` 形态发出（Codex 实测可消费）；不生成专用 `shell_call` item type |
| `shell_call_output` | request replay only | `supported` | 不作为 output item 生成；入站历史转 `tool_result` 见 Input Item |
| `apply_patch_call` | `custom_tool_call` name=`apply_patch` | `lossy_supported` | 出站以 `custom_tool_call` 形态发出（Codex 实测可消费）；不生成专用 `apply_patch_call` item type |
| `apply_patch_call_output` | request replay only | `supported` | 不作为 output item 生成；入站历史转 `tool_result` 见 Input Item |
| `mcp_call` | `function_call` namespace=`mcp__<server>` name=`<tool>` | `lossy_supported` | 上游若返回 MCP 工具调用，按扁平名回成 function_call 交客户端执行；不再生成 mcp_call item 与事件链 |
| `mcp_list_tools` | none | `unsupported_by_backend` | 出站不生成；历史丢弃（DEBUG） |
| `mcp_approval_request` | none | `unsupported_by_backend` | 出站不生成；`require_approval≠never` 降级 never + WARN；历史回灌见 Input Item（`dropped`，WARN + 丢弃） |
| `mcp_approval_response` | none | `unsupported_by_backend` | 出站不生成；历史回灌见 Input Item（`dropped`，WARN + 丢弃） |

## Responses SSE Events

| OpenAI SSE event | Anthropic 来源 | 当前状态 | 说明 |
|---|---|---|---|
| `response.created` | `message_start` | `supported` | 已输出 |
| `response.in_progress` | `message_start` | `supported` | 已输出 |
| `response.completed` | `message_stop` | `supported` | 已输出 |
| `response.incomplete` | `message_stop` + stop reason | `lossy_supported` | `max_tokens` 与 refusal 使用合法 incomplete reason；`pause_turn` 不写非法 reason |
| `response.failed` | upstream error | `supported` | 已输出 |
| `error` | Anthropic error event | `supported` | 上游 error 事件现在同时发出 OpenAI `error` 事件（code=upstream_error + message）与 `response.failed` 终态 |
| `response.queued` | none | `unsupported_by_backend` | 后端无队列状态 |
| `response.output_item.added` | content block start | `supported` | text/reasoning/tool use 支持 |
| `response.output_item.done` | content block stop | `supported` | text/reasoning/tool use 支持 |
| `response.content_part.added` | text block start | `supported` | output_text 支持 |
| `response.content_part.done` | text block stop | `supported` | output_text 支持 |
| `response.output_text.delta` | `text_delta` | `supported` | 已输出 |
| `response.output_text.done` | text block stop | `supported` | 已输出 |
| `response.output_text.annotation.added` | `citations_delta` | `lossy_supported` | `web_search_result_location`→`url_citation`；其它→`file_citation`；start/end 占位；**同时写入终态 content.annotations**；未知 type WARN + 丢弃 |
| `response.refusal.delta` | Anthropic refusal | `supported` | 已输出 |
| `response.refusal.done` | Anthropic refusal | `supported` | 已输出 |
| `response.reasoning_text.delta` | `thinking_delta` | `supported` | 已输出 |
| `response.reasoning_text.done` | thinking block stop | `supported` | 已输出 |
| `response.reasoning_summary_part.added` | summarized thinking | `supported` | summarized 模式 |
| `response.reasoning_summary_text.delta` | summarized thinking | `supported` | summarized 模式 |
| `response.reasoning_summary_text.done` | summarized thinking | `supported` | summarized 模式 |
| `response.reasoning_summary_part.done` | summarized thinking | `supported` | summarized 模式 |
| `response.function_call_arguments.delta` | `input_json_delta` | `supported` | 普通 function tool |
| `response.function_call_arguments.done` | tool_use stop | `supported` | 普通 function tool |
| `response.custom_tool_call_input.delta` | custom tool stop | `supported` | freeform custom tool |
| `response.custom_tool_call_input.done` | custom tool stop | `supported` | freeform custom tool |
| `response.file_search_call.searching` | none | `unsupported_by_backend` | 无等价 |
| `response.file_search_call.in_progress` | none | `unsupported_by_backend` | 无等价 |
| `response.file_search_call.completed` | none | `unsupported_by_backend` | 无等价 |
| `response.web_search_call.searching` | Anthropic web search | `supported` | server_tool_use(web_search) 触发 |
| `response.web_search_call.in_progress` | Anthropic web search | `supported` | server_tool_use(web_search) 触发 |
| `response.web_search_call.completed` | Anthropic web search | `supported` | web_search_tool_result 触发 |
| `response.code_interpreter_call_code.delta` | none | `unsupported_by_backend` | 网关不支持 code_interpreter，不再生成 |
| `response.code_interpreter_call_code.done` | none | `unsupported_by_backend` | 网关不支持 code_interpreter，不再生成 |
| `response.code_interpreter_call.in_progress` | none | `unsupported_by_backend` | 网关不支持 code_interpreter，不再生成 |
| `response.code_interpreter_call.interpreting` | none | `unsupported_by_backend` | 网关不支持 code_interpreter，不再生成 |
| `response.code_interpreter_call.completed` | none | `unsupported_by_backend` | 网关不支持 code_interpreter，不再生成 |
| `response.image_generation_call.in_progress` | none | `unsupported_by_backend` | 无等价 |
| `response.image_generation_call.generating` | none | `unsupported_by_backend` | 无等价 |
| `response.image_generation_call.partial_image` | none | `unsupported_by_backend` | 无等价 |
| `response.image_generation_call.completed` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_call_arguments.delta` | none | `unsupported_by_backend` | 不再生成：a 路径 MCP 走 `function_call.arguments.delta` |
| `response.mcp_call_arguments.done` | none | `unsupported_by_backend` | 不再生成：a 路径 MCP 走 `function_call.arguments.done` |
| `response.mcp_call.in_progress` | none | `unsupported_by_backend` | 不再生成 |
| `response.mcp_call.completed` | none | `unsupported_by_backend` | 不再生成 |
| `response.mcp_call.failed` | none | `unsupported_by_backend` | 不再生成 |
| `response.mcp_list_tools.in_progress` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_list_tools.completed` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_list_tools.failed` | none | `unsupported_by_backend` | 无等价 |
| `response.audio.delta` | none | `unsupported_by_backend` | 当前后端不产生 audio |
| `response.audio.done` | none | `unsupported_by_backend` | 当前后端不产生 audio |
| `response.audio.transcript.delta` | none | `unsupported_by_backend` | 当前后端不产生 audio transcript |
| `response.audio.transcript.done` | none | `unsupported_by_backend` | 当前后端不产生 audio transcript |

## Anthropic Content Blocks And Stream Events

| Anthropic block/event | OpenAI 映射 | 当前状态 | 说明 |
|---|---|---|---|
| request `text` | message content text | `supported` | 双向常用路径 |
| request `image` | input_image | `supported` | OpenAI URL/data URI 到 Anthropic image |
| request `document` | input_file | `supported` | 文件输入 |
| request `thinking` | reasoning | `supported` | 用于回放 thinking signature |
| request `redacted_thinking` | none | `dropped` | 网关只接受明文 thinking；redacted 密文静默忽略，不回灌 |
| request `tool_use` | function/custom/tool call | `supported` | 常用工具路径 |
| request `tool_result` | tool call output | `supported` | 常用工具结果 |
| stream `text` | output message | `supported` | 已输出 output_text |
| stream `thinking` | reasoning | `supported` | 已输出 reasoning events |
| stream `redacted_thinking` | none | `dropped` | 网关只接受明文 thinking；静默跳过，不生成 reasoning item |
| stream `tool_use` | function/custom tool call | `supported` | 已输出 tool call events |
| stream `server_tool_use` | built-in tool call | `supported` | name=web_search 映射为 web_search_call；code_execution 未登记（网关不支持 code_interpreter）；未登记的 server tool（如 web_fetch）及对应 result：**WARN + skip，不中断流**（非 response.failed） |
| stream `web_search_tool_result` | web search call result | `supported` | 完成 web_search_call（completed + output_item.done） |
| stream `web_fetch_tool_result` | web fetch result | `unsupported_by_backend` | OpenAI Responses 无直接等价 |
| stream `code_execution_tool_result` | none | `unsupported_by_backend` | 网关不支持 code_interpreter；skip + WARN，不中断流（同其它无等价 server result） |
| stream `bash_code_execution_tool_result` | none | `unsupported_by_backend` | Anthropic 托管 shell 执行结果，OpenAI Responses 无等价输出 item；对应 server_tool_use 在 start 阶段已 skip，result 阶段同步 skip + WARN，不中断流 |
| stream `text_editor_code_execution_tool_result` | none | `unsupported_by_backend` | Anthropic 托管 text editor 执行结果，OpenAI Responses 无等价；对应 server_tool_use start 阶段已 skip，result 同步 skip + WARN |
| stream `tool_search_tool_result` | none | `unsupported_by_backend` | Anthropic 服务端 tool_search 结果，网关的 tool_search 走客户端工具语义（非 server tool），此 server-side result 无等价；start 阶段已 skip，result 同步 skip + WARN |
| stream `container_upload` | none | `unsupported_by_backend` | 无 OpenAI Responses 等价输出 |
| stream `mid_conversation_system` | none | `unsupported_by_backend` | OpenAI Responses 无原生「中途 system 消息」输出项；当前 WARN + 跳过（不中断流），后续可考虑转为 developer marker |
| event `ping` | no-op | `supported` | 忽略是正确行为 |
| event `message_start` | `response.created/in_progress` | `supported` | 已处理 |
| event `content_block_start` | item/content start | `lossy_supported` | 已支持类型映射；未知类型会输出诊断性 failed |
| event `content_block_delta` | delta events | `lossy_supported` | text/thinking/tool/citation 已处理；未知 server tool delta 随 skip |
| event `content_block_stop` | done events | `lossy_supported` | 未知 block stop 未诊断 |
| event `message_delta` | stop reason / usage | `lossy_supported` | `max_tokens` 与 refusal 已映射；`pause_turn` 结束为 incomplete 但不写非法 reason |
| event `message_stop` | terminal response | `supported` | 已处理 |
| event `error` | response.failed/error | `supported` | raw SSE client 转 synthetic error |

## Enum Mapping

| 枚举类别 | OpenAI 值 | Anthropic 值 | 当前状态 | 说明 |
|---|---|---|---|---|
| role | `user` | `user` | `supported` | 直接映射 |
| role | `assistant` | `assistant` | `supported` | 直接映射 |
| role | `system` | top-level `system` | `lossy_supported` | Anthropic 无 system message role |
| role | `developer` | top-level `system` with marker | `lossy_supported` | 通过 marker 保留层级 |
| assistant phase | `commentary` | none | `raw_preserved` | 仅 Codex 客户端渲染用；不注入请求文本，避免上游模型模仿 |
| assistant phase | `final_answer` | none | `raw_preserved` | 仅 Codex 客户端渲染用；不注入请求文本，避免上游模型模仿 |
| response status | `in_progress` | message active | `supported` | created 后输出 |
| response status | `completed` | `end_turn/tool_use/stop_sequence` | `supported` | 需按 stop reason |
| response status | `incomplete` | `max_tokens` | `supported` | reason=`max_output_tokens` |
| response status | `failed` | upstream error | `supported` | response.failed |
| response status | `queued` | none | `unsupported_by_backend` | 无队列状态 |
| response status | `cancelled` | client cancel | `supported` | 客户端中途断开：metrics 记 `canceled`/499；**不写** `response.cancelled` / `response.failed` SSE（对端通常已断，写 socket 无收益）。终态后断开仍按 completed 收尾 |
| incomplete reason | `max_output_tokens` | `max_tokens` | `supported` | 直接映射 |
| incomplete reason | `content_filter` | policy/refusal | `supported` | refusal 映射 |
| stop reason | none | `end_turn` | `supported` | completed |
| stop reason | none | `tool_use` | `supported` | completed，客户端继续工具回合 |
| stop reason | none | `stop_sequence` | `supported` | completed |
| stop reason | none | `max_tokens` | `supported` | incomplete/max_output_tokens |
| stop reason | none | `pause_turn` | `lossy_supported` | incomplete，但不写入非法 reason |
| stop reason | none | `model_context_window_exceeded` | `lossy_supported` | 转 `response.failed` + `error.code=context_length_exceeded`，Codex 客户端据此标记上下文已满并触发下一轮自动压缩 |
| stop reason | none | `refusal` | `supported` | 映射为 content_filter 并输出 refusal 事件 |
| content part | `output_text` | `text` | `supported` | 直接映射 |
| content part | `refusal` | refusal stop/details | `supported` | 已输出 |
| content part | `reasoning_text` | `thinking` | `supported` | streaming part |
| reasoning summary | `summary_text` | summarized thinking | `supported` | summarized 模式 |
| tool choice | `auto` | `auto` | `supported` | 直接映射 |
| tool choice | `required` | `any` | `supported` | 语义近似 |
| tool choice | `none` | `none` | `supported` | 直接映射 |
