# Implementation Plan: 思维标签 LangChain 式流式解析

**Branch**: `004-thinking-tags-langchain` | **Date**: 2026-08-19 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/004-thinking-tags-langchain/spec.md`

**Note**: 设计细节已先行落于 `docs/superpowers/specs/2026-08-19-c-chat-thinking-tags-langchain-design.md`（含 LangChain 源码逐条对照），本 plan 聚焦仓库落地结构与章程校验，不重复设计内容。

## Summary

在 `internal/chatstreamconv` 的 c 流式出站正文解析中，按 langchainjs `ChatDeepSeek` 的 `_streamResponseChunks` 状态机一比一实现 `<think>` / `</think>` 思维标签剥离：标签之间文本进入 Responses reasoning 通道，标签不泄漏到用户可见正文。范围仅限 c 路径流式正文，历史回灌不变。两项用户扩展语义纳入本期：`</think>` 在非思维态作为开标签（toggle 双角色）、连续相同标签去重。

## Technical Context

**Language/Version**: Go 1.23+

**Primary Dependencies**: `github.com/openai/openai-go/v3`（协议常量）、`github.com/anthropics/anthropic-sdk-go`（常量参照）；无新增第三方依赖。

**Storage**: N/A（纯流式转换，无持久化）。

**Testing**: Go 标准 `testing`；被测包同目录 `think_test.go` 表驱动测试；验收门禁 `task check` / `task test-race` / `golangci-lint run ./...`。

**Target Platform**: Linux server（网关 `/v1/responses` 转发热路径）。

**Project Type**: web-service（协议转换层）。

**Performance Goals**: 流式逐 chunk 转换，仅做字符串扫描与尾部前缀暂存，无阻塞、无额外 goroutine；思维标签解析不得拖慢转发路径。

**Constraints**: 仅处理 c 路径（`backend_type=c`）流式正文；不修改 `a` / `r` 路径，不修改 `chatconvert` 历史回灌；标签精确匹配 `<think>` / `</think>`，不引入大小写/空白容错。

**Scale/Scope**: 单特性增量；改动收敛在 `internal/chatstreamconv/converter.go` 与测试文件。

## Constitution Check

*GATE: 对照 `.specify/memory/constitution.md`，本特性全部通过。*

- **I. 产品边界与协议透传**：仅做 wire 对齐，把正文内的思维标签形态映射到 Responses 语义（reasoning / output_text），不替上游裁决能力；范围严格限定 c 路径流式正文，历史回灌与 a/r 路径均不动。通过。
- **II. 分层单向依赖与唯一组装入口**：状态机与解析函数位于 L2 转换层 `internal/chatstreamconv`，仅依赖 `model` / `config` 等下层；不反向 import `server` / `scheduler`。通过。
- **III. 协议事实源与官方 SDK**：`<think>` / `</think>` 是模型在 content 内生成的思维标签约定，非协议事件/块/finish-reason 字面量，不属 AGENTS.md「协议常量对齐官方 SDK」约束范围；故以显式字符串常量 `thinkOpenTag` / `thinkCloseTag` 定义并复用，不硬编码散落。通过。
- **IV. 热路径隔离与结构化可观测**：`feedContentThink` 运行于 `/v1/*` 转发热路径，仅做轻量字符串处理，无阻塞、无 side-effect 日志；仅在 `content_filter` 丢弃路径触发状态重置。通过。
- **V. 测试、文档与最小增量**：测试与实现同包（`think_test.go`），18 类矩阵逐条覆盖；设计文档先行，改动保持最小增量。通过。

无违反项，无需 Complexity Tracking。

## Project Structure

### Documentation (this feature)

```text
specs/004-thinking-tags-langchain/
├── spec.md              # 功能规格（user story / FR / SC）
├── plan.md              # 本文件
├── tasks.md             # 任务拆分（已落地，全部完成）
├── checklists/
│   └── requirements.md  # 规格质量清单（已完成）
docs/superpowers/specs/
└── 2026-08-19-c-chat-thinking-tags-langchain-design.md  # 设计 + LangChain 源码对照
```

### Source Code (repository root)

```text
internal/chatstreamconv/
├── converter.go         # 思维标签状态机：feedContentThink / flushThinkEnd / resetThinkEnd / tagPrefixLen / trailingTagPrefixLen
└── think_test.go        # 18 类测试矩阵 + 工具交错 + content_filter 重置
```

**Structure Decision**: 单包增量结构，状态机与测试集中在 `internal/chatstreamconv`；设计细节外置到 `docs/superpowers/specs/` 避免 spec 内写实现细节。

## Complexity Tracking

无（Constitution Check 无违反，无需复杂度权衡）。
