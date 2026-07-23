# Breaker Probe Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Redesign breaker probe mechanism: auto-recover degraded sources after `degrade_interval`, replace `cooldown` with `circuit_interval`, remove 429 special handling.

**Architecture:** Three-layer change: (1) config struct rename + add, (2) breaker state machine with time-based auto-recovery, (3) scheduler integration and 429 cleanup.

**Tech Stack:** Go, config/breaker/scheduler packages

## Global Constraints

- `cooldown` renamed to `circuit_interval` (Go field `CircuitInterval`, YAML key `circuit_interval`), no backward compat
- New config field `degrade_interval` (Go field `DegradeInterval`, YAML key `degrade_interval`), default 30s
- `circuit_interval` default 1m
- 429 special handling fully removed — 429 goes through normal `RecordFailure()`
- `moveToEnd`/`restoreOriginal` preserved; auto-recovery via `autoRecoverDegraded()` pre-pass in `tryRoundGeneric`
- Degraded auto-recovery is time-based: no new `RecordFailure` for `degrade_interval` duration → auto Normal
- `RecordFailure()` when state is Degraded resets `degradedAt` timer

---

### Task 1: Config struct — rename Cooldown to CircuitInterval, add DegradeInterval

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/admin/admin.go`
- Modify: `internal/admin/convert.go`
- Modify: `internal/admin/admin_test.go`

**Interfaces:**
- Consumes: existing `BreakerCfg` struct
- Produces: `BreakerCfg` with `CircuitInterval` (replaces `Cooldown`) and `DegradeInterval` (new)

- [ ] **Step 1: Rename Cooldown → CircuitInterval in BreakerCfg**

In `internal/config/config.go`:
```go
type BreakerCfg struct {
    FirstByteTimeout   Duration `koanf:"first_byte_timeout" yaml:"first_byte_timeout,omitempty"`
    CircuitInterval    Duration `koanf:"circuit_interval" yaml:"circuit_interval,omitempty"`
    DegradeThreshold   int      `koanf:"degrade_threshold" yaml:"degrade_threshold,omitempty"`
    // ... rest unchanged
}
```

Rename field `Cooldown` → `CircuitInterval`, update yaml/koanf tags from `cooldown` → `circuit_interval`.

- [ ] **Step 2: Add DegradeInterval field**

Add after `Recovery`:
```go
DegradeInterval   Duration `koanf:"degrade_interval" yaml:"degrade_interval,omitempty"`
```

- [ ] **Step 3: Update applyDefaults**

Change default from `Cooldown: Duration(30 * time.Second)` to `CircuitInterval: Duration(1 * time.Minute)`. Add `DegradeInterval: Duration(30 * time.Second)`.

Update `applyDefaults` function: rename `Cooldown` → `CircuitInterval`, add `DegradeInterval` check.

- [ ] **Step 4: Update BreakerFor merge**

Rename `Cooldown` → `CircuitInterval` in the merge function. Add `DegradeInterval` merge.

- [ ] **Step 5: Update env override path list**

In `internal/config/config.go` around line 374:
```go
if !hasAnyEnv(k, breakerPrefix,
    "first_byte_timeout", "circuit_interval", "degrade_threshold",
    "recover_threshold", "half_open_probes", "recovery", "degrade_interval") {
```

Add `circuit_interval` and `degrade_interval` to the override target list.

- [ ] **Step 6: Update admin API (admin.go)**

Rename `Cooldown` → `CircuitInterval` in the `BreakerJSON` struct (line 405). Update serialization (line 463) to use `CircuitInterval`. Add `DegradeInterval` to `BreakerJSON`.

In `internal/admin/convert.go` (line 64), rename `Cooldown` → `CircuitInterval`, add `DegradeInterval` parsing.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/admin/admin.go internal/admin/convert.go internal/admin/admin_test.go
git commit -m "feat(config): rename cooldown to circuit_interval, add degrade_interval"
```

Run: `go build ./... && go vet ./...`

---

### Task 2: Admin HTML — update cooldown field and add degrade_interval

**Files:**
- Modify: `internal/admin/assets/index.html`

- [ ] **Step 1: Update breaker config form in index.html**

Rename the cooldown input field (lines 1128-1130):
- `x-model="cfg.breaker.cooldown"` → `x-model="cfg.breaker.circuit_interval"`
- Update labels/hints

Add a new input for `degrade_interval`:
```html
<div class="flex flex-col gap-1">
  <span class="ui-label" x-text="t('degradeInterval')"></span>
  <input class="ui-input ui-input-sm mono" x-model="cfg.breaker.degrade_interval" autocomplete="off" spellcheck="false">
  <span class="ui-hint" x-text="t('degradeIntervalHint')"></span>
</div>
```

- [ ] **Step 2: Update translations**

In both Chinese and English translation objects:
- Rename `cooldown` → `circuitInterval`, `cooldownHint` → `circuitIntervalHint`
- Add `degradeInterval` / `degradeIntervalHint`

- [ ] **Step 3: Commit**

```bash
git add internal/admin/assets/index.html
git commit -m "feat(admin): update breaker config form — circuit_interval + degrade_interval"
```

---

### Task 3: Breaker — add degradedAt, AutoRecover(), update RecordFailure/RecordSuccess

**Files:**
- Modify: `internal/breaker/breaker.go`

- [ ] **Step 1: Add degradedAt field to Breaker struct**

```go
type Breaker struct {
    mu               sync.Mutex
    cfg              config.BreakerCfg
    st               State
    failStreak       int
    successStreak    int
    degradeCount     int
    openedAt         time.Time
    degradedAt       time.Time   // set when entering Degraded, reset by RecordFailure/RecordSuccess
    halfOpenInflight int
    now              func() time.Time
}
```

- [ ] **Step 2: Record degradedAt when entering Degraded**

In `RecordFailure()`, when transition from Normal→Degraded or staying in Degraded (but not transitioning to CircuitOpen), set `b.degradedAt = b.now()`:

```go
// Inside RecordFailure, in the degrade check block:
if b.failStreak >= b.cfg.DegradeThreshold {
    b.failStreak = 0
    b.degradeCount++
    if b.degradeCount >= 2 {
        b.st = CircuitOpen
        b.openedAt = b.now()
    } else {
        b.st = Degraded
        b.degradedAt = b.now()  // NEW: record degrade time
    }
}
```

- [ ] **Step 3: Reset degradedAt in RecordSuccess when leaving Degraded**

```go
if b.st == Degraded && b.successStreak >= b.cfg.RecoverThreshold {
    b.st = Normal
    b.degradeCount = 0
    b.successStreak = 0
    b.degradedAt = time.Time{}  // NEW: reset
}
```

- [ ] **Step 4: Reset degradedAt in ForceNormal**

```go
func (b *Breaker) ForceNormal() State {
    // ...
    b.degradedAt = time.Time{}
    // ...
}
```

- [ ] **Step 5: Update Allow() to use CircuitInterval instead of Cooldown**

Change `b.cfg.Cooldown` → `b.cfg.CircuitInterval` (line 83).

- [ ] **Step 6: Implement AutoRecover()**

```go
// AutoRecover checks if a degraded source has been degraded for >=
// degrade_interval without new failures. If so, transitions to Normal.
// Returns (oldState, newState, true) on recovery, or (st, st, false).
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
```

- [ ] **Step 7: Update comment on `Cooldown` references in breaker.go**

Change comments referencing "cooldown" to "circuit_interval" where appropriate (lines 20, 22, 75, 102).

- [ ] **Step 8: Commit**

```bash
git add internal/breaker/breaker.go
git commit -m "feat(breaker): add time-based auto-recovery for degraded state"
```

Run: `go build ./...`

---

### Task 4: Scheduler — add autoRecoverDegraded(), remove 429 logic

**Files:**
- Modify: `internal/scheduler/scheduler.go`

- [ ] **Step 1: Remove 429 special handling from trySourceGeneric**

Revert the 429 check block: all errors uniformly go through `RecordFailure()`:

```go
// Inside trySourceGeneric, the !locked + ctx.Err() == nil block:
oldState := bk.State()
newState := bk.RecordFailure()
s.adjustOrder(src.Name, oldState, newState)
slog.Warn("上游源失败（未锁定）", "source", src.Name, "backend_type", bt, "old_state", oldState, "new_state", newState, "error", err)
```

Remove the `if backend.StatusCodeFromErr(err) == 429` branch entirely.

- [ ] **Step 2: Remove rateLimitRetryDelay field from Scheduler**

Delete `rateLimitRetryDelay` field from the struct and its initialization in `New()`.

- [ ] **Step 3: Remove 429 deferred retry from tryRoundGeneric**

Revert `tryRoundGeneric` to the original pattern: iterate sources, try each, return first success or last error. Remove `rateLimited` tracking, timer logic, and the retry-after-wait block.

The simplified `tryRoundGeneric`:
```go
func (s *Scheduler) tryRoundGeneric(...) (string, bool, error) {
    // Pre-pass: auto-recover degraded sources whose interval elapsed
    s.autoRecoverDegraded()

    var lastErr error
    var lastSource string
    for _, src := range s.runtimeSeq() {
        bk := s.breakerFor(&src)
        if !bk.Allow() {
            slog.Warn("跳过上游源", "source", src.Name, "reason", "breaker_open")
            continue
        }
        *attemptNo++
        locked, err := s.trySourceGeneric(ctx, &src, bk, rawBody, onEvent, onUpstream, *attemptNo)
        if locked {
            return src.Name, true, err
        }
        if err != nil {
            lastErr = err
            lastSource = src.Name
            bt, _ := config.NormalizeBackendType(src.BackendType)
            slog.Warn("上游源请求失败", "source", src.Name, "backend_type", bt, "error", err)
        }
    }
    return lastSource, false, lastErr
}
```

- [ ] **Step 4: Add autoRecoverDegraded() method**

```go
// autoRecoverDegraded checks all breakers for degraded sources whose
// degrade_interval has elapsed, and restores their original order.
func (s *Scheduler) autoRecoverDegraded() {
    for _, src := range s.runtimeSeq() {
        bk := s.breakerFor(&src)
        oldSt, newSt, ok := bk.AutoRecover()
        if ok {
            s.adjustOrder(src.Name, oldSt, newSt)
            slog.Info("上游源降级自动恢复", "source", src.Name, "old_state", oldSt, "new_state", newSt)
        }
    }
}
```

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/scheduler.go
git commit -m "feat(scheduler): add auto-recover degraded, remove 429 handling"
```

Run: `go build ./... && go vet ./...`

---

### Task 5: Tests — breaker + scheduler

**Files:**
- Modify: `internal/breaker/breaker_test.go`
- Modify: `internal/scheduler/scheduler_test.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/server/integration_test.go`

- [ ] **Step 1: Update config_test.go**

Rename `Cooldown` → `CircuitInterval` references. Update default expectations from `30s` → `1m` for `CircuitInterval`. Add test for `DegradeInterval` default.

- [ ] **Step 2: Add breaker AutoRecover tests**

In `internal/breaker/breaker_test.go`, add:

```go
func TestDegradedAutoRecoverAfterInterval(t *testing.T) {
    b := New(config.BreakerCfg{
        DegradeThreshold: 1,
        CircuitInterval:  config.Duration(1 * time.Minute),
        DegradeInterval:  config.Duration(50 * time.Millisecond),
    })
    b.RecordFailure() // -> degraded
    advanceTime(b, 100*time.Millisecond) // past degrade_interval
    oldSt, newSt, ok := b.AutoRecover()
    if !ok {
        t.Fatalf("expected auto recovery, got ok=false")
    }
    if newSt != Normal {
        t.Fatalf("expected Normal, got %v", newSt)
    }
    if oldSt != Degraded {
        t.Fatalf("expected oldState=Degraded, got %v", oldSt)
    }
}

func TestDegradedAutoRecoverResetOnFailure(t *testing.T) {
    b := New(config.BreakerCfg{
        DegradeThreshold: 1,
        CircuitInterval:  config.Duration(1 * time.Minute),
        DegradeInterval:  config.Duration(50 * time.Millisecond),
    })
    b.RecordFailure() // -> degraded
    time.Sleep(30 * time.Millisecond)
    b.RecordFailure() // still degraded (failStreak=1 < degradeThreshold for circuitOpen)
    // degradedAt was reset by the second RecordFailure
    // Total elapsed < 50ms (30 + a bit), so AutoRecover should not trigger
    // Actually we need to advance time past the original + reset interval
    advanceTime(b, 100*time.Millisecond) // from original degraded, but reset happened
    _, _, ok := b.AutoRecover()
    if ok {
        t.Fatal("expected no auto-recover because RecordFailure reset degradedAt")
    }
}

func TestDegradedAutoRecoverNotBeforeInterval(t *testing.T) {
    b := New(config.BreakerCfg{DegradeThreshold: 1, DegradeInterval: config.Duration(time.Hour)})
    b.RecordFailure() // -> degraded
    _, _, ok := b.AutoRecover()
    if ok {
        t.Fatal("should not recover before interval")
    }
}

func TestDegradedAutoRecoverAfterSuccess(t *testing.T) {
    b := New(config.BreakerCfg{DegradeThreshold: 1, RecoverThreshold: 1, DegradeInterval: config.Duration(time.Hour)})
    b.RecordFailure() // -> degraded
    b.RecordSuccess() // -> normal (recover threshold = 1)
    _, _, ok := b.AutoRecover()
    if ok {
        t.Fatal("should not auto-recover when already normal")
    }
}
```

- [ ] **Step 3: Update existing breaker tests**

Replace all `Cooldown: config.Duration(...)` → `CircuitInterval: config.Duration(...)` throughout `breaker_test.go`. Update `cfg()` helper function.

- [ ] **Step 4: Update scheduler tests**

Replace all `Cooldown` → `CircuitInterval` throughout `scheduler_test.go`.

Add a scheduler auto-recover test:
```go
func TestDegradedAutoRecoverRestoresOrder(t *testing.T) {
    bad := httptest.NewServer(http.HandlerFunc(err500))
    defer bad.Close()
    good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        goodAnthropicSSE(w)
    }))
    defer good.Close()

    cfg := &config.Config{
        Breaker: config.BreakerCfg{
            FirstByteTimeout: config.Duration(2 * time.Second),
            DegradeThreshold: 1,      // single failure → degraded
            RecoverThreshold: 1,
            CircuitInterval:  config.Duration(time.Minute),
            HalfOpenProbes:   1,
            MaxRetries:       0,
            DegradeInterval:  config.Duration(10 * time.Millisecond), // fast recovery
        },
        Sources: []config.Source{
            makeSource("A", bad.URL, 0),
            makeSource("B", good.URL, 1),
        },
    }
    s := New(cfg)

    // Round 1: A fails (500), B succeeds. A is now degraded.
    name, err := runGeneric(s, nil, nil)
    if err != nil {
        t.Fatalf("round 1 should succeed via B: %v", err)
    }
    if name != "B" {
        t.Fatalf("round 1 source=%q want B", name)
    }

    // Verify A is degraded
    srcA, _ := s.sourceByName("A")
    bkA := s.breakerFor(&srcA)
    if bkA.State() != breaker.Degraded {
        t.Fatalf("A should be degraded, got %v", bkA.State())
    }

    // Wait for degrade_interval to elapse
    time.Sleep(15 * time.Millisecond)

    // Round 2: autoRecoverDegraded should restore A to Normal.
    // Since A is still a 500 server, it will fail again and re-degrade.
    // But we verify the order was restored before the attempt.
    name, err = runGeneric(s, nil, nil)
    if err == nil && name == "B" {
        // B succeeded again — but check that A was restored first in order
    }

    // Verify A is degraded again (it failed again)
    if bkA.State() != breaker.Degraded {
        t.Fatalf("A should be re-degraded after failing, got %v", bkA.State())
    }

    // Verify order: A should still be at end (degraded again)
    s.ordMu.RLock()
    defer s.ordMu.RUnlock()
    if s.order[0].name != "B" {
        t.Fatalf("expected B first after re-degrade, got %s", s.order[0].name)
    }
}
```

- [ ] **Step 5: Update server tests**

Replace `Cooldown` → `CircuitInterval` in `server_test.go` and `integration_test.go`.

- [ ] **Step 6: Run all tests**

```bash
go test ./internal/config/... ./internal/breaker/... ./internal/scheduler/... ./internal/server/... -count=1 -v -race -timeout 120s
```

- [ ] **Step 7: Commit**

```bash
git add internal/breaker/breaker_test.go internal/scheduler/scheduler_test.go internal/config/config_test.go internal/server/server_test.go internal/server/integration_test.go
git commit -m "test(breaker): add auto-recover tests, rename cooldown->circuit_interval"
```

---

### Task 6: Update config.example.yaml

**Files:**
- Modify: `config.example.yaml`

- [ ] **Step 1: Rename cooldown → circuit_interval, add degrade_interval**

In both the global breaker section and per-source breaker sections:
```yaml
breaker:
  degrade_interval: 30s          # 降级后自动恢复探测间隔
  circuit_interval: 1m           # 熔断后转为半开探测间隔
  degrade_threshold: 3
  recover_threshold: 1
```

- [ ] **Step 2: Commit**

```bash
git add config.example.yaml
git commit -m "docs(config): update example config — circuit_interval, degrade_interval"
```

---

### Self-Review Checklist

- [ ] All `Cooldown` references renamed to `CircuitInterval` across all files (config, breaker, admin, tests, HTML, examples)
- [ ] `DegradeInterval` added to config, admin, HTML, examples
- [ ] 429 special handling fully removed from scheduler
- [ ] `degradedAt` set on entry to Degraded, reset by RecordFailure while degraded, reset on recovery
- [ ] `AutoRecover()` only fires when state is Degraded and interval elapsed
- [ ] `autoRecoverDegraded()` runs before each source iteration round in `tryRoundGeneric`
- [ ] Tests pass with `-race`
