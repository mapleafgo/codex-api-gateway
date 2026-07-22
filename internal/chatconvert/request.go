// Package chatconvert 将 OpenAI Responses 请求转为 Chat Completions 请求（仅流式）。
package chatconvert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// ChatRequest 是 Chat Completions 流式请求的最小结构。
type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	Tools         []ChatTool     `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions 控制流式 usage 回传。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMessage 是 Chat 多轮消息。
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
}

// ChatTool 是 function 工具声明。
type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

// ChatFunction 是 function 定义。
type ChatFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
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
func ToChat(req *oairesponses.ResponseNewParams, model string) (*ChatRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("chatconvert: nil request")
	}
	out := &ChatRequest{
		Model:         model,
		Stream:        true,
		StreamOptions: &StreamOptions{IncludeUsage: true},
	}
	if req.Temperature.Valid() {
		out.Temperature = ptr(req.Temperature.Value)
	}
	if req.TopP.Valid() {
		out.TopP = ptr(req.TopP.Value)
	}
	if req.MaxOutputTokens.Valid() && req.MaxOutputTokens.Value > 0 {
		out.MaxTokens = ptr(int(req.MaxOutputTokens.Value))
	}

	msgs, err := convertMessages(req)
	if err != nil {
		return nil, err
	}
	out.Messages = msgs
	out.Tools = convertTools(req.Tools)
	if tc := convertToolChoice(req.ToolChoice); tc != nil {
		out.ToolChoice = tc
	}
	return out, nil
}

// Marshal 将 ChatRequest 编成可 POST 的 JSON。
func Marshal(req *ChatRequest) ([]byte, error) {
	return json.Marshal(req)
}

func convertMessages(req *oairesponses.ResponseNewParams) ([]ChatMessage, error) {
	var out []ChatMessage
	if req.Instructions.Valid() && req.Instructions.Value != "" {
		out = append(out, ChatMessage{Role: "system", Content: req.Instructions.Value})
	}
	if req.Input.OfString.Valid() && req.Input.OfString.Value != "" {
		out = append(out, ChatMessage{Role: "user", Content: req.Input.OfString.Value})
		return out, nil
	}
	for i := range req.Input.OfInputItemList {
		item := &req.Input.OfInputItemList[i]
		msg, ok := convertItem(item)
		if ok {
			out = append(out, msg)
		}
	}
	return out, nil
}

func convertItem(item *oairesponses.ResponseInputItemUnionParam) (ChatMessage, bool) {
	switch {
	case item.OfMessage != nil:
		return convertEasyMessage(item.OfMessage)
	case item.OfFunctionCall != nil:
		// Chat Completions 的 function.arguments 就是 string 字段，原样透传，不做 JSON 强解。
		fc := item.OfFunctionCall
		return ChatMessage{
			Role: "assistant",
			ToolCalls: []ChatToolCall{{
				ID:   fc.CallID,
				Type: "function",
				Function: ChatToolCallFunc{
					Name:      fc.Name,
					Arguments: orDefault(fc.Arguments, "{}"),
				},
			}},
		}, true
	case item.OfFunctionCallOutput != nil:
		fco := item.OfFunctionCallOutput
		content := functionCallOutputText(fco)
		return ChatMessage{
			Role:       "tool",
			ToolCallID: fco.CallID,
			Content:    content,
		}, true
	case item.OfCustomToolCall != nil:
		// MVP：custom 当 function；Chat arguments 是 string，input 原样塞入。
		c := item.OfCustomToolCall
		return ChatMessage{
			Role: "assistant",
			ToolCalls: []ChatToolCall{{
				ID:   c.CallID,
				Type: "function",
				Function: ChatToolCallFunc{
					Name:      c.Name + "_custom",
					Arguments: orDefault(c.Input, "{}"),
				},
			}},
		}, true
	case item.OfCustomToolCallOutput != nil:
		c := item.OfCustomToolCallOutput
		return ChatMessage{
			Role:       "tool",
			ToolCallID: c.CallID,
			Content:    c.Output,
		}, true
	default:
		slog.Debug("chatconvert: 跳过无 Chat 等价的 input item", "type", itemType(item))
		return ChatMessage{}, false
	}
}

func convertEasyMessage(m *oairesponses.EasyInputMessageParam) (ChatMessage, bool) {
	role := string(m.Role)
	if role == "developer" {
		role = "system"
	}
	text := easyMessageText(m)
	if text == "" && role != "assistant" {
		return ChatMessage{}, false
	}
	return ChatMessage{Role: role, Content: text}, true
}

func easyMessageText(m *oairesponses.EasyInputMessageParam) string {
	if m.Content.OfString.Valid() {
		return m.Content.OfString.Value
	}
	var b strings.Builder
	for _, part := range m.Content.OfInputItemContentList {
		if part.OfInputText != nil {
			b.WriteString(part.OfInputText.Text)
		}
	}
	return b.String()
}

func functionCallOutputText(fco *oairesponses.ResponseInputItemFunctionCallOutputParam) string {
	if fco.Output.OfString.Valid() {
		return fco.Output.OfString.Value
	}
	// 数组 content 简化为拼接文本；图片等 MVP 丢弃
	var b strings.Builder
	for _, it := range fco.Output.OfResponseFunctionCallOutputItemArray {
		if it.OfInputText != nil {
			b.WriteString(it.OfInputText.Text)
		}
	}
	return b.String()
}

func convertTools(tools []oairesponses.ToolUnionParam) []ChatTool {
	var out []ChatTool
	for _, t := range tools {
		switch {
		case t.OfFunction != nil:
			f := t.OfFunction
			out = append(out, ChatTool{
				Type: "function",
				Function: ChatFunction{
					Name:        f.Name,
					Description: optString(f.Description),
					Parameters:  f.Parameters,
				},
			})
		case t.OfCustom != nil:
			c := t.OfCustom
			out = append(out, ChatTool{
				Type: "function",
				Function: ChatFunction{
					Name:        c.Name + "_custom",
					Description: optString(c.Description),
				},
			})
		default:
			slog.Debug("chatconvert: 跳过无 Chat 等价的 tool 声明")
		}
	}
	return out
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
			"function": map[string]string{"name": tc.OfCustomTool.Name + "_custom"},
		}
	default:
		return nil
	}
}

func optString(v oparam.Opt[string]) string {
	if v.Valid() {
		return v.Value
	}
	return ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func itemType(item *oairesponses.ResponseInputItemUnionParam) string {
	switch {
	case item.OfMessage != nil:
		return "message"
	case item.OfFunctionCall != nil:
		return "function_call"
	case item.OfFunctionCallOutput != nil:
		return "function_call_output"
	case item.OfReasoning != nil:
		return "reasoning"
	default:
		return "unknown"
	}
}
