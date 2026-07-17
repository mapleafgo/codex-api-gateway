# streamconv 回程 call 通用化设计（callKind）

## 背景

Codex 经本网关使用 MCP 时，`tool_search`（Codex 懒加载 MCP 工具的入口）连续被 aborted，导致 fetch/deepwiki 等 deferred 工具暴露不出来。

根因（已查实，见记忆 `bug/streamconv-tool-search-call-missing`）：`tool_search` 是 OpenAI Responses 的 hosted tool，模型调用后应产出独立的 `tool_search_call` item；但 `internal/streamconv/converter.go` 把上游 `tool_use(tool_search)` 当普通 function 处理，产出 `function_call`。item type 对不上，Codex 不认 → abort → 懒加载断。

更深层问题：converter 的回程 call 处理是「每 call 一个特例 handler」（`handleToolUseStart` / `handleServerToolUseStart` / `handleMcpToolUseStart` + 各 result handler），三个 handler 重复同一套流水线（allocate→build→added→events→done），且 `tool_search` 就是因为没有对应分支才漏掉。每加一种 call 类型就要加一个 handler，重复且易漏。

## 目标

1. **修复 tool_search_call 回程缺失**（验收标志：`tool_use(tool_search)` 产出 `tool_search_call` item，Codex 不再 abort，MCP 工具能经 tool_search 暴露后被调用）。
2. **把回程 call 重构为通用流水线**：`callKind` 策略 + `dispatchCallKind` + 通用 `handleCallStart/Delta/Stop/Result`，统一覆盖 function / custom / tool_search / web_search / code_interpreter / mcp 六类。新增 call 类型只改 dispatch 注册表，不加 handler。
3. **保回归**：现有五类 call 的事件序列逐项不变。

## 架构

### 上游 → 下游映射

上游 Anthropic call 来源（block type）有三类，下游 OpenAI Responses call item 有六类：

| 上游 block | name / 来源 | → 下游 item | arguments 模式 | result block |
|---|---|---|---|---|
| `tool_use` | custom 名单 | `custom_tool_call` | input（一次性） | 无（client 回灌） |
| `tool_use` | `tool_search` | `tool_search_call` | arguments（done 一次性，无 delta 事件） | 无 |
| `tool_use` | 其他 | `function_call` | arguments（delta/done） | 无 |
| `server_tool_use` | web_search / web_search_prime（方言） | `web_search_call` | action.query（状态事件） | `web_search_tool_result`（或 GLM 方言 `tool_result`） |
| `server_tool_use` | code_execution | `code_interpreter_call` | code（delta/done） | `code_execution_tool_result` |
| `mcp_tool_use` | probe 合成 | `mcp_call` | arguments（delta/done） | `mcp_tool_result` |

差异轴仅 4 个：① itemType；② 承载字段；③ arguments 流式方式；④ result 处理。

### callKind 策略接口

封装「一种 call 的完整生命周期」。通用流水线按策略驱动。

```go
type callKind interface {
    itemType() string        // function_call / tool_search_call / web_search_call / ...
    idPrefix() string        // fc / tsc / ws / ci / mcp_
    tracksToolUseID() bool   // hosted call 关联 result block 用

    // content_block_start：构建初始 item
    buildItem(itemIdx int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem
    // output_item.added 之后的事件链（in_progress / searching / code.delta / ...）
    startEvents(c *Converter, itemIdx int, itemID string) []model.SSEEvent
    // content_block_delta（input_json_delta）：流式 arguments（非流式 call 返回 nil）
    consumeDelta(c *Converter, st *callState, partial string) []model.SSEEvent
    // content_block_stop：完成 item + done 事件链（arguments.done / input.delta+done / completed）
    finish(c *Converter, st *callState, args string) (model.OutputItem, []model.SSEEvent)
    // result block 处理（client call 返回 nil）
    handleResult(c *Converter, ev *anthropic.MessageStreamEventUnion, itemIdx int) []model.SSEEvent
}

type callState struct {
    kind       callKind
    itemIdx    int
    itemID     string
    callID     string  // Anthropic tool_use id
    name       string
    argBuilder *strings.Builder
}
```

### dispatchCallKind

`dispatchCallKind(c, ev) callKind`：按 `ev.ContentBlock.Type` + name 选策略，未识别返回 nil（走 skip）。

- `tool_use`：customToolNames 命中 → customCallKind；name=="tool_search" → toolSearchCallKind；默认 → functionCallKind
- `server_tool_use`：`toolcatalog.ServerToolByAnthropicName` 命中 → webSearch / codeInterpreter；name 失配且 `declaredServerTools` 唯一 → 按声明身份回退（兼容 GLM `web_search_prime` 方言）；其余 → nil（skip）
- `mcp_tool_use` → mcpCallKind

### 通用流水线（pipeline.go）

Converter 加 `callByBlockIdx map[int]*callState`（block index → 进行中的 call）。三个 byToolUseID map（webSearch/codeExecution/mcp）由 `tracksToolUseID` 的 kind 注册。

- `handleCallStart`：dispatch → kind；nil 则回退 skip；allocate itemIdx/itemID；buildItem；append outputItems；记 callByBlockIdx；tracksToolUseID 则注册 byToolUseID；emit `output_item.added` + startEvents。
- `handleCallDelta`：lookup callByBlockIdx；非 input_json_delta 返回 nil；argBuilder += partial；kind.consumeDelta。
- `handleCallStop`：lookup；args = argBuilder；item, evts = kind.finish；更新 outputItems；emit evts + `output_item.done`；delete callByBlockIdx。
- `handleCallResult`：按 result block 的 tool_use_id 在 byToolUseID maps 查 callState → kind.handleResult。GLM `tool_result` 方言：handleBlockStart 的 tool_result 分支先查 webSearch 关联，命中当 web search result 处理。

### 覆盖范围与降级登记

OpenAI Responses 共 12 种 call item type，网关处理分四类（确保不漏、明确兜底）：

| call item | 处理 | 说明 |
|---|---|---|
| function_call | functionCallKind | tool_use default |
| custom_tool_call | customCallKind | tool_use + customToolNames（含 shell/apply_patch） |
| tool_search_call | toolSearchCallKind | tool_use + name=="tool_search"（本批修复） |
| web_search_call | webSearchCallKind | server_tool_use（含 GLM web_search_prime 方言回退） |
| code_interpreter_call | codeInterpreterCallKind | server_tool_use code_execution |
| mcp_call | mcpCallKind | mcp_tool_use（probe 合成） |
| shell_call | 降级 → custom_tool_call | customToolNames 有意含 "shell"（OfShell 请求侧降级成 name="shell"） |
| local_shell_call | 降级 → custom_tool_call（lossy） | 请求侧 appendLocalShellCall 也合并成 name="shell"，回程无法与 shell_call 区分 |
| apply_patch_call | 降级 → custom_tool_call | customToolNames 有意含 "apply_patch" |
| computer_call | 不支持 | declare.go default fail-fast，回程不会有 |
| file_search_call | 不支持 | 同上 |
| image_generation_call | 不支持 | 同上 |

callKind 实际 6 个即覆盖所有实际场景：shell/local_shell/apply_patch 经 customToolNames 收敛进 customCallKind（**有意降级**，当前无客户端触发 type=shell/apply_patch；遵循 YAGNI，不升级专门 item type；local_shell_call 因请求侧合并注定 lossy）。

dispatch 兜底：未知 `tool_use` name → functionCallKind（default）；未知 `server_tool_use` → skip（`handleSkippedServerToolUseStart`）；未知 result block → skip。

### 组件（每 kind 一个文件）

`callkind_function.go` / `callkind_custom.go` / `callkind_toolsearch.go` / `callkind_websearch.go` / `callkind_codeinterpreter.go` / `callkind_mcp.go`。各实现接口，字段提取/事件序列从现有 handler 原样搬迁（保证回归）。

## 关键设计决策

1. **`tool_search_call.execution = "client"`**：网关请求侧把 `tool_search` 降级成普通 tool（Anthropic 无 hosted 等价），GLM 调用后由 Codex 本地执行搜索（Codex 是 deferred tools 的持有者，后端无法搜），故 execution=client。真机验收（S4）确认；若 Codex 期望 server 则调整。
2. **`tool_search_call` 无流式 arguments 事件**：SDK 无 `ResponseToolSearchCallArgumentsDelta/Done` 事件类型（grep 确认），arguments 只随 `output_item.added`/`done` 的 item 携带。故 toolSearchCallKind.consumeDelta 返回 nil、finish 不产 arguments.done。
3. **新旧并存到收尾**：迁移期允许新旧 handler 并存（dispatch 对未迁移的 kind 返回 nil → 回退旧 handler），最后一个 kind 迁移完才删旧。降低回归风险。
4. **beta mcp probe 不动**：`mcp_tool_use`/`mcp_tool_result` 仍由 `anthropic.ScanEvents` probe + `synthesizeMCPEvent` 合成成标准事件进 converter，与本重构解耦。
5. **web_search 方言兼容保留**：GLM 的 `web_search_prime`（name 失配 + declaredServerTools 回退）和 `tool_result` 形态回传 web search 结果，逻辑迁入 dispatch / webSearch.handleResult，行为不变。

## 测试策略

- **TDD 每 task**：迁移类 task 先把旧 handler 行为钉成 pin 测试（旧代码上 GREEN），再迁移（新代码上同一测试 GREEN）；tool_search 修复类 task 先 RED（证明产出 function_call），再 GREEN（产出 tool_search_call）。
- **回归硬约束**：function/custom/web_search/code_interpreter/mcp 五类的事件序列逐项不变。S8 前 `grep handleToolUseStart\|handleServerToolUseStart\|handleMcpToolUseStart internal/streamconv/` 应空。
- **命令**：单包 `go test ./internal/streamconv/`、`go test ./internal/model/`；全量 `task check`；`task test-race`。

## 范围

A+B 全量（用户确认）：tool_use 类（function/custom/tool_search）+ hosted 类（web_search/code_interpreter/mcp）+ result 通用化，一次性重构。

## 风险与权衡

- **回归风险**：五类 call 事件序列若微变，Codex 行为漂移。缓解：每 task pin 旧行为；新旧并存到 S8。
- **execution 取值风险**：tool_search execution 定 client，S4 真机验收暴露真值。
- **重构面**：converter.go 核心重写，但按 kind 拆文件后每文件聚焦、可独立测试（符合「小而边界清晰的单元」原则）。
- **YAGNI**：不为未来 hypothetical 的 call 类型预留接口；接口按现有六类 + 四差异轴设计，恰好够用。

## 实施计划

见 `docs/superpowers/plans/2026-07-17-streamconv-call-kind-refactor.md`（S1–S8 task 分解 + TDD 步骤）。
