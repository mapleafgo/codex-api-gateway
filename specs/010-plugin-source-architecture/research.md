# Research: 插件式上游源架构

## 1. 契约层位置与粒度

**Decision**: 新增 `internal/plugin` 作为共享源插件契约层，定义 Descriptor、Source options schema、Backend、ModelCatalog、HealthProbe、AdminExtension 和 Registry；不把接口散落在 scheduler/admin/config。

**Rationale**: 四类内置源的公共需求已经稳定：身份声明、配置校验、流式执行、可选目录/探测/管理动作。集中契约能避免共享核心反向感知具体源，也让测试源可以在一个包内实现完整能力。

**Alternatives considered**: 只抽象 `Backend.Execute` 更小，但无法消除 admin、health、server 预检和模型列表中的类型分支；把所有能力塞进必选接口会迫使简单插件实现无关方法，因此可选能力用独立 interface。

## 2. 配置校验依赖方向

**Decision**: `config.Source` 只保存通用字段、`Backend` 与 `Options map[string]any`。`config` 定义 `SourceValidator` 窄接口，由 `cmd/server` 把不可变 Registry 注入 `config.Load`、configwatch、admin 写盘等调用点。

**Rationale**: 保持 L0 配置层不 import 插件契约或实现，同时让所有读写路径共用同一个校验结果。热重载失败发生在 `Load`，`holder.Replace` 不执行，旧配置继续服务。

**Alternatives considered**: 全局注册表实现简单，但引入进程级可变状态并破坏两阶段初始化的可测性；让 config import plugin 会反转基础层依赖，已排除。

## 3. 类型专属配置形状

**Decision**: 使用 `backend: <stable-id>` 与 `options:` 承载专属值。旧 `backend_type` 由通用解析显式拒绝；旧顶层 `github_token` 通过严格 source 解码作为未知字段拒绝；旧顶层 `anthropic:` 配置迁移到 Anthropic 源的 options，默认值由插件 schema 提供。`supports_web_search` 是跨源请求形状开关，继续留在通用区。

**Rationale**: 满足“不做旧格式兼容”和“共享核心不承载源专属事实”。schema 校验能给出源名、字段名和原因，避免请求期才暴露模糊错误。

**Alternatives considered**: 保留顶层全局 Anthropic 配置会让共享 config 继续知道某协议参数；静默迁移违背用户确认的干净破坏性升级。

## 4. 敏感 options 语义

**Decision**: GET 管理配置时敏感字段固定输出 `__codex_redacted__`；POST 时空串或缺省表示保留同名同类型源已有值，`__codex_clear__` 表示显式清空，其他值表示替换；字面量 `__codex_redacted__` 作为提交值拒绝。

**Rationale**: 全量管理表单需要区分“没改”和“清空”；固定哨兵可避免明文凭据回显，也能在浏览器刷新后保持脱敏状态。

**Alternatives considered**: 只用空串保留会导致无法清空；包装对象 `{value,clear}` 更精确但破坏 YAML options 的自然表达。

## 5. Copilot 分发与宿主委托

**Decision**: endpoint 发现、模型目录筛选缓存、r > a > c 协议路由、Zed-style headers、Device Flow、连通性探测全部移入 `internal/plugins/copilot`。该插件通过 `plugin.DelegateHost.BackendByID` 委托 `openai-responses`、`anthropic` 或 `openai-chat`，不 import 其他插件包。委托产出的观测事件由 Copilot 包装器改写为 `backend=github-copilot`。

**Rationale**: 这是本次架构的核心边界。宿主按 ID 解析被委托 Backend，Copilot 可以自治演进，而共享核心仍不知道分发型源的存在。

**Alternatives considered**: 直接组合三个 Backend 实现最短路径，但会造成插件横向依赖；为 Copilot 在 scheduler 增加 route 分支违反 FR-005/FR-007。

## 6. 管理页动态能力

**Decision**: 新增 `/admin/api/source-plugins` 返回描述符数组；前端按 Schema 渲染通用区和 options 字段，按 Action 元数据渲染扩展动作。动作统一走 `/admin/api/source-plugins/{backend}/actions/{action}`，由共享 admin 路由转发给对应插件。Device-code 类动作返回公开状态和轮询元数据，共享页面只理解通用 device-code 展示协议，不理解 GitHub/Copilot 文案或凭据。

**Rationale**: 满足新增源不改共享表单的目标，同时保留现有 Device Flow 用户码、轮询、取消和保存后生效体验。

**Alternatives considered**: 为每个插件注入任意 HTML 片段灵活但难做一致样式、无障碍和安全审查；硬编码四类表单正是要移除的耦合。

## 7. 流式模式与请求预检

**Decision**: Descriptor 声明 `StreamingKind=converted|passthrough`。scheduler 仅对 converted 模式启用 EventGate。Server 的启动前预检改为对首源调用可选 `RequestPreparer.PrepareRequest`；混合源的字段丢弃警告改为根据已启用插件声明的映射能力聚合判断，不再比较具体 backend ID。

**Rationale**: 现有 server 中 `a/r/c/g` 分支是隐藏耦合点；只换 scheduler 不足以满足架构守护要求。

**Alternatives considered**: 让每个插件自行决定 EventGate 会重复平台级失败终态职责；保留 server 特判会继续违反 FR-005。

## 8. 观测与 Token 归一化

**Decision**: Go 结构体和 wire JSON 从 `BackendType/backend_type` 改为 `Backend/backend`，值为注册表稳定 ID。Anthropic 插件在上游事件上报前把 cache read/create 加进 InputTokens；metrics 删除短码归一化分支，只信任事件中的完整输入计数。

**Rationale**: 观测身份属于插件契约的一部分；归一化是协议知识，不应留在平台 metrics。

**Alternatives considered**: 兼容输出两个字段会延长双轨身份并削弱 SC-002。
