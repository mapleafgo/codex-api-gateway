# Responses 全协议覆盖第一批 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 按覆盖矩阵补齐第一批协议缺口：枚举翻译、shell/apply_patch 输入项、refusal 输出、allowed_tools、unsupported 诊断和未处理 Anthropic block 防静默丢弃。

**Architecture:** 以 `docs/protocol-coverage.md` 为权威入口，每个任务先写 RED 测试，再实现，再更新矩阵。允许重写历史转换结构和拆分文件；允许引入能显著降低复杂度或协议误差的第三方依赖，但必须说明用途、替代方案和验证方式。OpenAI/Anthropic 两侧协议来源仍以当前官方 SDK 为准。

**Tech Stack:** Go 1.26.5、github.com/openai/openai-go/v3@v3.42.0、github.com/anthropics/anthropic-sdk-go@v1.57.0、现有 Taskfile/go test 门禁；可新增成熟第三方依赖，需在任务内记录版本和用途。

## Global Constraints

- `docs/protocol-coverage.md` 是协议覆盖状态权威记录；每个协议项状态只能是 `supported`、`lossy_supported`、`raw_preserved`、`unsupported_by_backend`、`deferred`。
- Anthropic 没有等价能力的项不得假装支持；请求阶段或 stream 阶段必须明确失败、raw 保真或登记为 deferred。
- 新语义支持必须 TDD：先写失败测试，运行确认失败，再实现最小代码，再跑目标包和全仓测试。
- 允许重写 `internal/convert`、`internal/streamconv`、`internal/store`、`internal/model` 的历史结构；每次重写必须保持任务边界可独立测试。
- 允许引入第三方依赖；新依赖必须解决具体协议问题，并在提交说明或任务记录中写清用途、替代方案和验证命令。
- 出站 Responses 事件继续使用自定义 `omitempty` struct 和 SDK 常量，避免直接 marshal SDK response/union 类型造成零值污染。
- 当前工作树已有未提交实现改动；执行任务时只 stage 本任务文件，禁止回滚用户或既有改动。

---

## File Structure

- `internal/model/constants.go`: 增补协议枚举常量，例如 refusal content/event 相关常量。
- `internal/model/event.go`: 增补 refusal delta/done event struct，必要时扩展 `ContentPartOut`。
- `internal/model/outputitem.go`: 如需表示 refusal content，复用 `OutputText` 或新增最小字段，避免扩大模型面。
- `internal/streamconv/converter.go`: 重写 terminal/refusal 处理、unsupported Anthropic block 诊断、`handleComplete` 返回多事件。
- `internal/streamconv/converter_test.go`: 覆盖 stop reason、refusal、unsupported block。
- `internal/convert/request.go`: 增补 shell/local_shell/apply_patch item 转换、allowed_tools、unsupported tool_choice/tool 明确错误。
- `internal/convert/request_test.go`: 覆盖新增输入项、tool_choice 和 unsupported 错误。
- `internal/convert/customtool.go`: 如 shell/apply_patch 名称来源变化，更新 freeform tool name 发现逻辑。
- `internal/store/session.go`: 如新增 item 需要跨轮回放，存储/回放对应 context item；否则保持 raw preserved。
- `internal/store/session_test.go`: 覆盖新增 item 的回放或 raw 保真。
- `docs/protocol-coverage.md`: 每个任务完成后更新对应行状态。

---

### Task 1: 修正 stop_reason / incomplete_details 枚举并补 refusal 事件骨架

**Files:**
- Modify: `internal/model/constants.go`
- Modify: `internal/model/event.go`
- Modify: `internal/streamconv/converter.go`
- Test: `internal/streamconv/converter_test.go`
- Modify: `docs/protocol-coverage.md`

**Interfaces:**
- Consumes: `Converter.Feed(ev *anthropic.MessageStreamEventUnion) ([]model.SSEEvent, error)`
- Produces: `response.refusal.delta` / `response.refusal.done` events; `pause_turn` 不再写非法 `incomplete_details.reason`

- [ ] **Step 1: Write failing tests**

Add tests to `internal/streamconv/converter_test.go`:

```go
func TestPauseTurnDoesNotEmitInvalidIncompleteReason(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "message_start",
		Message: anthropic.Message{ID: "msg_pause", Model: "claude-test"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{StopReason: anthropic.StopReasonPauseTurn},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	last := evs[len(evs)-1]
	if last.Type != "response.incomplete" {
		t.Fatalf("expected response.incomplete, got %s", last.Type)
	}
	if strings.Contains(string(last.Data), `"reason":"pause_turn"`) {
		t.Fatalf("pause_turn is not an OpenAI incomplete_details.reason: %s", last.Data)
	}
}

func TestRefusalStopReasonEmitsRefusalPartAndContentFilter(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "message_start",
		Message: anthropic.Message{ID: "msg_refusal", Model: "claude-test"},
	})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "message_delta",
		Delta: anthropic.MessageStreamEventUnionDelta{
			StopReason: anthropic.StopReasonRefusal,
			StopDetails: anthropic.RefusalStopDetails{
				Category: anthropic.RefusalStopDetailsCategoryCyber,
				Explanation: "I can't help with that.",
			},
		},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})

	types := eventTypes(evs)
	for _, want := range []string{
		"response.output_item.added",
		"response.content_part.added",
		"response.refusal.delta",
		"response.refusal.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.incomplete",
	} {
		if !slices.Contains(types, want) {
			t.Fatalf("missing %s in %v", want, types)
		}
	}
	last := evs[len(evs)-1]
	if !strings.Contains(string(last.Data), `"reason":"content_filter"`) {
		t.Fatalf("refusal should map to OpenAI content_filter: %s", last.Data)
	}
}
```

If `eventTypes` helper does not exist in `internal/streamconv/converter_test.go`, add:

```go
func eventTypes(evs []model.SSEEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}
```

- [ ] **Step 2: Run RED**

Run:

```bash
go test ./internal/streamconv -run 'TestPauseTurnDoesNotEmitInvalidIncompleteReason|TestRefusalStopReasonEmitsRefusalPartAndContentFilter' -count=1
```

Expected: FAIL. Current behavior emits invalid `pause_turn` reason and does not emit refusal events.

- [ ] **Step 3: Implement minimal model/event additions**

In `internal/model/constants.go`, add:

```go
ContentTypeRefusal = string(oaconstant.ValueOf[oaconstant.Refusal]())
```

In `internal/model/event.go`, add:

```go
type RefusalDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

type RefusalDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Refusal        string `json:"refusal"`
}
```

- [ ] **Step 4: Implement converter refusal flow**

In `internal/streamconv/converter.go`:

1. Add event constants:

```go
evRefusalDelta = string(oaconstant.ValueOf[oaconstant.ResponseRefusalDelta]())
evRefusalDone  = string(oaconstant.ValueOf[oaconstant.ResponseRefusalDone]())
```

2. Add fields to `Converter`:

```go
refusalText string
```

3. Update `recordStopReason`:

```go
func (c *Converter) recordStopReason(ev *anthropic.MessageStreamEventUnion) {
	c.stopReason = string(ev.Delta.StopReason)
	if ev.Delta.StopReason == anthropic.StopReasonRefusal {
		c.refusalText = ev.Delta.StopDetails.Explanation
		if c.refusalText == "" {
			c.refusalText = string(ev.Delta.StopDetails.Category)
		}
	}
	if ev.Usage.OutputTokens > 0 || ev.Usage.InputTokens > 0 {
		c.usage = &model.ResponseUsage{
			InputTokens:  int(ev.Usage.InputTokens),
			OutputTokens: int(ev.Usage.OutputTokens),
		}
		c.usage.TotalTokens = c.usage.InputTokens + c.usage.OutputTokens
	}
}
```

4. Update `statusFor`:

```go
case anthropic.StopReasonPauseTurn:
	return model.ResponseStatusIncomplete, ""
case anthropic.StopReasonRefusal:
	return model.ResponseStatusIncomplete, model.IncompleteReasonContentFilter
```

5. Change `handleComplete` to return `[]model.SSEEvent`; before terminal event, append refusal item events when `c.stopReason == string(anthropic.StopReasonRefusal)` and `c.refusalText != ""`.

6. Update callers from:

```go
out = append(out, c.handleComplete())
```

to:

```go
out = append(out, c.handleComplete()...)
```

- [ ] **Step 5: Run GREEN**

Run:

```bash
go test ./internal/streamconv -run 'TestPauseTurnDoesNotEmitInvalidIncompleteReason|TestRefusalStopReasonEmitsRefusalPartAndContentFilter' -count=1
go test ./internal/streamconv -count=1
```

Expected: PASS.

- [ ] **Step 6: Update coverage matrix**

In `docs/protocol-coverage.md`, update:

- `response.refusal.delta` -> `supported`
- `response.refusal.done` -> `supported`
- `content part` `refusal` -> `supported`
- stop reason `refusal` -> `supported`
- stop reason `pause_turn` -> `lossy_supported` with note: incomplete without invalid reason

- [ ] **Step 7: Verify and commit**

Run:

```bash
go test ./...
git diff --check
git add internal/model/constants.go internal/model/event.go internal/streamconv/converter.go internal/streamconv/converter_test.go docs/protocol-coverage.md
git diff --cached --check
git commit -m "fix(streamconv): map refusal and pause_turn to valid Responses events"
```

---

### Task 2: 语义转换 shell/local_shell/apply_patch input items

**Files:**
- Modify: `internal/model/constants.go`
- Modify: `internal/convert/request.go`
- Modify: `internal/store/session.go`
- Test: `internal/convert/request_test.go`
- Test: `internal/store/session_test.go`
- Modify: `docs/protocol-coverage.md`

**Interfaces:**
- Consumes: OpenAI `OfShellCall`、`OfShellCallOutput`、`OfLocalShellCall`、`OfLocalShellCallOutput`、`OfApplyPatchCall`、`OfApplyPatchCallOutput`
- Produces: Anthropic `tool_use` / `tool_result` using tool names `shell` and `apply_patch`

- [ ] **Step 1: Write failing conversion tests**

Add tests to `internal/convert/request_test.go`:

```go
func TestShellCallInputItemConvertsToShellToolUse(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfShellCall: &oairesponses.ResponseInputItemShellCallParam{
					CallID: "call_shell",
					Action: oairesponses.ResponseInputItemShellCallActionParam{
						Commands: []string{"pwd", "go test ./..."},
					},
				},
			}},
		},
	}
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	toolUse := out.Messages[0].Content[0].OfToolUse
	if toolUse == nil || toolUse.Name != "shell" || toolUse.ID != "call_shell" {
		t.Fatalf("bad shell tool_use: %+v", out.Messages[0].Content[0])
	}
	if got := fmt.Sprint(toolUse.Input); !strings.Contains(got, "go test ./...") {
		t.Fatalf("shell input lost commands: %#v", toolUse.Input)
	}
}

func TestApplyPatchCallInputItemConvertsToApplyPatchToolUse(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{{
				OfApplyPatchCall: &oairesponses.ResponseInputItemApplyPatchCallParam{
					CallID: "call_patch",
					Status: "completed",
					Operation: oairesponses.ResponseInputItemApplyPatchCallOperationUnionParam{
						OfUpdateFile: &oairesponses.ResponseInputItemApplyPatchCallOperationUpdateFileParam{
							Path: "README.md",
							Diff: "*** Begin Patch\n*** Update File: README.md\n@@\n-old\n+new\n*** End Patch\n",
						},
					},
				},
			}},
		},
	}
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	toolUse := out.Messages[0].Content[0].OfToolUse
	if toolUse == nil || toolUse.Name != "apply_patch" || toolUse.ID != "call_patch" {
		t.Fatalf("bad apply_patch tool_use: %+v", out.Messages[0].Content[0])
	}
	if got := fmt.Sprint(toolUse.Input); !strings.Contains(got, "*** Begin Patch") {
		t.Fatalf("apply_patch input lost diff: %#v", toolUse.Input)
	}
}

func TestShellAndApplyPatchOutputsConvertToToolResults(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: []oairesponses.ResponseInputItemUnionParam{
				{OfShellCallOutput: &oairesponses.ResponseInputItemShellCallOutputParam{
					CallID: "call_shell",
					Output: []oairesponses.ResponseFunctionShellCallOutputContentParam{{
						Stdout: "ok",
					}},
				}},
				{OfApplyPatchCallOutput: &oairesponses.ResponseInputItemApplyPatchCallOutputParam{
					CallID: "call_patch",
					Status: "completed",
					Output: oparam.NewOpt("Done"),
				}},
			},
		},
	}
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Content[0].OfToolResult.ToolUseID != "call_shell" {
		t.Fatalf("shell output did not produce tool_result: %+v", out.Messages[0])
	}
	if out.Messages[0].Content[1].OfToolResult.ToolUseID != "call_patch" {
		t.Fatalf("apply_patch output did not produce tool_result: %+v", out.Messages[0])
	}
}
```

- [ ] **Step 2: Run RED**

Run:

```bash
go test ./internal/convert -run 'TestShellCallInputItemConvertsToShellToolUse|TestApplyPatchCallInputItemConvertsToApplyPatchToolUse|TestShellAndApplyPatchOutputsConvertToToolResults' -count=1
```

Expected: FAIL because the items currently fall through raw preservation or are not semantically converted.

- [ ] **Step 3: Implement conversion helpers**

In `internal/convert/request.go`, add item branches before unknown fallback:

```go
if item.OfLocalShellCall != nil {
	return appendLocalShellCall(out, item.OfLocalShellCall)
}
if item.OfLocalShellCallOutput != nil {
	return appendToolResult(out, item.OfLocalShellCallOutput.ID, item.OfLocalShellCallOutput.Output)
}
if item.OfShellCall != nil {
	return appendShellCall(out, item.OfShellCall)
}
if item.OfShellCallOutput != nil {
	return appendToolResult(out, item.OfShellCallOutput.CallID, shellOutputText(item.OfShellCallOutput.Output))
}
if item.OfApplyPatchCall != nil {
	return appendApplyPatchCall(out, item.OfApplyPatchCall)
}
if item.OfApplyPatchCallOutput != nil {
	output := ""
	if item.OfApplyPatchCallOutput.Output.Valid() {
		output = item.OfApplyPatchCallOutput.Output.Value
	}
	return appendToolResult(out, item.OfApplyPatchCallOutput.CallID, output)
}
```

Add helpers:

```go
func appendShellCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemShellCallParam) error {
	input := strings.Join(call.Action.Commands, "\n")
	return appendToolUse(out, call.CallID, "shell", map[string]any{"input": input})
}

func appendLocalShellCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemLocalShellCallParam) error {
	input := strings.Join(call.Action.Command, " ")
	return appendToolUse(out, call.CallID, "shell", map[string]any{"input": input})
}

func appendApplyPatchCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemApplyPatchCallParam) error {
	patch := ""
	if diff := call.Operation.GetDiff(); diff != nil {
		patch = *diff
	}
	return appendToolUse(out, call.CallID, "apply_patch", map[string]any{"input": patch})
}

func shellOutputText(parts []oairesponses.ResponseFunctionShellCallOutputContentParam) string {
	var out []string
	for _, part := range parts {
		if part.Stdout != "" {
			out = append(out, part.Stdout)
		}
		if part.Stderr != "" {
			out = append(out, part.Stderr)
		}
	}
	return strings.Join(out, "\n")
}
```

If SDK field names differ, use `go test` compiler errors and local SDK definitions to adjust; do not guess.

- [ ] **Step 4: Preserve new item types in session**

Add model constants for:

```go
ItemTypeLocalShellCall
ItemTypeLocalShellCallOutput
ItemTypeShellCall
ItemTypeShellCallOutput
ItemTypeApplyPatchCall
ItemTypeApplyPatchCallOutput
```

In `internal/store/session.go`, choose minimal safe behavior:

- Store and replay these items as raw JSON unless a compact typed representation is necessary.
- Add tests proving they do not disappear after `SaveContext` + `Enrich`.

Test snippet:

```go
func TestEnrichRoundTripsShellCallRaw(t *testing.T) {
	s := New(0, 0, time.Hour)
	input := []oairesponses.ResponseInputItemUnionParam{{
		OfShellCall: &oairesponses.ResponseInputItemShellCallParam{
			CallID: "call_shell",
			Action: oairesponses.ResponseInputItemShellCallActionParam{
				Commands: []string{"echo hi"},
			},
		},
	}}
	s.SaveContext("resp_1", "src", input, nil)

	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_1"),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfString: oparam.NewOpt("next"),
		},
	}
	s.Enrich(req, "src")
	if req.Input.OfInputItemList[0].OfShellCall == nil {
		t.Fatalf("shell_call was not replayed: %+v", req.Input.OfInputItemList[0])
	}
}
```

- [ ] **Step 5: Run GREEN**

Run:

```bash
go test ./internal/convert -run 'TestShellCallInputItemConvertsToShellToolUse|TestApplyPatchCallInputItemConvertsToApplyPatchToolUse|TestShellAndApplyPatchOutputsConvertToToolResults' -count=1
go test ./internal/store -run TestEnrichRoundTripsShellCallRaw -count=1
go test ./internal/convert ./internal/store -count=1
```

Expected: PASS.

- [ ] **Step 6: Update coverage matrix**

In `docs/protocol-coverage.md`, update these rows from `deferred` to `supported` or `lossy_supported`:

- `local_shell_call`
- `local_shell_call_output`
- `shell_call`
- `shell_call_output`
- `apply_patch_call`
- `apply_patch_call_output`

Use `lossy_supported` if command environment, timeout, caller, or structured operation metadata are flattened into freeform text.

- [ ] **Step 7: Verify and commit**

Run:

```bash
go test ./...
git diff --check
git add internal/model/constants.go internal/convert/request.go internal/convert/request_test.go internal/store/session.go internal/store/session_test.go docs/protocol-coverage.md
git diff --cached --check
git commit -m "feat(convert): map shell and apply_patch input items"
```

---

### Task 3: 实现 allowed_tools tool_choice 安全降级

**Files:**
- Modify: `internal/convert/request.go`
- Test: `internal/convert/request_test.go`
- Modify: `docs/protocol-coverage.md`

**Interfaces:**
- Consumes: `req.ToolChoice.OfAllowedTools`
- Produces: filtered `out.Tools` and Anthropic `tool_choice.auto` / `tool_choice.any`

- [ ] **Step 1: Write failing tests**

Add tests:

```go
func TestAllowedToolsFiltersAnthropicToolsAndUsesRequiredMode(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{
			{OfFunction: &oairesponses.FunctionToolParam{Name: "keep", Parameters: map[string]any{"type": "object"}}},
			{OfFunction: &oairesponses.FunctionToolParam{Name: "drop", Parameters: map[string]any{"type": "object"}}},
		},
		ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
			OfAllowedTools: &oairesponses.ToolChoiceAllowedParam{
				Mode: oairesponses.ToolChoiceAllowedModeRequired,
				Tools: []map[string]any{{"type": "function", "name": "keep"}},
			},
		},
	}
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].OfTool.Name != "keep" {
		t.Fatalf("allowed_tools did not filter tools: %+v", out.Tools)
	}
	if out.ToolChoice.OfAny == nil {
		t.Fatalf("required allowed_tools should map to Anthropic any: %+v", out.ToolChoice)
	}
}

func TestAllowedToolsErrorsWhenNoSupportedToolsRemain(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{
			{OfFunction: &oairesponses.FunctionToolParam{Name: "available", Parameters: map[string]any{"type": "object"}}},
		},
		ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
			OfAllowedTools: &oairesponses.ToolChoiceAllowedParam{
				Mode: oairesponses.ToolChoiceAllowedModeRequired,
				Tools: []map[string]any{{"type": "function", "name": "missing"}},
			},
		},
	}
	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("expected allowed_tools error, got %v", err)
	}
}
```

- [ ] **Step 2: Run RED**

Run:

```bash
go test ./internal/convert -run 'TestAllowedToolsFiltersAnthropicToolsAndUsesRequiredMode|TestAllowedToolsErrorsWhenNoSupportedToolsRemain' -count=1
```

Expected: FAIL. Current converter does not implement `OfAllowedTools`.

- [ ] **Step 3: Change conversion signatures to allow errors**

Change:

```go
func convertToolChoice(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams)
```

to:

```go
func convertToolChoice(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) error
```

Update caller in `ToAnthropic`:

```go
if err := convertToolChoice(out, req); err != nil {
	return nil, err
}
```

- [ ] **Step 4: Implement allowed tool filtering**

Add helpers:

```go
func applyAllowedTools(out *anthropic.MessageNewParams, allowed *oairesponses.ToolChoiceAllowedParam) error {
	allowedNames := map[string]bool{}
	for _, tool := range allowed.Tools {
		name, _ := tool["name"].(string)
		if name != "" {
			allowedNames[name] = true
		}
	}
	var filtered []anthropic.ToolUnionParam
	for _, tool := range out.Tools {
		if tool.OfTool != nil && allowedNames[tool.OfTool.Name] {
			filtered = append(filtered, tool)
		}
	}
	if len(filtered) == 0 {
		return fmt.Errorf("tool_choice allowed_tools has no supported tools")
	}
	out.Tools = filtered
	switch allowed.Mode {
	case oairesponses.ToolChoiceAllowedModeRequired:
		out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	default:
		out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	}
	return nil
}
```

In `convertToolChoice`, before function/custom choice:

```go
if tc.OfAllowedTools != nil {
	if err := applyAllowedTools(out, tc.OfAllowedTools); err != nil {
		return err
	}
	applyParallelToolChoice(out, req)
	return nil
}
```

- [ ] **Step 5: Run GREEN**

Run:

```bash
go test ./internal/convert -run 'TestAllowedToolsFiltersAnthropicToolsAndUsesRequiredMode|TestAllowedToolsErrorsWhenNoSupportedToolsRemain' -count=1
go test ./internal/convert -count=1
```

Expected: PASS.

- [ ] **Step 6: Update coverage matrix**

Set `Tool Choice Union` row `allowed_tools` to `lossy_supported`, with note: filters supported Anthropic tools by name; hosted/MCP allowed entries remain unsupported.

- [ ] **Step 7: Verify and commit**

Run:

```bash
go test ./...
git diff --check
git add internal/convert/request.go internal/convert/request_test.go docs/protocol-coverage.md
git diff --cached --check
git commit -m "feat(convert): support allowed_tools tool choice"
```

---

### Task 4: 对 unsupported hosted tools / MCP / built-in choices 返回明确错误

**Files:**
- Modify: `internal/convert/request.go`
- Test: `internal/convert/request_test.go`
- Modify: `docs/protocol-coverage.md`

**Interfaces:**
- Consumes: unsupported `ToolUnionParam` and unsupported `ResponseNewParamsToolChoiceUnion` variants
- Produces: explicit conversion errors instead of silent skipping

- [ ] **Step 1: Write failing tests**

Add tests:

```go
func TestUnsupportedHostedToolChoiceReturnsError(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		ToolChoice: oairesponses.ResponseNewParamsToolChoiceUnion{
			OfHostedTool: &oairesponses.ToolChoiceTypesParam{
				Type: oairesponses.ToolChoiceTypesTypeImageGeneration,
			},
		},
	}
	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported tool_choice") {
		t.Fatalf("expected unsupported tool_choice error, got %v", err)
	}
}

func TestUnsupportedToolDefinitionReturnsError(t *testing.T) {
	req := &oairesponses.ResponseNewParams{
		Model: "gpt-5",
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("hi")},
		Tools: []oairesponses.ToolUnionParam{{
			OfImageGeneration: &oairesponses.ToolImageGenerationParam{},
		}},
	}
	_, err := ToAnthropic(req, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported tool") {
		t.Fatalf("expected unsupported tool error, got %v", err)
	}
}
```

- [ ] **Step 2: Run RED**

Run:

```bash
go test ./internal/convert -run 'TestUnsupportedHostedToolChoiceReturnsError|TestUnsupportedToolDefinitionReturnsError' -count=1
```

Expected: FAIL because unsupported tools are currently skipped or ignored.

- [ ] **Step 3: Make tool conversion return errors**

Change:

```go
func convertTools(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) error
func appendToolList(out *anthropic.MessageNewParams, tools []oairesponses.ToolUnionParam)
func appendToolUnion(out *anthropic.MessageNewParams, t oairesponses.ToolUnionParam)
```

to:

```go
func convertTools(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) error
func appendToolList(out *anthropic.MessageNewParams, tools []oairesponses.ToolUnionParam) error
func appendToolUnion(out *anthropic.MessageNewParams, t oairesponses.ToolUnionParam) error
```

For unsupported cases, return:

```go
return fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", toolType(t))
```

Add `toolType` helper using SDK getters where possible:

```go
func toolType(t oairesponses.ToolUnionParam) string {
	if typ := t.GetType(); typ != nil && *typ != "" {
		return *typ
	}
	raw, _ := json.Marshal(t)
	var obj struct{ Type string `json:"type"` }
	_ = json.Unmarshal(raw, &obj)
	if obj.Type != "" {
		return obj.Type
	}
	return "unknown"
}
```

If `ToolUnionParam.GetType` is not available in SDK, use only the JSON fallback.

- [ ] **Step 4: Error unsupported tool_choice variants**

In `convertToolChoice`, add:

```go
case tc.OfHostedTool != nil:
	return fmt.Errorf("unsupported tool_choice %q: hosted tools are not supported by this Anthropic backend", *tc.GetType())
case tc.OfMcpTool != nil:
	return fmt.Errorf("unsupported tool_choice %q: MCP tool choice is not supported by this Anthropic backend", *tc.GetType())
case tc.OfResponseNewsToolChoiceSpecificProgrammaticToolCallingParam != nil:
	return fmt.Errorf("unsupported tool_choice %q: programmatic tool calling is not supported by this Anthropic backend", *tc.GetType())
```

Keep supported choices from earlier tasks.

- [ ] **Step 5: Run GREEN**

Run:

```bash
go test ./internal/convert -run 'TestUnsupportedHostedToolChoiceReturnsError|TestUnsupportedToolDefinitionReturnsError' -count=1
go test ./internal/convert -count=1
```

Expected: PASS.

- [ ] **Step 6: Update coverage matrix**

Keep unsupported rows as `unsupported_by_backend`, but update explanation where behavior changed from silent skip to explicit conversion error:

- hosted tool choice
- `mcp` choice
- `file_search`
- `computer`
- `computer_use_preview`
- `mcp`
- `programmatic_tool_calling`
- `image_generation`

- [ ] **Step 7: Verify and commit**

Run:

```bash
go test ./...
git diff --check
git add internal/convert/request.go internal/convert/request_test.go docs/protocol-coverage.md
git diff --cached --check
git commit -m "fix(convert): fail fast for unsupported tools"
```

---

### Task 5: Anthropic 未处理 stream block 不再静默丢弃

**Files:**
- Modify: `internal/streamconv/converter.go`
- Test: `internal/streamconv/converter_test.go`
- Modify: `docs/protocol-coverage.md`

**Interfaces:**
- Consumes: Anthropic `content_block_start` with unsupported block types
- Produces: `response.failed` with diagnostic message and no duplicate terminal event

- [ ] **Step 1: Write failing tests**

Add test:

```go
func TestUnsupportedAnthropicBlockFailsInsteadOfSilentDrop(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "message_start",
		Message: anthropic.Message{ID: "msg_unsupported", Model: "claude-test"},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start",
		Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "server_tool_use",
			ID: "srv_1",
			Name: "web_search",
		},
	})
	if len(evs) != 1 || evs[0].Type != "response.failed" {
		t.Fatalf("expected response.failed for unsupported block, got %+v", evs)
	}
	if !strings.Contains(string(evs[0].Data), "server_tool_use") {
		t.Fatalf("failed event should name unsupported block: %s", evs[0].Data)
	}
	trailing, _ := c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_stop"})
	if len(trailing) != 0 {
		t.Fatalf("unsupported block should mark converter complete, got trailing events %+v", trailing)
	}
}
```

- [ ] **Step 2: Run RED**

Run:

```bash
go test ./internal/streamconv -run TestUnsupportedAnthropicBlockFailsInsteadOfSilentDrop -count=1
```

Expected: FAIL because unsupported block currently returns nil and later completes.

- [ ] **Step 3: Implement unsupported block failure**

In `handleBlockStart`, replace `return nil` fallback:

```go
return []model.SSEEvent{c.handleUnsupportedBlock(ev)}
```

Add:

```go
func (c *Converter) handleUnsupportedBlock(ev *anthropic.MessageStreamEventUnion) model.SSEEvent {
	c.completed = true
	blockType := ev.ContentBlock.Type
	if blockType == "" {
		blockType = "unknown"
	}
	resp := model.NewResponseObject(c.respID, model.ResponseStatusFailed, c.model, c.createdAt, c.echo)
	resp.Output = []model.OutputItem{}
	resp.Error = &model.ResponseError{
		Message: fmt.Sprintf("unsupported Anthropic content block %q", blockType),
	}
	return model.MarshalEvent(evResponseFailed, model.TerminalResponseEvent{
		Type: evResponseFailed, SequenceNumber: c.nextSeq(), Response: resp,
	})
}
```

Ensure `Feed` checks `c.completed` before handling later events:

```go
if c.completed && ev.Type != anError {
	return nil, nil
}
```

- [ ] **Step 4: Run GREEN**

Run:

```bash
go test ./internal/streamconv -run TestUnsupportedAnthropicBlockFailsInsteadOfSilentDrop -count=1
go test ./internal/streamconv -count=1
```

Expected: PASS.

- [ ] **Step 5: Update coverage matrix**

Update Anthropic stream rows:

- `server_tool_use` -> `unsupported_by_backend` or `deferred`, note now fails explicitly instead of silent drop.
- `web_search_tool_result`、`code_execution_tool_result`、`bash_code_execution_tool_result`、`text_editor_code_execution_tool_result`、`tool_search_tool_result` remain `deferred`, note unsupported block currently fails until mapped.
- event `content_block_start` remains `lossy_supported`, note unsupported types fail diagnostically.

- [ ] **Step 6: Verify and commit**

Run:

```bash
go test ./...
git diff --check
git add internal/streamconv/converter.go internal/streamconv/converter_test.go docs/protocol-coverage.md
git diff --cached --check
git commit -m "fix(streamconv): fail on unsupported Anthropic blocks"
```

---

### Task 6: 收口全仓验证与覆盖矩阵一致性

**Files:**
- Modify: `docs/protocol-coverage.md`
- Optional Modify: tests touched by previous tasks if flakes or compile fallout appear

**Interfaces:**
- Consumes: all previous task changes
- Produces: verified first-batch protocol baseline

- [ ] **Step 1: Run full verification**

Run:

```bash
task check
git diff --check
```

Expected: `task check` exit 0 and `git diff --check` no output.

- [ ] **Step 2: Self-review coverage matrix**

Run:

```bash
rg -n "第一批|需修正|静默忽略|deferred" docs/protocol-coverage.md
```

For each remaining `deferred` line in first-batch scope, either:

- update it to `supported` / `lossy_supported` / `unsupported_by_backend`, or
- keep `deferred` with an explicit reason that it is outside first-batch scope.

- [ ] **Step 3: Inspect uncommitted scope**

Run:

```bash
git status --short
git diff --stat
```

Expected: only intended first-batch files changed. Existing unrelated dirty files from before this plan must not be staged unless they are part of a completed task.

- [ ] **Step 4: Final commit if needed**

If Task 6 changed only docs or minor test fallout:

```bash
git add docs/protocol-coverage.md
git diff --cached --check
git commit -m "docs(protocol): reconcile first batch coverage status"
```

Skip commit if no changes remain.

---

## Self-Review

- Spec coverage: 第一批 6 项均有任务覆盖：枚举/refusal、shell/apply_patch、allowed_tools、unsupported tools、unsupported Anthropic block、矩阵收口。
- Placeholder scan: 本计划不使用占位式任务；每个任务包含测试、RED/GREEN 命令、实现方向、提交命令。
- Type consistency: 核心接口保持 `ToAnthropic`、`Converter.Feed`、`SessionStore.Enrich`；需要改签名的 `convertToolChoice` / `appendToolList` 在任务内明确更新调用方。
- Dependency policy: 计划允许引入依赖，但第一批没有强制新增依赖；若执行中引入，必须记录用途、替代方案和验证命令。
