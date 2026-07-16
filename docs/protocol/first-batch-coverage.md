# 第一批协议覆盖

日期: 2026-07-16

本文是 [协议覆盖矩阵](../protocol-coverage.md) 的第一批执行记录。状态名称、范围和未实现项均以主矩阵为准；本文件只逐项列出本批目标、实现边界和测试证据。

## 覆盖目标

| 主矩阵条目 | 状态 | 第一批行为 | 测试证据 |
|---|---|---|---|
| `local_shell_call` / `shell_call` | `lossy_supported` | 均映射为 Anthropic `tool_use`，名称为 `shell`；环境、超时和调用者元数据不映射 | `TestShellCallInputItemConvertsToShellToolUse`、`TestLocalShellCallInputItemConvertsToShellToolUse` |
| `local_shell_call_output` / `shell_call_output` | `lossy_supported` | 映射为 `tool_result`；会话以 raw JSON 回放 | `TestShellAndApplyPatchOutputsConvertToToolResults`、session 测试 |
| `apply_patch_call` / `apply_patch_call_output` | `lossy_supported` | 映射为 `apply_patch` tool use/result，保留 operation、path 和适用的 diff | `TestApplyPatchCallInputItemConvertsToApplyPatchToolUse`、`TestShellAndApplyPatchOutputsConvertToToolResults` |
| `text.format.json_schema` / `json_object` | `lossy_supported` | 单独使用时以强制 Anthropic synthetic tool 模拟结构化输出 | `TestStructuredOutputInjectsTool`、`TestJSONObjectFormatInjectsTool` |
| `structured output` + `allowed_tools` | `unsupported_by_backend` | Anthropic 没有同时表达“强制 synthetic structured-output tool”与“仅允许指定原始工具集合”的等价模式；请求阶段 fail-fast，不静默覆盖 allowed 集合 | `TestStructuredOutputRejectsAllowedToolsWithoutEquivalent` |
| specific `function` / `custom` choice | `supported` | 仅在声明工具中存在相同 type/name 时映射为 Anthropic 指定工具 | `TestSpecificToolChoiceRejectsUndeclaredIdentity`、`TestSpecificToolChoiceMapsDeclaredIdentity` |
| specific `apply_patch` / `shell` choice | `supported` | 仅在声明相同内置工具时映射；仅声明 `local_shell` 不可充当 specific `shell` | `TestSpecificToolChoiceRejectsUndeclaredIdentity`、`TestSpecificToolChoiceMapsDeclaredIdentity` |
| `allowed_tools` | `lossy_supported` | 仅支持 `auto` 和 `required`；每个 entry 与已声明工具精确比对 type/name/namespace | `TestAllowedToolsJSONModesAndParallelToolCalls`、`TestAllowedToolsRejectsUnknownMode`、`TestAllowedToolsRejectsUnsupportedAllowedEntries` |
| refusal content / refusal SSE | `supported` | 生成 refusal item、`response.refusal.delta`、`response.refusal.done` 与 `content_filter` incomplete；缺少 explanation 时使用稳定可读 fallback，不暴露 category | `TestRefusalStopReasonEmitsRefusalPartAndContentFilter`、`TestRefusalUsesReadableFallbackInsteadOfCategory` |
| refusal terminal response / session replay | `supported` | refusal 终态会丢弃此前已发出的 partial text；终态输出、持久化项和 `previous_response_id` 回放只保留 refusal item | `TestRefusalDiscardsPartialTextFromTerminalOutput`、`TestIntegrationRefusalDoesNotPersistPartialText` |
| `pause_turn` | `lossy_supported` | 返回 incomplete，但不写入 OpenAI 不支持的 incomplete reason | `TestPauseTurnDoesNotEmitInvalidIncompleteReason` |
| hosted/MCP/programmatic tool choice | `unsupported_by_backend` | 请求阶段明确返回转换错误 | `TestUnsupportedHostedToolChoiceReturnsError`、相关 allowed_tools 测试 |
| 未处理 Anthropic stream block | `unsupported_by_backend` / `deferred` | 当前显式 response.failed，后续按主矩阵逐项设计语义映射 | `TestIntegrationUnsupportedBlockDoesNotPersistHiddenOutput` |

## 暂不实现

- OpenAI hosted tools、MCP、computer use、code interpreter、image generation 和大部分 server tool result 仍按主矩阵标为 `unsupported_by_backend` 或 `deferred`，不得伪装为成功转换。
- `local_shell` 可以作为一般工具声明和 `allowed_tools` entry，但 OpenAI 当前 specific tool_choice union 只有 `shell`，因此不能把 `local_shell` 当作 exact `shell` 声明。
- 结构化输出与 `allowed_tools` 的组合没有安全等价表示；本批选择 fail-fast，而不是保留 synthetic tool 并绕过 allowed 集合。

## 验证命令

```bash
go test ./internal/convert -count=1
go test ./internal/streamconv ./internal/server -count=1
go test ./... -count=1
git diff --check
go vet ./...
```
