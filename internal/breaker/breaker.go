// Package breaker implements per-source health state and circuit breaking.
package breaker

import (
	"fmt"
	"sync"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// State represents the health state of a source.
type State int

const (
	// Normal means the source is healthy and keeps its configured priority.
	Normal State = iota
	// Degraded means the source is still usable but moved behind healthier sources.
	Degraded
	// CircuitOpen means the source is temporarily skipped until circuit_interval elapses.
	CircuitOpen
	// HalfOpen means limited probe requests are allowed after circuit_interval.
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Normal:
		return "normal"
	case Degraded:
		return "degraded"
	case CircuitOpen:
		return "circuitOpen"
	case HalfOpen:
		return "halfOpen"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// Breaker is a per-source circuit breaker with two-level failover:
// normal -> degraded (序列后移) -> circuitOpen (熔断).
type Breaker struct {
	mu               sync.Mutex
	cfg              config.BreakerCfg
	st               State
	failStreak       int
	successStreak    int
	degradeCount     int // 0 normal, 1 degraded, 2 circuitOpen
	openedAt         time.Time
	degradedAt       time.Time // set when entering Degraded; reset by RecordFailure/RecordSuccess
	halfOpenInflight int
	now              func() time.Time // injectable for testing
}

// New constructs a breaker from config.
func New(cfg config.BreakerCfg) *Breaker {
	return &Breaker{cfg: cfg, st: Normal, now: time.Now}
}

// State returns the current breaker state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st
}

// DegradeCount returns the degrade level (0=normal, 1=degraded, 2=circuitOpen).
func (b *Breaker) DegradeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.degradeCount
}

// intervalFor 返回某个失败等级的超时等待参数：
// degraded 用 degrade_interval，circuitOpen 用 circuit_interval。
func (b *Breaker) intervalFor(st State) time.Duration {
	switch st {
	case Degraded:
		return time.Duration(b.cfg.DegradeInterval)
	case CircuitOpen:
		return time.Duration(b.cfg.CircuitInterval)
	default:
		return 0
	}
}

// recoverySuccesses 返回某个恢复阶段需要的连续成功次数：
// degraded 用 degraded_recovery_threshold，halfOpen 用 circuit_recovery_threshold。
func (b *Breaker) recoverySuccesses(st State) int {
	switch st {
	case Degraded:
		return b.cfg.DegradedRecoveryThreshold
	case HalfOpen:
		return b.cfg.CircuitRecoveryThreshold
	default:
		return 0
	}
}

// recoveryState 返回恢复阶段连续成功后的目标状态：
// degraded 恢复为 normal，halfOpen 按 recovery 配置恢复为 normal 或 degraded。
func (b *Breaker) recoveryState(st State) State {
	switch st {
	case Degraded:
		return Normal
	case HalfOpen:
		if b.cfg.Recovery == config.RecoveryDegraded {
			return Degraded
		}
		return Normal
	default:
		return Normal
	}
}

// applyRecovery 把状态迁移到 recoveryState 的结果，并重置等级计数。
// 降级与熔断共用同一恢复动作，只有目标等级不同。
func (b *Breaker) applyRecovery(target State) {
	b.successStreak = 0
	b.st = target
	if target == Degraded {
		b.degradeCount = 1
		b.degradedAt = b.now()
		return
	}
	b.degradeCount = 0
	b.degradedAt = time.Time{}
}

// cooldownTransition 是「冷却到期」的唯一迁移实现（调用方必须持有 b.mu）。
// degraded 与 circuitOpen 共用它，差别只在到期后的目标状态：
//   - Degraded：到 degrade_interval 且无新失败 -> 重置计时窗口，状态保持 degraded。
//     降级源本就仍可服务，只需归还优先级位置重新获得被尝试的机会。
//   - CircuitOpen：到 circuit_interval -> 迁移到 halfOpen。熔断源被排除出候选
//     队列，必须改状态才有机会被真正探测；探测名额不在此占用（halfOpenInflight=0），
//     由随后真正发起的请求经 AllowTransition 占用。
//
// 返回 (oldState, newState, ok)；Normal/HalfOpen 或未到时机时 ok=false。
func (b *Breaker) cooldownTransition() (State, State, bool) {
	switch b.st {
	case Degraded:
		interval := b.intervalFor(Degraded)
		if interval <= 0 || b.now().Sub(b.degradedAt) < interval {
			return b.st, b.st, false
		}
		// 重置计时窗口，避免每个轮询周期重复触发；状态仍保持 degraded。
		b.degradedAt = b.now()
		return Degraded, Degraded, true
	case CircuitOpen:
		if b.now().Sub(b.openedAt) < b.intervalFor(CircuitOpen) {
			return b.st, b.st, false
		}
		b.st = HalfOpen
		b.halfOpenInflight = 0
		b.successStreak = 0
		return CircuitOpen, HalfOpen, true
	default:
		return b.st, b.st, false
	}
}

// Allow reports whether a request may proceed. In circuitOpen state it
// transitions to halfOpen after the circuit_interval elapses.
func (b *Breaker) Allow() bool {
	allowed, _, _ := b.AllowTransition()
	return allowed
}

// AllowTransition 判定一次请求能否发往该源，并返回迁移前后的状态：
//   - Normal / Degraded：放行（order 不变）。
//   - CircuitOpen：冷却未到 -> 拒绝；已到 -> 先走与 AutoProbe 相同的
//     cooldownTransition 迁移到 halfOpen，再按 halfOpen 规则放行本次探测。
//     这里必须复用同一实现：否则「轮内冷却到期」与「轮前定时迁移」会出现
//     两套行为，且探测名额的占用语义容易走偏。
//   - HalfOpen：探测名额未满 -> 放行并占名额；已满 -> 拒绝。
func (b *Breaker) AllowTransition() (allowed bool, oldState, newState State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	oldState = b.st
	// 先处理冷却到期（circuitOpen -> halfOpen），与定时入口共用同一迁移。
	b.cooldownTransition()
	switch b.st {
	case Normal, Degraded:
		return true, oldState, b.st
	case HalfOpen:
		if b.halfOpenInflight < b.cfg.CircuitRecoveryThreshold {
			b.halfOpenInflight++ // 占用一个探测名额
			return true, oldState, b.st
		}
		return false, oldState, b.st
	default:
		// 仍为 CircuitOpen 且未到时机：拒绝，且不改变 order。
		return false, oldState, oldState
	}
}

// RecordFailure records a failure and returns the (old, new) State pair.
// 在锁内原子捕获迁移前后状态：调用方据此调整运行时 order，若在锁外先读
// State() 再 Record，两次加锁间隙的并发迁移会导致 order 与实际状态错位。
// normal -> degraded (after degradeThreshold consecutive failures)
// degraded -> circuitOpen (after degradeThreshold more consecutive failures)
// halfOpen probe failure -> circuitOpen (circuit_interval reset)
func (b *Breaker) RecordFailure() (State, State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	old := b.st

	// Defensive: when already circuitOpen, don't accumulate failStreak /
	// degradeCount or reset openedAt. This avoids edge-case behaviour when
	// CircuitRecoveryThreshold > 1 and multiple probe failures arrive.
	if b.st == CircuitOpen {
		return old, b.st
	}

	b.successStreak = 0
	b.failStreak++

	if b.st == HalfOpen {
		b.st = CircuitOpen
		b.openedAt = b.now()
		b.halfOpenInflight = 0
		return old, b.st
	}

	if b.failStreak >= b.cfg.DegradeThreshold {
		b.failStreak = 0
		b.degradeCount++
		if b.degradeCount >= 2 {
			b.st = CircuitOpen
			b.openedAt = b.now()
		} else {
			b.st = Degraded
			b.degradedAt = b.now()
		}
	} else if b.st == Degraded {
		// Remained degraded but didn't cross threshold: still a failure,
		// reset the auto-recovery timer.
		b.degradedAt = b.now()
	}
	return old, b.st
}

// RecordSuccess records a success and returns the (old, new) State pair
// （锁内原子捕获，理由见 RecordFailure）。
// degraded -> normal (after degradedRecoveryThreshold consecutive successes)
// halfOpen probe success -> recovery per cfg.Recovery ("normal" | "degraded")
func (b *Breaker) RecordSuccess() (State, State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	old := b.st
	b.failStreak = 0

	if b.st == HalfOpen {
		b.halfOpenInflight = 0
		b.successStreak++
		if b.successStreak < b.recoverySuccesses(HalfOpen) {
			// 半开探测需要连续成功达到 circuit_recovery_threshold 才按 recovery 恢复。
			return old, b.st
		}
		b.applyRecovery(b.recoveryState(HalfOpen))
		return old, b.st
	}

	b.successStreak++
	if b.st == Degraded && b.successStreak >= b.recoverySuccesses(Degraded) {
		b.applyRecovery(b.recoveryState(Degraded))
	}
	return old, b.st
}

// AutoProbe 是「无请求驱动的冷却迁移」唯一入口，由调度器的后台线程与每轮前置
// 各调用一次，一次评估当前冷却态是否到期并迁移（degraded / circuitOpen 共用
// cooldownTransition，只有间隔与目标状态不同）：
//   - Degraded 且超过 degrade_interval 无新失败 -> (Degraded, Degraded, true)：
//     调度器据此把源恢复到原始优先级（重新给被尝试的机会）。健康状态**保持
//     degraded**、degradeCount 不清零，后续连续失败仍能升级到 circuitOpen；
//     只有真实请求成功（RecordSuccess 达到 degraded_recovery_threshold）才转回 normal。
//   - CircuitOpen 且超过 circuit_interval -> (CircuitOpen, HalfOpen, true)：
//     熔断源被排除出候选队列，必须改状态才有机会被真正探测。
//
// 必须独立于请求遍历：degraded/circuitOpen 源都会被后移到运行时队尾，只要前面
// 有健康源锁定成功，遍历就提前返回，队尾源永远等不到迁移。ok=false 表示当前
// 状态无需迁移或未到时机；(oldState, newState) 供调度器决定是否调整运行顺序。
func (b *Breaker) AutoProbe() (State, State, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cooldownTransition()
}

// UpdateCfg 原子替换阈值配置，保留当前健康状态与计数。
// 热重载路径调用（scheduler.Reload）：管理页修改断路器参数即时生效，
// 不重置已有源的降级/熔断进度。
func (b *Breaker) UpdateCfg(cfg config.BreakerCfg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg = cfg
}

// SetDegradedAt 设置 degradedAt 时间戳。仅供测试拨动自动恢复计时；
// 生产路径禁止调用（会破坏状态机不变量）。
func (b *Breaker) SetDegradedAt(t time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.degradedAt = t
}

// ForceNormal 手动将源提升回 normal：清零失败/成功 streak、degradeCount，
// 并重置 halfOpen 探测计数。用于管理页人工干预。
func (b *Breaker) ForceNormal() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st = Normal
	b.failStreak = 0
	b.successStreak = 0
	b.degradeCount = 0
	b.halfOpenInflight = 0
	b.openedAt = time.Time{}
	b.degradedAt = time.Time{}
	return b.st
}
