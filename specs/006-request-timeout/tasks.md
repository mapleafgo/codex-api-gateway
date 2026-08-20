# Tasks: 单请求最大超时时长配置

**Input**: Design documents from `/specs/006-request-timeout/`

**Prerequisites**: plan.md、spec.md、research.md、data-model.md、contracts/contract.md

**Tests**: 按规格 SC-005/SC-006 与项目章程 VII，测试作为每阶段必含任务（实现与
测试同目录、表驱动、并发改动跑 race）。

**Organization**: 任务按用户故事组织，可独立实现与验证。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无依赖）
- **[Story]**: 所属用户故事（US1/US2/US3）
- 描述含精确文件路径

## Phase 1: Setup

不适用：本仓库为既有 Go 服务，无需项目初始化；直接进入 Foundational 阶段。

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: 超时判因哨兵与配置管道，阻塞全部用户故事

**⚠️ CRITICAL**: 本阶段完成前不得开始任何用户故事实现

- [x] T001 [P] 在 internal/backend/helpers.go 新增 ErrUpstreamTimeout 哨兵错误、IsServerTimeout(ctx, err)（errors.Is(context.Cause(ctx), sentinel)），并让 IsClientCanceled / classifyOutcome 在服务端超时时返回/归类为 failed（非 canceled）；同步补 internal/backend/helpers_test.go 表驱动测试
- [x] T002 [P] 在 internal/config/config.go 为 BreakerCfg 新增 RequestTimeout Duration（koanf/yaml request_timeout）：applyDefaults 缺失/0 → 120s、validateBreakerNonNegative 负值拒绝、BreakerFor 支持 per-source 覆盖（0 继承）、applyEnvOverrides 白名单加入 breaker.request_timeout
- [x] T003 [P] 在 internal/config/config_test.go 补测试：缺省/0 → 120s、负值报 "must be >= 0"、全局与 per-source 覆盖/继承、env 覆盖 CODEX_API_GATEWAY_BREAKER__REQUEST_TIMEOUT
- [x] T004 [P] 在 internal/admin/admin.go 的 breakerView / adminConfigInput 增加 request_timeout 字段并补齐 GET/POST/roundtrip 映射；在 internal/admin/assets/index.html 断路器表单增加全局与每源 request_timeout 输入（含中英文 label）；internal/admin/admin_test.go 增加字段断言
- [x] T005 [P] 在 config.example.yaml 增加全局 breaker.request_timeout: 120s 与 per-source 覆盖示例注释（0/缺省=默认、负值拒绝语义）

**Checkpoint**: 配置与哨兵就绪，用户故事可开始实现

---

## Phase 3: User Story 1 - 单笔源请求超时兜底（未出流换源）(Priority: P1) 🎯 MVP

**Goal**: 每笔源请求受总时长上限约束，未出内容到点时按失败计入熔断并继续换源

**Independent Test**: 配置短超时（如 3s）+ 挂起源，验证该源约 3s 失败、请求换到
备用源并最终收到合法终态

### Implementation for User Story 1

- [x] T006 [US1] 在 internal/scheduler/scheduler.go 的 trySourceGeneric 叠加总时长定时器：在 fbCtx 之下派生 context.WithTimeoutCause(fbCtx, BreakerFor(src).RequestTimeout, backend.ErrUpstreamTimeout) 传给 Execute；未锁定超时走既有 RecordFailure+failover 路径，日志带 reason=timeout
- [x] T007 [US1] 在 internal/server/server.go 的 handleResponses 增加服务端超时归因：backend.IsServerTimeout(execErr) 时 status=failed、code=504、日志 reason=timeout 且不得落入 clientCanceled 分支
- [x] T008 [US1] 补 internal/scheduler 与 internal/server 表驱动测试：未出流短超时→失败+换源→合法终态；per-source 覆盖值生效（覆盖源按覆盖值、未覆盖源按全局）
- [x] T009 [US1] 在 internal/server/integration_test.go 增加集成场景：首个挂起源在约配置时长失败，备用源成功完成，客户端不无限等待

**Checkpoint**: US1 独立可用：任何源不可能无数据挂起超过配置时长

---

## Phase 4: User Story 2 - 超时与客户端取消可区分 (Priority: P2)

**Goal**: 服务端单笔超时与客户端取消在终态、日志、指标中可区分

**Independent Test**: 分别触发超时与取消，断言终态/code/reason 键互不相同

### Implementation for User Story 2

- [x] T010 [US2] 在 internal/server/server.go 补齐观测：超时日志键 reason=timeout + metrics RequestEvent{Code:504, Error 含 timeout}；客户端取消保持 499/canceled；确认 onUpstream 上报 status 不被记成 canceled
- [x] T011 [US2] 在 internal/server/server_test.go 增补区分测试：同场景超时（504/reason=timeout）与取消（499/canceled）断言 metrics 与结构化日志

**Checkpoint**: US1+US2 独立可用：超时根因在观测面不可混淆

---

## Phase 5: User Story 3 - 已出流的单笔请求同样被兜底 (Priority: P3)

**Goal**: 已向客户端写出事件后到点，保留已收内容、以 failed 终态收口、源锁定不换源

**Independent Test**: 上游出流超过配置时长，验证约到点收到 response.failed 收尾终态；r 路径同样有终态

### Implementation for User Story 3

- [x] T012 [US3] 在 internal/backend/anthropic.go 与 chat.go 确认/修正已锁定失败路径：服务端超时（非客户端取消）时复用既有收尾安全网产出 failed 终态（anthropic 补 response.failed、chat conv.Fail）；internal/backend/*_test.go 增补超时分支
- [x] T013 [US3] 在 internal/server/server.go 的 onEvent 回调记录"流内是否已出现终态事件"标志；IsServerTimeout 且流内无终态时补发 response.failed（扩展现有 evCount==0 补发分支），r 透传不代补上游事件、网关自中止场景由 server 兜底
- [x] T014 [US3] 在 internal/server/integration_test.go 增补已出流超时场景：a/c 后端流内 failed 终态；r 后端终态补发；源锁定断言（不再收到第二源事件）

**Checkpoint**: US1+US2+US3 全部独立可用

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: 文档、示例与全量门禁

- [x] T015 [P] 更新 README.md breaker 参数表（新增 request_timeout 行与语义说明）与 docs/protocol-coverage.md（2026-08-20 变更记录：单笔总时长 vs 首字节超时职责差异）
- [x] T016 [P] 按 specs/006-request-timeout/quickstart.md 用 task run + curl 抽查关键场景（短超时换源、已出流 failed 收尾、每源覆盖、管理页字段回显）
- [x] T017 运行全量门禁并修复：task fmt-check、go vet ./...、go test ./...、task test-race（共享状态/goroutine 相关改动覆盖）

**Checkpoint**: 全量验收通过，可进入 $speckit-converge

---

## Dependencies & Execution Order

### Phase Dependencies

- **Foundational (Phase 2)**: 无前置依赖，先于所有用户故事
- **US1 (Phase 3)**: 依赖 T001/T002（哨兵 + 配置）
- **US2 (Phase 4)**: 依赖 Foundational 与 US1 的 server 归因
- **US3 (Phase 5)**: 依赖 Foundational 与 US1 的 scheduler 定时器
- **Polish (Phase 6)**: 依赖全部用户故事

### User Story Dependencies

- **US1 (P1)**: Foundational 后即可开始，无跨故事依赖
- **US2 (P2)**: Foundational 后即可开始；45 用 US1 的 server 归因基座
- **US3 (P3)**: Foundational 后即可开始；用 US1 定时器与 US2 判因

### Within Each User Story

- 实现先行、测试紧随（同目录），服务/调度逻辑先于收口逻辑
- 故事完成后再进入下一优先级

### Parallel Opportunities

- T001-T005（Foundational）全部 [P]，无文件冲突
- T015/T016（Polish）[P]
- US2 与 US3 的测试可在各自实现后立即运行

## 实现策略

### MVP First (US1 Only)

1. T001-T005（Foundational）→ T006-T008（US1）→ T009 集成
2. STOP 并验证：挂起源无法拖垮请求超过配置时长

### Incremental Delivery

1. Foundational 完成后 US1（MVP）即独立成立
2. US2 补齐可观测区分（无行为回归）
3. US3 补齐已出流收尾（无行为回归）
4. Polish 收口文档与门禁

## Notes

- [P] 任务 = 不同文件、无依赖
- [Story] 标签映射规格用户故事，保证可追溯
- 每一用户故事独立可完成、可测试
- 与 first_byte_timeout 的差异语义（总时长不被首事件停止）已在 contracts/contract.md 固化，实现时以策略差异落实

## Phase 7: Convergence

**Purpose**: 收敛评估（$speckit-converge）发现的剩余缺口：管理页每源输入、指标可区分
断言、已出流源锁定断言。

- [x] T018 在 internal/admin/assets/index.html 每源卡片（src-card-*）增加 request_timeout 输入，绑定 src.breaker.request_timeout，复用 reqTimeout/reqTimeoutHint 中英文 label，保证 GET/POST 保存后回显 per T004（partial）
- [x] T019 在 internal/server/integration_test.go 增加超时与取消的 metrics 区分断言：超时场景 client 记录 Status=failed、Code=504，取消场景 Status=canceled、Code=499 per FR-006 / SC-003（partial）
- [x] T020 强化 TestIntegrationMidStreamTimeoutFailedTerminal：增加第二正常源并断言其零命中、流内不再出现 response.completed，证明已出流超时后源锁定不换源 per T014 / US3-AC1（partial）
