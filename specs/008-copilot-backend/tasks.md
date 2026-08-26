---

description: "Task list for GitHub Copilot 原生后端接入"
---

# Tasks: GitHub Copilot 原生后端接入

**Input**: Design documents from `/specs/008-copilot-backend/`

**Prerequisites**: plan.md (required), spec.md (required), research.md, data-model.md, contracts/copilot-api.md

**Tests**: 项目约定要求测试靠近实现（同目录 `*_test.go`），每个任务附带对应测试。

**Organization**: 按 spec.md 的三个 User Story 组织，每个 Story 独立可测。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件，无依赖）
- **[Story]**: 所属 User Story
- 包含确切文件路径

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: 配置层基础设施，所有 User Story 的前置

- [x] T001 [P] 在 `internal/config/config.go` 新增 `BackendGitHubCopilot = "g"` 常量，并在 `NormalizeBackendType` 的 switch 中新增 `case BackendGitHubCopilot` 分支返回合法值
- [x] T002 [P] 在 `internal/config/config.go` 的 `Source` 结构体新增 `GithubToken string` 字段（koanf tag `github_token`，yaml tag `github_token,omitempty`）
- [x] T003 在 `internal/config/backend_type_test.go` 新增表驱动测试：`NormalizeBackendType("g")` 返回 `"g", nil`；`Source.GithubToken` 的 YAML 序列化/反序列化
- [x] T004 [P] 在 `config.example.yaml` 新增 `g` 源示例（注释掉的 copilot 源，含 `backend_type: g`、`github_token: ${COPILOT_GITHUB_TOKEN}`、`default_model`）

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: `g` 后端的核心骨架与调度接入，所有 User Story 的共享基础

- [x] T005 [P] 在 `internal/copilotclient/endpoint.go` 实现 `discoverAPIEndpoint(ctx, httpClient, githubToken) (string, error)`：向 `api.github.com/graphql` 发 GraphQL 查询获取 `copilotEndpoints.api`，失败返回 `https://api.githubcopilot.com` + WARN 日志
- [x] T006 [P] 在 `internal/copilotclient/endpoint_test.go` 用 `httptest.Server` 测试：GraphQL 成功返回 endpoint；GraphQL 失败回退默认 endpoint；网络错误回退默认 endpoint
- [x] T007 [P] 在 `internal/copilotclient/models.go` 定义 `ModelInfo` 结构体（`ID`、`SupportedEndpoints []string`）和 `modelCache`（`http.Client`、`singleflight.Group`、`sync.RWMutex`、`models map[string]*ModelInfo`、`cachedAt`、`ttl`、`valid`）
- [x] T008 [P] 在 `internal/copilotclient/models.go` 实现 `(*modelCache).Get(ctx, endpoint, token) (map[string]*ModelInfo, error)`：目录级缓存读取，miss 时用 singleflight 去重 fetch
- [x] T009 [P] 在 `internal/copilotclient/models.go` 实现 `(*modelCache).fetch`：GET `<endpoint>/models`，解析响应，按筛选条件（`model_picker_enabled == true && capabilities.type == "chat" && (policy == nil || policy.state == "enabled")`）过滤，返回 `map[string]*ModelInfo`
- [x] T010 [P] 在 `internal/copilotclient/models_test.go` 用 `httptest.Server` 测试：正常返回并筛选；`model_picker_enabled: false` 被过滤；`capabilities.type != "chat"` 被过滤；`policy.state == "pending"` 被过滤；`restricted_to` 不影响筛选；缓存 TTL 内不重复请求；并发请求只触发一次 fetch
- [x] T011 [P] 在 `internal/backend/copilot.go` 定义 `CopilotBackend` 结构体（持有 `Responses *ResponsesBackend`、`Anthropic *AnthropicBackend`、`Chat *ChatBackend`）和 `NewCopilot(responses, anthropic, chat)` 构造函数
- [x] T012 在 `internal/copilotclient/client.go` 实现 `Headers() map[string]string`：返回 Zed 风格 header（`Editor-Version: Zed/0.1.0`、`X-GitHub-Api-Version: 2025-10-01`、`Authorization` 不在此设——由各 client 的 apiKey 参数注入）
- [x] T013 [P] 在 `internal/backend/copilot.go` 实现 `routeByEndpoints(info *copilotclient.ModelInfo) string`：按优先级 `/responses` → "r"、`/v1/messages` → "a"、`/chat/completions` → "c"，info 为 nil 或无匹配返回 "r"
- [x] T014 [P] 在 `internal/backend/copilot_test.go` 测试 `routeByEndpoints`：支持 `/responses` 返回 "r"；不支持 `/responses` 但支持 `/v1/messages` 返回 "a"；仅 `/chat/completions` 返回 "c"；同时支持多个返回 "r"；nil 返回 "r"；空 SupportedEndpoints 返回 "r"
- [x] T015 在 `internal/scheduler/scheduler.go` 的 `Scheduler` 结构体新增 `copilotBackend *backend.CopilotBackend` 字段，在 `New()` 中用独立 r/a/c Backend 实例组装
- [x] T016 在 `internal/scheduler/scheduler.go` 的 `backendFor()` 新增 `case config.BackendGitHubCopilot: return s.copilotBackend`；在 `ListUpstreamModels()` 新增对应的 `g` 分支（经 Copilot endpoint + OAuth token 拉取筛选目录）

**Checkpoint**: 基础架构就绪——config 识别 `g`、Backend 骨架与路由逻辑存在、scheduler 能分发到 `g`

---

## Phase 3: User Story 1 - 按模型能力自动路由 (Priority: P1) 🎯 MVP

**Goal**: `g` 源能根据模型 `supported_endpoints` 按 r > a > c 优先级委托已有后端转发流式请求

**Independent Test**: 配一个 `g` 源，发请求到支持 `/responses` 的模型，验证走 r 路径成功透传 SSE；发请求到只支持 `/v1/messages` 的模型，验证走 a 路径

- [x] T017 [P] [US1] 在 `internal/backend/copilot.go` 实现 `CopilotBackend.Execute`：解析 rawBody 取 model → resolveModel → 解析生效 endpoint（显式 `base_url` 或惰性 GraphQL 发现）→ 从模型缓存取 modelInfo → routeByEndpoints → 构造修改过的 source 副本（BaseURL=生效 endpoint、APIKey=githubToken、Headers=Zed header）→ 委托对应 Backend.Execute
- [x] T018 [US1] 在 `internal/copilotclient/client.go` 实现 per-source 惰性状态管理：用互斥锁保护的 map 按 source name 缓存 `*sourceState`（含 endpoint 发现结果和模型缓存实例），`github_token` 或显式 endpoint 变更时整体重建
- [x] T019 [P] [US1] 在 `internal/backend/copilot_test.go` 用 `httptest.Server` 测试 Execute 端到端：mock Copilot `/models` 返回模型支持 `/responses` → 验证请求打到 `/responses` 端点；mock 返回只支持 `/v1/messages` → 验证走 Anthropic 路径；mock 返回只支持 `/chat/completions` → 验证走 Chat 路径
- [x] T020 [US1] 在 `internal/backend/copilot_test.go` 测试 Execute 的上游 header：验证请求携带 `Editor-Version: Zed/0.1.0` 和 `X-GitHub-Api-Version: 2025-10-01`，Messages 路径不携带 `x-api-key`/`anthropic-version`，所有路径不携带 `Copilot-Integration-Id`
- [x] T021 [US1] 在 `internal/backend/copilot_test.go` 测试 model_map 和 default_model：验证委托前 model 被正确映射

**Checkpoint**: User Story 1 可独立验证——`g` 源能按模型路由到 r/a/c 并透传流式 SSE

---

## Phase 4: User Story 2 - 模型目录缓存与筛选 (Priority: P2)

**Goal**: `/models` 拉取带 TTL 缓存和筛选，作为路由决策的依据

**Independent Test**: 首个请求触发模型目录拉取并筛选；TTL 过期后的下一个请求触发重新拉取

- [x] T022 [US2] 在 `internal/copilotclient/models.go` 完善 `modelCache` 的 TTL 过期逻辑：`Get` 检查 `time.Since(cachedAt) > ttl` 时标记 invalid 触发重新 fetch
- [x] T023 [P] [US2] 在 `internal/copilotclient/models_test.go` 测试 TTL 过期：首次 fetch 后等待短 TTL 过期，第二次 Get 触发新 fetch；TTL 内第二次 Get 不触发 fetch
- [x] T024 [US2] 在 `internal/copilotclient/models_test.go` 测试拉取失败的回退：`/models` 返回 500 时 Get 返回 error，`internal/backend/copilot_test.go` 验证 Execute 层回退到默认路由 "r"

**Checkpoint**: 模型缓存与筛选完整——带 TTL、singleflight 去重、筛选条件正确、失败回退

---

## Phase 5: User Story 3 - GraphQL Endpoint 发现 (Priority: P3)

**Goal**: 首个请求通过 GraphQL 惰性发现 Copilot API endpoint，失败回退默认地址

**Independent Test**: 观察日志有 GraphQL 发现记录；模拟失败验证回退

- [x] T025 [US3] 在 `internal/copilotclient/client.go` 集成 `discoverAPIEndpoint`：首次请求时惰性发现，结果缓存在 per-source state 中，后续请求复用
- [x] T026 [P] [US3] 在 `internal/copilotclient/endpoint_test.go` 测试发现的缓存：同一 source 的第二次解析不重复 GraphQL 查询；不同 source 各自独立发现
- [x] T027 [US3] 在 `internal/copilotclient/endpoint_test.go` 测试端到端发现集成：mock GraphQL 返回自定义 endpoint → 验证 Directory 使用该 endpoint；mock GraphQL 失败 → 验证回退到注入的 fallback endpoint

**Checkpoint**: Endpoint 发现完整——GraphQL 查询成功用结果，失败回退默认，per-source 缓存

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: 文档、配置闭环、集成验证

- [x] T028 [P] 在 `config.example.yaml` 完善 `g` 源注释示例（含 `model_map`、`supports_web_search`、`breaker` 等可选字段说明）
- [x] T029 [P] 更新 `docs/protocol-coverage.md`：新增 `g` 路径的协议覆盖矩阵条目（认证方式、endpoint 发现、模型筛选、路由优先级）
- [x] T030 在 `internal/scheduler/scheduler_test.go` 新增 `g` 源的调度测试：`backendFor` 对 `g` 源返回 `copilotBackend`；`g` 源参与优先级和故障转移
- [x] T031 运行 `task check`（gofmt + go vet + 全部测试）确保通过
- [x] T032 运行 `task test-race` 确保模型缓存和 per-source 状态的并发安全
- [x] T036 将 endpoint/model 目录能力下沉到 `internal/copilotclient`，admin 旁路只持有 L1 client，并新增架构测试禁止 admin import backend/scheduler/breaker/server
- [x] T037 登记并验证 `contextTier` 不主动注入：未传不补 `long_context`、已传 r 透传、a/c 按矩阵处理 per FR-011
- [x] T038 登记并验证 GitHub token 禁止出现在日志、metrics、管理页响应的观测断言 per FR-014
- [x] T039 登记并验证管理页 `g` 源 `github_token` 不回显、空值按同名源保留、非空按新值覆盖 per FR-017

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: 无依赖，立即开始。T001-T004 可并行
- **Foundational (Phase 2)**: 依赖 Phase 1。T005-T016 内部大部分可并行
- **User Story 1 (Phase 3)**: 依赖 Phase 2 的 T011（CopilotBackend 骨架）、T013（routeByEndpoints）、T015-T016（scheduler 接入）
- **User Story 2 (Phase 4)**: 依赖 Phase 2 的 T007-T010（modelCache）。可与 US1/US3 并行
- **User Story 3 (Phase 5)**: 依赖 Phase 2 的 T005-T006（endpoint 发现）。可与 US1/US2 并行
- **Polish (Phase 6)**: 依赖所有 User Story 完成

### Within Each User Story

- 数据结构/工具函数 → 核心逻辑 → Execute 集成 → 测试

### Parallel Opportunities

- Phase 1 全部 [P]
- Phase 2 的 T005-T014 大部分 [P]（不同文件）
- Phase 3/4/5 的测试任务 [P]

---

## Implementation Strategy

### MVP First (User Story 1)

1. Phase 1: config 常量与字段
2. Phase 2: Backend 骨架、路由逻辑、scheduler 接入
3. Phase 3: Execute 实现 + 端到端测试
4. STOP and VALIDATE: 配一个 `g` 源，发请求验证路由分发

### Incremental Delivery

1. Setup + Foundational → 骨架就绪
2. US1 → MVP 可用
3. US2 → 模型筛选完善
4. US3 → Endpoint 发现完善
5. Polish → 文档与门禁

---

## Notes

- 测试靠近实现（同目录 `*_test.go`），表驱动 + httptest mock
- 涉及缓存和 per-source 状态的改动必须通过 `task test-race`
- 凭据（github_token）禁止出现在日志、metrics 或测试输出中
- `contextTier` 不主动注入——r 路径透传已传字段，a/c 按各自矩阵处理

## Phase 7: Convergence

- [x] T033 CRITICAL 修订项目章程的 backend 类型清单与协议路径表述，纳入 `g` 并同步 Sync Impact Report、版本与日期 per Constitution I/II (contradicts)
- [x] T034 为管理页 `g` 源新增、GitHub token 空值保存保留与指标标签执行真实浏览器或等效端到端验收 per Constitution VII (partial)
- [x] T035 对齐 tasks/plan 中模型缓存 `Get` 契约、测试文件路径、依赖状态与实际变更面 per tasks.md T003/T008、plan.md (contradicts)
