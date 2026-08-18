# Tasks: 思维标签 LangChain 式流式解析

**Input**: Design documents from `/specs/004-thinking-tags-langchain/`

**Prerequisites**: plan.md (required), spec.md (required), design doc `docs/superpowers/specs/2026-08-19-c-chat-thinking-tags-langchain-design.md`

**Tests**: 本特性在 spec.md 的 SC-003/SC-005 明确要求表驱动测试与门禁，故测试任务为必需。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无依赖）
- **[Story]**: 所属 user story（US1）
- 路径使用仓库真实结构

## Phase 1: Setup (Shared Infrastructure)

- [x] T001 确认分支 `004-thinking-tags-langchain` 与设计文档 `docs/superpowers/specs/2026-08-19-c-chat-thinking-tags-langchain-design.md` 已就绪

---

## Phase 2: Foundational (Blocking Prerequisites)

- [x] T002 [P] 在 `internal/chatstreamconv/converter.go` 定义思维标签常量 `thinkOpenTag="<think>"` / `thinkCloseTag="</think>"` 与状态字段 `thinkBuf` / `isThinking` / `thinkLastTag`
- [x] T003 [US1] 实现 `feedContentThink`：LangChain `_streamResponseChunks` 状态机（开闭标签、toggle 双角色、跨 chunk 前缀暂存、闭标签后残余立即正文且不再二次解析、连续同标签去重）

**Checkpoint**: 状态机核心就绪，可独立解析逐 chunk 正文

---

## Phase 3: User Story 1 - Chat 流式正文思维标签正确剥离 (Priority: P1) 🎯 MVP

**Goal**: c 路径流式正文中的 `<think>`/`</think>` 被正确剥离，标签间文本进入 reasoning，标签不泄漏到用户可见正文；原生 reasoning 同包时跳过标签解析。

**Independent Test**: `go test ./internal/chatstreamconv/ -run TestThink -count=1`，18 类矩阵全绿、标签泄漏为 0。

### Tests for User Story 1

- [x] T004 [P] [US1] 标准完整标签 + open/close 跨 chunk 拆分：`TestThinkTagsLangChainStateMachine` 用例 1-3
- [x] T005 [P] [US1] 流末未闭合 + toggle 开闭 + toggle 跨 chunk：`TestThinkTagsLangChainStateMachine` 用例 4-6
- [x] T006 [P] [US1] 连续同标签去重（闭标签/开标签/标准/跨 chunk）：`TestThinkTagsLangChainStateMachine` 用例 10-13
- [x] T007 [P] [US1] 思维块内 `<think>` 作思维文本 + 闭标签后残余不二次解析：用例 8-9
- [x] T008 [P] [US1] 空 content 分片保持状态 + 原生 reasoning 同包透传：`TestThinkTagsLangChainStateMachine` 用例 14-16
- [x] T009 [P] [US1] 流末残缺前缀按当前态收口 + 非精确标签按普通文本：`TestThinkTagsLangChainStateMachine` 用例 17-18
- [x] T010 [P] [US1] 思维块内工具调用顺序：`TestThinkThenToolCallOrder`
- [x] T011 [P] [US1] `content_filter` 丢弃路径清空思维状态：`TestThinkStateResetOnContentFilter`

### Implementation for User Story 1

- [x] T012 [US1] 实现 `flushThinkEnd`：finish_reason / FeedDone 收口，缓冲按当前态输出（含残缺前缀），与 LangChain end-of-stream flush 一致
- [x] T013 [US1] 实现 `resetThinkEnd`：`content_filter` 等丢弃正文路径清空 `thinkBuf` / `isThinking` / `thinkLastTag`
- [x] T014 [US1] 实现 `tagPrefixLen` / `trailingTagPrefixLen`：跨 chunk 残缺标签最长后缀暂存
- [x] T015 [US1] 在 `ConvertStream` 接入：原生 reasoning 优先透传、空 content 分片保持状态、其余走 `feedContentThink`

**Checkpoint**: US1 全功能可用且 18 类测试独立验证通过

---

## Phase 4: Polish & Cross-Cutting Concerns

- [x] T016 [P] 修复 `tagPrefixLen` 局部变量 `max` 遮蔽内置函数（revive `redefines-builtin-id`），重命名为 `limit`
- [x] T017 [P] 门禁验证：`task check` / `task test-race` / `golangci-lint run ./...` 全绿
- [x] T018 [P] 文档同步：spec.md 状态 Draft → 验收完成；设计文档已落 `docs/superpowers/specs/`

**Checkpoint**: 全部门禁通过，特性可交付

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (T001)**: 无依赖
- **Foundational (T002-T003)**: 依赖 T001，阻塞 US1
- **US1 (T004-T015)**: 依赖 Foundational
- **Polish (T016-T018)**: 依赖 US1 完成

### Within Each User Story

- 测试（T004-T011）与实现（T012-T015）并行编写并验证
- 核心状态机（T003）先于收口/重置（T012-T014）与接入（T015）

### Parallel Opportunities

- 所有测试任务（T004-T011）标记 [P]，可并行
- 常量/字段（T002）与状态机（T003）先行，之后测试与收口实现可并行

---

## Notes

- 本特性为单包最小增量，状态机与测试集中在 `internal/chatstreamconv`
- 范围严格限定 c 路径流式正文，历史回灌与 a/r 路径未改动（FR-010）
- 验收门禁对应 spec.md SC-001..SC-005，全部满足
