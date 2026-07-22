# Protocol Coverage Matrix

日期: 2026-07-22（Chat 全面透传 + 出站 token logprobs；structured / usage details 已支持）

本文是 OpenAI Responses API 到 Anthropic Messages API 的覆盖矩阵。它记录协议项是否被语义级翻译、损耗翻译、raw 保真、后端无等价能力或延期专项设计。后续任何协议补齐都必须同步更新本文。

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

本网关面向 **Codex CLI → Anthropic 兼容后端**，不做 OpenAI 全量 Responses 平台：

- 客户端**自带完整 `input`** 回灌，网关不做 session store；`previous_response_id` 非空时 WARN + 忽略。
- Responses ↔ Anthropic Messages **直转**，不走 Chat Completions 中枢，避免损失 Codex 专有形态。

### OpenAI Chat Completions 上游（`backend_type: c`）

客户端仍只走 `/v1/responses`。当 source 配置 `backend_type: c` 时，网关经 `chatconvert` → Chat Completions 流式上游 → `chatstreamconv` 回写 Responses SSE。

详细字段级状态见本文专节 **「Chat 后端覆盖矩阵（backend_type: c）」**。摘要：

- **已支持（A+B+透传收口）**：文本多轮、工具环、采样、`text.format`/`function.strict`/`text.verbosity`/`service_tier`/`safety_identifier`/`metadata`/`store`/`moderation`/`reasoning.effort`→`reasoning_effort`/`top_logprobs`（含出站 logprobs）、`stream_options.include_obfuscation`、`prompt_cache_key/options`、usage（含 details）、`finish_reason` 终态。
- **明确降级**：reasoning **出站 item/thinking 回灌** / 多模态 image/file；hosted 为 **function 化有损**；file_search/computer/image_generation 历史 **WARN 跳过**；compaction system marker。
- **与 a 路径关系**：Anthropic 源仍是 Responses↔Messages **直转**；Chat 是并行 Backend，不经 Chat 中枢转 Anthropic。

- Anthropic 无等价能力的字段：明确错误 / WARN + 丢弃 / echo-only，禁止把整段 JSON 灌进 system。

## 架构基础（与 AGENTS.md 对齐）

**形状透传，结果归上游。** 网关是协议转换层，不代上游拒绝能力、不编造 failed 终态。仅「协议不可映射」才允许转换错误或矩阵登记的丢弃。详见仓库根目录 `AGENTS.md`「协议转换职责边界」。

## 收口策略（产品 + 技术）

本节是协议映射的**硬边界**。后续 PR 若扩大边界，必须先改本节再改代码。

### 1. 产品范围内（做）

- Codex CLI → Anthropic 兼容后端的 **Responses ↔ Messages 直转**。
- 客户端自带完整 `input` 回灌；网关无 session store。
- 可语义映射的 tool / content / SSE 生命周期；有损处登记 `lossy_supported` 并说明损失。
- 网关自主 Anthropic `cache_control`（配置 TTL），不依赖 OpenAI prompt cache key。MCP `mcp_toolset` 在 inject 后重定位 tools 末项断点；`cache.ttl=1h` 时带 `extended-cache-ttl-2025-04-11` beta。

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
| Anthropic code_execution 无 container / 生成文件 URL / image 输出字段 | 丢弃 + WARN；logs 文本尽量保留 |
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
3. code_interpreter：image 丢弃 + logs 占位；声明侧 container **WARN + 丢弃**。
4. MCP：string allowlist 已支持；filter / 审批 / 非 Bearer headers 保持降级（硬限制）。

### 7. 收口内不再打磨（硬限制 / 协议天花板）

| 项 | 原因 |
|---|---|
| `search_context_size` 真映射 | Anthropic web_search 无字段 |
| code container / 生成文件 URL / image 真映射 | Anthropic code_execution 无等价 |
| MCP 审批协议 / filter AST / 任意 headers | 后端无能力或成本/边界外 |
| custom `format` grammar 完整保留 | Anthropic custom tool 无 OpenAI grammar 等价 |
| structured output 非 tool 模拟 | 无原生 json_schema 强制 |
| reasoning.effort 精确 token | OpenAI effort 非 budget；已改用 `output_config.effort` 语义映射，模型自行决定深度 |
| system/developer 中的 image | Anthropic system 仅文本 |
| 出站专用 `shell_call`/`apply_patch_call` item type | Codex 消费 `custom_tool_call` 已验证 |
| SSE citation 非 web 类 → file_citation | OpenAI 无更细等价 |


### 8. 为何 `code_interpreter` image 输出无法真映射

本条是收口内**硬限制**的详细依据，不是实现遗漏。

#### 协议形状不对齐

| 侧 | 形态 |
|---|---|
| OpenAI `code_interpreter` 输出 | union：`logs`（文本）与 **`image`（必填 `url` URI）** |
| Anthropic `code_execution` 结果 | `stdout` / `stderr` / `return_code`，可选 `content[]` 的 **`file_id`**；**没有** `type=image` + `url` 输出项 |

OpenAI 把「代码跑出的图」定义为**可渲染的 image output 项**；Anthropic 标准 code_execution 结果是**文本（+ 文件句柄）**，不是同构的 image URL 项。

#### 双向都缺关键能力

**入站（历史回灌 OpenAI → Anthropic）**

- 历史可能含 `outputs: [{type:image, url:...}]`。
- Anthropic `code_execution_tool_result` **无 image 槽位**，无法语义级写入。
- 把 URL 塞进 stdout 只是字符串，不是 image 输出。
- 下载 URL 再当 `image` content 塞进其它消息：网关变成文件代理，越界且仍对不齐 code_execution result 形态。

**出站（Anthropic → OpenAI SSE）**

- 上游若产出文件，多为 **`file_id`**，不是 OpenAI 的 `outputs[].image.url`。
- 要变成真 image 项，需：Files 凭据拉取 → 可访问 URL/data URI → 填 `outputs[{type:image,url}]`。
- 本网关**纯协议转换**，不做 Anthropic/OpenAI Files 托管与拉文件，无法凭空生成客户端可用 URL。

当前实现：入站 image **丢弃 + WARN + logs 占位**（`image output omitted`）；出站仅 `outputs[{type:logs}]`，对生成 `file_id` **WARN + 丢弃**。

#### 与「shell env 折进 input」的差别

| | shell 元数据 | code_interpreter image |
|---|---|---|
| 目标 | `tool_use.input` 任意 JSON 线索 | OpenAI **专用** `image` output 类型 |
| 对端槽位 | 有 | Anthropic result **无** image 槽 |
| 是否要托管资源 | 否 | 要（URL / 文件拉取） |

因此 env/limits/caller 可打磨；真·image **缺协议槽位 + 缺 Files 基础设施**，收口内不做。

#### 若未来要做（须先扩产品边界）

1. 配置上游 Files 读权限；出站 `file_id` → 下载 → 签名 URL 或 data URI → `outputs[].image`。
2. 入站历史 image：下载/缓存后以明确 lossy 策略回灌（仍可能非 code_execution 标准形态）。
3. 处理过期 URL、体积、隐私与失败路径。

在边界未扩大前，矩阵状态保持 `lossy_supported`，行为保持丢弃 + 可观测，**禁止**编造假 image URL 宣称语义支持。

## 变更记录

### 2026-07-22

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
  - `mcp_call` 历史：`param.Override` 注入 beta `mcp_tool_use` / `mcp_tool_result`，client 层同步 `anthropic-beta: mcp-client-2025-11-20`。
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
- 网关级指令注入从 `system_suffix` 改为 `base_instructions_file`（经 `/v1/models` 由 Codex 客户端注入，prompt cache 更友好）。

## Chat 后端覆盖矩阵（backend_type: c）

日期: 2026-07-22

本节只描述 **Responses → Chat Completions → Responses SSE** 路径（`backend_type: c`）。Anthropic 直转见上文各表；两路径**不共享**字段状态。

### 状态约定

沿用全局状态定义。Chat 路径额外：

- freeform 工具（`shell` / `apply_patch` / `custom`）在 Chat 侧声明为 `type=function` + `parameters={input:string}`；出站回程按 freeform 名映射为 Responses `custom_tool_call`（`tool_search` 为 `tool_search_call`）。
- 连续多条 Responses `function_call` / freeform call **必须**合并为一条 Chat `assistant` 消息的 `tool_calls[]`，否则多数 Chat 兼容上游 400。

### 产品边界（c）

| 做 | 不做 |
|---|---|
| Codex 文本 + 客户端 function / freeform agent 工具环 | 对外 `/v1/chat/completions` |
| 流式 SSE only（固定 `stream:true` + `include_usage`） | 非流式 Chat 完成体 |
| 与 a 源混排 failover / 熔断 | hosted **真实** server 执行（Chat 仅 shape） |
| `finish_reason` 终态对齐（stop/tool_calls/length/content_filter） | reasoning / thinking 与 Chat `reasoning_content` 完整等价 |
| | structured output / 多模态 image / OpenAI Files |

### Request 参数（c）

| Responses 参数 | Chat 映射 | 状态 | 说明 |
|---|---|---|---|
| `model` | `model` | `supported` | 经 source ModelMap / DefaultModel |
| `input` string | user message | `supported` | |
| `input` item list | `messages` | `lossy_supported` | 见下表；无 Chat 等价 item DEBUG/WARN 跳过 |
| `instructions` | system message（首条） | `supported` | 与 developer/system item 可合并 |
| `max_output_tokens` | `max_tokens` + `max_completion_tokens` | `supported` | 双写兼容旧上游与新模型 |
| `temperature` / `top_p` | 同名 | `supported` | |
| `parallel_tool_calls` | `parallel_tool_calls` | `supported` | 直接透传 |
| `tools` | `tools` | `lossy_supported` | function + freeform + hosted function 化（web_search/code_interpreter/mcp__*）；file_search/computer/image_generation 跳过 |
| `tool_choice` | `tool_choice` | `lossy_supported` | mode + function/custom/shell/apply_patch 名；**allowed_tools 精确过滤** tools 列表 + mode；hosted choice 忽略 |
| `stream` | 固定 `true` | `supported` | 客户端 stream 与否不影响上游 |
| `stream_options` | `include_usage: true` | `supported` | 网关强制打开 usage 末包 |
| `reasoning.*` | none | `unsupported_by_backend` | 跳过；不映射 Chat thinking 扩展 |
| `text.format` structured | `response_format` | `supported` | `json_schema`/`json_object`/`text` 原生透传 Chat；含 `strict`；不做 a 路径 synthetic tool |
| `text.verbosity` | `verbosity` | `supported` | low/medium/high 透传；a 路径仍忽略 |
| `service_tier` | `service_tier` | `supported` | Chat 官方字段透传；a 路径仍忽略 |
| `safety_identifier` | `safety_identifier` | `supported` | Chat 透传；a 路径忽略 |
| `metadata` | `metadata` | `supported` | Chat 整表透传；a 路径仅 user_id + echo |
| `store` | `store` | `supported` | Chat 透传；响应 echo 仍保留 |
| `moderation` | `moderation` | `supported` | model + policy modes 同形透传；a 路径忽略 |
| `reasoning.effort` | `reasoning_effort` | `supported` | 仅 effort；summary/context 无 Chat 顶层等价 |
| `top_logprobs` | `logprobs=true` + `top_logprobs` | `supported` | Chat 透传；a 路径忽略；出站见 SSE 表 |
| `stream_options.include_obfuscation` | 同名 | `supported` | 强制 `include_usage=true` 并透传 obfuscation；a 路径忽略 |
| `previous_response_id` 等平台字段 | none | `unsupported_by_backend` | 与 a 路径同产品边界 |
| `prompt_cache_key` | `prompt_cache_key` | `supported` | Chat 官方字段透传 |
| `prompt_cache_options` | `prompt_cache_options` | `supported` | mode/ttl 透传；content `prompt_cache_breakpoint` 不支持 |
| `prompt_cache_retention` | none | `unsupported_by_backend` | deprecated，不映射 |
| `user`（deprecated） | none | `dropped` | 与 a 一致不映射；请用 safety_identifier / metadata.user_id |

### Input Item（c）

| Responses item | Chat 映射 | 状态 | 说明 |
|---|---|---|---|
| `message` / EasyInputMessage | role + content 文本 | `lossy_supported` | `developer`→`system`；保留 `system`/`user`/`assistant`；content 取 text / input_text / **output_text**（经 Decode 归一） |
| `input_message` / `output_message` | 同 message 文本 | `supported` | 防御分支；SDK 实测几乎总落到 EasyInputMessage |
| `function_call` | assistant `tool_calls[]` | `supported` | **相邻 call 合并**到同一 assistant |
| `function_call_output` | role=tool | `supported` | content 数组拼文本；图片丢弃 |
| `custom_tool_call` | assistant tool_calls name 原样 | `supported` | arguments=`{"input":...}` freeform；相邻合并 |
| `custom_tool_call_output` | role=tool | `supported` | |
| `shell_call` / `local_shell_call` | tool_calls name=`shell` | `lossy_supported` | 命令折 `input`；env/limits 不进 Chat schema |
| `shell_call_output` / `local_shell_call_output` | role=tool | `lossy_supported` | status/stdout 折文本 |
| `apply_patch_call` | tool_calls name=`apply_patch` | `lossy_supported` | V4A 文本进 freeform `input` |
| `apply_patch_call_output` | role=tool | `lossy_supported` | |
| `tool_search_call` | tool_calls name=`tool_search` | `supported` | arguments 原样/对象序列化 |
| `tool_search_output` | 动态 tools + tool 消息 | `lossy_supported` | 注入 function 声明；result 文本为工具名列表 |
| `reasoning` | none | `dropped` | DEBUG 跳过（Chat 无加密 thinking 回灌） |
| `web_search_call` 历史 | assistant tool_calls + tool 文本 | `lossy_supported` | query/sources 折文本 |
| `code_interpreter_call` 历史 | tool_calls(code) + tool(logs) | `lossy_supported` | image 丢弃 |
| `mcp_call` 历史 | `mcp__server__tool` + tool result | `lossy_supported` | 无审批 |
| computer / file_search / image_generation / program / item_reference 历史 | none | `dropped` | **WARN** 跳过（`itemType` 显式识别，禁止静默 unknown） |
| `compaction` / `compaction_trigger` | system marker | `raw_preserved` | 对齐 a 路径：`<compaction>` / `<compaction_trigger />` 文本 |

### Tool 声明（c）

| Responses tool | Chat tools[] | 状态 | 说明 |
|---|---|---|---|
| `function` | function | `supported` | name/description/parameters/**strict** |
| `custom` | function + freeform parameters | `lossy_supported` | name **不加** `_custom` 后缀；grammar 丢失 |
| `shell` / `local_shell` | function name=`shell` freeform | `lossy_supported` | |
| `apply_patch` | function name=`apply_patch` freeform | `lossy_supported` | description 强调 V4A |
| `tool_search` | function name=`tool_search` | `supported` | |
| `namespace` | 展平 `ns__name` function | `lossy_supported` | 仅 function/custom 子项 |
| `web_search` / `web_search_preview` | function `web_search` | `lossy_supported` | 无 server 搜索 |
| `code_interpreter` | function `code_interpreter` | `lossy_supported` | 无 sandbox；container 丢弃 |
| `mcp` | `mcp__{server}__{tool}`（allowed_tools 列表） | `lossy_supported` | 无连接/审批；filter 不展开 |
| file_search / computer / image_generation / programmatic | none | `unsupported_by_backend` | 声明跳过 |

### 出站 SSE（c → Responses）

| Chat 流 | Responses 事件 | 状态 | 说明 |
|---|---|---|---|
| 首 chunk | `response.created` / `in_progress` | `supported` | |
| `delta.content` | message + `output_text.delta` | `supported` | string；兼容 content part 数组（取 text） |
| `choices[].logprobs.content` | `output_text.delta/done.logprobs` | `supported` | 需请求 `top_logprobs` 且上游返回；无 bytes 字段；`include=message.output_text.logprobs` 在 Chat 源不再 WARN |
| `delta.tool_calls` function | `function_call` 链 + arguments delta/done | `supported` | 按 index 累积；**name 到齐再 open**（兼容先 id 后 name） |
| `delta.tool_calls` freeform 名（shell/apply_patch/custom） | `custom_tool_call` + input delta/done | `supported` | 同上；`SanitizeClientToolInput` 解包/归一 |
| `delta.tool_calls` name=`tool_search` | `tool_search_call` | `supported` | arguments 随 item done |
| `delta.tool_calls` name=`web_search` | `web_search_call` 链 | `lossy_supported` | 无真实 sources |
| `delta.tool_calls` name=`code_interpreter` | `code_interpreter_call` 链 | `lossy_supported` | code 从 arguments 解；无 logs |
| `delta.tool_calls` name=`mcp__*__*` | `mcp_call` 链 | `lossy_supported` | 无 server result |
| `finish_reason=stop` / `tool_calls` | `response.completed` | `supported` | |
| `finish_reason=length` | `response.incomplete` reason=`max_output_tokens` | `supported` | |
| `finish_reason=content_filter` | `response.incomplete` + refusal 事件链 | `supported` | 累积 `delta.refusal`，缺省用 fallback 文案；清掉半截 text/tool output |
| usage 末包（空 choices） | 填 `usage` | `supported` | totals + `input_tokens_details.cached_tokens` + `output_tokens_details.reasoning_tokens`；并写 `cache_read_input_tokens` 兼容 a 路径字段名 |
| `[DONE]` 且无 finish_reason | 补 completed/incomplete | `supported` | FeedDone |
| 流中断 | `response.failed` | `supported` | Backend Fail |

### 已知缺口（c，产品边界外 / 硬限制）

| 项 | 说明 |
|---|---|
| 多模态 image/file 输入 | content 仅文本（image/file part DEBUG 跳过） |
| 厂商 `reasoning_content` | 忽略，不映射 Responses reasoning |
| hosted tools **真实** server 执行 | Chat 仅 function 形状；出站 completed 无真实 sources/logs |
| Chat 原生 `tools[].type=custom` + grammar | freeform 统一 function 化以兼容通用上游；grammar 丢失 |
| 出站 logprobs `bytes` 字段 | Chat TokenLogprob.bytes 不映射到 Responses（官方 delta logprobs 无 bytes） |
| Responses-only：`max_tool_calls` / `background` / `conversation` / `context_management` / `prompt` 模板 | 无 Chat 等价或产品边界外 |
| Chat-only：`frequency_penalty` / `presence_penalty` / `seed` / `stop` / `n` / `logit_bias` / `prediction` / `audio`/`modalities` / `web_search_options` | Responses 请求无对应顶层字段，无法从客户端映射 |
| orphan tool 配对 | **已补**：缺 output 时 WARN + 占位 `role=tool` |

### 收口内已打磨（2026-07-22）

| 项 | 行为 |
|---|---|
| computer/file_search/image_generation 等历史 | 显式 WARN + drop（非 unknown 静默） |
| compaction 历史 | system marker 回灌 |
| tool_calls 分片 name 晚到 | 有 name 再 `output_item.added`，避免误判 function |
| `delta.content` 数组 | 解析 text part，不整段 Feed 失败 |
| `developer` role | 压成 `system`（兼容 OpenCode 等对 developer 400 的上游） |
| `tool_choice.allowed_tools` | 精确过滤 tools + mode（auto/required） |
| `prompt_cache_key` / `prompt_cache_options` | 透传到 Chat body（retention 仍忽略） |
| structured `text.format` | → Chat `response_format`（含 strict） |
| `function.strict` | → Chat `tools[].function.strict` |
| `text.verbosity` / `service_tier` | → Chat 同名字段透传 |
| `safety_identifier` / `metadata` / `store` / `moderation` | → Chat 同形透传 |
| `reasoning.effort` | → Chat `reasoning_effort` |
| `top_logprobs` | → Chat `logprobs` + `top_logprobs` |
| `stream_options.include_obfuscation` | 透传；始终 `include_usage` |
| usage details | `cached_tokens` + `reasoning_tokens` 出站明细 + `cache_read_input_tokens` 兼容字段 |
| 出站 token logprobs | Chat `choices[].logprobs` → `response.output_text.delta/done.logprobs` + content part |
| OfInputMessage / OfOutputMessage | chatconvert 防御分支（SDK 极少落点） |
| `developer`→`system` | OpenCode/Console 等兼容上游对 developer 400 |

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
| `max_output_tokens` | `max_tokens` | `supported` | 未设置时默认 4096 |
| `temperature` | `temperature` | `supported` | 直接映射 |
| `top_p` | `top_p` | `supported` | 直接映射 |
| `parallel_tool_calls` | `disable_parallel_tool_use` 反向映射 | `supported` | `false` 时禁用 Anthropic 并行 tool use |
| `reasoning.effort` | `output_config.effort` + `thinking` | `lossy_supported` | `none`→thinking disabled；`low`/`medium`/`high`/`xhigh`/`max`→同名 Anthropic `output_config.effort`（覆盖官方全部五档）；未知档仅开 thinking 不伪造 effort。兼容后端对不支持的值可静默降级 |
| `reasoning.summary` | thinking display / summary events | `lossy_supported` | `concise` 映射到 summarized 输出 |
| `reasoning.generate_summary` | thinking display | `unsupported_by_backend` | deprecated，被 `reasoning.summary` 取代；非空时 **WARN + 忽略**，不复用 `summary` 路径 |
| `metadata` | response echo + Anthropic `metadata.user_id` | `lossy_supported` | `metadata.user_id` 透传到 Anthropic `metadata.user_id`；其余键值对无 Anthropic 等价能力，仅响应 echo 回显。未透传的键值对触发 WARN |
| `text.format.json_schema` | forced Anthropic tool | `lossy_supported` | 通过工具调用模拟 structured output；与所有不等价的显式 `tool_choice` 组合均明确转换失败 |
| `text.format.json_object` | forced `json_object` tool | `lossy_supported` | 通过工具调用模拟；与所有不等价的显式 `tool_choice` 组合均明确转换失败 |
| `text.verbosity` | none | `unsupported_by_backend` | a 忽略；Chat 见 c 专节 |
| `tools` | `tools` | `lossy_supported` | 仅部分工具类型支持，详见 Tool Union |
| `tool_choice` | `tool_choice` | `lossy_supported` | 仅部分 choice 支持；具体工具选择必须精确匹配声明的 type/name，详见 Tool Choice Union |
| `previous_response_id` | none | `unsupported_by_backend` | 网关无 session store，不做 enrich；Codex 主路径不传此字段（客户端完整回灌 `input`）。若请求携带非空值则 WARN + 忽略 |
| `store` | response echo only | `raw_preserved` | 无本地会话存储/回填；仅在响应对象 echo 请求值 |
| `truncation` | response echo only | `raw_preserved` | Anthropic 无直接等价策略 |
| `include` | partial | `lossy_supported` | 已满足：`reasoning.encrypted_content`、`web_search_call.action.sources`、`code_interpreter_call.outputs`、`message.input_image.image_url`；`message.output_text.logprobs` 仅 Chat 源 satisfied；其余（file_search/computer 等）WARN + 忽略 |
| `prompt_cache_key` | none | `unsupported_by_backend` | Anthropic 用内容 hash 缓存(cache_control)，不认客户端 key；网关已自主设 cache_control；非空时 **DEBUG + 忽略**（Codex 常发，可控协议差异） |
| `prompt_cache_options` | none | `unsupported_by_backend` | 网关已自主在 system/tools/顶层设 cache_control（TTL 可配；MCP toolset inject 后重定位 tools 末项断点；`1h` 带 `extended-cache-ttl-2025-04-11`），OpenAI options 结构对 Anthropic 无意义；mode/ttl 非空时 **DEBUG + 忽略** |
| `prompt_cache_retention` | none | `unsupported_by_backend` | deprecated（in_memory/24h），与 Anthropic cache_control 语义不同；非空时 **DEBUG + 忽略**（不映射 TTL） |
| `prompt` | none | `unsupported_by_backend` | 引用 prompt template 与变量，需服务端模板存储与解析；网关无 OpenAI prompt 存储能力；`prompt.id` 非空时 **WARN + 忽略** |
| `background` | none | `unsupported_by_backend` | 当前网关只支持同步 SSE |
| `conversation` | none | `unsupported_by_backend` | 本地 store 不是 OpenAI Conversation API |
| `context_management` | none | `unsupported_by_backend` | 请求级上下文管理（含 compaction）；OpenAI 服务端压缩，网关不做；非空时 **WARN + 忽略**。历史 `compaction` item 仍见 Input Item（`raw_preserved` marker） |
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
| `web_search_call`（input 历史） | `server_tool_use` + 空 result + sources 文本 | `lossy_supported` | query→input；无 encrypted 时 result content 空；URL 折可见文本；open_page/find 折 query。出站 stream 见 Output/SSE |
| `function_call` | assistant `tool_use` | `supported` | `arguments` 转 tool input |
| `function_call_output` | user `tool_result` | `supported` | `output` string 或 content 数组（`input_text`/`input_image`/`input_file`）→ tool_result 多 part；仅 `file_id` 无法拉取时 WARN + 丢弃 |
| `tool_search_call` | assistant `tool_use` name=`tool_search` | `supported` | 已有语义分支 |
| `tool_search_output` | dynamic tools + tool_result | `supported` | 工具注入并记录 developer marker |
| `additional_tools` | none | `unsupported_by_backend` | Responses Lite 产物；网关统一 `use_responses_lite=false`，该 item 不会出现，移除转换分支 |
| `reasoning` | `thinking` / `redacted_thinking` | `supported` | summary 优先，空则回退 content[].reasoning_text；有 encrypted 无文本→redacted；无 encrypted 丢弃 |
| `compaction` | system marker | `raw_preserved` | Anthropic 无 OpenAI compaction item |
| `image_generation_call` | none | `dropped` | 历史回灌 WARN + 丢弃；工具声明 fail-fast |
| `code_interpreter_call` | Anthropic code execution tool | `lossy_supported` | 映射为 `code_execution_20250522` tool use/result；`container` / 生成文件 `file_id`→`url` 不可转换；**image 输出无法真映射**（见收口策略 §8），丢弃 + WARN，logs 可含 `image output omitted` 占位 |
| `local_shell_call` | assistant `tool_use` name=`shell` | `lossy_supported` | 命令文本 + `env`/`working_directory`/`timeout_ms`/`user`/`status` 折入 tool_use.input；无 Anthropic 原生 shell 协议 |
| `local_shell_call_output` | user `tool_result` | `lossy_supported` | 文本 tool_result；可前缀 `[status=…]`；item.id 作 tool_use_id |
| `shell_call` | assistant `tool_use` name=`shell` | `lossy_supported` | 命令文本 + `environment_type` + `timeout_ms`/`max_output_length`/`status`/`caller_type`/`caller_id` 折入 tool_use.input；无 Anthropic 原生 shell 协议 |
| `shell_call_output` | user `tool_result` | `lossy_supported` | stdout/stderr + `[status]`/`[max_output_length]`/`[exit_code]`/`[timeout]` 折文本；caller 不映射 |
| `apply_patch_call` | assistant freeform `tool_use` name=`apply_patch` | `lossy_supported` | create/update/delete → V4A 文本（`*** Begin/End Patch`）；`status`/`caller` 无 Anthropic 字段（丢失） |
| `apply_patch_call_output` | user `tool_result` | `lossy_supported` | `[status=…]` + 可选日志文本；caller 不映射 |
| `mcp_list_tools` | developer marker | `lossy_supported` | 无 Anthropic 列表块；折成 `<mcp_list_tools>` developer 文本（server + tool names + error） |
| `mcp_approval_request` | none | `dropped` | Anthropic 无审批协议；网关不实现，历史回灌 WARN + 丢弃 |
| `mcp_approval_response` | none | `dropped` | Anthropic 无审批协议；网关不实现，历史回灌 WARN + 丢弃 |
| `mcp_call` | beta `mcp_tool_use` + `mcp_tool_result` | `lossy_supported` | param.Override 注入 messages；需 beta header；error→is_error |
| `custom_tool_call` | assistant custom `tool_use` | `supported` | freeform custom tool 支持 |
| `custom_tool_call_output` | user `tool_result` | `supported` | `output` string 或 content list → tool_result 多 part；仅 `file_id` 无法拉取时 WARN + 丢弃 |
| `compaction_trigger` | system marker | `raw_preserved` | Anthropic 无等价事件 |
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
| orphan `tool_use` 兜底 | input 历史含 `function_call`/`custom_tool_call`/`shell`/`apply_patch`/`tool_search_call` 但缺对应 `*_output`（中断后 resume / failover 丢历史 / 客户端 bug） | 补 `is_error=true` 占位 `tool_result` | 避免上游 Anthropic 以 `tool_use without tool_result` 400 拒绝整请求。占位补在该 tool_use 后的首个 user message 前部；无后续 user message 则新建。`server_tool_use`（`code_interpreter`，item 内自合成 result）不受影响。实现见 `internal/convert/request.go` `ensureToolUsePaired`；WARN 暴露该客户端异常 |

## Tool Union

| OpenAI tool | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `function` | client tool | `supported` | JSON schema 转 `input_schema` |
| `file_search` | none | `unsupported_by_backend` | 无 OpenAI vector store 后端；请求时返回明确转换错误 |
| `computer` | none | `unsupported_by_backend` | 需 computer use 执行环境；请求时返回明确转换错误 |
| `computer_use_preview` | none | `unsupported_by_backend` | 同上；请求时返回明确转换错误 |
| `web_search` | Anthropic web search server tool (20250305) | `lossy_supported` | `filters.allowed_domains` → `allowed_domains`；`user_location` → `user_location`；`search_context_size` 无 Anthropic 字段 → WARN + 忽略 |
| `web_search_preview` | Anthropic web search server tool (20250305) | `lossy_supported` | 同 web_search：`user_location` 映射；`search_context_size` WARN + 忽略；preview 无 domains filter |
| `mcp` | beta mcp_servers + mcp_toolset (mcp-client-2025-11-20) | `lossy_supported` | `allowed_tools: string[]` → toolset allowlist（已实现）；`allowed_tools: filter` → WARN + 全启用；`require_approval≠never` 降级 never + WARN；headers 仅 Bearer → `authorization_token`；`connector_id`/`tunnel_id` fail-fast；需 beta `mcp-client-2025-11-20` |
| `code_interpreter` | Anthropic code execution tool (20250522) | `lossy_supported` | 声明 `code_execution_20250522`；`container`（id/auto file_ids/memory）**WARN + 丢弃**（Anthropic 无 container） |
| `programmatic_tool_calling` | none | `unsupported_by_backend` | 无等价能力；请求时返回明确转换错误 |
| `image_generation` | none | `unsupported_by_backend` | Anthropic Messages 不生成 OpenAI image result；请求时返回明确转换错误 |
| `local_shell` | client custom tool `shell` | `lossy_supported` | 声明 freeform `shell`；历史元数据见 Input Item（env/cwd/timeout/user/status） |
| `shell` | client custom tool `shell` | `lossy_supported` | 声明 freeform `shell`；历史 limits/caller/environment_type 折入 input（见 Input Item）；skills 细节仍 lossy |
| `custom` | Anthropic custom tool | `lossy_supported` | `format` grammar/text 语义未完整保留 |
| `namespace` | flattened tool names | `lossy_supported` | namespace 被拼入 tool name；子工具仅支持 `function` / `custom`，其他类型明确转换失败 |
| `tool_search` | client tool `tool_search` | `supported` | 当前按普通 tool 暴露 |
| `apply_patch` | client custom tool `apply_patch` | `supported` | freeform input 支持 |

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
| `allowed_tools` | filtered tool set + choice mode | `lossy_supported` | 仅支持 `auto`/`required`；每个 allowed 条目按 `type`、namespace、name 与已声明工具精确匹配，未知 mode、转换名冲突和 hosted/MCP 条目明确报错；不能与 structured output 同时使用 |
| structured output + explicit incompatible choice | none | `unsupported_by_backend` | synthetic structured-output tool 已被强制时，`none`、`auto`、`required`、其他 specific choice、`allowed_tools` 和未知 choice 均无等价语义，明确转换失败 |
| hosted tool choice | none | `unsupported_by_backend` | file/web/computer/code/image 等内置工具不能安全模拟；请求时返回明确转换错误 |
| `mcp` | none | `unsupported_by_backend` | 无等价 MCP choice；请求时返回明确转换错误 |
| `programmatic_tool_calling` | none | `unsupported_by_backend` | 无等价 programmatic tool choice；请求时返回明确转换错误 |

## Output Item Union

| OpenAI output item | Anthropic 来源 | 当前状态 | 说明 |
|---|---|---|---|
| `message` | text block | `supported` | 输出 text message |
| `reasoning` | thinking/redacted thinking block | `supported` | 支持 summary/signature/encrypted |
| `function_call` | `tool_use` | `supported` | 回程 arguments 把整数值 `N.0` 收成整数，避免 Codex serde 失败 | 普通 tool use |
| `function_call_output` | request replay only | `supported` | 作为 input item 回放（含 content 数组形态） |
| `custom_tool_call` | custom `tool_use` | `supported` | freeform input 解包；`apply_patch` 额外归一 V4A 标记（去多余 `***`） |
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
| `code_interpreter_call` | Anthropic code execution | `lossy_supported` | server_tool_use + code_execution_tool_result → 事件链；outputs 仅 logs；`file_id`/image 真映射不可（§8）；stderr/return_code 并入 logs |
| `local_shell_call` | `custom_tool_call` name=`shell` | `lossy_supported` | 出站以 `custom_tool_call` 形态发出（Codex 实测可消费）；不生成专用 `local_shell_call` item type |
| `local_shell_call_output` | request replay only | `supported` | 不作为 output item 生成；入站历史转 `tool_result` 见 Input Item |
| `shell_call` | `custom_tool_call` name=`shell` | `lossy_supported` | 出站以 `custom_tool_call` 形态发出（Codex 实测可消费）；不生成专用 `shell_call` item type |
| `shell_call_output` | request replay only | `supported` | 不作为 output item 生成；入站历史转 `tool_result` 见 Input Item |
| `apply_patch_call` | `custom_tool_call` name=`apply_patch` | `lossy_supported` | 出站以 `custom_tool_call` 形态发出（Codex 实测可消费）；不生成专用 `apply_patch_call` item type |
| `apply_patch_call_output` | request replay only | `supported` | 不作为 output item 生成；入站历史转 `tool_result` 见 Input Item |
| `mcp_call` | Anthropic beta MCP `mcp_tool_use` + `mcp_tool_result` | `lossy_supported` | server_tool_use(mcp_tool_use) + mcp_tool_result 映射为 mcp_call 事件链；error 变体并入 failed（is_error） |
| `mcp_list_tools` | none | `unsupported_by_backend` | 出站不生成；历史见 Input Item（developer marker） |
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
| `response.code_interpreter_call_code.delta` | Anthropic code execution | `lossy_supported` | code_execution server_tool_use 的 input_json_delta 映射 |
| `response.code_interpreter_call_code.done` | Anthropic code execution | `lossy_supported` | server_tool_use block stop 映射 |
| `response.code_interpreter_call.in_progress` | Anthropic code execution | `lossy_supported` | server_tool_use(code_execution) 触发 |
| `response.code_interpreter_call.interpreting` | Anthropic code execution | `lossy_supported` | input_json_delta 结束后触发 |
| `response.code_interpreter_call.completed` | Anthropic code execution | `lossy_supported` | `code_execution_tool_result` 触发；`code_execution_tool_result_error` 无对应 completed 语义，不映射 |
| `response.image_generation_call.in_progress` | none | `unsupported_by_backend` | 无等价 |
| `response.image_generation_call.generating` | none | `unsupported_by_backend` | 无等价 |
| `response.image_generation_call.partial_image` | none | `unsupported_by_backend` | 无等价 |
| `response.image_generation_call.completed` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_call_arguments.delta` | Anthropic beta MCP `input_json_delta` | `lossy_supported` | mcp_tool_use 的 input_json_delta 映射为 arguments.delta |
| `response.mcp_call_arguments.done` | Anthropic beta MCP `tool_use` block stop | `lossy_supported` | server_tool_use stop 映射为 arguments.done |
| `response.mcp_call.in_progress` | Anthropic beta MCP `server_tool_use` | `lossy_supported` | server_tool_use(mcp_tool_use) 触发 |
| `response.mcp_call.completed` | Anthropic beta MCP `mcp_tool_result` | `lossy_supported` | mcp_tool_result 触发；output 为结果文本 |
| `response.mcp_call.failed` | Anthropic beta MCP `mcp_tool_result` (is_error) | `lossy_supported` | mcp_tool_result 的 is_error 变体映射为 failed |
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
| request `redacted_thinking` | reasoning encrypted | `supported` | 用于回放 redacted thinking |
| request `tool_use` | function/custom/tool call | `supported` | 常用工具路径 |
| request `tool_result` | tool call output | `supported` | 常用工具结果 |
| stream `text` | output message | `supported` | 已输出 output_text |
| stream `thinking` | reasoning | `supported` | 已输出 reasoning events |
| stream `redacted_thinking` | reasoning encrypted | `supported` | 已存 encrypted_content |
| stream `tool_use` | function/custom tool call | `supported` | 已输出 tool call events |
| stream `server_tool_use` | built-in tool call | `supported` | name=web_search 映射为 web_search_call（code_execution 见 catalog）；未登记的 server tool（如 web_fetch）及对应 result：**WARN + skip，不中断流**（非 response.failed） |
| stream `web_search_tool_result` | web search call result | `supported` | 完成 web_search_call（completed + output_item.done） |
| stream `web_fetch_tool_result` | web fetch result | `unsupported_by_backend` | OpenAI Responses 无直接等价 |
| stream `code_execution_tool_result` | code interpreter output | `supported` | 映射为 code_interpreter_call completed + outputs(logs)；`file_id` 丢弃 + WARN；`code_execution_tool_result_error` 无法转 completed |
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
| stop reason | none | `refusal` | `supported` | 映射为 content_filter 并输出 refusal 事件 |
| content part | `output_text` | `text` | `supported` | 直接映射 |
| content part | `refusal` | refusal stop/details | `supported` | 已输出 |
| content part | `reasoning_text` | `thinking` | `supported` | streaming part |
| reasoning summary | `summary_text` | summarized thinking | `supported` | summarized 模式 |
| tool choice | `auto` | `auto` | `supported` | 直接映射 |
| tool choice | `required` | `any` | `supported` | 语义近似 |
| tool choice | `none` | `none` | `supported` | 直接映射 |
