# Tasks: 子 Agent 对话归属修复

**Input**: Design documents from `specs/001-subagent-conversation-repair/`

**Prerequisites**: `spec.md`、`plan.md`、`research.md`、`data-model.md`、`contracts/decode-agent-message.md`、`quickstart.md`

## Phase 1: Setup

**Purpose**: 固化协议事实与可复现环境

- [x] T001 核实 Codex 0.147 `agent_message`、collaboration namespace 与 assistant role 契约，记录到 `specs/001-subagent-conversation-repair/research.md`

---

## Phase 2: Foundational

**Purpose**: 先建立失败测试与可执行验收链路

- [x] T002 [P] 增加单条 `agent_message` 位置、role 与后续 assistant 文本恢复测试，文件 `internal/convert/restore_agent_message_test.go`
- [x] T003 [P] 增加真实 `codex app-server --stdio` 父/子/父端到端测试，文件 `internal/server/appserver_e2e_test.go`

---

## Phase 3: User Story 1 - 单个子 Agent 对话保持对应

**Goal**: 子 agent `FINAL_ANSWER` 不再被合并进 `wait_agent` 工具结果，父 agent 能基于正确历史汇总。

**Independent Test**: `APP_SERVER_E2E=1 go test ./internal/server -run TestAppServerSubAgentHistoryKeepsAgentMessage -count=1 -v`

- [x] T004 [US1] 按 raw 位置将 `agent_message` 重建为 assistant 消息，并修复后续 `output_text` 对齐，文件 `internal/convert/request.go`
- [x] T005 [US1] 保留真实 app-server 断言：`wait_agent` 结果原样、`CHILD_FINAL_A` 为后续 assistant、父答复为 `PARENT_FINAL_A`，文件 `internal/server/appserver_e2e_test.go`

---

## Phase 4: User Story 2 - 并发子 Agent 不串话

**Goal**: 多条 inter-agent 消息按 raw 顺序恢复，不移动到末尾或复用同一工具结果。

**Independent Test**: `go test ./internal/convert -run TestRestoreMultipleAgentMessagesPreserveRawOrder -count=1 -v`

- [x] T006 [US2] 覆盖两个 `agent_message` 与两组 `function_call_output` 交错时的顺序和工具输出不变性，文件 `internal/convert/restore_agent_message_test.go`

---

## Phase 5: User Story 3 - 回归定位与可信修复

**Goal**: 保留根因证据、协议矩阵和可重复验收路径。

**Independent Test**: 阅读 `research.md` 与 `contracts/decode-agent-message.md`，并运行 quickstart 中两条命令。

- [x] T007 [US3] 记录 Codex 0.147 事实源、修复前错位证据与方案取舍，文件 `specs/001-subagent-conversation-repair/research.md`
- [x] T008 [US3] 同步 a/c 路径 `agent_message` 覆盖状态与变更记录，文件 `docs/protocol-coverage.md`

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: 全量验证与最终收敛

- [x] T009 运行常跑单元测试：`go test ./internal/convert -run 'TestRestore(AgentMessage|MultipleAgentMessages)' -count=1`
- [x] T010 运行真实 app-server 端到端测试：`APP_SERVER_E2E=1 go test ./internal/server -run TestAppServerSubAgentHistoryKeepsAgentMessage -count=3`
- [x] T011 运行 `task check`
- [x] T012 运行 `task test-race`
- [x] T013 运行 `golangci-lint run ./...`
- [x] T014 [US1] 修复 r 路径 plaintext `agent_message` outbound 折算，文件 `internal/backend/responses.go`
- [x] T015 [US1] 增加 r 路径真实 app-server 端到端测试，文件 `internal/server/appserver_e2e_test.go`

## Phase 7: 第二次 chat 复现回归

**Goal**: 子 agent 完成后，父会话 follow-up 用户输入必须原样出现在 Chat 出站历史尾部。

- [x] T016 [US3] 复现 `01a0151b` chat 路径：`agent_message` 与后续 `output_text` 恢复互相错位导致最新 user 被覆盖/丢弃，记录到 `research.md`
- [x] T017 [US1] 增加 `restoreAgentMessageKeepsLaterUserTurn` 单元测试并在修复前确认 RED，文件 `internal/convert/restore_agent_message_test.go`
- [x] T018 [US1] 增加 chat 路径真实 app-server follow-up 端到端测试，文件 `internal/server/appserver_e2e_test.go`
- [x] T019 [US1] 修复 `restoreAgentMessageFromRaw` 从 raw 整体重建 + 调整恢复顺序，文件 `internal/convert/request.go`
- [x] T020 运行 `APP_SERVER_E2E=1 go test ./internal/server -run TestAppServerChatBackendPreservesFollowUpQuestion -count=3`
- [x] T021 运行 `go test ./internal/convert ./internal/chatconvert ./internal/backend -count=1`
- [x] T022 运行 `task check`
- [x] T023 运行 `task test-race`
- [x] T024 运行 `golangci-lint run ./...`
- [x] T025 [US1] 增加 a 路径 follow-up 端到端测试，验证 `agent_message` 修复后与 c 路径行为一致，文件 `internal/server/appserver_e2e_test.go`

---

## Dependencies & Execution Order

1. T001 协议事实源。
2. T002、T003 可并行建立失败测试。
3. T004、T005 完成 P1 修复与端到端。
4. T006 验证多 agent 顺序。
5. T007、T008 可并行完成证据与矩阵。
6. T009-T015 依次完成最终验证。
7. T016-T019 完成第二次 chat 复现的测试与修复；T020-T024 为最终门禁。

## Implementation Strategy

MVP 是 T001-T005：单个子 agent 的完整父/子/父对话归属修复。T006 起防止并发交错回归，T007-T013 保证事实源、矩阵和全量门禁收敛。
