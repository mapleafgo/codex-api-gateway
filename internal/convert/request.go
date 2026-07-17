package convert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	aparam "github.com/anthropics/anthropic-sdk-go/packages/param"
	anthropicclient "github.com/mapleafgo/codex-api-gateway/internal/anthropic"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// DecodeResponseNewParams decodes a Responses request and restores union shapes
// that openai-go cannot infer losslessly from plain JSON.
func DecodeResponseNewParams(data []byte) (*oairesponses.ResponseNewParams, error) {
	if err := validateNamespaceToolChildren(data); err != nil {
		return nil, err
	}
	var req oairesponses.ResponseNewParams
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, err
	}
	restoreToolChoiceFromRaw(data, &req)
	return &req, nil
}

func restoreToolChoiceFromRaw(data []byte, req *oairesponses.ResponseNewParams) {
	var raw struct {
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || len(raw.ToolChoice) == 0 {
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw.ToolChoice, &obj); err != nil {
		return
	}
	var typ string
	if err := json.Unmarshal(obj["type"], &typ); err != nil {
		return
	}
	var name string
	_ = json.Unmarshal(obj["name"], &name)
	switch typ {
	case "function":
		if name != "" {
			req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
				OfFunctionTool: &oairesponses.ToolChoiceFunctionParam{Name: name},
			}
		}
	case "custom":
		if name != "" {
			req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
				OfCustomTool: &oairesponses.ToolChoiceCustomParam{Name: name},
			}
		}
	case "apply_patch":
		req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
			OfSpecificApplyPatchToolChoice: &oairesponses.ToolChoiceApplyPatchParam{},
		}
	case "shell":
		req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
			OfSpecificShellToolChoice: &oairesponses.ToolChoiceShellParam{},
		}
	default:
		if !toolChoiceExplicit(req.ToolChoice) {
			req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
				OfToolChoiceMode: oparam.NewOpt(oairesponses.ToolChoiceOptions(typ)),
			}
		}
	}
}

func validateNamespaceToolChildren(data []byte) error {
	var raw struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, rawTool := range raw.Tools {
		var tool struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(rawTool, &tool); err != nil {
			return err
		}
		if tool.Type != "namespace" {
			continue
		}
		for _, rawChild := range tool.Tools {
			var child struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(rawChild, &child); err != nil {
				return err
			}
			if child.Type != "function" && child.Type != "custom" {
				return fmt.Errorf("unsupported namespace tool type %q: Anthropic backend has no safe equivalent", child.Type)
			}
		}
	}
	return nil
}

// ToAnthropic converts a Response request into an Anthropic Messages request.
// prevItems carries stored output items from a previous turn; plaintext thinking
// signatures are looked up by reasoning item ID and injected into ThinkingBlockParam.
// 第二个返回值是 MCP beta 注入定义（mcp_servers + mcp_toolset），由 collectMCP 产出；
// 非 nil 时由 client 层注入到 marshal 后的请求体（SDK 不支持这组字段）。
func ToAnthropic(req *oairesponses.ResponseNewParams, cfg *config.Config, prevItems ...model.OutputItem) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
	// Build a signature lookup from stored reasoning items.
	sigByID := map[string]string{}
	for _, it := range prevItems {
		if it.Type == model.ItemTypeReasoning && it.Signature != "" {
			sigByID[it.ID] = it.Signature
		}
	}

	out := &anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: 4096,
	}
	if req.MaxOutputTokens.Valid() && req.MaxOutputTokens.Value > 0 {
		out.MaxTokens = req.MaxOutputTokens.Value
	}
	if req.Temperature.Valid() {
		out.Temperature = aparam.NewOpt(req.Temperature.Value)
	}
	if req.TopP.Valid() {
		out.TopP = aparam.NewOpt(req.TopP.Value)
	}

	var sysParts []instructionPart

	// Input can be a plain string or a list of items.
	if req.Input.OfString.Valid() && req.Input.OfString.Value != "" {
		out.Messages = append(out.Messages, anthropic.NewUserMessage(
			anthropic.ContentBlockParamUnion{OfText: &anthropic.TextBlockParam{Text: req.Input.OfString.Value}},
		))
	}
	// Trim historical reasoning to the most recent item. Anthropic's extended
	// thinking best practice is to carry only the latest thinking block across
	// turns — older ones add tokens and attention noise without helping the
	// model, and a large accumulated thinking context pushes upstream models
	// toward early end_turn. All reasoning signatures remain available via
	// sigByID, so the preserved block still resolves its correct signature.
	lastReasoning := -1
	for i := range req.Input.OfInputItemList {
		if req.Input.OfInputItemList[i].OfReasoning != nil {
			lastReasoning = i
		}
	}
	for i := range req.Input.OfInputItemList {
		item := &req.Input.OfInputItemList[i]
		if item.OfReasoning != nil && i != lastReasoning {
			continue
		}
		if err := appendItem(out, &sysParts, item, sigByID); err != nil {
			return nil, nil, fmt.Errorf("convert input item: %w", err)
		}
	}

	// Instructions fold into System as a separate text block.
	if req.Instructions.Valid() && req.Instructions.Value != "" {
		sysParts = append([]instructionPart{{role: model.RoleDeveloper, text: req.Instructions.Value}}, sysParts...)
	}
	if systemText := formatInstructionParts(sysParts); systemText != "" {
		out.System = []anthropic.TextBlockParam{{Text: systemText}}
	}

	applyReasoning(out, req, cfg)

	if err := convertTools(out, req); err != nil {
		return nil, nil, err
	}
	if err := injectStructuredOutput(out, req); err != nil {
		return nil, nil, err
	}
	if err := convertToolChoice(out, req); err != nil {
		return nil, nil, err
	}
	applyAnthropicCacheControl(out, cfg)

	mcp, err := collectMCP(req)
	if err != nil {
		return nil, nil, err
	}
	return out, mcp, nil
}

// collectMCP 扫描请求里的 mcp tool，产出 beta MCPInjection（mcp_servers + toolset）。
// 字段映射见 spec 2.2；损失处理见 spec 2.4。
// connector_id / tunnel_id 是 OpenAI 私有托管设施，不在 Anthropic 标准范围 → fail-fast。
func collectMCP(req *oairesponses.ResponseNewParams) (*anthropicclient.MCPInjection, error) {
	var inj anthropicclient.MCPInjection
	for _, t := range req.Tools {
		if t.OfMcp == nil {
			continue
		}
		m := t.OfMcp
		if m.ConnectorID != "" {
			return nil, fmt.Errorf("mcp connector_id %q is not supported: use server_url form instead", m.ConnectorID)
		}
		if m.TunnelID.Valid() && m.TunnelID.Value != "" {
			return nil, fmt.Errorf("mcp tunnel_id %q is not supported: use server_url form instead", m.TunnelID.Value)
		}
		serverURL := ""
		if m.ServerURL.Valid() {
			serverURL = m.ServerURL.Value
		}
		if serverURL == "" {
			return nil, fmt.Errorf("mcp server %q requires server_url (connector_id/tunnel_id unsupported)", m.ServerLabel)
		}
		token := ""
		if m.Authorization.Valid() {
			token = m.Authorization.Value
		}
		// headers：择优提取 Authorization: Bearer → authorization_token（authorization 空时回退）。
		if bearer, ok := m.Headers["Authorization"]; ok {
			if token != "" {
				slog.Warn("MCP server 同时设置 authorization 字段与 headers[Authorization]，headers 值被忽略",
					"server_label", m.ServerLabel)
			} else {
				token = strings.TrimPrefix(bearer, "Bearer ")
			}
		}
		for k := range m.Headers {
			if k != "Authorization" {
				slog.Warn("丢弃 MCP server 自定义 header（Anthropic 仅支持单一 authorization_token）",
					"server_label", m.ServerLabel, "header", k)
			}
		}
		// require_approval：Anthropic MCP 无审批协议。never/缺省正常；其余降级 never + WARN。
		if appr := approvalMode(m.RequireApproval); appr != "" && appr != "never" {
			slog.Warn("MCP require_approval 降级为 never（Anthropic 无审批协议，工具将直接执行）",
				"server_label", m.ServerLabel, "require_approval", appr)
		}
		inj.Servers = append(inj.Servers, anthropicclient.MCPServer{
			Type: "url", URL: serverURL, Name: m.ServerLabel, AuthorizationToken: token,
		})
		if m.AllowedTools.OfMcpToolFilter != nil {
			slog.Warn("mcp allowed_tools filter 不支持精确映射，降级为全启用（toolset default_config.enabled=true）",
				"server_label", m.ServerLabel)
		}
		enabled := allowedMCPToolNames(m.AllowedTools)
		inj.Toolsets = append(inj.Toolsets, anthropicclient.MCPToolset{
			MCPServerName: m.ServerLabel, EnabledTools: enabled,
		})
	}
	if inj.Empty() {
		return nil, nil
	}
	return &inj, nil
}

// approvalMode 从 ToolMcpRequireApprovalUnionParam 取出审批模式字符串（"" 表缺省=never）。
// SDK：OfMcpToolApprovalSetting 是 param.Opt[string]（值如 "never"/"on_failure"/"if_referenced"），
// OfMcpToolApprovalFilter 是 filter 对象（近似需审批，降级为 on_failure）。
func approvalMode(u oairesponses.ToolMcpRequireApprovalUnionParam) string {
	if u.OfMcpToolApprovalSetting.Valid() {
		return u.OfMcpToolApprovalSetting.Value
	}
	if u.OfMcpToolApprovalFilter != nil {
		return "on_failure"
	}
	return ""
}

// allowedMCPToolNames 从 allowed_tools union 取出命中的工具名列表。
// SDK：OfMcpAllowedTools 是 []string（allowlist）；OfMcpToolFilter 是 filter 对象（本批不展开）。
func allowedMCPToolNames(u oairesponses.ToolMcpAllowedToolsUnionParam) []string {
	return u.OfMcpAllowedTools
}

type instructionPart struct {
	role string
	text string
}

func appendItem(out *anthropic.MessageNewParams, sysParts *[]instructionPart, item *oairesponses.ResponseInputItemUnionParam, sigByID map[string]string) error {
	if item.OfMessage != nil {
		return appendMessage(out, sysParts, item.OfMessage)
	}
	if item.OfReasoning != nil {
		return appendReasoning(out, item.OfReasoning, sigByID)
	}
	if item.OfFunctionCall != nil {
		return appendFunctionCall(out, item.OfFunctionCall)
	}
	if item.OfFunctionCallOutput != nil {
		return appendFunctionCallOutput(out, item.OfFunctionCallOutput)
	}
	if item.OfCustomToolCall != nil {
		return appendCustomToolCall(out, item.OfCustomToolCall)
	}
	if item.OfCustomToolCallOutput != nil {
		return appendCustomToolCallOutput(out, item.OfCustomToolCallOutput)
	}
	if item.OfToolSearchCall != nil {
		return appendToolSearchCall(out, item.OfToolSearchCall)
	}
	if item.OfToolSearchOutput != nil {
		return appendToolSearchOutput(out, sysParts, item.OfToolSearchOutput)
	}
	if item.OfCodeInterpreterCall != nil {
		return appendCodeInterpreterCall(out, item.OfCodeInterpreterCall)
	}
	// 历史 MCP items（mcp_call / mcp_list_tools / mcp_approval_request / mcp_approval_response）
	// 无标准 Anthropic 请求侧 content block 变体（ContentBlockParamUnion 无 OfMCPToolUse 等），
	// 回灌暂不支持 → 显式丢弃 + WARN（避免 raw JSON 污染 system context）。
	if item.OfMcpCall != nil || item.OfMcpListTools != nil ||
		item.OfMcpApprovalRequest != nil || item.OfMcpApprovalResponse != nil {
		slog.Warn("丢弃历史 MCP item（Anthropic 请求侧无标准 mcp block 变体，回灌暂不支持）",
			"item_type", mcpHistoryItemType(item), "item_id", mcpHistoryItemID(item))
		return nil
	}
	if item.OfLocalShellCall != nil {
		return appendLocalShellCall(out, item.OfLocalShellCall)
	}
	if item.OfLocalShellCallOutput != nil {
		return appendToolResult(out, item.OfLocalShellCallOutput.ID, item.OfLocalShellCallOutput.Output)
	}
	if item.OfShellCall != nil {
		return appendShellCall(out, item.OfShellCall)
	}
	if item.OfShellCallOutput != nil {
		return appendToolResult(out, item.OfShellCallOutput.CallID, shellOutputText(item.OfShellCallOutput.Output))
	}
	if item.OfApplyPatchCall != nil {
		return appendApplyPatchCall(out, item.OfApplyPatchCall)
	}
	if item.OfApplyPatchCallOutput != nil {
		output := ""
		if item.OfApplyPatchCallOutput.Output.Valid() {
			output = item.OfApplyPatchCallOutput.Output.Value
		}
		return appendToolResult(out, item.OfApplyPatchCallOutput.CallID, output)
	}
	if item.OfAdditionalTools != nil {
		if err := appendToolList(out, item.OfAdditionalTools.Tools); err != nil {
			return err
		}
		*sysParts = append(*sysParts, instructionPart{
			role: model.RoleDeveloper,
			text: formatToolNames("developer_tools", item.OfAdditionalTools.Tools),
		})
		return nil
	}
	if item.OfCompaction != nil {
		*sysParts = append(*sysParts, instructionPart{
			role: model.RoleSystem,
			text: "<compaction>\n" + item.OfCompaction.EncryptedContent + "\n</compaction>",
		})
		return nil
	}
	if item.OfCompactionTrigger != nil {
		*sysParts = append(*sysParts, instructionPart{
			role: model.RoleSystem,
			text: "<compaction_trigger />",
		})
		return nil
	}
	if part, ok := unknownInputItemPart(item); ok {
		*sysParts = append(*sysParts, part)
	}
	return nil
}

func unknownInputItemPart(item *oairesponses.ResponseInputItemUnionParam) (instructionPart, bool) {
	raw, err := json.Marshal(item)
	if err != nil || string(raw) == "{}" || string(raw) == "null" {
		return instructionPart{}, false
	}
	typ := ""
	if ptr := item.GetType(); ptr != nil {
		typ = *ptr
	}
	if typ == "" {
		var obj struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &obj)
		typ = obj.Type
	}
	if typ == "" {
		typ = "unknown"
	}
	return instructionPart{
		role: model.RoleSystem,
		text: fmt.Sprintf("<openai_input_item type=\"%s\">\n%s\n</openai_input_item>", typ, raw),
	}, true
}

func appendMessage(out *anthropic.MessageNewParams, sysParts *[]instructionPart, m *oairesponses.EasyInputMessageParam) error {
	// Extract text/image blocks from content.
	var blocks []anthropic.ContentBlockParamUnion
	var textParts []string

	if m.Content.OfString.Valid() && m.Content.OfString.Value != "" {
		textParts = append(textParts, m.Content.OfString.Value)
	}
	for _, cp := range m.Content.OfInputItemContentList {
		if cp.OfInputText != nil {
			textParts = append(textParts, cp.OfInputText.Text)
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: cp.OfInputText.Text},
			})
		} else if cp.OfInputImage != nil {
			// Only the url/data-URI variant is mapped. The file_id variant
			// (OpenAI Files) is dropped: Anthropic image blocks take
			// base64/url only, and the gateway has no OpenAI credentials to
			// fetch the file. See README "Known limitations".
			if cp.OfInputImage.ImageURL.Valid() {
				blocks = append(blocks, imageBlock(cp.OfInputImage.ImageURL.Value))
			}
		} else if cp.OfInputFile != nil {
			if block := documentBlock(cp.OfInputFile); block != nil {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{OfDocument: block})
			}
		}
	}

	role := string(m.Role)

	// system/developer fold into top-level System.
	// NOTE: image blocks in system/developer messages are silently dropped here.
	// Anthropic's system parameter is []TextBlockParam (text-only), so images
	// cannot be represented in the system role. This is a protocol limitation.
	if role == model.RoleSystem || role == model.RoleDeveloper {
		*sysParts = append(*sysParts, instructionPart{
			role: role,
			text: joinNonEmpty("\n", textParts),
		})
		return nil
	}

	// For plain string content with no explicit content parts, use text blocks.
	if len(blocks) == 0 && len(textParts) > 0 {
		for _, t := range textParts {
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: t},
			})
		}
	}

	if len(blocks) == 0 {
		blocks = []anthropic.ContentBlockParamUnion{{OfText: &anthropic.TextBlockParam{}}}
	}

	out.Messages = append(out.Messages, anthropic.MessageParam{
		Role:    anthropic.MessageParamRole(role),
		Content: blocks,
	})
	return nil
}

func appendReasoning(out *anthropic.MessageNewParams, r *oairesponses.ResponseReasoningItemParam, sigByID map[string]string) error {
	text := ""
	if len(r.Summary) > 0 {
		text = r.Summary[0].Text
	}
	// encrypted_content 有值时，需要区分两种来源：
	//   1) redacted_thinking（无 summary 文本）-> attachRedactedThinking
	//   2) plaintext thinking 的 signature（有 summary 文本）-> attachThinking
	//     这是为 disable_response_storage=true 场景设计的：converter 把 thinking
	//     signature 写入 encrypted_content，让 Codex 通过标准字段回传。
	if r.EncryptedContent.Valid() && r.EncryptedContent.Value != "" {
		if text != "" {
			attachThinking(out, text, r.EncryptedContent.Value)
		} else {
			attachRedactedThinking(out, r.EncryptedContent.Value)
		}
		return nil
	}
	// 无 encrypted_content 时走 session enrich 路径（disable_response_storage=false），
	// reasoning item 由 Enrich 从 session store 回填，signature 在 sigByID 中查找。
	sig := sigByID[r.ID]
	attachThinking(out, text, sig)
	return nil
}

func attachRedactedThinking(out *anthropic.MessageNewParams, data string) {
	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleAssistant {
		out.Messages = append(out.Messages, anthropic.NewAssistantMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content = append([]anthropic.ContentBlockParamUnion{{
		OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: data},
	}}, last.Content...)
}

func attachThinking(out *anthropic.MessageNewParams, text, signature string) {
	if len(out.Messages) == 0 {
		out.Messages = append(out.Messages, anthropic.NewAssistantMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	if last.Role != anthropic.MessageParamRoleAssistant {
		out.Messages = append(out.Messages, anthropic.NewAssistantMessage())
		last = &out.Messages[len(out.Messages)-1]
	}
	last.Content = append([]anthropic.ContentBlockParamUnion{{
		OfThinking: &anthropic.ThinkingBlockParam{Thinking: text, Signature: signature},
	}}, last.Content...)
}

func appendFunctionCall(out *anthropic.MessageNewParams, fc *oairesponses.ResponseFunctionToolCallParam) error {
	return appendToolUse(out, fc.CallID, toolcatalog.ToolName(fc.Namespace.Value, fc.Name), json.RawMessage(orDefault(fc.Arguments, `{}`)))
}

func appendCustomToolCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseCustomToolCallParam) error {
	return appendToolUse(out, call.CallID, toolcatalog.ToolName(call.Namespace.Value, call.Name), map[string]any{"input": call.Input})
}

func appendShellCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemShellCallParam) error {
	input := strings.Join(call.Action.Commands, "\n")
	return appendToolUse(out, call.CallID, "shell", map[string]any{"input": input})
}

func appendLocalShellCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemLocalShellCallParam) error {
	input := strings.Join(call.Action.Command, " ")
	return appendToolUse(out, call.CallID, "shell", map[string]any{"input": input})
}

func appendApplyPatchCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemApplyPatchCallParam) error {
	var input map[string]any
	switch {
	case call.Operation.OfCreateFile != nil:
		input = map[string]any{
			"operation": "create_file",
			"path":      call.Operation.OfCreateFile.Path,
			"diff":      call.Operation.OfCreateFile.Diff,
		}
	case call.Operation.OfUpdateFile != nil:
		input = map[string]any{
			"operation": "update_file",
			"path":      call.Operation.OfUpdateFile.Path,
			"diff":      call.Operation.OfUpdateFile.Diff,
		}
	case call.Operation.OfDeleteFile != nil:
		input = map[string]any{
			"operation": "delete_file",
			"path":      call.Operation.OfDeleteFile.Path,
		}
	default:
		return fmt.Errorf("apply_patch call %q has an invalid operation", call.CallID)
	}
	return appendToolUse(out, call.CallID, "apply_patch", input)
}

func shellOutputText(parts []oairesponses.ResponseFunctionShellCallOutputContentParam) string {
	output := make([]string, 0, len(parts)*2)
	for _, part := range parts {
		if part.Stdout != "" {
			output = append(output, part.Stdout)
		}
		if part.Stderr != "" {
			output = append(output, part.Stderr)
		}
	}
	return strings.Join(output, "\n")
}

func appendToolUse(out *anthropic.MessageNewParams, id, name string, input any) error {
	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleAssistant {
		out.Messages = append(out.Messages, anthropic.NewAssistantMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, anthropic.ContentBlockParamUnion{
		OfToolUse: &anthropic.ToolUseBlockParam{
			ID:    id,
			Name:  name,
			Input: input,
		},
	})
	return nil
}

func appendFunctionCallOutput(out *anthropic.MessageNewParams, fco *oairesponses.ResponseInputItemFunctionCallOutputParam) error {
	outputText := ""
	if fco.Output.OfString.Valid() {
		outputText = fco.Output.OfString.Value
	}
	return appendToolResult(out, fco.CallID, outputText)
}

func appendCustomToolCallOutput(out *anthropic.MessageNewParams, output *oairesponses.ResponseCustomToolCallOutputParam) error {
	outputText := ""
	if output.Output.OfString.Valid() {
		outputText = output.Output.OfString.Value
	}
	return appendToolResult(out, output.CallID, outputText)
}

func appendToolSearchCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemToolSearchCallParam) error {
	callID := call.CallID.Value
	if callID == "" {
		callID = call.ID.Value
	}
	return appendToolUse(out, callID, "tool_search", toolSearchArgumentsInput(call.Arguments))
}

// toolSearchArgumentsInput 把 tool_search_call 的 arguments 转成 Anthropic
// tool_use input（必须 JSON object）。Codex 回灌历史时 arguments 通常是 JSON
// 字符串，若直接当 input，上游会收到字符串而非对象 → "'str' object has no
// attribute 'get'" 500。string/nil → json.RawMessage（保持 object 形态）。
func toolSearchArgumentsInput(args any) any {
	switch v := args.(type) {
	case string:
		return json.RawMessage(orDefault(v, `{}`))
	case nil:
		return json.RawMessage(`{}`)
	default:
		return v // 已是 object/map，原样透传
	}
}

func appendToolSearchOutput(out *anthropic.MessageNewParams, sysParts *[]instructionPart, output *oairesponses.ResponseToolSearchOutputItemParam) error {
	names := formatToolNames("tool_search_output", output.Tools)
	// tool_search 多轮回灌可能含重复 tool（不同轮搜到同一工具），跳过已声明的。
	for _, t := range output.Tools {
		decls, err := toolcatalog.Declare(t)
		if err != nil {
			return err
		}
		for _, d := range decls {
			if d.OfTool != nil && hasTool(out, d.OfTool.Name) {
				continue // 跳过已声明（多轮重复）
			}
			out.Tools = append(out.Tools, d)
		}
	}
	*sysParts = append(*sysParts, instructionPart{
		role: model.RoleDeveloper,
		text: names,
	})
	if !output.CallID.Valid() || output.CallID.Value == "" {
		return nil
	}
	return appendToolResult(out, output.CallID.Value, names)
}

// appendCodeInterpreterCall 把历史 code_interpreter_call input item 回放为 Anthropic
// 历史 content block：server_tool_use(code_execution, input={code}) + code_execution_tool_result。
// container_id 丢弃（Anthropic code execution 无 container 概念）。
// image 输出（OfImage）不可转换，直接忽略——回灌静默。
func appendCodeInterpreterCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseCodeInterpreterToolCallParam) error {
	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleAssistant {
		out.Messages = append(out.Messages, anthropic.NewAssistantMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, anthropic.NewServerToolUseBlock(
		call.ID, map[string]any{"code": call.Code.Value}, anthropic.ServerToolUseBlockParamNameCodeExecution,
	))

	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleUser {
		out.Messages = append(out.Messages, anthropic.NewUserMessage())
	}
	last = &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, anthropic.NewCodeExecutionToolResultBlock(
		anthropic.CodeExecutionResultBlockParam{Stdout: codeInterpreterLogs(call.Outputs)},
		call.ID,
	))
	return nil
}

// codeInterpreterLogs 把 code_interpreter_call 的 logs outputs 拼成单段 stdout 文本。
func codeInterpreterLogs(outputs []oairesponses.ResponseCodeInterpreterToolCallOutputUnionParam) string {
	var parts []string
	for _, o := range outputs {
		if o.OfLogs != nil && o.OfLogs.Logs != "" {
			parts = append(parts, o.OfLogs.Logs)
		}
	}
	return strings.Join(parts, "\n")
}

// mcpHistoryItemType 返回历史 MCP input item 的人类可读类型标签，用于 WARN 日志。
func mcpHistoryItemType(item *oairesponses.ResponseInputItemUnionParam) string {
	switch {
	case item.OfMcpCall != nil:
		return "mcp_call"
	case item.OfMcpListTools != nil:
		return "mcp_list_tools"
	case item.OfMcpApprovalRequest != nil:
		return "mcp_approval_request"
	case item.OfMcpApprovalResponse != nil:
		return "mcp_approval_response"
	}
	return "unknown"
}

// mcpHistoryItemID 返回历史 MCP input item 的标识符（call_id / approval_request_id），用于 WARN 日志。
func mcpHistoryItemID(item *oairesponses.ResponseInputItemUnionParam) string {
	switch {
	case item.OfMcpCall != nil:
		return item.OfMcpCall.ID
	case item.OfMcpListTools != nil:
		return item.OfMcpListTools.ID
	case item.OfMcpApprovalRequest != nil:
		return item.OfMcpApprovalRequest.ID
	case item.OfMcpApprovalResponse != nil:
		return item.OfMcpApprovalResponse.ApprovalRequestID
	}
	return ""
}

func appendToolResult(out *anthropic.MessageNewParams, callID, outputText string) error {
	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleUser {
		out.Messages = append(out.Messages, anthropic.NewUserMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: callID,
			Content: []anthropic.ToolResultBlockParamContentUnion{{
				OfText: &anthropic.TextBlockParam{Text: outputText},
			}},
		},
	})
	return nil
}

func applyReasoning(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams, cfg *config.Config) {
	effort := string(req.Reasoning.Effort)
	if effort == "" {
		return
	}
	if effort == model.ReasoningEffortNone {
		out.Thinking = anthropic.ThinkingConfigParamUnion{
			OfDisabled: &anthropic.ThinkingConfigDisabledParam{},
		}
		return
	}

	budget := int64(cfg.EffortBudget(effort))
	out.Thinking = anthropic.ThinkingConfigParamUnion{
		OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: budget},
	}

	// Anthropic requires thinking.budget_tokens < max_tokens.
	if out.MaxTokens <= budget {
		out.MaxTokens = budget + 4096
	}

	// reasoning.summary=concise -> summarized thinking display.
	if string(req.Reasoning.Summary) == model.ReasoningSummaryConcise {
		out.Thinking.OfEnabled.Display = anthropic.ThinkingConfigEnabledDisplaySummarized
	}
}

func convertTools(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) error {
	return appendToolList(out, req.Tools)
}

func appendToolList(out *anthropic.MessageNewParams, tools []oairesponses.ToolUnionParam) error {
	for _, t := range tools {
		decls, err := toolcatalog.Declare(t)
		if err != nil {
			return err
		}
		for _, d := range decls {
			if d.OfTool != nil && hasTool(out, d.OfTool.Name) {
				return fmt.Errorf("tool conversion name conflict for %q", d.OfTool.Name)
			}
			out.Tools = append(out.Tools, d)
		}
	}
	return nil
}

func appendConvertedTool(out *anthropic.MessageNewParams, name string, schema map[string]any, description *string, custom bool) error {
	if name == "" {
		return fmt.Errorf("tool conversion requires a name")
	}
	if hasTool(out, name) {
		return fmt.Errorf("tool conversion name conflict for %q", name)
	}
	out.Tools = append(out.Tools, toolcatalog.ClientTool(name, schema, description, custom))
	return nil
}

func hasTool(out *anthropic.MessageNewParams, name string) bool {
	for _, tool := range out.Tools {
		if tool.OfTool != nil && tool.OfTool.Name == name {
			return true
		}
	}
	return false
}

func optionalString(v oparam.Opt[string]) *string {
	if !v.Valid() {
		return nil
	}
	return &v.Value
}

func injectStructuredOutput(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) error {
	if req.Text.Format.OfJSONSchema != nil {
		f := req.Text.Format.OfJSONSchema
		description := optionalString(f.Description)
		if err := appendConvertedTool(out, f.Name, f.Schema, description, false); err != nil {
			return err
		}
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: f.Name},
		}
		return nil
	}
	if req.Text.Format.OfJSONObject != nil {
		if err := appendConvertedTool(out, model.StructuredOutputJSONObjectTool, map[string]any{"type": "object"}, nil, false); err != nil {
			return err
		}
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: model.StructuredOutputJSONObjectTool},
		}
	}
	return nil
}

func convertToolChoice(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) error {
	tc := req.ToolChoice
	switch {
	case tc.OfHostedTool != nil:
		return fmt.Errorf("unsupported tool_choice %q: hosted tools are not supported by this Anthropic backend", *tc.GetType())
	case tc.OfMcpTool != nil:
		return fmt.Errorf("unsupported tool_choice %q: MCP tool choice is not supported by this Anthropic backend", *tc.GetType())
	case tc.OfResponseNewsToolChoiceSpecificProgrammaticToolCallingParam != nil:
		return fmt.Errorf("unsupported tool_choice %q: programmatic tool calling is not supported by this Anthropic backend", *tc.GetType())
	}
	if tc.OfAllowedTools != nil {
		if out.ToolChoice.OfTool != nil {
			_, err := allowedToolNames(req.Tools, tc.OfAllowedTools)
			if err != nil {
				return err
			}
			return fmt.Errorf("structured output cannot be combined with allowed_tools: Anthropic has no equivalent constrained forced-tool mode")
		}
		if err := applyAllowedTools(out, req.Tools, tc.OfAllowedTools); err != nil {
			return err
		}
		applyParallelToolChoice(out, req)
		return nil
	}
	if out.ToolChoice.OfTool != nil {
		if !toolChoiceExplicit(tc) {
			return nil
		}
		if structuredToolChoiceEquivalent(out.ToolChoice.OfTool.Name, tc) {
			return nil
		}
		return fmt.Errorf("structured output cannot be combined with explicit tool_choice: Anthropic has no equivalent forced-tool mode")
	}
	defer applyParallelToolChoice(out, req)
	if tc.OfFunctionTool != nil {
		return applySpecificToolChoice(out, req.Tools, toolcatalog.Identity{OpenAIType: "function", Name: tc.OfFunctionTool.Name})
	}
	if tc.OfCustomTool != nil {
		return applySpecificToolChoice(out, req.Tools, toolcatalog.Identity{OpenAIType: "custom", Name: tc.OfCustomTool.Name})
	}
	if tc.OfSpecificApplyPatchToolChoice != nil {
		return applySpecificToolChoice(out, req.Tools, toolcatalog.Identity{OpenAIType: "apply_patch", Name: "apply_patch"})
	}
	if tc.OfSpecificShellToolChoice != nil {
		return applySpecificToolChoice(out, req.Tools, toolcatalog.Identity{OpenAIType: "shell", Name: "shell"})
	}
	if len(out.Tools) == 0 {
		return nil
	}
	if tc.OfToolChoiceMode.Valid() {
		switch string(tc.OfToolChoiceMode.Value) {
		case model.ToolChoiceAuto:
			out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
		case model.ToolChoiceRequired:
			out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
		case model.ToolChoiceNone:
			out.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		default:
			return fmt.Errorf("unsupported tool_choice mode %q: Anthropic backend has no safe equivalent", tc.OfToolChoiceMode.Value)
		}
		return nil
	}
	return nil
}

func toolChoiceExplicit(tc oairesponses.ResponseNewParamsToolChoiceUnion) bool {
	return tc.OfToolChoiceMode.Valid() ||
		tc.OfAllowedTools != nil ||
		tc.OfFunctionTool != nil ||
		tc.OfHostedTool != nil ||
		tc.OfMcpTool != nil ||
		tc.OfCustomTool != nil ||
		tc.OfSpecificApplyPatchToolChoice != nil ||
		tc.OfSpecificShellToolChoice != nil ||
		tc.OfResponseNewsToolChoiceSpecificProgrammaticToolCallingParam != nil
}

func structuredToolChoiceEquivalent(name string, tc oairesponses.ResponseNewParamsToolChoiceUnion) bool {
	return tc.OfFunctionTool != nil && tc.OfFunctionTool.Name == name
}

func applySpecificToolChoice(out *anthropic.MessageNewParams, declared []oairesponses.ToolUnionParam, want toolcatalog.Identity) error {
	identities, err := declaredToolIdentities(declared)
	if err != nil {
		return err
	}
	if !hasToolIdentity(identities, want) {
		return fmt.Errorf("tool_choice %s is not declared", want)
	}
	out.ToolChoice = anthropic.ToolChoiceUnionParam{
		OfTool: &anthropic.ToolChoiceToolParam{Name: want.ConvertedName()},
	}
	return nil
}

func applyAllowedTools(out *anthropic.MessageNewParams, declared []oairesponses.ToolUnionParam, allowed *oairesponses.ToolChoiceAllowedParam) error {
	allowedNames, err := allowedToolNames(declared, allowed)
	if err != nil {
		return err
	}
	var filtered []anthropic.ToolUnionParam
	for _, tool := range out.Tools {
		if tool.OfTool != nil && allowedNames[tool.OfTool.Name] {
			filtered = append(filtered, tool)
		}
	}
	if len(filtered) == 0 {
		return fmt.Errorf("tool_choice allowed_tools has no supported tools")
	}
	out.Tools = filtered
	switch allowed.Mode {
	case oairesponses.ToolChoiceAllowedModeAuto:
		out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	case oairesponses.ToolChoiceAllowedModeRequired:
		out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	default:
		return fmt.Errorf("tool_choice allowed_tools mode %q is unsupported", allowed.Mode)
	}
	return nil
}

func allowedToolNames(declared []oairesponses.ToolUnionParam, allowed *oairesponses.ToolChoiceAllowedParam) (map[string]bool, error) {
	declaredIdentities, err := declaredToolIdentities(declared)
	if err != nil {
		return nil, err
	}
	allowedNames := make(map[string]bool, len(allowed.Tools))
	for _, tool := range allowed.Tools {
		identities, err := parseAllowedToolIdentities(tool)
		if err != nil {
			return nil, err
		}
		for _, identity := range identities {
			if !hasToolIdentity(declaredIdentities, identity) {
				return nil, fmt.Errorf("tool_choice allowed_tools entry %s is not declared", identity)
			}
			allowedNames[identity.ConvertedName()] = true
		}
	}
	return allowedNames, nil
}

func declaredToolIdentities(tools []oairesponses.ToolUnionParam) ([]toolcatalog.Identity, error) {
	identities := make([]toolcatalog.Identity, 0, len(tools))
	for _, tool := range tools {
		ids, err := toolcatalog.Inspect(tool)
		if err != nil {
			return nil, err
		}
		identities = append(identities, ids...)
	}
	return identities, nil
}

func hasToolIdentity(identities []toolcatalog.Identity, want toolcatalog.Identity) bool {
	for _, identity := range identities {
		if identity.Equal(want) {
			return true
		}
	}
	return false
}

func parseAllowedToolIdentities(tool map[string]any) ([]toolcatalog.Identity, error) {
	return toolcatalog.InspectAllowed(tool)
}

func applyParallelToolChoice(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) {
	if len(out.Tools) == 0 || !req.ParallelToolCalls.Valid() || req.ParallelToolCalls.Value {
		return
	}
	if out.ToolChoice.OfAuto == nil && out.ToolChoice.OfAny == nil && out.ToolChoice.OfTool == nil {
		out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	}
	if out.ToolChoice.OfAuto != nil {
		out.ToolChoice.OfAuto.DisableParallelToolUse = anthropic.Bool(true)
	}
	if out.ToolChoice.OfAny != nil {
		out.ToolChoice.OfAny.DisableParallelToolUse = anthropic.Bool(true)
	}
	if out.ToolChoice.OfTool != nil {
		out.ToolChoice.OfTool.DisableParallelToolUse = anthropic.Bool(true)
	}
}

func documentBlock(file *oairesponses.ResponseInputFileParam) *anthropic.DocumentBlockParam {
	if file.FileURL.Valid() && (strings.HasPrefix(file.FileURL.Value, "http://") || strings.HasPrefix(file.FileURL.Value, "https://")) {
		block := &anthropic.DocumentBlockParam{
			Source: anthropic.DocumentBlockParamSourceUnion{
				OfURL: &anthropic.URLPDFSourceParam{URL: file.FileURL.Value},
			},
		}
		setDocumentTitle(block, file)
		return block
	}
	if !file.FileData.Valid() {
		return nil
	}
	mediaType, data, ok := strings.Cut(strings.TrimPrefix(file.FileData.Value, "data:"), ",")
	if !ok || data == "" {
		return nil
	}
	if before, _, ok := strings.Cut(mediaType, ";"); ok {
		mediaType = before
	}
	block := &anthropic.DocumentBlockParam{
		Source: anthropic.DocumentBlockParamSourceUnion{
			OfBase64: &anthropic.Base64PDFSourceParam{Data: data},
		},
	}
	if mediaType == "text/plain" {
		block.Source = anthropic.DocumentBlockParamSourceUnion{
			OfText: &anthropic.PlainTextSourceParam{Data: data},
		}
	}
	setDocumentTitle(block, file)
	return block
}

func setDocumentTitle(block *anthropic.DocumentBlockParam, file *oairesponses.ResponseInputFileParam) {
	if file.Filename.Valid() && file.Filename.Value != "" {
		block.Title = aparam.NewOpt(file.Filename.Value)
	}
}

func applyAnthropicCacheControl(out *anthropic.MessageNewParams, cfg *config.Config) {
	ttl := anthropic.CacheControlEphemeralTTLTTL5m
	if cfg != nil && cfg.Cache.TTL == "1h" {
		ttl = anthropic.CacheControlEphemeralTTLTTL1h
	}
	cacheControl := anthropic.NewCacheControlEphemeralParam()
	cacheControl.TTL = ttl
	out.CacheControl = cacheControl
	if len(out.System) > 0 {
		out.System[len(out.System)-1].CacheControl = cacheControl
	}
	setLastToolCacheControl(out.Tools, cacheControl)
}

// setLastToolCacheControl 给 tools 列表的最后一个 tool 加 cache_control，
// 派发由 toolcatalog.ApplyCacheControl 承载（覆盖所有已知 server tool 变体）。
func setLastToolCacheControl(tools []anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam) {
	if len(tools) == 0 {
		return
	}
	last := &tools[len(tools)-1]
	if !toolcatalog.ApplyCacheControl(last, cc) {
		slog.Warn("最后一个 tool 是未知变体，无法加 cache_control，tools 列表缓存将丢失")
	}
}

func orDefault(s string, def string) string {
	if s == "" {
		return def
	}
	return s
}

func formatInstructionParts(parts []instructionPart) string {
	var formatted []string
	for _, part := range parts {
		if part.text == "" {
			continue
		}
		role := part.role
		if role == "" {
			role = model.RoleDeveloper
		}
		formatted = append(formatted, fmt.Sprintf("<%s>\n%s\n</%s>", role, part.text, role))
	}
	if len(formatted) == 0 {
		return ""
	}
	formatted = append([]string{
		"OpenAI instruction hierarchy is preserved below. Apply <system> before <developer>; both override user messages.",
	}, formatted...)
	return joinNonEmpty("\n\n", formatted)
}

func formatToolNames(tag string, tools []oairesponses.ToolUnionParam) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		ids, err := toolcatalog.Inspect(tool)
		if err != nil {
			continue // 未知 tool 跳过，与原 switch 无 default 语义一致
		}
		for _, id := range ids {
			names = append(names, id.ConvertedName())
		}
	}
	body, err := json.Marshal(names)
	if err != nil {
		body = []byte("[]")
	}
	return fmt.Sprintf("<%s>\n%s\n</%s>", tag, string(body), tag)
}

func joinNonEmpty(sep string, parts []string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, sep)
}
