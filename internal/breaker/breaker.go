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

// Allow reports whether a request may proceed. In circuitOpen state it
// transitions to halfOpen after the circuit_interval elapses.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.st {
	case Normal, Degraded:
		return true
	case CircuitOpen:
		if b.now().Sub(b.openedAt) >= time.Duration(b.cfg.CircuitInterval) {
			b.st = HalfOpen
			b.halfOpenInflight = 1 // count this probe
			return true
		}
		return false
	case HalfOpen:
		if b.halfOpenInflight < b.cfg.HalfOpenProbes {
			b.halfOpenInflight++
			return true
		}
		return false
	}
	return true
}

// RecordFailure records a failure and returns the new State.
// normal -> degraded (after degradeThreshold consecutive failures)
// degraded -> circuitOpen (after degradeThreshold more consecutive failures)
// halfOpen probe failure -> circuitOpen (circuit_interval reset)
func (b *Breaker) RecordFailure() State {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Defensive: when already circuitOpen, don't accumulate failStreak /
	// degradeCount or reset openedAt. This avoids edge-case behaviour when
	// HalfOpenProbes > 1 and multiple probe failures arrive.
	if b.st == CircuitOpen {
		return b.st
	}

	b.successStreak = 0
	b.failStreak++

	if b.st == HalfOpen {
		b.st = CircuitOpen
		b.openedAt = b.now()
		b.halfOpenInflight = 0
		return b.st
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
	return b.st
}

// RecordSuccess records a success and returns the new State.
// degraded -> normal (after recoverThreshold consecutive successes)
// halfOpen probe success -> recovery per cfg.Recovery ("normal" | "degraded")
func (b *Breaker) RecordSuccess() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failStreak = 0
	b.successStreak++

	if b.st == HalfOpen {
		b.halfOpenInflight = 0
		switch b.cfg.Recovery {
		case "degraded":
			b.st = Degraded
			b.degradeCount = 1
		default: // "normal"
			b.st = Normal
			b.degradeCount = 0
			b.successStreak = 0
		}
		return b.st
	}

	if b.st == Degraded && b.successStreak >= b.cfg.RecoverThreshold {
		b.st = Normal
		b.degradeCount = 0
		b.successStreak = 0
		b.degradedAt = time.Time{}
	}
	return b.st
}

// AutoRecover 检查 degraded 源是否已超过 degrade_interval 无新失败。
// 若超时且无新失败（degradedAt 未被 RecordFailure 重置），自动升回 Normal。
// 返回 (oldState, newState, true) 表示已恢复，否则 (st, st, false)。
func (b *Breaker) AutoRecover() (State, State, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st != Degraded {
		return b.st, b.st, false
	}
	interval := time.Duration(b.cfg.DegradeInterval)
	if interval <= 0 {
		return b.st, b.st, false
	}
	if b.now().Sub(b.degradedAt) >= interval {
		b.st = Normal
		b.degradeCount = 0
		b.successStreak = 0
		b.degradedAt = time.Time{}
		return Degraded, Normal, true
	}
	return b.st, b.st, false
}

// SetDegradedAt 设置 degradedAt 时间戳，供 scheduler 测试使用。
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
