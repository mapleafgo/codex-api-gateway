// Package chatconvert 将 OpenAI Responses 请求转为 Chat Completions 请求（仅流式）。
package chatconvert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// ChatRequest 是 Chat Completions 流式请求的最小结构。
// FreeformNames 不进 wire，仅供 chatstreamconv 识别 shell/apply_patch/custom 回程形态。
type ChatRequest struct {
	Model               string              `json:"model"`
	Messages            []ChatMessage       `json:"messages"`
	Tools               []ChatTool          `json:"tools,omitempty"`
	ToolChoice          any                 `json:"tool_choice,omitempty"`
	Temperature         *float64            `json:"temperature,omitempty"`
	TopP                *float64            `json:"top_p,omitempty"`
	MaxTokens           *int                `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                `json:"max_completion_tokens,omitempty"`
	ParallelToolCalls   *bool               `json:"parallel_tool_calls,omitempty"`
	PromptCacheKey      *string             `json:"prompt_cache_key,omitempty"`
	PromptCacheOptions  *PromptCacheOptions `json:"prompt_cache_options,omitempty"`
	ResponseFormat      any                 `json:"response_format,omitempty"`
	Verbosity           *string             `json:"verbosity,omitempty"`
	ServiceTier         *string             `json:"service_tier,omitempty"`
	SafetyIdentifier    *string             `json:"safety_identifier,omitempty"`
	Metadata            map[string]string   `json:"metadata,omitempty"`
	Store               *bool               `json:"store,omitempty"`
	Moderation          *ChatModeration     `json:"moderation,omitempty"`
	ReasoningEffort     *string             `json:"reasoning_effort,omitempty"`
	Logprobs            *bool               `json:"logprobs,omitempty"`
	TopLogprobs         *int                `json:"top_logprobs,omitempty"`
	Stream              bool                `json:"stream"`
	StreamOptions       *StreamOptions      `json:"stream_options,omitempty"`
	FreeformNames       map[string]struct{} `json:"-"`
}

// PromptCacheOptions 对齐 OpenAI Chat prompt_cache_options 子集。
type PromptCacheOptions struct {
	Mode string `json:"mode,omitempty"`
	TTL  string `json:"ttl,omitempty"`
}

// StreamOptions 控制流式 usage / obfuscation。
type StreamOptions struct {
	IncludeUsage       bool  `json:"include_usage"`
	IncludeObfuscation *bool `json:"include_obfuscation,omitempty"`
}

// ChatModeration 对齐 Chat / Responses moderation 子集。
type ChatModeration struct {
	Model  string                `json:"model,omitempty"`
	Policy *ChatModerationPolicy `json:"policy,omitempty"`
}

// ChatModerationPolicy 是 moderation.policy。
type ChatModerationPolicy struct {
	Input  *ChatModerationMode `json:"input,omitempty"`
	Output *ChatModerationMode `json:"output,omitempty"`
}

// ChatModerationMode 是 score/block。
type ChatModerationMode struct {
	Mode string `json:"mode,omitempty"`
}

// ChatMessage 是 Chat 多轮消息。
type ChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"`
	// ReasoningContent 回传厂商推理文本（DeepSeek/Kimi/GLM 工具环常要求同框）。
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
}

// ChatTool 是 function 工具声明（Chat 无 custom type，freeform 也落 function）。
type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

// ChatFunction 是 function 定义。
type ChatFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

// ChatToolCall 是 assistant 侧 tool_calls 项。
type ChatToolCall struct {
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ChatToolCallFunc `json:"function"`
}

// ChatToolCallFunc 承载 name/arguments。
type ChatToolCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func ptr[T any](v T) *T { return &v }

// ToChat 将 Responses 请求转为 Chat 请求。
// model 应为已解析的上游模型名（由调用方做 ModelMap）。
// 调用前应经 convert.DecodeResponseNewParams，以恢复 assistant output_text 历史。
func ToChat(req *oairesponses.ResponseNewParams, model string) (*ChatRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("chatconvert: nil request")
	}
	out := &ChatRequest{
		Model:         model,
		Stream:        true,
		StreamOptions: &StreamOptions{IncludeUsage: true},
		FreeformNames: map[string]struct{}{},
	}
	if req.Temperature.Valid() {
		out.Temperature = ptr(req.Temperature.Value)
	}
	if req.TopP.Valid() {
		out.TopP = ptr(req.TopP.Value)
	}
	if req.MaxOutputTokens.Valid() && req.MaxOutputTokens.Value > 0 {
		n := int(req.MaxOutputTokens.Value)
		out.MaxTokens = ptr(n)
		out.MaxCompletionTokens = ptr(n)
	}
	if req.ParallelToolCalls.Valid() {
		out.ParallelToolCalls = ptr(req.ParallelToolCalls.Value)
	}
	if req.PromptCacheKey.Valid() && req.PromptCacheKey.Value != "" {
		out.PromptCacheKey = ptr(req.PromptCacheKey.Value)
	}
	if req.PromptCacheOptions.Mode != "" || req.PromptCacheOptions.Ttl != "" {
		out.PromptCacheOptions = &PromptCacheOptions{
			Mode: req.PromptCacheOptions.Mode,
			TTL:  req.PromptCacheOptions.Ttl,
		}
	}
	if rf := convertResponseFormat(req); rf != nil {
		out.ResponseFormat = rf
	}
	if v := string(req.Text.Verbosity); v != "" {
		out.Verbosity = ptr(v)
	}
	if st := string(req.ServiceTier); st != "" {
		out.ServiceTier = ptr(st)
	}
	if req.SafetyIdentifier.Valid() && req.SafetyIdentifier.Value != "" {
		out.SafetyIdentifier = ptr(req.SafetyIdentifier.Value)
	}
	if len(req.Metadata) > 0 {
		out.Metadata = map[string]string(req.Metadata)
	}
	if req.Store.Valid() {
		out.Store = ptr(req.Store.Value)
	}
	if m := convertChatModeration(req); m != nil {
		out.Moderation = m
	}
	if e := string(req.Reasoning.Effort); e != "" {
		out.ReasoningEffort = ptr(e)
	}
	if req.TopLogprobs.Valid() {
		n := int(req.TopLogprobs.Value)
		out.TopLogprobs = ptr(n)
		out.Logprobs = ptr(true)
	}
	if req.StreamOptions.IncludeObfuscation.Valid() {
		out.StreamOptions = &StreamOptions{
			IncludeUsage:       true,
			IncludeObfuscation: ptr(req.StreamOptions.IncludeObfuscation.Value),
		}
	}

	var dynamicTools []ChatTool
	msgs, err := convertMessages(req, out.FreeformNames, &dynamicTools)
	if err != nil {
		return nil, err
	}
	out.Messages = msgs
	ensureChatToolPaired(out)
	out.Tools = convertTools(req.Tools, out.FreeformNames)
	seen := map[string]struct{}{}
	for _, t := range out.Tools {
		seen[t.Function.Name] = struct{}{}
	}
	for _, t := range dynamicTools {
		if _, ok := seen[t.Function.Name]; ok {
			continue
		}
		seen[t.Function.Name] = struct{}{}
		out.Tools = append(out.Tools, t)
	}
	if req.ToolChoice.OfAllowedTools != nil {
		if err := applyChatAllowedTools(out, req.Tools, req.ToolChoice.OfAllowedTools); err != nil {
			return nil, err
		}
	} else if tc := convertToolChoice(req.ToolChoice); tc != nil {
		out.ToolChoice = tc
	}
	return out, nil
}

// Marshal 将 ChatRequest 编成可 POST 的 JSON（不含 FreeformNames）。
func Marshal(req *ChatRequest) ([]byte, error) {
	return json.Marshal(req)
}

// IsFreeformName 判断工具名是否应按 custom_tool_call 回程。
func (r *ChatRequest) IsFreeformName(name string) bool {
	if isBuiltinFreeform(name) {
		return true
	}
	if r == nil || r.FreeformNames == nil {
		return false
	}
	_, ok := r.FreeformNames[name]
	return ok
}

func isBuiltinFreeform(name string) bool {
	switch name {
	case "shell", "apply_patch":
		return true
	default:
		return false
	}
}

func convertMessages(req *oairesponses.ResponseNewParams, freeform map[string]struct{}, dynamicTools *[]ChatTool) ([]ChatMessage, error) {
	var out []ChatMessage
	if req.Instructions.Valid() && req.Instructions.Value != "" {
		out = append(out, ChatMessage{Role: "system", Content: req.Instructions.Value})
	}
	if req.Input.OfString.Valid() && req.Input.OfString.Value != "" {
		out = append(out, ChatMessage{Role: "user", Content: req.Input.OfString.Value})
		return out, nil
	}

	var pending *ChatMessage
	// pendingReasoning 暂存 Responses reasoning，挂到下一条/当前 assistant 的 reasoning_content。
	var pendingReasoning string
	takeReasoning := func() string {
		s := pendingReasoning
		pendingReasoning = ""
		return s
	}
	attachReasoning := func(msg *ChatMessage) {
		if msg == nil {
			return
		}
		if rc := takeReasoning(); rc != "" {
			if msg.ReasoningContent == "" {
				msg.ReasoningContent = rc
			} else {
				msg.ReasoningContent += "\n" + rc
			}
		}
	}
	flushPending := func() {
		if pending != nil {
			attachReasoning(pending)
			out = append(out, *pending)
			pending = nil
		}
	}
	appendToolCall := func(id, name, args string) {
		if pending == nil {
			pending = &ChatMessage{Role: "assistant"}
			attachReasoning(pending)
		}
		pending.ToolCalls = append(pending.ToolCalls, ChatToolCall{
			ID:   id,
			Type: "function",
			Function: ChatToolCallFunc{
				Name: name,
				// Chat Completions 要求 arguments 是合法 JSON 字符串；上游
				// （如 MiMo prefill）会对内容再 parse，截断/非 JSON 会 400。
				Arguments: chatFunctionArguments(args),
			},
		})
	}

	for i := range req.Input.OfInputItemList {
		item := &req.Input.OfInputItemList[i]
		switch {
		case item.OfMessage != nil:
			flushPending()
			if msg, ok := convertEasyMessage(item.OfMessage); ok {
				if msg.Role == "assistant" {
					attachReasoning(&msg)
				}
				out = append(out, msg)
			}
		case item.OfInputMessage != nil:
			flushPending()
			if msg, ok := convertInputMessage(item.OfInputMessage); ok {
				if msg.Role == "assistant" {
					attachReasoning(&msg)
				}
				out = append(out, msg)
			}
		case item.OfOutputMessage != nil:
			flushPending()
			if msg, ok := convertOutputMessage(item.OfOutputMessage); ok {
				attachReasoning(&msg)
				out = append(out, msg)
			}
		case item.OfFunctionCall != nil:
			fc := item.OfFunctionCall
			appendToolCall(fc.CallID, fc.Name, fc.Arguments)
		case item.OfFunctionCallOutput != nil:
			flushPending()
			fco := item.OfFunctionCallOutput
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: fco.CallID,
				Content:    functionCallOutputText(fco),
			})
		case item.OfCustomToolCall != nil:
			c := item.OfCustomToolCall
			freeform[c.Name] = struct{}{}
			appendToolCall(c.CallID, c.Name, freeformArgsJSON(c.Input))
		case item.OfCustomToolCallOutput != nil:
			flushPending()
			c := item.OfCustomToolCallOutput
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: c.CallID,
				Content:    customToolOutputText(c),
			})
		case item.OfShellCall != nil:
			call := item.OfShellCall
			freeform["shell"] = struct{}{}
			appendToolCall(call.CallID, "shell", freeformArgsJSON(strings.Join(call.Action.Commands, "\n")))
		case item.OfShellCallOutput != nil:
			flushPending()
			o := item.OfShellCallOutput
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: o.CallID,
				Content:    shellCallOutputText(o),
			})
		case item.OfLocalShellCall != nil:
			call := item.OfLocalShellCall
			freeform["shell"] = struct{}{}
			id := call.CallID
			if id == "" {
				id = call.ID
			}
			appendToolCall(id, "shell", freeformArgsJSON(strings.Join(call.Action.Command, " ")))
		case item.OfLocalShellCallOutput != nil:
			flushPending()
			o := item.OfLocalShellCallOutput
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: o.ID,
				Content:    localShellOutputText(o),
			})
		case item.OfApplyPatchCall != nil:
			call := item.OfApplyPatchCall
			freeform["apply_patch"] = struct{}{}
			patch, err := applyPatchText(call)
			if err != nil {
				slog.Warn("chatconvert: apply_patch 历史无法拼 V4A，跳过",
					"call_id", call.CallID, "error", err.Error())
				continue
			}
			appendToolCall(call.CallID, "apply_patch", freeformArgsJSON(patch))
		case item.OfApplyPatchCallOutput != nil:
			flushPending()
			o := item.OfApplyPatchCallOutput
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: o.CallID,
				Content:    applyPatchOutputText(o),
			})
		case item.OfToolSearchCall != nil:
			call := item.OfToolSearchCall
			callID := call.CallID.Value
			if callID == "" {
				callID = call.ID.Value
			}
			appendToolCall(callID, "tool_search", toolSearchArgsJSON(call.Arguments))
		case item.OfToolSearchOutput != nil:
			flushPending()
			appendToolSearchOutput(&out, item.OfToolSearchOutput, freeform, dynamicTools)
		case item.OfWebSearchCall != nil:
			call := item.OfWebSearchCall
			args, result := webSearchHistoryArgs(call)
			id := call.ID
			if id == "" {
				id = "ws_hist"
			}
			appendToolCall(id, chatNameWebSearch, args)
			flushPending()
			out = append(out, ChatMessage{Role: "tool", ToolCallID: id, Content: result})
		case item.OfCodeInterpreterCall != nil:
			call := item.OfCodeInterpreterCall
			args, result := codeInterpreterHistory(call)
			id := call.ID
			if id == "" {
				id = "ci_hist"
			}
			appendToolCall(id, chatNameCodeInterpreter, args)
			flushPending()
			out = append(out, ChatMessage{Role: "tool", ToolCallID: id, Content: result})
		case item.OfMcpCall != nil:
			call := item.OfMcpCall
			if call.ID == "" {
				slog.Debug("chatconvert: 跳过无 id 的 mcp_call")
				continue
			}
			name, args, result := mcpHistoryArgs(call)
			appendToolCall(call.ID, name, args)
			flushPending()
			out = append(out, ChatMessage{Role: "tool", ToolCallID: call.ID, Content: result})
		case item.OfMcpListTools != nil:
			list := item.OfMcpListTools
			names := make([]string, 0, len(list.Tools))
			for _, tl := range list.Tools {
				if tl.Name != "" {
					names = append(names, tl.Name)
					*dynamicTools = append(*dynamicTools, mcpToolDecl(list.ServerLabel, tl.Name))
				}
			}
			flushPending()
			out = append(out, ChatMessage{
				Role:    "system",
				Content: fmt.Sprintf("[mcp_list_tools server=%s tools=%s]", list.ServerLabel, strings.Join(names, ",")),
			})
		case item.OfMcpApprovalRequest != nil, item.OfMcpApprovalResponse != nil:
			slog.Warn("chatconvert: 丢弃 MCP 审批历史（Chat 无审批协议）",
				"type", itemType(item), "impact", "审批上下文不会发给 Chat 上游")
		case item.OfFileSearchCall != nil, item.OfComputerCall != nil,
			item.OfComputerCallOutput != nil, item.OfImageGenerationCall != nil,
			item.OfProgram != nil, item.OfProgramOutput != nil,
			item.OfItemReference != nil, item.OfAdditionalTools != nil:
			slog.Warn("chatconvert: 跳过无 Chat 等价的重要历史 item",
				"type", itemType(item), "impact", "对应上下文不会发给 Chat 上游")
		case item.OfCompaction != nil:
			flushPending()
			out = append(out, ChatMessage{
				Role:    "system",
				Content: "<compaction>\n" + item.OfCompaction.EncryptedContent + "\n</compaction>",
			})
		case item.OfCompactionTrigger != nil:
			flushPending()
			out = append(out, ChatMessage{Role: "system", Content: "<compaction_trigger />"})
		case item.OfReasoning != nil:
			// 明文 reasoning 折入 assistant.reasoning_content（工具环同框）；encrypted 无 Chat 槽位丢弃。
			t := reasoningContentText(item.OfReasoning)
			if t == "" {
				slog.Debug("chatconvert: 跳过空 reasoning（无 summary/content 文本）")
				continue
			}
			if pending != nil {
				if pending.ReasoningContent == "" {
					pending.ReasoningContent = t
				} else {
					pending.ReasoningContent += "\n" + t
				}
			} else if pendingReasoning == "" {
				pendingReasoning = t
			} else {
				pendingReasoning += "\n" + t
			}
		default:
			typ := itemType(item)
			if isImportantHistoryDrop(typ) {
				slog.Warn("chatconvert: 跳过无 Chat 等价的重要历史 item",
					"type", typ, "impact", "对应上下文不会发给 Chat 上游")
			} else {
				slog.Debug("chatconvert: 跳过无 Chat 等价的 input item", "type", typ)
			}
		}
	}
	flushPending()
	if pendingReasoning != "" {
		slog.Debug("chatconvert: 孤立 reasoning 无后续 assistant，丢弃",
			"chars", len(pendingReasoning))
	}
	return out, nil
}

func isImportantHistoryDrop(typ string) bool {
	switch typ {
	case "mcp_call", "web_search_call", "code_interpreter_call",
		"computer_call", "computer_call_output",
		"file_search_call", "image_generation_call",
		"program", "program_output", "item_reference", "additional_tools":
		return true
	default:
		return false
	}
}

const placeholderToolResultContent = "[no tool output available — this call's result was missing from the request history]"

// ensureChatToolPaired 为缺少 role=tool 回包的 tool_call 补占位 tool 消息。
func ensureChatToolPaired(out *ChatRequest) {
	if out == nil {
		return
	}
	resolved := map[string]struct{}{}
	for _, m := range out.Messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			resolved[m.ToolCallID] = struct{}{}
		}
	}
	var missing []string
	seen := map[string]struct{}{}
	for _, m := range out.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			id := tc.ID
			if id == "" {
				continue
			}
			if _, ok := resolved[id]; ok {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return
	}
	for _, id := range missing {
		out.Messages = append(out.Messages, ChatMessage{
			Role:       "tool",
			ToolCallID: id,
			Content:    placeholderToolResultContent,
		})
	}
	slog.Warn("chatconvert: 补占位 tool 消息（历史缺少 tool output）",
		"placeholder_count", len(missing),
		"impact", "避免 Chat 上游因 tool_call 无结果而 400")
}

func convertEasyMessage(m *oairesponses.EasyInputMessageParam) (ChatMessage, bool) {
	// developer → system：官方 Chat 有 developer，但多数兼容层（如 OpenCode）会 400。
	role := chatRole(string(m.Role))
	text := easyMessageText(m)
	if text == "" && role != "assistant" {
		return ChatMessage{}, false
	}
	return ChatMessage{Role: role, Content: text}, true
}

func chatRole(role string) string {
	if role == "developer" {
		return "system"
	}
	return role
}

func convertInputMessage(m *oairesponses.ResponseInputItemMessageParam) (ChatMessage, bool) {
	if m == nil {
		return ChatMessage{}, false
	}
	role := chatRole(m.Role)
	if role == "" {
		role = "user"
	}
	var b strings.Builder
	for _, part := range m.Content {
		switch {
		case part.OfInputText != nil:
			b.WriteString(part.OfInputText.Text)
		case part.OfInputImage != nil:
			slog.Debug("chatconvert: 跳过 input_message 中的 input_image（Chat 收口仅文本）")
		case part.OfInputFile != nil:
			slog.Debug("chatconvert: 跳过 input_message 中的 input_file（Chat 收口仅文本）")
		}
	}
	text := b.String()
	if text == "" {
		return ChatMessage{}, false
	}
	return ChatMessage{Role: role, Content: text}, true
}

// reasoningContentText 从 Responses reasoning item 提取明文推理文本。
// 优先 summary（网关出站约定），空则回退 content[].reasoning_text；忽略 encrypted_content。
func reasoningContentText(r *oairesponses.ResponseReasoningItemParam) string {
	if r == nil {
		return ""
	}
	var parts []string
	for _, s := range r.Summary {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	for _, c := range r.Content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func convertOutputMessage(m *oairesponses.ResponseOutputMessageParam) (ChatMessage, bool) {
	if m == nil {
		return ChatMessage{}, false
	}
	var b strings.Builder
	for _, cp := range m.Content {
		if cp.OfOutputText != nil {
			b.WriteString(cp.OfOutputText.Text)
		} else if cp.OfRefusal != nil {
			if cp.OfRefusal.Refusal != "" {
				b.WriteString(cp.OfRefusal.Refusal)
			}
		}
	}
	return ChatMessage{Role: "assistant", Content: b.String()}, true
}

func easyMessageText(m *oairesponses.EasyInputMessageParam) string {
	if m.Content.OfString.Valid() {
		return m.Content.OfString.Value
	}
	var b strings.Builder
	for _, part := range m.Content.OfInputItemContentList {
		switch {
		case part.OfInputText != nil:
			b.WriteString(part.OfInputText.Text)
		case part.OfInputImage != nil:
			slog.Debug("chatconvert: 跳过 message 中的 input_image（Chat 收口仅文本）")
		case part.OfInputFile != nil:
			slog.Debug("chatconvert: 跳过 message 中的 input_file（Chat 收口仅文本）")
		}
	}
	return b.String()
}

func functionCallOutputText(fco *oairesponses.ResponseInputItemFunctionCallOutputParam) string {
	if fco.Output.OfString.Valid() {
		return fco.Output.OfString.Value
	}
	var b strings.Builder
	for _, it := range fco.Output.OfResponseFunctionCallOutputItemArray {
		if it.OfInputText != nil {
			b.WriteString(it.OfInputText.Text)
		} else if it.OfInputImage != nil || it.OfInputFile != nil {
			slog.Debug("chatconvert: function_call_output 非文本 part 丢弃")
		}
	}
	return b.String()
}

func customToolOutputText(c *oairesponses.ResponseCustomToolCallOutputParam) string {
	if c.Output.OfString.Valid() {
		return c.Output.OfString.Value
	}
	var b strings.Builder
	for _, it := range c.Output.OfOutputContentList {
		if it.OfInputText != nil {
			b.WriteString(it.OfInputText.Text)
		}
	}
	return b.String()
}

func shellCallOutputText(out *oairesponses.ResponseInputItemShellCallOutputParam) string {
	var parts []string
	if out.Status != "" {
		parts = append(parts, "[status="+out.Status+"]")
	}
	if out.MaxOutputLength.Valid() {
		parts = append(parts, fmt.Sprintf("[max_output_length=%d]", out.MaxOutputLength.Value))
	}
	for _, part := range out.Output {
		if part.Stdout != "" {
			parts = append(parts, part.Stdout)
		}
		if part.Stderr != "" {
			parts = append(parts, part.Stderr)
		}
		if part.Outcome.OfExit != nil {
			parts = append(parts, fmt.Sprintf("[exit_code=%d]", part.Outcome.OfExit.ExitCode))
		} else if part.Outcome.OfTimeout != nil {
			parts = append(parts, "[timeout]")
		}
	}
	return strings.Join(parts, "\n")
}

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

func applyPatchText(call *oairesponses.ResponseInputItemApplyPatchCallParam) (string, error) {
	var patch string
	switch {
	case call.Operation.OfCreateFile != nil:
		patch = toolcatalog.FormatApplyPatchV4A("create_file", call.Operation.OfCreateFile.Path, call.Operation.OfCreateFile.Diff)
	case call.Operation.OfUpdateFile != nil:
		patch = toolcatalog.FormatApplyPatchV4A("update_file", call.Operation.OfUpdateFile.Path, call.Operation.OfUpdateFile.Diff)
	case call.Operation.OfDeleteFile != nil:
		patch = toolcatalog.FormatApplyPatchV4A("delete_file", call.Operation.OfDeleteFile.Path, "")
	default:
		return "", fmt.Errorf("invalid operation")
	}
	if patch == "" {
		return "", fmt.Errorf("empty patch")
	}
	return patch, nil
}

func freeformArgsJSON(input string) string {
	b, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		return `{"input":""}`
	}
	return string(b)
}

// chatFunctionArguments 保证 Chat tool_calls[].function.arguments 是合法 JSON。
//   - 空 → {}
//   - 已是合法 JSON → 原样
//   - 否则 → {"raw":"<原串>"}（避免 MiMo prefill "unexpected end of data"）
func chatFunctionArguments(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "{}"
	}
	if json.Valid([]byte(s)) {
		return s
	}
	b, err := json.Marshal(map[string]string{"raw": s})
	if err != nil {
		return "{}"
	}
	return string(b)
}

func toolSearchArgsJSON(args any) string {
	switch v := args.(type) {
	case string:
		if v == "" {
			return "{}"
		}
		if json.Valid([]byte(v)) {
			return v
		}
		b, _ := json.Marshal(v)
		return string(b)
	case nil:
		return "{}"
	case json.RawMessage:
		if len(v) == 0 {
			return "{}"
		}
		return string(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
}

func appendToolSearchOutput(out *[]ChatMessage, output *oairesponses.ResponseToolSearchOutputItemParam, freeform map[string]struct{}, dynamicTools *[]ChatTool) {
	names := make([]string, 0, len(output.Tools))
	for _, t := range output.Tools {
		for _, ct := range toolUnionToChat(t, freeform) {
			*dynamicTools = append(*dynamicTools, ct)
			names = append(names, ct.Function.Name)
		}
	}
	body := "tool_search_output: " + strings.Join(names, ",")
	if output.CallID.Valid() && output.CallID.Value != "" {
		*out = append(*out, ChatMessage{
			Role:       "tool",
			ToolCallID: output.CallID.Value,
			Content:    body,
		})
	}
}

func convertTools(tools []oairesponses.ToolUnionParam, freeform map[string]struct{}) []ChatTool {
	var out []ChatTool
	seen := map[string]struct{}{}
	for _, t := range tools {
		for _, ct := range toolUnionToChat(t, freeform) {
			if _, ok := seen[ct.Function.Name]; ok {
				continue
			}
			seen[ct.Function.Name] = struct{}{}
			out = append(out, ct)
		}
	}
	return out
}

func toolUnionToChat(t oairesponses.ToolUnionParam, freeform map[string]struct{}) []ChatTool {
	switch {
	case t.OfFunction != nil:
		f := t.OfFunction
		fn := ChatFunction{
			Name:        f.Name,
			Description: optString(f.Description),
			Parameters:  f.Parameters,
		}
		if f.Strict.Valid() {
			fn.Strict = ptr(f.Strict.Value)
		}
		return []ChatTool{{Type: "function", Function: fn}}
	case t.OfCustom != nil:
		c := t.OfCustom
		freeform[c.Name] = struct{}{}
		return []ChatTool{{
			Type: "function",
			Function: ChatFunction{
				Name:        c.Name,
				Description: optString(c.Description),
				Parameters:  toolcatalog.FreeformInputSchema(),
			},
		}}
	case t.OfShell != nil, t.OfLocalShell != nil:
		freeform["shell"] = struct{}{}
		return []ChatTool{{
			Type: "function",
			Function: ChatFunction{
				Name:       "shell",
				Parameters: toolcatalog.FreeformInputSchema(),
			},
		}}
	case t.OfApplyPatch != nil:
		freeform["apply_patch"] = struct{}{}
		return []ChatTool{{
			Type: "function",
			Function: ChatFunction{
				Name:        "apply_patch",
				Description: toolcatalog.ApplyPatchDescription(),
				Parameters:  toolcatalog.FreeformInputSchema(),
			},
		}}
	case t.OfToolSearch != nil:
		s := t.OfToolSearch
		return []ChatTool{{
			Type: "function",
			Function: ChatFunction{
				Name:        "tool_search",
				Description: optString(s.Description),
				Parameters:  s.Parameters,
			},
		}}
	case t.OfNamespace != nil:
		ns := t.OfNamespace
		var out []ChatTool
		for _, nested := range ns.Tools {
			switch {
			case nested.OfFunction != nil:
				nestedFn := nested.OfFunction
				cf := ChatFunction{
					Name:        toolcatalog.ToolName(ns.Name, nestedFn.Name),
					Description: optString(nestedFn.Description),
					Parameters:  nestedFn.Parameters,
				}
				if nestedFn.Strict.Valid() {
					cf.Strict = ptr(nestedFn.Strict.Value)
				}
				out = append(out, ChatTool{Type: "function", Function: cf})
			case nested.OfCustom != nil:
				c := nested.OfCustom
				name := toolcatalog.ToolName(ns.Name, c.Name)
				freeform[name] = struct{}{}
				out = append(out, ChatTool{
					Type: "function",
					Function: ChatFunction{
						Name:        name,
						Description: optString(c.Description),
						Parameters:  toolcatalog.FreeformInputSchema(),
					},
				})
			default:
				slog.Debug("chatconvert: 跳过 namespace 内不支持的子工具")
			}
		}
		return out
	case t.OfWebSearch != nil, t.OfWebSearchPreview != nil:
		return []ChatTool{webSearchToolDecl()}
	case t.OfCodeInterpreter != nil:
		if t.OfCodeInterpreter.Container.OfString.Valid() && t.OfCodeInterpreter.Container.OfString.Value != "" {
			slog.Warn("chatconvert: 丢弃 code_interpreter.container（Chat 无 container）",
				"container_id", t.OfCodeInterpreter.Container.OfString.Value)
		}
		return []ChatTool{codeInterpreterToolDecl()}
	case t.OfMcp != nil:
		return mcpDeclsFromTool(t.OfMcp)
	default:
		slog.Debug("chatconvert: 跳过无 Chat 等价的 tool 声明", "type", openaiToolType(t))
		return nil
	}
}

func convertChatModeration(req *oairesponses.ResponseNewParams) *ChatModeration {
	if req == nil {
		return nil
	}
	has := req.Moderation.Model != "" ||
		req.Moderation.Policy.Input.Mode != "" ||
		req.Moderation.Policy.Output.Mode != ""
	if !has {
		return nil
	}
	m := &ChatModeration{Model: req.Moderation.Model}
	var policy ChatModerationPolicy
	if req.Moderation.Policy.Input.Mode != "" {
		policy.Input = &ChatModerationMode{Mode: req.Moderation.Policy.Input.Mode}
	}
	if req.Moderation.Policy.Output.Mode != "" {
		policy.Output = &ChatModerationMode{Mode: req.Moderation.Policy.Output.Mode}
	}
	if policy.Input != nil || policy.Output != nil {
		m.Policy = &policy
	}
	return m
}

func convertResponseFormat(req *oairesponses.ResponseNewParams) any {
	if req == nil {
		return nil
	}
	switch {
	case req.Text.Format.OfJSONSchema != nil:
		f := req.Text.Format.OfJSONSchema
		js := map[string]any{
			"name":   f.Name,
			"schema": f.Schema,
		}
		if f.Description.Valid() && f.Description.Value != "" {
			js["description"] = f.Description.Value
		}
		if f.Strict.Valid() {
			js["strict"] = f.Strict.Value
		}
		return map[string]any{
			"type":        "json_schema",
			"json_schema": js,
		}
	case req.Text.Format.OfJSONObject != nil:
		return map[string]any{"type": "json_object"}
	case req.Text.Format.OfText != nil:
		return map[string]any{"type": "text"}
	default:
		return nil
	}
}

func applyChatAllowedTools(out *ChatRequest, declared []oairesponses.ToolUnionParam, allowed *oairesponses.ToolChoiceAllowedParam) error {
	if out == nil || allowed == nil {
		return nil
	}
	allowedNames, err := chatAllowedToolNames(declared, allowed)
	if err != nil {
		return err
	}
	var filtered []ChatTool
	for _, t := range out.Tools {
		if allowedNames[t.Function.Name] {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		return fmt.Errorf("chatconvert: tool_choice allowed_tools has no supported tools")
	}
	out.Tools = filtered
	if out.FreeformNames != nil {
		for name := range out.FreeformNames {
			if !allowedNames[name] {
				delete(out.FreeformNames, name)
			}
		}
	}
	switch allowed.Mode {
	case oairesponses.ToolChoiceAllowedModeRequired:
		out.ToolChoice = "required"
	case oairesponses.ToolChoiceAllowedModeAuto, "":
		out.ToolChoice = "auto"
	default:
		return fmt.Errorf("chatconvert: tool_choice allowed_tools mode %q is unsupported", allowed.Mode)
	}
	return nil
}

func chatAllowedToolNames(declared []oairesponses.ToolUnionParam, allowed *oairesponses.ToolChoiceAllowedParam) (map[string]bool, error) {
	declaredIDs := make([]toolcatalog.Identity, 0, len(declared))
	for _, tool := range declared {
		ids, err := toolcatalog.Inspect(tool)
		if err != nil {
			continue
		}
		declaredIDs = append(declaredIDs, ids...)
	}
	allowedNames := make(map[string]bool, len(allowed.Tools))
	for _, tool := range allowed.Tools {
		ids, err := toolcatalog.InspectAllowed(tool)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			matched := false
			for _, d := range declaredIDs {
				if d.Equal(id) {
					matched = true
					break
				}
				if (id.OpenAIType == "local_shell" || id.OpenAIType == "shell") &&
					(d.OpenAIType == "local_shell" || d.OpenAIType == "shell") &&
					id.Name == d.Name {
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("chatconvert: tool_choice allowed_tools entry %s is not declared", id)
			}
			allowedNames[id.ConvertedName()] = true
		}
	}
	return allowedNames, nil
}

func convertToolChoice(tc oairesponses.ResponseNewParamsToolChoiceUnion) any {
	switch {
	case tc.OfToolChoiceMode.Valid():
		return string(tc.OfToolChoiceMode.Value)
	case tc.OfFunctionTool != nil:
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.OfFunctionTool.Name},
		}
	case tc.OfCustomTool != nil:
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.OfCustomTool.Name},
		}
	case tc.OfSpecificShellToolChoice != nil:
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": "shell"},
		}
	case tc.OfSpecificApplyPatchToolChoice != nil:
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": "apply_patch"},
		}
	case tc.OfAllowedTools != nil:
		mode := string(tc.OfAllowedTools.Mode)
		if mode == "required" {
			return "required"
		}
		return "auto"
	case tc.OfHostedTool != nil, tc.OfMcpTool != nil,
		tc.OfResponseNewsToolChoiceSpecificProgrammaticToolCallingParam != nil:
		return nil
	default:
		return nil
	}
}

func openaiToolType(t oairesponses.ToolUnionParam) string {
	switch {
	case t.OfFunction != nil:
		return "function"
	case t.OfCustom != nil:
		return "custom"
	case t.OfShell != nil:
		return "shell"
	case t.OfLocalShell != nil:
		return "local_shell"
	case t.OfApplyPatch != nil:
		return "apply_patch"
	case t.OfToolSearch != nil:
		return "tool_search"
	case t.OfMcp != nil:
		return "mcp"
	case t.OfWebSearch != nil:
		return "web_search"
	case t.OfCodeInterpreter != nil:
		return "code_interpreter"
	default:
		return "unknown"
	}
}

func optString(v oparam.Opt[string]) string {
	if v.Valid() {
		return v.Value
	}
	return ""
}

func itemType(item *oairesponses.ResponseInputItemUnionParam) string {
	switch {
	case item.OfMessage != nil:
		return "message"
	case item.OfInputMessage != nil:
		return "input_message"
	case item.OfOutputMessage != nil:
		return "output_message"
	case item.OfFunctionCall != nil:
		return "function_call"
	case item.OfFunctionCallOutput != nil:
		return "function_call_output"
	case item.OfCustomToolCall != nil:
		return "custom_tool_call"
	case item.OfCustomToolCallOutput != nil:
		return "custom_tool_call_output"
	case item.OfShellCall != nil:
		return "shell_call"
	case item.OfShellCallOutput != nil:
		return "shell_call_output"
	case item.OfLocalShellCall != nil:
		return "local_shell_call"
	case item.OfLocalShellCallOutput != nil:
		return "local_shell_call_output"
	case item.OfApplyPatchCall != nil:
		return "apply_patch_call"
	case item.OfApplyPatchCallOutput != nil:
		return "apply_patch_call_output"
	case item.OfToolSearchCall != nil:
		return "tool_search_call"
	case item.OfToolSearchOutput != nil:
		return "tool_search_output"
	case item.OfReasoning != nil:
		return "reasoning"
	case item.OfMcpCall != nil:
		return "mcp_call"
	case item.OfMcpListTools != nil:
		return "mcp_list_tools"
	case item.OfMcpApprovalRequest != nil:
		return "mcp_approval_request"
	case item.OfMcpApprovalResponse != nil:
		return "mcp_approval_response"
	case item.OfWebSearchCall != nil:
		return "web_search_call"
	case item.OfCodeInterpreterCall != nil:
		return "code_interpreter_call"
	case item.OfComputerCall != nil:
		return "computer_call"
	case item.OfComputerCallOutput != nil:
		return "computer_call_output"
	case item.OfFileSearchCall != nil:
		return "file_search_call"
	case item.OfImageGenerationCall != nil:
		return "image_generation_call"
	case item.OfProgram != nil:
		return "program"
	case item.OfProgramOutput != nil:
		return "program_output"
	case item.OfItemReference != nil:
		return "item_reference"
	case item.OfAdditionalTools != nil:
		return "additional_tools"
	case item.OfCompaction != nil:
		return "compaction"
	case item.OfCompactionTrigger != nil:
		return "compaction_trigger"
	default:
		if ptr := item.GetType(); ptr != nil && *ptr != "" {
			return *ptr
		}
		return "unknown"
	}
}
