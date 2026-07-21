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
	// Codex 回灌历史 assistant 消息的 content 使用 output_text（与 Responses 输出
	// item 同形）。openai-go 把 type=message 统一解到 EasyInputMessage，其 content
	// 列表只认 input_text/input_image/input_file，output_text 被静默丢弃 → 上游
	// 收到空 assistant 消息 → 模型表现为"丢上下文"。从 raw JSON 恢复。
	restoreAssistantOutputTextFromRaw(data, &req)
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

// restoreAssistantOutputTextFromRaw 把 input 历史里 assistant message 的
// content[].type=output_text 归一成 EasyInputMessage 可承载的 input_text。
//
// 根因：openai-go 的 ResponseInputItemUnion 对 type=message 优先解成
// EasyInputMessage；其 content 列表 discriminator 只注册 input_text /
// input_image / input_file。Codex HTTP 回灌的 assistant 历史却是输出形态
// （output_text，与 stream 下发的 message item 同形），解完后 content 变空
// 列表，appendMessage 再填一个空 text block——角色骨架还在，正文全丢。
// 真实会话（半年汇报 PPT）里模型反复说"看不到上一轮"即此症状。
//
// 策略：仅在 raw 含 output_text/refusal 时改写；已是 input_text 的路径不动。
// 归一到 input_text 后复用既有 appendMessage，无需另开 OfOutputMessage 分支
// （实测带 id/status/phase 时 SDK 仍落 OfMessage，OfOutputMessage 基本到不了）。
func restoreAssistantOutputTextFromRaw(data []byte, req *oairesponses.ResponseNewParams) {
	var raw struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || len(raw.Input) == 0 {
		return
	}
	// input 也可以是纯 string，与 OfInputItemList 无关。
	if raw.Input[0] != '[' {
		return
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(raw.Input, &rawItems); err != nil {
		return
	}
	if len(rawItems) != len(req.Input.OfInputItemList) {
		// 不严格长度匹配，因为 phase/metadata 等额外字段可能导致 SDK 解析后条目数对不上(raw 有而 req 无)
		// 能恢复一条是一条，避免 commentary 等特殊 phase 的正文丢了
	}
	restored := 0
	for i, rawItem := range rawItems {
		if restoreOneAssistantOutputText(rawItem, &req.Input.OfInputItemList[i]) {
			restored++
		}
	}
	if restored > 0 {
		slog.Debug("恢复历史 assistant output_text 为 input_text",
			"restored_messages", restored,
			"input_items", len(rawItems))
	}
}

// restoreOneAssistantOutputText 处理单条 input item。返回是否执行了恢复。
func restoreOneAssistantOutputText(rawItem json.RawMessage, item *oairesponses.ResponseInputItemUnionParam) bool {
	var probe struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(rawItem, &probe); err != nil {
		return false
	}
	// type 缺省时按 message 处理（部分客户端省略）；明确非 message 则跳过。
	if probe.Type != "" && probe.Type != "message" {
		return false
	}
	// 仅 assistant 历史用 output_text；user/system/developer 走 input_text。
	if probe.Role != "" && probe.Role != "assistant" {
		return false
	}
	if len(probe.Content) == 0 || probe.Content[0] != '[' {
		return false
	}
	var parts []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
		// annotations 仅用于探测；OpenAI→Anthropic 历史无法还原 encrypted_index。
		Annotations json.RawMessage `json:"annotations"`
	}
	if err := json.Unmarshal(probe.Content, &parts); err != nil || len(parts) == 0 {
		return false
	}
	need := false
	droppedAnnotations := 0
	for _, p := range parts {
		if p.Type == "output_text" || p.Type == "refusal" {
			need = true
			if p.Type == "output_text" && len(p.Annotations) > 2 && string(p.Annotations) != "[]" && string(p.Annotations) != "null" {
				droppedAnnotations++
			}
		}
	}
	if !need {
		return false
	}
	if droppedAnnotations > 0 {
		// 可控 lossy：Anthropic 多轮 citation 需要 encrypted_index，OpenAI wire 无此字段。
		slog.Debug("历史 assistant output_text 的 annotations 无法映射到 Anthropic，已丢弃",
			"parts_with_annotations", droppedAnnotations,
			"impact", "正文保留；url_citation 不回传上游（协议无 encrypted_index）")
	}

	list := make(oairesponses.ResponseInputMessageContentListParam, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "output_text", "input_text":
			// 空 output_text 也 append 占位，保持与原 content 等长，避免结构漂移。
			list = append(list, oairesponses.ResponseInputContentUnionParam{
				OfInputText: &oairesponses.ResponseInputTextParam{Text: p.Text},
			})
		case "refusal":
			// refusal 无 input 侧等价；折成可见文本保留语义，避免整段对话被抹掉。
			text := p.Refusal
			if text == "" {
				text = p.Text
			}
			if text == "" {
				text = "[refusal]"
			}
			list = append(list, oairesponses.ResponseInputContentUnionParam{
				OfInputText: &oairesponses.ResponseInputTextParam{Text: text},
			})
		default:
			// input_image / input_file 等若夹在 assistant content 里，此处不重建
			// （SDK 若已解出则保留在 OfMessage；本恢复只补文本）。output_text
			// 场景下 Codex 实际几乎只发文本 part。
		}
	}
	if len(list) == 0 {
		return false
	}

	if item.OfMessage == nil {
		role := oairesponses.EasyInputMessageRoleAssistant
		if probe.Role != "" {
			role = oairesponses.EasyInputMessageRole(probe.Role)
		}
		item.OfMessage = &oairesponses.EasyInputMessageParam{
			Role: role,
			Type: oairesponses.EasyInputMessageTypeMessage,
		}
	}
	// 覆盖被 SDK 解空的 content；保留 Role/Phase 等已解字段。
	item.OfMessage.Content = oairesponses.EasyInputMessageContentUnionParam{
		OfInputItemContentList: list,
	}
	// 若 SDK 误落到 OfOutputMessage（当前未观察到），清掉避免 appendItem 双路径。
	item.OfOutputMessage = nil
	return true
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
// 第二个返回值是 MCP beta 注入定义（mcp_servers + mcp_toolset），由 collectMCP 产出；
// 非 nil 时由 client 层注入到 marshal 后的请求体（SDK 不支持这组字段）。
func ToAnthropic(req *oairesponses.ResponseNewParams, cfg *config.Config) (*anthropic.MessageNewParams, *anthropicclient.MCPInjection, error) {
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
	// toward early end_turn. Codex (HTTP Responses) carries the latest
	// thinking block's signature in encrypted_content, so the preserved block
	// still resolves its signature.
	lastReasoning := -1
	for i := range req.Input.OfInputItemList {
		if req.Input.OfInputItemList[i].OfReasoning != nil {
			lastReasoning = i
		}
	}
	var hasMCPHistory bool
	for i := range req.Input.OfInputItemList {
		item := &req.Input.OfInputItemList[i]
		if item.OfReasoning != nil && i != lastReasoning {
			continue
		}
		if err := appendItem(out, &sysParts, item, &hasMCPHistory); err != nil {
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

	applyReasoning(out, req)

	applyMetadata(out, req)

	if err := convertTools(out, req); err != nil {
		return nil, nil, err
	}
	if err := injectStructuredOutput(out, req); err != nil {
		return nil, nil, err
	}
	if err := convertToolChoice(out, req); err != nil {
		return nil, nil, err
	}
	// Codex 回灌历史或多轮 message item 可能产生连续同 role 的 Anthropic 消息
	// （例如多条 user 输入、reasoning + assistant text 组合成两条 assistant）。
	// Anthropic 官方后端会内部宽容合并，但部分兼容后端（如 Grok）以
	// "messages[N] 必须是包含 tool_result 的 user 消息" 400 拒绝整请求：
	// 它按位置严格校验 assistant(tool_use) → user(tool_result) 的交替顺序，
	// 连续同 role 会破坏这个位置约束。这里在 tool_use 配对补齐之前先合并。
	coalesceSameRoleMessages(out)
	// Codex 回灌历史时若带 tool call 却漏了对应 output（中断后 resume / failover 丢历史 /
	// 客户端 bug），会产出无配对的 tool_use，Anthropic 以 "tool_use without tool_result"
	// 400 拒绝整请求。在 messages 定稿后、设 cache 断点前补占位 result 降级。
	ensureToolUsePaired(out)
	// ensureToolUsePaired 补占位时可能在 tail 追加一条新的 user 消息，
	// 若原本末尾就是 user，会再次产生连续 user。再合并一次保证最终交替。
	coalesceSameRoleMessages(out)
	applyAnthropicCacheControl(out, cfg)

	mcp, err := collectMCP(req)
	if err != nil {
		return nil, nil, err
	}
	// 历史 mcp_call 通过 param.Override 注入 messages；client 层需要据此设 beta header。
	if mcp == nil {
		mcp = &anthropicclient.MCPInjection{}
	}
	if hasMCPHistory {
		mcp.History = true
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

// hasMCPHistory 非 nil 时，成功回放 mcp_call 会置 true，供 client 层设置 MCP beta header。
func appendItem(out *anthropic.MessageNewParams, sysParts *[]instructionPart, item *oairesponses.ResponseInputItemUnionParam, hasMCPHistory *bool) error {
	if item.OfMessage != nil {
		return appendMessage(out, sysParts, item.OfMessage)
	}
	// 防御：若未来 SDK 把带 id/status 的 assistant message 解到 OfOutputMessage，
	// 也要转成 Anthropic assistant text，避免静默跳过。
	if item.OfOutputMessage != nil {
		return appendOutputMessage(out, item.OfOutputMessage)
	}
	if item.OfReasoning != nil {
		return appendReasoning(out, item.OfReasoning)
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
	if item.OfWebSearchCall != nil {
		return appendWebSearchCall(out, item.OfWebSearchCall)
	}
	// 历史 MCP items 按变体分档：
	//   - mcp_call：通过 param.Override 注入 beta mcp_tool_use / mcp_tool_result（走
	//     anthropic-beta: mcp-client-2025-11-20），保留调用上下文。
	//   - mcp_list_tools：无 Anthropic 等价块，折成 developer marker（server + 工具名 + error），
	//     保留「有哪些工具可用」的线索，lossy。
	//   - mcp_approval_request / mcp_approval_response：Anthropic 无审批协议，网关不实现，
	//     WARN + 丢弃，避免误导模型以为审批已发生。
	if item.OfMcpCall != nil {
		wrote, err := appendMcpCall(out, item.OfMcpCall)
		if err != nil {
			return err
		}
		if wrote && hasMCPHistory != nil {
			*hasMCPHistory = true
		}
		return nil
	}
	if item.OfMcpListTools != nil {
		return appendMcpListToolsMarker(sysParts, item.OfMcpListTools)
	}
	if item.OfMcpApprovalRequest != nil || item.OfMcpApprovalResponse != nil {
		slog.Warn("丢弃历史 MCP 审批 item（Anthropic 无审批协议，网关不实现）",
			"item_type", mcpHistoryItemType(item), "item_id", mcpHistoryItemID(item))
		return nil
	}
	// 无 Anthropic 等价语义的 hosted / 专有 item（file_search / computer / image_generation /
	// program / item_reference / additional_tools）：WARN + 丢弃，禁止把整段 JSON 灌进
	// system context 干扰模型。工具声明阶段这些类型多数已 fail-fast，此处兜底历史回灌路径。
	if item.OfFileSearchCall != nil || item.OfComputerCall != nil ||
		item.OfComputerCallOutput != nil || item.OfImageGenerationCall != nil ||
		item.OfProgram != nil || item.OfProgramOutput != nil ||
		item.OfItemReference != nil || item.OfAdditionalTools != nil {
		typ := ""
		if ptr := item.GetType(); ptr != nil {
			typ = *ptr
		}
		if typ == "" {
			typ = "unknown"
		}
		slog.Warn("丢弃无 Anthropic 等价语义的历史 input item，对应数据被丢弃",
			"item_type", typ,
			"impact", "该 item 不会进入 system context，也不会传给上游")
		return nil
	}
	if item.OfLocalShellCall != nil {
		return appendLocalShellCall(out, item.OfLocalShellCall)
	}
	if item.OfLocalShellCallOutput != nil {
		return appendToolResult(out, item.OfLocalShellCallOutput.ID, localShellOutputText(item.OfLocalShellCallOutput))
	}
	if item.OfShellCall != nil {
		return appendShellCall(out, item.OfShellCall)
	}
	if item.OfShellCallOutput != nil {
		return appendToolResult(out, item.OfShellCallOutput.CallID, shellCallOutputText(item.OfShellCallOutput))
	}
	if item.OfApplyPatchCall != nil {
		return appendApplyPatchCall(out, item.OfApplyPatchCall)
	}
	if item.OfApplyPatchCallOutput != nil {
		return appendToolResult(out, item.OfApplyPatchCallOutput.CallID, applyPatchOutputText(item.OfApplyPatchCallOutput))
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

// appendOutputMessage 把 ResponseOutputMessage（assistant 输出形态）转成
// Anthropic assistant text。正常路径下 restoreAssistantOutputTextFromRaw 已把
// output_text 归一进 OfMessage，本函数是 SDK 若改 discriminator 后的兜底。
func appendOutputMessage(out *anthropic.MessageNewParams, m *oairesponses.ResponseOutputMessageParam) error {
	var blocks []anthropic.ContentBlockParamUnion
	for _, cp := range m.Content {
		if cp.OfOutputText != nil {
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: cp.OfOutputText.Text},
			})
		} else if cp.OfRefusal != nil {
			text := cp.OfRefusal.Refusal
			if text == "" {
				text = "[refusal]"
			}
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: text},
			})
		}
	}
	if len(blocks) == 0 {
		blocks = []anthropic.ContentBlockParamUnion{{OfText: &anthropic.TextBlockParam{}}}
	}
	out.Messages = append(out.Messages, anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleAssistant,
		Content: blocks,
	})
	return nil
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
			} else if cp.OfInputImage.FileID.Valid() && cp.OfInputImage.FileID.Value != "" {
				slog.Warn("丢弃 input_image.file_id（网关无 OpenAI Files 凭据拉取文件），对应数据被丢弃",
					"role", string(m.Role),
					"file_id", cp.OfInputImage.FileID.Value,
					"impact", "图片不会传递给上游")
			}
		} else if cp.OfInputFile != nil {
			if block := documentBlock(cp.OfInputFile); block != nil {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{OfDocument: block})
			} else if cp.OfInputFile.FileID.Valid() && cp.OfInputFile.FileID.Value != "" {
				slog.Warn("丢弃 input_file.file_id（网关无 OpenAI Files 凭据拉取文件），对应数据被丢弃",
					"role", string(m.Role),
					"file_id", cp.OfInputFile.FileID.Value,
					"impact", "文件不会传递给上游")
			}
		}
	}

	role := string(m.Role)

	// system/developer fold into top-level System.
	// NOTE: image blocks in system/developer messages are silently dropped here.
	// Anthropic's system parameter is []TextBlockParam (text-only), so images
	// cannot be represented in the system role. This is a protocol limitation.
	for _, b := range blocks {
		if b.OfImage != nil {
			slog.Warn("丢弃 system/developer message 中的 image block（Anthropic system 仅支持文本），对应数据被丢弃",
				"role", role,
				"impact", "图片不会传递给上游")
		}
	}
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

func appendReasoning(out *anthropic.MessageNewParams, r *oairesponses.ResponseReasoningItemParam) error {
	text := ""
	if len(r.Summary) > 0 {
		text = r.Summary[0].Text
	}
	// summary 为空时回退 content[].reasoning_text（部分 ZDR/兼容端只填 content）。
	if text == "" {
		for _, c := range r.Content {
			if c.Text != "" {
				text = c.Text
				break
			}
		}
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
	// 无 encrypted_content：signature 不可恢复。Anthropic Messages API 要求
	// ThinkingBlockParam.Signature 非空（required），空 signature 会被官方
	// 后端 400 拒绝。智谱/方舟等兼容后端虽接受空 signature，但回灌空 signature
	// thinking block 违反 round-trip 规范。按 Anthropic 官方建议，无 signature
	// 的 thinking block 应丢弃（不回传），而非用空值 attach。这不会导致模型
	// "失忆"：extended thinking 只需保留最近的 thinking block，且此处已有
	// lastReasoning 裁剪逻辑，非最新 reasoning 本就会被跳过。
	slog.Debug("reasoning item 缺少 encrypted_content，跳过 thinking block 回灌",
		"reasoning_id", r.ID,
		"impact", "该 reasoning item 不转递给上游（signature 为空，不符合 round-trip 要求）")
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
	input := map[string]any{
		"input": strings.Join(call.Action.Commands, "\n"),
	}
	// Environment 是 local/container 身份线索（非 env map）；只记 type，不 dump 整 union。
	switch {
	case call.Environment.OfLocal != nil:
		input["environment_type"] = "local"
	case call.Environment.OfContainerReference != nil:
		input["environment_type"] = "container_reference"
	}
	if call.Action.TimeoutMs.Valid() {
		input["timeout_ms"] = call.Action.TimeoutMs.Value
	}
	if call.Action.MaxOutputLength.Valid() {
		input["max_output_length"] = call.Action.MaxOutputLength.Value
	}
	if call.Status != "" {
		input["status"] = call.Status
	}
	putCallerMeta(input, call.Caller.OfDirect != nil, call.Caller.OfProgram != nil, call.Caller.GetCallerID())
	return appendToolUse(out, call.CallID, "shell", input)
}

func appendLocalShellCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemLocalShellCallParam) error {
	input := map[string]any{
		"input": strings.Join(call.Action.Command, " "),
	}
	if len(call.Action.Env) > 0 {
		input["env"] = call.Action.Env
	}
	if call.Action.WorkingDirectory.Valid() && call.Action.WorkingDirectory.Value != "" {
		input["working_directory"] = call.Action.WorkingDirectory.Value
	}
	if call.Action.TimeoutMs.Valid() {
		input["timeout_ms"] = call.Action.TimeoutMs.Value
	}
	if call.Action.User.Valid() && call.Action.User.Value != "" {
		input["user"] = call.Action.User.Value
	}
	if call.Status != "" {
		input["status"] = call.Status
	}
	return appendToolUse(out, call.CallID, "shell", input)
}

func appendApplyPatchCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemApplyPatchCallParam) error {
	// 与声明/回程一致：freeform V4A 文本，不要 structured JSON。
	// status/caller 无 Anthropic freeform 字段可挂，折入 tool_result 侧已有 status 文本；
	// 历史 call 本身只回灌 patch 正文（lossy：caller 丢失，见覆盖表）。
	var patch string
	switch {
	case call.Operation.OfCreateFile != nil:
		patch = toolcatalog.FormatApplyPatchV4A("create_file", call.Operation.OfCreateFile.Path, call.Operation.OfCreateFile.Diff)
	case call.Operation.OfUpdateFile != nil:
		patch = toolcatalog.FormatApplyPatchV4A("update_file", call.Operation.OfUpdateFile.Path, call.Operation.OfUpdateFile.Diff)
	case call.Operation.OfDeleteFile != nil:
		patch = toolcatalog.FormatApplyPatchV4A("delete_file", call.Operation.OfDeleteFile.Path, "")
	default:
		return fmt.Errorf("apply_patch call %q has an invalid operation", call.CallID)
	}
	if patch == "" {
		return fmt.Errorf("apply_patch call %q has an invalid operation", call.CallID)
	}
	return appendToolUse(out, call.CallID, "apply_patch", map[string]any{"input": patch})
}

// putCallerMeta 把 OpenAI tool call 的 caller 身份折进 tool_use.input（无 Anthropic 等价字段）。
func putCallerMeta(input map[string]any, direct, program bool, programCallerID *string) {
	switch {
	case direct:
		input["caller_type"] = "direct"
	case program:
		input["caller_type"] = "program"
		if programCallerID != nil && *programCallerID != "" {
			input["caller_id"] = *programCallerID
		}
	}
}

func shellOutputText(parts []oairesponses.ResponseFunctionShellCallOutputContentParam) string {
	output := make([]string, 0, len(parts)*3)
	for _, part := range parts {
		if part.Stdout != "" {
			output = append(output, part.Stdout)
		}
		if part.Stderr != "" {
			output = append(output, part.Stderr)
		}
		// outcome 无 Anthropic tool_result 结构字段，折进文本保留退出/超时线索。
		if part.Outcome.OfExit != nil {
			output = append(output, fmt.Sprintf("[exit_code=%d]", part.Outcome.OfExit.ExitCode))
		} else if part.Outcome.OfTimeout != nil {
			output = append(output, "[timeout]")
		}
	}
	return strings.Join(output, "\n")
}

// shellCallOutputText 拼 shell_call_output 全文：status/max_output_length 线索 + stdout/stderr/outcome。
func shellCallOutputText(out *oairesponses.ResponseInputItemShellCallOutputParam) string {
	var parts []string
	if out.Status != "" {
		parts = append(parts, "[status="+out.Status+"]")
	}
	if out.MaxOutputLength.Valid() {
		parts = append(parts, fmt.Sprintf("[max_output_length=%d]", out.MaxOutputLength.Value))
	}
	body := shellOutputText(out.Output)
	if body != "" {
		parts = append(parts, body)
	}
	return strings.Join(parts, "\n")
}

// applyPatchOutputText 拼 apply_patch_call_output：status + 可选日志。
func applyPatchOutputText(out *oairesponses.ResponseInputItemApplyPatchCallOutputParam) string {
	var parts []string
	if out.Status != "" {
		parts = append(parts, "[status="+out.Status+"]")
	}
	if out.Output.Valid() && out.Output.Value != "" {
		parts = append(parts, out.Output.Value)
	}
	return strings.Join(parts, "\n")
}

// localShellOutputText 拼 local_shell_call_output。
func localShellOutputText(out *oairesponses.ResponseInputItemLocalShellCallOutputParam) string {
	var parts []string
	if out.Status != "" {
		parts = append(parts, "[status="+out.Status+"]")
	}
	if out.Output != "" {
		parts = append(parts, out.Output)
	}
	return strings.Join(parts, "\n")
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
	// function_call_output.output 是 union：string 或 [{input_text|input_image|input_file}...]。
	// 只读 OfString 会把 content list 静默变空 tool_result，参见协议覆盖表 Input Item 说明。
	if items := fco.Output.OfResponseFunctionCallOutputItemArray; len(items) > 0 {
		blocks := functionCallOutputContent(fco.CallID, items)
		return appendToolResultBlocks(out, fco.CallID, blocks)
	}
	outputText := ""
	if fco.Output.OfString.Valid() {
		outputText = fco.Output.OfString.Value
	}
	return appendToolResult(out, fco.CallID, outputText)
}

func appendCustomToolCallOutput(out *anthropic.MessageNewParams, output *oairesponses.ResponseCustomToolCallOutputParam) error {
	// custom_tool_call_output.output 同样是 union：string 或 [{input_text|input_image|input_file}...]。
	if items := output.Output.OfOutputContentList; len(items) > 0 {
		blocks := customToolOutputContent(output.CallID, items)
		return appendToolResultBlocks(out, output.CallID, blocks)
	}
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
// image 输出（OfImage）不可转换，丢弃 + WARN。
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

// appendMcpCall 把历史 mcp_call 回放为 beta mcp_tool_use + mcp_tool_result。
// 标准 ContentBlockParamUnion 无 MCP 变体，用 param.Override 塞原始 JSON，
// 随 MessageNewParams marshal 后由 client 的 beta header 路径识别。
// 第二个返回值表示是否真正写入了消息块（id 为空时跳过且不置 History）。
func appendMcpCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemMcpCallParam) (bool, error) {
	if call.ID == "" {
		return false, nil
	}
	var input any = map[string]any{}
	if call.Arguments != "" {
		var parsed any
		if err := json.Unmarshal([]byte(call.Arguments), &parsed); err == nil {
			input = parsed
		} else {
			input = map[string]any{"raw": call.Arguments}
		}
	}
	useRaw, err := json.Marshal(map[string]any{
		"type":        "mcp_tool_use",
		"id":          call.ID,
		"name":        call.Name,
		"server_name": call.ServerLabel,
		"input":       input,
	})
	if err != nil {
		return false, err
	}
	resultContent := ""
	isError := false
	if call.Error.Valid() && call.Error.Value != "" {
		resultContent = call.Error.Value
		isError = true
	} else if call.Output.Valid() {
		resultContent = call.Output.Value
	}
	resultObj := map[string]any{
		"type":        "mcp_tool_result",
		"tool_use_id": call.ID,
		"is_error":    isError,
		"content":     []map[string]any{{"type": "text", "text": resultContent}},
	}
	resultRaw, err := json.Marshal(resultObj)
	if err != nil {
		return false, err
	}

	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleAssistant {
		out.Messages = append(out.Messages, anthropic.NewAssistantMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, aparam.Override[anthropic.ContentBlockParamUnion](json.RawMessage(useRaw)))

	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleUser {
		out.Messages = append(out.Messages, anthropic.NewUserMessage())
	}
	last = &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, aparam.Override[anthropic.ContentBlockParamUnion](json.RawMessage(resultRaw)))
	return true, nil
}

// appendMcpListToolsMarker 把历史 mcp_list_tools 折成 developer marker（工具名列表 + 可选 error）。
// Anthropic Messages 无 mcp_list_tools 等价块；作为文本上下文比整段丢弃更利于模型判断可用工具。
func appendMcpListToolsMarker(sysParts *[]instructionPart, list *oairesponses.ResponseInputItemMcpListToolsParam) error {
	names := make([]string, 0, len(list.Tools))
	for _, t := range list.Tools {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	buf := &strings.Builder{}
	buf.WriteString("<mcp_list_tools server=\"")
	buf.WriteString(list.ServerLabel)
	buf.WriteString("\">")
	if list.Error.Valid() && list.Error.Value != "" {
		buf.WriteString("\n<error>")
		buf.WriteString(list.Error.Value)
		buf.WriteString("</error>")
	}
	if len(names) > 0 {
		buf.WriteString("\n<tools>")
		buf.WriteString(strings.Join(names, ","))
		buf.WriteString("</tools>")
	}
	buf.WriteString("\n</mcp_list_tools>")
	*sysParts = append(*sysParts, instructionPart{
		role: model.RoleDeveloper,
		text: buf.String(),
	})
	slog.Debug("mcp_list_tools 折成 developer marker（Anthropic 无等价历史块）",
		"server_label", list.ServerLabel, "tool_count", len(names))
	return nil
}

// appendWebSearchCall 把历史 web_search_call 回放为 Anthropic
// server_tool_use(web_search) + web_search_tool_result。
// 出站 stream 已支持 web_search；此函数补齐入站历史，让后端识别 hosted 搜索上下文。
//
// 映射约定：
//   - action.search：query/queries → input.query；sources → result URL 列表
//   - action.open_page / find：Anthropic Messages 无 open_page/find 原生历史块，
//     将 URL/pattern 折入 query 文本做 lossy 回放
//   - Anthropic `web_search_result` 的 required 字段 `encrypted_content` 在 OpenAI wire
//     里没有；填空串会被官方 API 400。此处 result content 固定为空数组，sources URL 折成
//     同一 user 消息内的可见文本，保留模型可读上下文
func appendWebSearchCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseFunctionWebSearchParam) error {
	if call.ID == "" {
		return nil
	}
	query, sourceURLs := webSearchCallReplay(call)
	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleAssistant {
		out.Messages = append(out.Messages, anthropic.NewAssistantMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, anthropic.NewServerToolUseBlock(
		call.ID,
		map[string]any{"query": query},
		anthropic.ServerToolUseBlockParamNameWebSearch,
	))

	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleUser {
		out.Messages = append(out.Messages, anthropic.NewUserMessage())
	}
	last = &out.Messages[len(out.Messages)-1]
	// OpenAI wire 无 Anthropic required 的 encrypted_content；带空 encrypted 的伪
	// result 可能被官方 API 拒绝。result content 固定空数组，URL 列表折成可见文本
	// 挂在同一 user 消息里，保留模型可读上下文。
	last.Content = append(last.Content, anthropic.NewWebSearchToolResultBlock(
		[]anthropic.WebSearchResultBlockParam{}, call.ID,
	))
	if len(sourceURLs) > 0 {
		last.Content = append(last.Content, anthropic.ContentBlockParamUnion{
			OfText: &anthropic.TextBlockParam{
				Text: "[web_search sources]\n" + strings.Join(sourceURLs, "\n"),
			},
		})
	}
	return nil
}

// webSearchCallReplay 从 OpenAI web_search_call 提取 query 与 source URL 列表。
//
//nolint:staticcheck // OfSearch.Query 已 deprecated，但仍作 Queries 为空时的旧 wire 回退
func webSearchCallReplay(call *oairesponses.ResponseFunctionWebSearchParam) (string, []string) {
	query := ""
	var urls []string
	switch {
	case call.Action.OfSearch != nil:
		a := call.Action.OfSearch
		// Queries 为现行字段；Query 已 deprecated，仅作旧 wire 回退。
		if len(a.Queries) > 0 {
			query = strings.Join(a.Queries, "\n")
		} else if a.Query.Valid() && a.Query.Value != "" {
			query = a.Query.Value
		}
		for _, s := range a.Sources {
			if s.URL == "" {
				continue
			}
			urls = append(urls, s.URL)
		}
	case call.Action.OfOpenPage != nil:
		if call.Action.OfOpenPage.URL.Valid() {
			query = call.Action.OfOpenPage.URL.Value
		}
		if query != "" {
			urls = append(urls, query)
		}
	case call.Action.OfFind != nil:
		a := call.Action.OfFind
		parts := make([]string, 0, 2)
		if a.URL != "" {
			parts = append(parts, a.URL)
		}
		if a.Pattern != "" {
			parts = append(parts, a.Pattern)
		}
		query = strings.Join(parts, "\n")
		if a.URL != "" {
			urls = append(urls, a.URL)
		}
	}
	return query, urls
}

// codeInterpreterLogs 把 code_interpreter_call 的 logs outputs 拼成单段 stdout 文本。
func codeInterpreterLogs(outputs []oairesponses.ResponseCodeInterpreterToolCallOutputUnionParam) string {
	var parts []string
	for _, o := range outputs {
		if o.OfLogs != nil && o.OfLogs.Logs != "" {
			parts = append(parts, o.OfLogs.Logs)
		} else if o.OfImage != nil {
			// image 输出无 Anthropic code_execution_result 等价字段，丢弃 + WARN。
			// URL 仅进 WARN；logs 放简短占位，避免空 result 被误读为「无输出」。
			url := o.OfImage.URL
			slog.Warn("丢弃 code_interpreter_call 的 image 输出（Anthropic code_execution 无等价字段），对应数据被丢弃",
				"url", url,
				"impact", "图片不会出现在 code_execution_tool_result 中")
			parts = append(parts, "[code_interpreter image output omitted: no Anthropic equivalent]")
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
	return appendToolResultBlocks(out, callID, []anthropic.ToolResultBlockParamContentUnion{{
		OfText: &anthropic.TextBlockParam{Text: outputText},
	}})
}

// appendToolResultBlocks 追加一条 tool_result；content 为空时补空 text，避免 tool_use 失配。
func appendToolResultBlocks(out *anthropic.MessageNewParams, callID string, content []anthropic.ToolResultBlockParamContentUnion) error {
	if len(content) == 0 {
		content = []anthropic.ToolResultBlockParamContentUnion{{
			OfText: &anthropic.TextBlockParam{},
		}}
	}
	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleUser {
		out.Messages = append(out.Messages, anthropic.NewUserMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: callID,
			Content:   content,
		},
	})
	return nil
}

// functionCallOutputContent 把 function_call_output 的 content 数组转成 tool_result parts。
func functionCallOutputContent(callID string, items []oairesponses.ResponseFunctionCallOutputItemUnionParam) []anthropic.ToolResultBlockParamContentUnion {
	out := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(items))
	for _, item := range items {
		switch {
		case item.OfInputText != nil:
			out = append(out, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: item.OfInputText.Text},
			})
		case item.OfInputImage != nil:
			if part, ok := toolResultImagePart(callID, item.OfInputImage.ImageURL, item.OfInputImage.FileID); ok {
				out = append(out, part)
			}
		case item.OfInputFile != nil:
			if part, ok := toolResultFilePart(callID, item.OfInputFile.FileURL, item.OfInputFile.FileData, item.OfInputFile.FileID, item.OfInputFile.Filename); ok {
				out = append(out, part)
			}
		}
	}
	return out
}

// customToolOutputContent 把 custom_tool_call_output 的 content list 转成 tool_result parts。
func customToolOutputContent(callID string, items []oairesponses.ResponseCustomToolCallOutputOutputOutputContentListItemUnionParam) []anthropic.ToolResultBlockParamContentUnion {
	out := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(items))
	for _, item := range items {
		switch {
		case item.OfInputText != nil:
			out = append(out, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: item.OfInputText.Text},
			})
		case item.OfInputImage != nil:
			if part, ok := toolResultImagePart(callID, item.OfInputImage.ImageURL, item.OfInputImage.FileID); ok {
				out = append(out, part)
			}
		case item.OfInputFile != nil:
			if part, ok := toolResultFilePart(callID, item.OfInputFile.FileURL, item.OfInputFile.FileData, item.OfInputFile.FileID, item.OfInputFile.Filename); ok {
				out = append(out, part)
			}
		}
	}
	return out
}

func toolResultImagePart(callID string, imageURL, fileID oparam.Opt[string]) (anthropic.ToolResultBlockParamContentUnion, bool) {
	if imageURL.Valid() && imageURL.Value != "" {
		img := imageBlock(imageURL.Value)
		if img.OfImage == nil {
			return anthropic.ToolResultBlockParamContentUnion{}, false
		}
		return anthropic.ToolResultBlockParamContentUnion{OfImage: img.OfImage}, true
	}
	if fileID.Valid() && fileID.Value != "" {
		slog.Warn("丢弃 tool output 中的 input_image.file_id（网关无 OpenAI Files 凭据拉取文件），对应数据被丢弃",
			"call_id", callID,
			"file_id", fileID.Value,
			"impact", "图片不会出现在 tool_result 中")
	}
	return anthropic.ToolResultBlockParamContentUnion{}, false
}

func toolResultFilePart(callID string, fileURL, fileData, fileID, filename oparam.Opt[string]) (anthropic.ToolResultBlockParamContentUnion, bool) {
	file := &oairesponses.ResponseInputFileParam{
		FileURL:  fileURL,
		FileData: fileData,
		FileID:   fileID,
		Filename: filename,
	}
	if block := documentBlock(file); block != nil {
		return anthropic.ToolResultBlockParamContentUnion{OfDocument: block}, true
	}
	if fileID.Valid() && fileID.Value != "" {
		slog.Warn("丢弃 tool output 中的 input_file.file_id（网关无 OpenAI Files 凭据拉取文件），对应数据被丢弃",
			"call_id", callID,
			"file_id", fileID.Value,
			"impact", "文件不会出现在 tool_result 中")
	}
	return anthropic.ToolResultBlockParamContentUnion{}, false
}

// placeholderToolResultText 标注某 tool_use 在 input 历史中缺少 output，已由网关降级补占位。
// 文本是 wire 内容（发给上游模型），用英文以对模型友好。
const placeholderToolResultText = "[no tool output available — this call's result was missing from the request history]"

// placeholderToolResults 构造一组 is_error 占位 tool_result，按 ids 顺序排列。
func placeholderToolResults(ids []string) []anthropic.ContentBlockParamUnion {
	out := make([]anthropic.ContentBlockParamUnion, 0, len(ids))
	for _, id := range ids {
		out = append(out, anthropic.ContentBlockParamUnion{
			OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: id,
				IsError:   anthropic.Bool(true),
				Content: []anthropic.ToolResultBlockParamContentUnion{{
					OfText: &anthropic.TextBlockParam{Text: placeholderToolResultText},
				}},
			},
		})
	}
	return out
}

// coalesceSameRoleMessages 合并相邻同 role 的 Anthropic 消息，保证最终 messages 严格
// 按 user / assistant 交替排列。触发场景：
//   - Codex 回灌历史时出现连续两条 user message（例如 apply_patch_call_output 之后紧跟
//     用户 message，或多轮工具中断后累积的 user 输入）。
//   - reasoning + text output 拆成的两条 assistant message。
//
// Anthropic 官方后端会宽容合并，但部分兼容后端（如 Grok）按位置严格校验
// assistant(tool_use) → user(tool_result) 的交替顺序，连续同 role 会 400。
// 合并策略：把后一条的 content 追加到前一条尾部，保持原始 block 顺序不变。
// 若合并后触发去重（例如两条完全一致的空 assistant 占位）不做额外处理，
// 由后续 ensureToolUsePaired / cache_control 逻辑自行消化。
func coalesceSameRoleMessages(out *anthropic.MessageNewParams) {
	if len(out.Messages) < 2 {
		return
	}
	merged := make([]anthropic.MessageParam, 0, len(out.Messages))
	mergedCount := 0
	for i := range out.Messages {
		cur := out.Messages[i]
		if len(merged) > 0 && merged[len(merged)-1].Role == cur.Role {
			merged[len(merged)-1].Content = append(merged[len(merged)-1].Content, cur.Content...)
			mergedCount++
			continue
		}
		merged = append(merged, cur)
	}
	if mergedCount == 0 {
		return
	}
	out.Messages = merged
	slog.Debug("合并相邻同 role 的 Anthropic messages", "merged", mergedCount, "messages", len(out.Messages))
}

// ensureToolUsePaired 扫描产出 messages，为没有配对 tool_result 的 tool_use 补一个
// is_error 占位 tool_result。占位 result 插在该 tool_use 之后的第一个 user message 前部；
// 若其后没有 user message（assistant 是最后一条），则新建一个 user message 承载。
// server_tool_use（code_interpreter 等）自带配对 result，不受影响。
func ensureToolUsePaired(out *anthropic.MessageNewParams) {
	// 第一遍：收集所有已被 tool_result 引用的 tool_use id。
	resolved := map[string]struct{}{}
	for i := range out.Messages {
		for _, b := range out.Messages[i].Content {
			if b.OfToolResult != nil {
				resolved[b.OfToolResult.ToolUseID] = struct{}{}
			}
		}
	}
	// 第二遍：assistant 里的孤儿 tool_use 入 pending，遇到 user message 时把 pending
	// 补成占位 result prepend 到该 message 前部。
	var pending []string
	flushed := 0
	for i := range out.Messages {
		m := &out.Messages[i]
		switch m.Role {
		case anthropic.MessageParamRoleAssistant:
			for _, b := range m.Content {
				if b.OfToolUse != nil {
					if _, ok := resolved[b.OfToolUse.ID]; !ok {
						pending = append(pending, b.OfToolUse.ID)
					}
				}
			}
		case anthropic.MessageParamRoleUser:
			if len(pending) > 0 {
				m.Content = append(placeholderToolResults(pending), m.Content...)
				flushed += len(pending)
				pending = pending[:0]
			}
		}
	}
	// 末尾仍有孤儿（assistant 是最后一条）→ 新建 user message 承载占位 result。
	if len(pending) > 0 {
		out.Messages = append(out.Messages, anthropic.NewUserMessage(placeholderToolResults(pending)...))
		flushed += len(pending)
	}
	if flushed > 0 {
		slog.Warn("补占位 tool_result：input 历史存在未配对的 tool call（缺少对应 output），已降级为 is_error 占位 result 以避免上游 400",
			"placeholder_count", flushed)
	}
}

// reasoningEffortToOutputConfig 把 OpenAI reasoning effort 透传到 Anthropic output_config.effort。
var reasoningEffortToOutputConfig = map[string]anthropic.OutputConfigEffort{
	"low":    anthropic.OutputConfigEffortLow,
	"medium": anthropic.OutputConfigEffortMedium,
	"high":   anthropic.OutputConfigEffortHigh,
	"xhigh":  anthropic.OutputConfigEffortXhigh,
	"max":    anthropic.OutputConfigEffortMax,
}

func applyReasoning(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) {
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

	out.Thinking = anthropic.ThinkingConfigParamUnion{
		OfEnabled: &anthropic.ThinkingConfigEnabledParam{},
	}

	// 映射 output_config.effort：语义级别让模型自行决定 thinking 深度。
	if mapped, ok := reasoningEffortToOutputConfig[effort]; ok {
		out.OutputConfig.Effort = mapped
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

// applyMetadata 把 OpenAI metadata 中的 user_id 透传到 Anthropic metadata.user_id。
// Anthropic metadata 仅支持 user_id，其余键值对无等价能力，仅由响应 echo 回显。
func applyMetadata(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) {
	if len(req.Metadata) == 0 {
		return
	}
	if uid, ok := req.Metadata["user_id"]; ok && uid != "" {
		out.Metadata = anthropic.MetadataParam{
			UserID: aparam.NewOpt(uid),
		}
	}
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
