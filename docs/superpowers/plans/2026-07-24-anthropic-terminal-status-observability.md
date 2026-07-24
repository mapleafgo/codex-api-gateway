# Anthropic 终态观测一致性修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Anthropic Backend 的最终日志、上游指标和客户端指标准确记录 Responses 业务终态。

**Architecture:** `streamconv.Converter` 继续负责 Anthropic stop reason 到 Responses status 的唯一映射，并新增只读 `Status()` 方法。`AnthropicBackend` 在传输成功时读取该状态；传输失败仍按现有错误优先级记录 `failed`。

**Tech Stack:** Go、`log/slog`、Go 标准测试、`httptest`

## Global Constraints

- 不改变 SSE 协议转换结果、重试和熔断行为。
- 不调整默认 `max_tokens=4096`、reasoning effort、上下文或工具列表。
- 不复制 stop reason 映射，不从已发出的 SSE 数据反向解析状态。
- 所有手工文件修改只使用 `apply_patch`。

---

### Task 1: 暴露并使用转换器终态

**Files:**
- Modify: `internal/streamconv/converter.go`
- Modify: `internal/streamconv/converter_test.go`
- Modify: `internal/backend/anthropic.go`
- Modify: `internal/backend/anthropic_test.go`

**Interfaces:**
- Produces: `func (c *Converter) Status() string`
- Consumes: 现有 `statusFor(c.stopReason)` 映射

- [x] **Step 1: 编写转换器和 Backend 失败测试**

在 `internal/streamconv/converter_test.go` 增加表驱动测试，向 Converter 依次 Feed
`message_start`、带 stop reason 的 `message_delta` 和 `message_stop`，断言：

```go
tests := []struct {
	name       string
	stopReason anthropic.StopReason
	want       string
}{
	{name: "max tokens", stopReason: anthropic.StopReasonMaxTokens, want: model.ResponseStatusIncomplete},
	{name: "pause turn", stopReason: anthropic.StopReasonPauseTurn, want: model.ResponseStatusIncomplete},
	{name: "refusal", stopReason: anthropic.StopReasonRefusal, want: model.ResponseStatusIncomplete},
	{name: "end turn", stopReason: anthropic.StopReasonEndTurn, want: model.ResponseStatusCompleted},
	{name: "tool use", stopReason: anthropic.StopReasonToolUse, want: model.ResponseStatusCompleted},
}
```

在 `internal/backend/anthropic_test.go` 增加 `max_tokens` SSE 回归测试，断言
`UpstreamEvent.Status == "incomplete"`，并从 JSON 日志断言
`"msg":"Anthropic 上游流结束"` 与 `"status":"incomplete"` 同时存在。

- [x] **Step 2: 运行测试并确认 RED**

Run:

```bash
go test ./internal/streamconv ./internal/backend -run 'TestConverterStatus|TestAnthropicBackend_MaxTokensReportsIncomplete' -count=1
```

Expected: FAIL，原因是 `Converter.Status` 尚不存在，且 Backend 仍返回 `completed`。

- [x] **Step 3: 实现最小修复**

在 `internal/streamconv/converter.go` 增加：

```go
// Status returns the Responses terminal status derived from the upstream stop
// reason. Before a stop reason arrives, it returns completed for compatibility.
func (c *Converter) Status() string {
	status, _ := statusFor(c.stopReason)
	return status
}
```

在 `internal/backend/anthropic.go` 初始化业务状态时使用：

```go
status := conv.Status()
```

保留 `!locked` 和非客户端取消读取错误覆盖为 `failed` 的现有分支；客户端取消且业务终态
已经产生时继续保留 `conv.Status()`，不得强制改回 `completed`。

- [x] **Step 4: 运行测试并确认 GREEN**

Run:

```bash
gofmt -w internal/streamconv/converter.go internal/streamconv/converter_test.go internal/backend/anthropic.go internal/backend/anthropic_test.go
go test ./internal/streamconv ./internal/backend -run 'TestConverterStatus|TestAnthropicBackend_MaxTokensReportsIncomplete' -count=1
```

Expected: PASS。

### Task 2: 锁定 server 客户端指标状态

**Files:**
- Modify: `internal/server/server_test.go`

**Interfaces:**
- Consumes: `scheduler.UpstreamEvent.Status == "incomplete"`
- Verifies: client `metrics.RequestRecord.Status == "incomplete"`

- [x] **Step 1: 编写 server 集成测试**

增加 Anthropic `httptest` 上游，返回 `message_start`、`message_delta(max_tokens)`、
`message_stop`。调用 `/v1/responses` 后轮询 `srv.Metrics().Snapshot().Recent`，断言同一请求的：

```go
if record.Kind == "client" && record.Status != "incomplete" {
	t.Fatalf("client record=%+v want incomplete", record)
}
```

并断言响应 SSE 包含 `event: response.incomplete`。

- [x] **Step 2: 运行集成测试**

Run:

```bash
go test ./internal/server -run TestAnthropicIncompleteTerminalRecordsIncompleteClientStatus -count=1
```

Expected: PASS；Task 1 已修复 Backend 状态传播，测试用于锁定跨层行为。

- [x] **Step 3: 执行完整验证**

Run:

```bash
go test ./internal/streamconv ./internal/backend ./internal/server -shuffle=on -count=5
task test-race
task check
task build
git diff --check
```

Expected: 全部通过，无 race、格式、vet、测试或构建错误。

- [x] **Step 4: 复审并提交**

检查终态优先级、日志字段、注释和未相关工作树修改。仅暂存本计划涉及的文件：

```bash
git add internal/streamconv/converter.go internal/streamconv/converter_test.go \
  internal/backend/anthropic.go internal/backend/anthropic_test.go \
  internal/server/server_test.go \
  docs/superpowers/plans/2026-07-24-anthropic-terminal-status-observability.md
git commit -m "fix(observability): 修正 Anthropic incomplete 状态"
git push
```

Expected: 推送成功，`base_instructions.md` 与 `base_instructions_cn.md` 仍保持未提交。
