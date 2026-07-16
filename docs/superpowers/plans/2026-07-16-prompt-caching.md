# Prompt Caching 修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复网关 prompt caching 的 4 个 gap——messages 历史不缓存、tools 末尾非 function tool 时缓存丢失、cache 命中不可观测、TTL 固定 5m——让多轮对话的 system/tools/messages 历史都命中缓存,并能在日志里看到真实命中率。

**Architecture:** 围绕 `convert.applyAnthropicCacheControl` 展开:① 用 `MessageNewParams` 顶层 `CacheControl`(Anthropic automatic caching,自动覆盖 messages 历史,每轮前移);② 把 tools breakpoint 从写死 `OfTool` 改为按变体派发;④ TTL 从新增的 `config.CacheCfg.TTL` 读取(默认 `5m`,可配 `1h`);③ 在 `model.ResponseUsage` 补 cache 字段,`converter` 从上游 `Usage` 透传,`server` 在响应完成日志里打出。

**Tech Stack:** Go 1.24、`github.com/anthropics/anthropic-sdk-go@v1.57.0`、`github.com/openai/openai-go/v3@v3.42.0`、koanf 配置、slog 日志。

## Global Constraints

- 缓存 breakpoint 上限 4 个:网关使用 system(1)+ tools(1)+ automatic messages(1)= 3 个,不得超出。
- TTL 只能是 `5m`(默认)或 `1h`(`anthropic.CacheControlEphemeralTTLTTL5m` / `CacheControlEphemeralTTLTTL1h`)。
- 代码注释、commit message、日志用简体中文,与现有代码风格一致。
- TDD:每个 task 先写失败测试,再实现,再验证绿,再 commit。每个 task 独立可测。
- 不得改变现有 `applyAnthropicCacheControl` 之外的函数行为(reasoning 裁剪、web search 等保持不变)。

## File Structure

| 文件 | 责任 | 改动类型 |
|---|---|---|
| `internal/convert/request.go` | `applyAnthropicCacheControl` 与 tools breakpoint helper | 修改 |
| `internal/convert/request_test.go` | cache_control 单元测试 | 新增测试 |
| `internal/config/config.go` | `CacheCfg.TTL` 字段 + 默认值 | 修改 |
| `internal/model/responseobject.go` | `ResponseUsage` 补 cache 字段 | 修改 |
| `internal/streamconv/converter.go` | `recordStopReason` 透传 cache usage + `Usage()` getter | 修改 |
| `internal/streamconv/converter_test.go` | cache usage 透传测试 | 新增测试 |
| `internal/server/server.go` | 响应完成日志补 cache 命中字段 | 修改 |

---

### Task 1: tools breakpoint 支持所有 tool 变体(②)

**问题:** `applyAnthropicCacheControl` 写死 `out.Tools[last].OfTool != nil` 才加 cache_control。当最后一个 tool 是 web_search(`OfWebSearchTool20250305`)时,整个 tools 列表缓存丢失。

**Files:**
- Modify: `internal/convert/request.go:1084-1093`(`applyAnthropicCacheControl`)、新增 `setLastToolCacheControl` helper
- Test: `internal/convert/request_test.go`

**Interfaces:**
- Produces: `func setLastToolCacheControl(tools []anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam)`(包私有 helper,Task 2/3 复用)

- [ ] **Step 1: 写失败测试**

追加到 `internal/convert/request_test.go`(放在 `findWebSearchTool` 之后):

```go
// TestCacheControlAppliedToNonFunctionTool 复现 gap②:最后一个 tool 是
// web_search(OfWebSearchTool20250305)而非 function(OfTool)时,cache_control
// 仍应加到该 tool 上,否则整个 tools 列表缓存丢失。
func TestCacheControlAppliedToNonFunctionTool(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"web_search"}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Tools) == 0 || out.Tools[0].OfWebSearchTool20250305 == nil {
		t.Fatalf("expected web_search tool to be mapped: %+v", out.Tools)
	}
	cc := out.Tools[0].OfWebSearchTool20250305.CacheControl
	if cc.TTL != anthropic.CacheControlEphemeralTTLTTL5m {
		t.Fatalf("cache_control not applied to non-function tool: %+v", cc)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/convert/ -run TestCacheControlAppliedToNonFunctionTool -v`
Expected: FAIL(当前 `out.Tools[0].OfWebSearchTool20250305.CacheControl.TTL == ""`,因为 `applyAnthropicCacheControl` 只认 `OfTool`)。

- [ ] **Step 3: 实现 setLastToolCacheControl helper**

在 `internal/convert/request.go` 的 `applyAnthropicCacheControl` 函数后追加:

```go
// setLastToolCacheControl 给 tools 列表的最后一个 tool 加 cache_control,
// 按 union 变体派发(OfTool / OfWebSearchTool20250305)。hosted server tool
// 变体覆盖齐全后可继续在此 switch 扩展。
func setLastToolCacheControl(tools []anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam) {
	if len(tools) == 0 {
		return
	}
	last := &tools[len(tools)-1]
	switch {
	case last.OfTool != nil:
		last.OfTool.CacheControl = cc
	case last.OfWebSearchTool20250305 != nil:
		last.OfWebSearchTool20250305.CacheControl = cc
	}
}
```

- [ ] **Step 4: 改 applyAnthropicCacheControl 用 helper**

把 `internal/convert/request.go:1090-1092` 的:

```go
	if len(out.Tools) > 0 && out.Tools[len(out.Tools)-1].OfTool != nil {
		out.Tools[len(out.Tools)-1].OfTool.CacheControl = cacheControl
	}
```

替换为:

```go
	setLastToolCacheControl(out.Tools, cacheControl)
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/convert/ -run TestCacheControlAppliedToNonFunctionTool -v`
Expected: PASS。再跑全包:`go test ./internal/convert/` 全绿。

- [ ] **Step 6: commit**

```bash
git add internal/convert/request.go internal/convert/request_test.go
git commit -m "fix(convert): tools cache_control 支持非 function tool 变体"
```

---

### Task 2: 顶层 automatic cache_control 覆盖 messages 历史(①)

**问题:** 网关只在 system/tools 加显式 breakpoint,messages 对话历史每轮全量重算。Anthropic 顶层 `MessageNewParams.CacheControl` 会自动在最后缓存块加 marker,每轮前移,从而缓存整个 messages 历史。

**Files:**
- Modify: `internal/convert/request.go`(`applyAnthropicCacheControl` 加 `out.CacheControl = cc`)
- Test: `internal/convert/request_test.go`

**Interfaces:**
- Consumes: Task 1 的 `setLastToolCacheControl`

- [ ] **Step 1: 写失败测试**

追加到 `internal/convert/request_test.go`:

```go
// TestTopLevelCacheControlForMessageHistory 复现 gap①:顶层 cache_control
// 必须设置,Anthropic 才会自动缓存 messages 历史(system/tools 已有显式
// breakpoint,顶层 marker 覆盖到 messages 末尾)。
func TestTopLevelCacheControlForMessageHistory(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.CacheControl.TTL == "" {
		t.Fatalf("top-level cache_control not set; message history won't be cached")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/convert/ -run TestTopLevelCacheControlForMessageHistory -v`
Expected: FAIL(`out.CacheControl.TTL == ""`,当前未设顶层)。

- [ ] **Step 3: 在 applyAnthropicCacheControl 设顶层 CacheControl**

在 `internal/convert/request.go` 的 `applyAnthropicCacheControl` 里,`cacheControl` 构造之后、system 分支之前,插入:

```go
	out.CacheControl = cacheControl
```

(Task 3 会把 TTL 改成从 cfg 读,这里先用现有 5m 的 `cacheControl`。)

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/convert/ -run TestTopLevelCacheControlForMessageHistory -v`
Expected: PASS。全包 `go test ./internal/convert/` 全绿。

- [ ] **Step 5: commit**

```bash
git add internal/convert/request.go internal/convert/request_test.go
git commit -m "feat(convert): 顶层 cache_control 覆盖 messages 历史"
```

---

### Task 3: cache TTL 可配(④)

**问题:** TTL 写死 5m。Codex 长任务两轮间隔可能 > 5m,缓存过期重建(1.25x)。需要从配置读 TTL(`5m` 默认 / `1h`)。

**Files:**
- Modify: `internal/config/config.go`(新增 `CacheCfg` + 默认值)、`internal/convert/request.go`(`applyAnthropicCacheControl` 改签名接 cfg)
- Test: `internal/convert/request_test.go`、`internal/config/config_test.go`(若已存在,否则新增)

**Interfaces:**
- Produces: `applyAnthropicCacheControl(out *anthropic.MessageNewParams, cfg *config.Config)`(签名变更,`ToAnthropic` 调用处同步改)

- [ ] **Step 1: 写失败测试**

追加到 `internal/convert/request_test.go`:

```go
// TestCacheControlTTLFromConfig 复现 gap④:TTL 必须从 config.Cache.TTL 读,
// "1h" 时顶层 cache_control 用 1h,默认 5m。
func TestCacheControlTTLFromConfig(t *testing.T) {
	t.Run("default 5m", func(t *testing.T) {
		req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true}`)
		out, err := ToAnthropic(req, &config.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if out.CacheControl.TTL != anthropic.CacheControlEphemeralTTLTTL5m {
			t.Fatalf("default TTL want 5m, got %v", out.CacheControl.TTL)
		}
	})
	t.Run("1h from config", func(t *testing.T) {
		req := mustReq(t, `{"model":"gpt-5","input":"hi","stream":true}`)
		out, err := ToAnthropic(req, &config.Config{Cache: config.CacheCfg{TTL: "1h"}})
		if err != nil {
			t.Fatal(err)
		}
		if out.CacheControl.TTL != anthropic.CacheControlEphemeralTTLTTL1h {
			t.Fatalf("configured TTL want 1h, got %v", out.CacheControl.TTL)
		}
	})
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/convert/ -run TestCacheControlTTLFromConfig -v`
Expected: FAIL(`config.Config{Cache: ...}` 编译失败——`CacheCfg` 还不存在)。

- [ ] **Step 3: 在 config 加 CacheCfg**

在 `internal/config/config.go` 的 `type Config struct { ... }` 里追加字段:

```go
	Cache    CacheCfg    `koanf:"cache" yaml:"cache"`
```

并在 `SessionCfg` 定义附近追加:

```go
// CacheCfg 配置 Anthropic prompt cache 的 TTL。
type CacheCfg struct {
	TTL string `koanf:"ttl" yaml:"ttl"` // "5m"(默认)或 "1h"
}
```

- [ ] **Step 4: 给 Cache.TTL 加默认值**

在 `internal/config/config.go` 设置默认值的位置(与 `Session.TTL` 默认值同处,约 line 284 附近的默认值函数内)追加:

```go
	if cfg.Cache.TTL == "" {
		cfg.Cache.TTL = "5m"
	}
```

(若该默认值函数命名不同,定位到 `if c.Session.TTL == 0 { ... }` 同一函数体内追加即可。)

- [ ] **Step 5: 改 applyAnthropicCacheControl 签名 + TTL 逻辑**

把 `internal/convert/request.go` 的 `applyAnthropicCacheControl` 改为:

```go
func applyAnthropicCacheControl(out *anthropic.MessageNewParams, cfg *config.Config) {
	ttl := anthropic.CacheControlEphemeralTTLTTL5m
	if cfg != nil && cfg.Cache.TTL == "1h" {
		ttl = anthropic.CacheControlEphemeralTTLTTL1h
	}
	cacheControl := anthropic.NewCacheControlEphemeralParam()
	cacheControl.TTL = ttl
	out.CacheControl = cacheControl
	if len(out.System) > 0 {
		out.System[len(out.System)-1].CacheControl = cacheControl
	}
	setLastToolCacheControl(out.Tools, cacheControl)
}
```

并把 `ToAnthropic` 里的调用(`internal/convert/request.go` 约 line 185)从 `applyAnthropicCacheControl(out)` 改为 `applyAnthropicCacheControl(out, cfg)`(`ToAnthropic` 已有 `cfg *config.Config` 参数)。

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./internal/convert/ -run TestCacheControlTTLFromConfig -v`
Expected: PASS。全量 `go build ./... && go test ./...` 全绿(注意:任何 `applyAnthropicCacheControl` 的旧签名调用都已改)。

- [ ] **Step 7: commit**

```bash
git add internal/config/config.go internal/convert/request.go internal/convert/request_test.go
git commit -m "feat(config): cache TTL 可配（5m 默认 / 1h）"
```

---

### Task 4: cache 命中可观测(③)

**问题:** `recordStopReason` 只记 `InputTokens/OutputTokens`,上游返回的 `CacheReadInputTokens`/`CacheCreationInputTokens` 丢弃,日志看不到缓存命中率,无法验证 Task 1-3 的效果。

**Files:**
- Modify: `internal/model/responseobject.go`(`ResponseUsage` 补字段)
- Modify: `internal/streamconv/converter.go`(`recordStopReason` 透传 + `Usage()` getter)
- Modify: `internal/server/server.go`(响应完成日志补 cache 字段)
- Test: `internal/streamconv/converter_test.go`

**Interfaces:**
- Produces: `func (c *Converter) Usage() *model.ResponseUsage`(供 server 读 cache 命中)

- [ ] **Step 1: 写失败测试**

追加到 `internal/streamconv/converter_test.go`:

```go
// TestUsageRecordsCacheTokens 复现 gap③:上游 message_delta 的 usage 含
// cache_read_input_tokens / cache_creation_input_tokens,converter 必须透传,
// 否则日志无法观测缓存命中。
func TestUsageRecordsCacheTokens(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:    "message_start",
		Message: anthropic.Message{ID: "m", Model: "x"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: "end_turn"},
		Usage: anthropic.Usage{
			InputTokens:              50,
			OutputTokens:             10,
			CacheReadInputTokens:     1000,
			CacheCreationInputTokens: 200,
		},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})
	u := c.Usage()
	if u == nil {
		t.Fatal("expected usage after message_delta")
	}
	if u.CacheReadInputTokens != 1000 || u.CacheCreationInputTokens != 200 {
		t.Fatalf("cache tokens not propagated: %+v", u)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/streamconv/ -run TestUsageRecordsCacheTokens -v`
Expected: FAIL(`Usage()` 方法不存在,且 `CacheReadInputTokens` 字段不存在)。

- [ ] **Step 3: 给 ResponseUsage 补 cache 字段**

把 `internal/model/responseobject.go` 的 `ResponseUsage` 改为:

```go
type ResponseUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	TotalTokens              int `json:"total_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}
```

- [ ] **Step 4: recordStopReason 透传 cache tokens**

在 `internal/streamconv/converter.go` 的 `recordStopReason` 里,把 usage 构造改为:

```go
	if ev.Usage.OutputTokens > 0 || ev.Usage.InputTokens > 0 ||
		ev.Usage.CacheReadInputTokens > 0 || ev.Usage.CacheCreationInputTokens > 0 {
		c.usage = &model.ResponseUsage{
			InputTokens:              int(ev.Usage.InputTokens),
			OutputTokens:             int(ev.Usage.OutputTokens),
			CacheReadInputTokens:     int(ev.Usage.CacheReadInputTokens),
			CacheCreationInputTokens: int(ev.Usage.CacheCreationInputTokens),
		}
		c.usage.TotalTokens = c.usage.InputTokens + c.usage.OutputTokens
	}
```

- [ ] **Step 5: 加 Usage() getter**

在 `internal/streamconv/converter.go` 的 `StopReason()` getter 附近追加:

```go
// Usage returns the upstream token usage (including cache hit/creation) for
// diagnostics; nil before the message_delta carrying usage arrives.
func (c *Converter) Usage() *model.ResponseUsage { return c.usage }
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./internal/streamconv/ -run TestUsageRecordsCacheTokens -v`
Expected: PASS。`go build ./...` 全绿。

- [ ] **Step 7: 在 server 响应完成日志补 cache 字段**

在 `internal/server/server.go` 的"响应请求完成"日志(`slog.Info("响应请求完成", ...)`)前加:

```go
	var cacheRead, cacheCreate int
	if u := conv.Usage(); u != nil {
		cacheRead = u.CacheReadInputTokens
		cacheCreate = u.CacheCreationInputTokens
	}
```

并在该 `slog.Info` 的字段列表里追加:

```go
		"cache_read_tokens", cacheRead,
		"cache_creation_tokens", cacheCreate,
```

- [ ] **Step 8: 运行全量验证**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 全绿。

- [ ] **Step 9: commit**

```bash
git add internal/model/responseobject.go internal/streamconv/converter.go internal/streamconv/converter_test.go internal/server/server.go
git commit -m "feat(log): 透传并打印 cache 命中 token（可观测）"
```

---

## 收尾

- [ ] **更新文档**:把 `docs/protocol-coverage.md` 里 `prompt_cache_key` / `prompt_cache_options` / `prompt_cache_breakpoint` 三行的状态从 `deferred` 改为说明"网关已自主用 cache_control 覆盖 system/tools/messages,TTL 可配;OpenAI 的 key 字段对 Anthropic(内容 hash 缓存)无意义,忽略"。
- [ ] **合并 + push**:4 个 task 全绿后,合并到 main 并 push。
- [ ] **实地验证**:重新部署,跑 Codex 多轮,看"响应请求完成"日志的 `cache_read_tokens`/`cache_creation_tokens`——首轮 `cache_creation > 0`,后续轮 `cache_read > 0` 即缓存生效。

## Self-Review

1. **Spec coverage**:
   - ① messages 历史 → Task 2 ✓
   - ② tools 非 OfTool → Task 1 ✓
   - ③ cache 命中不可观测 → Task 4 ✓
   - ④ TTL 固定 5m → Task 3 ✓
   - 文档收尾、实地验证 → 收尾节 ✓
2. **Placeholder scan**:每个 step 都有完整可编译代码,无 TBD/TODO。
3. **Type consistency**:`setLastToolCacheControl`(Task1)、`applyAnthropicCacheControl(out, cfg)`(Task3)、`Usage()`(Task4)在跨 task 引用时签名一致;`CacheCfg{TTL}`(Task3)、`ResponseUsage` cache 字段(Task4)前后一致。
