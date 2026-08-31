<!--
Sync Impact Report
- Version change: 1.3.0 → 1.4.0
- Modified principles: 无
- Added principles: VIII. 转换保真：禁止有损降级
- Added sections: 无
- Removed sections: 无
- Follow-up TODOs: 无
-->

# codex-api-gateway 项目章程

## Core Principles

### I. 产品边界与协议透传

本服务必须面向 Codex CLI 的 OpenAI Responses 契约，公开入口保持 `/v1/responses`
与 `/v1/models`。每个源必须通过稳定且操作者可读的 `backend` 标识引用一个已注册源
插件；内置发布必须至少包含 Anthropic Messages、OpenAI Chat Completions、OpenAI
Responses 与 GitHub Copilot。源专属参数必须位于该源的 options 区域，共享核心只承载
跨源通用字段。上游调用必须以流式 SSE 为结果形态。

网关必须只做 wire 对齐：入站 Responses JSON 转为上游可接受的请求形状，上游流式结果
转回合法 Responses SSE。上游是否支持某模型、工具、会话能力，是否拒绝请求，是否返回
空结果或缺失字段，必须由上游运行时表现；网关必须按协议映射这些结果，禁止因推断兼容
后端能力而 fail-fast、改写 failed、编造能力不足终态或伪造结果。

网关禁止实现 session store，禁止代补 OpenAI 平台历史。`previous_response_id` 与
Conversation/store 语义必须按 `docs/protocol-coverage.md` 分路径处理：Anthropic 与
Chat 转换路径登记不支持并保持既定 WARN/忽略行为，Responses 透传路径交给上游且不代补
历史。唯一允许网关拒绝的场景是客户端字段或 item 无法安全映射到目标协议，且继续转发必
然破坏协议。

请求失败必须以 error 或 SSE 错误事件返回，禁止 panic 逃逸到客户端或进程退出路径。
任何新增源插件必须先定义与既有源同级的请求、流式、错误和观测契约。

### II. 协议事实源与官方 SDK

`docs/protocol-coverage.md` 是协议覆盖矩阵的单一事实源。协议行为变更必须同步更新
矩阵、实现和测试；各注册源路径的字段状态禁止互相套用；GitHub Copilot 必须作为认证、
模型目录与协议分发层复用被委托协议路径的字段矩阵，不得另建重复状态行。历史设计文档、
项目总结和 SDK文档快照只能作为背景证据，禁止覆盖当前矩阵、官方 SDK 类型与测试事实。

wire 层协议字面量（事件类型、item 类型、content block 类型、finish reason 等）必须
从 `github.com/openai/openai-go/v3` 与 `github.com/anthropics/anthropic-sdk-go`
的常量派生；SDK 已暴露常量时禁止硬编码字符串。SDK 两侧均有字段的语义必须优先透传，
无等价字段时必须在矩阵登记 `supported` / `lossy_supported` / `unsupported` /
`dropped` 的准确状态与损失。

`lossy_supported` 只表示协议有损或语义不保证，禁止解释为网关替上游拦截。相似转换
变体必须通过策略注册表表达差异轴（如 `streamconv` 的 `dispatchCallKind`），新增
变体必须只扩展注册表和对应策略，禁止继续增加特例 handler 或复制主流程。

### III. 分层单向依赖与唯一组装入口

代码必须维持 L0 基础层、L1 客户端层、L2 转换层、L2.5 Backend 适配层、L3 调度层、
L4 服务编排层、L5 管理/观测旁路、L6 配置热重载的职责边界，依赖方向必须自下而上，
禁止反向引用或跨层绕行。跨层共享的协议类型必须放在 `internal/model`。

`cmd/server` 是唯一组装入口，负责两阶段初始化、HTTP server、管理页、托盘、 watcher、
daemon 与生命周期组装。`internal/server` 是唯一 `/v1/*` 编排入口；`internal/scheduler`
负责源选择与 Backend 分发；Backend 只负责单源协议适配；转换层禁止路由、选源或修改
运行时配置。

具体源插件必须收拢在独立的插件实现包内。调度、服务编排、配置解析、管理框架和健康框架
只能依赖源插件契约与 Registry，禁止 import 具体插件实现包或在热路径中判断某个源的专属
ID、字段或文案。具体插件只允许在唯一组装入口互相组合；分发型插件必须通过宿主契约请求
被委托 Backend，禁止直接依赖其他插件实现包。

`internal/admin` 与 `internal/metrics` 是旁路能力，禁止进入 `/v1/*` 转发路径。管理页
只能通过公开管理 API 和配置写盘链路影响运行时，禁止直接持有或修改 scheduler、Backend、
熔断器或其他转发组件内部状态。

### IV. 配置单一真相源与原子生效

配置生效必须只有一条链路：磁盘 `config.yaml` → `config.Load` → `holder.Replace` →
`scheduler.Reload`，日志热应用只能在同一成功重载流程中回调。管理页保存、模型/源增删、
基线指令保存与外部编辑都必须先安全写盘；禁止任何第二条直接修改运行时配置的入口。

运行时配置必须通过 `*config.Holder` 读取，读者必须调用 `Current()` 获取不可变快照，
禁止把 `*config.Config` 缓存在长生命周期对象中。配置变更必须整体原子替换，禁止字段级
in-place 修改。

管理页写盘必须由 `writeMu` 串行化，并使用同目录临时文件加 `rename` 的原子写；失败
路径必须清理临时文件。配置加载或校验失败时必须保留旧配置与旧运行时状态，禁止半新半旧
生效。`configwatch` 必须覆盖文件与父目录事件、debounce、内容去重，并同步监听
`base_instructions.md` 的变化。

新增配置项必须同时更新 `config.example.yaml`、`internal/config` 校验和测试，并明确
是否影响 `scheduler.Reload`、日志重应用或仅启动期生效。`${VAR}` 插值与
`CODEX_API_GATEWAY_` 环境覆盖必须维持可测试语义，禁止把任意进程环境变量隐式写入配置。

### V. 调度可用性与失败终态语义

调度器必须按运行时优先级、禁用状态、degraded / circuitOpen / halfOpen 状态与探测
机会选择源；源状态变化必须按既有规则调整顺序，恢复必须只在真实请求成功后转为 normal。
客户端请求上下文取消必须保持源锁定语义并被记录为 canceled，禁止计入上游失败。

协议转换路径必须由 EventGate 兜底：状态与终态事件先缓冲，首个可见内容事件到达后才向
客户端 flush 并锁定源；仅状态/终态而无内容的流必须按空响应允许换源。透传路径禁止加
EventGate，必须保持上游事件原样透传，首个上游事件即锁定源。

源一旦锁定，或已经向客户端透传 failed / incomplete 等终态，必须停止换源，即使 Backend
随后返回 error；禁止客户端在同一终态后收到第二源事件。首字节超时只约束上游开始出流，
禁止把已出流后的长思考或长输出误判为超时。

普通 4xx 必须计入源失败并允许本轮继续尝试其他可用源，但整轮结束后必须以首个 4xx
归因返回，禁止触发额外整轮重试。408 与 429 是传输可用性信号，必须走正常失败、降级
与探测流程，禁止当作普通请求语义错误处理。

### VI. 热路径隔离与结构化可观测

`/v1/*` 转发路径是延迟敏感热路径。观测、调试、profiling 和管理能力必须非阻塞或旁路化：
metrics 必须用 `select + default` 投递到带缓冲 channel，channel 满时直接丢弃事件；
admin handler 必须被 recover 中间件包裹；指标聚合 panic 只能损失当前事件，禁止影响
请求转发或其他管理请求。

所有业务与诊断日志必须使用 `log/slog` 结构化键值，禁止 `fmt.Print*`、`log.Print*`
或直接写 stderr。错误必须作为 error 返回，禁止把 error 文本当日志重复打印。请求与
上游日志必须携带 request id、source、backend、attempt、状态或错误等可用上下文，
并截断超长上游 body。

日志、metrics、管理 SSE 与异常观测禁止记录 Authorization、API key、`x-api-key`、
Cookie、MCP authorization 等凭据。已知协议限制或明确降级可以 DEBUG 或静默；丢弃
完整工具结果、用户可见输出、关键字段或协议外异常分支时必须 WARN，并至少包含类型、
标识、关联 response/context id 与影响说明。

### VII. 测试、文档与最小增量

测试必须与被测包同目录并使用 Go 标准测试框架；转换、调度、熔断、配置、热重载、管理
API、生命周期和并发行为必须使用表驱动或场景测试锁定协议语义。涉及共享状态、channel
或 goroutine 的改动必须通过 `task test-race`。

提交前必须运行 `task check`（格式检查、`go vet`、全部测试）。本地行为验证必须使用
`task run` 启动当前源码与当前 `config.yaml`，禁止把外部部署实例当作当前代码的验收
对象。管理页前端/API 行为变更必须在真实浏览器或等效端到端测试中验收。

协议、API、配置、构建或发布行为变更必须同步更新 `README.md`、`docs/`、
`config.example.yaml` 与相关测试；新增静默跳过或降级分支必须有测试证明触发路径与
输出事件仍合法。提交信息必须使用 Conventional Commits，必要时冒号后使用中文描述。

改动必须保持最小增量，禁止无关重构、格式化噪音、元数据 churn 或扩大需求表面。实现
必须优先复用 `internal/*` 既有模式和共享类型；新增抽象必须在消除真实重复或承载明确
架构边界时才允许。

### VIII. 转换保真：禁止有损降级

网关转换任何用户可见内容、工具结果或关键输入字段时，必须保持数据完整到达上游：转换
逻辑必须优先无损映射，仅允许字段级省略或格式等价的无损降级；必须禁止丢弃图像本体、
工具结果、用户可见输出等关键数据后继续发送残缺请求。目标协议无法无损承载某项数据时，
该源必须按协议不可映射进入既有源级失败与换源流程，禁止静默丢弃并把残缺请求发给该
上游，因为残缺请求会误导上游模型产出答非所问的结果。

## 安全、隐私与本地运行边界

真实 API key、上游凭据、本地 URL 和本地专用配置禁止提交。`config.yaml`、日志、PID、
`bin/`、构建产物、压缩包和运行时快照是本地状态，禁止作为治理或协议事实源；`config.example.yaml`
必须保持可提交且不泄露真实凭据。

`source.headers` 必须经过名称合法性校验，且禁止覆盖网关统一管理的保留头：`Authorization`、
`Content-Type`、`Accept`、`x-api-key`、`anthropic-version`、`anthropic-beta`。自定义
头只能在上游 HTTP 层应用，被跳过的保留头必须可观测。

发往上游模型的所有 wire 文本（工具 description、schema description、占位文本、回灌
提示等）必须使用英文。中文用于代码注释、文档、commit message 和用户交互。代码注释必须
短且解释非显而易见的约束，禁止空泛叙述。

`base_instructions.md` 与 `base_instructions_cn.md` 是直接注入模型的基线指令，必须成对
同步、语义等价、保持最小增量和强制语气；禁止写入背景说明、长示例、实现过程或单侧例外。

Codex 客户端配置开关必须尊重 `CODEX_HOME`，只覆盖本网关 provider 块与 `model_provider`，
启用前必须备份原值，恢复失败必须保持现状并报错，写回必须保留原权限并使用原子替换。
托盘、autostart、daemon 和 headless 降级不得影响 HTTP 服务生命周期；关闭流程必须先取消
在途流，再 Shutdown HTTP server、关闭 watcher 与日志资源，并保持幂等。

## 开发、审查与发布工作流

开发门禁使用 `Taskfile.yml`：`task build` 构建，`task test` / `task test-race` 测试，
`task fmt` / `task fmt-check` 格式化，`task check` 作为常规提交前门禁。Go 代码必须通过
`gofmt`，包名短小全小写，导出标识符使用 PascalCase，非导出标识符使用 camelCase。

代码评审必须至少核对：分层依赖、协议矩阵同步、SDK 常量来源、配置闭环、热路径隔离、
失败终态语义、凭据保护、测试证据和文档同步。PR 必须包含变更摘要、`task check` 结果、
相关 issue 或设计文档；违反本章程的例外必须在 PR 中显式说明原因与补偿措施。

发布工作流必须先完成测试，再执行 Linux amd64/arm64、Darwin amd64/arm64、Windows
amd64/arm64 六平台构建。交叉构建必须使用 `CGO_ENABLED=0`，版本注入必须使用完整
`cmd/server` 变量路径，发布附件必须来自对应平台的干净构建产物。发布状态必须等待 CI
最终 conclusion，禁止把 in_progress 称为通过。

## Governance

本章程优先于仓库内历史设计文档和一般开发实践；与 `AGENTS.md`、README 或当前实现冲突时，
必须先修订事实源或章程再合入，禁止以文档漂移为由保持双轨规则。协议事实仍以当前
`docs/protocol-coverage.md`、官方 SDK、测试与代码共同校验后的结果为准。

修订必须以 PR 或等效评审记录变更原因、影响范围、迁移计划和测试证据；必须同步更新
Sync Impact Report、版本号与 Last Amended 日期。合规审查必须在合入前核对本章程全部
原则，不能仅核对与本次文件直接相关的条目。

版本策略：MAJOR 用于不兼容的治理原则移除、重定义或边界收缩；MINOR 用于新增原则、
新增治理章节或实质扩展；PATCH 用于措辞澄清、笔误和非语义修正。任何故意延迟的字段必须
在正文中说明原因，并列入 Sync Impact Report 的待办清单。

**Version**: 1.4.0 | **Ratified**: 2026-08-18 | **Last Amended**: 2026-08-31
