package breaker

import (
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// cfg is a test helper that builds a BreakerCfg with sensible defaults.
func cfg(degrade, recover int, recovery string) config.BreakerCfg {
	return config.BreakerCfg{
		DegradeThreshold: degrade,
		RecoverThreshold: recover,
		Cooldown:         config.Duration(30 * time.Second),
		HalfOpenProbes:   1,
		Recovery:         recovery,
	}
}

// advanceTime injects a future clock into the breaker for cooldown testing.
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
	st := b.RecordSuccess() // recover=1 -> normal
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
	st := b.RecordSuccess() // 2nd success -> normal
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
	st := b.RecordFailure() // 3rd failure in degraded -> circuitOpen
	if st != CircuitOpen {
		t.Fatalf("expected circuitOpen, got %v", st)
	}
	if b.DegradeCount() != 2 {
		t.Fatalf("expected degradeCount=2, got %d", b.DegradeCount())
	}
	if b.Allow() {
		t.Fatal("circuitOpen should not allow before cooldown")
	}
}

// --- circuitOpen -> halfOpen -> recovery ---

func TestCircuitOpenHalfOpenRecoveryNormal(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 6; i++ {
		b.RecordFailure() // -> degraded -> circuitOpen
	}
	advanceTime(b, 31*time.Second) // past cooldown
	if !b.Allow() {
		t.Fatal("should halfOpen after cooldown")
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected halfOpen, got %v", b.State())
	}
	st := b.RecordSuccess() // recovery=normal -> normal
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
		t.Fatal("should halfOpen after cooldown")
	}
	st := b.RecordSuccess() // recovery=degraded -> degraded
	if st != Degraded {
		t.Fatalf("should recover to degraded, got %v", st)
	}
	if b.DegradeCount() != 1 {
		t.Fatalf("degradeCount should be 1 after degraded recovery, got %d", b.DegradeCount())
	}
}

// --- halfOpen probe failure resets cooldown ---

func TestHalfOpenFailResets(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 6; i++ {
		b.RecordFailure() // -> circuitOpen
	}
	advanceTime(b, 31*time.Second) // past cooldown
	b.Allow()                      // -> halfOpen
	st := b.RecordFailure()        // probe fail -> circuitOpen, openedAt reset
	if st != CircuitOpen {
		t.Fatalf("expected circuitOpen after probe failure, got %v", st)
	}
	// Should not allow immediately (cooldown reset)
	if b.Allow() {
		t.Fatal("should not allow immediately after probe failure (cooldown reset)")
	}
	// After another cooldown period, should allow again
	advanceTime(b, 31*time.Second)
	if !b.Allow() {
		t.Fatal("should allow after second cooldown period")
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

func TestHalfOpenProbesLimit(t *testing.T) {
	b := New(config.BreakerCfg{
		DegradeThreshold: 1,
		RecoverThreshold: 1,
		Cooldown:         config.Duration(30 * time.Second),
		HalfOpenProbes:   1,
		Recovery:         "normal",
	})
	b.RecordFailure() // normal -> degraded
	b.RecordFailure() // degraded -> circuitOpen
	advanceTime(b, 31*time.Second)

	if !b.Allow() {
		t.Fatal("first Allow (transition) should succeed")
	}
	// With HalfOpenProbes=1, the transition consumed the slot
	if b.Allow() {
		t.Fatal("second Allow should be rejected (probes exhausted)")
	}
}

func TestHalfOpenProbesLimitMultiple(t *testing.T) {
	b := New(config.BreakerCfg{
		DegradeThreshold: 1,
		RecoverThreshold: 1,
		Cooldown:         config.Duration(30 * time.Second),
		HalfOpenProbes:   2,
		Recovery:         "normal",
	})
	b.RecordFailure() // -> degraded
	b.RecordFailure() // -> circuitOpen
	advanceTime(b, 31*time.Second)

	if !b.Allow() {
		t.Fatal("first Allow (transition) should succeed")
	}
	if !b.Allow() {
		t.Fatal("second Allow should succeed (HalfOpenProbes=2)")
	}
	if b.Allow() {
		t.Fatal("third Allow should be rejected (probes exhausted)")
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
	st := b.RecordSuccess()
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
	st := b.RecordFailure()
	if st != CircuitOpen {
		t.Fatalf("circuitOpen + failure should stay circuitOpen, got %v", st)
	}
}

// TestRecordFailureOnCircuitOpenNoSideEffects (M4) verifies the defensive
// early-return: when already circuitOpen, RecordFailure must not accumulate
// failStreak/degradeCount or reset openedAt (which would extend cooldown).
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
		if st := b.RecordFailure(); st != CircuitOpen {
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
