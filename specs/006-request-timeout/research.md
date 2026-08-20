# Research: 单请求最大超时时长配置

## Decision 1: 配置落点复用 BreakerCfg 覆盖模型

- **Decision**: 新增 `BreakerCfg.RequestTimeout`（koanf/yaml：`request_timeout`，
  Duration），全局默认 120s；per-source 经 `sources[].breaker.request_timeout`
  覆盖，未配置时经 `BreakerFor` 继承全局。
- **Rationale**: `first_byte_timeout` 已在该位置走通"全局默认 + per-source 覆盖
  + 负值校验 + env 白名单 + 管理页字段"整套链路，复用零新增管道；规格确认需要
  per-source 覆盖（Q1=A）。选择 `breaker` 段而非 `server` 段，因为该时长作用于
  上游调用生命周期，与首字节超时同属"单笔上游尝试"维度。
- **Alternatives considered**:
  - 放 `server.request_timeout`：语义上偏 HTTP listener，且无法复用 per-source
    覆盖与协议/校验管道，需第三处特例，拒绝。
  - 顶层 `request:` 段：新增配置命名空间，破坏现有分段习惯，且 env 覆盖白名单
    需新增分支，拒绝。

## Decision 2: 用 WithTimeoutCause + 哨兵错误区分服务端超时

- **Decision**: `trySourceGeneric` 在 `fbCtx`（首字节定时器）之下再派生
  `context.WithTimeoutCause(ctx, requestTimeout, backend.ErrUpstreamTimeout)`，
  把该 context 传给 Backend.Execute；超时到点后各后端通过
  `backend.IsServerTimeout(ctx, err)`（`errors.Is(context.Cause(ctx), sentinel)`）
  把该错误归类为"网关侧单笔超时"而非"客户端取消"。
- **Rationale**: 现有 `IsClientCanceled` 对 `context.Canceled/DeadlineExceeded`
  一律判真，若不区分，总超时会被后端按 cancelled 处理（不产出 failed 终态、
  指标记 canceled），违反 FR-006/FR-005。WithTimeoutCause 是标准库原生能力
  （Go 1.21+，本仓库 1.26），cause 仅在该超时自身到期时置位，首字节定时器
  取消（父 ctx 主动 cancel）不会误置 cause。
- **Alternatives considered**:
  - scheduler 返回后按 timer fired 标志包装错误：后端内部的 classifyOutcome /
    终态产出仍会翻车（无法区分导致不写 failed 事件），需引入跨层对账，拒绝。
  - 自定义 wrapper context 类型：侵入既有 ctx 传递路径，无必要，拒绝。

## Decision 3: 已出流超时的 failed 终态由后端优先、server 兜底

- **Decision**: a/c 后端把服务端超时从"取消"中剥离后，复用既有失败路径产终态
  （anthropic 收尾安全网补 `response.failed`、chat `conv.Fail`）；r 透传后端
  保持"不代补上游事件"原则，由 `server.handleResponses` 在检测到
  `backend.IsServerTimeout(execErr)` 且流内未出现过终态事件时补发
  `response.failed`（扩展现有 evCount==0 补发分支，新增"终态未出现"判定）。
- **Rationale**: FR-005 要求客户端 MUST 收到 failed 收尾终态；r 路径合成终态
  属网关主动中止场景，不属于改写上游事件；server 是唯一整轮收口方，能拿到
  "流内是否已产出终态"的完整视角（onEvent 回调里记录终态类型）。
- **Alternatives considered**:
  - r 后端合成 failed：违反章程"r 禁止代补终态/EventGate"，拒绝。
  - 客户端侧容错截断流：没有 wire 级验收保证，拒绝。

## Decision 4: 超时状态码与可观测约定

- **Decision**: 服务端单笔超时时终态 code 统一 504（Gateway Timeout），日志
  WARN（键 `reason=timeout`、`source`、`attempt`、`elapsed`），metrics
  `RequestEvent{Status:"failed", Code:504, Error:含 timeout 归因}`。客户端取消
  仍按既有 499/canceled 路径，互不覆盖。
- **Rationale**: 504 是标准"网关未在上游时限内收到响应"语义；日志/指标用同一
  reason 键保证排障事实链一致，满足 FR-006/SC-003。
- **Alternatives considered**: 复用 408（上游 408 是传输信号、走 breaker 流程，
  语义混淆）；复用 0/200（无法与真实失败区分），均拒绝。

## Decision 5: 热更新与快照语义

- **Decision**: 每笔尝试开始时从 `holder.Current()` 读取
  `BreakerFor(src).RequestTimeout`；配置热更新后新发起的尝试立即用新值，在途
  尝试沿用发起时快照；不触发 `scheduler.Reload`（该字段是纯值读取，无状态
  重建需求）。
- **Rationale**: 与 `first_byte_timeout` 现读取方式一致（`trySourceGeneric`
  每次取 `holder.Current()`），零额外机制；FR-008 要求热更新即时生效。
- **Alternatives considered**: 启动期缓存或 Reload 阶段注入：引入陈旧值与多余
  机制，拒绝。

## Decision 6: 测试策略

- **Decision**: 分层表驱动测试：config（默认/负值/env/合并）、scheduler
  （未出流超时→换源、仅状态事件后静默→到点超时、已出流→锁定+failed 终态、
  客户端取消早于超时→canceled）、server 集成（504/reason 映射、r 路径终态
  补发、与 499 区分）；并发/共享状态改动跑 `task test-race`。
- **Rationale**: 章程 VII 要求测试贴近实现、表驱动锁定语义；该改动横跨
  config/scheduler/backend/server，四层各有独立断言面。

## Decision 7: 文档与配置示例同步

- **Decision**: `config.example.yaml` 全局 `breaker.request_timeout: 120s` +
  per-source 示例注释；README breaker 参数表新增一行；`docs/protocol-coverage.md`
  追加变更记录，说明"单笔总时长"与"首字节超时"职责差异；管理页 H5 断路器表单
  增加输入项与中英文 label。
- **Rationale**: 章程 IV/VII 要求配置闭环与文档同步；规格 Assumptions 要求
  显式说明与首字节超时的语义差异，避免误读。
