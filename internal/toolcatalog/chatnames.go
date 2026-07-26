package toolcatalog

// 本文件集中定义 Chat 路径（c）的 function 名契约。
//
// c 路径出站（chatconvert）把 hosted / MCP / 内建 freeform 工具合成为同名
// function 声明发给 Chat 上游；回程（chatstreamconv.classifyTool）按同一名字
// 把 tool_call 还原为对应的 Responses hosted / custom item。出站合成与回程
// 识别共用这里的常量：改动任何一个名字都必须两侧同步，否则回程会把
// hosted 调用误判成普通 function_call。
const (
	// ChatNameWebSearch 是 web_search hosted 工具在 Chat function wire 上的名字。
	ChatNameWebSearch = "web_search"
	// ChatNameCodeInterpreter 是 code_interpreter hosted 工具在 Chat function wire 上的名字。
	ChatNameCodeInterpreter = "code_interpreter"
	// ChatNameShell 是内建 freeform shell 工具在 Chat function wire 上的名字。
	ChatNameShell = "shell"
	// ChatNameApplyPatch 是内建 freeform apply_patch 工具在 Chat function wire 上的名字。
	ChatNameApplyPatch = "apply_patch"
	// MCPChatNamePrefix 是 MCP 工具扁平化为 Chat function 名的前缀，
	// 完整形态为 mcp__<server_label>__<tool_name>。
	MCPChatNamePrefix = "mcp__"
)
