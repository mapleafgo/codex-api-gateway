# Implementation Plan: 子 Agent 对话归属修复

**Branch**: `001-subagent-conversation-repair` | **Date**: 2026-08-18 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/001-subagent-conversation-repair/spec.md`

## Summary

修复 Codex 0.147 多 agent V2 回灌中的父子消息归属回归。根因是 `type=agent_message` 不被 openai-go SDK 识别，当前网关把它并入最后一个 `function_call_output`，导致子 agent `FINAL_ANSWER` 被伪装成 `wait_agent` 工具结果，父 agent 后续请求丢失独立 assistant 回复并出现问答错配。修复以 raw input 位置重建 assistant 消息，并在重建后修复后续 assistant `output_text` 的索引错位。第二次 chat 协议复现进一步确认：`agent_message` 必须在 `output_text` 恢复前从 raw 整体重建 items，否则 SDK 丢 agent 后的索引偏移会覆盖/丢弃真正的后续 user 输入。

## Technical Context

**Language/Version**: Go 1.26.5

**Primary Dependencies**: openai-go v3.50.0、anthropic-sdk-go v1.63.0、Codex CLI 0.147.0

**Storage**: 无新增存储；会话历史仍由 Codex 客户端完整回灌。

**Testing**: Go 标准测试、环境变量门控的真实 `codex app-server --stdio` 端到端测试

**Target Platform**: Linux amd64 本机服务；协议逻辑与平台无关

**Project Type**: Go HTTP 协议网关

**Performance Goals**: 不新增热路径锁、goroutine 或网络调用；仅调整既有 raw JSON 恢复流程

**Constraints**: 保持分层依赖与最小增量；不引入 session store；不改变 r 路径透传语义

**Scale/Scope**: `internal/convert` 的 a/c 请求解码共用路径；端到端测试位于 `internal/server`

## Constitution Check

| Gate | 结论 |
|---|---|
| 产品边界与协议透传 | 通过。仅恢复 Codex 已发送的 `agent_message` 语义，不代上游裁决能力。 |
| 协议事实源与官方 SDK | 通过。以 Codex 0.147 tag 源码、openai-go 实测与 `docs/protocol-coverage.md` 为准。 |
| 分层单向依赖 | 通过。修复位于 L2 `internal/convert`，测试位于 `internal/server`，不引入反向依赖。 |
| 热路径隔离 | 通过。无新增阻塞观测、管理入口或后台任务。 |
| 测试、文档与最小增量 | 通过。单元测试覆盖顺序/role/后续文本，真实 app-server 测试覆盖父/子/父三段流；同步协议矩阵。 |

## Project Structure

### Documentation (this feature)

```text
specs/001-subagent-conversation-repair/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   └── decode-agent-message.md
└── tasks.md
```

### Source Code (repository root)

```text
internal/convert/
├── request.go
└── restore_agent_message_test.go

internal/server/
└── appserver_e2e_test.go

docs/
└── protocol-coverage.md
```

**Structure Decision**: 请求语义修复落在 L2 转换层 `internal/convert`，由 a/c 两条路径复用；真实 app-server 编排验收位于 L4 测试侧 `internal/server`，不改变生产依赖方向。

## Complexity Tracking

无章程违规。
