# Feature Specification: GitHub Copilot 原生后端接入

**Feature Branch**: `008-copilot-backend`

**Created**: 2026-08-25

**Status**: Draft

**Input**: "我想将 copilot2api 的 copilot 源接入过来" + 确认：认证走 config.yaml source 字段、backend_type 用 `g`、按模型路由到已有 r/a/c 协议转换（优先级 r > a > c）、仅流式、模型筛选与认证方式参照 Zed 实现

## User Scenarios & Testing *(mandatory)*

### User Story 1 - 配置 Copilot 源并按模型能力自动路由 (Priority: P1)

运维者在 `config.yaml` 声明一个 `backend_type: g` 的源，填入 GitHub OAuth token。缺省 `base_url` 时，网关用该 token 通过 GraphQL 查询发现 Copilot API endpoint；显式 `base_url` 直接作为固定 endpoint。网关拉取模型目录并按筛选条件过滤出可用模型，在收到请求时按模型 `supported_endpoints` 以优先级 r > a > c 选择已有的协议转换路径（Responses 透传 / Anthropic Messages / Chat Completions）转发到 Copilot 上游。

**Why this priority**: 这是整个功能的核心价值。Copilot 上不同模型支持不同协议端点（有的支持 `/responses`，有的原生 `/v1/messages`，有的只支持 `/chat/completions`），网关必须自动选择正确的协议路径才能让所有 Copilot 模型可用。网关已有的三条转换路径（r/a/c）是现成的，新增工作集中在认证发现、模型筛选与路由分发。

**Independent Test**: 配好一个 `g` 源，向网关请求一个支持 `/responses` 的模型（如 `gpt-5.3-codex`），验证走 `r` 路径成功；再请求一个只支持 `/v1/messages` 的模型（如 Claude 系列），验证走 `a` 路径成功。

**Acceptance Scenarios**:

1. **Given** config.yaml 有一个 `backend_type: g`、`github_token` 有效的源，且模型目录显示模型 X 支持 `/responses`，**When** 客户端请求模型 X，**Then** 网关走 `r`（Responses 透传）路径，向 Copilot 上游 `/responses` 端点转发并透传 SSE
2. **Given** 模型 Y 不支持 `/responses` 但支持 `/v1/messages`，**When** 客户端请求模型 Y，**Then** 网关走 `a`（Anthropic Messages）路径，把 Responses 请求转为 Anthropic Messages 格式发给 Copilot `/v1/messages` 端点，再把上游 Anthropic SSE 转回 Responses SSE
3. **Given** 模型 Z 仅支持 `/chat/completions`，**When** 客户端请求模型 Z，**Then** 网关走 `c`（Chat Completions）路径，把 Responses 请求转为 Chat 格式发给 Copilot `/chat/completions` 端点，再把上游 Chat SSE 转回 Responses SSE
4. **Given** 模型 W 同时支持 `/responses` 和 `/v1/messages`，**When** 客户端请求模型 W，**Then** 网关按优先级 r > a > c 选择 `r` 路径

---

### User Story 2 - Copilot 模型目录缓存与筛选 (Priority: P2)

网关在缓存未命中时从 Copilot `/models` 端点拉取模型目录，按 Zed 的筛选条件过滤出可用模型，解析每个模型的 `supported_endpoints`，缓存约 5 分钟作为路由决策依据。缓存未命中或拉取失败时，回退到默认路由（`r` 优先），让上游自行判定。

**Why this priority**: 协议探测和模型筛选是路由正确性的前提，必须与请求转发同步可用。

**Independent Test**: 发起首个请求后观察日志中有模型目录拉取记录；验证被过滤掉的模型（如 `model_picker_enabled: false`）不在可用列表中；修改缓存 TTL 让其过期后再次请求，验证触发重新拉取。

**Acceptance Scenarios**:

1. **Given** 网关首次启动或缓存过期，**When** 第一个请求到达，**Then** 网关从 Copilot `/models` 拉取目录，按筛选条件过滤可用模型，解析 `supported_endpoints` 并缓存
2. **Given** 缓存有效期内，**When** 请求到达，**Then** 直接使用缓存的模型能力信息做路由，不重复拉取
3. **Given** 模型目录拉取失败，**When** 请求到达，**Then** 网关回退到默认路由（优先 `r`），WARN 记录拉取失败，不阻塞请求
4. **Given** 客户端请求的 model 不在目录中，**When** 请求到达，**Then** 网关按默认优先级 `r` 尝试，让上游返回结果或错误

---

### User Story 3 - Copilot API endpoint 发现 (Priority: P3)

网关在首个请求时用 GitHub OAuth token 通过 GraphQL 查询发现公开 GitHub Copilot API endpoint 地址，失败时回退到默认 endpoint（`https://api.githubcopilot.com`）；enterprise 地址可通过显式 `base_url` 固定。

**Why this priority**: endpoint 发现是认证前置条件，但实现简单且 fallback 明确，可在认证和路由打通后补齐。

**Independent Test**: 发起首个请求后观察日志中有 GraphQL 发现记录；模拟 GraphQL 失败，验证回退到默认 endpoint 后请求仍成功。

**Acceptance Scenarios**:

1. **Given** 网关尚未发现 endpoint，**When** 收到第一个请求，**Then** 网关用 GitHub OAuth token 向 `api.github.com/graphql` 发送 GraphQL 查询获取 `copilotEndpoints.api`，作为上游 base URL
2. **Given** GraphQL 查询失败，**When** 网关需要转发请求，**Then** 回退到默认 endpoint `https://api.githubcopilot.com`，WARN 记录发现失败

---

### Edge Cases

- **GitHub OAuth token 失效**：上游返回 401/403，网关将该 `g` 源标记为失败并 WARN 记录，允许故障转移到其他源；禁止 panic 或进程退出。
- **GraphQL endpoint 发现失败**：回退到默认 endpoint（`https://api.githubcopilot.com`）并 WARN 记录，继续尝试而非直接拒绝。
- **多个 `g` 源共存**：每个 `g` 源独立持有自己的认证状态和模型目录缓存，互不干扰。
- **模型支持的端点与路由优先级无交集**：模型目录返回的 `supported_endpoints` 不含 `/responses`、`/v1/messages`、`/chat/completions` 中任何一个，回退到默认 `r` 路径尝试，让上游返回错误。
- **热重载**：`config.yaml` 中 `g` 源的 `github_token` 变更后，经配置生效链路重载，新 token 在下次请求时生效。
- **`g` 源被 `disabled: true`**：与现有源一致，不参与调度。
- **凭据保护**：GitHub token 禁止出现在日志、metrics 或管理页响应中。

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: 网关 MUST 新增 `backend_type: g`（GitHub Copilot），与现有 `a`/`c`/`r` 同级。
- **FR-002**: `g` 源 MUST 通过 `config.yaml` 的 source 字段 `github_token`（支持 `${ENV_VAR}` 插值）配置 GitHub OAuth token。
- **FR-003**: 网关 MUST 直接用 GitHub OAuth token 作为 Bearer token 调用 Copilot API（参照 Zed 实现），不经过 Copilot session token 交换步骤。
- **FR-004**: 缺省 `base_url` 时，网关 MUST 通过 GraphQL 查询发现 Copilot API endpoint：用 GitHub OAuth token 向 `api.github.com/graphql` 查询 `copilotEndpoints.api` 字段获取 base URL。GraphQL 查询失败时 MUST 回退到默认 endpoint `https://api.githubcopilot.com` 并 WARN 记录。显式 `base_url` MUST 作为固定 endpoint 优先，跳过发现。
- **FR-005**: `g` 源 MUST 仅支持流式请求转发，不实现非流式路径。
- **FR-006**: `g` 后端 MUST 作为协议分发器：根据请求模型的 `supported_endpoints`，按优先级 r > a > c 选择并委托给已有的对应后端（`ResponsesBackend` / `AnthropicBackend` / `ChatBackend`）执行转换与转发。优先级映射：Copilot `/responses` → `r`；Copilot `/v1/messages` → `a`；Copilot `/chat/completions` → `c`。
- **FR-007**: 当模型同时支持多个端点时，网关 MUST 按优先级 r > a > c 选择第一个匹配的协议路径。
- **FR-008**: 当模型不在目录中或目录拉取失败时，网关 MUST 回退到默认路径 `r`（Responses 透传），让上游返回结果或错误。
- **FR-009**: 网关 MUST 从 Copilot `/models` 端点拉取模型目录，解析每个模型的 `supported_endpoints` 字段，缓存约 5 分钟作为路由决策依据。缓存拉取使用 singleflight 去重并发请求。
- **FR-009a**: 网关 MUST 按以下条件筛选可用模型（参照 Zed 的 `get_models` 实现）：`model_picker_enabled == true` 且 `capabilities.type == "chat"` 且（`policy` 为空或 `policy.state == "enabled"`）。`billing.restricted_to` 字段 MUST NOT 参与筛选——它是声明性元数据，实际套餐权限由 Copilot 后端在请求时裁决。被过滤掉的模型不进入可用列表，也不参与路由决策。
- **FR-010**: 委托给 r/a/c 后端时，`g` 后端 MUST 将 GitHub OAuth token 注入为上游 Authorization，将 GraphQL 发现的（或默认回退的）Copilot endpoint 作为上游地址，并注入 Zed 风格的 header 集合：`Editor-Version: Zed/0.1.0`、`X-GitHub-Api-Version: 2025-10-01`。`a` 路径 MUST 省略 `x-api-key` 与 `anthropic-version`；所有路径不注入 `Copilot-Integration-Id` 或 `Editor-Plugin-Version`（这些是 VSCode 伪装头，Zed 方式不使用）。
- **FR-011**: `contextTier` MUST 不主动注入：客户端请求体未含该字段时，网关不得补 `long_context` 或其他默认值；已含该字段时，r/Responses 路径 MUST 原样透传。a/c 路径无 Responses 顶层 `contextTier` 槽位，按各自协议覆盖矩阵处理。
- **FR-012**: `g` 源 MUST 支持现有的 `model_map` / `default_model` / `disabled` / `breaker` / `headers` / `supports_web_search` 等 per-source 配置语义。
- **FR-013**: `g` 源 MUST 参与调度器的源选择、优先级、熔断与故障转移，与其他 backend 类型同等对待。
- **FR-014**: GitHub token MUST 禁止出现在日志、metrics、管理页响应或任何可观测输出中。
- **FR-015**: 新增 `backend_type` 值 `g` MUST 在 `config.NormalizeBackendType` 中注册并校验，同步更新 `config.example.yaml` 和 `internal/config` 测试。
- **FR-016**: `g` 源的认证状态和模型目录缓存 MUST 是 per-source 独立的，不使用进程级全局单例。
- **FR-017**: 管理页配置响应 MUST NOT 返回 GitHub token；全量保存时空 `github_token` MUST 按同名 `g` 源保留原 token，非空输入按新 token 覆盖。
- **FR-018**: Copilot endpoint 发现与模型目录 MUST 位于客户端层；管理页旁路 MUST NOT 持有 Backend、scheduler 或熔断器等转发组件。

### Key Entities *(include if feature involves data)*

- **copilotclient.Client**：Copilot 客户端层能力，按 source 维护 GitHub OAuth token、GraphQL 发现的 API endpoint 地址和模型目录缓存。参照 Zed 实现，不持有短效 session token，无需刷新逻辑。
- **modelCache**：单个 `g` 源的模型目录缓存，持有从 Copilot `/models` 拉取并筛选后的模型能力信息（`supported_endpoints`），带 TTL 和 singleflight 去重。
- **CopilotBackend**：`g` 后端的实现，组合 `copilotclient.Client` 与已有三个 Backend，对外实现 `Backend` 接口，内部按模型能力将请求委托给 `ResponsesBackend` / `AnthropicBackend` / `ChatBackend`。

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 配置一个有效的 `g` 源后，用户能通过 Codex CLI 经网关对不同 Copilot 模型完成流式对话，每个模型自动路由到正确的协议路径。
- **SC-002**: 网关连续运行任意时长后请求仍成功转发，因参照 Zed 直接用长期有效的 OAuth token，不存在 session token 过期问题。
- **SC-003**: `g` 源与现有 `a`/`c`/`r` 源共存时，调度器能正确分发请求，`g` 源失败时能故障转移到其他源。
- **SC-004**: GraphQL endpoint 发现失败时，网关回退到默认 endpoint 继续正常服务，不中断请求。
- **SC-005**: 所有新增和修改的代码通过 `task check`（gofmt、go vet、全部测试），涉及模型目录缓存的代码通过 `task test-race`。

## Assumptions

- 用户已通过其他方式获得有效的 GitHub OAuth token（`gho_...` 或 `ghu_...`），网关不负责 Device Flow 交互式登录流程。
- Copilot 的协议契约（GraphQL endpoint 发现、模型目录 `supported_endpoints` 与 `billing.restricted_to` 结构）参照 Zed（`zed-industries/zed`）的 `crates/copilot_chat/src/copilot_chat.rs` 实现提取，作为协议事实源。
- Zed 方式直接用 GitHub OAuth token 作为 Bearer 调 Copilot API，无需 Copilot session token 交换，因此本功能不实现 token 刷新子系统。
- Copilot 上游的 `/responses`、`/v1/messages`、`/chat/completions` 端点返回各自协议的标准 SSE 格式，可直接复用网关已有的 r/a/c 转换路径。
- 网关已有的 `AnthropicBackend`（`a`）和 `ChatBackend`（`c`）的转换逻辑可被 `g` 后端直接委托调用，只需替换上游地址、认证 token 和注入 Zed 风格 header。
- `billing.restricted_to` 不参与模型筛选（参照 Zed），实际套餐权限由 Copilot 后端在请求时裁决。
- 本功能面向本地运行场景，不处理公网暴露或多用户凭据隔离。
