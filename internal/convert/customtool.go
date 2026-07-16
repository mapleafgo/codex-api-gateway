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
		if item.OfAdditionalTools != nil {
			appendFromTools(item.OfAdditionalTools.Tools)
		}
		if item.OfToolSearchOutput != nil {
			appendFromTools(item.OfToolSearchOutput.Tools)
		}
	}
	return names
}

func appendFreeformToolName(names []string, tool oairesponses.ToolUnionParam) []string {
	switch {
	case tool.OfCustom != nil:
		names = append(names, tool.OfCustom.Name)
	case tool.OfApplyPatch != nil:
		names = append(names, "apply_patch")
	case tool.OfShell != nil || tool.OfLocalShell != nil:
		names = append(names, "shell")
	case tool.OfNamespace != nil:
		namespace := tool.OfNamespace
		for _, nested := range namespace.Tools {
			if nested.OfCustom != nil {
				names = append(names, toolcatalog.ToolName(namespace.Name, nested.OfCustom.Name))
			}
		}
	}
	return names
}
