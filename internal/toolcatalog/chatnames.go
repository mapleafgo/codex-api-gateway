package toolcatalog

// 本文件集中定义 Chat 路径（c）的 function 名契约。
//
// c 路径出站（chatconvert）把 hosted / MCP / 内建 freeform 工具合成为同名
// function 声明发给 Chat 上游；回程（chatstreamconv.classifyTool）按同一名字
// 还原为对应的 Responses 专项 item（web_search / shell / local_shell /
// apply_patch）。MCP 扁平名与普通 function/namespace 工具按
// function_call 回程，由客户端执行。出站合成与回程识别共用这里的常量：
// 改动任何一个名字都必须两侧同步。
const (
	// ChatNameWebSearch 是 web_search hosted 工具在 Chat function wire 上的名字。
	ChatNameWebSearch = "web_search"
	// ChatNameShell 是内建 freeform shell 工具在 Chat function wire 上的名字。
	ChatNameShell = "shell"
	// ChatNameLocalShell 是 local_shell 工具在 Chat function wire 上的名字。
	// 与 shell 保持不同名，回程才能区分 shell_call / local_shell_call。
	ChatNameLocalShell = "local_shell"
	// ChatNameApplyPatch 是内建 freeform apply_patch 工具在 Chat function wire 上的名字。
	ChatNameApplyPatch = "apply_patch"
	// MCPChatNamePrefix 是 MCP 工具扁平化为 Chat function 名的前缀，
	// 完整形态为 mcp__<server_label>__<tool_name>。
	MCPChatNamePrefix = "mcp__"
)
