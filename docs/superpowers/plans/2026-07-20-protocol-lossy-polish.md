# 协议 lossy 打磨 + 收口策略 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `docs/protocol-coverage.md` 写清产品/技术收口点；对范围内仍 lossy 的可映射字段做有限打磨（web_search `user_location`、shell 历史 env 元数据、code_interpreter image 日志），不扩展产品边界、不兼容 deprecated 字段。

**Architecture:** 协议真相源仍是 `docs/protocol-coverage.md`。实现落在既有分层：`toolcatalog.Declare`（工具声明）、`convert`（input 历史回灌）、`server.warnDroppedOrIgnoredParams`（请求级 WARN）、`streamconv` 本批不动。每个打磨项先 RED 测试锁定期望，再改实现与矩阵行说明。

**Tech Stack:** Go 1.26.5、`github.com/openai/openai-go/v3@v3.42.0`、`github.com/anthropics/anthropic-sdk-go@v1.57.0`、既有 `go test` / `task check`。

## Global Constraints

- **产品边界不扩大**：不做 session store、Conversation、background/queued、file_search/computer/image_gen/audio、MCP 审批协议、OpenAI Files 拉取。
- **deprecated 一律丢弃**：`reasoning.generate_summary`、`prompt_cache_retention`、`user` 等只 WARN + 忽略，**禁止**复用到新路径（如 generate_summary → summary）。
- **不做假映射**：Anthropic SDK 无字段的 OpenAI 参数不得编造（例如 `web_search.search_context_size` → Anthropic 无等价字段 → WARN + 忽略，不注入 system 提示假装实现）。
- **日志规范**：业务日志只用 `log/slog` 结构化键值；重要数据丢弃必须 WARN，并含类型/标识/impact。
- **分层**：工具声明改 `internal/toolcatalog`；input item 回灌改 `internal/convert`；请求参数 WARN 改 `internal/server`；禁止在 `server` 写协议转换。
- **TDD**：每个行为变更先写失败测试，再最小实现，再 GREEN；提交用 Conventional Commits（可中文描述）。
- **测试命令**：单测 `go test ./internal/<pkg>/ -count=1 -run <TestName> -v`；门禁 `task check`（或 `gofmt` + `go vet` + `go test ./...`）。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `docs/protocol-coverage.md` | 协议矩阵 + **收口策略**专节 + 各行状态说明（本批真相源） |
| `internal/toolcatalog/declare.go` | `web_search` / `web_search_preview` → Anthropic `WebSearchTool20250305Param` 字段映射 |
| `internal/toolcatalog/declare_test.go` | Declare 单测 |
| `internal/convert/request.go` | shell/local_shell 历史回灌 input 形状；code_interpreter image 丢弃日志/logs 注释 |
| `internal/convert/request_test.go` | 回灌与 WARN 行为测试 |
| `internal/server/server.go` | 可选：`search_context_size` 若仅出现在 tools 上，WARN 在 convert/declare 路径更合适（见 Task 2） |
| `README.md` | 仅当「已知限制」段落与矩阵冲突时同步一句（Task 5 评估） |

本批 **不修改** `streamconv`（出站 shell/web_search 事件链已可用）。

---

## Task 1: 协议映射文档写明收口点

**Files:**
- Modify: `docs/protocol-coverage.md`（在「关注面与产品边界」之后新增专节；同步变更记录）

**Interfaces:**
- Consumes: 无代码接口
- Produces: 文档节「## 收口策略（产品 + 技术）」供后续任务与 PR 引用

- [ ] **Step 1: 在 `docs/protocol-coverage.md`「关注面与产品边界」节后插入收口策略**

在 `## 关注面与产品边界` 整节之后、`## 变更记录` 之前，插入（全文粘贴，勿删改既有边界 bullet）：

```markdown
## 收口策略（产品 + 技术）

本节是协议映射的**硬边界**。后续 PR 若扩大边界，必须先改本节再改代码。

### 1. 产品范围内（做）

- Codex CLI → Anthropic 兼容后端的 **Responses ↔ Messages 直转**。
- 客户端自带完整 `input` 回灌；网关无 session store。
- 可语义映射的 tool / content / SSE 生命周期；有损处登记 `lossy_supported` 并说明损失。
- 网关自主 Anthropic `cache_control`（配置 TTL），不依赖 OpenAI prompt cache key。

### 2. 产品范围外（声明不做）

| 能力 | 处理 |
|---|---|
| `previous_response_id` / Conversation / 本地 `store` 会话回填 | `unsupported_by_backend` 或 echo-only；非空 WARN |
| `background` / `queued` / 非 SSE 同步 JSON 响应体 | 不做 |
| `file_search` / `computer*` / `image_generation` / `programmatic_tool_calling` | 工具声明 fail-fast；历史 item `dropped` |
| `audio*` SSE | 不做 |
| MCP `require_approval` 审批协议 | 降级 never + WARN；审批历史 item `dropped` |
| OpenAI Files 凭据拉取（`file_id`） | WARN + 丢弃 |
| OpenAI moderation / safety_identifier 透传 | WARN + 忽略 |

### 3. 后端/协议限制（无法等价实现）

| 限制 | 处理 |
|---|---|
| Anthropic 无 OpenAI `search_context_size` 字段 | **不得假映射**；请求带该字段时 WARN + 忽略 |
| Anthropic 无 output logprobs / stream obfuscation | WARN + 忽略 |
| Anthropic MCP 仅 `authorization_token` | 非 Bearer 的自定义 headers WARN + 丢弃 |
| Anthropic code_execution 无 container / 生成文件 URL / image 输出字段 | 丢弃 + WARN；logs 文本尽量保留 |
| 未知 Anthropic server tool（web_fetch 等） | 流式 **WARN + skip**，不 `response.failed` |

### 4. Deprecated 字段（一律丢弃）

下列字段 **禁止** 做兼容映射或注入 system 模拟：

| 字段 | 行为 |
|---|---|
| `reasoning.generate_summary` | WARN + 忽略（只用 `reasoning.summary`） |
| `prompt_cache_retention` | WARN + 忽略（不用其推导 TTL） |
| `user`（OpenAI 已废弃） | WARN + 忽略（可用 `metadata.user_id`） |

### 5. lossy 打磨原则

- **优先透传 SDK 两侧均存在的字段**（如 web_search `user_location`）。
- **历史回灌**可把客户端执行元数据折进 `tool_use.input` JSON（lossy 保留线索），不得改成 Anthropic 不认识的顶层字段。
- **不扩大 fail-fast 范围**去「假装严格」；已知无等价继续 WARN/drop 或 fail-fast 与矩阵一致。
- 每改一行矩阵状态，同步本文件变更记录日期小节。

### 6. 本批打磨范围（有序）

1. web_search：`user_location` 映射；`search_context_size` 明确 WARN（不可映射）。
2. shell / local_shell **历史 item**：env / cwd / timeout 等折入 `tool_use` input。
3. code_interpreter 历史 image 输出：丢弃语义不变，日志与 logs 文本更清晰。
4. MCP：文档确认 `allowed_tools` **字符串 allowlist 已支持**；filter 形态保持 WARN + 全启用（本批不展开 filter AST）。
```

- [ ] **Step 2: 变更记录追加一条**

在 `### 2026-07-20` 下追加：

```markdown
- **收口策略专节**：产品范围 / 范围外 / 后端限制 / deprecated 一律丢弃 / lossy 打磨原则写入 `docs/protocol-coverage.md`。
```

- [ ] **Step 3: 自检文档**

Run:

```bash
rg -n '收口策略|Deprecated 字段|search_context_size' docs/protocol-coverage.md
```

Expected: 命中新建专节与表格行；无残留「将实现 generate_summary 兼容」类表述。

- [ ] **Step 4: Commit**

```bash
git add docs/protocol-coverage.md
git commit -m "docs(protocol): 写明协议映射收口策略"
```

---

## Task 2: web_search `user_location` 映射 + `search_context_size` WARN

**Files:**
- Modify: `internal/toolcatalog/declare.go`（`OfWebSearch` / `OfWebSearchPreview` 分支）
- Modify: `internal/toolcatalog/declare_test.go`
- Modify: `docs/protocol-coverage.md` Tool Union 中 `web_search` / `web_search_preview` 行

**Interfaces:**
- Consumes: `oairesponses.WebSearchToolParam.UserLocation`（`City`/`Country`/`Region`/`Timezone` 为 `param.Opt[string]`）；`SearchContextSize` 枚举字符串
- Produces: `anthropic.WebSearchTool20250305Param.UserLocation`（`anthropic.UserLocationParam`）；**无** `SearchContextSize` 字段（SDK 不存在）

**事实（禁止写反）：**

- Anthropic `WebSearchTool20250305Param` 字段含：`AllowedDomains`、`BlockedDomains`、`UserLocation`、`MaxUses` 等，**没有** `search_context_size`。
- OpenAI `WebSearchToolParam` 有 `Filters.AllowedDomains`、`UserLocation`、`SearchContextSize`（low/medium/high）。

- [ ] **Step 1: 写失败测试（RED）**

在 `internal/toolcatalog/declare_test.go` 追加：

```go
func TestDeclareWebSearchMapsUserLocation(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfWebSearch: &oairesponses.WebSearchToolParam{
		Filters: oairesponses.WebSearchToolFiltersParam{AllowedDomains: []string{"example.com"}},
		UserLocation: oairesponses.WebSearchToolUserLocationParam{
			City:     oparam.NewOpt("Shanghai"),
			Country:  oparam.NewOpt("CN"),
			Region:   oparam.NewOpt("Shanghai"),
			Timezone: oparam.NewOpt("Asia/Shanghai"),
		},
	}})
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	ws := decls[0].OfWebSearchTool20250305
	if ws == nil {
		t.Fatal("expected WebSearchTool20250305")
	}
	if ws.UserLocation.City.Value != "Shanghai" || ws.UserLocation.Country.Value != "CN" {
		t.Fatalf("user_location not mapped: %+v", ws.UserLocation)
	}
	if len(ws.AllowedDomains) != 1 || ws.AllowedDomains[0] != "example.com" {
		t.Fatalf("allowed_domains regression: %+v", ws.AllowedDomains)
	}
}

func TestDeclareWebSearchSearchContextSizeDoesNotPanic(t *testing.T) {
	// Anthropic 无 search_context_size：Declare 必须成功且不假装写入不存在字段。
	decls, err := Declare(oairesponses.ToolUnionParam{OfWebSearch: &oairesponses.WebSearchToolParam{
		SearchContextSize: oairesponses.WebSearchToolSearchContextSizeHigh,
	}})
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if decls[0].OfWebSearchTool20250305 == nil {
		t.Fatal("expected web search tool")
	}
}
```

若 `WebSearchToolSearchContextSizeHigh` 常量名与 SDK 不一致，以 `rg 'WebSearchToolSearchContextSize' $(go env GOMODCACHE)/github.com/openai/openai-go/v3@v3.42.0/responses/` 结果为准。

- [ ] **Step 2: Run RED**

```bash
go test ./internal/toolcatalog/ -count=1 -run 'TestDeclareWebSearchMapsUserLocation' -v
```

Expected: FAIL（`user_location` 仍为零值）。

- [ ] **Step 3: 实现映射 helper（最小）**

在 `internal/toolcatalog/declare.go` 中：

1. 把 `OfWebSearch` / `OfWebSearchPreview` 改为调用共享构造，例如：

```go
func webSearchToolFromOpenAI(allowed []string, loc oairesponses.WebSearchToolUserLocationParam) anthropic.ToolUnionParam {
	p := &anthropic.WebSearchTool20250305Param{AllowedDomains: allowed}
	if loc.City.Valid() || loc.Country.Valid() || loc.Region.Valid() || loc.Timezone.Valid() {
		p.UserLocation = anthropic.UserLocationParam{
			City:     loc.City,
			Country:  loc.Country,
			Region:   loc.Region,
			Timezone: loc.Timezone,
		}
	}
	return anthropic.ToolUnionParam{OfWebSearchTool20250305: p}
}
```

注意：OpenAI / Anthropic 两侧 `param.Opt[string]` 包路径不同时，用 `.Valid()` / `.Value` 显式拷贝，不要假设类型别名可赋值。

2. `OfWebSearch`：

```go
case t.OfWebSearch != nil:
	return []anthropic.ToolUnionParam{
		webSearchToolFromOpenAI(t.OfWebSearch.Filters.AllowedDomains, t.OfWebSearch.UserLocation),
	}, nil
```

3. `OfWebSearchPreview`：若 preview 结构也有 `UserLocation`，同样传入；否则保持空 location + 空 domains。

4. **`SearchContextSize`：** 不在 Anthropic param 上写任何字段。可选：在 `Declare` 内当 `SearchContextSize != ""` 时：

```go
slog.Warn("忽略 web_search.search_context_size（Anthropic web_search 无等价字段），对应数据被丢弃",
	"field", "search_context_size",
	"value", string(t.OfWebSearch.SearchContextSize),
	"impact", "不会调整 Anthropic 搜索上下文规模")
```

需 `import "log/slog"`。

- [ ] **Step 4: GREEN**

```bash
go test ./internal/toolcatalog/ -count=1 -run 'TestDeclareWebSearch' -v
go test ./internal/convert/ -count=1 -run 'TestWebSearchToolMaps' -v
```

Expected: PASS。

- [ ] **Step 5: 更新矩阵行**

`docs/protocol-coverage.md` Tool Union：

| 行 | 新说明 |
|---|---|
| `web_search` | `filters.allowed_domains` → `allowed_domains`；`user_location` → `user_location`；`search_context_size` 无 Anthropic 字段 → WARN + 忽略 |
| `web_search_preview` | 同 web_search（字段以 preview SDK 为准） |

状态：可保持 `supported`（主路径完整），在说明中写清 `search_context_size` 为有损忽略；若希望更严可标 `lossy_supported`——**本计划采用 `lossy_supported`**，因 `search_context_size` 会被丢弃。

- [ ] **Step 6: Commit**

```bash
git add internal/toolcatalog/declare.go internal/toolcatalog/declare_test.go docs/protocol-coverage.md
git commit -m "feat(toolcatalog): web_search 映射 user_location，忽略 search_context_size"
```

---

## Task 3: shell / local_shell 历史回灌保留 env 元数据

**Files:**
- Modify: `internal/convert/request.go`（`appendShellCall`、`appendLocalShellCall`）
- Modify: `internal/convert/request_test.go`
- Modify: `docs/protocol-coverage.md` Input Item 中 shell/local_shell 行

**Interfaces:**
- Consumes:
  - `ResponseInputItemLocalShellCallParam.Action`：`Command []string`、`Env map[string]string`、`TimeoutMs`、`User`、`WorkingDirectory`
  - `ResponseInputItemShellCallParam.Action.Commands`；可选 `Environment`（container/local 身份，非 env map）
- Produces: Anthropic assistant `tool_use` name=`shell`，`input` 为 JSON object，**至少**含 `"input"` 字符串（兼容现有 freeform 契约），并附加可映射元数据键

**现有行为（须保持兼容）：**

```go
// local: Command 用空格 join；shell: Commands 用 \n join
appendToolUse(..., "shell", map[string]any{"input": <joined>})
```

- [ ] **Step 1: RED 测试**

在 `internal/convert/request_test.go` 追加（JSON 请求体风格与邻近 shell 测试一致）：

```go
func TestLocalShellCallPreservesEnvInToolUseInput(t *testing.T) {
	req := mustReq(t, `{
		"model":"gpt-5",
		"input":[{
			"type":"local_shell_call",
			"id":"lsc_1",
			"call_id":"call_lsc_1",
			"status":"completed",
			"action":{
				"type":"exec",
				"command":["echo","hi"],
				"env":{"FOO":"bar"},
				"working_directory":"/tmp",
				"timeout_ms":5000
			}
		}]
	}`)
	out, _, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("ToAnthropic: %v", err)
	}
	// 在 messages 中找到 tool_use name=shell，断言 input map 含 env/working_directory/timeout_ms
	// 且仍含 "input" 文本键（兼容）
}
```

实现断言时遍历 `out.Messages` → assistant content → `OfToolUse`，`json` 或类型断言读 `Input`。

同类加 `TestShellCallStillEmitsInputKey` 防止回归：仅 commands 时仍只有可执行文本。

若 `mustReq` 对 local_shell wire 解码失败，先用仓库内已有 `TestLocalShellCallInputItemConvertsToShellToolUse` 扩展，而不是新造解码路径。

- [ ] **Step 2: Run RED**

```bash
go test ./internal/convert/ -count=1 -run 'TestLocalShellCallPreservesEnvInToolUseInput|TestLocalShellCallInputItem' -v
```

Expected: 新测试 FAIL（input 仅有 command 文本）。

- [ ] **Step 3: 最小实现**

改 `appendLocalShellCall`：

```go
func appendLocalShellCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemLocalShellCallParam) error {
	input := map[string]any{
		"input": strings.Join(call.Action.Command, " "),
	}
	if len(call.Action.Env) > 0 {
		input["env"] = call.Action.Env
	}
	if call.Action.WorkingDirectory.Valid() && call.Action.WorkingDirectory.Value != "" {
		input["working_directory"] = call.Action.WorkingDirectory.Value
	}
	if call.Action.TimeoutMs.Valid() {
		input["timeout_ms"] = call.Action.TimeoutMs.Value
	}
	if call.Action.User.Valid() && call.Action.User.Value != "" {
		input["user"] = call.Action.User.Value
	}
	return appendToolUse(out, call.CallID, "shell", input)
}
```

`appendShellCall`：保持 `Commands` join 为 `input`；若 `call.Environment` 非空，可 `lossy` 写入 `"environment_type"`（local/container_auto/container_reference 之一），**不要** dump 整个 union raw 进 system。无法安全序列化则跳过并 `slog.Debug`，不 WARN（非重要业务数据丢失）。

注意：`appendToolUse` 的 call id 参数——local_shell 历史用 `CallID` 还是 `ID` 与现有函数一致（当前是 `call.CallID` / 现有 `appendLocalShellCall` 签名），**不要改 id 语义**。

- [ ] **Step 4: GREEN + 回归**

```bash
go test ./internal/convert/ -count=1 -run 'Shell|ApplyPatch|LocalShell' -v
```

Expected: PASS。

- [ ] **Step 5: 更新矩阵**

Input Item：

- `local_shell_call`：说明改为「命令文本 + env/cwd/timeout/user 折入 tool_use.input；仍 lossy（无 Anthropic 原生 shell env 协议）」
- `shell_call`：说明命令数组 + 可选 environment 类型线索；limits/caller 仍未完整映射

- [ ] **Step 6: Commit**

```bash
git add internal/convert/request.go internal/convert/request_test.go docs/protocol-coverage.md
git commit -m "feat(convert): shell 历史回灌保留 env/cwd/timeout 元数据"
```

---

## Task 4: code_interpreter 历史 image 输出可观测性

**Files:**
- Modify: `internal/convert/request.go`（`codeInterpreterLogs`）
- Modify: `internal/convert/request_test.go`（已有 `TestCodeInterpreterImageOutputWarns` 则扩展）
- Modify: `docs/protocol-coverage.md` 对应说明一句（可选）

**Interfaces:**
- Consumes: `ResponseCodeInterpreterToolCallOutputUnionParam`（`OfLogs` / `OfImage`）
- Produces: 拼进 `code_execution` result 的文本 logs；image 仍不进入 Anthropic result 结构

- [ ] **Step 1: RED / 扩展现有测试**

扩展 `TestCodeInterpreterImageOutputWarns`（或新建）：

1. 捕获 slog：必须仍含「丢弃」与 `impact`。
2. 转换后 tool_result / code_execution result 文本中，若同 item 仅有 image、无 logs，应含可读占位，例如：

```text
[code_interpreter image output omitted: no Anthropic equivalent]
```

避免上游只看到空 result 误以为执行无输出。

- [ ] **Step 2: 实现**

```go
} else if o.OfImage != nil {
	url := o.OfImage.URL
	slog.Warn("丢弃 code_interpreter_call 的 image 输出（Anthropic code_execution 无等价字段），对应数据被丢弃",
		"url", url,
		"impact", "图片不会出现在 code_execution_tool_result 中")
	parts = append(parts, "[code_interpreter image output omitted: no Anthropic equivalent]")
}
```

**禁止**把 base64/URL 全量塞进 logs（可能极大）；URL 仅进 WARN 字段。

- [ ] **Step 3: GREEN**

```bash
go test ./internal/convert/ -count=1 -run 'CodeInterpreter' -v
```

Expected: PASS。

- [ ] **Step 4: Commit**

```bash
git add internal/convert/request.go internal/convert/request_test.go docs/protocol-coverage.md
git commit -m "fix(convert): code_interpreter image 丢弃时保留可读 logs 占位"
```

---

## Task 5: MCP allowlist 文档对齐（无行为变更除非测试缺口）

**Files:**
- Modify: `docs/protocol-coverage.md`（`mcp` tool 行 + 收口策略已写 filter 边界）
- Test only if gap: `internal/convert/request_test.go`

**事实：**

- `allowedMCPToolNames` 已返回 `OfMcpAllowedTools`（`[]string`）。
- `injectMCP`：`EnabledTools` 非空 → `configs[name].enabled=true` + `default_config.enabled=false`。
- `OfMcpToolFilter` → WARN + 全启用。

- [ ] **Step 1: 确认已有测试覆盖 allowlist**

```bash
go test ./internal/convert/ -count=1 -run 'Mcp' -v
rg -n 'EnabledTools|allowed_tools|OfMcpAllowedTools' internal/convert/*_test.go internal/anthropic/*_test.go
```

若已有「非空 allowlist → configs」测试：本 Task 只改文档。

若无：补 `TestMcpAllowedToolsAllowlistPassedToInjection`：构造 `tools:[{type:mcp,...,allowed_tools:["a","b"]}]`，断言 `collectMCP` 或端到端 marshal 后 toolset `configs` 含 a/b。

- [ ] **Step 2: 矩阵表述**

`mcp` 行说明应明确：

- `allowed_tools: string[]` → Anthropic toolset allowlist（supported / 已实现）
- `allowed_tools: filter` → WARN + 全启用（lossy）
- `require_approval≠never` → 降级 never + WARN
- 自定义 headers 仅 Bearer → `authorization_token`

- [ ] **Step 3: Commit**

```bash
git add docs/protocol-coverage.md internal/convert/request_test.go  # 若有测试
git commit -m "docs(protocol): 明确 MCP allowed_tools allowlist 与 filter 边界"
```

---

## Task 6: 门禁与收口自检

**Files:** 无新文件；验证 Tasks 1–5

- [ ] **Step 1: 跑协议相关包测试**

```bash
go test ./internal/toolcatalog/ ./internal/convert/ ./internal/server/ ./internal/streamconv/ -count=1
```

Expected: PASS。

- [ ] **Step 2: 门禁**

```bash
task check
```

若无 Task：`gofmt -l internal/ cmd/` 为空 + `go vet ./...` + `go test ./...`。

- [ ] **Step 3: 文档一致性扫描**

```bash
# 不应再出现「将复用 generate_summary」或「search_context_size 映射到 Anthropic」假承诺
rg -n 'generate_summary.*复用|search_context_size.*映射|仍显式失败' docs/protocol-coverage.md && echo 'FOUND bad phrases' || echo 'OK'

# 收口专节存在
rg -n '## 收口策略' docs/protocol-coverage.md
```

Expected: bad phrases 无命中；收口专节存在。

- [ ] **Step 4: 最终 commit（若有 fmt 仅改动）**

```bash
git status
# 若仅 gofmt：
git add -u && git commit -m "chore: gofmt after protocol polish"
```

---

## 明确不在本计划内（YAGNI）

| 项 | 原因 |
|---|---|
| `reasoning.generate_summary` → `summary` 兼容 | deprecated，用户要求一律丢弃 |
| `text.verbosity` system 注入 | 假映射 + 污染 cache |
| `max_tool_calls` 网关截断 | 单请求工具环由客户端控制；收益低 |
| `response.cancelled` SSE 终态 | 对端已断；维持 metrics-only |
| MCP filter AST 展开 | 复杂度高；本批文档边界即可 |
| `search_context_size` 映射 | Anthropic SDK 无字段 |
| streamconv / admin / tray | 与本批 lossy 无关 |

---

## Self-Review

1. **Spec coverage：** 收口文档、user_location、shell env、code_interpreter image 可观测、MCP allowlist 文档均有独立 Task；deprecated 不兼容写在 Task 1 硬约束。
2. **Placeholder scan：** 无 TBD；SDK 常量名处要求以 `rg` 实测为准并给出路径。
3. **Type consistency：** `UserLocation` 两侧字段名一致；shell input 键名在 Task 3 固定为 `input`/`env`/`working_directory`/`timeout_ms`/`user`。
4. **风险点已写明：** `search_context_size` 不可映射；local_shell wire 解码依赖既有 `mustReq` 路径。

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-20-protocol-lossy-polish.md`.

**Two execution options:**

1. **Subagent-Driven (recommended)** — 每 Task 新开 subagent，Task 间 review  
2. **Inline Execution** — 本会话按 `executing-plans` 批量执行并设检查点  

Which approach?
