# Data Model: 单请求最大超时时长配置

## 实体

### BreakerCfg.RequestTimeout（全局默认 + per-source 覆盖）

- 类型：`config.Duration`（时长字符串，如 `120s`），koanf/yaml 键 `request_timeout`
- 默认值：缺省或 0 → `120 * time.Second`（`applyDefaults`）
- 校验：负值拒绝（`validateBreakerNonNegative`，全局与每源同一规则）
- 覆盖：`Config.BreakerFor(src)` 中，`src.Breaker.RequestTimeout != 0` 时覆盖
  全局；0 表示继承全局
- 生效粒度：单个源的单笔上游调用（非客户端请求整轮）

### Source.Breaker（每源覆盖）

- 字段：`sources[].breaker.request_timeout`
- 规则：未配置（nil 或 0）→ 继承全局；配置负值 → 校验报错；`MaxRetries`
  不参与覆盖的既有规则不变

### 超时执行上下文（scheduler 每笔尝试）

- `fbCtx`：首字节定时器（既有，首事件到达后停止）
- `attemptCtx`：`context.WithTimeoutCause(fbCtx, requestTimeout,
  backend.ErrUpstreamTimeout)`，传给 Backend.Execute；到点后 `context.Cause`
  置为哨兵错误
- 判定：`backend.IsServerTimeout(ctx, err)` = `errors.Is(context.Cause(ctx),
  ErrUpstreamTimeout)`；`IsClientCanceled` 在服务端超时时返回 false

### UpstreamEvent（log/metrics 来源）

- 单笔超时且未锁定：`Status:"failed"`、`Code:504`、`Error` 含 timeout 归因
  （经 `classifyOutcome` + `StatusCodeFromErr` 映射后由 scheduler 修正）
- 单笔超时且已锁定：`Status:"failed"`、`Code:504`、`Error` 含 timeout 归因
- 客户端取消：保持既有 `canceled` / 499 语义，不受影响

### server 终态补发（整轮收口）

- 条件：`backend.IsServerTimeout(execErr)` 且流内未出现过
  `response.completed / incomplete / failed` 终态事件
- 输出：`response.failed` 终态事件（复用既有 evCount==0 补发路径的构造方式）
- 状态映射：status="failed"、code=504、日志/指标 reason=timeout

## 状态机变化

- 新增：单笔尝试的"总时长定时"状态（starting → streaming/等待 → timed out）
- 新增：终态判定里"服务端超时"分支（不再落入 canceled）
- 保持：首字节超时、EventGate 缓冲/锁定、failover/熔断、客户端取消语义不变

## 校验规则（映射自 spec FR）

- `request_timeout` 缺省/0 → 120s（FR-001）
- `request_timeout` 负值 → 校验错误（FR-001）
- per-source 覆盖：覆盖值生效、未覆盖继承全局（FR-001a）
- 到点取消在途调用；未出内容 → 失败计数 + 允许换源（FR-003）
- 已出内容 → 锁定 + failed 终态收口 + 不换源（FR-005）
- 超时与取消在终态/日志/指标可区分（FR-006）
