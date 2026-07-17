# streamconv 回程 call → callKind 通用流水线重构

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `internal/streamconv/converter.go` 的回程 call 处理从「每 call 一个特例 handler」（`handleToolUseStart` / `handleServerToolUseStart` / `handleMcpToolUseStart` + 各自 result handler）整体重构为 `callKind` 策略 + 通用流水线，统一覆盖 function / custom / **tool_search** / web_search / code_interpreter / mcp 六类 call item。**顺手修复 tool_search_call 回程缺失**（当前落到 function_call，致 Codex `tool_search` 懒加载 abort、MCP 工具暴露不出来）。

**Architecture:**
- 上游 call 来源（Anthropic block）有三类：`tool_use`（client call：function/custom/tool_search）、`server_tool_use`（hosted：web_search/code_execution）、`mcp_tool_use`（probe 合成）。
- 当前每类一个 `handle*Start`，重复同一套流水线：`allocate itemOrder → build OutputItem → append + 注册 byToolUseID → output_item.added → call 事件链 → (delta) → output_item.done`，且 `tool_search` 漏（无分支，落到 function_call）。
- 重构后：定义 `callKind` 策略接口（封装 itemType / buildItem / startEvents / consumeDelta / finish / handleResult），`dispatchCallKind(blockType, name)` 选策略，通用 `handleCallStart/Delta/Stop/Result` 驱动流水线。**新增 call 类型只改 dispatch 注册表**，不再加 handler。
- 差异轴仅 4 个：① itemType；② 承载字段（arguments / input / action / code / server_label+name）；③ arguments 流式（delta.done / 一次性 done 给 / 状态事件 / 无）；④ result 处理（无 / web_search_tool_result / code_execution_tool_result / mcp_tool_result）。

**Tech Stack:** Go 1.26.5、anthropic-sdk-go `v1.57.0`、openai-go/v3 `v3.42.0`。

## Global Constraints

- **TDD（每 task）**：先写 RED 测试证明当前行为（旧 handler 的预期产出 / tool_search 的错误产出），再实现，再 GREEN。迁移类 task 的 RED 是「重构前后行为不变」的回归测试——**先在旧代码上把现有行为钉死成测试，再迁移**。
- **保回归是硬约束**：function / custom / web_search / code_interpreter / mcp 五类现有 call 的产出事件序列必须逐字节不变（已有测试 + 新增 pin 测试）。重构期允许新旧 handler 短暂并存，T8 才删旧。
- **不改 wire 契约**：不改变 Anthropic→Responses 的事件语义，只改 converter 内部组织。
- **tool_search 修复是验收标志**：T4 后，`tool_use(tool_search)` 必须产出 `tool_search_call` item（type=`tool_search_call`，带 call_id/arguments/execution=client/status），不再产出 function_call。
- **工具名锁定**：`tool_search` 的 Anthropic 侧 tool name 由 `toolcatalog.Declare` 固定为 `"tool_search"`（declare.go:31），回程 dispatch 按此硬匹配。custom 工具名集合由 `convert.FreeformToolNames` 注入（`Converter.customToolNames`）。
- **beta mcp probe 不动**：`mcp_tool_use`/`mcp_tool_result` 仍由 `anthropic.ScanEvents` 的 probe + `synthesizeMCPEvent` 合成成标准事件后进 converter（与本重构解耦）。
- **测试命令**：单包 `go test ./internal/streamconv/`、`go test ./internal/model/`；全量 `task check`；流式 `task test-race`。
- **提交风格**：Conventional Commits，可中文，如 `refactor(streamconv): 引入 callKind 策略 + 通用 call 流水线`。
- **worktree**：全量在 `.worktrees/streamconv-call-refactor` 实施，main 工作区保持干净（用户偏好，同 protocol-first-batch 策略）。
- **call 覆盖范围**：callKind 6 个（function/custom/tool_search/web_search/code_interpreter/mcp）覆盖所有实际场景。shell/local_shell/apply_patch 经 customToolNames 有意降级为 custom_tool_call（当前无客户端触发 type=shell/apply_patch；local_shell_call 因请求侧合并 lossy）。computer/file_search/image_generation 请求侧 declare fail-fast 不支持。完整矩阵见 spec「覆盖范围与降级登记」。dispatch 兜底：未知 tool_use→functionCallKind，未知 server_tool_use→skip。

---

## 文件结构

| 文件 | 改动 |
|---|---|
| `internal/model/outputitem.go` | `OutputItem` 加 `Execution` 字段；`MarshalJSON` 加 `tool_search_call` 分支（id/call_id/arguments/execution/status） |
| `internal/streamconv/callkind.go`（新） | `callKind` 接口 + `callState` + `dispatchCallKind` |
| `internal/streamconv/pipeline.go`（新） | 通用 `handleCallStart` / `handleCallDelta` / `handleCallStop` / `handleCallResult`；`Converter` 加 `callByBlockIdx map[int]*callState` |
| `internal/streamconv/converter.go` | `handleBlockStart`/`handleBlockDelta`/`handleBlockStop` 改为委托通用流水线；**删除** `handleToolUseStart`/`handleServerToolUseStart`/`dispatchServerToolUse`/`handleMcpToolUseStart`/`handleWebSearchResultStart`/`handleCodeExecutionResultStart`/`handleMcpToolResultStart`（T8） |
| `internal/streamconv/callkind_*.go`（新，每 kind 一个文件） | `functionCallKind` / `customCallKind` / `toolSearchCallKind` / `webSearchCallKind` / `codeInterpreterCallKind` / `mcpCallKind` |
| `internal/streamconv/*_test.go` | 每 task 的 RED/GREEN 测试；回归 pin 测试 |
| `docs/protocol-coverage.md` | `tool_search` tool：`unsupported_by_backend` → `supported`（回程 tool_search_call 已通） |

依赖方向不变：`streamconv` ← `model` / `toolcatalog`；无新循环。

---

## Task S1: model 层 tool_search_call item 支持

**Files:**
- Modify: `internal/model/outputitem.go`
- Test: `internal/model/outputitem_test.go`

**Interfaces:**
- Produces: `OutputItem.Execution`（tool_search_call 专用，值 `"client"`——tool_search 请求侧降级成普通 tool 由 GLM 调用、Codex 本地执行搜索，故 execution=client）

- [ ] **Step 1: RED — `tool_search_call` marshal 测试**

  `outputitem_test.go` 加：构造 `OutputItem{Type: ItemTypeToolSearchCall, ID:"tsc_0", CallID:"call_x", Arguments:`{"query":"fetch"}`, Execution:"client", Status:"completed"}`，`json.Marshal` 后断言：
  - `"type":"tool_search_call"`
  - 含 `id` / `call_id` / `arguments` / `execution:"client"` / `status`
  - **不含** `name`（tool_search_call 无 name 字段）、不含 `input`/`output` 等 omitempty 零值字段

  现状：`MarshalJSON` 无 tool_search_call 分支，走默认 `outputItem` 别名 marshal——会带 `name:""` 等多余字段、缺 `execution`。RED 成立。

- [ ] **Step 2: GREEN — 加字段 + MarshalJSON 分支**

  `OutputItem` 加 `Execution string `json:"execution,omitempty"` // tool_search_call`。
  `MarshalJSON` 在 `ItemTypeMcpCall` 分支后加 `ItemTypeToolSearchCall` 分支，marshal 固定字段集 `{type,id,call_id,arguments,execution,status}`（与 SDK `ResponseToolSearchCall` 对齐：id/arguments/call_id/execution/status 均 required）。

- [ ] **Step 3: 回归** `go test ./internal/model/`

---

## Task S2: callKind 骨架 + functionCallKind（验证流水线）

**Files:**
- New: `internal/streamconv/callkind.go`（接口 + callState + dispatchCallKind）
- New: `internal/streamconv/pipeline.go`（通用 handleCallStart/Delta/Stop）
- New: `internal/streamconv/callkind_function.go`（functionCallKind）
- Modify: `internal/streamconv/converter.go`（Converter 加 callByBlockIdx；handleBlockStart 的 tool_use 分支委托通用流水线）
- Test: `internal/streamconv/converter_function_test.go`（若已有 function_call 测试则复用 + 补 pin）

**Goal:** 建立 callKind 框架，把 `tool_use` + 非 custom + 非 tool_search 的 call（即 function_call）迁移到通用流水线，证明流水线正确。其余 call（custom/tool_search/server/mcp）**此 task 暂不动**——`handleToolUseStart` 仍处理 custom，`handleServerToolUseStart`/`handleMcpToolUseStart` 不动。

- [ ] **Step 1: RED — function_call 回归 pin**

  在旧代码上写/补测试：`tool_use(name="compute", id="call_1")` + 一段 `input_json_delta` + `content_block_stop`，断言产出事件序列**逐项**等于：`output_item.added(item=function_call, id=fc_N, call_id=call_1, name=compute)` → `function_call.arguments.delta`(每段) → `function_call.arguments.done`(完整 args) → `output_item.done`。先在旧 handler 上跑 GREEN（钉死现状）。

- [ ] **Step 2: 定义 callKind 接口与 callState**

  `callkind.go`：
  ```go
  type callKind interface {
      itemType() string
      idPrefix() string
      tracksToolUseID() bool                                   // hosted call 关联 result
      buildItem(itemIdx int, itemID string, ev *anthropic.MessageStreamEventUnion) model.OutputItem
      startEvents(c *Converter, itemIdx int, itemID string) []model.SSEEvent
      consumeDelta(c *Converter, st *callState, partial string) []model.SSEEvent
      finish(c *Converter, st *callState, args string) (model.OutputItem, []model.SSEEvent)
      handleResult(c *Converter, ev *anthropic.MessageStreamEventUnion, itemIdx int) []model.SSEEvent // client call 返回 nil
  }
  type callState struct {
      kind callKind; itemIdx int; itemID, callID, name string; argBuilder *strings.Builder
  }
  func dispatchCallKind(c *Converter, ev *anthropic.MessageStreamEventUnion) callKind { ... }
  ```
  `dispatchCallKind`：`tool_use` → customToolNames 命中→`customCallKind`（T3 前暂返 nil 让旧 handler 兜底，或直接返 customCallKind 占位）；name=="tool_search"→`toolSearchCallKind`（T4 前 panic/未实现）；default→`functionCallKind`。**为避免破坏 custom/tool_search，此步 dispatch 对 tool_use 只对「非 custom 且非 tool_search」返 functionCallKind，其余返 nil**（nil 时 handleBlockStart 回退旧 handler）。

- [ ] **Step 3: 通用流水线 pipeline.go**

  `Converter` 加 `callByBlockIdx map[int]*callState`（`New()` 初始化）。
  - `handleCallStart(c, ev)`: kind=dispatch；若 nil 返回 nil（回退）；allocate itemIdx/itemID(fmt `%s_%d`)；item=kind.buildItem；append outputItems；`callByBlockIdx[idx]=st`；若 tracksToolUseID 注册 byToolUseID；emit `output_item.added` + `kind.startEvents`。
  - `handleCallDelta(c, ev)`: lookup callByBlockIdx；非 input_json_delta 返回 nil；builder+=partial；`kind.consumeDelta`。
  - `handleCallStop(c, ev)`: lookup；args=builder；item,evts=kind.finish；更新 outputItems[itemIdx]；emit evts + `output_item.done`；delete callByBlockIdx。
  `handleBlockStart` tool_use 分支：先 `dispatchCallKind`，非 nil → `handleCallStart`；nil → 旧 `handleToolUseStart`（custom 暂走旧）。

- [ ] **Step 4: functionCallKind**

  `callkind_function.go`：itemType=`function_call`，idPrefix=`fc`，tracksToolUseID=false（function 无 result block），buildItem 取 `ev.ContentBlock.{ID,Name}`，startEvents=nil，consumeDelta 产 `function_call.arguments.delta`，finish 设 Arguments+Status=completed 产 `function_call.arguments.done`。

- [ ] **Step 5: GREEN** function_call pin 测试过；custom/tool_search/server/mcp 测试不受影响（仍走旧 handler）。

---

## Task S3: customCallKind 迁移

**Files:**
- New: `internal/streamconv/callkind_custom.go`
- Modify: `callkind.go`（dispatch tool_use 的 custom 分支返 customCallKind）、`converter.go`（tool_use 不再回退旧 handler；移除 tool_use 旧路径中对 custom 的处理，但保留 `handleToolUseStart` 函数直到 T8——改为只剩 custom 逻辑或直接删并迁）

- [ ] **Step 1: RED — custom_tool_call 回归 pin**（旧代码钉死：`tool_use(name in customToolNames)` → `custom_tool_call` item + `custom_tool_call.input.delta/done`，input=`{"input":<args>}` 经 `customToolInput` 解析）

- [ ] **Step 2: customCallKind**：itemType=`custom_tool_call`，idPrefix=`ctc`，tracksToolUseID=false，buildItem 同 function，consumeDelta **不产外部 delta**（custom delta 在 stop 时一次性给，与现 `handleBlockStop` custom 分支一致），finish 用 `customToolInput(args)` 设 Input、产 `custom_tool_call.input.delta`+`input.done`。

- [ ] **Step 3: dispatch 启用** + 删 `handleToolUseStart`（tool_use 全部走通用流水线）。GREEN：custom + function pin 都过。

---

## Task S4: toolSearchCallKind —— 修复 bug（验收标志）

**Files:**
- New: `internal/streamconv/callkind_toolsearch.go`
- Modify: `callkind.go`（dispatch tool_use name=="tool_search" → toolSearchCallKind）

- [ ] **Step 1: RED — 证明 bug**

  测试：`tool_use(name="tool_search", id="call_ts")` + input_json_delta(`{"query":"fetch"}`) + stop。断言产出 `output_item.added` 的 `item.type == "tool_search_call"`（**当前是 `function_call`，RED**）、`item.execution=="client"`、`item.call_id=="call_ts"`、`item.arguments` 含 query；**不**产 `function_call.arguments.*` 事件（tool_search 无专门 delta/done 事件，args 仅随 output_item.done 给）。

- [ ] **Step 2: toolSearchCallKind**：itemType=`tool_search_call`，idPrefix=`tsc`，tracksToolUseID=false，buildItem 取 ID（call_id）+ Execution="client"（初始 status=in_progress），startEvents=nil，consumeDelta=nil（不流式），finish 设 Arguments（累积 args）+Status=completed，**不产** arguments.delta/done（只靠 output_item.added/done 携带 item）。

- [ ] **Step 3: GREEN + 验收** tool_search 产 tool_search_call；用真实 Codex 跑一轮确认 TUI 不再 abort、deepwiki 工具能经 tool_search 暴露后被调用。

---

## Task S5: webSearchCallKind + codeInterpreterCallKind（server_tool_use 类）

**Files:**
- New: `callkind_websearch.go`、`callkind_codeinterpreter.go`
- Modify: `converter.go`（handleBlockStart 的 server_tool_use 分支委托通用流水线；方言回退逻辑——`declaredServerTools` 唯一时忽略 name——迁入 dispatchCallKind）

- [ ] **Step 1: RED — web_search_call / code_interpreter_call 回归 pin**（含 GLM `web_search_prime` 方言回退、`tool_result` 形态回传 web search 结果的兼容路径）

- [ ] **Step 2: webSearchCallKind**：itemType=`web_search_call`，idPrefix=`ws`，tracksToolUseID=true（关联 web_search_tool_result），buildItem 建 Action.Query（`extractWebSearchQuery`），startEvents 产 `in_progress`+`searching`。codeInterpreterCallKind：itemPrefix=`ci`，buildItem 建 Code/ContainerID，startEvents 产 `in_progress`+`interpreting`+`code.delta/done`（若有 code）。

- [ ] **Step 3: dispatch server_tool_use**：复用 `toolcatalog.ServerToolByAnthropicName` + `declaredServerTools` 回退；web_fetch 等无 Responses 等价的返 nil（走 skip）。删 `handleServerToolUseStart`/`dispatchServerToolUse`。GREEN。

---

## Task S6: mcpCallKind（mcp_tool_use）

**Files:**
- New: `callkind_mcp.go`
- Modify: `converter.go`（handleBlockStart 的 mcp_tool_use 分支委托通用流水线）

- [ ] **Step 1: RED — mcp_call 回归 pin**（probe 合成的 mcp_tool_use → mcp_call item + in_progress + arguments.delta/done；ServerLabel/Name/Arguments 从合成 Input map 解码）

- [ ] **Step 2: mcpCallKind**：itemType=`mcp_call`，idPrefix=`mcp_`（注意现 id 是 `mcp_%d`，保持），tracksToolUseID=true，buildItem 从 `decodeMcpUseInput(ev.ContentBlock.Input)` 取 serverLabel/name/args，startEvents 产 `in_progress`，consumeDelta 产 `mcp_call_arguments.delta`，finish 产 `mcp_call_arguments.done`。删 `handleMcpToolUseStart`/`decodeMcpUseInput`（迁入 kind 或 callkind_mcp.go）。

---

## Task S7: result 通用化

**Files:**
- New: `pipeline.go` 加 `handleCallResult`
- Modify: 各 `callkind_*.go`（实现 `handleResult`）、`converter.go`（handleBlockStart 的 `tool_result`/`web_search_tool_result`/`code_execution_tool_result`/`mcp_tool_result` 分支委托 `handleCallResult`）

- [ ] **Step 1: RED — 各 result 回归 pin**（web_search_tool_result→completed+sources；GLM tool_result 方言→同；code_execution_tool_result→outputs+completed；mcp_tool_result→output+completed/failed）

- [ ] **Step 2: handleCallResult**：按 `ev.ContentBlock.ToolUseID` 在对应 byToolUseID map（webSearch/codeExecution/mcp，T5/T6 已统一进 callState 关联）查 callState → `kind.handleResult`。client call（function/custom/tool_search）handleResult 返回 nil。webSearch 的 GLM `tool_result` 方言：handleBlockStart 的 tool_result 分支先查 webSearch 关联，命中→handleCallResult（webSearch kind）。

- [ ] **Step 3: 删** `handleWebSearchResultStart`/`handleCodeExecutionResultStart`/`handleMcpToolResultStart`/`extractWebSearchSources`/`decodeMcpResultInput`/`foldExecutionLogs`（迁入对应 kind）。GREEN。

---

## Task S8: 清理 + 文档 + final review

- [ ] **Step 1: 删旧 handler 残留** `handleToolUseStart`（若 T3 未删净）、`toolCallState` 旧定义（被 callState 取代）、旧 `toolCalls`/`toolArgBuilders` map（被 callByBlockIdx 取代）。确认 `handleBlockStart`/`handleBlockDelta`/`handleBlockStop` 全部委托通用流水线，无 call 专属分支。

- [ ] **Step 2: protocol-coverage 更新** `docs/protocol-coverage.md`：tool `tool_search` 由 `unsupported_by_backend` → `supported`（回程 tool_search_call 已通；登记：execution 固定 client、无专门 delta 事件、arguments 随 done 给）。

- [ ] **Step 3: 全量回归** `task check` + `task test-race`；`grep -n 'handleToolUseStart\|handleServerToolUseStart\|handleMcpToolUseStart' internal/streamconv/` 应空。

- [ ] **Step 4: whole-branch final review**（参照 task-final-review-report.md 范式）→ 合并 main。

---

## 风险与回退

- **回归风险**：五类现有 call 的事件序列若因重构微变，Codex 侧可能行为漂移。缓解：每 task 先 pin 旧行为再迁移；T8 前新旧可并存。
- **tool_search execution 取值**：定为 `"client"`（网关把 tool_search 降级成普通 tool，GLM 调用后由 Codex 本地执行搜索）。若 Codex 实际期望 `"server"`，T4 Step 3 真机验收时会暴露，届时调整。
- **dispatch 对未知 server_tool_use 的 skip**：保持现有 `handleSkippedServerToolUseStart` 语义（标记 skippedBlocks），不回归。
