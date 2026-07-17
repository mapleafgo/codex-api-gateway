package toolcatalog

import "github.com/anthropics/anthropic-sdk-go"

// serverToolByAnthropicName 登记 Anthropic 回程 server_tool_use 的 name → 身份。
// streamconv 用它判定一个 server_tool_use 是否对应已注册的 hosted server tool；
// 批次 A 注册 code_execution 后在此追加，回程 dispatch 自动覆盖。
var serverToolByAnthropicName = map[string]Identity{
	"web_search":     {OpenAIType: "web_search", Name: "web_search", Freeform: false},
	"code_execution": {OpenAIType: "code_interpreter", Name: "code_interpreter"},
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
	case tool.OfCodeExecutionTool20250522 != nil:
		tool.OfCodeExecutionTool20250522.CacheControl = cc
	default:
		return false
	}
	return true
}
