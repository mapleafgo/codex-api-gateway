# 通用 Tool Catalog 架构重构（批次 0）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立 `internal/toolcatalog` 单一事实来源包，把 convert / streamconv 中散落在 6+ 处 switch 的 tool 处理统一为 catalog 查询，行为零变化，为批次 A（code interpreter）/ B（MCP）铺路。

**Architecture:** 新建 `internal/toolcatalog` 包（被 `internal/convert` 与 `internal/streamconv` 共同依赖，无循环）。每种 tool 在 catalog 一处登记四维度：身份 `Identity`、类别 `Kind`、声明映射 `Declare`、server tool / cache 查询。请求侧（声明 / identity / allowed_tools / cache）与回程侧（server_tool_use dispatch）的 switch 全部改为遍历 catalog 查询。web_search 作为 `KindServerTool` 首例迁移进 catalog。

**Tech Stack:** Go 1.26.5、`github.com/anthropics/anthropic-sdk-go v1.57.0`、`github.com/openai/openai-go/v3 v3.42.0`、Go 标准测试。

## Global Constraints

- **纯重构，行为零变化**：批次 0 不改变任何现有 tool 的对外协议行为（function / custom / shell / apply_patch / tool_search / namespace / web_search 的输入输出完全不变），全程以现有 `request_test.go` / `converter_test.go` / 集成测试为安全网，**全程 GREEN**。
- **不写新 RED（协议行为测试）**：迁移 task 的验证步骤是"跑现有测试确认 GREEN"。仅 `internal/toolcatalog` 新包本身写新单元测试（验证 catalog 分类/映射行为，不是协议行为）。
- **静默跳过/降级必须 WARN**（见 AGENTS.md「静默跳过与降级处理约定」）：结构化日志至少含被丢弃内容类型、关联 response_id / 上下文 id、影响说明。禁止 `slog.Debug` / `fmt.Println`。
- **测试命令**：单包 `go test ./internal/<pkg>/`；全量门禁 `task check`（fmt + go vet + test）；流式状态机改动补 `task test-race`。
- **提交风格**：Conventional Commits，可中文描述，如 `refactor(convert): tool 声明 dispatch 迁移至 catalog`。
- **SDK 版本**：anthropic-sdk-go `v1.57.0`、openai-go/v3 `v3.42.0`，不得升级。

---

## 文件结构

新建 `internal/toolcatalog/`（本计划核心产物）：

| 文件 | 职责 |
|---|---|
| `internal/toolcatalog/identity.go` | `Kind` 常量、`Identity` 结构（请求侧身份）及其方法 |
| `internal/toolcatalog/inspect.go` | `Inspect`（OpenAI ToolUnionParam → 身份）、`InspectAllowed`（allowed_tools map → 身份）|
| `internal/toolcatalog/declare.go` | `Declare`（OpenAI ToolUnionParam → Anthropic 声明）、`ClientTool`（ToolParam 构造）、schema 辅助、`ToolName` |
| `internal/toolcatalog/server.go` | `ServerToolByAnthropicName`（回程 server_tool_use name 查询）、`ApplyCacheControl`（按 Anthropic 变体派发 cache_control）|
| `internal/toolcatalog/*_test.go` | catalog 各文件配套测试 |

修改既有文件：

| 文件 | 改动 |
|---|---|
| `internal/convert/request.go` | 删除 `appendToolUnion` / `appendWebSearchTool` / `toolIdentity` / `declaredToolIdentities` / `parseAllowedToolIdentities` / `parseAllowedNamespaceToolIdentities` / `formatToolNames` 内 switch / `setLastToolCacheControl` 内 switch / 散落 schema 辅助；dispatch 改调 catalog |
| `internal/convert/customtool.go` | `FreeformToolNames` / `appendFreeformToolName` switch 改调 catalog.Inspect |
| `internal/streamconv/converter.go` | `handleServerToolUseStart` 的 `name != "web_search"` 硬编码改调 `catalog.ServerToolByAnthropicName` |

依赖方向：`toolcatalog` → `{anthropic-sdk-go, openai-go, fmt, encoding/json}`；`convert` / `streamconv` → `toolcatalog`。无循环。

---

## Task 1: toolcatalog 身份与分类（identity + inspect）

**Files:**
- Create: `internal/toolcatalog/identity.go`
- Create: `internal/toolcatalog/inspect.go`
- Test: `internal/toolcatalog/inspect_test.go`

**Interfaces:**
- Consumes: `oairesponses.ToolUnionParam`（openai-go/v3/responses）
- Produces: `Kind`、`Identity`（含 `ConvertedName()` / `Equal()` / `String()`）、`Inspect(t) ([]Identity, error)`、`InspectAllowed(map[string]any) ([]Identity, error)`

- [ ] **Step 1: 写 identity.go**

```go
// Package toolcatalog 是 OpenAI Responses tool 类型到 Anthropic 处理方式的
// 单一事实来源。每种 tool 在一处登记其身份（Identity）、类别（Kind）与声明映射
// （Declare）；convert 与 streamconv 的 dispatch 统一查询本包，消除散落 switch。
package toolcatalog

import "fmt"

// Kind 分类一种 tool 在 Anthropic 侧的承载方式。
type Kind string

const (
	// KindClientTool 映射为 Anthropic ToolParam，由 Codex 客户端自行执行
	// （function / custom / shell / apply_patch / tool_search）。
	KindClientTool Kind = "client_tool"
	// KindServerTool 映射为 Anthropic 标准 server tool union 变体，
	// 由 Anthropic 托管执行（web_search）。
	KindServerTool Kind = "server_tool"
	// KindUnsupported 无安全等价物，按 protocol-coverage 矩阵 fail-fast 或 raw_preserved。
	KindUnsupported Kind = "unsupported"
)

// Identity 描述一个 OpenAI tool 在请求侧的身份。
type Identity struct {
	// OpenAIType 是 OpenAI Tool Union 的 type 值
	// （function / custom / shell / local_shell / apply_patch / tool_search / web_search）。
	OpenAIType string
	// Name 是写入 Anthropic 声明的工具名；namespace tool 形如 "<ns>__<name>"。
	Name string
	// Namespace 是 namespace 归属，非 namespace tool 为空。
	Namespace string
	// Freeform 为 true 表示输入是 freeform 文本（apply_patch / shell / custom），
	// 回程需把模型输出解包成裸文本以对齐客户端契约。
	Freeform bool
}

// ConvertedName 返回写入 Anthropic 声明的工具名（namespace 带前缀）。
func (i Identity) ConvertedName() string {
	if i.Namespace != "" {
		return i.Namespace + "__" + i.Name
	}
	return i.Name
}

// Equal 报告两个身份是否匹配（按 OpenAIType + Namespace + Name）。
// Freeform 不参与匹配，与原 convert.toolIdentity 的 == 比较行为一致。
func (i Identity) Equal(o Identity) bool {
	return i.OpenAIType == o.OpenAIType && i.Namespace == o.Namespace && i.Name == o.Name
}

func (i Identity) String() string {
	if i.Namespace != "" {
		return fmt.Sprintf("%s %q in namespace %q", i.OpenAIType, i.Name, i.Namespace)
	}
	return fmt.Sprintf("%s %q", i.OpenAIType, i.Name)
}
```

- [ ] **Step 2: 写 inspect_test.go（RED）**

```go
package toolcatalog

import (
	"testing"

	oairesponses "github.com/openai/openai-go/v3/responses"
)

func TestInspectClientTools(t *testing.T) {
	tests := []struct {
		name string
		tool oairesponses.ToolUnionParam
		want Identity
	}{
		{"function", oairesponses.ToolUnionParam{OfFunction: &oairesponses.FunctionToolParam{Name: "f"}}, Identity{OpenAIType: "function", Name: "f"}},
		{"custom", oairesponses.ToolUnionParam{OfCustom: &oairesponses.CustomToolParam{Name: "c"}}, Identity{OpenAIType: "custom", Name: "c", Freeform: true}},
		{"apply_patch", oairesponses.ToolUnionParam{OfApplyPatch: &oairesponses.ApplyPatchToolParam{}}, Identity{OpenAIType: "apply_patch", Name: "apply_patch", Freeform: true}},
		{"shell", oairesponses.ToolUnionParam{OfShell: &oairesponses.FunctionShellToolParam{}}, Identity{OpenAIType: "shell", Name: "shell", Freeform: true}},
		{"local_shell", oairesponses.ToolUnionParam{OfLocalShell: &oairesponses.ToolLocalShellParam{}}, Identity{OpenAIType: "local_shell", Name: "shell", Freeform: true}},
		{"tool_search", oairesponses.ToolUnionParam{OfToolSearch: &oairesponses.ToolSearchToolParam{}}, Identity{OpenAIType: "tool_search", Name: "tool_search"}},
		{"web_search", oairesponses.ToolUnionParam{OfWebSearch: &oairesponses.WebSearchToolParam{}}, Identity{OpenAIType: "web_search", Name: "web_search"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ids, err := Inspect(tc.tool)
			if err != nil {
				t.Fatalf("Inspect error: %v", err)
			}
			if len(ids) != 1 || !ids[0].Equal(tc.want) {
				t.Fatalf("got %+v, want %+v", ids, tc.want)
			}
		})
	}
}

func TestInspectNamespaceExpands(t *testing.T) {
	tool := oairesponses.ToolUnionParam{OfNamespace: &oairesponses.NamespaceToolParam{
		Name: "ns",
		Tools: []oairesponses.NamespaceToolToolUnionParam{
			{OfFunction: &oairesponses.NamespaceToolToolFunctionParam{Name: "f"}},
			{OfCustom: &oairesponses.CustomToolParam{Name: "c"}},
		},
	}}
	ids, err := Inspect(tool)
	if err != nil {
		t.Fatalf("Inspect error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 identities, got %d", len(ids))
	}
	if ids[0].ConvertedName() != "ns__f" || ids[1].ConvertedName() != "ns__c" {
		t.Fatalf("namespace names wrong: %+v", ids)
	}
	if !ids[1].Freeform {
		t.Fatalf("namespace custom must be freeform")
	}
}

func TestInspectUnsupportedErrors(t *testing.T) {
	_, err := Inspect(oairesponses.ToolUnionParam{})
	if err == nil {
		t.Fatal("expected error for unsupported tool")
	}
}

func TestInspectNamespaceUnsupportedChildErrors(t *testing.T) {
	tool := oairesponses.ToolUnionParam{OfNamespace: &oairesponses.NamespaceToolParam{
		Name:  "ns",
		Tools: []oairesponses.NamespaceToolToolUnionParam{{}}, // 空 child
	}}
	if _, err := Inspect(tool); err == nil {
		t.Fatal("expected error for unsupported namespace child")
	}
}

func TestInspectAllowedTypeStrings(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
		want Identity
	}{
		{"shell", map[string]any{"type": "shell"}, Identity{OpenAIType: "shell", Name: "shell", Freeform: true}},
		{"local_shell", map[string]any{"type": "local_shell"}, Identity{OpenAIType: "local_shell", Name: "shell", Freeform: true}},
		{"apply_patch", map[string]any{"type": "apply_patch"}, Identity{OpenAIType: "apply_patch", Name: "apply_patch", Freeform: true}},
		{"tool_search", map[string]any{"type": "tool_search"}, Identity{OpenAIType: "tool_search", Name: "tool_search"}},
		{"function", map[string]any{"type": "function", "name": "f"}, Identity{OpenAIType: "function", Name: "f"}},
		{"custom", map[string]any{"type": "custom", "name": "c"}, Identity{OpenAIType: "custom", Name: "c", Freeform: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ids, err := InspectAllowed(tc.raw)
			if err != nil {
				t.Fatalf("InspectAllowed error: %v", err)
			}
			if len(ids) != 1 || !ids[0].Equal(tc.want) {
				t.Fatalf("got %+v, want %+v", ids, tc.want)
			}
		})
	}
}

func TestInspectAllowedNamespace(t *testing.T) {
	raw := map[string]any{
		"type": "namespace", "name": "ns",
		"tools": []any{
			map[string]any{"type": "function", "name": "f"},
			map[string]any{"type": "custom", "name": "c"},
		},
	}
	ids, err := InspectAllowed(raw)
	if err != nil {
		t.Fatalf("InspectAllowed error: %v", err)
	}
	if len(ids) != 2 || ids[0].ConvertedName() != "ns__f" || ids[1].ConvertedName() != "ns__c" {
		t.Fatalf("namespace identities wrong: %+v", ids)
	}
}

func TestInspectAllowedErrors(t *testing.T) {
	if _, err := InspectAllowed(map[string]any{}); err == nil {
		t.Fatal("expected error for missing type")
	}
	if _, err := InspectAllowed(map[string]any{"type": "function"}); err == nil {
		t.Fatal("expected error for function without name")
	}
	if _, err := InspectAllowed(map[string]any{"type": "unknown"}); err == nil {
		t.Fatal("expected error for unsupported type")
	}
}
```

> **注意**：Step 2 中 `oairesponses.FunctionToolParam` / `CustomToolParam` / `ShellToolParam` / `LocalShellToolParam` / `ApplyPatchToolParam` / `ToolSearchToolParam` / `WebSearchToolParam` / `NamespaceToolParam` 是 SDK 中 `ToolUnionParam` 各 `Of*` 字段的具体类型。若 SDK 对某变体类型名不同（例如 `WebSearchToolParam` 实际为 `WebSearchPreviewToolParam`），以 `go doc github.com/openai/openai-go/v3/responses.ToolUnionParam` 的字段类型为准修正——本步 RED 编译失败即暴露确切名字。

- [ ] **Step 3: 运行测试验证 RED**

Run: `go test ./internal/toolcatalog/`
Expected: 编译失败（`Inspect` / `InspectAllowed` 未定义）。

- [ ] **Step 4: 写 inspect.go（实现）**

```go
package toolcatalog

import (
	"encoding/json"
	"fmt"

	oairesponses "github.com/openai/openai-go/v3/responses"
)

// Inspect 从一个 OpenAI ToolUnionParam 提取身份。
// namespace tool 展开为多个身份（每个子 tool 一个）；其余变体返回单个身份。
// 不支持的变体返回错误，调用方据此 fail-fast。
func Inspect(t oairesponses.ToolUnionParam) ([]Identity, error) {
	switch {
	case t.OfFunction != nil:
		return []Identity{{OpenAIType: "function", Name: t.OfFunction.Name}}, nil
	case t.OfCustom != nil:
		return []Identity{{OpenAIType: "custom", Name: t.OfCustom.Name, Freeform: true}}, nil
	case t.OfApplyPatch != nil:
		return []Identity{{OpenAIType: "apply_patch", Name: "apply_patch", Freeform: true}}, nil
	case t.OfShell != nil:
		return []Identity{{OpenAIType: "shell", Name: "shell", Freeform: true}}, nil
	case t.OfLocalShell != nil:
		return []Identity{{OpenAIType: "local_shell", Name: "shell", Freeform: true}}, nil
	case t.OfToolSearch != nil:
		return []Identity{{OpenAIType: "tool_search", Name: "tool_search"}}, nil
	case t.OfNamespace != nil:
		ns := t.OfNamespace
		out := make([]Identity, 0, len(ns.Tools))
		for _, nested := range ns.Tools {
			switch {
			case nested.OfFunction != nil:
				out = append(out, Identity{OpenAIType: "function", Namespace: ns.Name, Name: nested.OfFunction.Name})
			case nested.OfCustom != nil:
				out = append(out, Identity{OpenAIType: "custom", Namespace: ns.Name, Name: nested.OfCustom.Name, Freeform: true})
			default:
				return nil, fmt.Errorf("unsupported namespace tool: Anthropic backend has no safe equivalent")
			}
		}
		return out, nil
	case t.OfWebSearch != nil, t.OfWebSearchPreview != nil:
		return []Identity{{OpenAIType: "web_search", Name: "web_search"}}, nil
	default:
		return nil, fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", openaiToolType(t))
	}
}

// InspectAllowed 从一个 allowed_tools 条目（弱类型 map）提取身份。
// 与 Inspect 覆盖同一组 tool 类型，但入口是 tool_choice.allowed_tools 的
// `{type, name?, tools?}` 结构，而非强类型 ToolUnionParam。
func InspectAllowed(tool map[string]any) ([]Identity, error) {
	typ, ok := tool["type"].(string)
	if !ok || typ == "" {
		return nil, fmt.Errorf("tool_choice allowed_tools entry requires a type")
	}
	switch typ {
	case "shell", "local_shell":
		return []Identity{{OpenAIType: typ, Name: "shell", Freeform: true}}, nil
	case "apply_patch":
		return []Identity{{OpenAIType: typ, Name: "apply_patch", Freeform: true}}, nil
	case "tool_search":
		return []Identity{{OpenAIType: typ, Name: "tool_search"}}, nil
	case "function", "custom":
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tool_choice allowed_tools entry %q requires a name", typ)
		}
		return []Identity{{OpenAIType: typ, Name: name, Freeform: typ == "custom"}}, nil
	case "namespace":
		return inspectAllowedNamespace(tool)
	default:
		return nil, fmt.Errorf("unsupported tool_choice allowed_tools entry %q: Anthropic backend has no safe equivalent", typ)
	}
}

func inspectAllowedNamespace(tool map[string]any) ([]Identity, error) {
	namespace, _ := tool["name"].(string)
	if namespace == "" {
		return nil, fmt.Errorf("tool_choice allowed_tools namespace requires a name")
	}
	rawTools, _ := tool["tools"].([]any)
	if len(rawTools) == 0 {
		return nil, fmt.Errorf("tool_choice allowed_tools namespace %q requires tools", namespace)
	}
	out := make([]Identity, 0, len(rawTools))
	for _, rawTool := range rawTools {
		nested, ok := rawTool.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool_choice allowed_tools namespace %q has invalid child", namespace)
		}
		typ, _ := nested["type"].(string)
		if typ != "function" && typ != "custom" {
			return nil, fmt.Errorf("unsupported tool_choice allowed_tools namespace %q child type %q", namespace, typ)
		}
		name, _ := nested["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tool_choice allowed_tools namespace %q child %q requires a name", namespace, typ)
		}
		out = append(out, Identity{OpenAIType: typ, Namespace: namespace, Name: name, Freeform: typ == "custom"})
	}
	return out, nil
}

// openaiToolType 从 ToolUnionParam 取出 type 字符串，用于错误信息。
func openaiToolType(t oairesponses.ToolUnionParam) string {
	if typ := t.GetType(); typ != nil && *typ != "" {
		return *typ
	}
	raw, _ := json.Marshal(t)
	var obj struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &obj)
	if obj.Type != "" {
		return obj.Type
	}
	return "unknown"
}
```

- [ ] **Step 5: 运行测试验证 GREEN**

Run: `go test ./internal/toolcatalog/`
Expected: PASS（全部 inspect_test 用例通过）。

- [ ] **Step 6: Commit**

```bash
git add internal/toolcatalog/identity.go internal/toolcatalog/inspect.go internal/toolcatalog/inspect_test.go
git commit -m "feat(toolcatalog): 新增 Identity/Kind 与 Inspect/InspectAllowed 分类入口"
```

---

## Task 2: catalog.Declare + convert 请求声明 dispatch 迁移

**Files:**
- Create: `internal/toolcatalog/declare.go`
- Test: `internal/toolcatalog/declare_test.go`
- Modify: `internal/convert/request.go`（`appendToolList` / `appendToolUnion` / `appendWebSearchTool` / `appendConvertedTool`，约 591-700 行）

**Interfaces:**
- Consumes: Task 1 的 `Identity`；现有 `appendConvertedTool` 的 schema 辅助逻辑
- Produces: `Declare(t) ([]anthropic.ToolUnionParam, error)`、`ClientTool(name, schema, desc, custom) anthropic.ToolUnionParam`、`ToolName(ns, name) string`

- [ ] **Step 1: 写 declare_test.go（RED）**

```go
package toolcatalog

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

func TestDeclareFunction(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfFunction: &oairesponses.FunctionToolParam{
		Name: "f", Parameters: map[string]any{"type": "object"}, Description: oparam.NewOpt("d"),
	}})
	if err != nil {
		t.Fatalf("Declare error: %v", err)
	}
	if len(decls) != 1 || decls[0].OfTool == nil || decls[0].OfTool.Name != "f" {
		t.Fatalf("expected single ToolParam 'f', got %+v", decls)
	}
}

func TestDeclareCustomIsFreeform(t *testing.T) {
	decls, _ := Declare(oairesponses.ToolUnionParam{OfCustom: &oairesponses.CustomToolParam{Name: "c"}})
	if decls[0].OfTool.Type != anthropic.ToolTypeCustom {
		t.Fatalf("custom tool must set ToolTypeCustom")
	}
}

func TestDeclareNamespacePrefixesNames(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfNamespace: &oairesponses.NamespaceToolParam{
		Name:  "ns",
		Tools: []oairesponses.NamespaceToolToolUnionParam{{OfFunction: &oairesponses.NamespaceToolToolFunctionParam{Name: "f"}}},
	}})
	if err != nil {
		t.Fatalf("Declare error: %v", err)
	}
	if len(decls) != 1 || decls[0].OfTool.Name != "ns__f" {
		t.Fatalf("namespace name not prefixed: %+v", decls)
	}
}

func TestDeclareWebSearchMapsAllowedDomains(t *testing.T) {
	decls, err := Declare(oairesponses.ToolUnionParam{OfWebSearch: &oairesponses.WebSearchToolParam{
		Filters: oairesponses.WebSearchToolFiltersParam{AllowedDomains: []string{"a.com"}},
	}})
	if err != nil {
		t.Fatalf("Declare error: %v", err)
	}
	if decls[0].OfWebSearchTool20250305 == nil || len(decls[0].OfWebSearchTool20250305.AllowedDomains) != 1 {
		t.Fatalf("web_search not mapped: %+v", decls)
	}
}

func TestDeclareWebSearchPreviewNoDomains(t *testing.T) {
	decls, _ := Declare(oairesponses.ToolUnionParam{OfWebSearchPreview: &oairesponses.WebSearchPreviewToolParam{}})
	if decls[0].OfWebSearchTool20250305 == nil || len(decls[0].OfWebSearchTool20250305.AllowedDomains) != 0 {
		t.Fatalf("web_search_preview must map to empty-domain server tool")
	}
}

func TestDeclareUnsupportedErrors(t *testing.T) {
	if _, err := Declare(oairesponses.ToolUnionParam{}); err == nil {
		t.Fatal("expected error for unsupported tool")
	}
}
```

- [ ] **Step 2: 运行测试验证 RED**

Run: `go test ./internal/toolcatalog/`
Expected: 编译失败（`Declare` 未定义）。

- [ ] **Step 3: 写 declare.go（实现）**

```go
package toolcatalog

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	aparam "github.com/anthropics/anthropic-sdk-go/packages/param"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// Declare 把一个 OpenAI ToolUnionParam 映射为 Anthropic tool 声明。
// 返回的切片追加到 MessageNewParams.Tools；namespace tool 展开为多个声明。
// 不支持的变体返回错误（调用方 fail-fast）。
func Declare(t oairesponses.ToolUnionParam) ([]anthropic.ToolUnionParam, error) {
	switch {
	case t.OfFunction != nil:
		fn := t.OfFunction
		return []anthropic.ToolUnionParam{ClientTool(fn.Name, schemaFromAny(fn.Parameters), optionalString(fn.Description), false)}, nil
	case t.OfCustom != nil:
		c := t.OfCustom
		return []anthropic.ToolUnionParam{ClientTool(c.Name, freeformInputSchema(), optionalString(c.Description), true)}, nil
	case t.OfApplyPatch != nil:
		return []anthropic.ToolUnionParam{ClientTool("apply_patch", applyPatchInputSchema(), nil, true)}, nil
	case t.OfShell != nil:
		return []anthropic.ToolUnionParam{ClientTool("shell", freeformInputSchema(), nil, true)}, nil
	case t.OfLocalShell != nil:
		return []anthropic.ToolUnionParam{ClientTool("shell", freeformInputSchema(), nil, true)}, nil
	case t.OfToolSearch != nil:
		s := t.OfToolSearch
		return []anthropic.ToolUnionParam{ClientTool("tool_search", schemaFromAny(s.Parameters), optionalString(s.Description), false)}, nil
	case t.OfNamespace != nil:
		ns := t.OfNamespace
		out := make([]anthropic.ToolUnionParam, 0, len(ns.Tools))
		for _, nested := range ns.Tools {
			switch {
			case nested.OfFunction != nil:
				fn := nested.OfFunction
				out = append(out, ClientTool(ToolName(ns.Name, fn.Name), schemaFromAny(fn.Parameters), optionalString(fn.Description), false))
			case nested.OfCustom != nil:
				c := nested.OfCustom
				out = append(out, ClientTool(ToolName(ns.Name, c.Name), freeformInputSchema(), optionalString(c.Description), true))
			default:
				return nil, fmt.Errorf("unsupported namespace tool: Anthropic backend has no safe equivalent")
			}
		}
		return out, nil
	case t.OfWebSearch != nil:
		return []anthropic.ToolUnionParam{{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
			AllowedDomains: t.OfWebSearch.Filters.AllowedDomains,
		}}}, nil
	case t.OfWebSearchPreview != nil:
		return []anthropic.ToolUnionParam{{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{}}}, nil
	default:
		return nil, fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", openaiToolType(t))
	}
}

// ClientTool 构造一个 Anthropic client tool（ToolParam）。
// custom=true 标记为 freeform custom tool（apply_patch / shell / custom）。
// 被 Declare 与 convert 的 structured-output 注入共用。
func ClientTool(name string, schema map[string]any, description *string, custom bool) anthropic.ToolUnionParam {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	tool := &anthropic.ToolParam{
		Name:        name,
		InputSchema: toInputSchema(schema),
	}
	if description != nil {
		tool.Description = aparam.NewOpt(*description)
	}
	if custom {
		tool.Type = anthropic.ToolTypeCustom
	}
	return anthropic.ToolUnionParam{OfTool: tool}
}

// ToolName 返回 namespace 工具的转换后名（namespace 为空时原样返回）。
func ToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "__" + name
}

func optionalString(v oparam.Opt[string]) *string {
	if !v.Valid() {
		return nil
	}
	return &v.Value
}

func schemaFromAny(v any) map[string]any {
	s, _ := v.(map[string]any)
	return s
}

func freeformInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{"type": "string"},
		},
		"required": []string{"input"},
	}
}

func applyPatchInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operation": map[string]any{"type": "string", "enum": []string{"create_file", "delete_file", "update_file"}},
			"path":      map[string]any{"type": "string"},
			"diff":      map[string]any{"type": "string"},
		},
		"required": []string{"operation", "path"},
	}
}

func toInputSchema(schema map[string]any) anthropic.ToolInputSchemaParam {
	props, _ := schema["properties"].(map[string]any)
	var required []string
	switch r := schema["required"].(type) {
	case []string:
		required = r
	case []any:
		required = make([]string, 0, len(r))
		for _, item := range r {
			if s, ok := item.(string); ok {
				required = append(required, s)
			}
		}
	}
	return anthropic.ToolInputSchemaParam{Properties: props, Required: required}
}
```

- [ ] **Step 4: 运行 catalog 测试验证 GREEN**

Run: `go test ./internal/toolcatalog/`
Expected: PASS。

- [ ] **Step 5: 迁移 convert 请求声明 dispatch**

在 `internal/convert/request.go`：

(a) 删除 `appendToolUnion`（约 604-646）、`appendWebSearchTool`（约 648-661）、`appendConvertedTool` 内部的 ToolParam 构造逻辑，改为委托 catalog。

(b) 把 `appendConvertedTool`（约 678-700）改为：

```go
func appendConvertedTool(out *anthropic.MessageNewParams, name string, schema map[string]any, description *string, custom bool) error {
	if name == "" {
		return fmt.Errorf("tool conversion requires a name")
	}
	if hasTool(out, name) {
		return fmt.Errorf("tool conversion name conflict for %q", name)
	}
	out.Tools = append(out.Tools, toolcatalog.ClientTool(name, schema, description, custom))
	return nil
}
```

(c) 把 `appendToolList` / `appendToolUnion`（约 595-646）替换为单一函数：

```go
func appendToolList(out *anthropic.MessageNewParams, tools []oairesponses.ToolUnionParam) error {
	for _, t := range tools {
		decls, err := toolcatalog.Declare(t)
		if err != nil {
			return err
		}
		for _, d := range decls {
			if d.OfTool != nil && hasTool(out, d.OfTool.Name) {
				return fmt.Errorf("tool conversion name conflict for %q", d.OfTool.Name)
			}
			out.Tools = append(out.Tools, d)
		}
	}
	return nil
}
```

并删除原 `convertTools`（591-593）中对 `appendToolUnion` 的调用改为直接 `appendToolList`（或保留 `convertTools` 调 `appendToolList`，不变）。

(d) 删除已迁入 catalog 的散落 schema 辅助：`freeformInputSchema`（723）、`applyPatchInputSchema`（733）、`schemaFromAny`（718）、`toInputSchema`（574）、`optionalString`（711）、`toolName`（535）、`toolType`（663）。

> **关键**：`toolName` 在回灌路径（`appendFunctionCall` / `appendCustomToolCall`，410/414）也被调用。删除 `toolName` 前，把这两处的 `toolName(fc.Namespace.Value, fc.Name)` / `toolName(call.Namespace.Value, call.Name)` 改为 `toolcatalog.ToolName(...)`。`injectStructuredOutput`（745-766）仍调 `appendConvertedTool`，保持不变。

(e) import 块加 `"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"`。

- [ ] **Step 6: 运行 convert 测试验证 GREEN**

Run: `go test ./internal/convert/`
Expected: PASS（`TestWebSearchToolMapsToAnthropicServerTool` / `TestWebSearchPreviewToolMapsToAnthropicServerTool` / `TestCacheControlAppliedToNonFunctionTool` / `TestUnsupportedToolDefinitionReturnsError` 等全绿）。

- [ ] **Step 7: Commit**

```bash
git add internal/toolcatalog/declare.go internal/toolcatalog/declare_test.go internal/convert/request.go
git commit -m "refactor(convert): tool 声明 dispatch 迁移至 toolcatalog.Declare"
```

---

## Task 3: convert 身份 / 命名 / freeform dispatch 迁移

**Files:**
- Modify: `internal/convert/request.go`（`toolIdentity` 定义 954-972、`hasToolIdentity` 945、`declaredToolIdentities` 910-943、`formatToolNames` 1145-1175、`convertToolChoice` 与 `applySpecificToolChoice` 中的 `toolIdentity` 字面量）
- Modify: `internal/convert/customtool.go`（`appendFreeformToolName` 30-47）
- Test: `internal/convert/request_test.go` / `customtool_test.go`（既有用例，安全网）

**Interfaces:**
- Consumes: Task 1 的 `Inspect`、`Identity`（`ConvertedName` / `Equal`）；Task 2 的 `ToolName`
- Produces: convert 内不再有 `toolIdentity` 类型，统一用 `toolcatalog.Identity`

> **行为注意点（唯一与旧实现有意差异处）**：旧 `declaredToolIdentities`（910-943）对 `OfWebSearch`/`OfWebSearchPreview` 落入 `default` 报错。迁移后 `toolcatalog.Inspect` 对 web_search 返回合法身份（不报错）。这只影响「同一请求同时声明 web_search tool 且带 specific/allowed tool_choice」这一此前会 fail-fast 的边角组合——迁移后该组合不再因 web_search 报错。现有测试无此组合断言（web_search 测试均不带 tool_choice），故安全网保持 GREEN；Task 7 全量验收时再确认无回归。

- [ ] **Step 1: 用 catalog.Identity 替换 toolIdentity（全包一次性类型替换）**

在 `internal/convert/request.go`：

(a) 删除 `toolIdentity` 结构体（954-958）及其 `convertedName()`（960-965）、`String()`（967-972）方法。

(b) `hasToolIdentity`（945-952）改为：

```go
func hasToolIdentity(identities []toolcatalog.Identity, want toolcatalog.Identity) bool {
	for _, identity := range identities {
		if identity.Equal(want) {
			return true
		}
	}
	return false
}
```

(c) `declaredToolIdentities`（910-943）改为：

```go
func declaredToolIdentities(tools []oairesponses.ToolUnionParam) ([]toolcatalog.Identity, error) {
	identities := make([]toolcatalog.Identity, 0, len(tools))
	for _, tool := range tools {
		ids, err := toolcatalog.Inspect(tool)
		if err != nil {
			return nil, err
		}
		identities = append(identities, ids...)
	}
	return identities, nil
}
```

(d) `convertToolChoice`（768-831）与 `applySpecificToolChoice`（849-861）中的 `toolIdentity{typ:..., name:...}` 字面量改为 `toolcatalog.Identity{OpenAIType:..., Name:...}`，`.convertedName()` 改为 `.ConvertedName()`。涉及行：
  - `applySpecificToolChoice` 的参数类型与 `want.convertedName()` → `want.ConvertedName()`
  - `convertToolChoice` 中 `OfFunctionTool` → `toolcatalog.Identity{OpenAIType: "function", Name: tc.OfFunctionTool.Name}`
  - `OfCustomTool` → `toolcatalog.Identity{OpenAIType: "custom", Name: tc.OfCustomTool.Name}`
  - `OfSpecificApplyPatchToolChoice` → `toolcatalog.Identity{OpenAIType: "apply_patch", Name: "apply_patch"}`
  - `OfSpecificShellToolChoice` → `toolcatalog.Identity{OpenAIType: "shell", Name: "shell"}`
  - `applySpecificToolChoice` 签名 `want toolIdentity` → `want toolcatalog.Identity`

(e) `parseAllowedToolIdentities`（974-997）与 `parseAllowedNamespaceToolIdentities`（999-1025）：**仅改返回类型与 `toolIdentity` 字面量**，内部 switch 暂保留（Task 4 再迁内部逻辑到 `InspectAllowed`）。把 `[]toolIdentity` → `[]toolcatalog.Identity`，`toolIdentity{typ:..., name:...}` → `toolcatalog.Identity{OpenAIType:..., Name:...}`。`allowedToolNames`（889-907）中 `identity.convertedName()` → `identity.ConvertedName()`。

- [ ] **Step 2: 迁移 formatToolNames**

`formatToolNames`（1145-1175）改为：

```go
func formatToolNames(tag string, tools []oairesponses.ToolUnionParam) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		ids, err := toolcatalog.Inspect(tool)
		if err != nil {
			continue // 未知 tool 跳过，与原 switch 无 default 语义一致
		}
		for _, id := range ids {
			names = append(names, id.ConvertedName())
		}
	}
	body, err := json.Marshal(names)
	if err != nil {
		body = []byte("[]")
	}
	return fmt.Sprintf("<%s>\n%s\n</%s>", tag, string(body), tag)
}
```

- [ ] **Step 3: 迁移 FreeformToolNames**

`internal/convert/customtool.go` 的 `appendFreeformToolName`（30-47）改为：

```go
func appendFreeformToolName(names []string, tool oairesponses.ToolUnionParam) []string {
	ids, err := toolcatalog.Inspect(tool)
	if err != nil {
		return names
	}
	for _, id := range ids {
		if id.Freeform {
			names = append(names, id.ConvertedName())
		}
	}
	return names
}
```

customtool.go import 块加 `"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"`。

> **语义对齐**：旧 `appendFreeformToolName` 对 custom / apply_patch / shell / local_shell / namespace-custom 产出 freeform 名；`Inspect` 的 `Freeform` 标记精确覆盖同一集合（function / tool_search / web_search / namespace-function 的 `Freeform=false` 被过滤），故输出集合一致。

- [ ] **Step 4: 运行 convert 测试验证 GREEN**

Run: `go test ./internal/convert/`
Expected: PASS（`TestToolCallsConvert` / `TestCustomToolCallInputAndOutputConvert` / `TestAdditionalToolsAndToolSearchItemsConvert` / `TestSpecificToolChoiceRejectsUndeclaredIdentity` / `TestFreeformToolNames*` 等全绿）。

- [ ] **Step 5: Commit**

```bash
git add internal/convert/request.go internal/convert/customtool.go
git commit -m "refactor(convert): identity/命名/freeform dispatch 迁移至 toolcatalog.Inspect"
```

---

## Task 4: convert allowed_tools dispatch 迁移

**Files:**
- Modify: `internal/convert/request.go`（`parseAllowedToolIdentities` 974-997、`parseAllowedNamespaceToolIdentities` 999-1025）
- Test: `internal/convert/request_test.go`（既有 allowed_tools 用例，安全网）

**Interfaces:**
- Consumes: Task 1 的 `InspectAllowed`
- Produces: convert 内 `parseAllowedToolIdentities` 成为 `InspectAllowed` 的薄封装（或被内联删除）

- [ ] **Step 1: 把 parseAllowedToolIdentities 委托给 catalog**

`parseAllowedToolIdentities`（974-997）与 `parseAllowedNamespaceToolIdentities`（999-1025）整体替换为：

```go
func parseAllowedToolIdentities(tool map[string]any) ([]toolcatalog.Identity, error) {
	return toolcatalog.InspectAllowed(tool)
}
```

删除 `parseAllowedNamespaceToolIdentities`（其逻辑已并入 `toolcatalog.inspectAllowedNamespace`）。确认 `allowedToolNames`（889-907）对 `parseAllowedToolIdentities` 的调用签名不变（仍返回 `[]toolcatalog.Identity, error`，Task 3 已改类型）。

- [ ] **Step 2: 运行 convert 测试验证 GREEN**

Run: `go test ./internal/convert/`
Expected: PASS（`TestAllowedToolsFiltersAnthropicToolsAndUsesRequiredMode` / `TestAllowedToolsRejectsUnsupportedAllowedEntries` / `TestAllowedToolsRejectsPartialIdentity` / `TestAllowedToolsRejectsCrossTypeSameName` 等全绿）。

- [ ] **Step 3: Commit**

```bash
git add internal/convert/request.go
git commit -m "refactor(convert): allowed_tools dispatch 迁移至 toolcatalog.InspectAllowed"
```

---

## Task 5: catalog server tool 注册 + cache_control 派发迁移

**Files:**
- Create: `internal/toolcatalog/server.go`
- Test: `internal/toolcatalog/server_test.go`
- Modify: `internal/convert/request.go`（`setLastToolCacheControl` 1099-1115）

**Interfaces:**
- Consumes: `anthropic.ToolUnionParam`、`anthropic.CacheControlEphemeralParam`
- Produces: `ServerToolByAnthropicName(name) (Identity, bool)`、`ApplyCacheControl(*anthropic.ToolUnionParam, anthropic.CacheControlEphemeralParam) bool`

- [ ] **Step 1: 写 server_test.go（RED）**

```go
package toolcatalog

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestServerToolByAnthropicName(t *testing.T) {
	id, ok := ServerToolByAnthropicName("web_search")
	if !ok || id.Name != "web_search" {
		t.Fatalf("web_search must be registered: %+v ok=%v", id, ok)
	}
	if _, ok := ServerToolByAnthropicName("code_execution"); ok {
		t.Fatal("code_execution not registered in batch 0")
	}
}

func TestApplyCacheControlRecognizedVariants(t *testing.T) {
	cc := anthropic.CacheControlEphemeralParam{TTL: anthropic.CacheControlEphemeralTTLTTL5m}

	tool := anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: "f"}}
	if !ApplyCacheControl(&tool, cc) || tool.OfTool.CacheControl.TTL != cc.TTL {
		t.Fatalf("OfTool cache_control not applied")
	}

	ws := anthropic.ToolUnionParam{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{}}
	if !ApplyCacheControl(&ws, cc) {
		t.Fatalf("OfWebSearchTool20250305 cache_control not applied")
	}
}

func TestApplyCacheControlUnknownReturnsFalse(t *testing.T) {
	var empty anthropic.ToolUnionParam // 所有变体 nil
	if ApplyCacheControl(&empty, anthropic.CacheControlEphemeralParam{}) {
		t.Fatal("unknown variant must return false")
	}
}
```

- [ ] **Step 2: 运行测试验证 RED**

Run: `go test ./internal/toolcatalog/`
Expected: 编译失败（`ServerToolByAnthropicName` / `ApplyCacheControl` 未定义）。

- [ ] **Step 3: 写 server.go（实现）**

```go
package toolcatalog

import "github.com/anthropics/anthropic-sdk-go"

// serverToolByAnthropicName 登记 Anthropic 回程 server_tool_use 的 name → 身份。
// streamconv 用它判定一个 server_tool_use 是否对应已注册的 hosted server tool；
// 批次 A 注册 code_execution 后在此追加，回程 dispatch 自动覆盖。
var serverToolByAnthropicName = map[string]Identity{
	"web_search": {OpenAIType: "web_search", Name: "web_search", Freeform: false},
}

// ServerToolByAnthropicName 查询一个 Anthropic server_tool_use name 是否对应
// 已注册的 server tool。未注册返回 ok=false（调用方按 skip 处理）。
func ServerToolByAnthropicName(name string) (Identity, bool) {
	id, ok := serverToolByAnthropicName[name]
	return id, ok
}

// ApplyCacheControl 把 cache_control 写入一个 Anthropic tool union 的对应变体。
// 返回是否成功识别变体；未识别返回 false（调用方 WARN，避免静默丢失缓存）。
// 批次 A 注册 code_execution 变体后在此 switch 扩展。
func ApplyCacheControl(tool *anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam) bool {
	switch {
	case tool.OfTool != nil:
		tool.OfTool.CacheControl = cc
	case tool.OfWebSearchTool20250305 != nil:
		tool.OfWebSearchTool20250305.CacheControl = cc
	default:
		return false
	}
	return true
}
```

- [ ] **Step 4: 运行 catalog 测试验证 GREEN**

Run: `go test ./internal/toolcatalog/`
Expected: PASS。

- [ ] **Step 5: 迁移 setLastToolCacheControl**

`internal/convert/request.go` 的 `setLastToolCacheControl`（1099-1115）改为：

```go
// setLastToolCacheControl 给 tools 列表的最后一个 tool 加 cache_control，
// 派发由 toolcatalog.ApplyCacheControl 承载（覆盖所有已知 server tool 变体）。
func setLastToolCacheControl(tools []anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam) {
	if len(tools) == 0 {
		return
	}
	last := &tools[len(tools)-1]
	if !toolcatalog.ApplyCacheControl(last, cc) {
		slog.Warn("最后一个 tool 是未知变体，无法加 cache_control，tools 列表缓存将丢失")
	}
}
```

- [ ] **Step 6: 运行 convert 测试验证 GREEN**

Run: `go test ./internal/convert/`
Expected: PASS（`TestCacheControlAppliedToNonFunctionTool` / `TestSetLastToolCacheControlUnknownVariantNoPanic` 全绿）。

- [ ] **Step 7: Commit**

```bash
git add internal/toolcatalog/server.go internal/toolcatalog/server_test.go internal/convert/request.go
git commit -m "refactor(convert): cache_control 派发迁移至 toolcatalog.ApplyCacheControl"
```

---

## Task 6: streamconv 回程 server tool dispatch 迁移

**Files:**
- Modify: `internal/streamconv/converter.go`（`handleServerToolUseStart` 400-433）
- Test: `internal/streamconv/converter_test.go`（既有 web_search / skip 用例，安全网）

**Interfaces:**
- Consumes: Task 5 的 `ServerToolByAnthropicName`
- Produces: streamconv 不再硬编码 `"web_search"` 字符串

- [ ] **Step 1: 迁移 handleServerToolUseStart**

`internal/streamconv/converter.go` 的 `handleServerToolUseStart`（400-406）开头判定改为：

```go
func (c *Converter) handleServerToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	// 只有 catalog 已注册的 hosted server tool 才映射为 Responses item；
	// 其余 server_tool_use（web_fetch、code_execution …批次 0 未注册）无安全
	// Responses 等价物，跳过以保持流继续。
	if _, ok := toolcatalog.ServerToolByAnthropicName(ev.ContentBlock.Name); !ok {
		return c.handleSkippedServerToolUseStart(ev)
	}
	// 以下 web_search 映射逻辑保持不变（构造 web_search_call item + 事件链）。
	idx := c.itemOrder
	// …（原 407-432 行不变）
```

> **保留原 web_search 事件链**：`handleServerToolUseStart` 在通过 catalog 判定后，原 `idx := c.itemOrder` 及其后的 `web_search_call` item 构造、`webSearchByToolUseID` 记录、`output_item.added` / `web_search_call.in_progress` / `web_search_call.searching` 事件（407-432）保持完全不变。本步只把 `ev.ContentBlock.Name != "web_search"` 的硬编码字符串比较换成 catalog 查询。

import 块（converter.go 4-15）加 `"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"`。

- [ ] **Step 2: 运行 streamconv 测试验证 GREEN**

Run: `go test ./internal/streamconv/`
Expected: PASS（`TestWebSearchServerToolUseEmitsWebSearchCall` / `TestWebSearchResultSurfacesSources` / `TestNonWebSearchServerToolUseSkippedNotFailed` / `TestToolResultWithWebSearchToolUseIDCompletesWebSearchCall` 全绿）。

- [ ] **Step 3: 流式状态机补 race 检测**

Run: `task test-race`
Expected: PASS（无 data race）。

- [ ] **Step 4: Commit**

```bash
git add internal/streamconv/converter.go
git commit -m "refactor(streamconv): server_tool_use dispatch 迁移至 toolcatalog.ServerToolByAnthropicName"
```

---

## Task 7: 全变体兜底 + 全量验收 + 文档同步

**Files:**
- Verify: `internal/convert/request.go`（无残留散落 switch）、`internal/toolcatalog/*`
- Verify: `docs/protocol-coverage.md`（批次 0 不升级状态，仅确认）
- Test: 全仓库

**Interfaces:**
- Consumes: 全部前序 task

- [ ] **Step 1: 确认无残留散落 switch**

Run:
```bash
grep -nE 'appendToolUnion|appendWebSearchTool|parseAllowedNamespaceToolIdentities|type toolIdentity|func toolName' internal/convert/request.go
```
Expected: 无输出（全部已删除 / 迁移）。

Run:
```bash
grep -nE 'ev\.ContentBlock\.Name != "web_search"|== "web_search"' internal/streamconv/converter.go
```
Expected: 无输出（硬编码已由 catalog 查询替代，`extractWebSearchQuery` 等纯辅助函数内的字面量不在本次消除范围）。

- [ ] **Step 2: 确认 unsupported 变体仍 fail-fast**

OpenAI Tool Union 中批次 0 不支持的 hosted 变体（`file_search` / `computer` / `computer_use_preview` / `image_generation` / `programmatic_tool_calling` 等）在 `toolcatalog.Inspect` / `Declare` 落入 `default` 返回错误，等价于旧的 fail-fast。验证：

Run: `go test ./internal/convert/ -run 'TestUnsupportedToolDefinitionReturnsError|TestUnsupportedHostedToolChoiceReturnsError' -v`
Expected: PASS（fail-fast 行为由 catalog 的 default 错误承接）。

> **批次 0 范围说明**：spec 第 62 行的「全变体兜底」指任何协议变体在 catalog 都有处理所——已知 client/server 变体显式映射，未知变体落入 `default` fail-fast。批次 0 不新增对 `file_search` 等的 `KindUnsupported` 显式登记项（它们已由 default 覆盖）；如需显式登记（便于矩阵审计），留待后续批次按 spec 第 62 行扩展，不阻塞本批合并。

- [ ] **Step 3: 全量门禁**

Run: `task check`
Expected: fmt + go vet + 全量 test 全绿。

Run: `task test-race`
Expected: 无 data race。

- [ ] **Step 4: 确认 protocol-coverage 状态（批次 0 不升级）**

批次 0 是纯重构，`docs/protocol-coverage.md` 中 hosted tools 行的状态**不变**（`code_interpreter` 仍 `deferred`、`mcp` 仍 `unsupported_by_backend`、`web_search` 仍 `supported`）。打开 `docs/protocol-coverage.md`，确认 hosted tools 相关行的状态与「状态升级清单」（spec 第 176-187 行）的「现」列一致——批次 0 不应改动这些行。若发现批次 0 期间有误改，还原。

- [ ] **Step 5: Commit（若有文档微调）**

```bash
git add docs/protocol-coverage.md  # 仅当 Step 4 发现并修复了误改
git commit -m "docs(protocol-coverage): 确认批次 0 不升级 hosted tools 状态" || echo "无改动，跳过"
```

---

## Self-Review

**1. Spec 覆盖**（对照 spec 第 50-71 行 catalog 四维度 + 第 191 行批次 0 范围）：

- **身份 identity**（spec 56）：Task 1 `Identity` + `Inspect`，Task 3 迁移 `declaredToolIdentities` / `formatToolNames` / `FreeformToolNames`。✓
- **类别 kind**（spec 57）：Task 1 `Kind` 常量（`KindClientTool` / `KindServerTool` / `KindUnsupported`）。✓
- **声明映射 mapDecl**（spec 58）：Task 2 `Declare`，迁移 `appendToolUnion` / `appendWebSearchTool`。✓
- **回灌 mapReplay**（spec 59）：命名经 `ToolName`（Task 2）统一；回灌函数（`appendFunctionCall` 等）本身无 Of* 散落 switch，无需重写 dispatch——其 `toolName(...)` 调用已改 `toolcatalog.ToolName(...)`（Task 2 Step 5d）。✓
- **流式映射 mapStream**（spec 60）：Task 6 `ServerToolByAnthropicName` 驱动 `handleServerToolUseStart`，web_search handler 保留。✓
- **全变体兜底**（spec 62）：Task 7 Step 2 确认未知变体经 `Inspect`/`Declare` default fail-fast。✓
- **web_search 作 serverTool 首例**（spec 74）：Task 2 声明 + Task 5 注册 + Task 6 dispatch。✓
- **行为不变**（spec 70/191）：每个迁移 task 以既有测试为安全网，Task 7 全量门禁。✓

**2. 占位符扫描**：无 TBD / TODO / 「类似上文」/ 无代码的描述性步骤；declare_test.go 直接使用 `oparam.NewOpt`，无占位辅助函数。✓

**3. 类型一致性**：
- `Identity` 方法跨 task 统一：`ConvertedName()` / `Equal()` / `String()`（Task 1 定义，Task 3 使用）。
- `Inspect` / `InspectAllowed` / `Declare` / `ClientTool` / `ServerToolByAnthropicName` / `ApplyCacheControl` / `ToolName` 签名跨 task 一致。
- `toolcatalog.Identity` 在 convert 全包替换 `toolIdentity`（Task 3 一次性完成，含 `parseAllowedToolIdentities` 返回类型，避免跨 task 编译断裂）。
- `applySpecificToolChoice` 签名 `want toolcatalog.Identity`，调用方 `convertToolChoice` 字面量匹配。✓

**遗留风险（非阻塞，已标注）**：Task 3 行为注意点——web_search + tool_choice 边角组合从 fail-fast 变为不报错。无既有测试断言旧报错，Task 7 全量验收兜底。

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-07-17-tool-catalog-refactor.md`. Two execution options:**

**1. Subagent-Driven (recommended)** - 每个 task 派发独立 subagent，task 间两阶段 review，快速迭代。

**2. Inline Execution** - 在当前会话用 executing-plans 批次执行，带检查点 review。

**Which approach?**

> 若选 Subagent-Driven：REQUIRED SUB-SKILL `superpowers:subagent-driven-development`（fresh subagent per task + 两阶段 review）。
> 若选 Inline Execution：REQUIRED SUB-SKILL `superpowers:executing-plans`（批次执行 + 检查点）。
