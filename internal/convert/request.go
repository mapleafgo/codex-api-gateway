package convert

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	aparam "github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
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
func ToAnthropic(req *oairesponses.ResponseNewParams, cfg *config.Config, prevItems ...model.OutputItem) (*anthropic.MessageNewParams, error) {
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

	applyReasoning(out, req, cfg)

	if err := convertTools(out, req); err != nil {
		return nil, err
	}
	if err := injectStructuredOutput(out, req); err != nil {
		return nil, err
	}
	if err := convertToolChoice(out, req); err != nil {
		return nil, err
	}
	applyAnthropicCacheControl(out)
	return out, nil
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
	return appendToolUse(out, fc.CallID, toolName(fc.Namespace.Value, fc.Name), json.RawMessage(orDefault(fc.Arguments, `{}`)))
}

func appendCustomToolCall(out *anthropic.MessageNewParams, call *oairesponses.ResponseCustomToolCallParam) error {
	return appendToolUse(out, call.CallID, toolName(call.Namespace.Value, call.Name), map[string]any{"input": call.Input})
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
	return appendToolUse(out, callID, "tool_search", call.Arguments)
}

func appendToolSearchOutput(out *anthropic.MessageNewParams, sysParts *[]instructionPart, output *oairesponses.ResponseToolSearchOutputItemParam) error {
	if err := appendToolList(out, output.Tools); err != nil {
		return err
	}
	*sysParts = append(*sysParts, instructionPart{
		role: model.RoleDeveloper,
		text: formatToolNames("tool_search_output", output.Tools),
	})
	if !output.CallID.Valid() || output.CallID.Value == "" {
		return nil
	}
	return appendToolResult(out, output.CallID.Value, formatToolNames("tool_search_output", output.Tools))
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

func toolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "__" + name
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

// toInputSchema 将 Response 协议的完整 JSON schema（{type,properties,required,...}）
// 映射为 Anthropic 的 ToolInputSchemaParam。Anthropic 把 schema 拆成 Properties +
// Required + Type(默认 "object") 三个字段，不能把整个 schema 塞进 Properties —— 否则
// 生成的 input_schema 会 properties 套 properties 双重包裹，被智谱等严格网关以 400 拒绝。
func toInputSchema(schema map[string]any) anthropic.ToolInputSchemaParam {
	props, _ := schema["properties"].(map[string]any)
	var required []string
	switch r := schema["required"].(type) {
	case []string:
		required = r
	case []any:
		required = make([]string, 0, len(r))
		for _, item := range r {
			if s, ok := item.(string); ok {
				required = append(required, s)
			}
		}
	}
	return anthropic.ToolInputSchemaParam{Properties: props, Required: required}
}

func convertTools(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) error {
	return appendToolList(out, req.Tools)
}

func appendToolList(out *anthropic.MessageNewParams, tools []oairesponses.ToolUnionParam) error {
	for _, t := range tools {
		if err := appendToolUnion(out, t); err != nil {
			return err
		}
	}
	return nil
}

func appendToolUnion(out *anthropic.MessageNewParams, t oairesponses.ToolUnionParam) error {
	switch {
	case t.OfFunction != nil:
		fn := t.OfFunction
		return appendConvertedTool(out, fn.Name, fn.Parameters, optionalString(fn.Description), false)
	case t.OfCustom != nil:
		custom := t.OfCustom
		return appendConvertedTool(out, custom.Name, freeformInputSchema(), optionalString(custom.Description), true)
	case t.OfApplyPatch != nil:
		return appendConvertedTool(out, "apply_patch", applyPatchInputSchema(), nil, true)
	case t.OfShell != nil:
		return appendConvertedTool(out, "shell", freeformInputSchema(), nil, true)
	case t.OfLocalShell != nil:
		return appendConvertedTool(out, "shell", freeformInputSchema(), nil, true)
	case t.OfToolSearch != nil:
		search := t.OfToolSearch
		return appendConvertedTool(out, "tool_search", schemaFromAny(search.Parameters), optionalString(search.Description), false)
	case t.OfNamespace != nil:
		namespace := t.OfNamespace
		for _, nested := range namespace.Tools {
			if nested.OfFunction != nil {
				fn := nested.OfFunction
				if err := appendConvertedTool(out, toolName(namespace.Name, fn.Name), schemaFromAny(fn.Parameters), optionalString(fn.Description), false); err != nil {
					return err
				}
			} else if nested.OfCustom != nil {
				custom := nested.OfCustom
				if err := appendConvertedTool(out, toolName(namespace.Name, custom.Name), freeformInputSchema(), optionalString(custom.Description), true); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("unsupported namespace tool: Anthropic backend has no safe equivalent")
			}
		}
	case t.OfWebSearch != nil:
		return appendWebSearchTool(out, t.OfWebSearch.Filters.AllowedDomains)
	case t.OfWebSearchPreview != nil:
		return appendWebSearchTool(out, nil)
	default:
		return fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", toolType(t))
	}
	return nil
}

// appendWebSearchTool maps an OpenAI web_search / web_search_preview tool to
// Anthropic's native web search server tool (web_search_20250305). Both
// backends treat web search as a hosted tool, so this is a real mapping, not a
// drop. filters.allowed_domains maps to Anthropic allowed_domains; search_context_size
// has no Anthropic equivalent (Anthropic controls volume via max_uses) and is
// ignored in this MVP.
func appendWebSearchTool(out *anthropic.MessageNewParams, allowedDomains []string) error {
	out.Tools = append(out.Tools, anthropic.ToolUnionParam{
		OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
			AllowedDomains: allowedDomains,
		},
	})
	return nil
}

func toolType(t oairesponses.ToolUnionParam) string {
	if typ := t.GetType(); typ != nil && *typ != "" {
		return *typ
	}
	raw, _ := json.Marshal(t)
	var obj struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &obj)
	if obj.Type != "" {
		return obj.Type
	}
	return "unknown"
}

func appendConvertedTool(out *anthropic.MessageNewParams, name string, schema map[string]any, description *string, custom bool) error {
	if name == "" {
		return fmt.Errorf("tool conversion requires a name")
	}
	if hasTool(out, name) {
		return fmt.Errorf("tool conversion name conflict for %q", name)
	}
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	tool := &anthropic.ToolParam{
		Name:        name,
		InputSchema: toInputSchema(schema),
	}
	if description != nil {
		tool.Description = aparam.NewOpt(*description)
	}
	if custom {
		tool.Type = anthropic.ToolTypeCustom
	}
	out.Tools = append(out.Tools, anthropic.ToolUnionParam{OfTool: tool})
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

func schemaFromAny(v any) map[string]any {
	schema, _ := v.(map[string]any)
	return schema
}

func freeformInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{"type": "string"},
		},
		"required": []string{"input"},
	}
}

func applyPatchInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operation": map[string]any{"type": "string", "enum": []string{"create_file", "delete_file", "update_file"}},
			"path":      map[string]any{"type": "string"},
			"diff":      map[string]any{"type": "string"},
		},
		"required": []string{"operation", "path"},
	}
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
		return applySpecificToolChoice(out, req.Tools, toolIdentity{typ: "function", name: tc.OfFunctionTool.Name})
	}
	if tc.OfCustomTool != nil {
		return applySpecificToolChoice(out, req.Tools, toolIdentity{typ: "custom", name: tc.OfCustomTool.Name})
	}
	if tc.OfSpecificApplyPatchToolChoice != nil {
		return applySpecificToolChoice(out, req.Tools, toolIdentity{typ: "apply_patch", name: "apply_patch"})
	}
	if tc.OfSpecificShellToolChoice != nil {
		return applySpecificToolChoice(out, req.Tools, toolIdentity{typ: "shell", name: "shell"})
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

func applySpecificToolChoice(out *anthropic.MessageNewParams, declared []oairesponses.ToolUnionParam, want toolIdentity) error {
	identities, err := declaredToolIdentities(declared)
	if err != nil {
		return err
	}
	if !hasToolIdentity(identities, want) {
		return fmt.Errorf("tool_choice %s is not declared", want)
	}
	out.ToolChoice = anthropic.ToolChoiceUnionParam{
		OfTool: &anthropic.ToolChoiceToolParam{Name: want.convertedName()},
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
			allowedNames[identity.convertedName()] = true
		}
	}
	return allowedNames, nil
}

func declaredToolIdentities(tools []oairesponses.ToolUnionParam) ([]toolIdentity, error) {
	identities := make([]toolIdentity, 0, len(tools))
	for _, tool := range tools {
		switch {
		case tool.OfFunction != nil:
			identities = append(identities, toolIdentity{typ: "function", name: tool.OfFunction.Name})
		case tool.OfCustom != nil:
			identities = append(identities, toolIdentity{typ: "custom", name: tool.OfCustom.Name})
		case tool.OfApplyPatch != nil:
			identities = append(identities, toolIdentity{typ: "apply_patch", name: "apply_patch"})
		case tool.OfShell != nil:
			identities = append(identities, toolIdentity{typ: "shell", name: "shell"})
		case tool.OfLocalShell != nil:
			identities = append(identities, toolIdentity{typ: "local_shell", name: "shell"})
		case tool.OfToolSearch != nil:
			identities = append(identities, toolIdentity{typ: "tool_search", name: "tool_search"})
		case tool.OfNamespace != nil:
			for _, nested := range tool.OfNamespace.Tools {
				if nested.OfFunction != nil {
					identities = append(identities, toolIdentity{typ: "function", namespace: tool.OfNamespace.Name, name: nested.OfFunction.Name})
					continue
				}
				if nested.OfCustom != nil {
					identities = append(identities, toolIdentity{typ: "custom", namespace: tool.OfNamespace.Name, name: nested.OfCustom.Name})
					continue
				}
				return nil, fmt.Errorf("unsupported namespace tool: Anthropic backend has no safe equivalent")
			}
		default:
			return nil, fmt.Errorf("unsupported tool type %q: Anthropic backend has no safe equivalent", toolType(tool))
		}
	}
	return identities, nil
}

func hasToolIdentity(identities []toolIdentity, want toolIdentity) bool {
	for _, identity := range identities {
		if identity == want {
			return true
		}
	}
	return false
}

type toolIdentity struct {
	typ       string
	namespace string
	name      string
}

func (i toolIdentity) convertedName() string {
	if i.namespace != "" {
		return toolName(i.namespace, i.name)
	}
	return i.name
}

func (i toolIdentity) String() string {
	if i.namespace != "" {
		return fmt.Sprintf("%s %q in namespace %q", i.typ, i.name, i.namespace)
	}
	return fmt.Sprintf("%s %q", i.typ, i.name)
}

func parseAllowedToolIdentities(tool map[string]any) ([]toolIdentity, error) {
	typ, ok := tool["type"].(string)
	if !ok || typ == "" {
		return nil, fmt.Errorf("tool_choice allowed_tools entry requires a type")
	}
	switch typ {
	case "shell", "local_shell":
		return []toolIdentity{{typ: typ, name: "shell"}}, nil
	case "apply_patch":
		return []toolIdentity{{typ: typ, name: "apply_patch"}}, nil
	case "tool_search":
		return []toolIdentity{{typ: typ, name: "tool_search"}}, nil
	case "function", "custom":
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tool_choice allowed_tools entry %q requires a name", typ)
		}
		return []toolIdentity{{typ: typ, name: name}}, nil
	case "namespace":
		return parseAllowedNamespaceToolIdentities(tool)
	default:
		return nil, fmt.Errorf("unsupported tool_choice allowed_tools entry %q: Anthropic backend has no safe equivalent", typ)
	}
}

func parseAllowedNamespaceToolIdentities(tool map[string]any) ([]toolIdentity, error) {
	namespace, _ := tool["name"].(string)
	if namespace == "" {
		return nil, fmt.Errorf("tool_choice allowed_tools namespace requires a name")
	}
	rawTools, _ := tool["tools"].([]any)
	if len(rawTools) == 0 {
		return nil, fmt.Errorf("tool_choice allowed_tools namespace %q requires tools", namespace)
	}
	identities := make([]toolIdentity, 0, len(rawTools))
	for _, rawTool := range rawTools {
		nested, ok := rawTool.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool_choice allowed_tools namespace %q has invalid child", namespace)
		}
		typ, _ := nested["type"].(string)
		if typ != "function" && typ != "custom" {
			return nil, fmt.Errorf("unsupported tool_choice allowed_tools namespace %q child type %q", namespace, typ)
		}
		name, _ := nested["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("tool_choice allowed_tools namespace %q child %q requires a name", namespace, typ)
		}
		identities = append(identities, toolIdentity{typ: typ, namespace: namespace, name: name})
	}
	return identities, nil
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

func applyAnthropicCacheControl(out *anthropic.MessageNewParams) {
	cacheControl := anthropic.NewCacheControlEphemeralParam()
	cacheControl.TTL = anthropic.CacheControlEphemeralTTLTTL5m
	if len(out.System) > 0 {
		out.System[len(out.System)-1].CacheControl = cacheControl
	}
	setLastToolCacheControl(out.Tools, cacheControl)
}

// setLastToolCacheControl 给 tools 列表的最后一个 tool 加 cache_control,
// 按 union 变体派发(OfTool / OfWebSearchTool20250305)。hosted server tool
// 变体覆盖齐全后可继续在此 switch 扩展。
func setLastToolCacheControl(tools []anthropic.ToolUnionParam, cc anthropic.CacheControlEphemeralParam) {
	if len(tools) == 0 {
		return
	}
	last := &tools[len(tools)-1]
	switch {
	case last.OfTool != nil:
		last.OfTool.CacheControl = cc
	case last.OfWebSearchTool20250305 != nil:
		last.OfWebSearchTool20250305.CacheControl = cc
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
		switch {
		case tool.OfFunction != nil:
			names = append(names, tool.OfFunction.Name)
		case tool.OfCustom != nil:
			names = append(names, tool.OfCustom.Name)
		case tool.OfApplyPatch != nil:
			names = append(names, "apply_patch")
		case tool.OfShell != nil || tool.OfLocalShell != nil:
			names = append(names, "shell")
		case tool.OfToolSearch != nil:
			names = append(names, "tool_search")
		case tool.OfNamespace != nil:
			namespace := tool.OfNamespace
			for _, nested := range namespace.Tools {
				if nested.OfFunction != nil {
					names = append(names, toolName(namespace.Name, nested.OfFunction.Name))
				} else if nested.OfCustom != nil {
					names = append(names, toolName(namespace.Name, nested.OfCustom.Name))
				}
			}
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
