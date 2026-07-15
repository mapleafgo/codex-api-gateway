# 多源容灾体系重新设计

日期: 2026-07-15
状态: 待实现

## 1. 背景与动机

OpenAI2Response 网关当前容灾：scheduler 按优先级遍历源做 failover（一轮）+ breaker 简单熔断（`failure_threshold` 累计→open→`cooldown`→halfOpen）。一轮所有源失败即 `response.failed`。问题：
- 熔断太粗（累计失败即熔断，无降级中间态），偶发失败易误熔断好源
- 单请求内全失败不重试，瞬时故障直接失败
- 优先级静态（config `priority`），无法反映源运行时健康度

重新设计为**两级容灾（降级 + 熔断）+ 单请求重试**：源连续失败先降级（优先级后移但仍可用），再失败才熔断（探测队列）；单请求全失败按 backoff 重试。

## 2. 目标与非目标

**目标**
- 源健康状态机：normal / degraded / circuitOpen，跨请求持久（内存，进程重启重置）
- 运行时优先级序列：降级后移、升回恢复原位
- 单请求重试：全失败按固定 backoff 重试整轮，`max_retries` -1 无限 / 0 不重试
- config 简化：sources 用列表顺序作优先级（去掉 `priority`）

**非目标**
- 不持久化源健康状态到磁盘（重启重置，已确认）
- 不区分错误类型（连续失败即计，不分 5xx/超时）
- backoff 序列固定硬编码（不放 config）

## 3. 源健康状态机（per-source，跨请求持久，内存）

每源维护：`state`、`failStreak`（连续失败）、`successStreak`（连续成功）、`degradeCount`（0/1/2）、`openedAt`（熔断时刻）。

```
normal (degradeCount=0)
  └─ failStreak ≥ degrade_threshold ──▶ degraded
     (序列后移到末尾, failStreak归零, degradeCount=1)

degraded (degradeCount=1)
  └─ successStreak ≥ recover_threshold ──▶ normal
     (恢复原位 originalIndex, degradeCount=0, streaks归零)
  └─ failStreak ≥ degrade_threshold ──▶ degradeCount=2 ──▶ circuitOpen
     (进探测队列)

circuitOpen (degradeCount=2, Allow()=false)
  └─ cooldown 到期 ──▶ halfOpen 探测 (Allow 放行一次)
       ├─ 探测成功 ──▶ recovery 配置:
       │    • normal:   degradeCount=0, streaks归零, 恢复原位
       │    • degraded: degradeCount=1, 保持后移位置
       └─ 探测失败 ──▶ 重置 openedAt(重计 cooldown), 保持 circuitOpen
```

**计数规则**
- `RecordSuccess()`：`successStreak++`，`failStreak=0`。若 degraded 且 `successStreak ≥ recover_threshold` → 升回 normal。若 halfOpen 探测成功 → 按 recovery 配置。
- `RecordFailure()`：`failStreak++`，`successStreak=0`。若 normal 且 `failStreak ≥ degrade_threshold` → degraded。若 degraded 且 `failStreak ≥ degrade_threshold` → circuitOpen（`degradeCount=2`，`openedAt=now`）。若 halfOpen 探测失败 → 重置 `openedAt`，回 circuitOpen。
- `Allow()`：normal/degraded → true；circuitOpen → 若 `now-openedAt ≥ cooldown` 转 halfOpen 返回 true，否则 false；halfOpen → 受 `half_open_probes` 限制。

**阈值（不对称，已确认）**
- `degrade_threshold` 默认 3（失败 3 次才降级，保守）
- `recover_threshold` 默认 1（成功 1 次就升回，激进）

## 4. 运行时优先级序列

- 初始 = config sources 列表顺序（去 `priority`）；scheduler 持 `runtimeOrder []sourceRef`，每项含 `originalIndex`（config 列表位置）
- **降级**：源从当前位置移到 `runtimeOrder` 末尾
- **升回 normal**（degraded→normal 或 circuitOpen 探测成功 recovery=normal）：源恢复到 `originalIndex` 位置（重排 `runtimeOrder`）
- recovery=degraded：位置不变（保持后移）
- failover：遍历 `runtimeOrder`，`Allow()=false`（circuitOpen 未到 cooldown）的跳过

## 5. config schema

```yaml
sources:                  # 列表顺序 = 优先级（去掉 priority 字段）
  - name: ...
    base_url: ...
    api_key: ...
    model_map: { ... }
    default_model: ...
    breaker:              # 可选，per-source 覆盖（不含 max_retries）
      first_byte_timeout: 12s
      degrade_threshold: 3
      recover_threshold: 1
      cooldown: 30s
      half_open_probes: 1
      recovery: normal    # normal | degraded

breaker:                  # 全局默认
  first_byte_timeout: 12s
  degrade_threshold: 3
  recover_threshold: 1
  cooldown: 30s
  half_open_probes: 1
  recovery: normal
  max_retries: 0          # 单请求全失败重试：-1 无限 / 0 不重试 / N（仅全局）
  # backoff 固定 [2s,4s,6s,8s,10s]，硬编码，不放 config
```

**迁移**：旧 `priority`、`failure_threshold` 字段解析时忽略并告警（log），不报错。

## 6. 单请求重试（scheduler.Execute 内）

固定 backoff 序列 `[2s, 4s, 6s, 8s, 10s]`（封顶 10s）。

```
mr := cfg.Breaker.MaxRetries
for attempt := 0; mr == -1 || attempt <= mr; attempt++ {
    for src in runtimeOrder:
        if !breaker.Allow(src): continue          // circuitOpen 跳过
        locked, _ := trySource(src)
        if locked: return nil                      // 成功（已 RecordSuccess）
    // 一轮无成功（全失败 or 全熔断跳过）
    if mr == 0: return ErrAllSourcesFailed
    if mr != -1 && attempt == mr: return ErrAllSourcesFailed
    wait backoff[clamp(attempt, len)] with ctx.Done() interrupt
}
```

- `clamp(attempt)`：attempt 超过序列长度用末值（10s）
- backoff sleep 用 `select { case <-ctx.Done(): return ctx.Err(); case <-timer: }`，客户端断开即停

## 7. 全熔断后重试的行为

当一轮中所有源 `Allow()=false`（全 circuitOpen，未到 cooldown）：
- 一轮无成功 → 触发重试 → backoff 等待累计推进时间
- 每次重试遍历调 `Allow()`，内部检查 `now-openedAt ≥ cooldown` 自动转 halfOpen 放行
- 累计等待达 `cooldown`（默认 30s；backoff 2+4+6+8+10=30s 约第 5 次）→ 某源 halfOpen → 下轮 `trySource` 探测
  - 成功 → 升回 → 服务请求
  - 失败 → 重置 cooldown → 继续 circuitOpen → 继续 backoff
- 循环直到：成功 / `ctx.Done()` / `max_retries` 耗尽

**后果（已确认接受）**
- `max_retries=-1` + 全源永久故障 = 客户端 SSE 永挂，唯一出口客户端断开（ctx）
- `max_retries=0` = 全熔断/全失败立即 `response.failed`（不重试，等同现状）
- 全熔断时前几次重试可能"空转"（源未到 cooldown 全跳过），无害，只是等

## 8. 改造面

**internal/config**
- `Source`：去掉 `Priority`，加 `originalIndex int`（Load 时按列表序赋）
- `BreakerCfg`：`FirstByteTimeout`/`DegradeThreshold`/`RecoverThreshold`/`Cooldown`/`HalfOpenProbes`/`Recovery string`/`MaxRetries int`（替代 `FailureThreshold`，保留 `FirstByteTimeout` 供 trySource watchdog；`MaxRetries` 仅全局，per-source override 不含）
- `OrderedSources()` → 返回列表序（不排序），每源带 `originalIndex`
- `BreakerFor()` 合并逻辑改新字段
- `validate()`：新默认值；旧字段（priority/failure_threshold）忽略告警

**internal/breaker**
- 状态机扩展：`normal | degraded | circuitOpen | halfOpen`（替代 closed/open/halfOpen）
- 字段：`failStreak`/`successStreak`/`degradeCount`/`openedAt`
- `Allow()`：按上述规则（normal/degraded true；circuitOpen 看 cooldown 转 halfOpen；halfOpen 受 probes 限）
- `RecordSuccess()`：successStreak++，failStreak=0；degraded 升回 / halfOpen 探测成功按 recovery
- `RecordFailure()`：failStreak++，successStreak=0；normal→degraded / degraded→circuitOpen / halfOpen→circuitOpen 重置
- 暴露 `DegradeCount()`/`State()` 供 scheduler 决定序列后移/恢复

**internal/scheduler**
- `runtimeOrder []sourceRef`（含 originalIndex），初始 = OrderedSources
- `Execute`：加重试外层循环（§6）+ 遍历 `runtimeOrder`
- 降级触发后（RecordFailure 导致 degradeCount 增）：把源移到 `runtimeOrder` 末尾
- 升回触发后（RecordSuccess 导致恢复 normal）：源恢复到 `originalIndex` 位置
- 锁护 `runtimeOrder`（并发多请求）
- `trySource` 签名不变（已用 SDK 类型）

**internal/server**：无改（`Execute` 签名不变，`response.failed` 仍由 execErr 触发）

## 9. 边界与错误处理

- **ctx.Done()**：重试 backoff sleep 可中断；客户端断开立即返回，防 SSE 永挂（-1 的唯一安全出口）
- **并发**：`breaker.mu`（已有，扩展护新字段）；scheduler 锁护 `runtimeOrder`（多请求并发读写序列）
- **序列一致性**：降级后移 / 升回恢复需原子（锁内完成 read-modify-write）
- **重启**：状态内存，进程重启全部归零（normal + runtimeOrder=config 序）
- **backoff clamp**：attempt 超序列长度用末值 10s
- **half_open_probes**：halfOpen 状态允许的并发探测数（沿用现有语义）
- **per-source breaker 覆盖**：`BreakerFor` 合并全局 + 源级（沿用）

## 10. 测试策略

- **breaker 单元**：状态机各转换（normal⇄degraded、degraded→circuitOpen、circuitOpen→halfOpen→recovery normal/degraded、计数重置、阈值边界）
- **scheduler 单元**：
  - 降级后移序列、升回恢复原位
  - 单请求重试：max_retries 0/1/-1、backoff 间隔、ctx.Done 中断
  - 全熔断重试：累计等待到 halfOpen 探测
  - failover 仍按 runtimeOrder、熔断源跳过
  - 并发：多请求不破坏 runtimeOrder
- **config 单元**：列表序=优先级、originalIndex 赋值、旧字段忽略告警、新默认值
- **集成（依赖需求 3）**：多源 failover + 降级 + 重试端到端
- 收口：`go test ./...` + `go build ./...` 全绿

## 11. 实现顺序（预告，详由 writing-plans）

1. config 改造（Source 去 Priority + originalIndex、BreakerCfg 新字段、RetryCfg、OrderedSources 列表序、迁移告警）
2. breaker 状态机重写（normal/degraded/circuitOpen/halfOpen + 计数 + Allow/Record* + 升降级判定）
3. scheduler：runtimeOrder + Execute 重试循环 + 序列后移/恢复 + 锁
4. 测试（breaker/scheduler/config 单元 + 集成）
5. 收口
