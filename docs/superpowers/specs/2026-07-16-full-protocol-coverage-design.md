# Responses 全协议覆盖设计

日期: 2026-07-16
状态: 设计已批准，待实施计划

## 背景

本仓库是 `codex-api-gateway`，负责把 OpenAI Responses API 请求转换为 Anthropic Messages API 请求，并把 Anthropic SSE 转回 Responses SSE。当前实现已经覆盖 Codex 常用路径，包括文本、图片/文件输入、function/custom tool、reasoning、部分 compaction、tool_search 与 session replay。

最新审计发现：OpenAI Responses API 的 input item、tool、tool_choice、output item、SSE event 和枚举远多于当前语义级支持范围。继续靠局部补丁容易再次遗漏，尤其是 hosted tools、MCP、code interpreter、image generation、computer use、Anthropic server tool result 与 refusal/status 枚举。

## 目标

以 OpenAI Responses API 全协议为主表，建立可维护的逐项覆盖机制：

1. 新增 `docs/protocol-coverage.md`，作为协议覆盖矩阵的权威记录。
2. 所有协议项只能落入 `supported`、`lossy_supported`、`raw_preserved`、`unsupported_by_backend`、`deferred` 五种状态之一。
3. Anthropic 有等价能力的项逐步做语义级转换。
4. Anthropic 没有等价能力的项不做假支持，明确登记为 `unsupported_by_backend` 或 `deferred`。
5. 任何新增语义支持必须走 TDD：先写失败测试，再实现，再验证。

## 非目标

- 不在第一阶段实现 OpenAI hosted tools 的真实执行能力，例如 file search、image generation、computer use、MCP approval。
- 不把 Anthropic 没有的能力伪装成成功转换。
- 不为了维持旧结构而牺牲协议正确性；允许重写历史代码和拆分现有模块，只要每一步有测试保护且范围服务于协议覆盖目标。
- 不拒绝第三方依赖；允许并鼓励引入能显著降低实现复杂度、减少协议误差或提供成熟解析/生成能力的依赖。新依赖必须在实施计划中说明用途、替代方案和验证方式。

## 状态模型

`docs/protocol-coverage.md` 中每一行必须有明确状态：

- `supported`: 有语义级转换，并有测试覆盖。
- `lossy_supported`: 可用但有字段或行为损耗，文档必须说明损耗。
- `raw_preserved`: 原始 JSON 被保存或注入上下文，避免静默丢弃，但不宣称语义支持。
- `unsupported_by_backend`: Anthropic Messages 无等价能力，不能安全模拟。
- `deferred`: 需要专项设计才能决定映射方式。

实现时不得新增第六种状态。需要表达细节时写在说明列。

## 架构设计

### 覆盖矩阵优先

`docs/protocol-coverage.md` 是实现入口。每次补齐协议项时，先从矩阵选定目标行，再写 RED 测试证明当前行为缺失或错误，然后实现最小代码，最后把状态更新为对应结果。

矩阵按以下分类维护：

- Request Parameters
- Input Content Union
- Input Item Union
- Tool Union
- Tool Choice Union
- Output Item Union
- Responses SSE Events
- Anthropic Content Blocks And Stream Events
- Enum Mapping

### 转换策略

转换按能力分层：

1. **语义级转换**：OpenAI 与 Anthropic 有明确对应关系，例如 `function_call` 到 `tool_use`、`input_text` 到 text block。
2. **损耗转换**：能完成主要语义但字段损失，例如 OpenAI `namespace` 被压平成工具名。
3. **raw 保真**：当前无法语义转换但可以避免丢弃，例如 unknown input item 存 raw JSON。
4. **明确不支持**：无后端等价能力或模拟会误导客户端，例如 image generation hosted tool。
5. **延期专项**：存在可能映射但风险较高，例如 code interpreter、web search server tool、MCP。

实现可以重写现有转换、stream、session 或 model 层结构。判断标准不是“是否兼容旧代码形状”，而是协议覆盖矩阵是否更清晰、测试是否更直接、后续补齐是否更低风险。对明显适合成熟库处理的场景，例如 JSON Schema、JSON Patch / unified diff、协议枚举抽取、SSE 解析或文档化覆盖检查，可以引入第三方依赖，前提是依赖边界清晰且不会替代两侧官方 SDK 的协议来源地位。

### 错误与枚举策略

枚举翻译必须以两侧 SDK 的官方枚举为准。特别是 OpenAI `incomplete_details.reason` 当前 SDK 只列 `max_output_tokens` 与 `content_filter`，不能把 Anthropic `pause_turn` 或 `refusal` 原样写进去。

第一批实现需要把 stop reason 映射修正为：

- `end_turn`、`tool_use`、`stop_sequence` -> `completed`
- `max_tokens` -> `incomplete` + `max_output_tokens`
- `refusal` -> 输出 refusal content 或 `content_filter`，具体由测试锁定
- `pause_turn` -> 不写非法 incomplete reason，先按明确失败或 deferred 策略处理

### 不支持能力策略

`unsupported_by_backend` 项不能静默忽略。实现上可以选择：

- 请求阶段返回 400/failed，说明工具或 item 不受当前后端支持。
- 对历史 item 做 raw 保真，但不触发工具执行。
- 对 stream 中 Anthropic server-only block 生成可诊断记录，避免静默丢掉模型输出。

具体行为由实施计划逐任务确定，但必须满足“不可无声丢失”。

## 第一批实施范围

第一批只做协议骨架和 Codex 工具闭环中风险最高的项：

1. 修正 stop reason / incomplete reason / refusal 枚举翻译。
2. 补 `shell_call`、`local_shell_call`、`apply_patch_call` 及对应 output 的语义级输入转换。
3. 补 `refusal` content 和 SSE 事件输出。
4. 补 `allowed_tools` tool_choice 的安全降级策略。
5. 对 hosted tool choice、MCP choice、无等价 input/tool/output item 返回明确 unsupported 或记录 deferred。
6. 对 Anthropic 未处理 stream block 增加诊断策略，避免静默丢弃。

## 测试策略

每个实现任务使用 TDD：

1. 在对应包写一个最小失败测试。
2. 运行目标测试，确认失败原因是功能缺失或枚举错误。
3. 写最小实现。
4. 运行目标包测试。
5. 运行 `go test ./...`。

测试位置沿用现有包：

- request conversion: `internal/convert/request_test.go`
- custom/freeform tool: `internal/convert/customtool_test.go`
- stream conversion: `internal/streamconv/converter_test.go`
- session replay: `internal/store/session_test.go`
- HTTP integration: `internal/server/integration_test.go`
- model/event shape: `internal/model/event_test.go`

## 文档维护规则

每个协议项状态变化必须同时满足：

1. `docs/protocol-coverage.md` 状态更新。
2. 对应测试证明行为。
3. 实现代码引用 SDK 常量或 SDK 类型，不新增裸字符串枚举，除非 SDK 未暴露该值。
4. 若仍是 `deferred` 或 `unsupported_by_backend`，说明列必须给出原因。

## 风险与对策

| 风险 | 对策 |
|---|---|
| 全协议范围过大导致实施失控 | 用覆盖矩阵拆分，第一批只处理枚举、shell/apply_patch/refusal/diagnostic |
| Anthropic 无等价能力时误报支持 | 统一使用 `unsupported_by_backend`，禁止假转换 |
| Hosted tools 映射复杂 | 先登记，后续逐项专项设计 |
| 为避免改动而堆叠兼容层 | 允许重写历史结构，任务边界按可测试交付拆分 |
| 新依赖引入供应链和维护成本 | 实施计划中逐项说明用途、替代方案、版本和验证命令 |
| SDK union 演进导致矩阵过期 | 后续可增加 SDK union 抽取工具，目前先人工维护矩阵 |
| 当前工作树已有未提交实现改动 | 文档提交只 stage 新增文档，避免混入实现改动 |

## 成功标准

- `docs/protocol-coverage.md` 覆盖 OpenAI request 参数、input item、tool、tool_choice、output item、SSE event、枚举，以及 Anthropic content block/stream event。
- 第一批实施计划能从矩阵中逐项选任务，每个任务具备 RED/GREEN 验证命令。
- 后续补齐时不会再出现“协议项没有登记、代码静默忽略”的情况。
