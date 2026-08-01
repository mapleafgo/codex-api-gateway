package chatconvert

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// Hosted tool names used on Chat function wire (lossy; no server execution)
// 见 toolcatalog/chatnames.go：出站合成与 chatstreamconv 回程识别共用契约。

func mcpChatName(serverLabel, toolName string) string {
	return toolcatalog.MCPChatNamePrefix + serverLabel + "__" + toolName
}

func webSearchToolDecl() ChatTool {
	return ChatTool{
		Type: "function",
		Function: ChatFunction{
			Name:        toolcatalog.ChatNameWebSearch,
			Description: "Search the web (Chat backend: shape-only; no hosted search execution).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		},
	}
}

func mcpToolDecl(serverLabel, toolName, serverDescription string) ChatTool {
	return ChatTool{
		Type: "function",
		Function: ChatFunction{
			Name:        mcpChatName(serverLabel, toolName),
			Description: serverDescription,
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
	}
}

func mcpDeclsFromTool(m *oairesponses.ToolMcpParam) []ChatTool {
	if m == nil || m.ServerLabel == "" {
		return nil
	}
	if toolcatalog.HasMCPConnectionFields(m) {
		slog.Debug("chatconvert: MCP 连接字段由 Codex 客户端本地使用，不注入 Chat 上游",
			"server_label", m.ServerLabel)
	}
	names := m.AllowedTools.OfMcpAllowedTools
	if len(names) == 0 && m.AllowedTools.OfMcpToolFilter != nil {
		// filter 形态不展开
		slog.Warn("chatconvert: MCP allowed_tools filter 不展开为 Chat function 声明",
			"server_label", m.ServerLabel)
		return nil
	}
	if len(names) == 0 {
		slog.Debug("chatconvert: MCP 无 allowed_tools 列表，仅依赖历史 mcp_call 名称",
			"server_label", m.ServerLabel)
		return nil
	}
	out := make([]ChatTool, 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		out = append(out, mcpToolDecl(m.ServerLabel, n, optString(m.ServerDescription)))
	}
	return out
}

func webSearchHistoryArgs(call *oairesponses.ResponseFunctionWebSearchParam) (argsJSON, resultText string) {
	query, urls := webSearchQueryAndURLs(call)
	b, _ := json.Marshal(map[string]string{"query": query})
	var parts []string
	if query != "" {
		parts = append(parts, "[web_search query]\n"+query)
	}
	if len(urls) > 0 {
		parts = append(parts, "[web_search sources]\n"+strings.Join(urls, "\n"))
	}
	if len(parts) == 0 {
		parts = append(parts, "[web_search]")
	}
	return string(b), strings.Join(parts, "\n")
}

//nolint:staticcheck // OfSearch.Query deprecated fallback
func webSearchQueryAndURLs(call *oairesponses.ResponseFunctionWebSearchParam) (string, []string) {
	if call == nil {
		return "", nil
	}
	query := ""
	var urls []string
	switch {
	case call.Action.OfSearch != nil:
		a := call.Action.OfSearch
		if len(a.Queries) > 0 {
			query = strings.Join(a.Queries, "\n")
		} else if a.Query.Valid() && a.Query.Value != "" {
			query = a.Query.Value
		}
		for _, s := range a.Sources {
			if s.URL != "" {
				urls = append(urls, s.URL)
			}
		}
	case call.Action.OfOpenPage != nil:
		if call.Action.OfOpenPage.URL.Valid() {
			query = call.Action.OfOpenPage.URL.Value
			urls = append(urls, query)
		}
	case call.Action.OfFind != nil:
		a := call.Action.OfFind
		parts := make([]string, 0, 2)
		if a.URL != "" {
			parts = append(parts, a.URL)
			urls = append(urls, a.URL)
		}
		if a.Pattern != "" {
			parts = append(parts, a.Pattern)
		}
		query = strings.Join(parts, "\n")
	}
	return query, urls
}

func mcpHistoryArgs(call *oairesponses.ResponseInputItemMcpCallParam) (name, args, result string) {
	name = mcpChatName(call.ServerLabel, call.Name)
	args = call.Arguments
	if args == "" {
		args = "{}"
	}
	if call.Error.Valid() && call.Error.Value != "" {
		result = call.Error.Value
	} else if call.Output.Valid() {
		result = call.Output.Value
	} else {
		result = "[mcp_call]"
	}
	return name, args, result
}
