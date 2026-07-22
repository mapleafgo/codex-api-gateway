package chatconvert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	oairesponses "github.com/openai/openai-go/v3/responses"
)

// Hosted tool names used on Chat function wire (lossy; no server execution).
const (
	chatNameWebSearch       = "web_search"
	chatNameCodeInterpreter = "code_interpreter"
	mcpNamePrefix           = "mcp__"
)

func mcpChatName(serverLabel, toolName string) string {
	return mcpNamePrefix + serverLabel + "__" + toolName
}

func webSearchToolDecl() ChatTool {
	return ChatTool{
		Type: "function",
		Function: ChatFunction{
			Name:        chatNameWebSearch,
			Description: "Search the web (Chat backend: shape-only; no hosted search execution).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
				"required": []string{"query"},
			},
		},
	}
}

func codeInterpreterToolDecl() ChatTool {
	return ChatTool{
		Type: "function",
		Function: ChatFunction{
			Name:        chatNameCodeInterpreter,
			Description: "Run code (Chat backend: shape-only; no sandbox).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{"type": "string"},
				},
				"required": []string{"code"},
			},
		},
	}
}

func mcpToolDecl(serverLabel, toolName string) ChatTool {
	return ChatTool{
		Type: "function",
		Function: ChatFunction{
			Name:        mcpChatName(serverLabel, toolName),
			Description: fmt.Sprintf("MCP tool %s on server %s (Chat backend: shape-only).", toolName, serverLabel),
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func mcpDeclsFromTool(m *oairesponses.ToolMcpParam) []ChatTool {
	if m == nil || m.ServerLabel == "" {
		return nil
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
		out = append(out, mcpToolDecl(m.ServerLabel, n))
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

func codeInterpreterHistory(call *oairesponses.ResponseCodeInterpreterToolCallParam) (argsJSON, resultText string) {
	code := ""
	if call.Code.Valid() {
		code = call.Code.Value
	}
	b, _ := json.Marshal(map[string]string{"code": code})
	var logs []string
	for _, o := range call.Outputs {
		if o.OfLogs != nil && o.OfLogs.Logs != "" {
			logs = append(logs, o.OfLogs.Logs)
		} else if o.OfImage != nil {
			slog.Warn("chatconvert: 丢弃 code_interpreter 历史 image 输出",
				"url", o.OfImage.URL, "impact", "Chat 路径无 image 槽位")
			logs = append(logs, "[code_interpreter image output omitted]")
		}
	}
	body := strings.Join(logs, "\n")
	if body == "" {
		body = "[code_interpreter]"
	}
	return string(b), body
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
