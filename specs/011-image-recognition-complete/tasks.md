# Tasks: 图片识别三协议完全可用

**Input**: Design documents from `/specs/011-image-recognition-complete/`

**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/

**Tests**: 本仓库 AGENTS.md 要求测试贴近实现、表驱动，本 feature 的验收场景全部由
测试任务显式覆盖。

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## Path Conventions

- 单仓库 Go 服务：`internal/` 包、`docs/` 文档、`specs/011-image-recognition-complete/` 规格。

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: 建立 `internal/imagemapper` 包骨架（判定 + 脱敏）

- [x] T001 创建 `internal/imagemapper/decision.go`，定义 `Kind` 常量、`Decision` 结构与
  `Inspect` / `InspectParam` / `InspectContentParam` 判定入口（按 contracts/imagemapper.md）
- [x] T002 创建 `internal/imagemapper/sanitize.go`，实现 `SanitizeURL`（URL 抹 query/
  fragment；data URI 返回类型+字节数元数据）
- [x] T003 [P] 创建 `internal/imagemapper/decision_test.go`，表驱动覆盖 URL / data-URI /
  仅 file_id / 无 url 无 file_id / detail 提取（含 original）
- [x] T004 [P] 创建 `internal/imagemapper/sanitize_test.go`，覆盖带签名参数 URL 脱敏、
  data URI 元数据、不含凭据断言

**Checkpoint**: `go test ./internal/imagemapper/...` 通过，判定与脱敏独立可验证

---

## Phase 2: User Story 1 - 识别图片在三协议完整到达上游 (Priority: P1) 🎯 MVP

**Goal**: 用户消息与工具结果中的 URL / data-URI 图片在 a/c 路径映射到原生槽位、r 路径
透传，detail 按槽位取舍

**Independent Test**: `internal/convert` 与 `internal/chatconvert` 的图片映射测试 +
`docs/protocol-coverage.md` 状态与实现一致

### Tests for User Story 1

- [x] T005 [P] [US1] 在 `internal/convert/request_test.go` 补 a 路径 user 消息图片
  （URL / data-URI）→ image block 断言（detail 丢弃且图像本体保留）
- [x] T006 [P] [US1] 在 `internal/convert/request_test.go` 补 a 路径 tool 结果图片 →
  tool_result image block 断言
- [x] T007 [P] [US1] 在 `internal/chatconvert/request_test.go` 补 c 路径 user 消息图片
  （URL / data-URI）→ `image_url` part 带 detail 断言
- [x] T008 [P] [US1] 在 `internal/chatconvert/request_test.go` 补 c 路径 tool 结果图片 →
  聚合 user `image_url` part 断言
- [x] T009 [P] [US1] 在 `internal/backend/responses_test.go` 补 r 路径透传确认：含
  `input_image` 的请求体经 `PrepareUpstreamBody` 后不被改写（SC-001 保持原样）

### Implementation for User Story 1

- [x] T010 [US1] 在 `internal/chatconvert/request.go` 的 `ChatImageURL` 增加 `detail`
  字段（`omitempty`），`imagePart` 支持透传 detail
- [x] T011 [US1] 在 `internal/convert/request.go` 的 appendMessage input_image 分支改为
  调用 `imagemapper.InspectParam`：`KindMapped` 走 `imageBlock(Decision.URL)`
- [x] T012 [US1] 在 `internal/convert/request.go` 的 `toolResultImagePart` 改为调用
  `imagemapper.InspectContentParam`：`KindMapped` 走 `imageBlock(Decision.URL)`
- [x] T013 [US1] 在 `internal/chatconvert/request.go` 的 `inputImagePart` /
  `inputImageContentPart` 改为调用 `imagemapper.InspectParam` / `InspectContentParam`，
  `KindMapped` 构造带 detail 的 `image_url` part
- [x] T014 [US1] 更新 `docs/protocol-coverage.md`：user / tool 结果图片三路径
  `supported` 状态与 detail 槽位登记

**Checkpoint**: a/c 路径图片映射测试全绿，矩阵与实现一致

---

## Phase 3: User Story 2 - 无法无损映射时明确失败并换源 (Priority: P2)

**Goal**: 仅 file_id 图片与系统/开发者指令图片在 a/c 非透传源产生源级失败，r 透传
保留；禁止丢图发残缺请求

**Independent Test**: a/c 路径对 file_id 与 system/developer 图片返回 error 的断言

### Tests for User Story 2

- [x] T015 [P] [US2] 在 `internal/convert/request_test.go` 补 a 路径 user 消息
  file_id 图片 → error（源级失败）断言
- [x] T016 [P] [US2] 在 `internal/convert/request_test.go` 补 a 路径 tool 结果 file_id
  图片 → error 断言
- [x] T017 [P] [US2] 在 `internal/convert/request_test.go` 补 a 路径 system/developer
  图片 → error 断言
- [x] T018 [P] [US2] 在 `internal/chatconvert/request_test.go` 补 c 路径 system/developer
  图片 → error 断言
- [x] T019 [P] [US2] 在 `internal/convert/request_test.go` 与
  `internal/chatconvert/request_test.go` 补转换 error 文本断言：包含源/原因，可归因且
  不产生成功假象（FR-005 / SC-005）

### Implementation for User Story 2

- [x] T020 [US2] 在 `internal/convert/request.go` 的 appendMessage input_image 分支：
  `KindFileID` / `KindMalformed` 返回 `fmt.Errorf` 源级失败（替换原 WARN 丢弃）
- [x] T021 [US2] 在 `internal/convert/request.go` 的 `toolResultImagePart`：
  `KindFileID` / `KindMalformed` 返回 error（替换原 WARN 丢弃）
- [x] T022 [US2] 在 `internal/convert/request.go` 的 appendMessage system/developer 分支：
  发现 image block 返回 error（Anthropic system 仅文本，不再 WARN 后发残缺指令）
- [x] T023 [US2] 在 `internal/chatconvert/request.go` 的 `convertMessageRole`
  system/developer 分支：含图片 parts 时返回 error（Chat system 仅文本 parts）
- [x] T024 [US2] 更新 `docs/protocol-coverage.md`：file_id 与 system/developer 图片
  三路径登记（a/c 源级失败、r 透传）

**Checkpoint**: file_id / system 图片在 a/c 路径无残缺请求发出，测试全绿

---

## Phase 4: User Story 3 - 图片相关观测不泄露且可归因 (Priority: P3)

**Goal**: 图片 URL 日志脱敏、data URI 只记元数据、错误可归因

**Independent Test**: `internal/imagemapper/sanitize_test.go` 与 a/c 路径改造后日志断言

### Tests for User Story 3

- [x] T025 [P] [US3] 在 `internal/imagemapper/sanitize_test.go` 补 data URI 元数据
  （类型+字节数、不含本体）断言；T004 负责 URL 基础地址断言，两者分工明确无重复
- [x] T026 [P] [US3] 在 `internal/convert/request_test.go` 补 a 路径日志脱敏断言：
  file_id 错误上下文不含完整图片地址

### Implementation for User Story 3

- [x] T027 [US3] a/c 路径错误与日志中图片引用统一经 `imagemapper.SanitizeURL` 后记录
  （`internal/convert/request.go`、`internal/chatconvert/request.go`）
- [x] T028 [US3] 核对 `docs/protocol-coverage.md` 中图片相关观测说明（脱敏、日志
  不含凭据）与实现一致

**Checkpoint**: 日志脱敏断言通过，矩阵观测说明与实现一致

---

## Phase 5: Polish & Cross-Cutting Concerns

**Purpose**: 全量验证与文档收敛

- [x] T029 运行 `task check`（fmt + vet + 全部测试）与 `task test-race`，全绿
- [x] T030 [P] 按 quickstart.md 逐条核对验收场景（单元测试 + 可选端到端）
- [x] T031 更新 `specs/011-image-recognition-complete/` 下 plan/quickstart 若有与
  实现不符的细节（矩阵、路径、语义）

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies - can start immediately
- **User Story 1 (Phase 2)**: 依赖 Phase 1（imagemapper 判定可用）
- **User Story 2 (Phase 3)**: 依赖 Phase 1 + Phase 2（共享判定入口与改造点相邻）
- **User Story 3 (Phase 4)**: 依赖 Phase 1（SanitizeURL）
- **Polish (Phase 5)**: 依赖全部用户故事完成

### Within Each User Story

- Tests 先写（TDD 语义），确保 FAIL 后实现；实现后再全绿
- 同一文件（`internal/convert/request.go`、`internal/chatconvert/request.go`）的任务
  顺序执行，避免并发冲突

### Parallel Opportunities

- Phase 1 中 T003 / T004 可并行
- 各用户故事的测试任务 [P] 可并行
- `internal/imagemapper/`、`internal/convert/`、`internal/chatconvert/` 三包改造在
  判定入口就绪后可并行推进

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. 完成 Phase 1（imagemapper 骨架）
2. 完成 Phase 2（US1：识别图片三协议完整映射）
3. **STOP and VALIDATE**: a/c 图片映射测试全绿 + `task test`

### Incremental Delivery

1. Setup → 判定/脱敏可用
2. US1 → 图片完整到达上游（MVP）
3. US2 → 源级失败与换源
4. US3 → 观测脱敏
5. Polish → 全量门禁

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to specific user story for traceability
- 所有 wire 文本保持英文；中文只用于注释与文档
- 改完每个 Phase 后运行 `task test`，全部完成后运行 `task check` + `task test-race`
