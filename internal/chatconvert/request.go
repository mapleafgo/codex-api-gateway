// Package chatconvert 将 OpenAI Responses 请求转为 Chat Completions 请求（仅流式）。
package chatconvert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
	openai "github.com/openai/openai-go/v3"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// ChatRequest 是 Chat Completions 流式请求的最小结构。
// FreeformNames 不进 wire，仅供 chatstreamconv 识别 freeform custom 工具回程形态；
// shell/local_shell/apply_patch 由 ChatName* 常量专项识别。
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []ChatTool    `json:"tools,omitempty"`
	// sendEmptyTools 置位后 wire 输出 tools: []：Anthropic 代理型 Chat 上游
	// 要求消息含工具历史时 tools 字段必须存在（可为空数组）。
	sendEmptyTools      bool
	ToolChoice          any                 `json:"tool_choice,omitempty"`
	Temperature         *float64            `json:"temperature,omitempty"`
	TopP                *float64            `json:"top_p,omitempty"`
	MaxTokens           *int                `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                `json:"max_completion_tokens,omitempty"`
	ParallelToolCalls   *bool               `json:"parallel_tool_calls,omitempty"`
	ResponseFormat      any                 `json:"response_format,omitempty"`
	ServiceTier         *string             `json:"service_tier,omitempty"`
	Metadata            map[string]string   `json:"metadata,omitempty"`
	Store               *bool               `json:"store,omitempty"`
	Moderation          *ChatModeration     `json:"moderation,omitempty"`
	ReasoningEffort     *string             `json:"reasoning_effort,omitempty"`
	Logprobs            *bool               `json:"logprobs,omitempty"`
	TopLogprobs         *int                `json:"top_logprobs,omitempty"`
	Stream              bool                `json:"stream"`
	StreamOptions       *StreamOptions      `json:"stream_options,omitempty"`
	FreeformNames       map[string]struct{} `json:"-"`
	// DeclaredNames 是请求声明的扁平工具名 → 身份映射，仅供 chatstreamconv
	// 回程还原 namespace/name，不进 wire。
	DeclaredNames map[string]toolcatalog.Identity `json:"-"`
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

// ChatContentPart 是 user 消息的多模态内容项（text | image_url）。
type ChatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ChatImageURL `json:"image_url,omitempty"`
}

// ChatImageURL 承载图片地址（URL 或 data URL），对齐 opencode lowerMedia 的形状透传。
type ChatImageURL struct {
	URL string `json:"url"`
}

// MarshalJSON 按 opencode wire 规则输出：
// assistant 无文本显式 content:null，tool 空输出显式 content:""，
// 其余角色沿用 omitempty（避免 user/system 出现多余字段）。
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	switch m.Role {
	case "assistant":
		return json.Marshal(struct {
			Role             string         `json:"role"`
			Content          any            `json:"content"`
			ReasoningContent string         `json:"reasoning_content,omitempty"`
			ToolCallID       string         `json:"tool_call_id,omitempty"`
			ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
		}{m.Role, m.Content, m.ReasoningContent, m.ToolCallID, m.ToolCalls})
	case "tool":
		content := m.Content
		if content == nil {
			content = ""
		}
		return json.Marshal(struct {
			Role             string         `json:"role"`
			Content          any            `json:"content"`
			ReasoningContent string         `json:"reasoning_content,omitempty"`
			ToolCallID       string         `json:"tool_call_id,omitempty"`
			ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
		}{m.Role, content, m.ReasoningContent, m.ToolCallID, m.ToolCalls})
	default:
		return json.Marshal(struct {
			Role             string         `json:"role"`
			Content          any            `json:"content,omitempty"`
			ReasoningContent string         `json:"reasoning_content,omitempty"`
			ToolCallID       string         `json:"tool_call_id,omitempty"`
			ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
		}{m.Role, m.Content, m.ReasoningContent, m.ToolCallID, m.ToolCalls})
	}
}

// ChatTool 是 function 或 custom 工具声明；无 grammar 的 custom 仍降级 function。
type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatFunction `json:"-"`
}

// ChatFunction 是 function 定义。
type ChatFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

// ChatCustomGrammar 对齐 pi / Chat SDK 的 custom grammar（lark | regex）。
type ChatCustomGrammar struct {
	Syntax     string `json:"syntax"`
	Definition string `json:"definition"`
}

// MarshalJSON 输出 function wire。Chat 上游无 custom 槽位，统一 function 形态。
func (t ChatTool) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     string       `json:"type"`
		Function ChatFunction `json:"function"`
	}{t.Type, t.Function})
}

// ChatToolCall 是 assistant 侧 tool_calls 项。
type ChatToolCall struct {
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ChatToolCallFunc `json:"-"`
}

// MarshalJSON 输出 function wire。历史 custom_tool_call 统一 function 降级。
func (t ChatToolCall) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID       string           `json:"id,omitempty"`
		Type     string           `json:"type,omitempty"`
		Function ChatToolCallFunc `json:"function"`
	}{t.ID, t.Type, t.Function})
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
		// max_tokens + max_completion_tokens 双写：兼容旧上游与新模型。
		out.MaxTokens = ptr(n)
		out.MaxCompletionTokens = ptr(n)
	}
	if req.ParallelToolCalls.Valid() {
		out.ParallelToolCalls = ptr(req.ParallelToolCalls.Value)
	}
	if req.PromptCacheKey.Valid() && req.PromptCacheKey.Value != "" {
		// Chat Completions 协议无顶层 prompt_cache_key 槽位（OpenAI Chat 标准
		// 未定义该字段，仅供 Responses/Codex 扩展）。透传会导致严格上游 400
		// （如 0v0.info 报「未知请求字段：prompt_cache_key」），故丢弃。
		slog.Debug("chatconvert: 丢弃 prompt_cache_key（Chat 无顶层等价字段）",
			"prompt_cache_key", req.PromptCacheKey.Value)
	}
	if req.PromptCacheOptions.Mode != "" || req.PromptCacheOptions.Ttl != "" {
		// Chat 请求体无 prompt_cache_options 槽位（仅 content part 的
		// prompt_cache_breakpoint），不透传。
		slog.Debug("chatconvert: 丢弃 prompt_cache_options（Chat 无顶层等价字段）",
			"mode", req.PromptCacheOptions.Mode, "ttl", req.PromptCacheOptions.Ttl)
	}
	// text.format（json_schema/json_object）不支持，静默忽略，不写 response_format。
	if v := string(req.Text.Verbosity); v != "" {
		// Chat 无顶层 verbosity 槽位（Responses 专有），丢弃避免未知字段 400。
		slog.Debug("chatconvert: 丢弃 text.verbosity（Chat 无等价字段）", "verbosity", v)
	}
	if st := string(req.ServiceTier); st != "" {
		out.ServiceTier = ptr(st)
	}
	if req.SafetyIdentifier.Valid() && req.SafetyIdentifier.Value != "" {
		// Chat 无顶层 safety_identifier 槽位（Responses 专有），丢弃避免 400。
		slog.Debug("chatconvert: 丢弃 safety_identifier（Chat 无等价字段）",
			"safety_identifier", req.SafetyIdentifier.Value)
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
		// reasoning_effort 任意值原样透传，不替上游校验/拒绝。
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
		seen[chatToolName(t)] = struct{}{}
	}
	for _, t := range dynamicTools {
		if _, ok := seen[chatToolName(t)]; ok {
			continue
		}
		seen[chatToolName(t)] = struct{}{}
		out.Tools = append(out.Tools, t)
	}
	if req.ToolChoice.OfAllowedTools != nil {
		if err := applyChatAllowedTools(out, req.Tools, req.ToolChoice.OfAllowedTools); err != nil {
			return nil, err
		}
	} else if tc := convertToolChoice(req.ToolChoice); tc != nil {
		out.ToolChoice = tc
	}
	out.DeclaredNames = chatDeclaredToolNames(req)
	if len(out.Tools) == 0 && hasChatToolHistory(out.Messages) {
		out.sendEmptyTools = true
	}
	return out, nil
}

// chatDeclaredToolNames 收集请求声明（含历史 tool_search_output 动态工具）的
// 扁平工具名 → 身份映射，供回程按声明还原 namespace/name。
func chatDeclaredToolNames(req *oairesponses.ResponseNewParams) map[string]toolcatalog.Identity {
	out := toolcatalog.DeclaredIdentities(req.Tools)
	for i := range req.Input.OfInputItemList {
		item := req.Input.OfInputItemList[i]
		if item.OfToolSearchOutput != nil {
			for name, id := range toolcatalog.DeclaredIdentities(item.OfToolSearchOutput.Tools) {
				out[name] = id
			}
		}
	}
	return out
}

// Marshal 将 ChatRequest 编成可 POST 的 JSON（不含 FreeformNames）。
func Marshal(req *ChatRequest) ([]byte, error) {
	return json.Marshal(req)
}

// MarshalJSON 保留 tools 的 nil/空数组差异：无工具且无工具历史时省略，
// 有工具历史但无活动工具时显式输出 tools: []（对齐 pi 的 Anthropic 代理兼容）。
func (r *ChatRequest) MarshalJSON() ([]byte, error) {
	type alias ChatRequest
	a := (*alias)(r)
	var tools *[]ChatTool
	switch {
	case r.sendEmptyTools:
		tools = &[]ChatTool{}
	case len(r.Tools) > 0:
		tools = &r.Tools
	}
	return json.Marshal(struct {
		*alias
		Tools *[]ChatTool `json:"tools,omitempty"`
	}{a, tools})
}

// IsFreeformName 判断工具名是否应按 custom_tool_call 回程。
func (r *ChatRequest) IsFreeformName(name string) bool {
	if r == nil || r.FreeformNames == nil {
		return false
	}

	_, ok := r.FreeformNames[name]
	return ok
}

// hasChatToolHistory 判断 Chat 消息是否含历史工具环（assistant.tool_calls 或 role=tool）。
func hasChatToolHistory(msgs []ChatMessage) bool {
	for _, m := range msgs {
		if m.Role == "tool" {
			return true
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

func chatToolName(t ChatTool) string {
	return t.Function.Name
}

// chatCustomGrammar 从 Responses custom tool format 提取 Chat grammar；无 grammar 返回 nil。
func chatCustomGrammar(c *oairesponses.CustomToolParam) *ChatCustomGrammar {
	if c == nil || c.Format.OfGrammar == nil {
		return nil
	}
	g := c.Format.OfGrammar
	if g.Syntax == "" || g.Definition == "" {
		return nil
	}
	return &ChatCustomGrammar{Syntax: g.Syntax, Definition: g.Definition}
}

// customChatTool 统一把 custom 工具降级为 function（Chat 上游无 custom 槽位，
// opencode2api 会把 custom 转 function 且丢弃 grammar，历史回程还会把裸文本
// 当 arguments 导致上游 400）。function description 保持客户端原文，
// grammar 格式约束只写进 parameters.properties.s.description。
func customChatTool(name, description string, grammar *ChatCustomGrammar) ChatTool {
	inputDesc := ""
	if grammar != nil && grammar.Definition != "" {
		inputDesc = toolcatalog.FormatRequirementDescription(grammar.Definition)
	}
	return ChatTool{
		Type: "function",
		Function: ChatFunction{
			Name:        name,
			Description: description,
			Parameters:  toolcatalog.FreeformInputSchema(inputDesc),
		},
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
	// pendingImages 暂存工具结果中的图片，按 opencode 语义在下一个
	// user/assistant 消息前（或流尾）合并成独立 user 消息。
	var pendingImages []ChatContentPart
	// web_search 历史缺 id 时用带序号的 fallback，避免多轮碰撞。
	var wsHistSeq int
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
	flushImages := func() {
		if len(pendingImages) == 0 {
			return
		}
		out = append(out, ChatMessage{Role: "user", Content: pendingImages})
		pendingImages = nil
	}
	appendToolMessage := func(id, text string, images []ChatContentPart) {
		out = append(out, ChatMessage{Role: "tool", ToolCallID: id, Content: text})
		if len(images) > 0 {
			pendingImages = append(pendingImages, images...)
		}
	}
	// appendSystemUpdate 把时序 system 更新折成 <system-update> user 文本：
	// 有未 flush 的图片时合并进同一条 user，否则折入前一条 user（字符串或 parts）。
	appendSystemUpdate := func(text string) {
		wrapped := wrapSystemUpdate(text)
		if len(pendingImages) > 0 {
			parts := make([]ChatContentPart, 0, len(pendingImages)+1)
			parts = append(parts, pendingImages...)
			parts = append(parts, ChatContentPart{Type: "text", Text: wrapped})
			out = append(out, ChatMessage{Role: "user", Content: parts})
			pendingImages = nil
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == "user" {
			last := &out[n-1]
			switch c := last.Content.(type) {
			case string:
				if c == "" {
					last.Content = wrapped
				} else {
					last.Content = c + "\n" + wrapped
				}
			case []ChatContentPart:
				last.Content = append(c, ChatContentPart{Type: "text", Text: wrapped})
			}
			return
		}
		out = append(out, ChatMessage{Role: "user", Content: wrapped})
	}
	appendToolCall := func(id, name, args string) {
		// 上一条 assistant 文本（可能已带 reasoning_content）与本次 tool_calls 同轮：
		// 合并进它，避免拆成两条 assistant 导致严格上游（DeepSeek/OpenCode）要求
		// reasoning_content 与 tool_calls 同框时 400。
		if n := len(out); n > 0 && out[n-1].Role == "assistant" && len(out[n-1].ToolCallID) == 0 {
			p := &out[n-1]
			attachReasoning(p)
			p.ToolCalls = append(p.ToolCalls, ChatToolCall{
				ID:   id,
				Type: "function",
				Function: ChatToolCallFunc{
					Name: name,
					// Chat Completions 要求 arguments 是合法 JSON 字符串；上游
					// （如 MiMo prefill）会对内容再 parse，截断/非 JSON 会 400。
					Arguments: chatFunctionArguments(args),
				},
			})
			return
		}
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
			msg, ok, err := convertEasyMessage(item.OfMessage)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if msg.Role == "system" {
				if s, isStr := msg.Content.(string); isStr && s != "" {
					appendSystemUpdate(s)
				}
				continue
			}
			flushImages()
			if msg.Role == "assistant" {
				attachReasoning(&msg)
			}
			out = append(out, msg)
		case item.OfInputMessage != nil:
			flushPending()
			msg, ok, err := convertInputMessage(item.OfInputMessage)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if msg.Role == "system" {
				if s, isStr := msg.Content.(string); isStr && s != "" {
					appendSystemUpdate(s)
				}
				continue
			}
			flushImages()
			if msg.Role == "assistant" {
				attachReasoning(&msg)
			}
			out = append(out, msg)
		case item.OfOutputMessage != nil:
			flushPending()
			flushImages()
			if msg, ok := convertOutputMessage(item.OfOutputMessage); ok {
				attachReasoning(&msg)
				out = append(out, msg)
			}
		case item.OfFunctionCall != nil:
			fc := item.OfFunctionCall
			// 历史 function_call 携带 namespace：按声明扁平名回填，否则上游
			// Chat 声明的是 ns__tool / mcp__server__tool，名字对不上。
			appendToolCall(fc.CallID, toolcatalog.ToolName(fc.Namespace.Value, fc.Name), fc.Arguments)
		case item.OfFunctionCallOutput != nil:
			flushPending()
			fco := item.OfFunctionCallOutput
			if fco.CallID == "" {
				slog.Warn("chatconvert: 跳过无 call_id 的 function_call_output",
					"type", itemType(item), "impact", "该工具结果不会发给 Chat 上游")
				continue
			}
			text, images, err := functionCallOutputParts(fco)
			if err != nil {
				return nil, err
			}
			appendToolMessage(fco.CallID, text, images)
		case item.OfCustomToolCall != nil:
			c := item.OfCustomToolCall
			name := toolcatalog.ToolName(c.Namespace.Value, c.Name)
			freeform[name] = struct{}{}
			// Chat 上游无 custom 槽位：所有 custom 工具历史统一降级为
			// function + {"s":...}，避免 opencode2api 把裸文本当 arguments。
			appendToolCall(c.CallID, name, freeformArgsJSON(c.Input))
		case item.OfCustomToolCallOutput != nil:
			flushPending()
			c := item.OfCustomToolCallOutput
			if c.CallID == "" {
				slog.Warn("chatconvert: 跳过无 call_id 的 custom_tool_call_output",
					"type", itemType(item), "impact", "该工具结果不会发给 Chat 上游")
				continue
			}
			text, images, err := customToolOutputParts(c)
			if err != nil {
				return nil, err
			}
			appendToolMessage(c.CallID, text, images)
		case item.OfShellCall != nil:
			call := item.OfShellCall
			appendToolCall(call.CallID, toolcatalog.ChatNameShell, freeformArgsJSON(strings.Join(call.Action.Commands, "\n")))
		case item.OfShellCallOutput != nil:
			flushPending()
			o := item.OfShellCallOutput
			appendToolMessage(o.CallID, shellCallOutputText(o), nil)
		case item.OfLocalShellCall != nil:
			call := item.OfLocalShellCall
			id := call.CallID
			if id == "" {
				id = call.ID
			}
			appendToolCall(id, toolcatalog.ChatNameLocalShell, freeformArgsJSON(strings.Join(call.Action.Command, " ")))
		case item.OfLocalShellCallOutput != nil:
			flushPending()
			o := item.OfLocalShellCallOutput
			appendToolMessage(o.ID, localShellOutputText(o), nil)
		case item.OfApplyPatchCall != nil:
			call := item.OfApplyPatchCall
			args, err := applyPatchArgs(call)
			if err != nil {
				slog.Warn("chatconvert: apply_patch 历史无法拼 structured 参数，跳过",
					"call_id", call.CallID, "error", err.Error())
				continue
			}
			appendToolCall(call.CallID, toolcatalog.ChatNameApplyPatch, args)
		case item.OfApplyPatchCallOutput != nil:
			flushPending()
			o := item.OfApplyPatchCallOutput
			appendToolMessage(o.CallID, applyPatchOutputText(o), nil)
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
				wsHistSeq++
				id = fmt.Sprintf("ws_hist_%d", wsHistSeq)
			}
			appendToolCall(id, toolcatalog.ChatNameWebSearch, args)
			flushPending()
			appendToolMessage(id, result, nil)
		case item.OfCodeInterpreterCall != nil:
			// 不支持 code_interpreter，静默忽略
		case item.OfMcpCall != nil:
			call := item.OfMcpCall
			if call.ID == "" {
				slog.Debug("chatconvert: 跳过无 id 的 mcp_call")
				continue
			}
			name, args, result := mcpHistoryArgs(call)
			appendToolCall(call.ID, name, args)
			flushPending()
			appendToolMessage(call.ID, result, nil)
		case item.OfMcpListTools != nil:
			list := item.OfMcpListTools
			names := make([]string, 0, len(list.Tools))
			for _, tl := range list.Tools {
				if tl.Name != "" {
					names = append(names, tl.Name)
					*dynamicTools = append(*dynamicTools, mcpToolDecl(list.ServerLabel, tl.Name, ""))
				}
			}
			// mcp_list_tools 是历史工具列表结果，不转模型文本：opencode 无此类型，
			// Codex 不把 AdditionalTools 转消息（工具经请求 tools/ToolSpec 声明）。
			slog.Debug("chatconvert: 历史 mcp_list_tools 不转文本，仅注入工具声明",
				"server_label", list.ServerLabel, "tool_count", len(names))
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
			// compaction 密文只对生成它的服务端有意义；Codex 在非 OpenAI provider 下走
			// local 压缩，摘要以明文 user 消息回灌，不会携带该 item。无法解读则丢弃。
			slog.Warn("chatconvert: 丢弃历史 compaction（密文不可解读，非本网关可用的压缩产物）",
				"type", itemType(item), "impact", "压缩历史不会发给 Chat 上游；Codex local 压缩以明文摘要 user 消息回灌")
		case item.OfCompactionTrigger != nil:
			// 请求控制信号，不是模型输入；Codex 明确丢弃，不转发。
			slog.Debug("chatconvert: 丢弃 compaction_trigger（请求控制信号，非模型输入）")
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
			// 已知的重要历史类型全部被前面的显式分支接住（mcp_call/web_search_call
			// 等各有 WARN），走到这里的只有全部 Of* 为 nil 的未知类型——可控跳过。
			slog.Debug("chatconvert: 跳过无 Chat 等价的 input item", "type", itemType(item))
		}
	}
	flushPending()
	flushImages()
	if pendingReasoning != "" {
		// reasoning-only 历史也发 assistant（content:null + reasoning_content），对齐 opencode。
		out = append(out, ChatMessage{Role: "assistant", ReasoningContent: pendingReasoning})
	}
	return out, nil
}

const placeholderToolResultContent = "[no tool output available — this call's result was missing from the request history]"

// ensureChatToolPaired 为缺少 role=tool 回包的 tool_call 补占位 tool 消息。
// 同时把已有但位置不对的 tool 消息挪到对应 assistant 之后：
// 严格 Chat 上游（如 JD）要求 assistant(tool_calls) 后必须紧跟各 tool_call_id 的 tool 消息。
func ensureChatToolPaired(out *ChatRequest) {
	if out == nil || len(out.Messages) == 0 {
		return
	}

	// 索引 tool 消息（同 id 保留首次出现），并标记被 assistant.tool_calls 声明过的 id。
	toolByID := make(map[string]ChatMessage)
	claimed := make(map[string]struct{})
	for _, m := range out.Messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			if _, ok := toolByID[m.ToolCallID]; !ok {
				toolByID[m.ToolCallID] = m
			}
		}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					claimed[tc.ID] = struct{}{}
				}
			}
		}
	}

	// 快速路径：每条含 tool_calls 的 assistant 后已紧跟其完整 tool 回包。
	if chatToolPairingAlreadyValid(out.Messages, toolByID, claimed) {
		return
	}

	newMsgs := make([]ChatMessage, 0, len(out.Messages)+len(claimed))
	emitted := make(map[string]struct{}, len(claimed))
	missingCount := 0

	for _, m := range out.Messages {
		if m.Role == "tool" {
			// 被 assistant 声明的 tool 统一在 assistant 后重放；这里只保留无归属的孤儿 tool。
			if m.ToolCallID != "" {
				if _, ok := claimed[m.ToolCallID]; ok {
					continue
				}
			}
			newMsgs = append(newMsgs, m)
			continue
		}

		newMsgs = append(newMsgs, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}

		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				continue
			}
			if _, ok := emitted[tc.ID]; ok {
				continue
			}
			emitted[tc.ID] = struct{}{}
			if tool, ok := toolByID[tc.ID]; ok {
				newMsgs = append(newMsgs, tool)
			} else {
				newMsgs = append(newMsgs, ChatMessage{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    placeholderToolResultContent,
				})
				missingCount++
			}
		}
	}

	out.Messages = newMsgs
	if missingCount > 0 {
		slog.Warn("chatconvert: 补占位 tool 消息（历史缺少 tool output）",
			"placeholder_count", missingCount,
			"impact", "避免 Chat 上游因 tool_call 无结果而 400")
	} else {
		// 仅重排：已知协议规范化路径，不抬 Warn。
		slog.Debug("chatconvert: 重排 tool 消息至 assistant 紧邻位置",
			"impact", "满足严格 Chat 上游对 tool_calls 后紧跟 tool 的要求")
	}
}

// chatToolPairingAlreadyValid 检查 assistant(tool_calls) 后是否已紧跟全部对应 tool 消息。
func chatToolPairingAlreadyValid(msgs []ChatMessage, toolByID map[string]ChatMessage, claimed map[string]struct{}) bool {
	// 每个 claimed id 都必须有 tool 消息。
	for id := range claimed {
		if _, ok := toolByID[id]; !ok {
			return false
		}
	}
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		need := make(map[string]struct{})
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				need[tc.ID] = struct{}{}
			}
		}
		if len(need) == 0 {
			continue
		}
		j := i + 1
		for j < len(msgs) && msgs[j].Role == "tool" {
			id := msgs[j].ToolCallID
			if id == "" {
				return false
			}
			if _, ok := need[id]; !ok {
				// 紧邻的 tool 不属于本条 assistant → 顺序不合法。
				return false
			}
			delete(need, id)
			j++
		}
		if len(need) > 0 {
			return false
		}
		// 跳过已消费的 tool 段，外层 i++ 会再 +1，这里把 i 推到 tool 段末尾前一位。
		i = j - 1
	}
	return true
}

// convertEasyMessage 把 Responses 的 message item 转成 Chat 消息：
// system/developer 以 role=system 返回（由调用方折成 <system-update> user），
// user 可带 []ChatContentPart 多模态内容，assistant 保持纯文本。
func convertEasyMessage(m *oairesponses.EasyInputMessageParam) (ChatMessage, bool, error) {
	if m == nil {
		return ChatMessage{}, false, nil
	}
	role := string(m.Role)
	if role == "" {
		role = "user"
	}
	text, parts, err := easyMessageText(m)
	return convertMessageRole(role, text, parts, err)
}

func convertInputMessage(m *oairesponses.ResponseInputItemMessageParam) (ChatMessage, bool, error) {
	if m == nil {
		return ChatMessage{}, false, nil
	}
	role := m.Role
	if role == "" {
		role = "user"
	}
	text, parts, err := convertInputContent(m.Content)
	return convertMessageRole(role, text, parts, err)
}

// convertMessageRole 按角色组装消息；system/developer 只取文本（包装由调用方负责）。
func convertMessageRole(role string, text string, parts []ChatContentPart, err error) (ChatMessage, bool, error) {
	if err != nil {
		return ChatMessage{}, false, err
	}
	switch role {
	case "system", "developer":
		if text == "" {
			return ChatMessage{}, false, nil
		}
		return ChatMessage{Role: "system", Content: text}, true, nil
	case "user":
		msg, ok := buildUserMessage(text, parts)
		return msg, ok, nil
	case "assistant":
		if text == "" {
			return ChatMessage{}, false, nil
		}
		return ChatMessage{Role: "assistant", Content: text}, true, nil
	default:
		return ChatMessage{}, false, fmt.Errorf("chatconvert: 不支持的 message role %q", role)
	}
}

// buildUserMessage 纯文本消息用 string，含图片时用有序 []ChatContentPart（对齐 opencode）。
func buildUserMessage(text string, parts []ChatContentPart) (ChatMessage, bool) {
	if len(parts) > 0 {
		return ChatMessage{Role: "user", Content: parts}, true
	}
	if text == "" {
		return ChatMessage{}, false
	}
	return ChatMessage{Role: "user", Content: text}, true
}

// imagePart 构造 Chat image_url part。
func imagePart(url string) ChatContentPart {
	return ChatContentPart{Type: "image_url", ImageURL: &ChatImageURL{URL: url}}
}

// inputImagePart 仅接受 image_url（file_id 无 Chat 槽位，报协议不可映射错误）。
func inputImagePart(img *oairesponses.ResponseInputImageParam) (ChatContentPart, error) {
	if img == nil {
		return ChatContentPart{}, nil
	}
	url := ""
	if img.ImageURL.Valid() {
		url = img.ImageURL.Value
	}
	if url == "" && img.FileID.Valid() && img.FileID.Value != "" {
		return ChatContentPart{}, fmt.Errorf("chatconvert: input_image file_id 无法映射到 Chat（仅支持 image_url）")
	}
	if url == "" {
		return ChatContentPart{}, fmt.Errorf("chatconvert: input_image 缺少 image_url，无法映射到 Chat")
	}
	return imagePart(url), nil
}

// inputImageContentPart 与 inputImagePart 同规则，针对 function_call_output 的图片项。
func inputImageContentPart(img *oairesponses.ResponseInputImageContentParam) (ChatContentPart, error) {
	if img == nil {
		return ChatContentPart{}, nil
	}
	url := ""
	if img.ImageURL.Valid() {
		url = img.ImageURL.Value
	}
	if url == "" && img.FileID.Valid() && img.FileID.Value != "" {
		return ChatContentPart{}, fmt.Errorf("chatconvert: tool output image file_id 无法映射到 Chat（仅支持 image_url）")
	}
	if url == "" {
		return ChatContentPart{}, fmt.Errorf("chatconvert: tool output image 缺少 image_url，无法映射到 Chat")
	}
	return imagePart(url), nil
}

// convertInputContent 把 input content 列表转成文本与有序多模态 parts。
// input_file 在 Chat 无等价槽位，报错而非占位降级。
func convertInputContent(parts []oairesponses.ResponseInputContentUnionParam) (string, []ChatContentPart, error) {
	var b strings.Builder
	var out []ChatContentPart
	hasImage := false
	for _, part := range parts {
		switch {
		case part.OfInputText != nil:
			if part.OfInputText.Text != "" {
				b.WriteString(part.OfInputText.Text)
				out = append(out, ChatContentPart{Type: "text", Text: part.OfInputText.Text})
			}
		case part.OfInputImage != nil:
			hasImage = true
			p, err := inputImagePart(part.OfInputImage)
			if err != nil {
				return "", nil, err
			}
			out = append(out, p)
		case part.OfInputFile != nil:
			return "", nil, fmt.Errorf("chatconvert: input_file 无法映射到 Chat（仅支持 input_text/input_image）")
		}
	}
	if !hasImage {
		return b.String(), nil, nil
	}
	return b.String(), out, nil
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
	text := b.String()
	if text == "" {
		return ChatMessage{}, false
	}
	return ChatMessage{Role: "assistant", Content: text}, true
}

func easyMessageText(m *oairesponses.EasyInputMessageParam) (string, []ChatContentPart, error) {
	if m.Content.OfString.Valid() {
		return m.Content.OfString.Value, nil, nil
	}
	return convertInputContent(m.Content.OfInputItemContentList)
}

// functionCallOutputParts 提取工具输出文本与图片：
// 文本留在 role=tool，图片收集为独立 user 消息（对齐 opencode lowerToolMessages）。
func functionCallOutputParts(fco *oairesponses.ResponseInputItemFunctionCallOutputParam) (string, []ChatContentPart, error) {
	if fco == nil {
		return "", nil, nil
	}
	if fco.Output.OfString.Valid() {
		return fco.Output.OfString.Value, nil, nil
	}
	var textParts []string
	var images []ChatContentPart
	for _, it := range fco.Output.OfResponseFunctionCallOutputItemArray {
		switch {
		case it.OfInputText != nil:
			if it.OfInputText.Text != "" {
				textParts = append(textParts, it.OfInputText.Text)
			}
		case it.OfInputImage != nil:
			p, err := inputImageContentPart(it.OfInputImage)
			if err != nil {
				return "", nil, err
			}
			images = append(images, p)
		case it.OfInputFile != nil:
			return "", nil, fmt.Errorf("chatconvert: function_call_output 含 input_file，无法映射到 Chat")
		}
	}
	return strings.Join(textParts, "\n"), images, nil
}

func customToolOutputParts(c *oairesponses.ResponseCustomToolCallOutputParam) (string, []ChatContentPart, error) {
	if c == nil {
		return "", nil, nil
	}
	if c.Output.OfString.Valid() {
		return c.Output.OfString.Value, nil, nil
	}
	var textParts []string
	var images []ChatContentPart
	for _, it := range c.Output.OfOutputContentList {
		switch {
		case it.OfInputText != nil:
			if it.OfInputText.Text != "" {
				textParts = append(textParts, it.OfInputText.Text)
			}
		case it.OfInputImage != nil:
			p, err := inputImagePart(it.OfInputImage)
			if err != nil {
				return "", nil, err
			}
			images = append(images, p)
		case it.OfInputFile != nil:
			return "", nil, fmt.Errorf("chatconvert: custom tool output 含 input_file，无法映射到 Chat")
		}
	}
	return strings.Join(textParts, "\n"), images, nil
}

// escapeSystemUpdateText 与 opencode wrapSystemUpdate 一致：XML 转义 & < >，
// 避免时序 system 更新内容关闭 wrapper。
func escapeSystemUpdateText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func wrapSystemUpdate(text string) string {
	return "<system-update>\n" + escapeSystemUpdateText(text) + "\n</system-update>"
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

func applyPatchArgs(call *oairesponses.ResponseInputItemApplyPatchCallParam) (string, error) {
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
		return "", fmt.Errorf("invalid operation")
	}
	if call.Status != "" {
		input["status"] = call.Status
	}
	switch {
	case call.Caller.OfDirect != nil:
		input["caller_type"] = "direct"
	case call.Caller.OfProgram != nil:
		input["caller_type"] = "program"
		if call.Caller.OfProgram.CallerID != "" {
			input["caller_id"] = call.Caller.OfProgram.CallerID
		}
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func freeformArgsJSON(input string) string {
	b, err := json.Marshal(map[string]string{"s": input})
	if err != nil {
		return `{"s":""}`
	}
	return string(b)
}

// chatFunctionArguments 仅为空参数补充 Chat 所需的空对象；非空参数保持上游原始语义。
func chatFunctionArguments(s string) string {
	if s == "" {
		return "{}"
	}
	return s
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
			names = append(names, chatToolName(ct))
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
			if _, ok := seen[chatToolName(ct)]; ok {
				continue
			}
			seen[chatToolName(ct)] = struct{}{}
			out = append(out, ct)
		}
	}
	return out
}

// normalizeToolSchema 完整投影 Chat 工具 schema（对齐 opencode ToolSchemaProjection.openAI）：
// 顶层强制 type=object；anyOf 的 record 变体展平进 properties 并强制
// additionalProperties=false；递归移除 anyOf/type 数组中的 null 变体，
// 单个 anyOf 变体直接并入父级。
func normalizeToolSchema(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{"type": "object"}
	}
	flattened := make(map[string]any, len(m))
	anyOf, hasAnyOf := m["anyOf"]
	for k, child := range m {
		if k == "anyOf" {
			continue
		}
		flattened[k] = child
	}
	flattened["type"] = "object"
	var variants []any
	if arr, ok := anyOf.([]any); ok {
		for _, variant := range arr {
			if _, ok := variant.(map[string]any); ok {
				variants = append(variants, variant)
			}
		}
	}
	if len(variants) > 0 {
		props := map[string]any{}
		for _, variant := range variants {
			vm := variant.(map[string]any)
			if vp, ok := vm["properties"].(map[string]any); ok {
				for pk, pv := range vp {
					props[pk] = pv
				}
			}
		}
		flattened["properties"] = props
		flattened["additionalProperties"] = false
	} else if hasAnyOf {
		flattened["anyOf"] = anyOf
	}
	normalized, ok := normalizeToolSchemaNode(flattened).(map[string]any)
	if !ok {
		return map[string]any{"type": "object"}
	}
	return normalized
}

// normalizeToolSchemaNode 递归归一化 schema 节点：先去除 anyOf 并合并唯一
// 非 null 变体，再收敛 type 数组中的 null。
func normalizeToolSchemaNode(v any) any {
	switch m := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(m))
		for k, child := range m {
			if k == "anyOf" {
				continue
			}
			out[k] = normalizeToolSchemaNode(child)
		}
		if anyOf, ok := m["anyOf"].([]any); ok {
			variants := make([]any, 0, len(anyOf))
			for _, variant := range anyOf {
				if vm, ok := variant.(map[string]any); ok && isNullSchemaType(vm["type"]) {
					continue
				}
				variants = append(variants, normalizeToolSchemaNode(variant))
			}
			switch len(variants) {
			case 1:
				if vm, ok := variants[0].(map[string]any); ok {
					for k, val := range vm {
						out[k] = val
					}
				} else {
					out["anyOf"] = variants
				}
			default:
				if len(variants) > 0 {
					out["anyOf"] = variants
				}
			}
		}
		if types, ok := out["type"].([]any); ok {
			cleaned := removeNullType(types)
			switch len(cleaned) {
			case 0:
				delete(out, "type")
			case 1:
				out["type"] = cleaned[0]
			default:
				out["type"] = cleaned
			}
		}
		return out
	case []any:
		out := make([]any, len(m))
		for i, child := range m {
			out[i] = normalizeToolSchemaNode(child)
		}
		return out
	default:
		return v
	}
}

func removeNullType(types []any) []any {
	out := make([]any, 0, len(types))
	for _, t := range types {
		if t != "null" {
			out = append(out, t)
		}
	}
	return out
}

// isNullSchemaType 判断 schema 节点是否为 null 类型（字符串或 ["null"] 数组）。
func isNullSchemaType(t any) bool {
	switch v := t.(type) {
	case string:
		return v == "null"
	case []any:
		return len(v) == 1 && v[0] == "null"
	default:
		return false
	}
}

func toolUnionToChat(t oairesponses.ToolUnionParam, freeform map[string]struct{}) []ChatTool {
	switch {
	case t.OfFunction != nil:
		f := t.OfFunction
		fn := ChatFunction{
			Name:        f.Name,
			Description: optString(f.Description),
			Parameters:  normalizeToolSchema(f.Parameters),
		}
		if f.Strict.Valid() {
			fn.Strict = ptr(f.Strict.Value)
		}
		return []ChatTool{{Type: "function", Function: fn}}
	case t.OfCustom != nil:
		c := t.OfCustom
		freeform[c.Name] = struct{}{}
		return []ChatTool{customChatTool(c.Name, optString(c.Description), chatCustomGrammar(c))}
	case t.OfShell != nil, t.OfLocalShell != nil:
		name := toolcatalog.ChatNameShell
		if t.OfLocalShell != nil {
			name = toolcatalog.ChatNameLocalShell
		}
		return []ChatTool{{
			Type: "function",
			Function: ChatFunction{
				Name:       name,
				Parameters: toolcatalog.FreeformInputSchema(""),
			},
		}}
	case t.OfApplyPatch != nil:
		return []ChatTool{{
			Type: "function",
			Function: ChatFunction{
				Name:       toolcatalog.ChatNameApplyPatch,
				Parameters: toolcatalog.FreeformInputSchema(""),
			},
		}}
	case t.OfToolSearch != nil:
		s := t.OfToolSearch
		return []ChatTool{{
			Type: "function",
			Function: ChatFunction{
				Name:        "tool_search",
				Description: optString(s.Description),
				Parameters:  normalizeToolSchema(s.Parameters),
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
					Parameters:  normalizeToolSchema(nestedFn.Parameters),
				}
				if nestedFn.Strict.Valid() {
					cf.Strict = ptr(nestedFn.Strict.Value)
				}
				out = append(out, ChatTool{Type: "function", Function: cf})
			case nested.OfCustom != nil:
				c := nested.OfCustom
				name := toolcatalog.ToolName(ns.Name, c.Name)
				freeform[name] = struct{}{}
				out = append(out, customChatTool(name, optString(c.Description), chatCustomGrammar(c)))
			default:
				slog.Debug("chatconvert: 跳过 namespace 内不支持的子工具")
			}
		}
		return out
	case t.OfWebSearch != nil, t.OfWebSearchPreview != nil:
		return []ChatTool{webSearchToolDecl()}
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
		if allowedNames[chatToolName(t)] {
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
			if id.OpenAIType == model.ToolTypeMcp && id.Name == "" {
				matched := false
				for _, d := range declaredIDs {
					if d.Namespace == id.Namespace {
						matched = true
						break
					}
				}
				if !matched {
					return nil, fmt.Errorf("chatconvert: tool_choice allowed_tools entry %s is not declared", id)
				}
				for _, d := range declaredIDs {
					if d.Namespace == id.Namespace {
						allowedNames[d.ConvertedName()] = true
					}
				}
				continue
			}
			matched := false
			for _, d := range declaredIDs {
				if d.Equal(id) {
					matched = true
					break
				}
				if (id.OpenAIType == model.ToolTypeLocalShell || id.OpenAIType == model.ToolTypeShell) &&
					(d.OpenAIType == model.ToolTypeLocalShell || d.OpenAIType == model.ToolTypeShell) &&
					id.Name == d.Name {
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("chatconvert: tool_choice allowed_tools entry %s is not declared", id)
			}
			// c 路径出站把 local_shell 声明为 name=local_shell；
			// toolcatalog.Identity 沿用 a 路径的 Anthropic 名 shell，这里对准 Chat 名。
			name := id.ConvertedName()
			if id.OpenAIType == model.ToolTypeLocalShell {
				name = toolcatalog.ChatNameLocalShell
			}
			allowedNames[name] = true
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
			"function": map[string]string{"name": toolcatalog.ChatNameShell},
		}
	case tc.OfSpecificApplyPatchToolChoice != nil:
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": toolcatalog.ChatNameApplyPatch},
		}
	case tc.OfAllowedTools != nil:
		mode := string(tc.OfAllowedTools.Mode)
		if mode == "required" {
			return "required"
		}
		return "auto"
	case tc.OfHostedTool != nil:
		return convertHostedToolChoice(tc.OfHostedTool)
	case tc.OfMcpTool != nil:
		slog.Debug("chatconvert: tool_choice mcp 无 Chat 等价，降级为默认选择",
			"server_label", tc.OfMcpTool.ServerLabel, "name", optString(tc.OfMcpTool.Name))
		return nil
	case tc.OfResponseNewsToolChoiceSpecificProgrammaticToolCallingParam != nil:
		slog.Debug("chatconvert: tool_choice programmatic 无 Chat 等价，降级为默认选择")
		return nil
	default:
		return nil
	}
}

// convertHostedToolChoice 把 hosted tool_choice 映射为 Chat 强制 function 选择。
// c 路径已把 web_search 声明为同名 synthetic function（见 hosted.go /
// toolcatalog/chatnames.go），强制选择按同名 function 下发即可。
func convertHostedToolChoice(hosted *oairesponses.ToolChoiceTypesParam) any {
	switch hosted.Type {
	case oairesponses.ToolChoiceTypesType(toolcatalog.ChatNameWebSearch),
		oairesponses.ToolChoiceTypesTypeWebSearchPreview,
		oairesponses.ToolChoiceTypesTypeWebSearchPreview2025_03_11:
		return namedFunctionChoice(toolcatalog.ChatNameWebSearch)
	default:
		slog.Debug("chatconvert: tool_choice hosted 类型无 Chat 等价，降级为默认选择",
			"type", string(hosted.Type))
		return nil
	}
}

// namedFunctionChoice 构造 Chat 的强制 function 选择（SDK 参数，type 恒为 function）。
func namedFunctionChoice(name string) any {
	return openai.ChatCompletionNamedToolChoiceParam{
		Function: openai.ChatCompletionNamedToolChoiceFunctionParam{Name: name},
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
