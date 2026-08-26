# Tasks: 管理页 Copilot Device Flow 授权

**Input**: Design documents from `specs/009-admin-copilot-device-flow/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/admin-copilot-auth.md

## Phase 1: Setup（共享基础）

**Purpose**: 建立可独立验证的 L1 Device Flow 客户端与测试骨架

- [X] T001 在 `internal/copilotclient/auth.go` 新增 DeviceFlow 类型、`StartDeviceFlow` 与 `PollDeviceFlow`（Zed 参数与 slow down 语义）
- [X] T002 在 `internal/copilotclient/auth_test.go` 用 httptest 表驱动覆盖 pending、slow down、成功、过期、拒绝、未知错误
- [X] T003 在 `internal/admin/copilot_auth.go` 新增会话管理器与公开状态模型，状态迁移对齐 data-model.md
- [X] T004 在 `internal/admin/copilot_auth_test.go` 覆盖状态迁移与凭据不回显

**Checkpoint**: Device Flow 客户端和会话状态机可独立测试

---

## Phase 2: Foundational（阻塞性前置）

**Purpose**: 打通管理 API 与配置写盘链路

- [X] T005 在 `internal/admin/admin.go` 挂载 `/admin/api/copilot/auth/start`、`/admin/api/copilot/auth/status`、`/admin/api/copilot/auth/cancel` 并接入 recoverMiddleware
- [X] T006 在 `internal/admin/copilot_auth.go` 实现目标源校验：name 必填、backend_type 为 `g`、github_token 必须为空
- [X] T007 在 `internal/admin/copilot_auth.go` 实现成功落盘：基于 holder 快照更新或新增源，复用 `writeConfigYAMLLocked` 原子写
- [X] T008 在 `internal/admin/copilot_auth_test.go` 覆盖新增源、更新源、非 g 名称、写盘失败与 token 不透出

**Checkpoint**: 管理 API 可启动 Device Flow 并在 mock 成功时保存源

---

## Phase 3: User Story 1 - 为新建 Copilot 源完成登录授权 (Priority: P1) 🎯 MVP

**Goal**: 管理员填写源草稿、发起 Github Device Flow、展示 user code、等待授权后源被保存

**Independent Test**: start 接口在 mock user code 成功后返回 authorized；管理页出现新 Copilot 源

### Implementation for User Story 1

- [X] T009 [US1] 在 `internal/admin/copilot_auth.go` 实现 start 处理：校验草稿、启动活跃会话、请求 device code、后台轮询
- [X] T010 [US1] 在 `internal/admin/copilot_auth.go` 实现 status 处理：返回公开状态和 interval_seconds
- [X] T011 [US1] 在 `internal/admin/assets/index.html` 新增 Copilot 源表单“ 使用 GitHub 授权”入口并显示 user code/verification_uri
- [X] T012 [US1] 在 `internal/admin/assets/index.html` 增加按 interval 轮询 status 的前端逻辑

**Checkpoint**: 全新 Copilot 授权：弹窗展示 user code, 授权成功后新增源

---

## Phase 4: User Story 2 - 为已有 Copilot 源重新授权 (Priority: P2)

**Goal**: 已有 g 源支持重新 Device Flow 授权，token 更新但源身份不变

**Independent Test**: 对已存在 Copilot 源 start 并在 mock 成功，现有源被原地更新且数量不变

### Implementation for User Story 2

- [X] T013 [US2] 在 `internal/admin/copilot_auth.go` 支持目标同名 g 源更新，落盘前复查同名源类型
- [X] T014 [US2] 在 `internal/admin/copilot_auth_test.go` 新增同名 g 源更新测试
- [X] T015 [US2] 在 `internal/admin/assets/index.html` 为已有 Copilot 源卡片加入“授权”动作并复用同一样板

**Checkpoint**: 已有源通过授权更新 token，且源数量不变化

---

## Phase 5: User Story 3 - 取消、失败与凭据保护 (Priority: P3)

**Goal**: 支持取消、错误终态、且 token 不进入日志、响应

**Independent Test**: 取消与过期/拒绝 mock 返回终态，且 gateway.log/HTTP 响应无 token

### Implementation for User Story 3

- [X] T016 [US3] 在 `internal/admin/copilot_auth.go` 实现 cancel 终止轮询、saving 阶段返回冲突
- [X] T017 [US3] 在 `internal/admin/copilot_auth.go` 统一安全化错误：仅公开不含 token 的终态
- [X] T018 [US3] 在 `internal/admin/copilot_auth_test.go` 新增取消、冲突、失败/过期/慢速、并发重复初始化与 token 泄漏检查（run `task test-race`）
- [X] T019 [US3] 在 `internal/admin/assets/index.html` 加入取消按钮和终态错误提示

**Checkpoint**: 所有授权失败分支稳定、并发唯一会话、凭据不可泄露

---

## Phase N: Polish & Cross-Cutting Concerns

**Purpose**: 文档与门禁收尾

- [X] T020 更新 `README.md` 增加管理页 Copilot 授权说明
- [X] T021 更新 `internal/admin/admin_test.go` 的 Dashboard 标签断言及 `docs/protocol-coverage.md` 的 Copilot 管理授权说明（如适用）
- [X] T022 运行 `task fmt`、`task check` 与 `task test-race` 并确保通过

---

## Dependencies & Execution Order

### Phase Dependencies

- Phase 1（T001-T004）必须先完成，auth client 与测试骨架就绪
- Phase 2（T005-T008）在 Phase 1 之后，打通管理 API 与写盘
- US1（Phase 3）依赖 Phase 2
- US2（Phase 4）依赖 Phase 3 的 auth 会话（含会话保存路径）
- US3（Phase 5）依赖 Phase 1-3 的会话能力
- Polish（T020-T022）在所有用户故事完成后执行

### User Story Dependencies

- US1 离基础 + 能 start/status + 保存新源
- US2 沿用 US1 的 start/save，新增已有源更新分支
- US3 沿用全流程，新增 cancel / 错误分支 / 并发/安全测试

### Parallel Opportunities

- T001/T003 可以并行（不同文件）
- T002 与 T004 可并行（各自测试文件）
- T005/T006 在同一文件必须串行
- Phase 3/4/5 UI 可并行但是顺序实现以保持最小增量

## Implementation Strategy

1. 完成 Phase 1-2，DeviceFlow 与 admin 基础协议可独立测试
2. 交付 MVP（US1），验证新源授权
3. 增量加 US2（重新授权）
4. 增量完成 US3（取消/安全），跑 race
5. Polish 与门禁
