package convert

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	aparam "github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
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

	var sysParts []string

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
		sysParts = append([]string{req.Instructions.Value}, sysParts...)
	}
	if len(sysParts) > 0 {
		out.System = []anthropic.TextBlockParam{{Text: joinNonEmpty("\n", sysParts)}}
	}

	applyReasoning(out, req, cfg)

	if err := convertTools(out, req); err != nil {
		return nil, err
	}
	injectStructuredOutput(out, req)
	convertToolChoice(out, req)
	applyAnthropicCacheControl(out)
	return out, nil
}

func appendItem(out *anthropic.MessageNewParams, sysParts *[]string, item *oairesponses.ResponseInputItemUnionParam, sigByID map[string]string) error {
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
	return nil
}

func appendMessage(out *anthropic.MessageNewParams, sysParts *[]string, m *oairesponses.EasyInputMessageParam) error {
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
		*sysParts = append(*sysParts, joinNonEmpty("\n", textParts))
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
	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleAssistant {
		out.Messages = append(out.Messages, anthropic.NewAssistantMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	last.Content = append(last.Content, anthropic.ContentBlockParamUnion{
		OfToolUse: &anthropic.ToolUseBlockParam{
			ID:    fc.CallID,
			Name:  fc.Name,
			Input: json.RawMessage(orDefault(fc.Arguments, `{}`)),
		},
	})
	return nil
}

func appendFunctionCallOutput(out *anthropic.MessageNewParams, fco *oairesponses.ResponseInputItemFunctionCallOutputParam) error {
	if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != anthropic.MessageParamRoleUser {
		out.Messages = append(out.Messages, anthropic.NewUserMessage())
	}
	last := &out.Messages[len(out.Messages)-1]
	outputText := ""
	if fco.Output.OfString.Valid() {
		outputText = fco.Output.OfString.Value
	}
	last.Content = append(last.Content, anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: fco.CallID,
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
	for _, t := range req.Tools {
		if t.OfFunction == nil {
			continue
		}
		fn := t.OfFunction
		schema := fn.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tool := &anthropic.ToolParam{
			Name:        fn.Name,
			InputSchema: toInputSchema(schema),
		}
		if fn.Description.Valid() {
			tool.Description = aparam.NewOpt(fn.Description.Value)
		}
		out.Tools = append(out.Tools, anthropic.ToolUnionParam{OfTool: tool})
	}
	return nil
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

func convertToolChoice(out *anthropic.MessageNewParams, req *oairesponses.ResponseNewParams) {
	if out.ToolChoice.OfTool != nil {
		return // already forced by structured output
	}
	if len(out.Tools) == 0 {
		return
	}
	defer applyParallelToolChoice(out, req)
	tc := req.ToolChoice
	if tc.OfToolChoiceMode.Valid() {
		switch string(tc.OfToolChoiceMode.Value) {
		case model.ToolChoiceAuto:
			out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
		case model.ToolChoiceRequired:
			out.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
		case model.ToolChoiceNone:
			out.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		}
		return
	}
	if tc.OfFunctionTool != nil {
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: tc.OfFunctionTool.Name},
		}
	} else if tc.OfCustomTool != nil {
		out.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: tc.OfCustomTool.Name},
		}
	}
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

func joinNonEmpty(sep string, parts []string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, sep)
}
