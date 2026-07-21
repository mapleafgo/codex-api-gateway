package toolcatalog

import "github.com/anthropics/anthropic-sdk-go"

// serverToolByAnthropicName 登记 Anthropic 回程 server_tool_use 的 name → 身份。
// streamconv 用它判定一个 server_tool_use 是否对应已注册的 hosted server tool；
// code_execution 已注册；新增 server tool 在此追加，回程 dispatch 自动覆盖。
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

// serverToolOpenAITypes 是已注册 server tool 的 OpenAIType 集合，由
// serverToolByAnthropicName 的 value 推导。IsServerTool 据此判定一个请求侧
// 身份是否为标准 server tool；新增 server tool 自动覆盖，无需另行登记。
var serverToolOpenAITypes = func() map[string]bool {
	m := make(map[string]bool, len(serverToolByAnthropicName))
	for _, id := range serverToolByAnthropicName {
		m[id.OpenAIType] = true
	}
	return m
}()

// IsServerTool 报告一个请求侧 Identity 是否为标准 server tool（web_search /
// code_interpreter）。回程用它从请求声明的工具里筛出 server tool 身份：
// 当上游 name 失配（兼容端方言，如 GLM 的 web_search_prime）时，若声明的
// server tool 唯一可确定，则忽略 name 按此身份回退 dispatch。beta server
// tool（MCP）走独立 probe 路径，不计入。
func IsServerTool(id Identity) bool {
	return serverToolOpenAITypes[id.OpenAIType]
}

// ApplyCacheControl 把 cache_control 写入一个 Anthropic tool union 的对应变体。
// 返回是否成功识别变体；未识别返回 false（调用方 WARN，避免静默丢失缓存）。
// 通过 SDK GetCacheControl 统一派发，覆盖全部 ToolUnion 变体，无需手写 switch。
func ApplyCacheControl(tool *anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam) bool {
	if tool == nil {
		return false
	}
	p := tool.GetCacheControl()
	if p == nil {
		return false
	}
	*p = cc
	return true
}
