# MCP → Managed MCP Connector 映射（批次 B）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 OpenAI `mcp` hosted tool 双向映射到 Anthropic beta managed MCP connector（beta header + JSON 注入 + 回程 probe），状态 `unsupported_by_backend` → `lossy_supported`。

**Architecture:** MCP 是 beta 能力，标准 `MessageNewParams`/`ToolUnionParam` 无对应字段。方案：`convert.ToAnthropic` 额外产出 `anthropic.MCPInjection`（server 列表 + toolset），经 `scheduler` 的 `RequestBuilder` 管道透传到 `client.Stream`，后者 marshal 后用 JSON 注入 `mcp_servers`（顶层）+ `mcp_toolset`（tools[]）并追加 beta header `mcp-client-2025-11-20`。回程 `mcp_tool_use`/`mcp_tool_result` 是 beta block，标准 `MessageStreamEventUnion` 无 `Of*` 变体，由 `ScanEvents` 新增 probe 解析 raw payload 成合成事件，streamconv 消费为 `mcp_call` item。

**Tech Stack:** Go 1.26.5、anthropic-sdk-go `v1.57.0`（MCP 仅 beta）、openai-go/v3 `v3.42.0`。

## Global Constraints

- **依赖批次 0**：catalog 已存在，dispatch 走 catalog。本批在 catalog 注册 `mcp`。
- **依赖批次 A 无**：A/B 架构独立（spec 195 行）。本批不动 code interpreter 路径。
- **不动 SDK 类型链**：标准 `MessageNewParams` 顶层无 `mcp_servers`、`ToolUnionParam` 无 `OfMCPToolset`——用 `client.Stream` 的 JSON 注入（复用 `injectStream` 模式），不切到 beta SDK 类型（spec 147 行）。
- **TDD（新能力）**：每个映射先写 RED 测试证明当前 `unsupported_by_backend`（fail-fast 或 skip），再实现，再 GREEN。
- **信息损失必须登记**（spec 166-172）：`mcp_list_tools` 不回传；`require_approval≠never` 降级为 never + WARN；自定义 `headers` 仅 `Authorization: Bearer` 提取；`connector_id`/`tunnel_id` fail-fast。全部在 README + protocol-coverage 登记。
- **fail-fast 安全语义**（spec 145）：`require_approval=on_failure`/`if_referenced` → 降级 never + WARN（透明降级优于完全不可用）；网关不产 `mcp_approval_request`，客户端通过"未收到 approval、mcp_call 直接 completed"感知。
- **beta header 常量化**：`mcp-client-2025-11-20` 用命名常量，便于跟进版本（spec 172）。
- **测试命令**：单包 `go test ./internal/<pkg>/`；全量 `task check`；流式 `task test-race`。
- **提交风格**：Conventional Commits，可中文，如 `feat(client): 注入 beta MCP mcp_servers/mcp_toolset`。

---

## 文件结构

| 文件 | 改动 |
|---|---|
| `internal/model/constants.go` | 加 `ItemTypeMcpCall` 常量 |
| `internal/model/outputitem.go` | `OutputItem` 加 `ServerLabel` 字段；`MarshalJSON` 加 `mcp_call` 分支 |
| `internal/model/event.go` | 加 `McpCallEvent` / `McpCallArgumentsDeltaEvent` / `McpCallArgumentsDoneEvent` |
| `internal/toolcatalog/inspect.go` | `Inspect` 加 `OfMcp` case；加 `KindBetaServerTool` 常量 |
| `internal/anthropic/mcp.go`（新） | `MCPInjection`/`MCPServer`/`MCPToolset` 类型 + `injectMCP` 函数 |
| `internal/anthropic/client.go` | `Stream` 加 `mcp *MCPInjection` 参数 + JSON 注入 + beta header；`ScanEvents` 加 mcp probe |
| `internal/convert/request.go` | `ToAnthropic` 返回 `*MCPInjection`；新增 `collectMCP`（字段映射 + 损失 + fail-fast） |
| `internal/scheduler/scheduler.go` | `RequestBuilder`/`Execute`/`ExecutePrepared`/`tryRoundPrepared`/`trySource` 透传 `*MCPInjection` |
| `internal/server/server.go` | `RequestBuilder` 闭包（220 行）返回 `*MCPInjection` |
| `internal/streamconv/converter.go` | `mcp_call` 事件常量；`Converter` 加 `mcpCallByToolUseID`；`handleBlockStart` 加 mcp block 分支；`handleMcpToolUseStart`/`handleMcpToolResultStart` |
| `internal/convert/request.go`（回灌） | `appendItem` 加 `OfMcpCall` 回灌（beta JSON） |

依赖方向：`anthropic`（底层，MCPInjection 所在）← `convert` / `scheduler` / `streamconv`；无循环。

---

## Task B1: model 层 mcp_call 类型与事件

**Files:**
- Modify: `internal/model/constants.go`
- Modify: `internal/model/outputitem.go`
- Modify: `internal/model/event.go`
- Test: `internal/model/outputitem_test.go`

**Interfaces:**
- Consumes: `oaconstant.McpCall` / `ResponseMcpCall*`
- Produces: `ItemTypeMcpCall`、`OutputItem.ServerLabel`、3 个事件 struct

- [ ] **Step 1: 加常量**

`internal/model/constants.go` 的 `ItemTypeCodeInterpreterCall` 行（批次 A 加的）下加：

```go
	ItemTypeMcpCall = string(oaconstant.ValueOf[oaconstant.McpCall]())
```

- [ ] **Step 2: 写 model 测试（RED）**

`internal/model/outputitem_test.go` 加：

```go
func TestMcpCallItemMarshalsRequiredFields(t *testing.T) {
	item := OutputItem{
		Type: ItemTypeMcpCall, ID: "mcp_0", Status: ResponseStatusInProgress,
		ServerLabel: "weather", Name: "get_forecast", Arguments: `{"city":"sf"}`,
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for _, key := range []string{"type", "id", "server_label", "name", "arguments"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("mcp_call wire missing required key %q: %s", key, raw)
		}
	}
	if got["type"] != "mcp_call" {
		t.Fatalf("bad type: %v", got["type"])
	}
}
```

- [ ] **Step 3: 运行测试验证 RED**

Run: `go test ./internal/model/ -run TestMcpCall -v`
Expected: 编译失败（`ItemTypeMcpCall`/`ServerLabel` 未定义）。

- [ ] **Step 4: 加 OutputItem.ServerLabel 字段**

`internal/model/outputitem.go` 的 `OutputItem` struct（批次 A 加的 `Outputs` 字段后）加：

```go
	ServerLabel string `json:"server_label,omitempty"` // mcp_call
```

- [ ] **Step 5: 加 MarshalJSON 的 mcp_call 分支**

`internal/model/outputitem.go` 的 `MarshalJSON`（批次 A 加的 `code_interpreter_call` 分支后）插入：

```go
	if i.Type == ItemTypeMcpCall {
		// mcp_call failed 由 Status=failed 表达，错误文本并入 Output。
		// （OpenAI wire 的 error 字段为 nullable；本网关不单独产出 error 字段。）
		return json.Marshal(struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			Status      string `json:"status,omitempty"`
			ServerLabel string `json:"server_label"`
			Name        string `json:"name"`
			Arguments   string `json:"arguments"`
			Output      string `json:"output,omitempty"`
		}{
			Type: i.Type, ID: i.ID, Status: i.Status,
			ServerLabel: i.ServerLabel, Name: i.Name, Arguments: i.Arguments,
			Output: i.Output,
		})
	}
```

> **为何单独分支**：`server_label`/`name`/`arguments` 在 OpenAI wire 为 `api:"required"`，透传的 `omitempty` 会丢空值。复用 `OutputItem.Output`（已有，tool 输出）承载 mcp_call output，`Error` 需新增或复用——当前 `OutputItem` 无 `Error` 字段；mcp_call failed 的 error 用 `Status=failed` 表达，error 文本并入 `Output`（见 Task B4），故本分支不写单独 error 字段，去掉 `Error` 行：把上面 struct 的 `Error` 字段与赋值删除，failed 时 error 文本放 `Output`。

> **修正（落实）**：上面 MarshalJSON 分支删除 `Error string` 字段与 `Error: i.Error` 赋值（`OutputItem` 无 Error 字段，编译会失败）。mcp_call 的 failed 状态由 `Status` 表达，错误文本写入 `Output`。

- [ ] **Step 6: 加事件 struct**

`internal/model/event.go`（批次 A 加的 `CodeInterpreterCallCodeDoneEvent` 后）加：

```go
// McpCallEvent 用于 mcp_call 的 in_progress / completed / failed 事件。
type McpCallEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
}

// McpCallArgumentsDeltaEvent 用于 response.mcp_call_arguments.delta。
type McpCallArgumentsDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

// McpCallArgumentsDoneEvent 用于 response.mcp_call_arguments.done。
type McpCallArgumentsDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Arguments      string `json:"arguments"`
}
```

- [ ] **Step 7: 运行测试验证 GREEN**

Run: `go test ./internal/model/`
Expected: PASS。

- [ ] **Step 8: Commit**

```bash
git add internal/model/constants.go internal/model/outputitem.go internal/model/event.go internal/model/outputitem_test.go
git commit -m "feat(model): 新增 mcp_call item 类型与事件"
```

---

## Task B2: catalog 注册 mcp（identity + beta kind）

**Files:**
- Modify: `internal/toolcatalog/identity.go`
- Modify: `internal/toolcatalog/inspect.go`
- Test: `internal/toolcatalog/inspect_test.go`

**Interfaces:**
- Consumes: `oairesponses.ToolUnionParam.OfMcp`
- Produces: `KindBetaServerTool` 常量；`Inspect` 覆盖 `OfMcp`

> **说明**：MCP 请求映射不走 `catalog.Declare`（标准 `ToolUnionParam` 无 `OfMCPToolset`），而由 `convert.collectMCP` 专门产出 `MCPInjection`（Task B3）。catalog 仅登记 identity（供 freeform/identity 一致性），并补 `KindBetaServerTool` 语义常量（spec 57 行）。

- [ ] **Step 1: 写 RED 测试**

`internal/toolcatalog/inspect_test.go` 的 `TestInspectClientTools` table 加：

```go
		{"mcp", oairesponses.ToolUnionParam{OfMcp: &oairesponses.ToolMcpParam{ServerLabel: "s"}}, Identity{OpenAIType: "mcp", Name: "mcp"}},
```

- [ ] **Step 2: 运行测试验证 RED**

Run: `go test ./internal/toolcatalog/ -run TestInspectClientTools -v`
Expected: FAIL（`OfMcp` 落入 default 报错）。

- [ ] **Step 3: identity.go 加 KindBetaServerTool**

`internal/toolcatalog/identity.go` 的 `KindUnsupported` 常量后加：

```go
	// KindBetaServerTool 需 beta API（如 MCP connector），由 convert 产出
	// beta 注入定义、client 层注入请求体（非标准 ToolUnionParam）。
	KindBetaServerTool Kind = "beta_server_tool"
```

- [ ] **Step 4: inspect.go 加 case**

`internal/toolcatalog/inspect.go` 的 `Inspect` switch（`OfCodeInterpreter` case 后，批次 A 加的）加：

```go
	case t.OfMcp != nil:
		return []Identity{{OpenAIType: "mcp", Name: "mcp"}}, nil
```

- [ ] **Step 5: 运行 catalog 测试验证 GREEN**

Run: `go test ./internal/toolcatalog/`
Expected: PASS。

- [ ] **Step 6: 确认 convert 声明路径不误伤 mcp**

Run: `go test ./internal/convert/`
Expected: 全绿。

> **关键检查**：批次 0 的 `appendToolList` 调 `catalog.Declare` 遍历 `req.Tools`。`Declare` 对 `OfMcp` 无 case（本批只在 `Inspect` 加），会落入 default 报错。需在 `declare.go` 的 `Declare` switch 加一个 `OfMcp` case **返回空声明 + nil error**（MCP 不产标准 tool，由 collectMCP 单独处理）：

```go
	case t.OfMcp != nil:
		// MCP 是 beta server tool，不产出标准 ToolUnionParam；
		// 其请求定义由 convert.collectMCP 产出 MCPInjection，client 注入。
		return nil, nil
```

加在 `Declare` 的 `OfCodeInterpreter` case（批次 A）后。这样 `appendToolList` 对 mcp tool 跳过（空声明），不报错。

- [ ] **Step 7: Commit**

```bash
git add internal/toolcatalog/identity.go internal/toolcatalog/inspect.go internal/toolcatalog/declare.go internal/toolcatalog/inspect_test.go
git commit -m "feat(toolcatalog): 注册 mcp 为 beta_server_tool，声明侧让位 collectMCP"
```

---

## Task B3: MCPInjection 类型 + 请求产出 + 管道 + JSON 注入 + beta header

**Files:**
- Create: `internal/anthropic/mcp.go`
- Modify: `internal/anthropic/client.go`（`Stream` 签名 + 注入 + beta header）
- Modify: `internal/convert/request.go`（`ToAnthropic` 返回值 + `collectMCP`）
- Modify: `internal/scheduler/scheduler.go`（`RequestBuilder` + 4 方法透传）
- Modify: `internal/server/server.go`（220 行闭包）
- Test: `internal/anthropic/mcp_test.go`、`internal/convert/request_test.go`、`internal/anthropic/client_test.go`

**Interfaces:**
- Consumes: `oairesponses.ToolMcpParam`（字段见 spec 134-135）；`injectStream` 模式
- Produces: `anthropic.MCPInjection`/`MCPServer`/`MCPToolset`；`injectMCP`；`ToAnthropic` 第三返回值；管道透传

- [ ] **Step 1: 写 anthropic.MCPInjection 类型与 injectMCP（含测试）**

Create `internal/anthropic/mcp.go`：

```go
package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MCPBetaHeader 是 MCP managed connector 所需的 anthropic-beta 值。
const MCPBetaHeader = "mcp-client-2025-11-20"

// MCPServer 描述一个待注入请求体顶层 mcp_servers[] 的 beta server 定义。
type MCPServer struct {
	Type               string `json:"type"` // "url"
	URL                string `json:"url"`
	Name               string `json:"name"`
	AuthorizationToken string `json:"authorization_token,omitempty"`
}

// MCPToolset 描述一个待注入 tools[] 的 mcp_toolset（allowlist 模式）。
type MCPToolset struct {
	MCPServerName string   // server_label
	EnabledTools  []string // allowed_tools 命中项；空表示全启用（default_config.enabled=true）
}

// MCPInjection 汇总一次请求的全部 MCP 定义，由 convert 产出、client 注入。
type MCPInjection struct {
	Servers  []MCPServer
	Toolsets []MCPToolset
}

func (m *MCPInjection) Empty() bool { return m == nil || len(m.Servers) == 0 }

// injectMCP 把 mcp_servers（顶层）与 mcp_toolset（tools[] 追加）写入已 marshal 的请求体。
// mcp 为空时原样返回。复用 injectStream 的 map 操作模式，json.Number 保数值精度。
func injectMCP(body []byte, mcp *MCPInjection) ([]byte, error) {
	if mcp.Empty() {
		return body, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	servers := make([]any, 0, len(mcp.Servers))
	for _, s := range mcp.Servers {
		servers = append(servers, map[string]any{
			"type": "url", "url": s.URL, "name": s.Name,
			"authorization_token": s.AuthorizationToken,
		})
	}
	obj["mcp_servers"] = servers

	tools, _ := obj["tools"].([]any)
	for _, ts := range mcp.Toolsets {
		entry := map[string]any{"type": "mcp_toolset", "mcp_server_name": ts.MCPServerName}
		if len(ts.EnabledTools) == 0 {
			// 无 allowlist：默认配置全启用。
			entry["default_config"] = map[string]any{"enabled": true}
		} else {
			cfg := map[string]any{}
			for _, name := range ts.EnabledTools {
				cfg[name] = map[string]any{"enabled": true}
			}
			entry["configs"] = cfg
			entry["default_config"] = map[string]any{"enabled": false}
		}
		tools = append(tools, entry)
	}
	obj["tools"] = tools
	return json.Marshal(obj)
}

// mergeBetaHeader 把 mcp beta 值并入已有 anthropic-beta（逗号分隔），避免覆盖 thinking。
func mergeBetaHeader(existing string) string {
	if existing == "" {
		return MCPBetaHeader
	}
	if strings.Contains(existing, MCPBetaHeader) {
		return existing
	}
	return existing + "," + MCPBetaHeader
}

var _ = fmt.Sprintf
```

> **`var _ = fmt.Sprintf`**：占位防 unused import；若 Step 1 不直接用 fmt（injectMCP 未用 fmt），删除 import 与该行。最终实现里若无需 fmt 则一并删除。

Create `internal/anthropic/mcp_test.go`：

```go
package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInjectMCPAddsServersAndToolset(t *testing.T) {
	body := []byte(`{"model":"x","tools":[{"type":"web_search_20250305"}]}`)
	out, err := injectMCP(body, &MCPInjection{
		Servers:  []MCPServer{{URL: "https://s.example", Name: "weather", AuthorizationToken: "tok"}},
		Toolsets: []MCPToolset{{MCPServerName: "weather", EnabledTools: []string{"get"}}},
	})
	if err != nil {
		t.Fatalf("injectMCP: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	servers := obj["mcp_servers"].([]any)
	s0 := servers[0].(map[string]any)
	if s0["type"] != "url" || s0["name"] != "weather" || s0["authorization_token"] != "tok" {
		t.Fatalf("bad server: %v", s0)
	}
	tools := obj["tools"].([]any)
	ts := tools[1].(map[string]any) // 原有 web_search 在前
	if ts["type"] != "mcp_toolset" || ts["mcp_server_name"] != "weather" {
		t.Fatalf("bad toolset: %v", ts)
	}
}

func TestInjectMCPEmptyNoop(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	out, err := injectMCP(body, nil)
	if err != nil || string(out) != string(body) {
		t.Fatalf("empty must be noop")
	}
}

func TestMergeBetaHeader(t *testing.T) {
	if got := mergeBetaHeader(""); got != MCPBetaHeader {
		t.Fatalf("empty base: %q", got)
	}
	if got := mergeBetaHeader("interleaved-thinking-2025-05-14"); !strings.Contains(got, MCPBetaHeader) || !strings.Contains(got, "interleaved-thinking") {
		t.Fatalf("must merge: %q", got)
	}
	if got := mergeBetaHeader(MCPBetaHeader); got != MCPBetaHeader {
		t.Fatalf("must dedupe: %q", got)
	}
}
```

> **修正 import**：mcp.go 若不用 `fmt`/`strings`（injectMCP 用 strings.NewReader、mergeBetaHeader 用 strings），保留 strings，删除 fmt 与 `var _ = fmt.Sprintf`。mcp_test.go 用 strings.Contains，import strings。

- [ ] **Step 2: 运行 mcp 测试验证 GREEN**

Run: `go test ./internal/anthropic/ -run 'TestInjectMCP|TestMergeBeta' -v`
Expected: PASS。

- [ ] **Step 3: client.Stream 接入 mcp 注入与 beta header**

`internal/anthropic/client.go` 的 `Stream` 签名（106）改为：

```go
func (c *Client) Stream(ctx context.Context, endpoint, apiKey string, req *anthropic.MessageNewParams, mcp *MCPInjection) (io.ReadCloser, error) {
```

在 `injectStream` 之后（114-116 后）加：

```go
	if body, err = injectMCP(body, mcp); err != nil {
		return nil, err
	}
```

beta header 部分（135-137）改为：

```go
	beta := ""
	if thinkingEnabled(req) {
		beta = "interleaved-thinking-2025-05-14"
	}
	if !mcp.Empty() {
		beta = mergeBetaHeader(beta)
	}
	if beta != "" {
		httpReq.Header.Set("anthropic-beta", beta)
	}
```

修复 `client_test.go` 与所有 `Stream` 调用点编译（加 `nil` 实参）：`grep -rn '\.Stream(' --include='*_test.go'` 后在每个调用补第 5 参 `nil`（测试无 MCP 时）。

- [ ] **Step 4: scheduler 管道透传**

`internal/scheduler/scheduler.go`：

`RequestBuilder`（45）改为：

```go
type RequestBuilder func(src config.Source) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error)
```

> **import 别名**：scheduler 已 import anthropic client 为 `anthropicclient`（见 scheduler.go:244 `anthropicclient.ScanEvents`）。`MCPInjection` 在 `internal/anthropic` 包，用 `anthropicclient.MCPInjection`。

`Execute`（150）改为透传 mcp：

```go
func (s *Scheduler) Execute(ctx context.Context, req *anthropic.MessageNewParams, mcp *anthropicclient.MCPInjection, onEvent func(*anthropic.MessageStreamEventUnion) error) error {
	_, err := s.ExecutePrepared(ctx, func(_ config.Source) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
		return req, mcp, nil
	}, onEvent)
	return err
}
```

`tryRoundPrepared`（188）：`build(src)` 返回 3 值，传 mcp 给 `trySource`：

```go
		req, mcp, err := build(src)
		if err != nil { ... }
		locked, err := s.trySource(ctx, &src, bk, req, mcp, onEvent)
```

`trySource`（213）签名加 `mcp *anthropicclient.MCPInjection`，传给 `client.Stream`：

```go
func (s *Scheduler) trySource(ctx context.Context, src *config.Source, bk *breaker.Preaker,
	req *anthropic.MessageNewParams, mcp *anthropicclient.MCPInjection, onEvent func(*anthropic.MessageStreamEventUnion) error) (bool, error) {
	...
	body, err := s.client.Stream(fbCtx, src.BaseURL, src.APIKey, &resolvedReq, mcp)
	...
}
```

修复 scheduler 测试里 `Execute`/`ExecutePrepared`/`trySource` 调用点（补 mcp 实参，通常 `nil`）。

- [ ] **Step 5: server 闭包返回 mcp**

`internal/server/server.go` 的 `buildAnthropicRequest`（354）返回值改为 `(*oairesponses.ResponseNewParams, *anthropic.MessageNewParams, *anthropic.MCPInjection, error)`：

```go
func (s *Server) buildAnthropicRequest(body []byte, src config.Source) (*oairesponses.ResponseNewParams, *anthropic.MessageNewParams, *anthropic.MCPInjection, error) {
```

377 行的 `ToAnthropic` 调用改为：

```go
	anthReq, mcp, err := convert.ToAnthropic(req, s.cfg, prevItems...)
	if err != nil {
		return nil, nil, nil, err
	}
	return req, anthReq, mcp, nil
```

220 行 `ExecutePrepared` 闭包改为：

```go
	sourceName, execErr := s.sch.ExecutePrepared(r.Context(), func(src config.Source) (*anthropic.MessageNewParams, *anthropic.MCPInjection, error) {
		req, anthReq, mcp, err := s.buildAnthropicRequest(body, src)
		// ...（原 221-250 的日志逻辑，用 anthReq/mcp 不变）
		return anthReq, mcp, nil
	}, onEvent)
```

> **190 行**：`buildAnthropicRequest` 的另一调用点（预检）相应改：`if _, _, _, err := s.buildAnthropicRequest(body, ordered[0]); err != nil {`。

- [ ] **Step 6: convert.ToAnthropic 产出 MCPInjection**

`internal/convert/request.go` 的 `ToAnthropic` 签名（114）改为：

```go
func ToAnthropic(req *oairesponses.ResponseNewParams, cfg *config.Config, prevItems ...model.OutputItem) (*anthropic.MessageNewParams, *anthropic.MCPInjection, error) {
```

在 `convertTools` 后、return 前加：

```go
	mcp, err := collectMCP(req)
	if err != nil {
		return nil, nil, err
	}
```

返回改为 `return out, mcp, nil`。

新增 `collectMCP`（字段映射 + 损失 + fail-fast）：

```go
// collectMCP 扫描请求里的 mcp tool，产出 beta MCPInjection（mcp_servers + toolset）。
// 字段映射见 spec 2.2；损失处理见 spec 2.4。
// connector_id / tunnel_id 是 OpenAI 私有托管设施，不在 Anthropic 标准范围 → fail-fast。
func collectMCP(req *oairesponses.ResponseNewParams) (*anthropic.MCPInjection, error) {
	var inj anthropic.MCPInjection
	for _, t := range req.Tools {
		if t.OfMcp == nil {
			continue
		}
		m := t.OfMcp
		if m.ConnectorID != "" {
			return nil, fmt.Errorf("mcp connector_id %q is not supported: use server_url form instead", m.ConnectorID)
		}
		if m.TunnelID.Valid() && m.TunnelID.Value != "" {
			return nil, fmt.Errorf("mcp tunnel_id %q is not supported: use server_url form instead", m.TunnelID.Value)
		}
		serverURL := ""
		if m.ServerURL.Valid() {
			serverURL = m.ServerURL.Value
		}
		if serverURL == "" {
			return nil, fmt.Errorf("mcp server %q requires server_url (connector_id/tunnel_id unsupported)", m.ServerLabel)
		}
		token := ""
		if m.Authorization.Valid() {
			token = m.Authorization.Value
		}
		// headers：择优提取 Authorization: Bearer → authorization_token（authorization 空时回退）。
		if bearer, ok := m.Headers["Authorization"]; ok && token == "" {
			token = strings.TrimPrefix(bearer, "Bearer ")
		}
		for k := range m.Headers {
			if k != "Authorization" {
				slog.Warn("丢弃 MCP server 自定义 header（Anthropic 仅支持单一 authorization_token）",
					"server_label", m.ServerLabel, "header", k)
			}
		}
		// require_approval：Anthropic MCP 无审批协议。never/缺省正常；其余降级 never + WARN。
		if appr := approvalMode(m.RequireApproval); appr != "" && appr != "never" {
			slog.Warn("MCP require_approval 降级为 never（Anthropic 无审批协议，工具将直接执行）",
				"server_label", m.ServerLabel, "require_approval", appr)
		}
		inj.Servers = append(inj.Servers, anthropic.MCPServer{
			Type: "url", URL: serverURL, Name: m.ServerLabel, AuthorizationToken: token,
		})
		enabled := allowedMCPToolNames(m.AllowedTools)
		inj.Toolsets = append(inj.Toolsets, anthropic.MCPToolset{
			MCPServerName: m.ServerLabel, EnabledTools: enabled,
		})
	}
	if inj.Empty() {
		return nil, nil
	}
	return &inj, nil
}

// approvalMode 从 ToolMcpRequireApprovalUnionParam 取出审批模式字符串（"" 表缺省=never）。
// SDK：OfMcpToolApprovalSetting 是 param.Opt[string]（值如 "never"/"on_failure"/"if_referenced"），
// OfMcpToolApprovalFilter 是 filter 对象（近似需审批，降级为 on_failure）。
func approvalMode(u oairesponses.ToolMcpRequireApprovalUnionParam) string {
	if u.OfMcpToolApprovalSetting.Valid() {
		return u.OfMcpToolApprovalSetting.Value
	}
	if u.OfMcpToolApprovalFilter != nil {
		return "on_failure"
	}
	return ""
}

// allowedMCPToolNames 从 allowed_tools union 取出命中的工具名列表。
// SDK：OfMcpAllowedTools 是 []string（allowlist）；OfMcpToolFilter 是 filter 对象（本批不展开）。
func allowedMCPToolNames(u oairesponses.ToolMcpAllowedToolsUnionParam) []string {
	return u.OfMcpAllowedTools
}
```

> **字段名以 SDK 为准**：`ToolMcpRequireApprovalUnionParam` 的 `OfNever`/`OfOnFailure`/`OfIfReferenced`、`ToolMcpAllowedToolsUnionParam` 的 `OfAllowedToolsStringArray` 是推测的字段名——RED/go doc 锁定确切变体名（`go doc github.com/openai/openai-go/v3/responses.ToolMcpRequireApprovalUnionParam`）。`m.ServerURL`/`m.Authorization`/`m.TunnelID` 是 `param.Opt[string]`（用 `.Valid()`/`.Value`），`m.ServerLabel`/`m.ConnectorID` 是 `string`，`m.Headers` 是 `map[string]string`（go doc 已确认，见调研）。

- [ ] **Step 7: convert 测试 + 全链路编译**

`internal/convert/request_test.go` 加：

```go
func TestMcpToolProducesMCPInjection(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"weather","server_url":"https://s.example","allowed_tools":["get"]},{"type":"web_search"}],"stream":true}`)
	out, mcp, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatalf("mcp must not fail fast: %v", err)
	}
	if mcp == nil || len(mcp.Servers) != 1 || mcp.Servers[0].Name != "weather" {
		t.Fatalf("MCPInjection not produced: %+v", mcp)
	}
	if len(mcp.Toolsets) != 1 || len(mcp.Toolsets[0].EnabledTools) != 1 {
		t.Fatalf("toolset allowlist wrong: %+v", mcp.Toolsets)
	}
	// mcp tool 不进标准 tools[]（web_search 进，mcp 不进）
	for _, tool := range out.Tools {
		if tool.OfTool != nil && tool.OfTool.Name == "weather" {
			t.Fatal("mcp must not appear as standard ToolParam")
		}
	}
}

func TestMcpConnectorIDFailsFast(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","tools":[{"type":"mcp","server_label":"s","connector_id":"cntr_x"}],"stream":true}`)
	if _, _, err := ToAnthropic(req, &config.Config{}); err == nil {
		t.Fatal("connector_id must fail fast")
	}
}
```

Run: `go build ./... && go test ./internal/convert/ ./internal/anthropic/ ./internal/scheduler/ ./internal/server/`
Expected: 编译通过；新测试 PASS；既有测试若因签名变更失败，补 `nil`/`_` 实参后全绿。

- [ ] **Step 8: Commit**

```bash
git add internal/anthropic/mcp.go internal/anthropic/mcp_test.go internal/anthropic/client.go internal/convert/request.go internal/scheduler/scheduler.go internal/server/server.go internal/convert/request_test.go internal/anthropic/client_test.go
git commit -m "feat(mcp): 请求侧产出 MCPInjection，client 注入 beta mcp_servers/mcp_toolset"
```

---

## Task B4: ScanEvents mcp probe + streamconv 回程 handler

**Files:**
- Modify: `internal/anthropic/client.go`（`ScanEvents` probe）
- Modify: `internal/streamconv/converter.go`（事件常量、Converter 字段、handleBlockStart 分支、handler）
- Test: `internal/streamconv/converter_test.go`

**Interfaces:**
- Consumes: Task B1 model 事件；ScanEvents probe
- Produces: `mcp_call` item + 5 事件（in_progress/arguments.delta/arguments.done/completed/failed）

- [ ] **Step 1: ScanEvents 加 mcp probe**

`internal/anthropic/client.go` 的 `ScanEvents`（184）在 error probe（199-218）后、标准 unmarshal（220）前加 mcp probe：

```go
		// mcp_tool_use / mcp_tool_result 是 beta block，标准 MessageStreamEventUnion
		// 无 Of* 变体，标准 unmarshal 会丢字段。探测 raw type，构造合成事件。
		if json.Unmarshal([]byte(payload), &probe) == nil && (probe.Type == "mcp_tool_use" || probe.Type == "mcp_tool_result") {
			synthetic, err := synthesizeMCPEvent([]byte(payload))
			if err != nil {
				return fmt.Errorf("parse mcp block: %w: %s", err, truncForLog([]byte(payload), 500))
			}
			if err := fn(synthetic); err != nil {
				return err
			}
			continue
		}
```

新增 `synthesizeMCPEvent`（同文件或 mcp.go）：

```go
// synthesizeMCPEvent 把 beta mcp block 的 raw JSON 解析成合成 MessageStreamEventUnion，
// 使 converter 能用标准 ev.ContentBlock 字段消费（Type/ID/Input/Name/Content/ToolUseID）。
func synthesizeMCPEvent(payload []byte) (*anthropic.MessageStreamEventUnion, error) {
	var raw struct {
		Type       string          `json:"type"`
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		ServerName string          `json:"server_name"`
		Input      json.RawMessage `json:"input"`
		ToolUseID  string          `json:"tool_use_id"`
		IsError    bool            `json:"is_error"`
		Content    json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, err
	}
	ev := &anthropic.MessageStreamEventUnion{Type: "content_block_start"}
	cb := anthropic.ContentBlockStartEventContentBlockUnion{Type: raw.Type}
	switch raw.Type {
	case "mcp_tool_use":
		cb.ID = raw.ID
		cb.Name = raw.Name
		// server_name 无标准字段槽，编码进 Input：{server_name, name, arguments}。
		cb.Input = map[string]any{
			"server_name": raw.ServerName,
			"name":        raw.Name,
			"arguments":   string(raw.Input),
		}
	case "mcp_tool_result":
		cb.ToolUseID = raw.ToolUseID
		cb.Content = anthropic.ContentBlockStartEventContentBlockUnionContent{
			URL: mcpResultText(raw.Content), // 复用 URL 槽承载 output 文本（见下）
		}
		cb.ErrorCode = ""
		if raw.IsError {
			cb.Content.RetrievedAt = "1" // 复用 RetrievedAt 槽作 is_error 标志（非空=error）
		}
	}
	ev.ContentBlock = cb
	return ev, nil
}

// mcpResultText 从 mcp_tool_result.content（[]{type,text}）拼出纯文本。
func mcpResultText(content json.RawMessage) string {
	var parts []map[string]any
	if json.Unmarshal(content, &parts) != nil {
		return string(content)
	}
	var b strings.Builder
	for _, p := range parts {
		if t, ok := p["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}
```

> **字段槽复用说明**：`mcp_tool_result` 的 output 文本与 is_error 标志无标准 `ContentBlock` 字段槽，借用 `Content.URL`（output 文本）与 `Content.RetrievedAt`（is_error 非空标志）。converter handler（Task B4 Step 3）按此约定读取。这是 probe 合成事件的内部契约，不外泄。

- [ ] **Step 2: converter 加 mcp block 常量与状态**

`internal/streamconv/converter.go` 的 block 常量块（`anBlockCodeExecutionToolResult` 附近）加：

```go
	// beta mcp block：aconstant 无对应（beta only），硬编码 wire 字符串。
	anBlockMcpToolUse   = "mcp_tool_use"
	anBlockMcpToolResult = "mcp_tool_result"
```

事件常量块加：

```go
	evMcpCallInProgress     = string(oaconstant.ValueOf[oaconstant.ResponseMcpCallInProgress]())
	evMcpCallArgumentsDelta = string(oaconstant.ValueOf[oaconstant.ResponseMcpCallArgumentsDelta]())
	evMcpCallArgumentsDone  = string(oaconstant.ValueOf[oaconstant.ResponseMcpCallArgumentsDone]())
	evMcpCallCompleted      = string(oaconstant.ValueOf[oaconstant.ResponseMcpCallCompleted]())
	evMcpCallFailed         = string(oaconstant.ValueOf[oaconstant.ResponseMcpCallFailed]())
```

`Converter` struct（`codeExecutionByToolUseID` 后）加：

```go
	// MCP call state: Anthropic mcp tool_use id -> output item index.
	mcpCallByToolUseID map[string]int
```

`New()` 加 `mcpCallByToolUseID: map[string]int{},`。

- [ ] **Step 3: 写 RED 测试**

`internal/streamconv/converter_test.go` 加：

```go
func TestMcpToolUseEmitsMcpCall(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_start", Message: anthropic.Message{ID: "m", Model: "x"}})
	// 模拟 ScanEvents probe 合成的 mcp_tool_use 事件
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "mcp_tool_use", ID: "toolu_mcp1", Name: "get",
			Input: map[string]any{"server_name": "weather", "name": "get", "arguments": `{"q":"sf"}`},
		},
	})
	added := eventData(t, eventByType(t, evs, "response.output_item.added"))
	item := added["item"].(map[string]any)
	if item["type"] != "mcp_call" || item["server_label"] != "weather" || item["name"] != "get" {
		t.Fatalf("bad mcp_call item: %v", item)
	}
	if item["arguments"] != `{"q":"sf"}` {
		t.Fatalf("bad arguments: %v", item["arguments"])
	}
	eventByType(t, evs, "response.mcp_call.in_progress")
	eventByType(t, evs, "response.mcp_call_arguments.delta")
	eventByType(t, evs, "response.mcp_call_arguments.done")

	evs2, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "mcp_tool_result", ToolUseID: "toolu_mcp1",
			Content: anthropic.ContentBlockStartEventContentBlockUnionContent{URL: "sunny"},
		},
	})
	done := eventData(t, eventByType(t, evs2, "response.output_item.done"))
	doneItem := done["item"].(map[string]any)
	if doneItem["status"] != "completed" || doneItem["output"] != "sunny" {
		t.Fatalf("bad mcp_call done: %v", doneItem)
	}
}

func TestMcpToolResultErrorEmitsFailed(t *testing.T) {
	c := New()
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_start", Message: anthropic.Message{ID: "m", Model: "x"}})
	c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "mcp_tool_use", ID: "toolu_mcp2", Name: "get",
			Input: map[string]any{"server_name": "w", "name": "get", "arguments": "{}"},
		},
	})
	evs, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 1,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{
			Type: "mcp_tool_result", ToolUseID: "toolu_mcp2",
			Content: anthropic.ContentBlockStartEventContentBlockUnionContent{URL: "boom", RetrievedAt: "1"},
		},
	})
	eventByType(t, evs, "response.mcp_call.failed")
	done := eventData(t, eventByType(t, evs, "response.output_item.done"))
	if done["item"].(map[string]any)["status"] != "failed" {
		t.Fatalf("expected failed status")
	}
}
```

- [ ] **Step 4: 运行测试验证 RED**

Run: `go test ./internal/streamconv/ -run TestMcp -v`
Expected: FAIL（mcp block type 未识别，走 `handleUnsupportedBlock` → response.failed，或无 mcp_call item）。

> **注意**：`handleBlockStart` 对未知 block type 走 `handleUnsupportedBlock`（response.failed）。RED 阶段 mcp_tool_use 会触发 failed——这正是要修的。

- [ ] **Step 5: handleBlockStart 加 mcp 分支**

`internal/streamconv/converter.go` 的 `handleBlockStart`（255-287）switch 加（在 `default`/`handleUnsupportedBlock` 前）：

```go
	case anBlockMcpToolUse:
		return c.handleMcpToolUseStart(ev)
	case anBlockMcpToolResult:
		return c.handleMcpToolResultStart(ev)
```

- [ ] **Step 6: 新增 mcp handler**

`internal/streamconv/converter.go` 加：

```go
// handleMcpToolUseStart 把（probe 合成的）mcp_tool_use 映射为 mcp_call item + 事件链。
// Input 由 synthesizeMCPEvent 编码为 {server_name, name, arguments}。
func (c *Converter) handleMcpToolUseStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx := c.itemOrder
	c.itemOrder++
	itemID := fmt.Sprintf("mcp_%d", idx)
	serverLabel, toolName, args := decodeMcpUseInput(ev.ContentBlock.Input)
	item := model.OutputItem{
		Type:        model.ItemTypeMcpCall,
		ID:          itemID,
		Status:      model.ResponseStatusInProgress,
		ServerLabel: serverLabel,
		Name:        toolName,
		Arguments:   args,
	}
	c.outputItems = append(c.outputItems, item)
	c.mcpCallByToolUseID[ev.ContentBlock.ID] = idx

	out := []model.SSEEvent{
		model.MarshalEvent(evOutputItemAdded, model.OutputItemAddedEvent{
			Type: evOutputItemAdded, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, Item: item,
		}),
		model.MarshalEvent(evMcpCallInProgress, model.McpCallEvent{
			Type: evMcpCallInProgress, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
	}
	if args != "" {
		out = append(out,
			model.MarshalEvent(evMcpCallArgumentsDelta, model.McpCallArgumentsDeltaEvent{
				Type: evMcpCallArgumentsDelta, SequenceNumber: c.nextSeq(),
				OutputIndex: idx, ItemID: itemID, Delta: args,
			}),
			model.MarshalEvent(evMcpCallArgumentsDone, model.McpCallArgumentsDoneEvent{
				Type: evMcpCallArgumentsDone, SequenceNumber: c.nextSeq(),
				OutputIndex: idx, ItemID: itemID, Arguments: args,
			}),
		)
	}
	return out
}

func decodeMcpUseInput(input any) (serverLabel, name, args string) {
	m, ok := input.(map[string]any)
	if !ok {
		return "", "", ""
	}
	if v, ok := m["server_name"].(string); ok {
		serverLabel = v
	}
	if v, ok := m["name"].(string); ok {
		name = v
	}
	if v, ok := m["arguments"].(string); ok {
		args = v
	}
	return
}

// handleMcpToolResultStart 把（probe 合成的）mcp_tool_result 映射为 mcp_call output + completed/failed。
// output 文本在 ev.ContentBlock.Content.URL，is_error 标志在 Content.RetrievedAt（非空=error）。
func (c *Converter) handleMcpToolResultStart(ev *anthropic.MessageStreamEventUnion) []model.SSEEvent {
	idx, ok := c.mcpCallByToolUseID[ev.ContentBlock.ToolUseID]
	if !ok || idx >= len(c.outputItems) {
		return nil
	}
	itemID := fmt.Sprintf("mcp_%d", idx)
	rc := ev.ContentBlock.Content
	isError := rc.RetrievedAt != ""
	c.outputItems[idx].Output = rc.URL
	if isError {
		c.outputItems[idx].Status = model.ResponseStatusFailed
		return []model.SSEEvent{
			model.MarshalEvent(evMcpCallFailed, model.McpCallEvent{
				Type: evMcpCallFailed, SequenceNumber: c.nextSeq(),
				OutputIndex: idx, ItemID: itemID,
			}),
			model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
				Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
				OutputIndex: idx, Item: c.outputItems[idx],
			}),
		}
	}
	c.outputItems[idx].Status = model.ResponseStatusCompleted
	return []model.SSEEvent{
		model.MarshalEvent(evMcpCallCompleted, model.McpCallEvent{
			Type: evMcpCallCompleted, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, ItemID: itemID,
		}),
		model.MarshalEvent(evOutputItemDone, model.OutputItemDoneEvent{
			Type: evOutputItemDone, SequenceNumber: c.nextSeq(),
			OutputIndex: idx, Item: c.outputItems[idx],
		}),
	}
}
```

- [ ] **Step 7: 运行测试验证 GREEN**

Run: `go test ./internal/streamconv/`
Expected: PASS（新 mcp 用例 + 既有用例不回归）。

- [ ] **Step 8: race + Commit**

Run: `task test-race`
Expected: 无 data race。

```bash
git add internal/anthropic/client.go internal/streamconv/converter.go internal/streamconv/converter_test.go
git commit -m "feat(mcp): 回程 mcp_tool_use/result probe 解析为 mcp_call 事件链"
```

---

## Task B5: convert 多轮 input 回灌（mcp_call + mcp_approval_response）

**Files:**
- Modify: `internal/convert/request.go`（`appendItem` 加 `OfMcpCall`/`OfMcpListTools`/`OfMcpApprovalRequest`/`OfMcpApprovalResponse` 分支）
- Test: `internal/convert/request_test.go`

**Interfaces:**
- Consumes: `oairesponses.ResponseInputItemUnionParam.OfMcpCall` 等
- Produces: 历史 beta mcp_tool_use + mcp_tool_result JSON 回放（经 `MCPInjection` 注入路径或历史 content block）

> **回灌机制**：历史 `mcp_call` 需回放为 Anthropic beta `mcp_tool_use` + `mcp_tool_result`。但请求侧 messages 的 content block 也无标准 mcp 变体（`ContentBlockParamUnion` 无 `OfMCPToolUse`）。故历史 mcp 回放**同样走 JSON 注入**：`ToAnthropic` 把历史 mcp 调用收集进 `MCPInjection` 的扩展，或作为单独的历史注入。为最小化，本批把历史 mcp 调用**追加进 messages 最后一条 user message 的 content，用 raw JSON 文本占位**会破坏协议——故采用：历史 mcp 调用**丢弃 + WARN**（raw_preserved 的保守降级），仅在 `docs/protocol-coverage.md` 登记。

- [ ] **Step 1: 写 RED 测试**

`internal/convert/request_test.go` 加：

```go
func TestMcpCallInputDroppedWithWarn(t *testing.T) {
	// 历史 mcp_call 回灌：本批保守丢弃 + WARN（Anthropic 请求侧无标准 mcp block 变体）。
	req := mustReq(t, `{"model":"gpt-5","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"mcp_call","id":"mcp_1","status":"completed","server_label":"w","name":"get","arguments":"{}","output":"r"}
	],"stream":true}`)
	if _, _, err := ToAnthropic(req, &config.Config{}); err != nil {
		t.Fatalf("mcp_call history must not error (drop+warn): %v", err)
	}
}
```

- [ ] **Step 2: 运行测试验证 RED**

Run: `go test ./internal/convert/ -run TestMcpCallInputDroppedWithWarn -v`
Expected: FAIL（`OfMcpCall` 无 case，落入 unknown → system context 保留，不报错但行为未定义；或当前已不报错则测试已 GREEN，此时断言日志 WARN 需另设）。

> **若已 GREEN**：说明 `appendItem` 对 `OfMcpCall` 已走 unknown 保留分支（不报错）。本步改为显式丢弃 + WARN（避免历史 mcp 内容污染 system context）。

- [ ] **Step 3: appendItem 加显式丢弃分支**

`internal/convert/request.go` 的 `appendItem`（`OfCodeInterpreterCall` 分支后）加：

```go
	if item.OfMcpCall != nil || item.OfMcpListTools != nil ||
		item.OfMcpApprovalRequest != nil || item.OfMcpApprovalResponse != nil {
		slog.Warn("丢弃历史 MCP item（Anthropic 请求侧无标准 mcp block 变体，回灌暂不支持）",
			"item_type", mcpHistoryItemType(item), "call_id", mcpHistoryItemID(item))
		return nil
	}
```

加辅助函数：

```go
func mcpHistoryItemType(item *oairesponses.ResponseInputItemUnionParam) string {
	switch {
	case item.OfMcpCall != nil:
		return "mcp_call"
	case item.OfMcpListTools != nil:
		return "mcp_list_tools"
	case item.OfMcpApprovalRequest != nil:
		return "mcp_approval_request"
	case item.OfMcpApprovalResponse != nil:
		return "mcp_approval_response"
	}
	return "unknown"
}

func mcpHistoryItemID(item *oairesponses.ResponseInputItemUnionParam) string {
	if item.OfMcpCall != nil {
		return item.OfMcpCall.ID
	}
	if item.OfMcpListTools != nil {
		return item.OfMcpListTools.ID
	}
	return ""
}
```

- [ ] **Step 4: 运行测试验证 GREEN + 不回归**

Run: `go test ./internal/convert/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/convert/request.go internal/convert/request_test.go
git commit -m "feat(convert): 历史 MCP item 丢弃 + WARN（请求侧无标准 mcp block 变体）"
```

---

## Task B6: 全量验收 + 文档同步

**Files:**
- Modify: `docs/protocol-coverage.md`
- Modify: `README.md`

- [ ] **Step 1: 全量门禁**

Run: `task check && task test-race`
Expected: 全绿，无 data race。

- [ ] **Step 2: 升级 protocol-coverage 状态**

按 spec「状态升级清单」（182-187 行）更新 `docs/protocol-coverage.md`：
- Tool `mcp`：`unsupported_by_backend` → `lossy_supported`（→beta mcp_servers+mcp_toolset）
- Input/Output `mcp_call`：`unsupported_by_backend` → `lossy_supported`
- Events `mcp_call*`：`unsupported_by_backend` → `lossy_supported`
- `mcp_list_tools`：保持（Anthropic 不暴露工具列表 item）
- `mcp_approval_request` / `mcp_approval_response`：保持（Anthropic 无审批；`require_approval≠never` 降级 never + WARN，历史回灌 raw_preserved）

- [ ] **Step 3: 更新 README 已知限制**

`README.md`「已知限制」加（spec 207 行）：

> - **MCP**：`mcp_list_tools` 工具列表、`require_approval` 审批流（≠never 时降级为 never）、自定义 `headers`（仅 `Authorization: Bearer` 提取）、`connector_id`/`tunnel_id` 不可转换（fail-fast）；历史 MCP item 回灌暂不支持（丢弃 + WARN）；需后端支持 beta `mcp-client-2025-11-20`。

- [ ] **Step 4: Commit**

```bash
git add docs/protocol-coverage.md README.md
git commit -m "docs: MCP 升级为 lossy_supported，登记已知损失"
```

---

## Self-Review

**1. Spec 覆盖**（对照 spec 第 123-172 行）：

- **client 层 beta header**（129）：Task B3 Step 3（`mergeBetaHeader`）。✓
- **请求侧 2.2**（131-147）：`collectMCP` 字段映射（Task B3 Step 6）；headers 择优提取 + WARN；require_approval 降级 + WARN；connector_id/tunnel_id fail-fast；类型链约束（JSON 注入不切 beta，Task B3 Step 1/3）。✓
- **回程 2.3**（149-158）：`ScanEvents` probe（Task B4 Step 1）；`mcp_tool_use`→mcp_call + 事件链（Task B4 Step 6）；`mcp_tool_result`→output + completed/failed（Task B4 Step 6）；`mcp_list_tools` 不发（回程无 list_tools block 处理，自动忽略）。✓
- **input 回灌 2.3.1**（160-164）：历史 mcp_call 丢弃 + WARN（Task B5）；tool_choice `OfMcpTool`/`OfHostedTool` 保持 unsupported——`convertToolChoice`（批次 0 未改）已对 `OfMcpTool`/`OfHostedTool` fail-fast（request.go:771-774），本批不动。✓
- **信息损失 2.4**（166-172）：mcp_list_tools、require_approval、headers、connector_id/tunnel_id、beta API 依赖——Task B3（collectMCP）+ Task B6（文档）。✓
- **状态升级**（182-187）：Task B6。✓

**2. 占位符扫描**：
- `var _ = fmt.Sprintf`（mcp.go）已注明"若不用 fmt 则删除 import 与该行"，非遗留占位。
- mcp.go 的 MarshalJSON 分支 `Error` 字段已注明删除（OutputItem 无 Error）。
- SDK 不确定字段名（`ToolMcpRequireApprovalUnionParam.Of*`、`ToolMcpAllowedToolsUnionParam.OfAllowedToolsStringArray`）均以 RED/go doc 锁定，明确处置。
- 无 TBD/TODO/「类似上文」。✓

**3. 类型一致性**：
- `anthropic.MCPInjection` 在 Task B3 Step 1 定义，被 convert（Step 6）/scheduler（Step 4）/client（Step 3）/server（Step 5）一致引用。
- `RequestBuilder` 返回 `(params, *anthropic.MCPInjection, error)` 跨 server/scheduler 一致。
- `client.Stream` 第 5 参 `*MCPInjection` 跨 scheduler/client 一致。
- `ItemTypeMcpCall`（Task B1）在 converter（Task B4）使用一致。
- probe 合成事件的字段槽契约（Input 编码 server_name/name/arguments；Content.URL=output、RetrievedAt=is_error）在 `synthesizeMCPEvent`（Task B4 Step 1）与 handler（Step 6）一致。✓

**遗留风险（已标注）**：
- `ToolMcpRequireApprovalUnionParam`/`ToolMcpAllowedToolsUnionParam` 变体字段名——Task B3 Step 6 RED/go doc 锁定。
- 历史 MCP item 回灌保守丢弃（Task B5）——spec 2.3.1 要求回放，但请求侧无标准 mcp block 变体，本批降级为丢弃 + WARN，protocol-coverage 登记。若后续 Anthropic 暴露请求侧 mcp block，再升级。
- 批次 0 必须先合并；批次 A 独立（可并行或先后）。
- probe 字段槽复用（URL/RetrievedAt）是内部 hack——若 SDK 演进提供标准 mcp 字段，应迁移。

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-07-17-mcp-connector-mapping.md`.**

> 本批依赖批次 0 合并。三批依次执行顺序：批次 0 → 批次 A → 批次 B（A/B 架构独立，亦可 0 → B → A）。执行选项（subagent-driven / inline）由批次 0 plan 的 handoff 统一确定，本批沿用。
