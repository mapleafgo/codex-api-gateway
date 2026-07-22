# Chat 路径 Hosted Tools + MCP 有损映射设计

> 日期：2026-07-22  
> 状态：实现中  
> 范围：`backend_type: c`（Responses → Chat Completions → Responses）

## 结论

Chat Completions **无** OpenAI/Anthropic server-side hosted 执行。本设计将 Responses 的 hosted 工具 **function 化**，保留声明、历史与出站 **item 形态**，不宣称真实检索/沙箱/MCP 连接。

## 映射

| Responses | Chat 声明 name | 历史 | 出站 item |
|---|---|---|---|
| web_search / web_search_preview | `web_search` | tool_calls + tool 文本(sources) | `web_search_call` 事件链（无真实 sources 则空） |
| code_interpreter | `code_interpreter` | tool_calls(code) + tool(logs) | `code_interpreter_call` 链（logs 仅历史有） |
| mcp | `mcp__{server}__{tool}` | 同上；allowed_tools 字符串列表展开声明 | `mcp_call` 链 |
| file_search / computer / image_generation | 跳过 | dropped WARN | — |

## 不做

- Chat 上游真实 server 执行
- MCP connector_id/tunnel/审批
- code image URL / Files 拉取
