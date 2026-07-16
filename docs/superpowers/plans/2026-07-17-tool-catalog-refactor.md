# 通用 Tool Catalog 架构重构（批次 0）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立 tool 处理通用 catalog（身份/声明/回灌/流式四维度 + 全变体兜底），把现有所有 tool 迁移进去，请求侧与回程侧 dispatch 全走 catalog，协议行为零变化。

**Architecture:** 新建 `internal/toolcatalog` 包作为 tool 行为的单一事实来源。每个 tool 注册 `identity` / `kind` / `mapDecl` / `mapReplay`；回程因依赖 converter 流式状态机，catalog 提供 block→handler 的 dispatch 表、handler 实现留在 `streamconv`。`internal/convert`（请求侧）与 `internal/streamconv`（回程侧）改为遍历 catalog 查询，消除 6+ 处散落 switch。所有 SDK 变体（Tool Union 17 / Input Item 31 / Tool Choice 9 / Anthropic 回程 block 全集）在 catalog 登记兜底，不再有散落 default。

**Tech Stack:** Go; `github.com/anthropics/anthropic-sdk-go@v1.57.0`; `github.com/openai/openai-go/v3@v3.42.0`

## Global Constraints

- **行为不变是唯一验收标准**：现有测试全程 GREEN——`internal/convert`（62+8）、`internal/streamconv`（33）、`internal/server` integration（23）。每个迁移步骤后必须 `go test ./...` 通过。
- 本计划是**纯重构**：不新增协议行为、不新增对外能力。新增的只有 `toolcatalog` 包自身的结构性单测。
- AGENTS.md 静默跳过约定：任何丢弃上游数据的分支必须 WARN 级结构化日志。
- 提交门禁 `task check`（gofmt + govet + golangci-lint + go test）。涉及状态机的 Task 6 额外跑 `task test-race`。
- Conventional Commits（`refactor(convert): ...` / `refactor(streamconv): ...` / `feat(toolcatalog): ...`）。
- 允许重写 `request.go` / `converter.go` 结构，判断标准是"catalog 更清晰 + 测试更直接"，而非"兼容旧形状"。
- 现有 spec：`docs/superpowers/specs/2026-07-17-hosted-tools-mapping-design.md`。

## 文件结构

- Create `internal/toolcatalog/catalog.go` — `Kind`/`Identity`/`Decl`/`Spec`/`Catalog` 类型，`Default()` 注册表，`Lookup`/`LookupByType`/`FreeformNames` 查询。
- Create `internal/toolcatalog/declare.go` — 各 tool 的 `mapDecl` 实现（请求声明映射）。
- Create `internal/toolcatalog/replay.go` — 各 tool call/output 的 `mapReplay` 实现（input 回灌）。
- Create `internal/toolcatalog/stream.go` — 回程 Anthropic block → Responses 事件的 dispatch 表（handler 名 + 路由，handler 体留 `streamconv`）。
- Create `internal/toolcatalog/*_test.go` — catalog 自身结构性单测。
- Modify `internal/convert/request.go` — `appendToolUnion`/`declaredToolIdentities`/`parseAllowedToolIdentities`/`formatToolNames`/`setLastToolCacheControl`/`appendItem`/`convertToolChoice` 系列改为遍历 catalog。
- Modify `internal/convert/customtool.go` — `FreeformToolNames` 改为委托 `toolcatalog.FreeformNames`。
- Modify `internal/streamconv/converter.go` — `handleBlockStart`/`handleServerToolUseStart`/`handleToolUseStart`/`handleWebSearchResultStart`/`SetCustomToolNames` 改为走 catalog dispatch 表。

依赖方向：`toolcatalog` → `{anthropic-sdk, openai-go, internal/model}`；`convert`/`streamconv` → `toolcatalog`。无循环。

---

## Task 1: toolcatalog 包骨架与查询接口

**Files:**
- Create: `internal/toolcatalog/catalog.go`
- Create: `internal/toolcatalog/catalog_test.go`

**Interfaces:**
- Produces: `Kind`（`client|server|beta_server|unsupported`）、`Identity{Type,Name,Namespace}`（方法 `ConvertedName()`、`String()`）、`Decl{Tool *anthropic.ToolUnionParam, Beta *BetaInjection, Unsupported string}`、`Spec{OpenAIType, Kind, Freeform, Identify, Declare}`、`Catalog`、`Default() *Catalog`、`Lookup(t) (*Spec, Identity, bool)`、`LookupByType(string) (*Spec, bool)`。

- [ ] **Step 1: 写 catalog.go 类型与骨架**

```go
// Package toolcatalog is the single source of truth for how each OpenAI
// Responses tool maps to Anthropic across its lifecycle: identity, request
// declaration, input replay, and stream conversion. convert (request side)
// and streamconv (response side) dispatch through this catalog instead of
// scattered switches.
package toolcatalog

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// Kind 表征 tool 的执行模型，决定声明映射与回程处理方式。
type Kind string

const (
	KindClient      Kind = "client"      // → anthropic.ToolParam，由 Codex 客户端执行
	KindServer      Kind = "server"      // → 标准 server tool union，由 Anthropic 执行
	KindBetaServer  Kind = "beta_server" // → beta 注入（MCP），批次 B 填充
	KindUnsupported Kind = "unsupported" // fail-fast / raw_preserved（按 protocol-coverage 矩阵）
)

// Identity 是一个 tool 调用的身份，服务 tool_choice / allowed_tools / 命名。
type Identity struct {
	Type      string
	Name      string
	Namespace string
}

// ConvertedName 返回压平后的 Anthropic tool 名（namespace__name 或 name）。
func (i Identity) ConvertedName() string {
	if i.Namespace != "" {
		return i.Namespace + "__" + i.Name
	}
	return i.Name
}

func (i Identity) String() string {
	if i.Namespace != "" {
		return fmt.Sprintf("%s %q in namespace %q", i.Type, i.Name, i.Namespace)
	}
	return fmt.Sprintf("%s %q", i.Type, i.Name)
}

// Decl 是声明映射 mapDecl 的产物（互斥 sum type）。
type Decl struct {
	Tool        *anthropic.ToolUnionParam // KindClient / KindServer
	Beta        *BetaInjection            // KindBetaServer（批次 B）
	Unsupported string                    // KindUnsupported 的原因
}

// BetaInjection 由批次 B（MCP）填充。
type BetaInjection struct{}

// Spec 描述一种 OpenAI tool 的转换行为。批次 0 填充 Identify/Declare；
// mapReplay/mapStream 在后续 task 加入。
type Spec struct {
	OpenAIType string
	Kind       Kind
	Freeform   bool
	Identify   func(t oairesponses.ToolUnionParam) (Identity, bool)
	Declare    func(t oairesponses.ToolUnionParam) (Decl, error)
}

// Catalog 是已注册 Spec 的集合。
type Catalog struct {
	specs []Spec
}

// Lookup 按 OpenAI ToolUnionParam 变体查找处理它的 Spec 与解析出的身份。
func (c *Catalog) Lookup(t oairesponses.ToolUnionParam) (*Spec, Identity, bool) {
	for i := range c.specs {
		s := &c.specs[i]
		if s.Identify == nil {
			continue
		}
		if id, ok := s.Identify(t); ok {
			return s, id, true
		}
	}
	return nil, Identity{}, false
}

// LookupByType 按原始 type 字符串查找（用于 allowed_tools 的 raw map 解析）。
func (c *Catalog) LookupByType(typ string) (*Spec, bool) {
	for i := range c.specs {
		if c.specs[i].OpenAIType == typ {
			return &c.specs[i], true
		}
	}
	return nil, false
}
```

- [ ] **Step 2: 写 Default()（暂只占位，Task 2 填充真实 specs）**

在 `catalog.go` 末尾：

```go
// Default 返回内置 tool 集合的 catalog。specs 在 declare.go 注册。
func Default() *Catalog { return &Catalog{specs: defaultSpecs()} }

func defaultSpecs() []Spec { return nil } // Task 2 填充
```

- [ ] **Step 3: 写 catalog_test.go（查询行为单测）**

```go
package toolcatalog

import (
	"testing"

	oairesponses "github.com/openai/openai-go/v3/responses"
)

func TestEmptyCatalogLookupReturnsFalse(t *testing.T) {
	c := &Catalog{}
	_, _, ok := c.Lookup(oairesponses.ToolUnionParam{})
	if ok {
		t.Fatal("空 catalog 的 Lookup 必须返回 false")
	}
	if _, ok := c.LookupByType("function"); ok {
		t.Fatal("空 catalog 的 LookupByType 必须返回 false")
	}
}

func TestIdentityConvertedName(t *testing.T) {
	if got := (Identity{Name: "shell"}).ConvertedName(); got != "shell" {
		t.Fatalf("无 namespace 时 ConvertedName=%q，want shell", got)
	}
	if got := (Identity{Namespace: "ns", Name: "f"}).ConvertedName(); got != "ns__f" {
		t.Fatalf("有 namespace 时 ConvertedName=%q，want ns__f", got)
	}
}
```

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/toolcatalog/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/toolcatalog/
git commit -m "feat(toolcatalog): 引入 tool 行为 catalog 骨架与查询接口"
```

---

## Task 2: 注册请求侧 Spec（identity + mapDecl）

**Files:**
- Create: `internal/toolcatalog/declare.go`
- Modify: `internal/toolcatalog/catalog.go`（`defaultSpecs` 指向 `declare.go` 的列表）
- Create: `internal/toolcatalog/declare_test.go`

**Interfaces:**
- Consumes: Task 1 的 `Catalog`/`Spec`/`Identity`/`Decl`。
- Produces: `defaultSpecs() []Spec` 覆盖 function/custom/shell/local_shell/apply_patch/tool_search/namespace/web_search/web_search_preview；未覆盖变体（file_search/computer/image_generation/programmatic/computer_use_preview）走 Task 7 的兜底。

**映射数据（从现有 `internal/convert/request.go` 的 `appendToolUnion` 逐 case 提取，行为等价）：**

| OpenAI 变体 | Kind | FixedName | Declare 产出 |
|---|---|---|---|
| `OfFunction` | client | （字段 name） | `Tool{OfTool: ToolParam{Name, InputSchema: toInputSchema(parameters), Description?}}` |
| `OfCustom` | client（freeform） | （字段 name） | `Tool{OfTool: ToolParam{Name, InputSchema: freeformInputSchema(), Description?, Type: custom}}` |
| `OfShell` / `OfLocalShell` | client（freeform） | `shell` | `Tool{OfTool: ToolParam{Name:"shell", InputSchema: freeformInputSchema(), Type: custom}}` |
| `OfApplyPatch` | client（freeform） | `apply_patch` | `Tool{OfTool: ToolParam{Name:"apply_patch", InputSchema: applyPatchInputSchema(), Type: custom}}` |
| `OfToolSearch` | client | `tool_search` | `Tool{OfTool: ToolParam{Name:"tool_search", InputSchema: schemaFromAny(parameters), Description?}}` |
| `OfNamespace` | client（递归） | （子 tool 名带 namespace） | 对每个子 function/custom 递归产出 ToolParam（name=`ns__child`） |
| `OfWebSearch` | server | （server tool） | `Tool{OfWebSearchTool20250305: {AllowedDomains: filters.allowed_domains}}` |
| `OfWebSearchPreview` | server | （server tool） | `Tool{OfWebSearchTool20250305: {}}` |

- [ ] **Step 1: 写 declare.go——把 helper 与各 Spec 注册**

把 `request.go` 里这些 helper 原样搬入（或保留在 convert 但 catalog 引用；本步选择搬入 toolcatalog 以集中）：`toInputSchema`、`freeformInputSchema`、`applyPatchInputSchema`、`schemaFromAny`、`optionalString`、`toolName`（ns+name 拼接）。

```go
package toolcatalog

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	aparam "github.com/anthropics/anthropic-sdk-go/packages/param"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// defaultSpecs 是内置 tool 的注册表（单一事实来源）。
// 顺序无关：Identify 用变体指针判别，Lookup 线性扫描命中即返回。
func defaultSpecs() []Spec {
	return []Spec{
		functionSpec(), customSpec(), shellSpec(), localShellSpec(),
		applyPatchSpec(), toolSearchSpec(), namespaceSpec(),
		webSearchSpec(), webSearchPreviewSpec(),
	}
}

func functionSpec() Spec {
	return Spec{
		OpenAIType: "function", Kind: KindClient,
		Identify: func(t oairesponses.ToolUnionParam) (Identity, bool) {
			if t.OfFunction == nil {
				return Identity{}, false
			}
			return Identity{Type: "function", Name: t.OfFunction.Name}, true
		},
		Declare: func(t oairesponses.ToolUnionParam) (Decl, error) {
			fn := t.OfFunction
			return Decl{Tool: clientTool(fn.Name, toInputSchema(fn.Parameters), optionalString(fn.Description), false)}, nil
		},
	}
}

func customSpec() Spec {
	return Spec{
		OpenAIType: "custom", Kind: KindClient, Freeform: true,
		Identify: func(t oairesponses.ToolUnionParam) (Identity, bool) {
			if t.OfCustom == nil {
				return Identity{}, false
			}
			return Identity{Type: "custom", Name: t.OfCustom.Name}, true
		},
		Declare: func(t oairesponses.ToolUnionParam) (Decl, error) {
			c := t.OfCustom
			return Decl{Tool: clientTool(c.Name, freeformInputSchema(), optionalString(c.Description), true)}, nil
		},
	}
}

func shellSpec() Spec { return freeformFixedSpec("shell", func(t oairesponses.ToolUnionParam) bool { return t.OfShell != nil }) }
func localShellSpec() Spec {
	return freeformFixedSpec("shell", func(t oairesponses.ToolUnionParam) bool { return t.OfLocalShell != nil })
}
func applyPatchSpec() Spec {
	return Spec{
		OpenAIType: "apply_patch", Kind: KindClient, Freeform: true,
		Identify: fixedIdentify("apply_patch", func(t oairesponses.ToolUnionParam) bool { return t.OfApplyPatch != nil }),
		Declare: func(t oairesponses.ToolUnionParam) (Decl, error) {
			return Decl{Tool: clientTool("apply_patch", applyPatchInputSchema(), nil, true)}, nil
		},
	}
}
func toolSearchSpec() Spec {
	return Spec{
		OpenAIType: "tool_search", Kind: KindClient,
		Identify: func(t oairesponses.ToolUnionParam) (Identity, bool) {
			if t.OfToolSearch == nil {
				return Identity{}, false
			}
			return Identity{Type: "tool_search", Name: "tool_search"}, true
		},
		Declare: func(t oairesponses.ToolUnionParam) (Decl, error) {
			s := t.OfToolSearch
			return Decl{Tool: clientTool("tool_search", schemaFromAny(s.Parameters), optionalString(s.Description), false)}, nil
		},
	}
}

func namespaceSpec() Spec {
	return Spec{
		OpenAIType: "namespace", Kind: KindClient,
		// namespace 递归：Identify 命中变体，Declare 对每个子 tool 产出 ns__name。
		// 因一个 namespace 含多个子 tool，Declare 返回首工具并在 convert 侧用
		// NamespaceDecls 辅助产出完整列表（见 declare.go）。
		Identify: func(t oairesponses.ToolUnionParam) (Identity, bool) {
			if t.OfNamespace == nil {
				return Identity{}, false
			}
			return Identity{Type: "namespace", Name: t.OfNamespace.Name}, true
		},
		Declare: func(t oairesponses.ToolUnionParam) (Decl, error) {
			// 实际多产出由 NamespaceDecls 处理；单 Spec.Declare 此处不直接用。
			return Decl{}, fmt.Errorf("namespace use NamespaceDecls")
		},
	}
}

func webSearchSpec() Spec {
	return Spec{
		OpenAIType: "web_search", Kind: KindServer,
		Identify: func(t oairesponses.ToolUnionParam) (Identity, bool) {
			if t.OfWebSearch == nil {
				return Identity{}, false
			}
			return Identity{Type: "web_search"}, true
		},
		Declare: func(t oairesponses.ToolUnionParam) (Decl, error) {
			return Decl{Tool: &anthropic.ToolUnionParam{
				OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
					AllowedDomains: t.OfWebSearch.Filters.AllowedDomains,
				},
			}}, nil
		},
	}
}

func webSearchPreviewSpec() Spec {
	return Spec{
		OpenAIType: "web_search_preview", Kind: KindServer,
		Identify: func(t oairesponses.ToolUnionParam) (Identity, bool) {
			if t.OfWebSearchPreview == nil {
				return Identity{}, false
			}
			return Identity{Type: "web_search_preview"}, true
		},
		Declare: func(t oairesponses.ToolUnionParam) (Decl, error) {
			return Decl{Tool: &anthropic.ToolUnionParam{
				OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{},
			}}, nil
		},
	}
}

// fixedIdentify 构造固定名 tool 的 Identify（shell/apply_patch/tool_search 等）。
func fixedIdentify(name string, match func(oairesponses.ToolUnionParam) bool) func(oairesponses.ToolUnionParam) (Identity, bool) {
	return func(t oairesponses.ToolUnionParam) (Identity, bool) {
		if !match(t) {
			return Identity{}, false
		}
		return Identity{Type: name, Name: name}, true
	}
}

func freeformFixedSpec(name string, match func(oairesponses.ToolUnionParam) bool) Spec {
	return Spec{
		OpenAIType: name, Kind: KindClient, Freeform: true,
		Identify: fixedIdentify(name, match),
		Declare: func(oairesponses.ToolUnionParam) (Decl, error) {
			return Decl{Tool: clientTool(name, freeformInputSchema(), nil, true)}, nil
		},
	}
}

// NamespaceDecls 展开一个 namespace tool 的全部子工具声明。
func NamespaceDecls(t oairesponses.ToolUnionParam) ([]*anthropic.ToolUnionParam, error) {
	if t.OfNamespace == nil {
		return nil, nil
	}
	ns := t.OfNamespace.Name
	var out []*anthropic.ToolUnionParam
	for _, nested := range t.OfNamespace.Tools {
		switch {
		case nested.OfFunction != nil:
			fn := nested.OfFunction
			out = append(out, clientTool(toolName(ns, fn.Name), toInputSchema(fn.Parameters), optionalString(fn.Description), false))
		case nested.OfCustom != nil:
			c := nested.OfCustom
			out = append(out, clientTool(toolName(ns, c.Name), freeformInputSchema(), optionalString(c.Description), true))
		default:
			return nil, fmt.Errorf("unsupported namespace tool: Anthropic backend has no safe equivalent")
		}
	}
	return out, nil
}

// clientTool 构造一个 Anthropic 普通 ToolParam。
func clientTool(name string, schema anthropic.ToolInputSchemaParam, desc *string, custom bool) *anthropic.ToolUnionParam {
	tool := &anthropic.ToolParam{Name: name, InputSchema: schema}
	if desc != nil {
		tool.Description = aparam.NewOpt(*desc)
	}
	if custom {
		tool.Type = anthropic.ToolTypeCustom
	}
	return &anthropic.ToolUnionParam{OfTool: tool}
}

// —— 从 convert/request.go 原样迁入的 helper（保持行为等价） ——
func toolName(namespace, name string) string {
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
func schemaFromAny(v any) map[string]any { s, _ := v.(map[string]any); return s }
func freeformInputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}}
}
func applyPatchInputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"operation": map[string]any{"type": "string", "enum": []string{"create_file", "delete_file", "update_file"}},
		"path":      map[string]any{"type": "string"},
		"diff":      map[string]any{"type": "string"},
	}, "required": []string{"operation", "path"}}
}
func toInputSchema(schema map[string]any) anthropic.ToolInputSchemaParam {
	props, _ := schema["properties"].(map[string]any)
	var required []string
	switch r := schema["required"].(type) {
	case []string:
		required = r
	case []any:
		for _, item := range r {
			if s, ok := item.(string); ok {
				required = append(required, s)
			}
		}
	}
	return anthropic.ToolInputSchemaParam{Properties: props, Required: required}
}
```

- [ ] **Step 2: 写 declare_test.go（每 tool 的 Declare 产出断言，对照现有 `request_test.go` 的期望）**

对 function/custom/shell/local_shell/apply_patch/tool_search/web_search/web_search_preview 各写一个用例：构造 `ToolUnionParam` 变体 → `Default().Lookup` → `Declare` → 断言产出的 `ToolUnionParam` 变体与字段。namespace 用 `NamespaceDecls` 断言子工具展开。这些断言值直接抄自现有 `TestWebSearchToolMapsToAnthropicServerTool` / `TestCustomToolNotDropped` 等的期望。

- [ ] **Step 3: 运行测试**

Run: `go test ./internal/toolcatalog/`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/toolcatalog/
git commit -m "feat(toolcatalog): 注册请求侧 tool Spec（identity + mapDecl）"
```

---

## Task 3: convert 请求侧声明 dispatch 迁移

**Files:**
- Modify: `internal/convert/request.go`（`appendToolUnion` 604-646、`appendWebSearchTool` 654-661、`appendConvertedTool`/`hasTool` 辅助、`declaredToolIdentities` 910-943、`formatToolNames` 1145-1175、`setLastToolCacheControl` 1102-1115）
- Modify: `internal/convert/customtool.go`（`FreeformToolNames` 9-28 改为委托 catalog）

**Interfaces:**
- Consumes: `toolcatalog.Default()`、`Spec.Declare`、`Spec.Identify`、`toolcatalog.NamespaceDecls`、`toolcatalog.FreeformNames`。

- [ ] **Step 1: 改写 `appendToolUnion` 走 catalog**

替换 `request.go:604-646` 的整个 switch：

```go
func appendToolUnion(out *anthropic.MessageNewParams, t oairesponses.ToolUnionParam) error {
	cat := toolcatalog.Default()
	// namespace 展开为多个声明
	if decls, err := toolcatalog.NamespaceDecls(t); err != nil {
		return err
	} else if decls != nil {
		for _, d := range decls {
			if err := appendAnthropicTool(out, d); err != nil {
				return err
			}
		}
		return nil
	}
	spec, id, ok := cat.Lookup(t)
	if !ok {
		return fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", toolType(t))
	}
	if spec.Kind == toolcatalog.KindUnsupported {
		return fmt.Errorf("unsupported tool type %q: %s", id.Type, "Anthropic backend has no safe equivalent")
	}
	decl, err := spec.Declare(t)
	if err != nil {
		return err
	}
	if decl.Tool != nil {
		return appendAnthropicTool(out, decl.Tool)
	}
	if decl.Unsupported != "" {
		return fmt.Errorf("unsupported tool %q: %s", id.Type, decl.Unsupported)
	}
	return fmt.Errorf("tool %q produced no declaration", id.Type)
}

// appendAnthropicTool 把一个已构造的 Anthropic tool union 加入 out.Tools，
// 带名称冲突校验（替代旧 appendConvertedTool 的 hasTool 逻辑）。
func appendAnthropicTool(out *anthropic.MessageNewParams, t *anthropic.ToolUnionParam) error {
	if t.OfTool != nil {
		if hasTool(out, t.OfTool.Name) {
			return fmt.Errorf("tool conversion name conflict for %q", t.OfTool.Name)
		}
	}
	out.Tools = append(out.Tools, *t)
	return nil
}
```

删除 `appendWebSearchTool`（逻辑已进 `webSearchSpec().Declare`）。保留 `hasTool`、`toolType`。

- [ ] **Step 2: 改写 `declaredToolIdentities` 走 catalog identity**

替换 `request.go:910-943`：

```go
func declaredToolIdentities(tools []oairesponses.ToolUnionParam) ([]toolcatalog.Identity, error) {
	cat := toolcatalog.Default()
	identities := make([]toolcatalog.Identity, 0, len(tools))
	for _, tool := range tools {
		if tool.OfNamespace != nil {
			for _, nested := range tool.OfNamespace.Tools {
				var id toolcatalog.Identity
				switch {
				case nested.OfFunction != nil:
					id = toolcatalog.Identity{Type: "function", Namespace: tool.OfNamespace.Name, Name: nested.OfFunction.Name}
				case nested.OfCustom != nil:
					id = toolcatalog.Identity{Type: "custom", Namespace: tool.OfNamespace.Name, Name: nested.OfCustom.Name}
				default:
					return nil, fmt.Errorf("unsupported namespace tool: Anthropic backend has no safe equivalent")
				}
				identities = append(identities, id)
			}
			continue
		}
		spec, id, ok := cat.Lookup(tool)
		if !ok {
			return nil, fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", toolType(tool))
		}
		identities = append(identities, id)
	}
	return identities, nil
}
```

把 `toolIdentity` 类型别名化或替换为 `toolcatalog.Identity`（更新 `applySpecificToolChoice`/`hasToolIdentity`/`convertedName` 等引用——这些在 Task 4 处理；本步先保留 `toolIdentity` 为 `type toolIdentity = toolcatalog.Identity` 别名以最小改动）。

- [ ] **Step 3: 改写 `formatToolNames` 与 `setLastToolCacheControl` 走 catalog**

`formatToolNames`（1145-1175）：遍历 tools，用 `cat.Lookup` 取 identity，收集 `id.ConvertedName()`（namespace 用 `toolcatalog.NamespaceDecls` 同款的 ns__name 逻辑，可抽 `namespaceChildIdentities` 辅助）。default 分支不再出现（未注册变体由 Lookup 返回 false，跳过——但 `formatToolNames` 现有行为对未知 tool 静默不含，保持）。

`setLastToolCacheControl`（1102-1115）：保持现有按变体派发（`OfTool`/`OfWebSearchTool20250305`）——这是对**产出后**的 Anthropic tool union 加 cache_control，与 catalog 的 OpenAI→Anthropic 方向无关，保留原样即可（不算散落 switch 的目标）。

- [ ] **Step 4: 改写 `FreeformToolNames` 委托 catalog**

`customtool.go`：把 `appendFreeformToolName` 的变体判别改为查 `cat.Lookup(t).Freeform`：

```go
func FreeformToolNames(req *oairesponses.ResponseNewParams) []string {
	cat := toolcatalog.Default()
	var names []string
	collect := func(tools []oairesponses.ToolUnionParam) {
		for i := range tools {
			t := tools[i]
			if t.OfNamespace != nil {
				for _, nested := range t.OfNamespace.Tools {
					if spec, id, ok := cat.Lookup(nested); ok && spec.Freeform {
						names = append(names, id.ConvertedName())
					}
				}
				continue
			}
			if spec, id, ok := cat.Lookup(t); ok && spec.Freeform {
				names = append(names, id.ConvertedName())
			}
		}
	}
	collect(req.Tools)
	for i := range req.Input.OfInputItemList {
		item := req.Input.OfInputItemList[i]
		if item.OfAdditionalTools != nil {
			collect(item.OfAdditionalTools.Tools)
		}
		if item.OfToolSearchOutput != nil {
			collect(item.OfToolSearchOutput.Tools)
		}
	}
	return names
}
```

删除 `appendFreeformToolName`。

- [ ] **Step 5: 运行测试（验收）**

Run: `go test ./internal/convert/...`
Expected: PASS（全部 62+8 用例 GREEN，证明声明/identity/freeform 行为不变）

- [ ] **Step 6: Commit**

```bash
git add internal/convert/ internal/toolcatalog/
git commit -m "refactor(convert): 请求侧 tool 声明/identity/freeform 改走 catalog"
```

---

## Task 4: tool_choice / allowed_tools 迁移

**Files:**
- Modify: `internal/convert/request.go`（`convertToolChoice` 768-831、`parseAllowedToolIdentities` 974-997、`parseAllowedNamespaceToolIdentities` 999-1025、`applySpecificToolChoice` 849-861、`toolIdentity` 954-972、`hasToolIdentity` 945-952）

**Interfaces:**
- Consumes: `toolcatalog.Identity`、`toolcatalog.Default().LookupByType`。

- [ ] **Step 1: 用 `toolcatalog.Identity` 替换本地 `toolIdentity`**

删除 `toolIdentity` 结构（954-972），全局替换为 `toolcatalog.Identity`。`hasToolIdentity` 改为对 `[]toolcatalog.Identity` 比较（`Identity` 已是值类型，`==` 可用）。`applySpecificToolChoice` 里 `want toolcatalog.Identity{...}`、`want.convertedName()` → `want.ConvertedName()`。

- [ ] **Step 2: `parseAllowedToolIdentities` 用 `LookupByType`**

替换 974-997：对 raw map 的 `type` 字段，`cat.LookupByType(typ)` 判定是否已知；`shell`/`local_shell` 映射到 `Identity{Type:typ, Name:"shell"}`（保持现有特例），`apply_patch`/`tool_search` 同理，`function`/`custom` 取 name，`namespace` 走 `parseAllowedNamespaceToolIdentities`，未知 type 报 "no safe equivalent"（与现有一致）。

注：`LookupByType` 命中只代表"已知 tool 类型"，仍需按现有逻辑校验 name/namespace——本步是把"已知类型集合"的判定收敛到 catalog，分支语义不变。

- [ ] **Step 3: 运行测试**

Run: `go test ./internal/convert/...`
Expected: PASS（`TestAllowedTools*`、`TestSpecificToolChoice*`、`TestToolChoice*` 全 GREEN）

- [ ] **Step 4: Commit**

```bash
git add internal/convert/
git commit -m "refactor(convert): tool_choice/allowed_tools 走 catalog identity"
```

---

## Task 5: 请求侧回灌（mapReplay）迁移

**Files:**
- Create: `internal/toolcatalog/replay.go`
- Create: `internal/toolcatalog/replay_test.go`
- Modify: `internal/convert/request.go`（`appendItem` 195-270 及其 `append*Call`/`append*Output` 系列 409-533）

**Interfaces:**
- Produces: catalog 的 replay 注册——按 OpenAI Input Item 变体（`OfFunctionCall`/`OfCustomToolCall`/`OfShellCall`/`OfLocalShellCall`/`OfApplyPatchCall`/`OfToolSearchCall` 及各自 Output、`OfWebSearchCall`）映射到 Anthropic 历史 `tool_use`+`tool_result`。

- [ ] **Step 1: 在 catalog 引入 replay 维度**

在 `catalog.go` 的 `Spec` 增加（replay 与声明不同维度——call/output 是 input item，按变体 dispatch）：

```go
// Replay 把一个 OpenAI input call/output item 回放为 Anthropic 历史 block。
// 返回 (appendToLastAssistant, appendToUserToolResult) 两类副作用。
type Replay struct {
	AppendToolUse   func(out *anthropic.MessageNewParams) error   // call → assistant tool_use
	AppendToolResult func(out *anthropic.MessageNewParams) error  // output → user tool_result
}
```

为避免 Spec 膨胀，replay 用独立 dispatch（按 input item 变体），而非挂在 Spec 上。在 `replay.go` 提供：

```go
// ReplayCallItem 把一个 input call item 回放为 Anthropic 历史 tool_use。
// 未识别变体返回 (false, nil) 由调用方走 unknownInputItemPart。
func ReplayCallItem(out *anthropic.MessageNewParams, item *oairesponses.ResponseInputItemUnionParam) (handled bool, err error)
// ReplayOutputItem 把一个 input output item 回放为 Anthropic 历史 tool_result。
func ReplayOutputItem(out *anthropic.MessageNewParams, item *oairesponses.ResponseInputItemUnionParam) (handled bool, err error)
```

实现体从 `request.go` 的 `appendFunctionCall`/`appendCustomToolCall`/`appendShellCall`/`appendLocalShellCall`/`appendApplyPatchCall`/`appendToolSearchCall`（→ tool_use）与 `appendFunctionCallOutput`/`appendCustomToolCallOutput`/`shellOutputText`+`appendToolResult`/apply_patch output/local_shell output（→ tool_result）**原样搬入** `replay.go`，`appendItem` 改为调用 `ReplayCallItem`/`ReplayOutputItem`。

- [ ] **Step 2: `appendItem` 改为 catalog dispatch**

`appendItem`（195-270）的 `OfFunctionCall`/`OfCustomToolCall`/`OfLocalShellCall`/`OfShellCall`/`OfApplyPatchCall` 及各自 Output 分支，替换为：

```go
if handled, err := toolcatalog.ReplayCallItem(out, item); err != nil {
	return err
} else if handled {
	return nil
}
if handled, err := toolcatalog.ReplayOutputItem(out, item); err != nil {
	return err
} else if handled {
	return nil
}
```

保留 `OfMessage`/`OfReasoning`/`OfToolSearchCall`/`OfToolSearchOutput`/`OfAdditionalTools`/`OfCompaction`/`OfCompactionTrigger`/unknown 在 `appendItem`（这些非 tool call/output，或已有专属处理；本步只迁移纯 tool call/output 回灌）。`OfWebSearchCall`（hosted 无状态）本步保持现有行为（不产生副作用）。

- [ ] **Step 3: 运行测试**

Run: `go test ./internal/convert/...`
Expected: PASS（`TestToolCallsConvert`、`TestCustomToolCallInputAndOutputConvert`、`TestShellCallInputItemConvertsToShellToolUse`、`TestShellAndApplyPatchOutputsConvertToToolResults` 等全 GREEN）

- [ ] **Step 4: Commit**

```bash
git add internal/toolcatalog/ internal/convert/
git commit -m "refactor(convert): tool call/output 回灌迁移进 catalog mapReplay"
```

---

## Task 6: 回程流式 dispatch 迁移

**Files:**
- Create: `internal/toolcatalog/stream.go`
- Create: `internal/toolcatalog/stream_test.go`
- Modify: `internal/streamconv/converter.go`（`handleBlockStart` 255-287、`handleServerToolUseStart` 400-433、`handleWebSearchResultStart` 435-455、`handleToolUseStart` 372-398、`SetCustomToolNames` 197-206）

**Interfaces:**
- Produces: `toolcatalog.ServerToolNames()`（返回已知 server tool 名集合，如 `{"web_search"}`，批次 A 加 `code_execution`）、`toolcatalog.BlockRouter`（block type → 是否跳过/路由到 handler 的声明）。
- 注：回程 handler 体留在 `streamconv`（依赖 converter 状态机：`itemOrder`/`seq`/`outputItems`/`webSearchByToolUseID`）。catalog 只提供"哪些 block 类型已知、哪些 server tool 名受支持、custom/freeform 名集合从哪来"的声明，消除 `if name != "web_search"` 这类硬编码。

- [ ] **Step 1: catalog 提供 server tool 名与 block 分类声明**

`stream.go`：

```go
package toolcatalog

// ServerToolNames 返回 catalog 中已知会以 server_tool_use 形式出现的 tool 名。
// streamconv 据此判定 server_tool_use block 是否可映射（其余跳过）。
func (c *Catalog) ServerToolNames() map[string]Kind {
	out := map[string]Kind{}
	for i := range c.specs {
		if c.specs[i].Kind == KindServer || c.specs[i].Kind == KindBetaServer {
			// server tool 在 Anthropic 侧的 name（web_search / code_execution / ...）
			// 由各 Spec 额外字段提供；批次 0 只有 web_search，硬映射在此。
		}
	}
	out["web_search"] = KindServer
	return out
}
```

（批次 A 接入 code_interpreter 时在此加 `out["code_execution"] = KindServer`。）

- [ ] **Step 2: converter 用 catalog 判定 server tool，消除硬编码 `name != "web_search"`**

`converter.go` `New()` 持有 `serverTools map[string]struct{}`（从 `toolcatalog.Default().ServerToolNames()` 派生）。`handleServerToolUseStart`（400-433）：

```go
func (c *Converter) handleServerToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	name := string(ev.ContentBlock.Name)
	switch name {
	case "web_search":
		return c.handleWebSearchServerToolUseStart(ev)
	default:
		return c.handleSkippedServerToolUseStart(ev)
	}
}
```

（保留显式 `web_search` 分支，因其事件序列特定；但"哪些 server tool 受支持"的真相在 catalog `ServerToolNames()`。若 `ServerToolNames()` 不含该名 → 走 skipped，与现有 `if name != "web_search"` 等价。可用 `if _, ok := c.serverTools[name]; !ok { return c.handleSkippedServerToolUseStart(ev) }` 后再 `switch name`。）

`handleBlockStart`（255-287）的 `anBlockCodeExecutionToolResult` 等跳过分支保持（Task 7 兜底确认）。

- [ ] **Step 3: `SetCustomToolNames` 改由 catalog 派生 freeform 名**

`SetCustomToolNames`（197-206）当前由 server 侧传入 `FreeformToolNames(req)` 的结果。保持调用链不变（server → converter.SetCustomToolNames），但 `FreeformToolNames` 已在 Task 3 改为 catalog 派生——本步仅确认 `customToolNames` 初值 `{"apply_patch":true,"shell":true}` 与 catalog freeform 集合一致（catalog 里 apply_patch/shell/local_shell/custom 均 Freeform=true）。把 `New()` 的硬编码初值改为从 catalog 派生：

```go
customToolNames: toolcatalog.Default().FreeformFixedNames(), // {"apply_patch","shell"}
```

在 `catalog.go` 加 `FreeformFixedNames() map[string]bool`（遍历 specs，Freeform 且有 FixedName 的收集）。

- [ ] **Step 4: 运行测试（含 race）**

Run: `go test ./internal/streamconv/... && go test -race ./internal/streamconv/...`
Expected: PASS（`TestWebSearchServerToolUseEmitsWebSearchCall`、`TestWebSearchResultSurfacesSources`、`TestNonWebSearchServerToolUseSkippedNotFailed`、`TestToolResultBlockSkippedNotFailed`、`TestConverterOutputItemsFunctionCall`、`TestConverterOutputItemsCustomToolCall` 全 GREEN）

Run: `go test ./internal/server/...`（integration，含 `TestIntegrationWebSearchRoundTrip`、`TestIntegrationCustomToolStream`）
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/toolcatalog/ internal/streamconv/
git commit -m "refactor(streamconv): 回程 server tool/custom 名判定走 catalog"
```

---

## Task 7: 全变体兜底 + 全量验收 + 文档同步

**Files:**
- Modify: `internal/toolcatalog/declare.go`（为不支持的 hosted tool 补 `unsupported` Spec 或确认 `Lookup` 返回 false 的统一处理）
- Modify: `internal/convert/request.go`（确认无散落 default 报错之外的分支）
- Modify: `docs/protocol-coverage.md`（批次 0 说明：架构重构，状态不变）
- Modify: `README.md`（如架构说明需更新）

- [ ] **Step 1: 确认未支持 hosted tool 的兜底统一**

grep 确认 `file_search`/`computer`/`computer_use_preview`/`image_generation`/`programmatic_tool_calling` 及其 input item（`OfFileSearchCall`/`OfComputerCall`/`OfImageGenerationCall`/`OfProgram`/`OfProgramOutput`）在 catalog `Lookup` 返回 false → `appendToolUnion` 走 "unsupported tool type" 报错（Task 3 已实现）、`appendItem` 走 `unknownInputItemPart`（raw_preserved）。

如希望显式登记（而非依赖 Lookup false），为每个加 `unsupportedSpec("file_search")` 等注册到 `defaultSpecs`，`Kind: KindUnsupported`，`Declare` 返回 `Decl{Unsupported: "..."}`。本步选择**显式登记**（兑现 spec 的"全变体兜底，无散落 default"）：

```go
func unsupportedSpec(openaiType string) Spec {
	return Spec{
		OpenAIType: openaiType, Kind: KindUnsupported,
		Identify: func(t oairesponses.ToolUnionParam) (Identity, bool) {
			if !hasVariant(t, openaiType) {
				return Identity{}, false
			}
			return Identity{Type: openaiType}, true
		},
		Declare: func(oairesponses.ToolUnionParam) (Decl, error) {
			return Decl{Unsupported: "Anthropic backend has no safe equivalent"}, nil
		},
	}
}
```

`hasVariant(t, "file_search")` 用 `t.OfFileSearch != nil` 等（逐一）。在 `defaultSpecs` 追加 `unsupportedSpec("file_search")`、`unsupportedSpec("computer")`、`unsupportedSpec("computer_use_preview")`、`unsupportedSpec("image_generation")`、`unsupportedSpec("programmatic_tool_calling")`。

- [ ] **Step 2: 确认 `appendToolUnion` 对 unsupported Spec 的报错文案与现有 `TestUnsupportedToolDefinitionReturnsError` 一致**

现有测试期望报错含 "no safe equivalent"——Task 3 的 `appendAnthropicTool` 路径与 Task 7 的 `Decl.Unsupported` 都含此串。运行 `TestUnsupportedToolDefinitionReturnsError` 确认 GREEN。

- [ ] **Step 3: 全量验收**

Run: `task check`
Expected: gofmt/vet/golangci/test 全 PASS

Run: `task test-race`
Expected: 全 PASS（含 integration 23 用例）

- [ ] **Step 4: 文档同步**

`docs/protocol-coverage.md`：在"资料来源"后加一段批次 0 说明——"tool 处理已统一到 `internal/toolcatalog`，本批为架构重构，各 tool 状态不变；后续 hosted tool（code interpreter/MCP）接入以 catalog 注册形式进行。" 各矩阵行状态不改。

`README.md`：若"项目结构"提到 `internal/convert`，补一句 tool 行为集中在 `internal/toolcatalog`。

- [ ] **Step 5: Commit**

```bash
git add internal/toolcatalog/ internal/convert/ docs/protocol-coverage.md README.md
git commit -m "refactor(toolcatalog): 全变体兜底登记 + 批次 0 文档同步"
```

---

## Self-Review

**1. Spec 覆盖**：
- catalog 四维度（identity/declare/replay/stream）：Task 1（骨架）、Task 2（declare）、Task 5（replay）、Task 6（stream dispatch）。✓
- 全变体兜底：Task 7。✓
- web_search 统一重构：Task 2（declare）+ Task 6（stream）。✓
- 行为不变：每个 Task 的验收 = 对应现有测试 GREEN。✓
- 衍生项（bash/text_editor_code_execution）保持 deferred：Task 6 维持跳过分支。✓

**2. Placeholder 扫描**：catalog 新代码（Task 1/2/5）给完整 Go；迁移 Task（3/4/5/6）给目标 dispatch 代码 + 定位（行号）+ 验证测试名；映射数据表完整。无 "TBD/handle edge cases"。Task 6 的 server tool 名映射因批次 0 仅 web_search，给出显式分支 + catalog 真相源说明，非占位。

**3. 类型一致性**：`toolcatalog.Identity`（Task 1）→ Task 4 替换 `toolIdentity`、Task 3 `declaredToolIdentities` 返回 `[]toolcatalog.Identity`、`ConvertedName()`/`String()` 方法名一致。`Decl{Tool, Beta, Unsupported}`（Task 1）→ Task 2/3/7 使用一致。`Spec{Identify, Declare}` 签名全链一致。

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-17-tool-catalog-refactor.md`. Two execution options:

1. **Subagent-Driven (recommended)** — 每个 Task 派发独立 subagent，Task 间 review，快速迭代。
2. **Inline Execution** — 本会话内用 executing-plans 批量执行，检查点 review。

哪种？
