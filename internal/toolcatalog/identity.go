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
	// KindBetaServerTool 需 beta API（如 MCP connector），由 convert 产出
	// beta 注入定义、client 层注入请求体（非标准 ToolUnionParam）。
	KindBetaServerTool Kind = "beta_server_tool"
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
