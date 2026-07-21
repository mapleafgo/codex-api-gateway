# Anthropic cache_control 遗漏补齐设计

日期: 2026-07-21
状态: 设计定稿，待用户审阅后写 implementation plan
关联:
- `docs/superpowers/plans/2026-07-16-prompt-caching.md`（已落地的 system / tools / top-level 基线）
- `docs/protocol-coverage.md`（`prompt_cache_*` 行：网关自主 cache_control）
- `internal/convert/request.go`（`applyAnthropicCacheControl`）
- `internal/toolcatalog/server.go`（`ApplyCacheControl`）
- `internal/anthropic/mcp.go` / `client.go`（MCP 注入与 beta header）

## 背景

网关在 `ToAnthropic` 末尾通过 `applyAnthropicCacheControl` 自主设置 Anthropic prompt cache：

1. 顶层 `MessageNewParams.CacheControl`（automatic，覆盖 messages 历史末块）
2. `System[last].CacheControl`（system 前缀）
3. `Tools[last].CacheControl`（tools 前缀，经 `toolcatalog.ApplyCacheControl` 按变体派发）

TTL 来自 `config.Cache.TTL`（`5m` 默认 / `1h`），用量字段 `cache_read_input_tokens` / `cache_creation_input_tokens` 已透传。

2026-07-21 对照 Anthropic 官方文档 + `anthropic-sdk-go@v1.57.0` 调研后，主路径（function / web_search / code_execution + system + 多轮 messages）已标准闭环，但仍有三类可落地缺口：

| # | 严重度 | 缺口 |
|---|---|---|
| 1 | 高 | MCP `mcp_toolset` 在 `applyAnthropicCacheControl` **之后**由 `injectMCP` append 到 `tools[]`，断点仍落在旧 tools 末项；仅 MCP 时甚至没有 tools 断点 |
| 2 | 中 | `ApplyCacheControl` 手写 switch 只认 3 个 union 变体；SDK 已有 `ToolUnionParam.GetCacheControl()` 可统一写 |
| 3 | 中低 | `cache.ttl=1h` 时 body 写了 `ttl:1h`，但未加 `extended-cache-ttl-2025-04-11` beta（部分后端 / 旧路径可能需要） |

本设计补齐以上三项，并加固测试。可观测侧的 `cache_creation` 5m/1h 拆分**本轮不做**。

## 目标

1. **MCP 场景下 tools 列表最终末项始终携带 `cache_control`**，与「tools 前缀缓存」惯例一致。
2. **`ApplyCacheControl` 对 SDK 已知全部 `ToolUnionParam` 变体可靠**，新增 server tool 变体无需再改 switch。
3. **`ttl=1h` 时自动合并 `extended-cache-ttl-2025-04-11` beta header**，与 thinking / MCP beta 逗号共存。
4. 测试覆盖：仅 MCP、普通 tools+MCP、无 MCP 回归、1h beta、`GetCacheControl` 路径。

## 非目标

- 不改变 system 合成策略（仍为单一 `TextBlockParam` 整块断点）。
- 不映射 OpenAI `prompt_cache_key` / `prompt_cache_options` / `prompt_cache_retention` / `prompt_cache_breakpoint`（语义不等价，保持 WARN + 忽略）。
- 不在网关侧做最小 token 门槛校验（由 Anthropic 静默跳过过短 prompt）。
- 不在 content block 上手工为 messages 历史打多个显式断点（继续依赖顶层 automatic）。
- 不拆分 usage 的 `ephemeral_5m_input_tokens` / `ephemeral_1h_input_tokens`（本轮不做）。
- 不改 `mcp_servers` 顶层结构、授权 token 注入策略，也不把 authorization token 单独做成 cache key 语义——cache 仍由 Anthropic 内容 hash 决定。

## 约束

- 缓存 breakpoint 上限 4 个：补丁后仍为 system(1) + tools(1) + top-level automatic(1) = **3**，不得超 4。
- TTL 仅 `5m` / `1h`（与 `config.CacheCfg` 校验及 SDK 常量一致）。
- 分层：协议转换仍在 `convert`；MCP wire 注入与 beta header 仍在 `anthropic` client 层；`toolcatalog` 只负责 tool 变体上的 cache 字段派发。
- 日志走 `slog` 结构化键值；错误正常返回。
- 测试：被测包同目录 `*_test.go`，TDD 先 RED 再实现。

## 设计

### 1. MCP tools 断点重定位（高）

**问题时序（现状）**

```
ToAnthropic
  → convertTools / collectMCP
  → applyAnthropicCacheControl   // tools[last] 打断点（标准 ToolUnion 范围）
  → client.Stream
      → json.Marshal(req)
      → injectStream
      → injectMCP                // 再 append mcp_toolset → tools 末尾变了
```

结果：

- 有普通 tools + MCP：断点不在最终 `tools[-1]`，MCP 段不进 tools 前缀缓存。
- 仅 MCP：`out.Tools` 为空，**完全不设** tools breakpoint。

**方案（已选 A）**：在 `injectMCP` 完成 toolset 追加后，对最终 `tools[]` 重定位断点。

步骤（仍在 `injectMCP` 内，body 已是 `map[string]any`）：

1. 若未注入任何 toolset 且原 tools 未变，保持现有行为（`mcp == nil` / 无 Servers 早退不变）。
2. 注入全部 `mcp_toolset` 后：
   - 遍历 `tools`：若某项是 `map[string]any` 且含 `cache_control`，**删除**该字段（避免双断点，保证 tools 列表只有一个 breakpoint）。
   - 取最终 `tools[len-1]`：写入
     ```json
     "cache_control": { "type": "ephemeral", "ttl": "<from top-level or 5m>" }
     ```
   - TTL 来源：优先读 body 顶层 `cache_control.ttl`（与 convert 写入的 top-level 一致）；缺失则 `"5m"`。
3. 若最终 `tools` 为空（理论上仅 History 无 Servers 已早退；有 Servers 但 Toolsets 空时 tools 可能仍为空），**不写** tools 断点——无 tools 可缓存。

**为何不把 MCP 提前进 convert 的 `ToolUnionParam`**

- SDK 标准 `ToolUnionParam` 无 `mcp_toolset` 变体；MCP 本就只能 marshal 后注入。
- 在 inject 层重定位与现有「beta 字段后注入」架构一致，不引入双路径声明。

**为何清除旧 tools 上的 cache_control**

- Anthropic 允许多个 breakpoint，但 tools 列表惯例是「末项一个」覆盖整个 tools 前缀。
- 若保留旧断点 + 新末项断点，会多占一个 breakpoint 名额，且语义重复；清除后仍是 3 个全局断点（system + tools + top-level）。

**auth token 与 cache**

- `authorization_token` 在 `mcp_servers` 顶层，不在 tools 项上。
- tools 断点只覆盖 tools 数组内容 hash；token 变化不直接改变 tools 前缀 hash。这是 Anthropic 内容 hash 的固有行为，本设计不额外处理。

### 2. ApplyCacheControl 统一走 GetCacheControl（中）

**现状**

```go
func ApplyCacheControl(tool *anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam) bool {
	switch {
	case tool.OfTool != nil:
		tool.OfTool.CacheControl = cc
	case tool.OfWebSearchTool20250305 != nil:
		// ...
	case tool.OfCodeExecutionTool20250522 != nil:
		// ...
	default:
		return false
	}
	return true
}
```

**目标**

```go
func ApplyCacheControl(tool *anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam) bool {
	if tool == nil {
		return false
	}
	p := tool.GetCacheControl() // SDK 对所有 ToolUnion 变体返回 *CacheControlEphemeralParam
	if p == nil {
		return false
	}
	*p = cc
	return true
}
```

- `setLastToolCacheControl` 调用方不变；未知/空 union 仍 `false` + WARN。
- 测试：保留现有 recognized / unknown 用例；可额外用任一 SDK 变体（如 bash）证明不再依赖 3 分支 switch（可选，若引入 bash 构造成本高可仅依赖 GetCacheControl 非 nil 路径）。

**注意**：`GetCacheControl` 返回的是变体内部字段指针；赋值必须写 `*p = cc`，不可只改局部副本。

### 3. 1h TTL 自动加 extended-cache-ttl beta（中低）

**位置**：`internal/anthropic/client.go` 的 `Stream` 组装 `anthropic-beta` 处。

**规则**

- 当 `req.CacheControl.TTL == anthropic.CacheControlEphemeralTTLTTL1h`（即 `"1h"`）时，把 `extended-cache-ttl-2025-04-11` 并入 beta 列表。
- 与现有 thinking / MCP beta 同一套逗号分隔合并；可抽小 helper（类似 `mergeBetaHeader`）或通用 `appendBeta(existing, value)` 去重。
- `ttl=5m` 或顶层 `CacheControl` 为零值：不加该 beta。

**常量**

```go
const ExtendedCacheTTLBetaHeader = "extended-cache-ttl-2025-04-11"
```

放在 `internal/anthropic` 包内（与 `MCPBetaHeader` 并列）。

**兼容性说明**

- 官方主 Messages API 的稳定 `CacheControlEphemeralParam` 已含 `TTL1h`，部分路径可能已不强制 beta；显式加 header 对官方通常无害，对仍校验 beta 的兼容后端有益。
- 若未来证实完全 GA 且 header 导致某兼容源 400，再改为 per-source 开关；**本轮不做配置开关**（YAGNI）。

### 4. 测试加固

| 用例 | 包 | 断言 |
|---|---|---|
| 普通 tools + MCP 注入后断点在末项 mcp_toolset | `internal/anthropic` | 仅 `tools[-1]` 有 `cache_control`；前项 function 无 |
| 仅 MCP（tools 初始空）注入后末项有断点 | `internal/anthropic` | `tools[-1].cache_control.type==ephemeral`，ttl 与顶层一致 |
| 无 MCP 时 injectMCP 不改 body | 已有 | 回归 |
| TTL 从顶层继承 1h | `internal/anthropic` | 末项 `ttl=="1h"` |
| ApplyCacheControl recognized / unknown | `internal/toolcatalog` | 保留；实现改为 GetCacheControl 后仍绿 |
| Stream 在 CacheControl.TTL=1h 时 header 含 extended-cache-ttl | `internal/anthropic` | 检查发出的 HTTP header（可用 httptest 捕获） |
| convert 现有 cache 单测 | `internal/convert` | 全绿回归 |
| 集成测（可选加强） | `internal/server` | 现有「body 含 cache_control」保留；若改动成本低，可断言 system / tools / top-level 三处（非阻塞） |

### 5. 文档同步

实现完成后：

- 在 `docs/protocol-coverage.md` 的 prompt cache 相关说明中补一句：MCP toolset 在 inject 后重定位 tools 末项断点；`1h` 带 `extended-cache-ttl-2025-04-11`。
- 不强制改 README，除非用户可见配置行为变化（本轮配置面不变）。

## 数据流（补丁后）

```
ToAnthropic
  → applyAnthropicCacheControl
      system[last] + tools[last](标准变体) + top-level CacheControl
  → client.Stream
      → marshal
      → injectStream
      → injectMCP
          append mcp_toolset
          清除 tools[*].cache_control
          写 tools[-1].cache_control（TTL 跟 top-level）
      → beta:
          interleaved-thinking（若 thinking）
          mcp-client-2025-11-20（若 MCP）
          extended-cache-ttl-2025-04-11（若 TTL=1h）
```

## 错误与边界

| 场景 | 行为 |
|---|---|
| inject 后 tools 为空 | 不写 tools 断点 |
| tools 末项不是 object（异常 body） | 不写断点；`slog.Warn` 一次便于发现 wire 异常 |
| ApplyCacheControl 空 union | 返回 false；`setLastToolCacheControl` 已有 WARN |
| 1h + 无 CacheControl 顶层 | 不加 extended-cache-ttl；TTL 默认 5m 路径 |

## 分层与文件

| 文件 | 改动 |
|---|---|
| `internal/anthropic/mcp.go` | `injectMCP` 末尾重定位 tools 断点；可抽 `relocateToolsCacheControl` |
| `internal/anthropic/mcp_test.go` | 新增 MCP 断点用例 |
| `internal/anthropic/client.go` | 1h → extended-cache-ttl beta |
| `internal/anthropic/client_test.go`（或新建） | 1h header 用例 |
| `internal/toolcatalog/server.go` | `ApplyCacheControl` 改 GetCacheControl |
| `internal/toolcatalog/server_test.go` | 回归 / 可选扩展 |
| `docs/protocol-coverage.md` | 短说明同步 |

`convert` 的 `applyAnthropicCacheControl` **本轮可不改**（MCP 断点在 client 层闭环；1h TTL 已由 convert 写入 body 顶层 CacheControl）。

## 验收标准

1. 仅 MCP tools 的上游 body：`tools[-1]` 含 `cache_control`。
2. 普通 tools + MCP：`cache_control` 只在最终 tools 末项，不在中间 function 项。
3. 无 MCP：与补丁前一致（system + tools[last] + top-level）。
4. `cache.ttl=1h` 的请求：`anthropic-beta` 含 `extended-cache-ttl-2025-04-11`。
5. `go test ./internal/anthropic/ ./internal/toolcatalog/ ./internal/convert/ ./internal/server/` 相关包全绿；`task check` 或等价门禁通过。

## 实现顺序（建议，写入 plan 时展开为 TDD task）

1. RED/GREEN：`ApplyCacheControl` → `GetCacheControl`（行为不变，先绿锁回归）
2. RED/GREEN：`injectMCP` tools 断点重定位（仅 MCP + 普通+MCP + TTL 继承）
3. RED/GREEN：`Stream` 1h beta header
4. 文档同步 + 全量相关测试

## 风险

- **JSON map 操作**：复用现有 `json.Number` 解码路径，避免 float 化 token 等字段。
- **兼容后端对 mcp_toolset 上 cache_control 的接受度**：官方 Anthropic tools 项支持 cache_control；若某兼容端拒识，表现为上游 400——与现有「按标准协议转发」原则一致，不为此加特殊降级。
- **breakpoint 清除策略**：若未来需要 tools 内多个分段断点，本设计的「只保留末项」需重新评估；当前网关不需要。
