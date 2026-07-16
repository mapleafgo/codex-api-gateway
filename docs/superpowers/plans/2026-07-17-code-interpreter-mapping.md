# Code Interpreter → Code Execution 映射（批次 A）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 OpenAI `code_interpreter` tool 双向映射到 Anthropic 标准 `code_execution_20250522`（请求声明 + 回程流式 + 多轮回灌），状态 `deferred` → `lossy_supported`。

**Architecture:** 复用批次 0 的 catalog（`internal/toolcatalog`）：`code_interpreter` 注册为 `serverTool`，请求声明经 `catalog.Declare` 自动产出 `OfCodeExecutionTool20250522`，回程 `server_tool_use(code_execution)` + `code_execution_tool_result` 经 `catalog.ServerToolByAnthropicName` 驱动 streamconv handler。两端结构与 web_search 同构（`server_tool_use` + 结果块，靠 `tool_use_id` 关联）。model 层新增 `code_interpreter_call` item 类型与 5 个事件。

**Tech Stack:** Go 1.26.5、anthropic-sdk-go `v1.57.0`（`OfCodeExecutionTool20250522` 为**标准非 beta** tool）、openai-go/v3 `v3.42.0`。

## Global Constraints

- **依赖批次 0**：本批假设批次 0 已合并——`toolcatalog` 包存在，convert/streamconv 的 dispatch 已走 catalog。若批次 0 未合并，先合并批次 0。
- **纯 convert + streamconv，不动 client**：code execution 是标准 tool，无需 beta header、无需 JSON 注入。`internal/anthropic/client.go` 本批不改。
- **TDD（新能力）**：每个映射先写 RED 测试证明当前 `deferred`（fail-fast 或 skip），再实现，再 GREEN。
- **已知损失必须登记**：`container`（file_ids / memory_limit / 显式 container）、代码生成文件（`file_id`→`url`）不可转换——在 README「已知限制」与 `docs/protocol-coverage.md` 显式登记，丢弃路径输出 WARN（AGENTS.md 约定）。
- **input.code 字段名风险点**：Anthropic `server_tool_use(code_execution)` 的 `input` SDK 类型为 `any`，本计划假设 `{code}`，以 RED 测试锁定确切字段名；若官方文档/wire 不同则据实修正（spec 第 113 行）。
- **测试命令**：单包 `go test ./internal/<pkg>/`；全量 `task check`；流式状态机 `task test-race`。
- **提交风格**：Conventional Commits，可中文，如 `feat(streamconv): code_execution 回程映射为 code_interpreter_call`。

---

## 文件结构

| 文件 | 改动 |
|---|---|
| `internal/model/constants.go` | 加 `ItemTypeCodeInterpreterCall` 常量 |
| `internal/model/outputitem.go` | `OutputItem` 加 `ContainerID`/`Code`/`Outputs` 字段；新增 `CodeInterpreterOutput` 类型；`MarshalJSON` 加 `code_interpreter_call` 分支（保证 `outputs` required） |
| `internal/model/event.go` | 加 `CodeInterpreterCallEvent` / `CodeInterpreterCallCodeDeltaEvent` / `CodeInterpreterCallCodeDoneEvent` 事件 struct |
| `internal/toolcatalog/inspect.go` | `Inspect` 加 `OfCodeInterpreter` case |
| `internal/toolcatalog/declare.go` | `Declare` 加 `OfCodeInterpreter` → `OfCodeExecutionTool20250522` |
| `internal/toolcatalog/server.go` | `serverToolByAnthropicName` 加 `"code_execution"`；`ApplyCacheControl` 加 `OfCodeExecutionTool20250522` case |
| `internal/convert/request.go` | `appendItem` 加 `OfCodeInterpreterCall` 回灌分支（`server_tool_use` + `code_execution_tool_result`） |
| `internal/streamconv/converter.go` | 事件常量；`Converter` 加 `codeExecutionByToolUseID`；`handleServerToolUseStart` 按 catalog identity 分发；新增 code_execution handler；`handleBlockStart` 的 `code_execution_tool_result` 从 skip 改 handler |

依赖方向不变（批次 0 已建立）。

---

## Task A1: model 层 code_interpreter_call 类型与事件

**Files:**
- Modify: `internal/model/constants.go`
- Modify: `internal/model/outputitem.go`
- Modify: `internal/model/event.go`
- Test: `internal/model/outputitem_test.go`（新增/扩充）

**Interfaces:**
- Consumes: `oaconstant.CodeInterpreterCall` / `ResponseCodeInterpreterCall*`
- Produces: `ItemTypeCodeInterpreterCall`、`CodeInterpreterOutput`、`OutputItem.ContainerID/Code/Outputs`、3 个事件 struct

- [ ] **Step 1: 加常量（RED 准备）**

`internal/model/constants.go` 的 `ItemTypeWebSearchCall` 行（39）下加：

```go
	ItemTypeCodeInterpreterCall = string(oaconstant.ValueOf[oaconstant.CodeInterpreterCall]())
```

- [ ] **Step 2: 写 model 测试（RED）**

`internal/model/outputitem_test.go` 加（若文件不存在则新建 `package model` 测试）：

```go
package model

import (
	"encoding/json"
	"testing"
)

func TestCodeInterpreterCallItemMarshalsRequiredOutputs(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeCodeInterpreterCall, ID: "ci_0", Status: ResponseStatusInProgress,
		ContainerID: "ci_container_0", Code: "print(1)",
		Outputs: []CodeInterpreterOutput{},
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for _, key := range []string{"type", "id", "status", "container_id", "code", "outputs"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("code_interpreter_call wire missing required key %q: %s", key, raw)
		}
	}
	if got["type"] != "code_interpreter_call" {
		t.Fatalf("bad type: %v", got["type"])
	}
}

func TestCodeInterpreterCallItemCarriesLogsOutput(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeCodeInterpreterCall, ID: "ci_0", Status: ResponseStatusCompleted,
		ContainerID: "ci_container_0", Code: "print(1)",
		Outputs: []CodeInterpreterOutput{{Type: "logs", Logs: "1\n"}},
	}
	raw, _ := json.Marshal(item)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	outputs := got["outputs"].([]any)
	first := outputs[0].(map[string]any)
	if first["type"] != "logs" || first["logs"] != "1\n" {
		t.Fatalf("bad logs output: %v", first)
	}
}
```

- [ ] **Step 3: 运行测试验证 RED**

Run: `go test ./internal/model/`
Expected: 编译失败（`CodeInterpreterOutput` 未定义；`ContainerID`/`Code`/`Outputs` 字段不存在）。

- [ ] **Step 4: 加 OutputItem 字段与 CodeInterpreterOutput 类型**

`internal/model/outputitem.go` 的 `OutputItem` struct（`Action` 字段后）加：

```go
	ContainerID string                  `json:"container_id,omitempty"` // code_interpreter_call
	Code        string                  `json:"code,omitempty"`         // code_interpreter_call
	Outputs     []CodeInterpreterOutput `json:"outputs,omitempty"`      // code_interpreter_call
```

在 `WebSearchSource` 类型后加：

```go
// CodeInterpreterOutput is one output of a code_interpreter_call (logs / image).
// 本批仅承载 logs；image（file_id→url）不可转换，丢弃 + WARN。
type CodeInterpreterOutput struct {
	Type string `json:"type"`           // "logs"
	Logs string `json:"logs,omitempty"`
}
```

- [ ] **Step 5: 加 MarshalJSON 的 code_interpreter_call 分支**

`internal/model/outputitem.go` 的 `MarshalJSON`（49）在 `if i.Type != ItemTypeMessage` 透传分支**之前**插入：

```go
	if i.Type == ItemTypeCodeInterpreterCall {
		return json.Marshal(struct {
			Type        string                  `json:"type"`
			ID          string                  `json:"id"`
			Status      string                  `json:"status"`
			ContainerID string                  `json:"container_id"`
			Code        string                  `json:"code"`
			Outputs     []CodeInterpreterOutput `json:"outputs"`
		}{
			Type: i.Type, ID: i.ID, Status: i.Status,
			ContainerID: i.ContainerID, Code: i.Code,
			Outputs: i.Outputs,
		})
	}
```

> **为何单独分支**：OpenAI wire 把 `outputs` 标记 `api:"required"`，即便空也须写出（`[]`）。透传分支用 `omitempty` 会丢空 outputs，破坏协议。message item 同理已有独立分支。

- [ ] **Step 6: 加事件 struct**

`internal/model/event.go`（`WebSearchCallEvent` 后）加：

```go
// CodeInterpreterCallEvent 用于 code_interpreter_call 的 in_progress / interpreting / completed 事件。
type CodeInterpreterCallEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
}

// CodeInterpreterCallCodeDeltaEvent 用于 response.code_interpreter_call_code.delta。
type CodeInterpreterCallCodeDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

// CodeInterpreterCallCodeDoneEvent 用于 response.code_interpreter_call_code.done。
type CodeInterpreterCallCodeDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Code           string `json:"code"`
}
```

- [ ] **Step 7: 运行测试验证 GREEN**

Run: `go test ./internal/model/`
Expected: PASS。

- [ ] **Step 8: Commit**

```bash
git add internal/model/constants.go internal/model/outputitem.go internal/model/event.go internal/model/outputitem_test.go
git commit -m "feat(model): 新增 code_interpreter_call item 类型与事件"
```

---

## Task A2: catalog 注册 code_interpreter（请求声明 + server tool + cache）

**Files:**
- Modify: `internal/toolcatalog/inspect.go`
- Modify: `internal/toolcatalog/declare.go`
- Modify: `internal/toolcatalog/server.go`
- Test: `internal/toolcatalog/declare_test.go` / `server_test.go`（扩充）

**Interfaces:**
- Consumes: 批次 0 的 `Inspect` / `Declare` / `ServerToolByAnthropicName` / `ApplyCacheControl`
- Produces: `OfCodeInterpreter` 在 `Inspect`/`Declare` 有 case；`"code_execution"` 在 server 注册表；`ApplyCacheControl` 覆盖 `OfCodeExecutionTool20250522`

- [ ] **Step 1: 写 RED 测试**

`internal/toolcatalog/declare_test.go` 加：

```go
func TestDeclareCodeInterpreterMapsToCodeExecution(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfCodeInterpreter: &oairesponses.ToolCodeInterpreterParam{}})
	if err != nil {
		t.Fatalf("code_interpreter must not fail fast: %v", err)
	}
	if decls[0].OfCodeExecutionTool20250522 == nil {
		t.Fatalf("code_interpreter not mapped to code_execution: %+v", decls)
	}
}
```

`internal/toolcatalog/inspect_test.go` 的 `TestInspectClientTools` table 加一行：

```go
		{"code_interpreter", oairesponses.ToolUnionParam{OfCodeInterpreter: &oairesponses.ToolCodeInterpreterParam{}}, Identity{OpenAIType: "code_interpreter", Name: "code_interpreter"}},
```

`internal/toolcatalog/server_test.go` 的 `TestServerToolByAnthropicName` 断言更新（原断言 `code_execution` 未注册，现改为已注册）：

```go
	id, ok := ServerToolByAnthropicName("code_execution")
	if !ok || id.OpenAIType != "code_interpreter" {
		t.Fatalf("code_execution must be registered: %+v ok=%v", id, ok)
	}
```

并在 `TestApplyCacheControlRecognizedVariants` 加：

```go
	ce := anthropic.ToolUnionParam{OfCodeExecutionTool20250522: &anthropic.CodeExecutionTool20250522Param{}}
	if !ApplyCacheControl(&ce, cc) {
		t.Fatalf("OfCodeExecutionTool20250522 cache_control not applied")
	}
```

- [ ] **Step 2: 运行测试验证 RED**

Run: `go test ./internal/toolcatalog/`
Expected: FAIL（`OfCodeInterpreter` 落入 default 报错 / `code_execution` 未注册 / cache 变体未覆盖）。

- [ ] **Step 3: inspect.go 加 case**

`internal/toolcatalog/inspect.go` 的 `Inspect` switch（`OfWebSearch` case 前）加：

```go
	case t.OfCodeInterpreter != nil:
		return []Identity{{OpenAIType: "code_interpreter", Name: "code_interpreter"}}, nil
```

- [ ] **Step 4: declare.go 加 case**

`internal/toolcatalog/declare.go` 的 `Declare` switch（`OfWebSearch` case 前）加：

```go
	case t.OfCodeInterpreter != nil:
		// container（file_ids / memory_limit / 显式 cntr_xxx）无 Anthropic 等价，丢弃。
		// Anthropic code execution 无状态单次执行、无 container 概念（已知损失）。
		// Name 由 SDK default 为 code_execution，无需显式设。
		return []anthropic.ToolUnionParam{{OfCodeExecutionTool20250522: &anthropic.CodeExecutionTool20250522Param{}}}, nil
```

- [ ] **Step 5: server.go 加注册与 cache 变体**

`internal/toolcatalog/server.go` 的 `serverToolByAnthropicName` map 加：

```go
	"code_execution": {OpenAIType: "code_interpreter", Name: "code_interpreter"},
```

`ApplyCacheControl` switch（`OfWebSearchTool20250305` case 后）加：

```go
	case tool.OfCodeExecutionTool20250522 != nil:
		tool.OfCodeExecutionTool20250522.CacheControl = cc
```

- [ ] **Step 6: 运行 catalog 测试验证 GREEN**

Run: `go test ./internal/toolcatalog/`
Expected: PASS。

- [ ] **Step 7: 验证 convert 请求声明自动生效（批次 0 已迁移 dispatch）**

Run: `go test ./internal/convert/ -run TestUnsupportedToolDefinitionReturnsError -v`
Expected: PASS（code_interpreter 不再 fail-fast；既有 unsupported 用例仍 fail-fast）。

> **请求声明无需改 convert**：批次 0 已把 `appendToolList` 迁移到 `catalog.Declare`，本批 catalog 注册后 code_interpreter 自动映射。但需确认 `TestUnsupportedToolDefinitionReturnsError`（若它用 code_interpreter 作为 unsupported 样本）已改用真正不支持的 tool（如 `file_search`）——若该测试恰好断言 code_interpreter 报错，需把样例换成 `file_search`。Step 7 即暴露此情况。

- [ ] **Step 8: Commit**

```bash
git add internal/toolcatalog/inspect.go internal/toolcatalog/declare.go internal/toolcatalog/server.go internal/toolcatalog/declare_test.go internal/toolcatalog/inspect_test.go internal/toolcatalog/server_test.go
git commit -m "feat(toolcatalog): 注册 code_interpreter 为 code_execution server tool"
```

---

## Task A3: convert 多轮 input 回灌（code_interpreter_call）

**Files:**
- Modify: `internal/convert/request.go`（`appendItem` 195-、新增 `appendCodeInterpreterCall`）
- Test: `internal/convert/request_test.go`（新增）

**Interfaces:**
- Consumes: `oairesponses.ResponseInputItemUnionParam.OfCodeInterpreterCall`（`*ResponseCodeInterpreterToolCallParam`，字段 ID/Code/ContainerID/Outputs/Status）；`anthropic.NewServerToolUseBlock` / `NewCodeExecutionToolResultBlock`
- Produces: 历史多轮上下文回放 `server_tool_use(code_execution)` + `code_execution_tool_result`

- [ ] **Step 1: 写 RED 测试**

`internal/convert/request_test.go` 加：

```go
func TestCodeInterpreterCallInputReplaysAsServerToolUseAndResult(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},
		{"type":"code_interpreter_call","id":"ci_1","status":"completed","container_id":"cntr_x","code":"print(2)","outputs":[{"type":"logs","logs":"2\n"}]}
	],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("replay must not fail: %v", err)
	}
	raw, _ := json.Marshal(out.Messages)
	if !strings.Contains(string(raw), `"code_execution"`) {
		t.Fatalf("server_tool_use(code_execution) not replayed: %s", raw)
	}
	if !strings.Contains(string(raw), `"code_execution_result"`) {
		t.Fatalf("code_execution_tool_result not replayed: %s", raw)
	}
	if strings.Contains(string(raw), `"cntr_x"`) {
		t.Fatalf("container_id must be dropped on replay: %s", raw)
	}
}
```

- [ ] **Step 2: 运行测试验证 RED**

Run: `go test ./internal/convert/ -run TestCodeInterpreterCallInputReplaysAsServerToolUseAndResult -v`
Expected: FAIL（`code_interpreter_call` input item 当前无 case，落入 unknown 保留为 system context，不产 `code_execution` block）。

- [ ] **Step 3: appendItem 加分发**

`internal/convert/request.go` 的 `appendItem`（195-，在 `if item.OfFunctionCall != nil` 等同级分支处）加：

```go
	if item.OfCodeInterpreterCall != nil {
		return appendCodeInterpreterCall(out, item.OfCodeInterpreterCall)
	}
```

- [ ] **Step 4: 实现 appendCodeInterpreterCall**

`internal/convert/request.go`（`appendToolSearchCall` 附近）加：

```go
// appendCodeInterpreterCall 把历史 code_interpreter_call input item 回放为 Anthropic
// 历史 content block：server_tool_use(code_execution, input={code}) + code_execution_tool_result。
// container_id 丢弃（Anthropic code execution 无 container 概念）。
func appendCodeInterpreterCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseCodeInterpreterToolCallParam) error {
	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleAssistant {
		out.Messages = append(out.Messages, anthropic.NewAssistantMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, anthropic.NewServerToolUseBlock(
		call.ID, map[string]any{"code": call.Code}, anthropic.ServerToolUseBlockParamNameCodeExecution,
	))

	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleUser {
		out.Messages = append(out.Messages, anthropic.NewUserMessage())
	}
	last = &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, anthropic.NewCodeExecutionToolResultBlock(
		anthropic.CodeExecutionResultBlockParam{Stdout: codeInterpreterLogs(call.Outputs)},
		call.ID,
	))
	return nil
}

// codeInterpreterLogs 把 code_interpreter_call 的 logs outputs 拼成单段 stdout 文本。
func codeInterpreterLogs(outputs []oairesponses.ResponseCodeInterpreterToolCallOutputUnionParam) string {
	var parts []string
	for _, o := range outputs {
		if o.OfLogs != nil && o.OfLogs.Logs != "" {
			parts = append(parts, o.OfLogs.Logs)
		}
	}
	return strings.Join(parts, "\n")
}
```

> **字段名以 SDK 为准**：`ServerToolUseBlockParamNameCodeExecution` 是 `NewServerToolUseBlock` 第三参的类型（`ServerToolUseBlockParamName`）；`ResponseCodeInterpreterToolCallOutputUnionParam.OfLogs.Logs` 是 logs 输出路径。若 SDK 字段名不同（RED/go doc 暴露），据实修正。image 输出（`OfImage`）不可转换，本函数直接忽略（丢弃，非执行前置，不发 WARN——回灌静默）。

- [ ] **Step 5: 运行 convert 测试验证 GREEN**

Run: `go test ./internal/convert/ -run TestCodeInterpreterCallInputReplaysAsServerToolUseAndResult -v`
Expected: PASS。

Run: `go test ./internal/convert/`
Expected: 全绿（既有用例不回归）。

- [ ] **Step 6: Commit**

```bash
git add internal/convert/request.go internal/convert/request_test.go
git commit -m "feat(convert): code_interpreter_call 历史 input 回灌为 server_tool_use + result"
```

---

## Task A4: streamconv 回程 code_execution → code_interpreter_call 事件链

**Files:**
- Modify: `internal/streamconv/converter.go`（事件常量、`Converter` struct、`handleServerToolUseStart` 分发、新增 handler、`handleBlockStart` result 分支）
- Test: `internal/streamconv/converter_test.go`（新增）

**Interfaces:**
- Consumes: 批次 0 的 `catalog.ServerToolByAnthropicName`；Task A1 的 model 事件/item
- Produces: `code_interpreter_call` item + 5 个事件（in_progress/interpreting/code.delta/code.done/completed）

- [ ] **Step 1: 写 RED 测试**

`internal/streamconv/converter_test.go` 加：

```go
func TestCodeExecutionServerToolUseEmitsCodeInterpreterCall(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_start", Message: anthropic.Message{ID: "m", Model: "x"}})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:  "server_tool_use",
			ID:    "toolu_ci1",
			Name:  "code_execution",
			Input: map[string]any{"code": "print(3)"},
		},
	})
	added := eventData(t, eventByType(t, evs, "response.output_item.added"))
	item := added["item"].(map[string]any)
	if item["type"] != "code_interpreter_call" {
		t.Fatalf("expected code_interpreter_call, got %v", item["type"])
	}
	if item["code"] != "print(3)" {
		t.Fatalf("bad code: %v", item["code"])
	}
	if item["container_id"] == "" {
		t.Fatal("container_id must be synthesized")
	}
	eventByType(t, evs, "response.code_interpreter_call.in_progress")
	eventByType(t, evs, "response.code_interpreter_call.interpreting")
	eventByType(t, evs, "response.code_interpreter_call_code.delta")
	eventByType(t, evs, "response.code_interpreter_call_code.done")

	evs2, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type:  "content_block_start",
		Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type:      "code_execution_tool_result",
			ToolUseID: "toolu_ci1",
			// stdout 在 Content（CodeExecutionToolResultBlockContentUnion），非顶层。
			Content: anthropic.ContentBlockStartEventContentBlockUnionContent{Stdout: "3\n"},
		},
	})
	done := eventData(t, eventByType(t, evs2, "response.output_item.done"))
	doneItem := done["item"].(map[string]any)
	if doneItem["status"] != "completed" {
		t.Fatalf("expected completed, got %v", doneItem["status"])
	}
	outputs := doneItem["outputs"].([]any)
	if outputs[0].(map[string]any)["logs"] != "3\n" {
		t.Fatalf("bad logs output: %v", outputs[0])
	}
}

func TestCodeExecutionResultStderrFoldedIntoLogs(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_start", Message: anthropic.Message{ID: "m", Model: "x"}})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "server_tool_use", ID: "toolu_ci2", Name: "code_execution", Input: map[string]any{"code": "x"},
		},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "code_execution_tool_result", ToolUseID: "toolu_ci2",
			Content: anthropic.ContentBlockStartEventContentBlockUnionContent{Stdout: "out", Stderr: "err"},
		},
	})
	done := eventData(t, eventByType(t, evs, "response.output_item.done"))
	logs := done["item"].(map[string]any)["outputs"].([]any)[0].(map[string]any)["logs"]
	if !strings.Contains(logs.(string), "out") || !strings.Contains(logs.(string), "err") {
		t.Fatalf("stdout+stderr must fold into logs: %v", logs)
	}
}
```

> **input.code 字段名**：本测试用 `Input: map[string]any{"code": "print(3)"}`。若实际 wire 字段名不同（如 `"source"`），handler 的 `extractCodeExecutionCode` 与本测试同步修正——RED 即锁定。

- [ ] **Step 2: 运行测试验证 RED**

Run: `go test ./internal/streamconv/ -run TestCodeExecution -v`
Expected: FAIL（`code_execution` 当前经 `handleSkippedServerToolUseStart` 跳过，无 `code_interpreter_call` item；result block 走 `handleSkippedBlockStart`）。

- [ ] **Step 3: 加事件常量**

`internal/streamconv/converter.go` 的事件常量 var 块（`evWebSearchCallCompleted` 后）加：

```go
	evCodeInterpreterCallInProgress = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallInProgress]())
	evCodeInterpreterCallInterpreting = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallInterpreting]())
	evCodeInterpreterCallCodeDelta = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallCodeDelta]())
	evCodeInterpreterCallCodeDone = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallCodeDone]())
	evCodeInterpreterCallCompleted = string(oaconstant.ValueOf[oaconstant.ResponseCodeInterpreterCallCompleted]())
```

- [ ] **Step 4: Converter 加状态字段**

`internal/streamconv/converter.go` 的 `Converter` struct（`webSearchByToolUseID` 字段后）加：

```go
	// Code execution state: Anthropic tool_use id -> output item index.
	codeExecutionByToolUseID map[string]int
```

`New()` 的返回 struct（`webSearchByToolUseID: map[string]int{}` 后）加：

```go
		codeExecutionByToolUseID: map[string]int{},
```

- [ ] **Step 5: handleServerToolUseStart 按 identity 分发**

批次 0 把 `handleServerToolUseStart`（400）改为经 `catalog.ServerToolByAnthropicName` 判定。本批在判定通过后按 `OpenAIType` 分发。把该函数改为：

```go
func (c *Converter) handleServerToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	id, ok := toolcatalog.ServerToolByAnthropicName(ev.ContentBlock.Name)
	if !ok {
		return c.handleSkippedServerToolUseStart(ev)
	}
	switch id.OpenAIType {
	case "web_search":
		return c.handleWebSearchServerToolUseStart(ev)
	case "code_interpreter":
		return c.handleCodeExecutionServerToolUseStart(ev)
	}
	return c.handleSkippedServerToolUseStart(ev)
}
```

把原 `handleServerToolUseStart` 中 web_search 的事件链逻辑（原 407-432，`idx := c.itemOrder` 起到返回 `web_search_call` 事件）整体提取为独立函数：

```go
func (c *Converter) handleWebSearchServerToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	itemID := fmt.Sprintf("ws_%d", idx)
	item := model.OutputItem{
		Type:   model.ItemTypeWebSearchCall,
		ID:     itemID,
		Status: model.ResponseStatusInProgress,
		Action: &model.WebSearchAction{Type: "search", Query: extractWebSearchQuery(ev.ContentBlock.Input)},
	}
	c.outputItems = append(c.outputItems, item)
	c.webSearchByToolUseID[ev.ContentBlock.ID] = idx

	return []model.SSEEvent{
		model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
			Type: evOutputItemAdded, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, Item: item,
		}),
		model.MarshalEvent(evWebSearchCallInProgress, model.WebSearchCallEvent{
			Type: evWebSearchCallInProgress, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
		model.MarshalEvent(evWebSearchCallSearching, model.WebSearchCallEvent{
			Type: evWebSearchCallSearching, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
	}
}
```

> **行为不变保证**：web_search 事件链逐字搬迁，仅函数名变化；`TestWebSearchServerToolUseEmitsWebSearchCall` 等既有用例保持 GREEN。

- [ ] **Step 6: 新增 code_execution handler**

`internal/streamconv/converter.go` 加：

```go
// handleCodeExecutionServerToolUseStart 把 Anthropic server_tool_use(code_execution)
// 映射为 code_interpreter_call item + 事件链。
// container_id 由网关合成（Anthropic 无 container，已知损失）。
// input.code 假设为 {"code": "..."}，字段名以 RED/wire 锁定。
func (c *Converter) handleCodeExecutionServerToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	itemID := fmt.Sprintf("ci_%d", idx)
	code := extractCodeExecutionCode(ev.ContentBlock.Input)
	item := model.OutputItem{
		Type:        model.ItemTypeCodeInterpreterCall,
		ID:          itemID,
		Status:      model.ResponseStatusInProgress,
		ContainerID: fmt.Sprintf("ci_container_%d", idx),
		Code:        code,
		Outputs:     []model.CodeInterpreterOutput{},
	}
	c.outputItems = append(c.outputItems, item)
	c.codeExecutionByToolUseID[ev.ContentBlock.ID] = idx

	out := []model.SSEEvent{
		model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
			Type: evOutputItemAdded, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, Item: item,
		}),
		model.MarshalEvent(evCodeInterpreterCallInProgress, model.CodeInterpreterCallEvent{
			Type: evCodeInterpreterCallInProgress, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
		model.MarshalEvent(evCodeInterpreterCallInterpreting, model.CodeInterpreterCallEvent{
			Type: evCodeInterpreterCallInterpreting, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
	}
	if code != "" {
		out = append(out,
			model.MarshalEvent(evCodeInterpreterCallCodeDelta, model.CodeInterpreterCallCodeDeltaEvent{
				Type: evCodeInterpreterCallCodeDelta, SequenceNumber: c.nextSeq(),
				OutputIndex: idx, ItemID: itemID, Delta: code,
			}),
			model.MarshalEvent(evCodeInterpreterCallCodeDone, model.CodeInterpreterCallCodeDoneEvent{
				Type: evCodeInterpreterCallCodeDone, SequenceNumber: c.nextSeq(),
				OutputIndex: idx, ItemID: itemID, Code: code,
			}),
		)
	}
	return out
}

// extractCodeExecutionCode 从 server_tool_use(code_execution) 的 input 取出代码。
// input 是 free-form JSON，假设 {"code": "..."}（spec 第 113 行风险点，RED 锁定）。
func extractCodeExecutionCode(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	if c, ok := m["code"].(string); ok {
		return c
	}
	return ""
}

// handleCodeExecutionResultStart 把 code_execution_tool_result 映射为
// code_interpreter_call 的 outputs（stdout/stderr → logs）+ completed。
// file_id（代码生成的文件）无 url 凭据不可转换，丢弃 + WARN。
func (c *Converter) handleCodeExecutionResultStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx, ok := c.codeExecutionByToolUseID[ev.ContentBlock.ToolUseID]
	if !ok || idx >= len(c.outputItems) {
		return nil // 无关联的 code_execution server_tool_use，交由 handleBlockStart 兜底 skip
	}
	itemID := fmt.Sprintf("ci_%d", idx)
	// code_execution_tool_result 的 stdout/stderr 在 ev.ContentBlock.Content
	// （CodeExecutionToolResultBlockContentUnion），而非顶层或 AsCodeExecutionToolResult。
	// 生成的文件列表在其 Content.OfContent（[]CodeExecutionOutputBlock）。
	rc := ev.ContentBlock.Content
	logs := foldExecutionLogs(rc.Stdout, rc.Stderr)
	c.outputItems[idx].Status = model.ResponseStatusCompleted
	if logs != "" {
		c.outputItems[idx].Outputs = []model.CodeInterpreterOutput{{Type: "logs", Logs: logs}}
	}
	for _, out := range rc.Content.OfContent {
		if out.FileID != "" {
			slog.Warn("丢弃 code execution 生成的文件（无 OpenAI files url 凭据）",
				"response_id", c.respID, "tool_use_id", ev.ContentBlock.ToolUseID, "file_id", out.FileID)
		}
	}
	return []model.SSEEvent{
		model.MarshalEvent(evCodeInterpreterCallCompleted, model.CodeInterpreterCallEvent{
			Type: evCodeInterpreterCallCompleted, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, Item: c.outputItems[idx],
		}),
	}
}

// foldExecutionLogs 把 stdout 与非空 stderr 合并为 logs 文本（OpenAI logs 承载 stdout/stderr）。
func foldExecutionLogs(stdout, stderr string) string {
	var parts []string
	if stdout != "" {
		parts = append(parts, stdout)
	}
	if stderr != "" {
		parts = append(parts, stderr)
	}
	return strings.Join(parts, "\n")
}
```

- [ ] **Step 7: handleBlockStart 接入 result handler**

`internal/streamconv/converter.go` 的 `handleBlockStart`（255-287）当前把 `anBlockCodeExecutionToolResult, anBlockCodeExecutionToolResultError` 一并送 `handleSkippedBlockStart`（279-284）。改为：`code_execution_tool_result`（非 error）若关联已知 tool_use_id 则走 handler，否则 skip；error 变体仍 skip。把该 case 拆出：

```go
	case anBlockCodeExecutionToolResult:
		if _, ok := c.codeExecutionByToolUseID[ev.ContentBlock.ToolUseID]; ok {
			return c.handleCodeExecutionResultStart(ev)
		}
		return c.handleSkippedBlockStart(ev)
	case anBlockWebFetchToolResult,
		anBlockWebFetchToolResultError,
		anBlockWebSearchToolResultError,
		anBlockCodeExecutionToolResultError:
		return c.handleSkippedBlockStart(ev)
```

> **error 变体**：`code_execution_tool_result_error` 无成功结果，仍 skip + WARN（`handleSkippedBlockStart` 已 WARN）；关联的 `code_interpreter_call` 因永远收不到 completed 会保持 `in_progress`——这是已知损失（上游 code execution 失败时无法转 completed）。若需更稳健可在此把 item 标记 failed，但本批保持与 skip 一致的最小行为。

- [ ] **Step 8: 运行 streamconv 测试验证 GREEN**

Run: `go test ./internal/streamconv/ -run TestCodeExecution -v`
Expected: PASS。

Run: `go test ./internal/streamconv/`
Expected: 全绿（web_search / skip 既有用例不回归）。

- [ ] **Step 9: 流式状态机 race 检测**

Run: `task test-race`
Expected: 无 data race。

- [ ] **Step 10: Commit**

```bash
git add internal/streamconv/converter.go internal/streamconv/converter_test.go
git commit -m "feat(streamconv): code_execution 回程映射为 code_interpreter_call 事件链"
```

---

## Task A5: 全量验收 + 文档同步

**Files:**
- Modify: `docs/protocol-coverage.md`（状态升级）
- Modify: `README.md`（已知限制）

- [ ] **Step 1: 全量门禁**

Run: `task check`
Expected: fmt + go vet + 全量 test 全绿。

- [ ] **Step 2: 升级 protocol-coverage 状态**

按 spec「状态升级清单」（176-181 行）更新 `docs/protocol-coverage.md`：
- Tool `code_interpreter`：`deferred` → `lossy_supported`（→code_execution_20250522，container 丢失）
- Input/Output `code_interpreter_call`：`deferred` → `lossy_supported`
- Events `code_interpreter_call*`：`deferred` → `lossy_supported`
- Anthropic `code_execution_tool_result`：`deferred` → `supported`
- `bash_code_execution_tool_result` / `text_editor_code_execution_tool_result`：保持 `deferred`（spec 187 行）

逐行核对矩阵，更新状态列与说明列（注明 container/file_id 损失）。

- [ ] **Step 3: 更新 README 已知限制**

`README.md`「已知限制」加（spec 206 行）：

> - **code interpreter**：`container`（file_ids / memory_limit / 显式 container）、代码生成文件（`file_id`→`url`）不可转换；`code_execution_tool_result_error` 无法转 completed。

- [ ] **Step 4: Commit**

```bash
git add docs/protocol-coverage.md README.md
git commit -m "docs: code interpreter 升级为 lossy_supported，登记已知损失"
```

---

## Self-Review

**1. Spec 覆盖**（对照 spec 第 83-121 行）：

- **请求侧 1.1**（87-98）：catalog 注册 + Declare → Task A2；container 丢弃已注释；cache_control 随 `ApplyCacheControl` 扩展（Task A2 Step 5）。✓
- **回程流式 1.2**（102-113）：server_tool_use → 事件链（Task A4 Step 6）；code_execution_tool_result → outputs logs + completed（Task A4 Step 6/7）；container_id 合成（Task A4 Step 6）；stderr/return_code 并入 logs（Task A4 `foldExecutionLogs`）；file_id 丢弃 + WARN（Task A4 Step 6）；input.code 字段名 RED 锁定（Task A4 Step 1 注释）。✓
- **input 回灌 1.3**（116-117）：mapReplay → Task A3；container_id 丢弃（Task A3 RED 断言 + 实现）。✓
- **衍生 result 1.4**（119-121）：`bash`/`text_editor_code_execution_tool_result` 保持 `deferred`——`handleBlockStart` 的 error 变体与 bash/text_editor 变体仍走 skip（Task A4 未改这些 case；`anBlockCodeExecutionToolResultError` 等仍在 skip 列表）。✓
- **状态升级**（176-181）：Task A5。✓

**2. 占位符扫描**：无 TBD / TODO。SDK 字段名不确定处（`ServerToolUseBlockParamNameCodeExecution`、`ResponseCodeInterpreterToolCallOutputUnionParam.OfLogs`、input.code）均以「RED/go doc 锁定」明确处置路径，非遗留占位。✓

**3. 类型一致性**：
- `ItemTypeCodeInterpreterCall`（Task A1 定义）在 converter.go（Task A4）使用一致。
- `CodeInterpreterOutput{Type, Logs}`（Task A1）在 converter（Task A4 Outputs）与 model 测试（Task A1）一致。
- 3 个事件 struct（Task A1）在 converter 事件常量（Task A4 Step 3）与 handler（Task A4 Step 6）引用一致。
- `handleServerToolUseStart` 分发：web_search → `handleWebSearchServerToolUseStart`，code_interpreter → `handleCodeExecutionServerToolUseStart`，命名跨 step 一致。✓

**遗留风险（已标注）**：
- input.code 字段名（spec 113 行）——Task A4 RED 锁定。
- `code_execution_tool_result_error` 无法转 completed（Task A5 Step 3 README 登记）。
- 批次 0 必须先合并（Global Constraints）。

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-07-17-code-interpreter-mapping.md`.**

> 本批依赖批次 0 合并。三批依次执行顺序：批次 0 → 批次 A → 批次 B。执行选项（subagent-driven / inline）由批次 0 plan 的 handoff 统一确定，本批沿用同一方式。
