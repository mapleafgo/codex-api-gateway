# cache_control 遗漏补齐 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补齐 Anthropic cache_control 三处遗漏——MCP tools 断点重定位、ApplyCacheControl 全变体派发、1h TTL 的 extended-cache-ttl beta——使 tools 前缀缓存与官方惯例一致。

**Architecture:** ① `toolcatalog.ApplyCacheControl` 改用 SDK `GetCacheControl()` 统一写字段；② `injectMCP` 追加 mcp_toolset 后清除旧 tools 断点并写到最终末项；③ `client.Stream` 在 `CacheControl.TTL==1h` 时合并 `extended-cache-ttl-2025-04-11` beta。

**Tech Stack:** Go、`github.com/anthropics/anthropic-sdk-go@v1.57.0`、`log/slog`、标准 `testing` + `httptest`。

**Spec:** `docs/superpowers/specs/2026-07-21-cache-control-gaps-design.md`

## Global Constraints

- 断点上限 4：补丁后仍 system+tools+top-level=3。
- TTL 仅 `5m` / `1h`。
- 注释、commit message、日志用中文；标识符英文。
- TDD：每个 task 先失败测试再实现再 commit。
- 不改 convert 的 `applyAnthropicCacheControl` 本体（MCP 在 client 层闭环）。
- 不做 usage 5m/1h 拆分。

## File Structure

| 文件 | 责任 | 改动 |
|---|---|---|
| `internal/toolcatalog/server.go` | ApplyCacheControl | 改 |
| `internal/toolcatalog/server_test.go` | bash 变体覆盖 | 改 |
| `internal/anthropic/mcp.go` | injectMCP 断点重定位 | 改 |
| `internal/anthropic/mcp_test.go` | MCP 断点用例 | 改 |
| `internal/anthropic/client.go` | 1h beta | 改 |
| `internal/anthropic/client_test.go` | Stream beta 用例 | 改 |
| `docs/protocol-coverage.md` | 短说明 | 改 |

---

### Task 1: ApplyCacheControl 走 GetCacheControl

**Files:**
- Modify: `internal/toolcatalog/server.go`
- Test: `internal/toolcatalog/server_test.go`

**Interfaces:**
- Produces: `ApplyCacheControl(*anthropic.ToolUnionParam, anthropic.CacheControlEphemeralParam) bool`（签名不变）

- [ ] **Step 1: 写失败测试（bash 变体）**

在 `TestApplyCacheControlRecognizedVariants` 末尾追加：

```go
	bash := anthropic.ToolUnionParam{OfBashTool20250124: &anthropic.ToolBash20250124Param{}}
	if !ApplyCacheControl(&bash, cc) || bash.OfBashTool20250124.CacheControl.TTL != cc.TTL {
		t.Fatalf("OfBashTool20250124 cache_control not applied via GetCacheControl")
	}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/toolcatalog/ -run TestApplyCacheControlRecognizedVariants -v`
Expected: FAIL（switch 无 bash 分支）

- [ ] **Step 3: 实现**

```go
func ApplyCacheControl(tool *anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam) bool {
	if tool == nil {
		return false
	}
	p := tool.GetCacheControl()
	if p == nil {
		return false
	}
	*p = cc
	return true
}
```

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/toolcatalog/ -run 'ApplyCacheControl|ServerTool|IsServerTool' -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/toolcatalog/server.go internal/toolcatalog/server_test.go
git commit -m "refactor(toolcatalog): ApplyCacheControl 统一走 GetCacheControl"
```

---

### Task 2: injectMCP 重定位 tools 末项 cache_control

**Files:**
- Modify: `internal/anthropic/mcp.go`
- Test: `internal/anthropic/mcp_test.go`

**Interfaces:**
- Produces: `injectMCP` 行为扩展；包私有 `relocateToolsCacheControl(obj map[string]any)`

- [ ] **Step 1: 写失败测试**

```go
// TestInjectMCPRelocatesCacheControlToLastToolset 普通 tools + MCP：
// 旧 function 上的 cache_control 必须清除，末项 mcp_toolset 必须带断点。
func TestInjectMCPRelocatesCacheControlToLastToolset(t *testing.T) {
	body := []byte(`{
		"model":"x",
		"cache_control":{"type":"ephemeral","ttl":"5m"},
		"tools":[{"type":"tool","name":"f","cache_control":{"type":"ephemeral","ttl":"5m"}}]
	}`)
	out, err := injectMCP(body, &MCPInjection{
		Servers:  []MCPServer{{URL: "https://s.example", Name: "weather"}},
		Toolsets: []MCPToolset{{MCPServerName: "weather", EnabledTools: []string{"get"}}},
	})
	if err != nil {
		t.Fatalf("injectMCP: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools := obj["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools len=%d want 2", len(tools))
	}
	first := tools[0].(map[string]any)
	if _, ok := first["cache_control"]; ok {
		t.Fatalf("first tool must not keep cache_control: %v", first)
	}
	last := tools[1].(map[string]any)
	cc, ok := last["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("last tool missing cache_control: %v", last)
	}
	if cc["type"] != "ephemeral" || cc["ttl"] != "5m" {
		t.Fatalf("bad cache_control: %v", cc)
	}
}

// TestInjectMCPOnlyToolsetGetsCacheControl 仅 MCP（初始 tools 空）时
// 末项 mcp_toolset 仍应带 cache_control，TTL 继承顶层 1h。
func TestInjectMCPOnlyToolsetGetsCacheControl(t *testing.T) {
	body := []byte(`{
		"model":"x",
		"cache_control":{"type":"ephemeral","ttl":"1h"},
		"tools":[]
	}`)
	out, err := injectMCP(body, &MCPInjection{
		Servers:  []MCPServer{{URL: "https://s.example", Name: "weather"}},
		Toolsets: []MCPToolset{{MCPServerName: "weather"}},
	})
	if err != nil {
		t.Fatalf("injectMCP: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	tools := obj["tools"].([]any)
	last := tools[0].(map[string]any)
	cc := last["cache_control"].(map[string]any)
	if cc["ttl"] != "1h" {
		t.Fatalf("ttl want 1h, got %v", cc["ttl"])
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/anthropic/ -run 'TestInjectMCPRelocates|TestInjectMCPOnlyToolset' -v`
Expected: FAIL

- [ ] **Step 3: 实现 relocateToolsCacheControl 并在 injectMCP 末尾调用**

```go
// relocateToolsCacheControl 在 MCP toolset 追加后，保证 tools 列表只有
// 末项一个 cache_control 断点；TTL 继承顶层 cache_control，缺省 5m。
func relocateToolsCacheControl(obj map[string]any) {
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}
	ttl := "5m"
	if top, ok := obj["cache_control"].(map[string]any); ok {
		if t, ok := top["ttl"].(string); ok && t != "" {
			ttl = t
		}
	}
	for _, item := range tools {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		delete(m, "cache_control")
	}
	last, ok := tools[len(tools)-1].(map[string]any)
	if !ok {
		slog.Warn("tools 末项不是 object，无法设 cache_control，tools 列表缓存将丢失")
		return
	}
	last["cache_control"] = map[string]any{
		"type": "ephemeral",
		"ttl":  ttl,
	}
}
```

在 `injectMCP` 中 `obj["tools"] = tools` 之后、`return json.Marshal` 之前调用 `relocateToolsCacheControl(obj)`。

需 import `log/slog`。

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/anthropic/ -run 'InjectMCP|MergeBeta' -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/anthropic/mcp.go internal/anthropic/mcp_test.go
git commit -m "fix(anthropic): MCP 注入后重定位 tools 末项 cache_control"
```

---

### Task 3: 1h TTL 加 extended-cache-ttl beta

**Files:**
- Modify: `internal/anthropic/client.go`、`internal/anthropic/mcp.go`（常量可放 mcp.go 旁或 client.go）
- Test: `internal/anthropic/client_test.go`

**Interfaces:**
- Produces: `const ExtendedCacheTTLBetaHeader = "extended-cache-ttl-2025-04-11"`
- Produces: `appendBeta(existing, value string) string`（去重合并，可复用/替换 mergeBetaHeader 内部）

- [ ] **Step 1: 写失败测试**

```go
func TestStreamExtendedCacheTTLBetaOn1h(t *testing.T) {
	var gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req := &anthropic.MessageNewParams{
		CacheControl: anthropic.CacheControlEphemeralParam{
			Type: "ephemeral",
			TTL:  anthropic.CacheControlEphemeralTTLTTL1h,
		},
	}
	rc, err := New().Stream(context.Background(), srv.URL, "k", req, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	rc.Close()
	if !strings.Contains(gotBeta, ExtendedCacheTTLBetaHeader) {
		t.Fatalf("anthropic-beta missing extended-cache-ttl: %q", gotBeta)
	}
}

func TestStreamNoExtendedCacheTTLOn5m(t *testing.T) {
	var gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req := &anthropic.MessageNewParams{
		CacheControl: anthropic.CacheControlEphemeralParam{
			Type: "ephemeral",
			TTL:  anthropic.CacheControlEphemeralTTLTTL5m,
		},
	}
	rc, err := New().Stream(context.Background(), srv.URL, "k", req, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	rc.Close()
	if strings.Contains(gotBeta, ExtendedCacheTTLBetaHeader) {
		t.Fatalf("5m must not set extended-cache-ttl: %q", gotBeta)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/anthropic/ -run 'TestStreamExtendedCacheTTL|TestStreamNoExtendedCacheTTL' -v`
Expected: FAIL（常量未定义 / header 不含）

- [ ] **Step 3: 实现**

在 `mcp.go` 或 `client.go`：

```go
const ExtendedCacheTTLBetaHeader = "extended-cache-ttl-2025-04-11"
```

`appendBeta`：

```go
func appendBeta(existing, value string) string {
	if value == "" {
		return existing
	}
	if existing == "" {
		return value
	}
	if strings.Contains(existing, value) {
		return existing
	}
	return existing + "," + value
}
```

`mergeBetaHeader` 可改为 `return appendBeta(existing, MCPBetaHeader)`。

`Stream` beta 组装：

```go
	beta := ""
	if thinkingEnabled(req) {
		beta = "interleaved-thinking-2025-05-14"
	}
	if mcp != nil && mcp.NeedsBeta() {
		beta = appendBeta(beta, MCPBetaHeader)
	}
	if req != nil && req.CacheControl.TTL == anthropic.CacheControlEphemeralTTLTTL1h {
		beta = appendBeta(beta, ExtendedCacheTTLBetaHeader)
	}
```

- [ ] **Step 4: 测试通过**

Run: `go test ./internal/anthropic/ -count=1`

- [ ] **Step 5: Commit**

```bash
git add internal/anthropic/client.go internal/anthropic/mcp.go internal/anthropic/client_test.go
git commit -m "feat(anthropic): 1h cache TTL 自动加 extended-cache-ttl beta"
```

---

### Task 4: 文档同步 + 回归

**Files:**
- Modify: `docs/protocol-coverage.md`（prompt cache 相关行）

- [ ] **Step 1: 更新 protocol-coverage 说明**

在「网关自主 Anthropic cache_control」相关 bullet 或 `prompt_cache_options` 行补：

- MCP toolset 在 inject 后重定位 tools 末项断点
- `cache.ttl=1h` 时带 `extended-cache-ttl-2025-04-11`

- [ ] **Step 2: 全量相关测试**

Run: `go test ./internal/anthropic/ ./internal/toolcatalog/ ./internal/convert/ ./internal/server/ -count=1`

- [ ] **Step 3: Commit**

```bash
git add docs/protocol-coverage.md
git commit -m "docs(protocol): 同步 cache_control MCP 断点与 1h beta 说明"
```

## Spec Coverage Checklist

| Spec 项 | Task |
|---|---|
| MCP tools 断点重定位 | Task 2 |
| ApplyCacheControl GetCacheControl | Task 1 |
| 1h extended-cache-ttl | Task 3 |
| 测试加固 | Task 1–3 |
| protocol-coverage 同步 | Task 4 |
| usage 拆分 | 非目标，跳过 |

## Execution

本计划可在当前会话 inline 执行（executing-plans / 直接 TDD），无需强制 subagent。
