# Tasks: 剔除正文思维标签处理

**Input**: Design documents from `specs/005-remove-think-tags/`

**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/

**Tests**: 本特征是删除型改动；测试任务以既有测试回归与残留检查为主（spec 的
Independent Test 与 quickstart.md 验证场景）。

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2)
- Include exact file paths in descriptions

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: 无新建基础设施，本阶段省略。

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: 无阻塞前置依赖，本阶段省略。

---

## Phase 3: User Story 1 - 正文原样透传（不再识别思维标签）(Priority: P1) MVP

**Goal**: 删除 `chatstreamconv` 正文思维标签解析：content 一律按普通正文透传，
标签不剥离、不产生推理内容。

**Independent Test**: `go test ./internal/chatstreamconv -count=1` 通过且
`think_test.go` 不再存在；`rg "feedContentThink|thinkOpenTag|thinkCloseTag|isThinking|thinkBuf|thinkLastTag" internal/chatstreamconv` 无匹配。

### Implementation for User Story 1

- [x] T001 [US1] 删除标签解析测试文件 internal/chatstreamconv/think_test.go（其引用待删常量 thinkOpenTag/thinkCloseTag）
- [x] T002 [US1] 删除正文标签常量与状态字段（thinkOpenTag/thinkCloseTag/isThinking/thinkBuf/thinkLastTag 及注释）internal/chatstreamconv/converter.go
- [x] T003 [US1] content 无原生 reasoning 时改为直接 feedText 透传（替换 feedContentThink 调用）internal/chatstreamconv/converter.go
- [x] T004 [US1] 删除 feedContentThink/flushThinkEnd/resetThinkEnd/tagPrefixLen/trailingTagPrefixLen 实现 internal/chatstreamconv/converter.go
- [x] T005 [US1] 清理 finish_reason、FeedDone、prepareRefusalOutput 中的 flushThinkEnd/resetThinkEnd 调用 internal/chatstreamconv/converter.go

**Checkpoint**: 至此 US1 完成：正文原样透传，无标签状态机。

---

## Phase 4: User Story 2 - 独立推理字段映射保持不变 (Priority: P2)

**Goal**: `delta.reasoning_content` / `delta.reasoning` / `delta.reasoning_text`
的既有推理映射不受删除影响。

**Independent Test**: `go test ./internal/chatstreamconv -run "Reasoning" -count=1`
覆盖 TestReasoningContentBeforeText 等原生推理字段测试，全部通过。

### Implementation for User Story 2

- [x] T006 [P] [US2] 核对（不改动）internal/chatstreamconv/converter_test.go 中原生推理字段测试仍覆盖 reasoning_content/reasoning_text 映射

**Checkpoint**: 推理字段映射保持，无回归。

---

## Phase 5: Polish & Cross-Cutting Concerns

**Purpose**: 文档同步与全量验证。

- [x] T007 更新 docs/protocol-coverage.md：变更记录新增 2026-08-20 剔除条目，确认 c 路径出站矩阵 `delta.content` 行"原样透传"语义
- [x] T008 全量门禁：`rg` 残留检查、`task check`、`task test-race`、`golangci-lint run ./...`

---

## Phase 5: Convergence

- [x] T009 新增 internal/chatstreamconv/converter_test.go 表驱动测试：content 含完整/残缺/跨分片思维标签时逐字原样进入 output_text、不产生 reasoning item per US1 验收场景 1-3 与 SC-001（partial）

---

## Dependencies & Execution Order

### Phase Dependencies

- **US1 (Phase 3)**: 无前置依赖，先行实施
- **US2 (Phase 4)**: 与 US1 大部分并行；但 T001（删 think_test.go）必须先于 T002（删常量），否则编译失败
- **Polish (Phase 5)**: 依赖 US1 与 US2 完成

### User Story Dependencies

- **User Story 1 (P1)**: 独立交付，删除型变更主体
- **User Story 2 (P2)**: 只核对既有测试，不产生代码变更，可独立验证

### Within User Story 1

- T001 → T002 → T003 → T004 → T005 同文件串行，按顺序执行（删除常量前先删引用它的测试）

### Parallel Opportunities

- T006 [P]：与 US1 各任务并行（不同文件）

---

## Parallel Example: User Story 1 + User Story 2

```bash
# US2 核对与 US1 删除可并行：
Task: "核对 converter_test.go 原生推理字段测试"
Task: "删除 think_test.go；删常量与状态机；content 直接 feedText"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. 完成 US1（T001-T005）
2. **STOP and VALIDATE**: `go test ./internal/chatstreamconv -count=1` + 残留检查
3. 完成 US2（T006）与 Polish（T007-T008）

### Notes

- 保持最小增量：只动 `internal/chatstreamconv/converter.go`、删除 `think_test.go`、
  更新 `docs/protocol-coverage.md`；不触碰 converter_test.go 既有测试与 a/r 路径。
- 历史规格 002/004 与思维标签设计文档按 spec Assumptions 保留为历史记录。
