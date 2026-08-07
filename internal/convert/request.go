package convert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	aparam "github.com/anthropics/anthropic-sdk-go/packages/param"
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
	case model.ToolTypeFunction:
		if name != "" {
			req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
				OfFunctionTool: &oairesponses.ToolChoiceFunctionParam{Name: name},
			}
		}
	case model.ToolTypeCustom:
		if name != "" {
			req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
				OfCustomTool: &oairesponses.ToolChoiceCustomParam{Name: name},
			}
		}
	case model.ToolTypeApplyPatch:
		req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
			OfSpecificApplyPatchToolChoice: &oairesponses.ToolChoiceApplyPatchParam{},
		}
	case model.ToolTypeShell:
		req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
			OfSpecificShellToolChoice: &oairesponses.ToolChoiceShellParam{},
		}
	case string(oairesponses.ToolChoiceTypesTypeFileSearch),
		string(oairesponses.ToolChoiceTypesTypeComputer),
		string(oairesponses.ToolChoiceTypesTypeComputerUse),
		string(oairesponses.ToolChoiceTypesTypeComputerUsePreview),
		string(oairesponses.ToolChoiceTypesTypeImageGeneration),
		string(oairesponses.ToolChoiceTypesTypeCodeInterpreter),
		model.ToolTypeWebSearch:
		// SDK union 对 hosted tool_choice 的 JSON 会误落 OfAllowedTools
		// （实测五种 hosted 形态全中），此处从 raw 恢复为 OfHostedTool，
		// 下游 a/c/r 各自决定映射或透传。裸 web_search 不在 SDK 枚举内，
		// 但 wire 合法，字符串透传。
		req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{
			OfHostedTool: &oairesponses.ToolChoiceTypesParam{Type: oairesponses.ToolChoiceTypesType(typ)},
		}
	case model.ToolTypeMcp:
		var serverLabel string
		_ = json.Unmarshal(obj["server_label"], &serverLabel)
		mcp := &oairesponses.ToolChoiceMcpParam{ServerLabel: serverLabel}
		if name != "" {
			mcp.Name = oparam.NewOpt(name)
		}
		req.ToolChoice = oairesponses.ResponseNewParamsToolChoiceUnion{OfMcpTool: mcp}
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
	sdkN := len(req.Input.OfInputItemList)
	rawN := len(rawItems)
	// 常见：SDK 丢弃无法识别的 item 后列表变短；极端：sdk_count=0 而 raw 仍有条目。
	// 旧实现按 raw 全长索引 SDK 切片，sdk_count=0 时会 panic，主对话 500 无法继续。
	// 整表丢光时从 raw 重建；普通长度不一致只按最小长度处理。每次请求只打一条日志。
	if sdkN == 0 && rawN > 0 {
		rebuilt := make([]oairesponses.ResponseInputItemUnionParam, 0, rawN)
		for _, rawItem := range rawItems {
			var item oairesponses.ResponseInputItemUnionParam
			if err := json.Unmarshal(rawItem, &item); err != nil {
				continue
			}
			rebuilt = append(rebuilt, item)
		}
		if len(rebuilt) > 0 {
			req.Input.OfInputItemList = rebuilt
			sdkN = len(rebuilt)
			slog.Warn("SDK input 列表为空，已从 raw 重建历史条目",
				"raw_count", rawN, "rebuilt_count", sdkN)
		} else {
			slog.Warn("SDK input 列表为空且无法从 raw 重建",
				"raw_count", rawN, "sdk_count", 0)
		}
	} else if rawN != sdkN {
		slog.Debug("rawItems 与 SDK 解析条目数不一致，按最小长度恢复",
			"raw_count", rawN, "sdk_count", sdkN)
	}
	n := rawN
	if sdkN < n {
		n = sdkN
	}
	restored := 0
	for i := 0; i < n; i++ {
		if restoreOneAssistantOutputText(rawItems[i], &req.Input.OfInputItemList[i]) {
			restored++
		}
	}
	if restored > 0 {
		slog.Debug("恢复历史 assistant output_text 为 input_text",
			"restored_messages", restored,
			"input_items", n)
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
	if probe.Type != "" && probe.Type != model.ItemTypeMessage {
		return false
	}
	// 仅 assistant 历史用 output_text；user/system/developer 走 input_text。
	if probe.Role != "" && probe.Role != model.RoleAssistant {
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
		if p.Type == model.ContentTypeOutputText || p.Type == model.ContentTypeRefusal {
			need = true
			if p.Type == model.ContentTypeOutputText && len(p.Annotations) > 2 && string(p.Annotations) != "[]" && string(p.Annotations) != "null" {
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
		case model.ContentTypeOutputText, model.ContentTypeInputText:
			// 空 output_text 也 append 占位，保持与原 content 等长，避免结构漂移。
			list = append(list, oairesponses.ResponseInputContentUnionParam{
				OfInputText: &oairesponses.ResponseInputTextParam{Text: p.Text},
			})
		case model.ContentTypeRefusal:
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
		if tool.Type != model.ToolTypeNamespace {
			continue
		}
		for _, rawChild := range tool.Tools {
			var child struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(rawChild, &child); err != nil {
				return err
			}
			if child.Type != model.ToolTypeFunction && child.Type != model.ToolTypeCustom {
				return fmt.Errorf("unsupported namespace tool type %q: Anthropic backend has no safe equivalent", child.Type)
			}
		}
	}
	return nil
}

// ToAnthropic converts a Response request into an Anthropic Messages request.
// MCP 由 Codex 客户端本地执行，工具声明直接展开为标准 tool（toolcatalog.Declare）。
func ToAnthropic(req *oairesponses.ResponseNewParams, cfg *config.Config) (*anthropic.MessageNewParams, error) {
	defaultMaxTokens := int64(config.DefaultAnthropicMaxTokens)
	if cfg != nil && cfg.Anthropic.DefaultMaxTokens > 0 {
		defaultMaxTokens = int64(cfg.Anthropic.DefaultMaxTokens)
	}
	out := &anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: defaultMaxTokens,
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
	for i := range req.Input.OfInputItemList {
		item := &req.Input.OfInputItemList[i]
		if item.OfReasoning != nil && i != lastReasoning {
			continue
		}
		if err := appendItem(out, &sysParts, item); err != nil {
			return nil, fmt.Errorf("convert input item: %w", err)
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
		return nil, err
	}
	if err := convertToolChoice(out, req); err != nil {
		return nil, err
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
	// Grok 等兼容后端严格校验 assistant(tool_use) 之后紧接的 user message
	// 必须含所有 tool_use ID 的 tool_result。若某个 tool_result 出现在更远的
	// user 中（例如 tool_search_output 在 function_call_output 之前插入，
	// 拆散了 tool_use→tool_result 的配对），将无紧邻结果的 tool_use 移到
	// 其对应 tool_result 所在 user 之前的 assistant 消息中。
	ensureToolResultProximity(out)
	coalesceSameRoleMessages(out)
	applyAnthropicCacheControl(out, cfg)

	return out, nil
}

type instructionPart struct {
	role string
	text string
}

func appendItem(out *anthropic.MessageNewParams, sysParts *[]instructionPart, item *oairesponses.ResponseInputItemUnionParam) error {
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
		return nil // 不支持 code_interpreter，静默忽略
	}
	if item.OfWebSearchCall != nil {
		return appendWebSearchCall(out, item.OfWebSearchCall)
	}
	// 历史 MCP items 按变体分档：
	//   - mcp_call：按扁平名直接回填标准 tool_use + tool_result（不注入 beta MCP 块）。
	//   - mcp_list_tools：只注入工具声明，不写模型文本（对齐 c 路径）。
	//   - mcp_approval_request / mcp_approval_response：Anthropic 无审批协议，网关不实现，
	//     WARN + 丢弃，避免误导模型以为审批已发生。
	if item.OfMcpCall != nil {
		return appendMcpCall(out, item.OfMcpCall)
	}
	if item.OfMcpListTools != nil {
		return appendMcpListTools(out, item.OfMcpListTools)
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
		// compaction 密文只对生成它的服务端有意义；Codex 在非 OpenAI provider 下走 local
		// 压缩，摘要以明文 user 消息回灌，不会携带该 item。无法解读则丢弃，禁止污染上游。
		slog.Warn("丢弃历史 compaction（密文不可解读，非本网关可用的压缩产物）",
			"item_type", mcpHistoryItemType(item), "item_id", mcpHistoryItemID(item),
			"impact", "压缩历史不会发给 Anthropic 上游；Codex local 压缩以明文摘要 user 消息回灌")
		return nil
	}
	if item.OfCompactionTrigger != nil {
		// 请求控制信号，不是模型输入；Codex 明确丢弃。
		slog.Debug("丢弃历史 compaction_trigger（请求控制信号，非模型输入）",
			"item_type", mcpHistoryItemType(item), "item_id", mcpHistoryItemID(item))
		return nil
	}
	if typ, ok := unknownInputItemType(item); ok {
		slog.Warn("丢弃未知 input item（SDK 未登记类型，网关未实现映射）",
			"item_type", typ,
			"impact", "该 item 不进入 system context，避免污染模型决策")
	}
	return nil
}

func unknownInputItemType(item *oairesponses.ResponseInputItemUnionParam) (string, bool) {
	if item == nil {
		return "", false
	}
	raw, err := json.Marshal(item)
	if err != nil || string(raw) == "{}" || string(raw) == "null" {
		return "", false
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
	return typ, true
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
	// NOTE: image blocks in system/developer messages are dropped here.
	// Anthropic's system parameter is []TextBlockParam (text-only), so images
	// cannot be represented in the system role. This is a protocol limitation.
	// WARN 必须只在真正折入 System 时输出：user/assistant 的 image 会随
	// blocks 正常发给上游，在这之前告警就是对每张用户图片撒谎。
	if role == model.RoleSystem || role == model.RoleDeveloper {
		for _, b := range blocks {
			if b.OfImage != nil {
				slog.Warn("丢弃 system/developer message 中的 image block（Anthropic system 仅支持文本），对应数据被丢弃",
					"role", role,
					"impact", "图片不会传递给上游")
			}
		}
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
	// encrypted_content 有值时：有文本 → attachThinking（明文 signature）；
	// 无文本（redacted 密文）→ 只接受明文 thinking，静默忽略。
	if r.EncryptedContent.Valid() && r.EncryptedContent.Value != "" {
		if text != "" {
			attachThinking(out, text, r.EncryptedContent.Value)
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
	// tool_use.input 必须是 JSON object：Anthropic 与多数兼容后端（含 Grok）会拒绝
	// string / array 形态的 input（400: "缺少有效 id、name 或 object input"）。
	return appendToolUse(out, fc.CallID, toolcatalog.ToolName(fc.Namespace.Value, fc.Name),
		toolUseInputPassthrough(fc.Arguments))
}

// toolUseInputPassthrough 产出可 marshal 进 Anthropic tool_use.input 的值：
//   - 空 → {}
//   - 首个 JSON object → RawMessage 原样字节
//   - 否则 → {"raw":"<原串>"}（不塞裸 string/array；Grok 等兼容端硬性要求 object）
func toolUseInputPassthrough(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if raw, ok := toolUseInputJSON(s); ok {
		return raw
	}
	// 非 object：包装为 object，保留原串内容供模型阅读，避免上游 400。
	wrapped, err := json.Marshal(map[string]string{"raw": s})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(wrapped)
}

// toolUseInputJSON 尝试从 s 开头解出一个 JSON object（保留原始字节）。
func toolUseInputJSON(s string) (json.RawMessage, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil || len(raw) == 0 {
		return nil, false
	}
	if raw[0] != '{' {
		return nil, false
	}
	return raw, true
}

func appendCustomToolCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseCustomToolCallParam) error {
	return appendToolUse(out, call.CallID, toolcatalog.ToolName(call.Namespace.Value, call.Name), map[string]any{"s": call.Input})
}

func appendShellCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemShellCallParam) error {
	input := map[string]any{
		"s": strings.Join(call.Action.Commands, "\n"),
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
		"s": strings.Join(call.Action.Command, " "),
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
	if call.Status != "" {
		input["status"] = call.Status
	}
	putCallerMeta(input, call.Caller.OfDirect != nil, call.Caller.OfProgram != nil, call.Caller.GetCallerID())
	return appendToolUse(out, call.CallID, "apply_patch", input)
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
	if id == "" || name == "" {
		slog.Warn("跳过缺少 id 或 name 的 tool_use（上游会 400 拒绝）",
			"id", id,
			"name", name,
			"impact", "该 tool call 不进入上游 messages")
		return nil
	}
	// 兜底：任何非 object 的 input 都会触发 Grok 类后端 400。
	input = ensureToolUseInputObject(input)
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

// ensureToolUseInputObject 保证 tool_use.input 序列化为 JSON object。
// Anthropic 官方与 Grok 等兼容后端要求 input 为 object；string/array/null 会 400。
func ensureToolUseInputObject(input any) any {
	switch v := input.(type) {
	case nil:
		return json.RawMessage(`{}`)
	case json.RawMessage:
		if len(v) == 0 || v[0] != '{' {
			return wrapNonObjectToolUseInput(string(v))
		}
		return v
	case map[string]any:
		return v
	case string:
		// 再走一遍透传：可能是 JSON object 字符串。
		return toolUseInputPassthrough(v)
	default:
		// 已是 struct 等可序列化类型：检查序列化后是否以 { 开头。
		raw, err := json.Marshal(v)
		if err != nil || len(raw) == 0 || raw[0] != '{' {
			return wrapNonObjectToolUseInput(fmt.Sprint(v))
		}
		return json.RawMessage(raw)
	}
}

func wrapNonObjectToolUseInput(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	wrapped, err := json.Marshal(map[string]string{"raw": s})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(wrapped)
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

// toolSearchArgumentsInput 与 function_call 一致：优先 object；非 object 包成 {"raw":...}。
func toolSearchArgumentsInput(args any) any {
	switch v := args.(type) {
	case string:
		return toolUseInputPassthrough(v)
	case nil:
		return json.RawMessage(`{}`)
	case json.RawMessage:
		if len(v) == 0 {
			return json.RawMessage(`{}`)
		}
		return toolUseInputPassthrough(string(v))
	default:
		return ensureToolUseInputObject(v)
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

// appendMcpCall 把历史 mcp_call 直接回填为标准 tool_use + tool_result。
// MCP 由 Codex 客户端本地执行，历史形态与回程一致走扁平名
// mcp__<server>__<tool>，不再重建 beta mcp_tool_use/mcp_tool_result。
func appendMcpCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseInputItemMcpCallParam) error {
	if call.ID == "" {
		return nil
	}
	name := toolcatalog.ToolName("mcp__"+call.ServerLabel, call.Name)
	if err := appendToolUse(out, call.ID, name, toolUseInputPassthrough(call.Arguments)); err != nil {
		return err
	}
	if call.Error.Valid() && call.Error.Value != "" {
		return appendToolResult(out, call.ID, call.Error.Value)
	}
	if call.Output.Valid() {
		return appendToolResult(out, call.ID, call.Output.Value)
	}
	// 缺输出时不补空 tool_result，交由 ensureToolUsePaired 补 is_error 占位 + WARN。
	return nil
}

// appendMcpListTools 把历史 mcp_list_tools 的可用工具注入 out.Tools 声明，
// 不写 system 文本、不转模型消息（对齐 c 路径与 tool_search_output）。
func appendMcpListTools(out *anthropic.MessageNewParams, list *oairesponses.ResponseInputItemMcpListToolsParam) error {
	if list == nil {
		return nil
	}
	if list.Error.Valid() && list.Error.Value != "" {
		slog.Warn("mcp_list_tools 返回错误，不注入工具声明",
			"server_label", list.ServerLabel,
			"error", list.Error.Value,
			"impact", "工具列表不可用，历史 mcp_call 仍按扁平名回填")
		return nil
	}
	count := 0
	for _, tl := range list.Tools {
		if tl.Name == "" {
			continue
		}
		name := toolcatalog.ToolName("mcp__"+list.ServerLabel, tl.Name)
		if hasTool(out, name) {
			continue
		}
		var desc *string
		if tl.Description.Valid() {
			v := tl.Description.Value
			desc = &v
		}
		schema, _ := tl.InputSchema.(map[string]any)
		out.Tools = append(out.Tools, toolcatalog.ClientTool(name, schema, desc))
		count++
	}
	if count > 0 {
		slog.Debug("mcp_list_tools 历史注入工具声明",
			"server_label", list.ServerLabel,
			"tool_count", count)
	}
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
	// web_search_tool_result 只能出现在 assistant 消息（DeepSeek 400 实测）。
	// OpenAI wire 无 Anthropic required 的 encrypted_content，result content 固定
	// 空数组；URL 列表折成可见文本放在同一 assistant 消息里，保留模型可读上下文。
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
// code_interpreter_call 已整体丢弃，不参与配对；web_search 等 server_tool_use
// 自带配对 result，不受影响。
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

// ensureToolResultProximity 保证每条 assistant message 的 tool_use 结果都在
// 紧接的下一条 user message 中。若 tool_result 出现在更远的 user 中，把对应
// tool_use 摘出，前移/后移到紧邻该 result 所在 user 之前的 assistant 里
// （Grok 等上游要求 assistant 的全部待处理 tool_use 在下一条 user 中闭环）。
//
// 实现为单遍重建：先在原始索引上规划「哪些 tool_use 移到哪条 user 之前」，
// 再一次性生成新 messages。禁止就地插入/删除——突变会使已计算的位置索引
// 失效，多批迁移场景会漏修。产出的相邻同 role 消息由随后的
// coalesceSameRoleMessages 合并，保持严格交替。
func ensureToolResultProximity(out *anthropic.MessageNewParams) {
	// 每个 tool_result 所在 user message 的原始索引。
	resultPos := map[string]int{}
	for i := range out.Messages {
		m := &out.Messages[i]
		if m.Role != anthropic.MessageParamRoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.OfToolResult != nil {
				resultPos[b.OfToolResult.ToolUseID] = i
			}
		}
	}
	if len(resultPos) == 0 {
		return
	}

	// 规划：moved[target] 是要插到原始索引 target（user 消息）之前的
	// tool_use，按扫描顺序保序；kept[i] 是第 i 条消息保留的 content。
	moved := map[int][]anthropic.ContentBlockParamUnion{}
	kept := make([][]anthropic.ContentBlockParamUnion, len(out.Messages))
	total := 0
	for i := range out.Messages {
		m := &out.Messages[i]
		kept[i] = m.Content
		if m.Role != anthropic.MessageParamRoleAssistant {
			continue
		}
		var stay []anthropic.ContentBlockParamUnion
		changed := false
		for _, b := range m.Content {
			if b.OfToolUse != nil {
				if pos, ok := resultPos[b.OfToolUse.ID]; ok && pos != i+1 {
					moved[pos] = append(moved[pos], b)
					total++
					changed = true
					continue
				}
			}
			stay = append(stay, b)
		}
		if changed {
			kept[i] = stay
		}
	}
	if total == 0 {
		return
	}

	rebuilt := make([]anthropic.MessageParam, 0, len(out.Messages)+len(moved))
	for i := range out.Messages {
		if blocks := moved[i]; len(blocks) != 0 {
			rebuilt = append(rebuilt, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: blocks,
			})
		}
		if len(kept[i]) == 0 {
			// tool_use 全部迁走的 assistant：整条移除，避免空 content 400。
			continue
		}
		msg := out.Messages[i]
		msg.Content = kept[i]
		rebuilt = append(rebuilt, msg)
	}
	out.Messages = rebuilt
	slog.Debug("tool_result 邻近性修复完成", "moved_tool_use", total)
}

// reasoningEffortToOutputConfig 把 OpenAI reasoning.effort 映射到 Anthropic
// output_config.effort。覆盖 Anthropic 全部五档：
//
//	none → thinking disabled（不走本表）
//	low/medium/high/xhigh/max → 同名透传
//
// 键与值均从官方 SDK 常量派生，禁止裸字符串。
// OpenAI 的 minimal 不单独映射（按产品要求不补）。
var reasoningEffortToOutputConfig = map[string]anthropic.OutputConfigEffort{
	model.ReasoningEffortLow:    anthropic.OutputConfigEffortLow,
	model.ReasoningEffortMedium: anthropic.OutputConfigEffortMedium,
	model.ReasoningEffortHigh:   anthropic.OutputConfigEffortHigh,
	model.ReasoningEffortXhigh:  anthropic.OutputConfigEffortXhigh,
	model.ReasoningEffortMax:    anthropic.OutputConfigEffortMax,
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

	// 用 Override 注入 raw JSON，不传 budget_tokens（SDK struct 无 omitempty，
	// 零值会序列化为 0 导致 API 拒绝；effort 由 output_config.effort 控制）。
	summarized := string(req.Reasoning.Summary) == model.ReasoningSummaryConcise
	thinkingJSON := `{"type":"enabled"}`
	if summarized {
		thinkingJSON = `{"type":"enabled","display":"summarized"}`
	}
	out.Thinking = aparam.Override[anthropic.ThinkingConfigParamUnion](
		json.RawMessage(thinkingJSON),
	)

	// 映射 output_config.effort：语义级别让模型自行决定 thinking 深度。
	if mapped, ok := reasoningEffortToOutputConfig[effort]; ok {
		out.OutputConfig.Effort = mapped
		return
	}
	// 未知档位仍开 thinking，但不伪造 output_config.effort，交上游决定。
	slog.Debug("未知 reasoning.effort，仅启用 thinking，不写 output_config.effort",
		"effort", effort,
		"impact", "Anthropic 按默认 effort 处理；兼容后端可能静默忽略")
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

func hasTool(out *anthropic.MessageNewParams, name string) bool {
	for _, tool := range out.Tools {
		if tool.OfTool != nil && tool.OfTool.Name == name {
			return true
		}
	}
	return false
}

func convertToolChoice(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) error {
	tc := req.ToolChoice
	switch {
	case tc.OfHostedTool != nil:
		// hosted tool_choice 无 Anthropic 等价物：由上游自行决定如何处理。网关不代劳拒绝。
		slog.Debug("hosted tool_choice 交给上游处理（网关不映射 Anthropic 等价 type）",
			"tool_choice_type", *tc.GetType())
		return nil
	case tc.OfMcpTool != nil:
		// MCP 工具已展开为扁平 function 声明（mcp__<server>__<tool>），
		// 映射为标准 tool choice。
		mcp := tc.OfMcpTool
		flatName := toolcatalog.ToolName("mcp__"+mcp.ServerLabel, mcp.Name.Value)
		if err := applySpecificToolChoice(out, req.Tools, toolcatalog.Identity{OpenAIType: model.ToolTypeFunction, Name: flatName}); err != nil {
			return err
		}
		return nil
	case tc.OfResponseNewsToolChoiceSpecificProgrammaticToolCallingParam != nil:
		// programmatic tool_choice 无 Anthropic 等价物：由上游自行决定。网关不代劳拒绝。
		slog.Debug("programmatic tool_choice 交给上游处理（网关不映射 Anthropic 等价 type）")
		return nil
	}
	if tc.OfAllowedTools != nil {
		if err := applyAllowedTools(out, req.Tools, tc.OfAllowedTools); err != nil {
			return err
		}
		applyParallelToolChoice(out, req)
		return nil
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
			names, err := expandAllowedIdentity(declaredIdentities, identity)
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				allowedNames[name] = true
			}
		}
	}
	return allowedNames, nil
}

// expandAllowedIdentity 校验 allowed_tools 条目并把可用的扁平工具名展开。
// mcp 条目只带 server_label（无 name）时代表放行整个 server 的已声明工具。
func expandAllowedIdentity(declared []toolcatalog.Identity, id toolcatalog.Identity) ([]string, error) {
	if id.OpenAIType == model.ToolTypeMcp && id.Name == "" {
		var names []string
		for _, d := range declared {
			if d.Namespace == id.Namespace {
				names = append(names, d.ConvertedName())
			}
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("tool_choice allowed_tools entry %s is not declared", id)
		}
		return names, nil
	}
	if !hasToolIdentity(declared, id) {
		return nil, fmt.Errorf("tool_choice allowed_tools entry %s is not declared", id)
	}
	return []string{id.ConvertedName()}, nil
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
	// 兼容扁平名选择：namespace / mcp 声明展开成 ns__name / mcp__server__tool 后，
	// tool_choice 可能按扁平 function 名选择，按 ConvertedName 兜底匹配。
	if want.Namespace == "" {
		for _, identity := range identities {
			if identity.Namespace != "" && identity.ConvertedName() == want.Name {
				return true
			}
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
	if cfg != nil && !cfg.Anthropic.CacheEnabledValue() {
		return
	}
	// TTL 固定 5m，不再可配。
	cacheControl := anthropic.NewCacheControlEphemeralParam()
	cacheControl.TTL = anthropic.CacheControlEphemeralTTLTTL5m
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
		slog.Warn("最后一个 tool 是未知变体，无法加 cache_control",
			"impact", "tools 列表缓存将丢失",
			"tool_index", len(tools)-1)
	}
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
