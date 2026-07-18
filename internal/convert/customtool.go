package convert

import (
	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// FreeformToolNames 返回请求里以 freeform custom tool 形式声明的工具名。
// 这类工具（如 Codex 的 apply_patch）输入是 grammar/freeform 文本而非 JSON：
// 请求侧转成 Anthropic custom tool 后，返回侧需把模型输出解包成裸文本，
// 才能与客户端的 freeform 调用契约对齐。
func FreeformToolNames(req *oairesponses.ResponseNewParams) []string {
	var names []string
	appendFromTools := func(tools []oairesponses.ToolUnionParam) {
		for i := range tools {
			tool := tools[i]
			names = appendFreeformToolName(names, tool)
		}
	}
	appendFromTools(req.Tools)
	for i := range req.Input.OfInputItemList {
		item := req.Input.OfInputItemList[i]
		if item.OfToolSearchOutput != nil {
			appendFromTools(item.OfToolSearchOutput.Tools)
		}
	}
	return names
}

func appendFreeformToolName(names []string, tool oairesponses.ToolUnionParam) []string {
	ids, err := toolcatalog.Inspect(tool)
	if err != nil {
		return names
	}
	for _, id := range ids {
		if id.Freeform {
			names = append(names, id.ConvertedName())
		}
	}
	return names
}

// DeclaredServerTools 返回请求里声明的标准 server tool 身份（去重）。
// 回程 server_tool_use 在上游 name 失配（兼容端方言，如 GLM 的
// web_search_prime）时，用此集合做身份回退：若唯一可确定，则忽略上游 name
// 按该身份 dispatch。扫描范围与 FreeformToolNames 一致（含历史 input item
// 里携带的 OfToolSearchOutput）。
func DeclaredServerTools(req *oairesponses.ResponseNewParams) []toolcatalog.Identity {
	var ids []toolcatalog.Identity
	appendFromTools := func(tools []oairesponses.ToolUnionParam) {
		for i := range tools {
			inspected, err := toolcatalog.Inspect(tools[i])
			if err != nil {
				continue
			}
			for _, id := range inspected {
				if toolcatalog.IsServerTool(id) && !hasToolIdentity(ids, id) {
					ids = append(ids, id)
				}
			}
		}
	}
	appendFromTools(req.Tools)
	for i := range req.Input.OfInputItemList {
		item := req.Input.OfInputItemList[i]
		if item.OfToolSearchOutput != nil {
			appendFromTools(item.OfToolSearchOutput.Tools)
		}
	}
	return ids
}
