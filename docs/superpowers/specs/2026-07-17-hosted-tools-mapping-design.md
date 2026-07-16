# Hosted Tools 代理映射设计（code interpreter / web search / MCP）

日期: 2026-07-17
状态: 设计定稿，批次 0 plan 就绪（`docs/superpowers/plans/2026-07-17-tool-catalog-refactor.md`）
分支: `feat/hosted-tools`
关联: `docs/protocol-coverage.md`（hosted tools 行的专项收尾）、`docs/superpowers/specs/2026-07-16-full-protocol-coverage-design.md`

## 背景

本仓库是 `codex-api-gateway`，把 OpenAI Responses API 请求转换为 Anthropic Messages API 请求，并把 Anthropic SSE 流式转回 Responses SSE。架构是**纯协议转换**：除 hosted server tool 外，网关不执行任何工具——function / custom / shell / apply_patch 全部映射成 Anthropic `tool_use`，由 Codex 客户端自行执行并回灌 output。

`docs/protocol-coverage.md` 矩阵已把 `code_interpreter` / `code_execution` 相关项标记为 `deferred`（专项映射前请求返回明确转换错误），把 `mcp` 相关项标记为 `unsupported_by_backend`（"MCP server/tool lifecycle 不等价；请求时返回明确转换错误"）。本设计正是这些 `deferred` / `unsupported_by_backend` 项的专项收尾：按 **Anthropic 官方标准协议**给出双向映射，不针对特定服务商做能力假设。

## 目标

1. 按 Anthropic 标准，补全 **code interpreter**（→ code execution）、**MCP**（→ managed MCP connector）两类 hosted tool 的请求侧 + 回程流式双向映射。
2. **复核 web search** 现有映射的边界（已端到端 `supported`，仅查缺）。
3. 更新 `docs/protocol-coverage.md` 对应行的状态与说明。
4. 建立 **tool 处理通用架构**（catalog 驱动），让所有 tool 类型（现有 + code interpreter + MCP + 未来 hosted tool）走统一注册/派发框架，消除散落 switch。
5. 全程 TDD：每个映射先写 RED 测试（基于官方文档 wire 样本），再实现，再验证。

## 非目标

- 不在网关内自建代码沙箱或 MCP client 进程（即不做"网关托管执行"）。仅做协议映射，执行仍由 Anthropic 侧 hosted 能力承担。
- 不伪装 Anthropic 没有的能力为成功；所有信息损失在矩阵与 README 显式登记。
- 不改变已 `supported` 路径的**协议行为**：function / custom / shell / apply_patch / tool_search / namespace / web_search 在批次 0 会把实现迁移进 catalog（代码重构），但对外输入输出完全不变，由现有测试锁定；reasoning / compaction 等非 tool 路径不在 catalog 范围，保持原样。

## 可行性依据（资料 + SDK，三重交叉验证）

- **Anthropic SDK** `github.com/anthropics/anthropic-sdk-go@v1.57.0`
- **OpenAI SDK** `github.com/openai/openai-go/v3@v3.42.0`
- **Anthropic 官方文档**
  - code execution: `https://platform.claude.com/docs/en/agents-and-tools/tool-use/code-execution-tool`
  - MCP connector: `https://platform.claude.com/docs/en/agents-and-tools/mcp-connector`
- **OpenAI 官方文档**
  - code interpreter: `https://developers.openai.com/api/docs/guides/tools-code-interpreter`
  - MCP and connectors: `https://developers.openai.com/api/docs/guides/tools-connectors-mcp`

关键 SDK 事实（权威，编译期可验证）：

- OpenAI `responses.ToolUnionParam`（response.go:23534）含 `OfCodeInterpreter *ToolCodeInterpreterParam`、`OfMcp *ToolMcpParam`、`OfWebSearch`、`OfWebSearchPreview`。
- Anthropic 标准 `message.ToolUnionParam`（message.go:3616）含 `OfCodeExecutionTool20250522/20250825/20260120/20260521`、`OfWebSearchTool20250305`——code execution 是**标准（非 beta）** tool。
- Anthropic 标准 `ToolUnionParam` **没有 MCP 变体**；MCP 仅存在于 beta：`BetaMessageNewParams.MCPServers []BetaRequestMCPServerURLDefinitionParam` + `BetaToolUnionParam.OfMCPToolset`，需 beta header `mcp-client-2025-11-20`。
- Anthropic 回程 `ServerToolUseBlock.Name` 可为 `web_search | web_fetch | code_execution | bash_code_execution | text_editor_code_execution | tool_search_*`；结果块 `code_execution_tool_result → CodeExecutionResultBlock{ content []CodeExecutionOutputBlock, return_code, stderr, stdout }`。
- Anthropic 回程 MCP 是**全新 block 类型** `mcp_tool_use{id,name,server_name,input}` / `mcp_tool_result{tool_use_id,is_error,content}`，**标准 `ContentBlockStartEventContentBlockUnion` / `ContentBlock` 不承载**（无对应 `Of*` 变体）——回程需特殊解析。
- 本网关 `internal/anthropic/client.go` 是**手工 POST `/v1/messages`**（不走 SDK `Messages.New`），beta header 已有条件注入机制（`thinkingEnabled` → `interleaved-thinking-2025-05-14`，多 beta 逗号分隔）。

## 设计

### 通用 tool 处理架构（重构基础，批次 0）

**痛点**：同一 tool 类型的行为散落在 6+ 处 switch——请求侧 `appendToolUnion`、`declaredToolIdentities`、`parseAllowedToolIdentities`、`formatToolNames`、`setLastToolCacheControl`、`FreeformToolNames`；回程侧 `handleBlockStart`、`handleServerToolUseStart`、`handleWebSearchResultStart`。新增一个 tool 要同步改 5–6 处，易遗漏（protocol-coverage 反复出现的原因）。

**架构：catalog 驱动**。建立 tool 处理的单一事实来源（新增 `internal/convert/toolcatalog`，或并入 `convert`）。每种 tool 在一处登记其**完整生命周期四维度**，覆盖 SDK 已知的全部变体（OpenAI Tool Union 17 项 / Input Item 31 项 / Tool Choice 9 项 / Anthropic 回程 block 全集）：

- **身份 identity**：`openaiType`、固定名（`shell`/`apply_patch`/`tool_search`）或字段名、namespace 归属、freeform 标记——统一服务于 `tool_choice`、`allowed_tools`、命名与回程 function/custom 判定。
- **类别 kind**：`clientTool`（→ `ToolParam`，客户端执行）/ `serverTool`（→ 标准 server tool union 变体，Anthropic 执行）/ `betaServerTool`（→ beta 注入，如 MCP）/ `unsupported`（fail-fast 或 raw_preserved，按矩阵）。
- **声明映射 mapDecl**：OpenAI Tool Union 变体 → Anthropic 声明（`ToolParam` / server tool union / beta 注入 / error）。
- **回灌映射 mapReplay**：OpenAI Input Item 的 call+output 对 → Anthropic 历史 `tool_use`+`tool_result`（clientTool）或 server tool 回放（server/betaTool），服务多轮上下文。覆盖 `function/custom/shell/apply_patch/tool_search/web_search/code_interpreter/mcp` 调用与输出。
- **流式映射 mapStream**：Anthropic 回程 block（`tool_use`/`server_tool_use`/`web_search_tool_result`/`code_execution_tool_result`/`mcp_tool_use`/`mcp_tool_result`/`web_fetch_*`/`bash`·`text_editor_code_execution_*`）→ Responses call item + events + outputs。

**全变体兜底**：SDK 每个变体在 catalog 都有登记——本批不支持的 hosted tool（`file_search`/`computer`/`computer_use_preview`/`image_generation`/`programmatic_tool_calling` 及其 input item / tool_choice）统一登记为 `unsupported`，按 `docs/protocol-coverage.md` 矩阵状态走 fail-fast 或 raw_preserved，**不再有散落 default 分支**。这是"通用性"的兑现：任何协议变体都有 catalog 处所，新增支持只需升级该登记的 kind 与映射。

请求侧（声明/回灌/身份）与回程侧（流式）的 dispatch 全部改为遍历 catalog 查询，消除散落 switch。

**收益**：新增 tool（code interpreter / MCP / 未来 hosted tool）= catalog 注册一项 + 提供映射函数，零散落 switch 改动；tool 行为集中、与 `docs/protocol-coverage.md` 矩阵一一对应、可审计。

**特例处理**：namespace tool 是结构性嵌套（catalog 按子 tool 类型递归）；custom/function 名取自字段（catalog 提供 nameResolver）；MCP 的 beta 注入（`kind=betaServerTool`，`mapRequest` 产出 `betaInjection{mcp_servers, mcp_toolset}`，由 `client` 层消费注入）。

**重构约束**：批次 0 是纯重构，不改变任何现有 tool 的协议行为，全部由现有 `request_test.go` / `converter_test.go` / 集成测试保护；迁移完成后 GREEN，再做 A/B。

### web search（批次 0 统一重构 + 复核，当前 `supported`）

web_search 是当前唯一已实现的 server tool（`web_search` / `web_search_preview` → `web_search_20250305`；回程 `server_tool_use(web_search)` + `web_search_tool_result` → `web_search_call` 事件链 + `action.sources`）。**批次 0 将其请求声明（`appendWebSearchTool`）与回程流式（`handleServerToolUseStart` / `handleWebSearchResultStart` / `extractWebSearchSources` / `extractWebSearchQuery`）统一迁移进 catalog**，作为 **`serverTool` kind 的首个范例**——使 catalog 的 server tool 维度有真实迁移验证，直接为批次 A（code interpreter 同为 `serverTool`）铺路。行为不变，由 `TestWebSearch*` / `TestWebSearchServerToolUseEmitsWebSearchCall` / `TestWebSearchResultSurfacesSources` 等测试保护。

复核项（迁移同期查缺）：

- `search_context_size`（low/medium/high）：Anthropic 用 `max_uses` 控制搜索量；近似映射属 `lossy_supported` 增强，**本轮不做**，保持现状登记。
- 历史 `web_search_call` input item 回灌：web_search 是 hosted 无状态，确认迁移后 catalog 的 `mapReplay` 不对其产生副作用。
- 结论：迁移后维持 `supported`，无协议行为变化。

### 1. code interpreter → code execution（`deferred` → `lossy_supported`）

两端都是 hosted Python 沙箱，结构与 web search 同构（`server_tool_use` + 结果块，靠 `tool_use_id` 关联）。

#### 1.1 请求侧（批次 A：catalog 注册 `code_interpreter` Spec，`serverTool`）

`mapDecl`：

```
OpenAI OfCodeInterpreter{ Container: {type:auto, memory_limit, file_ids} | "cntr_xxx" }
  → Anthropic OfCodeExecutionTool20250522{ Type: code_execution_20250522, Name: code_execution }
```

- `container`（含 `file_ids` / `memory_limit` / 显式 `cntr_xxx`）**无 Anthropic 等价，丢弃**（Anthropic code execution 无状态单次执行、无 container 概念）。登记为已知损失。
- 版本选择：`code_execution_20250522`（最稳定的标准变体；SDK 同时提供 20250825/20260120/20260521，可在配置层后续扩展，本轮固定 20250522）。
- identity / tool_choice / allowed_tools 随 catalog 注册自动可用（批次 0 已统一）；`setLastToolCacheControl` 对 `OfCodeExecutionTool20250522` 变体的 cache_control 派发随 catalog 声明统一处理（与 web_search 同路径）。

#### 1.2 回程流式（`internal/streamconv/converter.go`）

当前 `code_execution` 走跳过（`handleSkippedServerToolUseStart` / `handleSkippedBlockStart`）。批次 A 在 catalog/streamconv 注册 `code_execution` 的回程 handler（复刻批次 0 为 web_search 建立的 `serverTool` 模式，按 `tool_use_id` 关联）：

| Anthropic 事件 | Responses 事件 / item |
|---|---|
| `content_block_start` type=`server_tool_use` name=`code_execution`，input={code} | `output_item.added`（`code_interpreter_call`，status=`in_progress`，`container_id`=合成占位）；`code_interpreter_call.in_progress`；`code_interpreter_call.interpreting`；从 `input.code` 发 `code_interpreter_call_code.delta` + `code_interpreter_call_code.done` |
| `content_block_start` type=`code_execution_tool_result`（`CodeExecutionResultBlock`） | outputs：`stdout` → `{type:logs, logs}`；发 `code_interpreter_call.completed` + `output_item.done` |

- `container_id`：Anthropic 无 container，回程合成稳定占位（如 `ci_container_<idx>`），登记为损失。
- `stderr` / 非零 `return_code`：并入 `logs` 输出（OpenAI `logs` 类型承载 stdout/stderr 文本）。
- `content[].file_id`（代码生成的文件）：Anthropic 用 `file_id`，OpenAI `image`/`file` 输出需 `url`。网关无 OpenAI files 凭据，无法转换 → **丢弃并 WARN**（遵循 AGENTS.md 静默跳过约定）。
- outputs 承载：OpenAI code_interpreter 仅有 5 个 streaming event（`in_progress`/`interpreting`/`code.delta`/`code.done`/`completed`），**无独立 output event**；`outputs` 随 `code_interpreter_call` item 在 `output_item.done` 时整体携带（SDK response.go:4659-4808，item `Outputs` 字段 `api:"required"`）。converter 在 `code_execution_tool_result` 到达时把 stdout 填入 item.Outputs，再发 `completed` + `output_item.done`。

#### 1.3 input 回灌（多轮）

批次 A 在 catalog `mapReplay` 注册 `code_interpreter_call`：OpenAI input 历史项转成 Anthropic 历史内容块回放——`server_tool_use(code_execution, input={code})` + `code_execution_tool_result(stdout=outputs.logs)`，使模型在多轮上下文中保留历史代码与结果。`container_id` 丢弃。

#### 1.4 衍生 server tool result（保持 `deferred`）

`bash_code_execution_tool_result` / `text_editor_code_execution_tool_result`：OpenAI 侧无对应 hosted item（shell 走 custom tool 由客户端执行、apply_patch 同理），**本轮不映射**，保持 `deferred`，维持当前显式失败 + WARN。

### 2. MCP → managed MCP connector（`unsupported_by_backend` → `lossy_supported`）

**状态重评**：矩阵原判 `unsupported_by_backend`（"lifecycle 不等价"）。调研证实 Anthropic 提供 managed MCP connector（beta），语义等价于 OpenAI hosted MCP（都是服务端代连 remote MCP server、执行 tool）。故重评为 `lossy_supported`，损耗见 2.4。

#### 2.1 client 层（批次 B：`internal/anthropic/client.go`）

请求含 MCP tool 时，`anthropic-beta` header 追加 `mcp-client-2025-11-20`（与现有 `interleaved-thinking-2025-05-14` 逗号分隔）。URL 不变（`/v1/messages`）。触发条件由 catalog 声明（请求含 `betaServerTool` kind 的 tool）。

#### 2.2 请求侧（批次 B：catalog 注册 `mcp` Spec，`betaServerTool`）

```
OpenAI OfMcp{ server_label, server_url|connector_id|tunnel_id, authorization,
              allowed_tools, headers, require_approval, server_description }
  → Anthropic（beta 结构）:
      顶层 mcp_servers[]: { type:"url", url:server_url, name:server_label,
                            authorization_token:authorization }
      tools[] 里 mcp_toolset: { type:"mcp_toolset", mcp_server_name:server_label,
                                default_config/ configs(allowlist) }
```

- 字段映射：`server_label`→`name`，`server_url`→`url`，`authorization`→`authorization_token`，`allowed_tools`→`mcp_toolset` allowlist 模式（`default_config.enabled=false` + 命中项 `configs[name].enabled=true`）。
- `headers`：Anthropic server 定义只有单一 `authorization_token`、无自定义 header 槽。**择优提取** `headers["Authorization"]: Bearer xxx` → `authorization_token`（`authorization` 字段为空时回退）；其余 header 丢弃 + WARN（含 `X-API-Key` 等非标认证时 server 可能连不上，上游自然报错）。
- `require_approval`：Anthropic MCP connector **无审批协议**（Claude 直接执行 MCP 工具，无批准前置门）。`never`/缺省正常映射；`on_failure`/`if_referenced` → **降级为 never**（`mcp_toolset` 本就无 approval 配置）+ **WARN**（含 `server_label`、原 `require_approval` 值，审计"有无审批的破坏性工具在执行"）。客户端通过"未收到 `mcp_approval_request`、`mcp_call` 直接 completed"感知审批未走（OpenAI Responses 协议无标准"approval 绕过"标注位，不往 output 注入以免破坏协议）。无配置开关。
- `connector_id` / `tunnel_id`（OpenAI 私有托管 connector / Secure Tunnel）：属 OpenAI 私有基础设施，不在 Anthropic 标准范围 → **fail-fast**（请求含这些来源时返回明确转换错误）。客户端改用 `server_url` 形式。
- **类型链约束**：标准 `MessageNewParams` 顶层无 `mcp_servers`、`ToolUnionParam` 无 `OfMCPToolset`。实现方案：`ToAnthropic` 额外产出 MCP 定义（server 列表 + toolset），`client.Stream` 在 marshal 后用类似 `injectStream` 的 JSON 注入把 `mcp_servers` 与 `mcp_toolset` 注入请求体（避免把整条类型链切到 beta）。

#### 2.3 回程流式（批次 B：catalog/streamconv 注册 `mcp_tool_use`/`mcp_tool_result` handler + `ScanEvents` probe）

Anthropic 回程 `mcp_tool_use` / `mcp_tool_result` 是 beta block，标准 `MessageStreamEventUnion` / `ContentBlockStartEventContentBlockUnion` **无 `Of*` 变体承载**。方案：在 `ScanEvents` 增加 probe（参考现有 error 事件 probe 机制），识别 `type=mcp_tool_use` / `type=mcp_tool_result` 的 raw payload，合成 converter 可识别的结构化事件；converter 映射：

| Anthropic block | Responses 事件 / item |
|---|---|
| `mcp_tool_use{id,name,server_name,input}` | `output_item.added`（`mcp_call`，`server_label`=server_name，arguments=input JSON）；`mcp_call.in_progress`；`mcp_call_arguments.delta` + `done` |
| `mcp_tool_result{tool_use_id,is_error,content}` | `mcp_call` output（content text）；`mcp_call.completed`（is_error 时 `failed`）+ `output_item.done` |

`mcp_list_tools`：Anthropic 不暴露工具列表 item → **不发**（OpenAI 客户端缺它仍可工作）。登记为损失。

#### 2.4 信息损失

- `mcp_list_tools` 工具列表不回传（Anthropic 连接后注入系统提示，不通过 item 暴露）；历史回灌丢弃 + WARN。客户端靠 `mcp_call` 照常工作，`mcp_list_tools` 非执行前置。
- `require_approval`（`on_failure`/`if_referenced`）：Anthropic 无审批协议，**降级为 never + WARN**[server_label, 原值]（多数场景 approval 是防御性默认，透明降级优于 fail-fast 致完全不可用）；`never`/缺省正常映射。回程不产出 `mcp_approval_request`；历史 `mcp_approval_response` 回灌无 Anthropic 等价，丢弃并 WARN（raw_preserved）。
- 自定义 `headers`：仅 `Authorization: Bearer` 提取到 `authorization_token`（`authorization` 字段空时回退），其余丢弃 + WARN。
- `connector_id` / `tunnel_id`：OpenAI 私有托管服务，不在 Anthropic 标准范围，fail-fast。
- 需 beta API（`mcp-client-2025-11-20`，代码做命名常量便于跟进版本演进）；按标准发，后端若不实现 beta endpoint 则上游自然报错、网关透传为 `response.failed`（不预判后端、不加配置开关）。

## 状态升级清单（实现后更新 `docs/protocol-coverage.md`）

| 行 | 现 | 目标 |
|---|---|---|
| Tool `code_interpreter` | deferred | `lossy_supported`（→code_execution_20250522，container 丢失） |
| Input/Output `code_interpreter_call` | deferred | `lossy_supported` |
| Events `code_interpreter_call*` | deferred | `lossy_supported` |
| Anthropic `code_execution_tool_result` | deferred | `supported` |
| Tool `mcp` | unsupported_by_backend | `lossy_supported`（→beta mcp_servers+mcp_toolset） |
| Input/Output `mcp_call` | unsupported_by_backend | `lossy_supported` |
| Events `mcp_call*` | unsupported_by_backend | `lossy_supported` |
| `mcp_list_tools` | unsupported_by_backend | 保持（Anthropic 不暴露工具列表 item） |
| `mcp_approval_request` / `mcp_approval_response` | unsupported_by_backend | 保持（Anthropic 无审批；`require_approval≠never` 时降级为 never + WARN，不产 approval_request，历史回灌 raw_preserved） |
| `bash_code_execution_tool_result` / `text_editor_code_execution_tool_result` | deferred | 保持 `deferred` |

## 分批计划

- **批次 0：通用 tool 架构重构**。建 catalog，迁移现有 tool（function/custom/shell/apply_patch/tool_search/namespace/web_search），请求侧 + 回程侧 dispatch 全部走 catalog，**行为不变**（现有测试全 GREEN）。纯重构，为 A/B 铺路。
- **批次 A：code interpreter 接入**。catalog 注册 `code_interpreter`（`serverTool`），复用批次 0 为 web_search 建立的 `serverTool` catalog 模式，验证其对标准 server tool 的扩展。纯 `convert` + `streamconv`，不动 `client`。
- **批次 B：MCP 接入**。catalog 注册 `mcp`（`betaServerTool`），验证架构对 beta server tool 的扩展。涉及 `client`（beta header）+ `convert`（mcp_servers / mcp_toolset 注入）+ `streamconv`（mcp block probe 解析）。工程量与风险最大，单独成批。

每批独立可合并：批次 0 合并后 main 即获得通用架构收益（即便不做 A/B，新增 tool 也变简单）；A/B 在架构上各自独立。

## 测试策略

- 复刻 `internal/streamconv/converter_test.go` 的 web search 用例模式（server_tool_use → 事件链 → result → completed）。
- wire 样本取自官方文档：code execution 的 `server_tool_use(code_execution)` + `code_execution_tool_result`；MCP 的 `mcp_tool_use` + `mcp_tool_result`。
- 请求侧用 `internal/convert/request_test.go` 表驱动测试覆盖 tool 声明与 input 回灌。
- 批次 0 是纯重构，以现有测试为安全网（全程 GREEN，不写新 RED）；批次 A/B 是新能力，先写 RED 测试证明当前 fail-fast / 跳过，再实现，再 GREEN。涉及流式状态机补充 `task test-race`。

## 已知限制（同步更新 README「已知限制」与 `docs/protocol-coverage.md`）

- code interpreter：`container`（file_ids / memory_limit / 显式 container）、代码生成文件（`file_id`→`url`）不可转换。
- MCP：`mcp_list_tools` 工具列表、`require_approval` 审批流、自定义 `headers`、`connector_id`/`tunnel_id` 不可转换；需后端支持 beta `mcp-client-2025-11-20`。
