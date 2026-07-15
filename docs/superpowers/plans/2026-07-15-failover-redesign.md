# 多源容灾体系重构 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把简单熔断 + 单轮 failover 升级为两级容灾（降级 + 熔断，跨请求持久）+ 单请求 backoff 重试。

**Architecture:** breaker 状态机扩展（normal/degraded/circuitOpen/halfOpen，连续失败降级后移、成功升回）；scheduler 维护运行时优先级序列 + Execute 重试外层循环；config 用 sources 列表序去 priority。server 不动。

**Tech Stack:** Go 1.26、gopkg.in/yaml.v3、openai-go/v3、anthropic-sdk-go

## Global Constraints

- backoff 固定硬编码 `[2s, 4s, 6s, 8s, 10s]`（封顶 10s），不放 config
- 源健康状态跨请求持久（内存）、进程重启重置
- 现有 trySource 逻辑保留：首字节锁、watchdog（`FirstByteTimeout`）、model resolve、SDK 类型
- `server` 包不改（`Execute` 签名不变）
- 默认值：`degrade_threshold=3`、`recover_threshold=1`、`max_retries=0`、`recovery=normal`、`cooldown=30s`、`half_open_probes=1`、`first_byte_timeout=12s`
- 旧字段 `priority`/`failure_threshold` 解析忽略并 log 告警，不报错
- config.yaml 含 api_key，输出/日志掩码
- 出站不用 `map[string]any`（继承 SDK 迁移约束）

## 接口契约（改造后）

```go
// config
type Source struct { Name, BaseURL, APIKey, DefaultModel string; ModelMap map[string]string; Breaker *BreakerCfg; OriginalIndex int }
type BreakerCfg struct {
    FirstByteTimeout, Cooldown Duration
    DegradeThreshold, RecoverThreshold, HalfOpenProbes, MaxRetries int
    Recovery string // "normal" | "degraded"
}
func (c *Config) OrderedSources() []Source // 列表序，不排序，OriginalIndex 已赋

// breaker
type State int // normal | degraded | circuitOpen | halfOpen
func New(cfg config.BreakerCfg) *Breaker
func (b *Breaker) Allow() bool
func (b *Breaker) RecordSuccess()   // 返回新 State（供 scheduler 判升回）
func (b *Breaker) RecordFailure()   // 返回新 State（供 scheduler 判降级）
func (b *Breaker) State() State
func (b *Breaker) DegradeCount() int

// scheduler（Execute 签名不变）
func (s *Scheduler) Execute(ctx context.Context, req *anthropic.MessageNewParams, onEvent func(*anthropic.MessageStreamEventUnion) error) error
```

> **RecordSuccess/RecordFailure 返回 State**：scheduler 需据返回值判断是否触发序列后移（降级）或恢复原位（升回），避免再调 State() 产生竞态。

---

### Task 1: config 改造 + breaker 状态机重写

config 与 breaker 紧耦合（breaker 用 BreakerCfg 字段），必须同任务改以保证编译自洽。

**Files:**
- Modify: `internal/config/config.go`、`internal/config/config_test.go`
- Rewrite: `internal/breaker/breaker.go`、`internal/breaker/breaker_test.go`

**Interfaces:**
- Produces: 见接口契约（config.Source/BreakerCfg/OrderedSources、breaker 状态机 API）

- [ ] **Step 1: 改 config.Source 去 Priority 加 OriginalIndex**

`internal/config/config.go`：
```go
type Source struct {
	Name         string            `yaml:"name"`
	BaseURL      string            `yaml:"base_url"`
	APIKey       string            `yaml:"api_key"`
	ModelMap     map[string]string `yaml:"model_map"`
	DefaultModel string            `yaml:"default_model"`
	Breaker      *BreakerCfg       `yaml:"breaker"`
	OriginalIndex int              `yaml:"-"` // 运行时赋，列表序
}
```
删 `Priority int` 字段。

- [ ] **Step 2: 改 BreakerCfg 字段**

```go
type BreakerCfg struct {
	FirstByteTimeout  Duration `yaml:"first_byte_timeout"`
	Cooldown          Duration `yaml:"cooldown"`
	DegradeThreshold  int      `yaml:"degrade_threshold"`
	RecoverThreshold  int      `yaml:"recover_threshold"`
	HalfOpenProbes    int      `yaml:"half_open_probes"`
	MaxRetries        int      `yaml:"max_retries"`
	Recovery          string   `yaml:"recovery"`
}
```
删 `FailureThreshold`。

- [ ] **Step 3: OrderedSources 用列表序赋 OriginalIndex**

```go
func (c *Config) OrderedSources() []Source {
	out := make([]Source, len(c.Sources))
	copy(out, c.Sources)
	for i := range out {
		out[i].OriginalIndex = i
	}
	return out // 不排序：列表序即优先级
}
```

- [ ] **Step 4: validate 默认值 + 旧字段告警**

```go
func (c *Config) validate() error {
	if len(c.Sources) == 0 { return fmt.Errorf("config: at least one source required") }
	if c.Session.MaxEntries == 0 { c.Session.MaxEntries = 10000 }
	if c.Session.TTL == 0 { c.Session.TTL = Duration(time.Hour) }
	def := BreakerCfg{
		FirstByteTimeout: Duration(12 * time.Second), Cooldown: Duration(30 * time.Second),
		DegradeThreshold: 3, RecoverThreshold: 1, HalfOpenProbes: 1, MaxRetries: 0, Recovery: "normal",
	}
	c.Breaker = applyDefaults(c.Breaker, def)
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.Name == "" || s.BaseURL == "" { return fmt.Errorf("config: source %d missing name/base_url", i) }
		if s.Breaker != nil { s.Breaker = applyDefaultsPtr(s.Breaker, def) }
	}
	return nil
}
```
`applyDefaults`/`applyDefaultsPtr`：零值字段填默认。

旧字段告警：`Load()` 里若 yaml 含 `priority` 或 `failure_threshold` 键，`log.Printf("[config] ignored deprecated field ...")`。实现：用 `yaml.Node` 二次扫描或 `yaml.Unmarshal` 到 `map[string]any` 检测（仅告警，不阻断）。

- [ ] **Step 5: 改 BreakerFor 合并**

```go
func (c *Config) BreakerFor(s *Source) BreakerCfg {
	if s.Breaker == nil { return c.Breaker }
	merged := c.Breaker
	m := *s.Breaker
	if m.FirstByteTimeout != 0 { merged.FirstByteTimeout = m.FirstByteTimeout }
	if m.Cooldown != 0 { merged.Cooldown = m.Cooldown }
	if m.DegradeThreshold != 0 { merged.DegradeThreshold = m.DegradeThreshold }
	if m.RecoverThreshold != 0 { merged.RecoverThreshold = m.RecoverThreshold }
	if m.HalfOpenProbes != 0 { merged.HalfOpenProbes = m.HalfOpenProbes }
	if m.Recovery != "" { merged.Recovery = m.Recovery }
	// MaxRetries 仅全局，不 per-source 覆盖
	return merged
}
```

- [ ] **Step 6: 写 breaker 状态机失败测试**

`internal/breaker/breaker_test.go`（覆盖各转换）：
```go
func TestNormalToDegradedAfterThreshold(t *testing.T) {
	b := New(cfg(3, 1, "normal")) // degrade=3, recover=1
	for i := 0; i < 2; i++ { b.RecordFailure() }
	if b.State() != normal { t.Fatal("premature degrade") }
	b.RecordFailure() // 第3次
	if b.State() != degraded { t.Fatalf("expected degraded, got %v", b.State()) }
}
func TestDegradedRecoversOnSuccess(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 3; i++ { b.RecordFailure() } // → degraded
	b.RecordSuccess() // recover=1 → normal
	if b.State() != normal { t.Fatal("did not recover") }
}
func TestDegradedToCircuitOpen(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 3; i++ { b.RecordFailure() } // degraded
	for i := 0; i < 3; i++ { b.RecordFailure() } // → circuitOpen
	if b.State() != circuitOpen { t.Fatal("expected circuitOpen") }
	if b.Allow() { t.Fatal("circuitOpen should not allow before cooldown") }
}
func TestCircuitOpenHalfOpenRecovery(t *testing.T) {
	b := New(cfg(3, 1, "normal"))
	for i := 0; i < 6; i++ { b.RecordFailure() } // circuitOpen
	advanceTime(b, 31*time.Second) // cooldown 过
	if !b.Allow() { t.Fatal("should halfOpen after cooldown") }
	st := b.RecordSuccess() // recovery=normal
	if st != normal { t.Fatal("should recover to normal") }
}
func TestHalfOpenFailResets(t *testing.T) { /* circuitOpen→halfOpen→RecordFailure→circuitOpen, openedAt 重置 */ }
func TestCountersResetOnOpposite(t *testing.T) { /* success 清 failStreak, failure 清 successStreak */ }
```
Run: `go test ./internal/breaker/` → FAIL（状态机未实现）。

- [ ] **Step 7: 实现 breaker 状态机**

`internal/breaker/breaker.go`（完整重写）：
```go
package breaker

import (
	"sync"
	"time"

	"openai2response/internal/config"
)

type State int
const (
	normal State = iota
	degraded
	circuitOpen
	halfOpen
)

type Breaker struct {
	mu             sync.Mutex
	cfg            config.BreakerCfg
	st             State
	failStreak     int
	successStreak  int
	degradeCount   int // 0 normal, 1 degraded, 2 circuitOpen
	openedAt       time.Time
	halfOpenInflight int
	now            func() time.Time // 可注入便于测试
}

func New(cfg config.BreakerCfg) *Breaker {
	return &Breaker{cfg: cfg, st: normal, now: time.Now}
}

func (b *Breaker) State() State { return b.st }
func (b *Breaker) DegradeCount() int { return b.degradeCount }

func (b *Breaker) Allow() bool {
	b.mu.Lock(); defer b.mu.Unlock()
	switch b.st {
	case normal, degraded:
		return true
	case circuitOpen:
		if b.now().Sub(b.openedAt) >= time.Duration(b.cfg.Cooldown) {
			b.st = halfOpen; b.halfOpenInflight = 0; return true
		}
		return false
	case halfOpen:
		if b.halfOpenInflight < b.cfg.HalfOpenProbes { b.halfOpenInflight++; return true }
		return false
	}
	return true
}

// RecordFailure 记失败；返回新 State。正常→降级→熔断；halfOpen 探测失败→重置。
func (b *Breaker) RecordFailure() State {
	b.mu.Lock(); defer b.mu.Unlock()
	b.successStreak = 0
	b.failStreak++
	if b.st == halfOpen {
		b.st = circuitOpen; b.openedAt = b.now(); b.halfOpenInflight = 0
		return b.st
	}
	if b.failStreak >= b.cfg.DegradeThreshold {
		b.failStreak = 0
		b.degradeCount++
		if b.degradeCount >= 2 {
			b.st = circuitOpen; b.openedAt = b.now()
		} else {
			b.st = degraded
		}
	}
	return b.st
}

// RecordSuccess 记成功；返回新 State。degraded 升回 normal；halfOpen 探测成功按 recovery。
func (b *Breaker) RecordSuccess() State {
	b.mu.Lock(); defer b.mu.Unlock()
	b.failStreak = 0
	b.successStreak++
	if b.st == halfOpen {
		switch b.cfg.Recovery {
		case "degraded":
			b.st = degraded; b.degradeCount = 1
		default: // "normal"
			b.st = normal; b.degradeCount = 0; b.successStreak = 0
		}
		b.halfOpenInflight = 0
		return b.st
	}
	if b.st == degraded && b.successStreak >= b.cfg.RecoverThreshold {
		b.st = normal; b.degradeCount = 0; b.successStreak = 0
	}
	return b.st
}
```
（测试用的 `advanceTime` 通过 `b.now = func() time.Time { ... }` 注入；测试 helper 设置。）

- [ ] **Step 8: 测试通过 + config 测试 + commit**

更新 config_test.go（新字段、列表序、OriginalIndex、旧字段告警、默认值）。Run: `go test ./internal/breaker/ ./internal/config/` → PASS。`go build ./...`。
```bash
git add internal/config/ internal/breaker/
git commit -m "feat(config,breaker): 两级容灾状态机 + config 列表序"
```

---

### Task 2: scheduler runtimeOrder + 重试循环

**Files:**
- Modify: `internal/scheduler/scheduler.go`、`internal/scheduler/scheduler_test.go`

**Interfaces:**
- Consumes: `config.OrderedSources()`（列表序+OriginalIndex）、`breaker.New/Allow/RecordSuccess/RecordFailure`（返回 State）
- Produces: `Execute` 重试循环 + runtimeOrder 序列管理（签名不变）

- [ ] **Step 1: 写重试 + 序列失败测试**

`internal/scheduler/scheduler_test.go`：
```go
func TestRetryOnAllFail(t *testing.T) {
	// 两源都 500，max_retries=2，backoff 极小（测试用 stub）
	// 断言：trySource 被调用 ≥3 轮（初始+2重试），最后 ErrAllSourcesFailed
}
func TestNoRetryWhenMaxRetriesZero(t *testing.T) {
	// max_retries=0，全失败 → 只 1 轮，立即 ErrAllSourcesFailed
}
func TestRetryCtxCancel(t *testing.T) {
	// max_retries=-1，backoff sleep 中 cancel ctx → Execute 返回 ctx.Err，不永挂
}
func TestDegradeMovesSourceToEnd(t *testing.T) {
	// 源A连续失败3次降级 → runtimeOrder 中 A 移到末尾，B 在前
	// 下一请求先试 B
}
func TestRecoverRestoresOriginalPosition(t *testing.T) {
	// 源A降级后移 → 后续成功升回 normal → 恢复到 originalIndex 位置
}
func TestCircuitOpenSourceSkipped(t *testing.T) {
	// 源A熔断 Allow=false → 跳过，试 B
}
func TestAllCircuitOpenTriggersRetry(t *testing.T) {
	// 全熔断 → 一轮无成功 → 重试（backoff 推进 cooldown 后 halfOpen）
}
```
Run: `go test ./internal/scheduler/` → FAIL。

- [ ] **Step 2: 实现 runtimeOrder + 重试循环**

`internal/scheduler/scheduler.go` 关键改动：
```go
type Scheduler struct {
	cfg          *config.Config
	client       *anthropicclient.Client
	breakers     map[string]*breaker.Breaker
	order        []orderEntry // runtimeOrder
	bkMu         sync.Mutex
	ordMu        sync.RWMutex
}
type orderEntry struct {
	name          string
	originalIndex int
}

func New(cfg *config.Config) *Scheduler {
	srcs := cfg.OrderedSources()
	order := make([]orderEntry, len(srcs))
	for i, s := range srcs { order[i] = orderEntry{s.Name, s.OriginalIndex} }
	return &Scheduler{
		cfg: cfg, client: anthropicclient.New(),
		breakers: map[string]*breaker.Breaker{}, order: order,
	}
}

var backoff = []time.Duration{2,4,6,8,10,10,10,10,10,10} // 秒，clamp 封顶10
func waitBackoff(ctx context.Context, attempt int) error {
	d := backoff[attempt]
	if attempt >= len(backoff) { d = 10 * time.Second } else { d = d * time.Second }
	t := time.NewTimer(d); defer t.Stop()
	select {
	case <-ctx.Done(): return ctx.Err()
	case <-t.C: return nil
	}
}

func (s *Scheduler) Execute(ctx context.Context, req *anthropic.MessageNewParams, onEvent func(*anthropic.MessageStreamEventUnion) error) error {
	mr := s.cfg.Breaker.MaxRetries
	var lastErr error
	for attempt := 0; mr == -1 || attempt <= mr; attempt++ {
		for _, src := range s.cfg.OrderedSources() { // 每次取 config 序
			// 但按 runtimeOrder 调整：用 s.runtimeSeq() 得当前顺序的 sources
		}
		// 实际：s.runtimeSeq() 返回按 order 排序的 []config.Source
		success, err := s.tryRound(ctx, req, onEvent)
		if success { return nil }
		if err != nil { lastErr = err }
		if mr == 0 { break }
		if mr != -1 && attempt == mr { break }
		if werr := waitBackoff(ctx, attempt); werr != nil { return werr }
	}
	if lastErr != nil { return fmt.Errorf("%w (last: %v)", ErrAllSourcesFailed, lastErr) }
	return ErrAllSourcesFailed
}
```
`tryRound`：按 `runtimeSeq()` 遍历，`Allow()` 跳过熔断，`trySource`；RecordFailure 后若 State 变 degraded/circuitOpen → `moveToEnd(name)`；RecordSuccess 后若 State 变 normal（从 degraded/halfOpen 升回）→ `restoreOriginal(name)`。序列操作在 `ordMu` 锁内。

`runtimeSeq()`：按 `order`（含 originalIndex）从 `cfg.Sources` 取，`order` 顺序即运行时优先级。

`moveToEnd(name)`：把 order 中该 name 的 entry 移到末尾。
`restoreOriginal(name)`：把该 entry 移回 originalIndex 位置（重排 order）。

`trySource` 内部不变（首字节锁/watchdog/model resolve/SDK 类型），但其调用的 RecordFailure/RecordSuccess 现在返回 State，trySource 据此返回是否触发序列调整（或 tryRound 统一处理）。

- [ ] **Step 3: 测试通过 + commit**

Run: `go test ./internal/scheduler/` → PASS。`go build ./...`。
```bash
git add internal/scheduler/
git commit -m "feat(scheduler): runtimeOrder 降级后移/升回恢复 + 单请求 backoff 重试"
```

---

### Task 3: 集成测试（全通路 + 容灾场景）

**Files:**
- Create: `internal/server/integration_test.go`

**Interfaces:**
- Consumes: Task 1/2 全部产物

- [ ] **Step 1: 搭集成测试脚手架**

httptest 起 mock Anthropic 后端（可编程返回 SSE 流），构造 `server.New(cfg)` + `httptest.NewServer(srv.Handler())`，POST `/v1/responses`（stream=true），读 SSE 流解析事件。
```go
func startMockBackend(events []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		for _, ev := range events { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ...); w.(http.Flusher).Flush() }
	}))
}
```

- [ ] **Step 2: 全通路用例**

- **纯文本流**：mock 发 message_start/content_block_start(text)/delta×N/stop → 断言 response.created/in_progress/output_text.delta×N/output_text.done/completed 事件序列 + sequence 单调 + completed 含 object/output/completed_at/usage。
- **tool 多轮**：mock 发 tool_use + input_json_delta → 断言 function_call 事件 + call_id；第二轮 POST 带 function_call_output，mock 继续响应 → 断言 enrich（previous_response_id 解析）。
- **reasoning**：plaintext thinking（signature_delta）→ reasoning_text.delta/done + OutputItem.Signature 存储用于 round-trip；summarized 模式 → reasoning_summary_*；redacted_thinking → EncryptedContent。
- **error→failed**：mock 发 error 事件 → response.failed（单个，不双发）。

- [ ] **Step 3: 容灾用例**

- **failover**：源 A mock 500、源 B mock 成功 → 断言只用 B，事件正常。
- **降级**：源 A 连续 3 次 500 → 断言 A 在 runtimeOrder 后移（通过下次请求先试 B 验证）。
- **重试**：两源一轮全 500，max_retries=2 + backoff stub 极小 → 断言重试轮数。
- **熔断恢复**：源 A 熔断后 cooldown 过 → halfOpen 探测成功 → 升回。

- [ ] **Step 4: 收口**

```bash
go test ./... && go build ./... && go vet ./...
```
全绿。
```bash
git add internal/server/integration_test.go
git commit -m "test: 全通路 + 容灾集成测试"
```

---

## Self-Review

**Spec 覆盖**：
- 状态机 normal/degraded/circuitOpen/halfOpen + 双向升降级 → Task 1 ✅
- degrade_threshold=3/recover_threshold=1 不对称 → Task 1 测试 ✅
- 运行时序列降级后移/升回恢复原位 → Task 2 ✅
- config 列表序去 priority + originalIndex → Task 1 ✅
- BreakerCfg 新字段（含 max_retries）→ Task 1 ✅
- 单请求重试 backoff [2,4,6,8,10] + max_retries -1/0/N → Task 2 ✅
- 全熔断重试 → Task 2 测试 + Task 3 集成 ✅
- ctx.Done 中断 → Task 2 测试 ✅
- 旧字段告警 → Task 1 ✅
- server 不改 → 各 Task（Execute 签名不变）✅
- 集成测试（需求 3 全通路）→ Task 3 ✅

**Placeholder 扫描**：Task 2 Step 2 的 `tryRound`/`runtimeSeq`/`moveToEnd`/`restoreOriginal` 给了职责描述 + 关键逻辑，但完整代码较长——实现时按描述 + 接口契约细化（非 TODO 逃避，是明确函数职责）。

**类型一致性**：`RecordSuccess()/RecordFailure()` 返回 `State`（Task 1 定义）→ Task 2 trySource/tryRound 据返回值判序列调整，一致；`orderEntry{name, originalIndex}`（Task 2）与 `Source.OriginalIndex`（Task 1）一致。

**Spec 缺口（已发现）**：spec §5 `BreakerCfg` 漏列 `FirstByteTimeout`——Task 1 保留（trySource watchdog 依赖）。**需补 spec §5 加 `first_byte_timeout`**（计划已含，spec 待同步）。
