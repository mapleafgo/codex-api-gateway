# 断路器探测机制重新设计

日期: 2026-07-23
状态: 待实现

> 本设计后续按「机会窗口」语义改版：degrade_interval 超时只把 degraded 源恢复到
> 原始优先级位置进入机会窗口（健康状态**保持 degraded**）；机会内成功才转 normal，
> 机会内失败重新排到队尾等下一次机会，累计 N 次机会失败（中间无成功）触发
> circuitOpen。本文以改版后语义为准（2026-08-04 更新）。

## 1. 背景与动机

当前断路器有三个突出问题：

**Degraded 永远无法恢复**
源被降级后 `moveToEnd` 排到队尾。只要前面有健康源，它永远不被尝试，也就永远没有机会通过 `RecordSuccess()` 恢复。唯一的恢复途径是人工提升或所有源全挂。

**CircuitOpen 的冷却探测同样永远不触发**（2026-09-14 修复）
熔断源被后移到运行时队尾且不占优先级槽位。原实现只在请求遍历（`tryRoundGeneric` →
`AllowTransition()`）里判定 `circuit_interval` 到期——只要前面有健康源锁定成功，遍历就提前
返回，队尾熔断源永远不会被走到，`circuitOpen → halfOpen` 一次也不会发生，探测彻底失效
（线上表现为熔断源长时间停在 circuitOpen，永不进入半开）。
修复：新增与请求遍历解耦的 `AutoHalfOpen()`，由同一恢复线程/每轮前置评估按时间驱动迁移
（`evaluateRecoveries`），到期即进入 halfOpen 并恢复原始优先级位置。

**429 处理错误**
目前对 HTTP 429 做了特殊处理：不计入 breaker failure + 10s 延迟重试（其他源优先）。但 429 本质是限流，不应该有特殊路径——让它走正常的降级/熔断/探测流程即可。

本次重新设计引入 **基于超时的自动恢复** 机制，使降级和熔断态的源在间隔时间后自动获得探测机会，无需特判。

## 2. 目标与非目标

**目标**
- Degraded 源在 `degrade_interval` 内无新失败后恢复到原始优先级位置（机会窗口；健康状态保持 degraded，便于机会失败升级到 circuitOpen）
- `cooldown` 更名为 `circuit_interval`（默认 1m），作为 CircuitOpen→HalfOpen 探针间隔，现有 HalfOpen 机制不变
- 去掉 429 特判逻辑，429 走正常 RecordFailure 流程
- Degraded 时的 `moveToEnd` 保留（避免影响正常源延迟），auto-recovery 保证恢复
- 新配置项闭环：config 定义、默认值、校验、merge、env override

**非目标**
- 不改变 degrade → circuitOpen 路径（仍为 degrade_threshold 连续失败触发）
- 不改变跨层路由（不引入 level_fallback_attempts）
- 不改变 breaker 核心状态机结构（Normal/Degraded/CircuitOpen/HalfOpen 不变）
- 不持久化源健康状态

## 3. 状态机变更

### 现有状态机

```
Normal ── failStreak≥degrade_threshold ──→ Degraded (moveToEnd)
　　　　　　　　　　　　　　　　　　　　　　　　　│
Degraded ── failStreak≥degrade_threshold ──→ CircuitOpen
　　　　　　│
　　　　　　── successStreak≥recover_threshold ──→ Normal (restoreOriginal)

CircuitOpen ── cooldown到期 ──→ HalfOpen
　　　　　　　　　　　　　　　│
HalfOpen ── recover_threshold次成功 ──→ Normal/Degraded(按recovery)
　　　　　　── 失败 ──→ CircuitOpen(计时重置)
```

### 新增自动恢复路径（高亮）

```
Normal ── failStreak≥degrade_threshold ──→ Degraded (moveToEnd)
　　　　　　　　　　　　　　　　　　　　　　　　　│
Degraded ── failStreak≥degrade_threshold ──→ CircuitOpen
　　　　　　│
　　　　　　── successStreak≥recover_threshold ──→ Normal (restoreOriginal)
　　　　　　── ★ degrade_interval 内无新失败 ──→ 恢复原始位置(机会窗口，状态仍 Degraded)
　　　　　　── 机会内失败 ──→ moveToEnd 等下一次机会；累计 N 次机会失败 ──→ CircuitOpen

CircuitOpen ── circuit_interval到期(默认1m) ──→ HalfOpen
　　　　　　　　　　　　　　　　　　　│
HalfOpen ── recover_threshold次成功 ──→ Normal/Degraded(按recovery)
　　　　　　── 失败 ──→ CircuitOpen(计时重置)
```

**关键行为**：
- Degraded 在 `degrade_interval` 内无新失败后经 `restoreOriginal()` 恢复到原始优先级位置(机会窗口)，健康状态保持 degraded，degradeCount 不清零
- 机会窗口内请求成功(recover_threshold 次) → 转 Normal；机会窗口内失败 → moveToEnd 重新排到队尾等下一次机会；failStreak 累计且仅成功清零，累计 N 次机会失败 → CircuitOpen
- `RecordFailure()` 重置 `degradedAt` 计时器。429 不再特判，因此 429 也正常重置计时

## 4. 配置变更

### BreakerCfg 新增字段

```yaml
breaker:
  degrade_interval: 30s       # 新增，默认 30s。Degraded 源超时后恢复到原始位置进入机会窗口的间隔
  circuit_interval: 1m        # 默认 1m。CircuitOpen→HalfOpen 探针间隔（替代原 cooldown）
  degrade_threshold: 3        # 不变
  recover_threshold: 1        # 不变。HalfOpen 需要连续成功次数
  recovery: "normal"          # 不变
  first_byte_timeout: 12s     # 不变
  half_open_probes: 1         # 不变
  max_retries: 0              # 不变
```

`DegradeInterval`（YAML key `degrade_interval`）以 Duration 类型存储，0 值在 `applyDefaults` 中覆盖为默认 30s。`CircuitInterval`（YAML key `circuit_interval`）同理，默认 1m。per-source 级别均支持配置覆盖。

### Breaker 新增字段

```go
type Breaker struct {
    // ... 现有字段
    degradedAt time.Time  // 记录进入 Degraded 的时刻，用于自动恢复判断
}
```

### 新增方法

```go
// AutoProbe 是「无请求驱动的冷却迁移」唯一入口：一次评估 degraded / circuitOpen
// 是否到期并迁移，两者共用同一份 cooldownTransition，仅间隔与目标状态不同。
//   - Degraded 超过 degrade_interval 无新失败 -> (Degraded, Degraded, true)：
//     调度器恢复原始优先级位置，状态保持 degraded（机会窗口）。
//   - CircuitOpen 超过 circuit_interval -> (CircuitOpen, HalfOpen, true)：
//     熔断源被排除出候选队列，必须改状态才有机会被探测。
// 返回 (oldState, newState, ok)，scheduler 据此调用 restoreOriginal。
func (b *Breaker) AutoProbe() (State, State, bool)
```

### 对现有方法的变更

- `RecordFailure()`：当状态为 Degraded 时，重置 `degradedAt = now()`。保持现有 degrade→circuitOpen 逻辑不变。
- `RecordSuccess()`：当状态为 Degraded 时，重置 `degradedAt = zero`（不再 Degraded 了）。其余不变。
- `ForceNormal()`：重置 `degradedAt = zero`。
- `Allow()`：不需要变更。Normal/Degraded 始终允许通过。

### 去掉 429 特判

撤销 `internal/scheduler/scheduler.go` 中 `trySourceGeneric` 和 `tryRoundGeneric` 的 429 特殊处理逻辑：

- `trySourceGeneric`：所有错误统一走 `RecordFailure()`
- `tryRoundGeneric`：去掉 `rateLimited` 源跟踪和延迟重试队列
- 删除 `Scheduler.rateLimitRetryDelay` 字段
- `backend.StatusCodeFromErr` 保持导出（已有用途，且其他包可能依赖）

## 5. Scheduler 变更

### evaluateRecoveries()：统一冷却迁移前置

后台恢复线程（每分钟）与每轮 `tryRoundGeneric` 前置都调用一次 `evaluateRecoveries()`，
对每个源调用 `breaker.AutoProbe()`，两种冷却态走同一份迁移实现；到时机后统一
`restoreOriginal`（degraded 与 circuitOpen 的动作都只是「归还运行优先级」）。

```go
func (s *Scheduler) evaluateRecoveries() {
    // 必须遍历 healthSeq：circuitOpen 源不占运行时优先级槽位，
    // runtimeSeq 不收录它们，只用 runtimeSeq 会漏掉队尾熔断源的冷却迁移。
    for _, src := range s.healthSeq() {
        if src.Disabled {
            continue
        }
        s.autoProbe(&src)
    }
}

func (s *Scheduler) autoProbe(src *config.Source) {
    bk := s.breakerFor(src)
    oldState, newState, ok := bk.AutoProbe()
    if !ok {
        return
    }
    s.restoreOriginal(src.Name) // degraded/circuitOpen 都只是归还优先级
    slog.Info("上游源冷却到期恢复运行优先级",
        "source", src.Name, "old_state", oldState, "new_state", newState)
}
```

该方案的性能影响：`evaluateRecoveries` 遍历所有源（通常 < 20 个），每次 `AutoProbe()`
仅做一次 `time.Time` 比较，开销可以忽略。

## 6. 测试计划

### Breaker 单元测试

| 测试 | 验证点 |
|------|--------|
| TestAutoProbeDegradedElapsed | Degraded 后超时，AutoProbe() 返回 ok=true 且状态保持 Degraded |
| TestAutoProbeFailureResetsDegradedAt | Degraded 后 RecordFailure 重置计时器，AutoProbe 不生效 |
| TestAutoProbeDegradedNotElapsed | Degraded 后未到间隔，AutoProbe 不生效 |
| TestAutoProbeCircuitOpenElapsed | circuitOpen 冷却到期，AutoProbe() 返回 ok=true 且迁到 halfOpen |

### Scheduler 集成测试

| 测试 | 验证点 |
|------|--------|
| TestSchedulerAutoProbeDegradedSource | AutoProbe 后 scheduler 调用 restoreOriginal，顺序恢复 |
| TestDegradedWithMixOf429And500 | 429 不再特判，正常触发 degrade，auto-recover 仍生效 |
| Test429DegradesNormally | 源连续返回 429，正常触发 degrade→moveToEnd |

## 7. 实现步骤

1. Config: BreakerCfg `cooldown` 改为 `CircuitInterval`（YAML key `circuit_interval`），新增 `DegradeInterval`（YAML key `degrade_interval`）；设默认值 `degrade_interval=30s`、`circuit_interval=1m`；更新 `applyDefaults`、`BreakerFor` merge、env override
2. Breaker: 新增 `degradedAt` 字段、`AutoRecover()` 方法；`RecordFailure()` 在 Degraded 态时重置 `degradedAt`
3. Scheduler: 新增 `autoRecoverDegraded()`；在 `tryRoundGeneric` 开头调用；去掉 429 特判逻辑和 `rateLimitRetryDelay`
4. `Cooldown` 字段替换为 `CircuitInterval`（YAML key `circuit_interval`），默认 1m
5. 更新 `config.example.yaml`
6. 更新 breaker 和 scheduler 测试

## 8. 遗留问题

- `recover_threshold` 同时用于 Degraded→Normal（机会窗口内真实请求成功恢复）和 HalfOpen→Normal（探测恢复）。degrade_interval 超时只恢复原始优先级位置、不清健康状态，不替代 recover_threshold 的成功计数。
