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
	}
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
	for i := range req.Input.OfInputItemList {
		item := &req.Input.OfInputItemList[i]
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
	injectStructuredOutput(out, req)
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
		appendToolList(out, item.OfAdditionalTools.Tools)
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

	if role == model.RoleAssistant && m.Phase != "" {
		applyAssistantPhase(blocks, string(m.Phase))
	}

	out.Messages = append(out.Messages, anthropic.MessageParam{
		Role:    anthropic.MessageParamRole(role),
		Content: blocks,
	})
	return nil
}

func applyAssistantPhase(blocks []anthropic.ContentBlockParamUnion, phase string) {
	if len(blocks) == 0 || phase == "" {
		return
	}
	marker := "<assistant_phase>" + phase + "</assistant_phase>\n"
	for i := range blocks {
		if blocks[i].OfText != nil {
			blocks[i].OfText.Text = marker + blocks[i].OfText.Text
			return
		}
	}
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
	patch := ""
	if diff := call.Operation.GetDiff(); diff != nil {
		patch = *diff
	}
	return appendToolUse(out, call.CallID, "apply_patch", map[string]any{"input": patch})
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
	appendToolList(out, output.Tools)
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
	appendToolList(out, req.Tools)
	return nil
}

func appendToolList(out *anthropic.MessageNewParams, tools []oairesponses.ToolUnionParam) {
	for _, t := range tools {
		appendToolUnion(out, t)
	}
}

func appendToolUnion(out *anthropic.MessageNewParams, t oairesponses.ToolUnionParam) {
	switch {
	case t.OfFunction != nil:
		fn := t.OfFunction
		appendConvertedTool(out, fn.Name, fn.Parameters, optionalString(fn.Description), false)
	case t.OfCustom != nil:
		custom := t.OfCustom
		appendConvertedTool(out, custom.Name, freeformInputSchema(), optionalString(custom.Description), true)
	case t.OfApplyPatch != nil:
		appendConvertedTool(out, "apply_patch", freeformInputSchema(), nil, true)
	case t.OfShell != nil:
		appendConvertedTool(out, "shell", freeformInputSchema(), nil, true)
	case t.OfLocalShell != nil:
		appendConvertedTool(out, "shell", freeformInputSchema(), nil, true)
	case t.OfToolSearch != nil:
		search := t.OfToolSearch
		appendConvertedTool(out, "tool_search", schemaFromAny(search.Parameters), optionalString(search.Description), false)
	case t.OfNamespace != nil:
		namespace := t.OfNamespace
		for _, nested := range namespace.Tools {
			if nested.OfFunction != nil {
				fn := nested.OfFunction
				appendConvertedTool(out, toolName(namespace.Name, fn.Name), schemaFromAny(fn.Parameters), optionalString(fn.Description), false)
			} else if nested.OfCustom != nil {
				custom := nested.OfCustom
				appendConvertedTool(out, toolName(namespace.Name, custom.Name), freeformInputSchema(), optionalString(custom.Description), true)
			}
		}
	}
}

func appendConvertedTool(out *anthropic.MessageNewParams, name string, schema map[string]any, description *string, custom bool) {
	if name == "" {
		return
	}
	if hasTool(out, name) {
		return
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

func injectStructuredOutput(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) {
	if req.Text.Format.OfJSONSchema != nil {
		f := req.Text.Format.OfJSONSchema
		tool := &anthropic.ToolParam{
			Name:        f.Name,
			InputSchema: toInputSchema(f.Schema),
		}
		if f.Description.Valid() {
			tool.Description = aparam.NewOpt(f.Description.Value)
		}
		out.Tools = append(out.Tools, anthropic.ToolUnionParam{OfTool: tool})
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: f.Name},
		}
		return
	}
	if req.Text.Format.OfJSONObject != nil {
		out.Tools = append(out.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        model.StructuredOutputJSONObjectTool,
			InputSchema: anthropic.ToolInputSchemaParam{}, // Type 默认 "object"，表示任意对象
		}})
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: model.StructuredOutputJSONObjectTool},
		}
	}
}

func convertToolChoice(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) error {
	if out.ToolChoice.OfTool != nil {
		return nil // already forced by structured output
	}
	tc := req.ToolChoice
	if tc.OfAllowedTools != nil {
		if err := applyAllowedTools(out, tc.OfAllowedTools); err != nil {
			return err
		}
		applyParallelToolChoice(out, req)
		return nil
	}
	if len(out.Tools) == 0 {
		return nil
	}
	defer applyParallelToolChoice(out, req)
	if tc.OfToolChoiceMode.Valid() {
		switch string(tc.OfToolChoiceMode.Value) {
		case model.ToolChoiceAuto:
			out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
		case model.ToolChoiceRequired:
			out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
		case model.ToolChoiceNone:
			out.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		}
		return nil
	}
	if tc.OfFunctionTool != nil {
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: tc.OfFunctionTool.Name},
		}
	} else if tc.OfCustomTool != nil {
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: tc.OfCustomTool.Name},
		}
	} else if tc.OfSpecificApplyPatchToolChoice != nil {
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: "apply_patch"},
		}
	} else if tc.OfSpecificShellToolChoice != nil {
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: "shell"},
		}
	}
	return nil
}

func applyAllowedTools(out *anthropic.MessageNewParams, allowed *oairesponses.ToolChoiceAllowedParam) error {
	allowedNames := map[string]bool{}
	for _, tool := range allowed.Tools {
		name, _ := tool["name"].(string)
		if name != "" {
			allowedNames[name] = true
		}
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
	case oairesponses.ToolChoiceAllowedModeRequired:
		out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	default:
		out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	}
	return nil
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
	if len(out.Tools) > 0 && out.Tools[len(out.Tools)-1].OfTool != nil {
		out.Tools[len(out.Tools)-1].OfTool.CacheControl = cacheControl
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
