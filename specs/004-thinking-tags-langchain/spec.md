# Feature Specification: 思维标签 LangChain 式流式解析

**Feature Branch**: `004-thinking-tags-langchain`

**Created**: 2026-08-19

**Updated**: 2026-08-19

**Status**: Approved

**Input**: User description: "重新处理正文中的 `<think>` 与 `</think>` 思维标签，按 LangChain 社区实现一比一处理；只做 c 路径流式正文解析，历史回灌维持现状。本轮补充：1比1 对照 LangChain 的思维标签处理，行为语义以 langchainjs `ChatDeepSeek` 实现为准；`</think>` 同时作为开始与结束标签（toggle）的情况也必须处理，旧网关实现废弃，完全按 LangChain 1:1 重构。"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Chat 流式正文思维标签正确剥离 (Priority: P1)

使用 Chat 兼容上游时，模型可能把思维过程写在正文里，用 `<think>` 与 `</think>` 两个标签包裹。用户在客户端看到的最终回答必须只包含真正的内容：标签被剥离，标签之间的思维内容进入 reasoning 通道；如果上游已经通过独立字段返回思维内容，则正文标签不再二次解析。

**Why this priority**: 这是用户可见的流式协议缺陷，标签泄漏会直接污染最终回答和历史回灌，是本轮唯一要修复的行为。

**Independent Test**: 向转换层输入一组覆盖完整标签、跨 chunk 拆分、流末未闭合、孤立闭标签、原生 reasoning 与 content 同包的 Chat 流式事件，逐条验证输出中标签不泄漏、推理与正文归属正确。

**Acceptance Scenarios**:

1. **Given** 上游流式正文包含完整 `<think>...</think>`，**When** 网关完成转换，**Then** 输出包含 reasoning item 与最终正文，两个标签不进入用户可见正文。
2. **Given** `<think>` 或 `</think>` 被拆成多个流式片段，**When** 网关拼接处理，**Then** 仍能正确识别标签边界，标签不进入正文。
3. **Given** 流结束仍处于思维块内，**When** 网关收口，**Then** 已收到的思维文本作为 reasoning 保留，不被丢弃。
4. **Given** 正文只出现孤立 `</think>`、没有前置 `<think>` 且其后跟随文本，**When** 网关处理，**Then** 该标签按 toggle 语义作为开标签进入思维块，其后文本作为 reasoning 收口，不进入正文。
5. **Given** 同一 chunk 已携带原生 reasoning 字段，**When** 网关处理该 chunk 的 content，**Then** 不再按标签解析，content 原样透传。
6. **Given** 上游仅用 `</think>` 同时作为思维块的开/闭标签（如 `</think>思考</think>回答`），**When** 网关处理，**Then** 思维文本进入 reasoning，正文不含标签。
7. **Given** 正文连续出现两个同一个思维标签（同一 chunk 内或跨 chunk 拆分），**When** 网关处理，**Then** 重复标签合并为单个分隔符，不产生空思维块或 toggle 抖动。

### Edge Cases

- 思维标签跨 chunk 被拆成 `<th` + `ink>`、`</th` + `ink>` 等碎片时，标签前缀必须暂存（最长后缀匹配），不能把残缺标签泄漏进正文或 reasoning。
- `</think>` 必须同时支持开/闭双角色：非思维态下作为开标签进入思维块（toggle），思维态下作为闭标签退出；该语义是 LangChain 1:1 之外的用户要求扩展。
- 闭标签后同一 chunk 的残余文本必须立即作为正文输出并清空缓冲，不再对该残余做二次标签解析；残余即使包含 `<think>` 也按正文透传（LangChain 一比一语义）。
- 空 content 分片（仅携带 tool_calls / usage / finish_reason 等）必须原样透传，且不得清空或改变思维缓冲与思维状态。
- 思维块内出现工具调用时，必须先关闭 reasoning，再进入工具调用；工具调用分片不改变思维缓冲，其后的正文继续按当前状态处理。
- `content_filter` 等丢弃正文路径不得残留思维缓冲状态，避免下一次请求串状态。
- 标签精确匹配 `<think>` 与 `</think>`：`< think>`、`</think >`、大小写变体不识别，按普通文本或思维文本原样输出。
- 思维文本中出现字面 `</think>` 时按 LangChain 语义作为闭标签立即关闭（不转义）；thinking 内再次出现 `<think>` 不改变状态，作为思维文本输出。
- 连续相同标签 MUST 去重：同一 chunk 内紧邻的相同标签视为单个分隔符；上一 chunk 以完整标签收尾且无后续文本时，下一 chunk 开头的同标签同样并入该分隔符，不改变思维状态。

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: 网关 MUST 在 Chat 流式转换中识别 `<think>` 与 `</think>` 标签，并把标签之间的文本映射为 reasoning 内容。
- **FR-002**: 标签本身 MUST NOT 出现在用户可见正文中；开标签前、闭标签后的文本 MUST 保持为正文。
- **FR-003**: 标签 MUST 支持跨流式 chunk 拆分，残缺标签前缀 MUST 被暂存并在后续 chunk 中正确拼接。
- **FR-004**: 上游 chunk 已带原生 reasoning 字段时，MUST 优先透传该字段，并跳过对该 chunk content 的标签解析。
- **FR-005**: 流结束时仍处于思维块内，MUST 把剩余思维文本（含残缺闭标签前缀）作为 reasoning 收口；非思维态剩余文本（含残缺标签前缀）MUST 作为正文收口。
- **FR-006**: `</think>` MUST 同时作为开/闭标签：非思维态下进入思维块，思维态下退出思维块。
- **FR-007**: 思维块内出现工具调用 MUST 先关闭 reasoning，再输出工具调用；工具调用分片 MUST 不改变思维缓冲与思维状态。
- **FR-008**: 闭标签后的同 chunk 残余文本 MUST 立即作为正文输出并清空缓冲，MUST NOT 对该残余再次执行标签解析。
- **FR-009**: 空 content 分片 MUST 保持思维缓冲与思维状态不变。
- **FR-010**: 历史回灌行为 MUST 保持不变；本特性只调整流式正文解析。
- **FR-011**: 连续相同标签 MUST 去重（同一 chunk 与跨 chunk 两种形态），重复标签不进入正文、不产生空思维块。

### Key Entities

- **思维标签**: `<think>` 作为思维块开始标签；`</think>` 同时作为开始与结束标签（toggle）。
- **思维缓冲**: 跨 chunk 暂存尚未完成标签拼接或尚未确定归属的正文片段。
- **思维状态**: 当前是否处于思维块内（开标签后、闭标签前为开启），空 content 分片不得改变它。
- **reasoning 内容**: 从正文标签之间提取的思维文本，进入用户不可见但可存续的 reasoning 通道。
- **最终正文**: 用户可见回答，标签被剥离后的剩余文本。

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 完整标签、跨 chunk 拆分、流末未闭合、toggle 开闭、原生 reasoning 同包、闭标签残余、空 content 分片、连续同标签去重、流末残缺前缀九类场景全部通过自动化验收，标签泄漏为 0。
- **SC-002**: 每个验收场景都能重复运行 3 次且结果一致。
- **SC-003**: 边界样例（同一 chunk 先开后闭、闭标签残余、跨 chunk 拆分、流末未闭合、toggle 开闭、连续同标签去重、空 content 分片、非精确标签）的输出归属与 LangChain `_streamResponseChunks` 逐项一致，其中 `</think>` 双角色与连续同标签去重为用户要求的扩展语义。
- **SC-004**: 原生 reasoning 字段行为与历史回灌行为不受影响，相关既有测试保持通过。
- **SC-005**: 流式转换并发测试无 race；`task check`、`task test-race`、`golangci-lint run ./...` 全部通过。

## Assumptions

- 1比1 基线为 langchainjs `ChatDeepSeek` 的 `_streamResponseChunks` 状态机：PR `langchain-ai/langchainjs#9726`（commit `1877454e6a501eba7bf36fc088335eaea149c8ce`），当前 main 分支同文件仍是同一逻辑；Python 版 `langchain-deepseek` 只透传 `reasoning_content`、不解析正文标签，不作为基线。`</think>` 在非思维态下作为开标签与连续同标签去重是本特性的扩展语义，其余行为逐条对齐 LangChain。
- 实际目标标签是精确匹配的 `<think>` 与 `</think>`；界面可能吞掉尖括号，导致显示为 ` thinking` / ` response`，以原始流为准。
- 本特性只覆盖 c 路径流式正文解析，不调整 Responses → Chat 的历史 `reasoning_content` 回灌。
- `<think>` 在思维态内按 LangChain 语义作为思维文本，不做开/闭切换；多标签注册表（`<think>`、`<reasoning>` 等）、标签空白与大小写容错不在本期范围。
- 背景设计文档已单独记录在 `docs/superpowers/specs/2026-08-19-c-chat-thinking-tags-langchain-design.md`，内含逐条源码对照证据。
