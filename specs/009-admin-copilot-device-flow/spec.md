# Feature Specification: 管理页 Copilot Device Flow 授权

**Feature Branch**: `009-admin-copilot-device-flow`

**Created**: 2026-08-26

**Status**: Draft

**Input**: 在管理员支持 Copilot 授权；只做 GitHub Device Flow 登录授权，参照 Zed 实现，不做 copilot2api 凭据导入

## User Scenarios & Testing *(mandatory)*

### User Story 1 - 为新建 Copilot 源完成登录授权 (Priority: P1)

管理员在管理控制台新建 Copilot 源时填写源名称和连接参数，不需要手工寻找或粘贴 GitHub token。管理控制台展示 GitHub 用户码和验证地址；管理员在浏览器完成授权后，控制台收到成功状态，该源连同新获得的凭据一起保存并生效。

**Why this priority**: 这是本功能的核心价值：消除手工获取 GitHub OAuth token 的门槛，让新建 Copilot 源可以直接完成接入。

**Independent Test**: 使用一个不存在的源名称发起授权，在模拟授权服务中确认用户码后等待成功，验证源被保存且后续能使用新凭据访问 Copilot。

**Acceptance Scenarios**:

1. **Given** 管理员已填写合法的新建 Copilot 源草稿，**When** 发起 Device Flow 授权并在 GitHub 完成用户码确认，**Then** 该源以可用状态保存，GitHub token 只保存在服务端配置中。
2. **Given** 管理员尚未完成 GitHub 授权，**When** 查看授权界面，**Then** 能看到用户码、验证地址、轮询节奏和当前状态。
3. **Given** 另一个管理员或页面已经发起了未完成授权，**When** 再次发起授权，**Then** 系统返回当前活跃会话状态，不会创建第二个并发会话。

---

### User Story 2 - 为已有 Copilot 源重新授权 (Priority: P2)

管理员选择一个已有 Copilot 源重新发起 Device Flow。旧 token 继续保留到新授权成功为止；成功后系统更新该源的 GitHub token，并保持源身份不变。

**Why this priority**: 已有源可能需要更换账号或恢复失效凭据，重新授权必须安全且不产生重复源。

**Independent Test**: 对一个已保存的 Copilot 源发起授权，在授权服务返回新 token 后验证同名源的配置被更新，且不会新增第二个同名源。

**Acceptance Scenarios**:

1. **Given** 已存在一个 Copilot 源，**When** 管理员为它重新授权并完成 GitHub 登录，**Then** 该源的凭据更新为新 token，源数量不变。
2. **Given** 授权尚未成功，**When** 管理员查看已有源，**Then** 旧配置继续保留并可继续使用。

---

### User Story 3 - 取消、失败与凭据保护 (Priority: P3)

管理员可以取消进行中的授权。GitHub 拒绝授权、用户码过期、网络失败或保存失败都会显示明确的终态错误；所有界面和可观测输出都不暴露 access token。

**Why this priority**: 授权涉及外部服务、长期凭据和管理页共享状态，失败路径与保密性决定功能是否可信。

**Independent Test**: 分别模拟取消、过期、拒绝、网络错误和保存失败，验证每次都得到稳定终态、可重试，并且响应与日志中没有完整 token。

**Acceptance Scenarios**:

1. **Given** 一个等待用户确认的授权会话，**When** 管理员取消它，**Then** 会话进入取消终态，既有源配置不变。
2. **Given** GitHub 返回拒绝、过期或其他错误，**When** 轮询达到对应结果，**Then** 会话进入错误终态并展示不含凭据的原因。
3. **Given** 授权成功但本地保存失败，**When** 系统尝试落盘，**Then** 会话进入错误终态，前端不能收到 access token。

### Edge Cases

- 用户码在 GitHub 侧过期时，系统必须结束本次会话并允许重新发起。
- GitHub 要求放慢轮询时，系统必须按外部指示延长间隔，不得继续原频率请求。
- 同名目标不是 Copilot 源时，系统必须拒绝把 Copilot 凭据写入该名称。
- 进程重启会丢失未完成的授权进度；管理员需要重新发起，而已保存源不受影响。
- 管理控制台刷新后仍能看到当前会话的可公开状态。
- 新源草稿缺少名称或类型不正确时，必须在启动前失败。

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: 管理控制台 MUST 支持使用 GitHub Device Flow 发起 Copilot 登录授权。
- **FR-002**: 授权流程 MUST 展示 verification URI 和 user code，并按授权服务返回的 interval 轮询状态。
- **FR-003**: 授权流程 MUST 使用 Zed 当前公开 Copilot OAuth 应用的 client ID 与 `read:user` scope。
- **FR-004**: 系统 MUST 处理 pending、slow down、过期、拒绝、成功和其他 OAuth 错误结果。
- **FR-005**: 授权成功后，系统 MUST 将获得的长期 GitHub OAuth token 作为指定 Copilot 源的认证凭据保存。
- **FR-006**: 目标不存在时 MUST 按提交的合法 Copilot 源草稿新增；目标是同名 Copilot 源时 MUST 更新其凭据和应用草稿中的其他源字段。
- **FR-007**: 目标名称已存在但不是 Copilot 源时 MUST 拒绝授权。
- **FR-008**: 同一运行实例同一时间 MUST 只有一个活跃 Device Flow 会话。
- **FR-009**: 管理 UI MUST 提供查询当前公开状态和取消活跃会话的能力。
- **FR-010**: device code 和 access token MUST 不出现在管理界面响应、快照、日志、指标或事件流中。
- **FR-011**: 凭据保存 MUST 复用既有配置持久化与热生效机制，禁止建立第二条运行时配置修改链路。
- **FR-012**: 授权取消或失败 MUST NOT 修改既有源凭据。
- **FR-013**: 本功能 MUST NOT 提供 copilot2api 凭据文件导入、token 手动导入辅助流程或自动 token 刷新。

### Key Entities *(include if feature involves data)*

- **Device Flow 会话**: 表示一次授权尝试，包含目标源名、用户码、验证地址、设备码、轮询间隔、状态和终态错误；access token 只在成功落盘所需的短时间内存在于服务端。
- **Copilot 源草稿**: 表示授权成功后要创建或更新的源定义，至少包含源名称、接口类型和相关连接参数。
- **公开授权状态**: 面向管理界面的安全视图，只包含状态、用户码、验证地址、轮询间隔、目标源名和非敏感错误信息。

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 管理员从发起授权到看到用户码的时间不超过 5 秒（不含外部网络异常）。
- **SC-002**: 管理员完成 GitHub 授权后，源配置在 10 秒内进入已授权且可调度状态（不含外部网络异常）。
- **SC-003**: 所有授权响应和可观测输出中完整 access token 出现次数为 0。
- **SC-004**: 并发重复发起授权时活跃会话数量始终为 1。
- **SC-005**: 取消、过期、拒绝和保存失败的每一次尝试都产生明确终态，且不改变既有源凭据。

## Assumptions

- 管理控制台已有访问边界由部署环境负责；本功能不新增独立的管理员账号体系。
- Zed 的公开 OAuth 应用 ID 可用于本网关的 Copilot Device Flow。
- GitHub OAuth token 的生命周期足够长期，本版本不实现自动刷新。
- 配置持久化位置和热重载机制沿用现有网关约定。
