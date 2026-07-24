# Usage 指标归一化实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 统一三类后端的输入 Token 统计口径，修复 Chat 缓存字段优先级，并暴露非阻塞指标采集的丢弃量。

**Architecture:** Backend 继续上报各协议的原始 usage，`metrics.Collector` 在消费边界按 `BackendType` 归一化完整输入，确保总量、分组、历史和命中率使用同一口径。Collector 用原子计数记录 channel 满时的丢弃事件，管理 API 和页面直接消费扩展后的 snapshot。

**Tech Stack:** Go、标准库 `sync/atomic` 与 `testing`、Alpine.js 管理页。

## Global Constraints

- 不增加持久化存储或第三方依赖。
- 不增加请求维度的缓存命中率。
- 不把 DeepSeek 的 cache miss 解释为缓存创建。
- 不改变 Backend 对客户端 Responses usage 的协议输出。
- 不改变 scheduler 的重试和选源行为。
- 指标投递必须继续使用 `select + default`，不得阻塞 `/v1/*` 转发路径。
- 手工文件编辑只使用 `apply_patch`。

---

### Task 1：归一化输入 Token

**Files:**
- Modify: `internal/metrics/metrics.go:45-155,196-322`
- Test: `internal/metrics/metrics_test.go`

**Interfaces:**
- Consumes: `RequestEvent.BackendType`、`InputTokens`、`CacheRead`、`CacheCreate`
- Produces: `normalizedInputTokens(RequestEvent) int`；归一化后的 `Snapshot.TotalInput`、`GroupStat.InputTokens` 和 `RequestRecord.InputTokens`

- [ ] **Step 1：编写失败测试**

在 `internal/metrics/metrics_test.go` 增加表驱动测试，验证 Anthropic 增加缓存
读写量，而 Chat/Responses 不重复增加：

```go
func TestCollectorNormalizesInputTokens(t *testing.T) {
	tests := []struct {
		name        string
		backendType string
		want        int64
	}{
		{name: "anthropic", backendType: "a", want: 1250},
		{name: "chat", backendType: "c", want: 1000},
		{name: "responses", backendType: "r", want: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New()
			defer c.Stop()
			c.Record(RequestEvent{
				Kind: KindUpstream, BackendType: tt.backendType,
				SourceName: "source", Model: "model",
				InputTokens: 1000, CacheRead: 200, CacheCreate: 50,
			})
			waitForRequests(t, c, 1)
			s := c.Snapshot()
			if s.TotalInput != tt.want {
				t.Fatalf("TotalInput=%d want %d", s.TotalInput, tt.want)
			}
			if s.ByGroup[0].InputTokens != tt.want {
				t.Fatalf("group input=%d want %d", s.ByGroup[0].InputTokens, tt.want)
			}
			if s.Recent[0].InputTokens != int(tt.want) {
				t.Fatalf("recent input=%d want %d", s.Recent[0].InputTokens, tt.want)
			}
		})
	}
}
```

补充 client 历史记录测试，确认 Anthropic client 记录也使用完整输入，但不进入总量。

- [ ] **Step 2：运行测试并确认失败**

```bash
go test ./internal/metrics -run 'TestCollectorNormalizesInputTokens|TestCollectorNormalizesClientHistoryInput' -count=1
```

预期：Anthropic 的 `TotalInput`、分组或历史断言仍得到原始 `1000`。

- [ ] **Step 3：实现最小归一化**

在 `internal/metrics/metrics.go` 增加：

```go
func normalizedInputTokens(ev RequestEvent, backendType string) int {
	input := ev.InputTokens
	if backendType == "a" {
		input += ev.CacheRead + ev.CacheCreate
	}
	return input
}
```

`apply` 解析缺省 BackendType 后只计算一次 `inputTokens`，并用于：

```go
c.total.input += int64(inputTokens)
g.input += int64(inputTokens)
rec.InputTokens = inputTokens
```

删除独立的 `total.cacheInput` 累计字段。`Snapshot` 使用
`c.total.input` 作为命中率分母。

- [ ] **Step 4：运行指标包测试**

```bash
go test ./internal/metrics -count=1
```

预期：全部通过。

- [ ] **Step 5：提交本任务**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "fix(metrics): 统一输入 token 统计口径"
```

---

### Task 2：修复 Chat 缓存字段优先级

**Files:**
- Modify: `internal/chatstreamconv/converter.go:288-307`
- Test: `internal/chatstreamconv/converter_test.go`

**Interfaces:**
- Consumes: Chat usage 的 `prompt_cache_hit_tokens` 和 `prompt_tokens_details`
- Produces: `ResponseUsage.CacheReadInputTokens`、`CacheCreationInputTokens`

- [ ] **Step 1：编写混合 shape 失败测试**

```go
func TestUsageDeepSeekCacheHitSurvivesEmptyDetails(t *testing.T) {
	c := New()
	raw := `{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_cache_hit_tokens":80,"prompt_tokens_details":{}}}`
	if _, err := c.Feed([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if got := c.Usage().CacheReadInputTokens; got != 80 {
		t.Fatalf("CacheReadInputTokens=%d want 80", got)
	}
}

func TestUsageDetailsCachedTokensOverrideDeepSeekCacheHit(t *testing.T) {
	c := New()
	raw := `{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_cache_hit_tokens":80,"prompt_tokens_details":{"cached_tokens":60}}}`
	if _, err := c.Feed([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if got := c.Usage().CacheReadInputTokens; got != 60 {
		t.Fatalf("CacheReadInputTokens=%d want 60", got)
	}
}
```

- [ ] **Step 2：运行测试并确认失败**

```bash
go test ./internal/chatstreamconv -run 'TestUsageDeepSeekCacheHitSurvivesEmptyDetails|TestUsageDetailsCachedTokensOverrideDeepSeekCacheHit' -count=1
```

预期：第一个用例得到 `0`；第二个用例通过。

- [ ] **Step 3：实现非零 details 优先**

```go
cacheRead := chunk.Usage.PromptCacheHitTokens
cacheCreate := 0
if d := chunk.Usage.PromptTokensDetails; d != nil {
	if d.CachedTokens > 0 {
		cacheRead = d.CachedTokens
	}
	cacheCreate = d.CacheWriteTokens
}
```

- [ ] **Step 4：运行转换与 Backend 回归测试**

```bash
go test ./internal/chatstreamconv ./internal/backend -count=1
```

预期：全部通过。

- [ ] **Step 5：提交本任务**

```bash
git add internal/chatstreamconv/converter.go internal/chatstreamconv/converter_test.go
git commit -m "fix(chatstreamconv): 保留兼容缓存命中字段"
```

---

### Task 3：暴露指标丢弃量

**Files:**
- Modify: `internal/metrics/metrics.go:59-66,117-175,280-297`
- Test: `internal/metrics/metrics_test.go`

**Interfaces:**
- Produces: `Snapshot.DroppedEvents uint64`，JSON 字段 `dropped_events`

- [ ] **Step 1：编写失败测试**

使用未启动 consumer 的 Collector 确定性灌满 channel：

```go
func TestRecordCountsDroppedEvents(t *testing.T) {
	c := &Collector{
		events: make(chan RequestEvent, 1),
		groups: map[groupKey]*groupAgg{},
	}
	c.Record(RequestEvent{})
	c.Record(RequestEvent{})
	if got := c.Snapshot().DroppedEvents; got != 1 {
		t.Fatalf("DroppedEvents=%d want 1", got)
	}
}
```

- [ ] **Step 2：运行测试并确认失败**

```bash
go test ./internal/metrics -run TestRecordCountsDroppedEvents -count=1
```

预期：编译失败，`Snapshot` 尚无 `DroppedEvents`。

- [ ] **Step 3：实现原子丢弃计数**

在 `Snapshot` 增加：

```go
DroppedEvents uint64 `json:"dropped_events"`
```

在 `Collector` 增加：

```go
droppedEvents atomic.Uint64
```

在 `Record` 的 default 分支执行：

```go
c.droppedEvents.Add(1)
```

`Snapshot` 使用 `c.droppedEvents.Load()` 填充字段。

- [ ] **Step 4：运行指标包及 race 测试**

```bash
go test ./internal/metrics -count=1
go test -race ./internal/metrics -count=1
```

预期：全部通过。

- [ ] **Step 5：提交本任务**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): 暴露指标事件丢弃量"
```

---

### Task 4：更新管理页与文档

**Files:**
- Modify: `internal/admin/assets/index.html`
- Modify: `README.md:183`
- Modify: `docs/superpowers/plans/2026-07-24-cache-hit-rate.md`
- Test: `internal/admin/admin_test.go`

**Interfaces:**
- Consumes: snapshot 的 `total_requests`、`total_input`、`dropped_events`
- Produces: “上游调用量”“总输入”“指标丢弃量”管理页卡片

- [ ] **Step 1：编写管理 API 失败测试**

在 `internal/admin/admin_test.go` 的 metrics snapshot 测试中解码 JSON，并断言：

```go
if _, ok := body["dropped_events"]; !ok {
	t.Fatal("metrics snapshot missing dropped_events")
}
```

- [ ] **Step 2：运行测试并确认失败**

```bash
go test ./internal/admin -run TestMetrics -count=1
```

预期：在 Task 3 实现前缺少字段；若 Task 3 已完成，该 API 契约测试直接通过，
继续完成 UI 文案测试或静态检查。

- [ ] **Step 3：更新管理页**

中英文文案增加：

```text
cardReq: 上游调用量 / Upstream calls
cardDrop: 指标丢弃量 / Dropped metrics
```

`metricCards` 增加：

```javascript
{ key:'drop', label: this.t('cardDrop'), display: this.fmtTokens(s.dropped_events || 0), raw: this.fmtRaw(s.dropped_events || 0), tone: (s.dropped_events > 0 ? 'tone-warn' : ''), color: 'var(--warn)' }
```

- [ ] **Step 4：更新文档**

README 明确：

```text
观测台按上游尝试聚合；总输入已按协议归一化，缓存 Token 命中率 =
缓存读取 Token / 归一化总输入 Token。指标为进程生命周期内内存累计值，
非阻塞 channel 满时会累计展示指标丢弃量。
```

同步修订旧计划中“不改变 TotalInput”约束，并登记实际实现和验证结果。

- [ ] **Step 5：运行管理页相关测试**

```bash
go test ./internal/admin ./internal/server -count=1
```

预期：全部通过。

- [ ] **Step 6：提交本任务**

```bash
git add internal/admin/assets/index.html internal/admin/admin_test.go README.md docs/superpowers/plans/2026-07-24-cache-hit-rate.md
git commit -m "docs(admin): 明确 usage 指标统计口径"
```

---

### Task 5：完整验证

**Files:**
- Verify only

**Interfaces:**
- Consumes: Tasks 1-4 的完整改动
- Produces: 可审计的测试、race、lint 与构建证据

- [ ] **Step 1：运行定向测试**

```bash
go test ./internal/metrics ./internal/chatstreamconv ./internal/backend ./internal/admin ./internal/server -count=1
```

预期：全部通过。

- [ ] **Step 2：运行 race 门禁**

```bash
task test-race
```

预期：全部通过。

- [ ] **Step 3：运行仓库门禁**

```bash
task check
```

预期：通过；若被既有无关格式问题阻塞，记录具体文件，不修改无关工作树内容。

- [ ] **Step 4：检查改动边界**

```bash
git diff --check
git status --short
git log --oneline -5
```

预期：无空白错误；仅包含本功能提交以及用户原有未提交文件。
