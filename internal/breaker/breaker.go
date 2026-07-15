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
	// CircuitOpen means the source is temporarily skipped until cooldown elapses.
	CircuitOpen
	// HalfOpen means limited probe requests are allowed after cooldown.
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
// transitions to halfOpen after the cooldown elapses.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.st {
	case Normal, Degraded:
		return true
	case CircuitOpen:
		if b.now().Sub(b.openedAt) >= time.Duration(b.cfg.Cooldown) {
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
// halfOpen probe failure -> circuitOpen (cooldown reset)
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
		}
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
	}
	return b.st
}
