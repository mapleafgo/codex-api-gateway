package breaker

import (
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// cfg is a test helper that builds a BreakerCfg with sensible defaults.
func cfg(degrade, recover int, recovery string) config.BreakerCfg {
	return config.BreakerCfg{
		DegradeThreshold:          degrade,
		DegradedRecoveryThreshold: recover,
		CircuitInterval:           config.Duration(30 * time.Second),
		DegradeInterval:           config.Duration(30 * time.Second),
		CircuitRecoveryThreshold:  1,
		Recovery:                  recovery,
	}
}

// advanceTime injects a future clock into the breaker for circuit_interval testing.
func advanceTime(b *Breaker, d time.Duration) {
	base := b.now()
	b.now = func() time.Time { return base.Add(d) }
}

// --- normal <-> degraded ---

func TestNormalToDegradedAfterThreshold(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 2; i++ {
		b.RecordFailure()
	}
	if b.State() != Normal {
		t.Fatalf("premature degrade: state=%v", b.State())
	}
	b.RecordFailure() // 3rd failure
	if b.State() != Degraded {
		t.Fatalf("expected degraded, got %v", b.State())
	}
	if b.DegradeCount() != 1 {
		t.Fatalf("expected degradeCount=1, got %d", b.DegradeCount())
	}
}

func TestDegradedRecoversOnSuccess(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 3; i++ {
		b.RecordFailure() // -> degraded
	}
	_, st := b.RecordSuccess() // recover=1 -> normal
	if st != Normal {
		t.Fatalf("expected normal after recovery, got %v", st)
	}
	if b.DegradeCount() != 0 {
		t.Fatalf("expected degradeCount=0, got %d", b.DegradeCount())
	}
}

func TestDegradedNeedsMultipleSuccesses(t *testing.T) {
	b := New(cfg(3, 2, "normal")) // recover threshold = 2
	for i := 0; i < 3; i++ {
		b.RecordFailure() // -> degraded
	}
	b.RecordSuccess() // 1st success, not enough
	if b.State() != Degraded {
		t.Fatalf("premature recovery: state=%v", b.State())
	}
	_, st := b.RecordSuccess() // 2nd success -> normal
	if st != Normal {
		t.Fatalf("expected normal after 2 successes, got %v", st)
	}
}

func TestDegradedStaysOnMixedFailures(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 3; i++ {
		b.RecordFailure() // -> degraded
	}
	b.RecordFailure() // failStreak=1, not enough for circuitOpen
	if b.State() != Degraded {
		t.Fatalf("should still be degraded, got %v", b.State())
	}
	b.RecordSuccess() // recover=1 -> normal
	if b.State() != Normal {
		t.Fatalf("should recover to normal, got %v", b.State())
	}
}

// --- degraded -> circuitOpen ---

func TestDegradedToCircuitOpen(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 3; i++ {
		b.RecordFailure() // -> degraded
	}
	for i := 0; i < 2; i++ {
		b.RecordFailure()
	}
	if b.State() != Degraded {
		t.Fatalf("should still be degraded before 3rd post-degrade failure, got %v", b.State())
	}
	_, st := b.RecordFailure() // 3rd failure in degraded -> circuitOpen
	if st != CircuitOpen {
		t.Fatalf("expected circuitOpen, got %v", st)
	}
	if b.DegradeCount() != 2 {
		t.Fatalf("expected degradeCount=2, got %d", b.DegradeCount())
	}
	if b.Allow() {
		t.Fatal("circuitOpen should not allow before circuit_interval")
	}
}

// --- circuitOpen -> halfOpen -> recovery ---

func TestCircuitOpenHalfOpenRecoveryNormal(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 6; i++ {
		b.RecordFailure() // -> degraded -> circuitOpen
	}
	advanceTime(b, 31*time.Second) // past circuit_interval
	if !b.Allow() {
		t.Fatal("should halfOpen after circuit_interval")
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected halfOpen, got %v", b.State())
	}
	_, st := b.RecordSuccess() // recovery=normal -> normal
	if st != Normal {
		t.Fatalf("should recover to normal, got %v", st)
	}
	if b.DegradeCount() != 0 {
		t.Fatalf("degradeCount should be 0 after normal recovery, got %d", b.DegradeCount())
	}
}

func TestCircuitOpenHalfOpenRecoveryDegraded(t *testing.T) {
	b := New(cfg(3, 1, "degraded"))
	for i := 0; i < 6; i++ {
		b.RecordFailure() // -> circuitOpen
	}
	advanceTime(b, 31*time.Second)
	if !b.Allow() {
		t.Fatal("should halfOpen after circuit_interval")
	}
	_, st := b.RecordSuccess() // recovery=degraded -> degraded
	if st != Degraded {
		t.Fatalf("should recover to degraded, got %v", st)
	}
	if b.DegradeCount() != 1 {
		t.Fatalf("degradeCount should be 1 after degraded recovery, got %d", b.DegradeCount())
	}
}

// TestHalfOpenRecoveryDegradedResetsDegradedAt 覆盖：半开探测成功恢复到 degraded
// 时，必须像 RecordFailure 进入 degraded 一样重新初始化 degradedAt。否则旧时间戳
// 会让 AutoRecover 立刻判定 degrade_interval 已超时，跳过正常的恢复冷却。
func TestHalfOpenRecoveryDegradedResetsDegradedAt(t *testing.T) {
	b := New(cfg(3, 1, "degraded"))
	for i := 0; i < 6; i++ {
		b.RecordFailure() // -> degraded -> circuitOpen
	}
	if b.State() != CircuitOpen {
		t.Fatalf("setup: want circuitOpen, got %v", b.State())
	}
	advanceTime(b, 31*time.Second) // past circuit_interval (30s)
	if !b.Allow() {
		t.Fatal("should halfOpen after circuit_interval")
	}
	_, st := b.RecordSuccess() // recovery=degraded -> degraded
	if st != Degraded {
		t.Fatalf("want degraded after halfOpen success, got %v", st)
	}

	b.mu.Lock()
	fresh := b.degradedAt
	now := b.now()
	b.mu.Unlock()
	if fresh.IsZero() {
		t.Fatal("degradedAt should be set after halfOpen->degraded recovery")
	}

	// 半开恢复成 degraded 后 degradedAt 必须重置为当前时刻（否则旧时间戳会让
	// AutoRecover 立即判定 degrade_interval 已超时），同一时刻不得自动恢复。
	if !fresh.Equal(now) {
		t.Fatalf("degradedAt should be fresh after halfOpen recovery: got %v want %v", fresh, now)
	}
	if _, _, recovered := b.AutoRecover(); recovered {
		t.Fatal("AutoRecover should be false immediately after halfOpen->degraded recovery")
	}

	// 超过 degrade_interval 后应恢复（机会窗口）。
	advanceTime(b, 31*time.Second)
	if _, _, recovered := b.AutoRecover(); !recovered {
		t.Fatal("AutoRecover should be true after degrade_interval")
	}
}

// --- halfOpen probe failure resets circuit_interval ---

func TestHalfOpenFailResets(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 6; i++ {
		b.RecordFailure() // -> circuitOpen
	}
	advanceTime(b, 31*time.Second) // past circuit_interval
	b.Allow()                      // -> halfOpen
	_, st := b.RecordFailure()     // probe fail -> circuitOpen, openedAt reset
	if st != CircuitOpen {
		t.Fatalf("expected circuitOpen after probe failure, got %v", st)
	}
	// Should not allow immediately (circuit_interval reset)
	if b.Allow() {
		t.Fatal("should not allow immediately after probe failure (circuit_interval reset)")
	}
	// After another circuit_interval period, should allow again
	advanceTime(b, 31*time.Second)
	if !b.Allow() {
		t.Fatal("should allow after second circuit_interval period")
	}
}

// --- counter mutual exclusion ---

func TestCountersResetOnOpposite(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	b.RecordFailure()
	b.RecordFailure() // failStreak=2
	b.RecordSuccess() // failStreak should be 0, successStreak=1
	// Now 2 failures should NOT degrade (failStreak was reset)
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != Normal {
		t.Fatalf("failStreak should have been reset by success, got state=%v", b.State())
	}
	// 3rd failure degrades
	b.RecordFailure()
	if b.State() != Degraded {
		t.Fatalf("expected degraded after 3 failures post-reset, got %v", b.State())
	}
}

func TestSuccessStreakResetOnFailure(t *testing.T) {
	b := New(cfg(3, 2, "normal")) // recover threshold = 2
	for i := 0; i < 3; i++ {
		b.RecordFailure() // -> degraded
	}
	b.RecordSuccess() // successStreak=1
	b.RecordFailure() // successStreak should be 0
	if b.State() != Degraded {
		t.Fatalf("should still be degraded, got %v", b.State())
	}
	// Now need 2 consecutive successes again
	b.RecordSuccess() // successStreak=1
	if b.State() != Degraded {
		t.Fatalf("1 success not enough (need 2), got %v", b.State())
	}
	b.RecordSuccess() // successStreak=2 -> normal
	if b.State() != Normal {
		t.Fatalf("expected normal after 2 successes, got %v", b.State())
	}
}

// --- halfOpen probe limits ---

func TestCircuitRecoveryThresholdLimit(t *testing.T) {
	b := New(config.BreakerCfg{
		DegradeThreshold:          1,
		DegradedRecoveryThreshold: 1,
		CircuitInterval:           config.Duration(30 * time.Second),
		DegradeInterval:           config.Duration(30 * time.Second),
		CircuitRecoveryThreshold:  1,
		Recovery:                  "normal",
	})
	b.RecordFailure() // normal -> degraded
	b.RecordFailure() // degraded -> circuitOpen
	advanceTime(b, 31*time.Second)

	if !b.Allow() {
		t.Fatal("first Allow (transition) should succeed")
	}
	// With CircuitRecoveryThreshold=1, the transition consumed the slot
	if b.Allow() {
		t.Fatal("second Allow should be rejected (probes exhausted)")
	}
}

func TestCircuitRecoveryThresholdLimitMultiple(t *testing.T) {
	b := New(config.BreakerCfg{
		DegradeThreshold:          1,
		DegradedRecoveryThreshold: 1,
		CircuitInterval:           config.Duration(30 * time.Second),
		DegradeInterval:           config.Duration(30 * time.Second),
		CircuitRecoveryThreshold:  2,
		Recovery:                  "normal",
	})
	b.RecordFailure() // -> degraded
	b.RecordFailure() // -> circuitOpen
	advanceTime(b, 31*time.Second)

	if !b.Allow() {
		t.Fatal("first Allow (transition) should succeed")
	}
	if !b.Allow() {
		t.Fatal("second Allow should succeed (CircuitRecoveryThreshold=2)")
	}
	if b.Allow() {
		t.Fatal("third Allow should be rejected (probes exhausted)")
	}
}

// TestHalfOpenRequiresProbeSuccesses 验证半开探测需要连续成功达到
// circuit_recovery_threshold 才按 recovery 策略恢复，首个成功不能提前恢复。
func TestHalfOpenRequiresProbeSuccesses(t *testing.T) {
	b := New(config.BreakerCfg{
		DegradeThreshold:          1,
		DegradedRecoveryThreshold: 1,
		CircuitInterval:           config.Duration(30 * time.Second),
		DegradeInterval:           config.Duration(30 * time.Second),
		CircuitRecoveryThreshold:  2,
		Recovery:                  "normal",
	})
	b.RecordFailure() // -> degraded
	b.RecordFailure() // -> circuitOpen
	advanceTime(b, 31*time.Second)

	if !b.Allow() {
		t.Fatal("first Allow (transition) should succeed")
	}
	if _, st := b.RecordSuccess(); st != HalfOpen {
		t.Fatalf("首次半开成功不应恢复，got %v", st)
	}
	if _, st := b.RecordSuccess(); st != Normal {
		t.Fatalf("连续 circuit_recovery_threshold 次成功后才应恢复 normal，got %v", st)
	}
}

// TestHalfOpenFailureResetsCircuitOpen 验证半开探测中途失败即回到 circuitOpen。
func TestHalfOpenFailureResetsCircuitOpen(t *testing.T) {
	b := New(config.BreakerCfg{
		DegradeThreshold:          1,
		DegradedRecoveryThreshold: 1,
		CircuitInterval:           config.Duration(30 * time.Second),
		DegradeInterval:           config.Duration(30 * time.Second),
		CircuitRecoveryThreshold:  2,
		Recovery:                  "normal",
	})
	b.RecordFailure() // -> degraded
	b.RecordFailure() // -> circuitOpen
	advanceTime(b, 31*time.Second)

	if !b.Allow() {
		t.Fatal("first Allow (transition) should succeed")
	}
	if _, st := b.RecordSuccess(); st != HalfOpen {
		t.Fatalf("首次成功应保持 halfOpen，got %v", st)
	}
	if _, st := b.RecordFailure(); st != CircuitOpen {
		t.Fatalf("半开失败应回到 circuitOpen，got %v", st)
	}
}

// --- normal state Allow always true ---

func TestNormalAlwaysAllows(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 10; i++ {
		if !b.Allow() {
			t.Fatal("normal should always allow")
		}
	}
}

// --- degraded state Allow always true ---

func TestDegradedAlwaysAllows(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 3; i++ {
		b.RecordFailure() // -> degraded
	}
	if !b.Allow() {
		t.Fatal("degraded should allow traffic")
	}
}

// --- failStreak resets after degrade transition ---

func TestFailStreakResetsAfterDegradeTransition(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 3; i++ {
		b.RecordFailure() // -> degraded, failStreak reset to 0
	}
	// Only 2 more failures should NOT trigger circuitOpen
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != Degraded {
		t.Fatalf("failStreak should have reset at degrade transition, got %v", b.State())
	}
	b.RecordFailure() // 3rd -> circuitOpen
	if b.State() != CircuitOpen {
		t.Fatalf("expected circuitOpen, got %v", b.State())
	}
}

// --- RecordSuccess on normal is a no-op state-wise ---

func TestRecordSuccessOnNormalStaysNormal(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	_, st := b.RecordSuccess()
	if st != Normal {
		t.Fatalf("normal + success should stay normal, got %v", st)
	}
}

// --- RecordFailure on circuitOpen stays circuitOpen (not halfOpen) ---

func TestRecordFailureOnCircuitOpenStaysCircuitOpen(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 6; i++ {
		b.RecordFailure() // -> circuitOpen
	}
	_, st := b.RecordFailure()
	if st != CircuitOpen {
		t.Fatalf("circuitOpen + failure should stay circuitOpen, got %v", st)
	}
}

// TestRecordFailureOnCircuitOpenNoSideEffects (M4) verifies the defensive
// early-return: when already circuitOpen, RecordFailure must not accumulate
// failStreak/degradeCount or reset openedAt (which would extend circuit_interval).
func TestRecordFailureOnCircuitOpenNoSideEffects(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 6; i++ {
		b.RecordFailure() // 3 -> degraded, 6 -> circuitOpen
	}

	b.mu.Lock()
	openedAtBefore := b.openedAt
	degradeCountBefore := b.degradeCount
	b.mu.Unlock()

	// Call RecordFailure enough times to exceed threshold (3 more) — without
	// the early-return guard, this would bump degradeCount and reset openedAt.
	for i := 0; i < 5; i++ {
		if _, st := b.RecordFailure(); st != CircuitOpen {
			t.Fatalf("should stay circuitOpen on extra failure #%d, got %v", i+1, st)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.openedAt.Equal(openedAtBefore) {
		t.Fatalf("openedAt should not be reset when already circuitOpen")
	}
	if b.degradeCount != degradeCountBefore {
		t.Fatalf("degradeCount should not increase when already circuitOpen: got %d, want %d",
			b.degradeCount, degradeCountBefore)
	}
}

// TestForceNormal 手动提升：任意状态重置为 normal，清零计数。
func TestForceNormal(t *testing.T) {
	b := New(cfg(2, 1, "normal"))
	b.RecordFailure()
	b.RecordFailure() // -> degraded
	if b.State() != Degraded {
		t.Fatalf("setup: want degraded, got %v", b.State())
	}
	st := b.ForceNormal()
	if st != Normal {
		t.Fatalf("ForceNormal state=%v", st)
	}
	if b.State() != Normal || b.DegradeCount() != 0 {
		t.Fatalf("state=%v degrade=%d", b.State(), b.DegradeCount())
	}
	// 再次失败应从 0 计数，不会立刻 degraded
	b.RecordFailure()
	if b.State() != Normal {
		t.Fatalf("fail streak should have reset, got %v", b.State())
	}
}

// TestForceNormalFromCircuitOpen 熔断态也可手动提升。
func TestForceNormalFromCircuitOpen(t *testing.T) {
	b := New(cfg(2, 1, "normal"))
	for i := 0; i < 4; i++ {
		b.RecordFailure()
	}
	if b.State() != CircuitOpen {
		t.Fatalf("setup: want circuitOpen, got %v", b.State())
	}
	if st := b.ForceNormal(); st != Normal {
		t.Fatalf("ForceNormal=%v", st)
	}
	if !b.Allow() {
		t.Fatal("after ForceNormal should allow")
	}
}

// --- AutoRecover (time-based degraded recovery) ---

func TestAutoRecoverDegradedElapsed(t *testing.T) {
	b := New(cfg(1, 1, "normal"))
	b.RecordFailure() // -> degraded
	if b.State() != Degraded {
		t.Fatalf("setup: want degraded, got %v", b.State())
	}
	// degradedAt is set when entering Degraded
	b.mu.Lock()
	start := b.degradedAt
	b.mu.Unlock()
	if start.IsZero() {
		t.Fatal("degradedAt should be set after entering Degraded")
	}

	// Advance past degrade_interval (cfg uses 30s)
	b.now = func() time.Time { return start.Add(31 * time.Second) }

	oldSt, newSt, recovered := b.AutoRecover()
	if !recovered {
		t.Fatal("AutoRecover should return true after degrade_interval")
	}
	if oldSt != Degraded {
		t.Fatalf("old state: want Degraded, got %v", oldSt)
	}
	if newSt != Degraded {
		t.Fatalf("new state: want Degraded (B 语义，超时只恢复机会、状态保持 degraded), got %v", newSt)
	}
	if b.State() != Degraded {
		t.Fatalf("state after AutoRecover: want Degraded (degraded 保留以便后续失败升级到熔断), got %v", b.State())
	}
}

func TestAutoRecoverDegradedNotElapsed(t *testing.T) {
	b := New(cfg(1, 1, "normal"))
	b.RecordFailure() // -> degraded
	b.mu.Lock()
	start := b.degradedAt
	b.mu.Unlock()

	// Only advance 5s (less than 30s degrade_interval)
	b.now = func() time.Time { return start.Add(5 * time.Second) }

	_, _, recovered := b.AutoRecover()
	if recovered {
		t.Fatal("AutoRecover should return false before degrade_interval elapses")
	}
	if b.State() != Degraded {
		t.Fatalf("state should stay Degraded, got %v", b.State())
	}
}

func TestAutoRecoverSkipsNormal(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	_, _, recovered := b.AutoRecover()
	if recovered {
		t.Fatal("AutoRecover on Normal should return false")
	}
}

func TestAutoRecoverSkipsCircuitOpen(t *testing.T) {
	b := New(cfg(1, 1, "normal"))
	b.RecordFailure() // -> degraded
	b.RecordFailure() // -> circuitOpen
	_, _, recovered := b.AutoRecover()
	if recovered {
		t.Fatal("AutoRecover on CircuitOpen should return false")
	}
}

func TestAutoRecoverPreservesDegradeCount(t *testing.T) {
	b := New(cfg(1, 1, "normal"))
	b.RecordFailure() // -> degraded (degradeCount=1)
	if b.DegradeCount() != 1 {
		t.Fatalf("setup: degradeCount=1, got %d", b.DegradeCount())
	}

	b.mu.Lock()
	start := b.degradedAt
	b.mu.Unlock()
	b.now = func() time.Time { return start.Add(31 * time.Second) }

	b.AutoRecover()
	if b.DegradeCount() != 1 {
		t.Fatalf("degradeCount should stay 1 after AutoRecover (保持 degraded 以允许升级到熔断), got %d", b.DegradeCount())
	}
}

// TestAutoRecoverPreservesCircuitOpenPath 覆盖 B 语义的核心动机：degrade 超时
// 只恢复机会、不清除 degradeCount；随后连续 DegradeThreshold 次失败必须能
// 从 degraded 升级到 circuitOpen，而不是被反复重置回 normal 导致永不熔断。
func TestAutoRecoverPreservesCircuitOpenPath(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 3; i++ {
		b.RecordFailure() // -> degraded (degradeCount=1)
	}
	if b.State() != Degraded {
		t.Fatalf("setup: want degraded, got %v", b.State())
	}

	b.mu.Lock()
	start := b.degradedAt
	b.mu.Unlock()
	b.now = func() time.Time { return start.Add(31 * time.Second) }

	if _, _, recovered := b.AutoRecover(); !recovered {
		t.Fatal("AutoRecover should return true after degrade_interval")
	}
	if b.State() != Degraded {
		t.Fatalf("超时后应保持 degraded, got %v", b.State())
	}

	// 再给三次机会：连续 3 次失败 -> degraded 升级到 circuitOpen。
	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}
	if b.State() != CircuitOpen {
		t.Fatalf("三次失败后应触发熔断 circuitOpen, got %v", b.State())
	}
}

func TestAutoRecoverFailureResetsDegradedAt(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 3; i++ {
		b.RecordFailure() // -> degraded
	}
	b.mu.Lock()
	firstDegradedAt := b.degradedAt
	b.mu.Unlock()

	// Advance 25s (less than 30s degrade_interval)
	b.now = func() time.Time { return firstDegradedAt.Add(25 * time.Second) }

	b.RecordFailure() // failure in degraded resets degradedAt

	b.mu.Lock()
	afterFailure := b.degradedAt
	b.mu.Unlock()

	if !afterFailure.After(firstDegradedAt) {
		t.Fatal("degradedAt should be reset after failure in Degraded state")
	}

	// AutoRecover at 25s from the new degradedAt should not yet recover
	b.now = func() time.Time { return afterFailure.Add(25 * time.Second) }
	_, _, recovered := b.AutoRecover()
	if recovered {
		t.Fatal("AutoRecover should not recover 25s after the new degradedAt")
	}

	// But after 31s from the new degradedAt it should
	b.now = func() time.Time { return afterFailure.Add(31 * time.Second) }
	_, _, recovered = b.AutoRecover()
	if !recovered {
		t.Fatal("AutoRecover should recover 31s after the new degradedAt")
	}
}
