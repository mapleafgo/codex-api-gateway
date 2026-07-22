package model

import "encoding/json"

// SSEEvent is one server-sent event to emit to the Codex client.
// Data holds the already-marshaled event JSON.
type SSEEvent struct {
	Type string
	Data json.RawMessage
}

// OutputTextDeltaEvent carries an output text token delta.
type OutputTextDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
	// Logprobs 对齐官方 response.output_text.delta.logprobs；空时 omitempty。
	Logprobs []TokenLogprob `json:"logprobs,omitempty"`
}

// OutputTextDoneEvent closes an output text content part.
type OutputTextDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Text           string `json:"text"`
	// Logprobs 对齐官方 response.output_text.done.logprobs；空时 omitempty。
	Logprobs []TokenLogprob `json:"logprobs,omitempty"`
}

// TokenLogprob 是 Responses output_text 的 token 级 log 概率（无 Chat bytes 字段）。
type TokenLogprob struct {
	Token       string            `json:"token"`
	Logprob     float64           `json:"logprob"`
	TopLogprobs []TopTokenLogprob `json:"top_logprobs,omitempty"`
}

// TopTokenLogprob 是某一位置的候选 token。
type TopTokenLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

// RefusalDeltaEvent 承载拒绝内容的增量文本。
type RefusalDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

// RefusalDoneEvent 标记拒绝内容已经结束。
type RefusalDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	ItemID         string `json:"item_id"`
	Refusal        string `json:"refusal"`
}

// ReasoningTextDeltaEvent carries a reasoning text token delta.
type ReasoningTextDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

// ReasoningTextDoneEvent closes a reasoning text stream.
type ReasoningTextDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Text           string `json:"text"`
}

// ReasoningSummaryPartAddedEvent opens a reasoning summary part.
type ReasoningSummaryPartAddedEvent struct {
	Type           string      `json:"type"`
	SequenceNumber int64       `json:"sequence_number,omitempty"`
	OutputIndex    int         `json:"output_index"`
	SummaryIndex   int         `json:"summary_index"`
	ItemID         string      `json:"item_id"`
	Part           SummaryPart `json:"part"`
}

// ReasoningSummaryPartDoneEvent closes a reasoning summary part.
type ReasoningSummaryPartDoneEvent struct {
	Type           string      `json:"type"`
	SequenceNumber int64       `json:"sequence_number,omitempty"`
	OutputIndex    int         `json:"output_index"`
	SummaryIndex   int         `json:"summary_index"`
	ItemID         string      `json:"item_id"`
	Part           SummaryPart `json:"part"`
}

// ReasoningSummaryTextDeltaEvent carries a reasoning summary text delta.
type ReasoningSummaryTextDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

// ReasoningSummaryTextDoneEvent closes a reasoning summary text stream.
type ReasoningSummaryTextDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	ItemID         string `json:"item_id"`
	Text           string `json:"text"`
}

// ContentPartAddedEvent opens a message content part.
type ContentPartAddedEvent struct {
	Type           string         `json:"type"`
	SequenceNumber int64          `json:"sequence_number,omitempty"`
	OutputIndex    int            `json:"output_index"`
	ContentIndex   int            `json:"content_index"`
	ItemID         string         `json:"item_id"`
	Part           ContentPartOut `json:"part"`
}

// ContentPartDoneEvent closes a message content part.
type ContentPartDoneEvent struct {
	Type           string         `json:"type"`
	SequenceNumber int64          `json:"sequence_number,omitempty"`
	OutputIndex    int            `json:"output_index"`
	ContentIndex   int            `json:"content_index"`
	ItemID         string         `json:"item_id"`
	Part           ContentPartOut `json:"part"`
}

// FunctionCallArgumentsDeltaEvent carries partial tool-call arguments.
type FunctionCallArgumentsDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

// FunctionCallArgumentsDoneEvent closes tool-call arguments.
type FunctionCallArgumentsDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Arguments      string `json:"arguments"`
}

// CustomToolCallInputDeltaEvent carries custom tool input.
type CustomToolCallInputDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

// CustomToolCallInputDoneEvent closes custom tool input.
type CustomToolCallInputDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Input          string `json:"input"`
}

// WebSearchCallEvent is the payload for response.web_search_call.in_progress /
// .searching / .completed events. These events carry only identifiers; the item
// itself travels via output_item.added/done.
type WebSearchCallEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
}

// CodeInterpreterCallEvent 用于 code_interpreter_call 的 in_progress / interpreting / completed 事件。
type CodeInterpreterCallEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
}

// CodeInterpreterCallCodeDeltaEvent 用于 response.code_interpreter_call_code.delta。
type CodeInterpreterCallCodeDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

// CodeInterpreterCallCodeDoneEvent 用于 response.code_interpreter_call_code.done。
type CodeInterpreterCallCodeDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Code           string `json:"code"`
}

// McpCallEvent 用于 mcp_call 的 in_progress / completed / failed 事件。
type McpCallEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
}

// McpCallArgumentsDeltaEvent 用于 response.mcp_call_arguments.delta。
type McpCallArgumentsDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

// McpCallArgumentsDoneEvent 用于 response.mcp_call_arguments.done。
type McpCallArgumentsDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Arguments      string `json:"arguments"`
}

// OutputItemAddedEvent opens an output item.
type OutputItemAddedEvent struct {
	Type           string     `json:"type"`
	SequenceNumber int64      `json:"sequence_number,omitempty"`
	OutputIndex    int        `json:"output_index"`
	Item           OutputItem `json:"item"`
}

// OutputItemDoneEvent closes an output item.
type OutputItemDoneEvent struct {
	Type           string     `json:"type"`
	SequenceNumber int64      `json:"sequence_number,omitempty"`
	OutputIndex    int        `json:"output_index"`
	Item           OutputItem `json:"item"`
}

// TerminalResponseEvent wraps ResponseObject for created/in_progress/
// completed/incomplete/failed events.
type TerminalResponseEvent struct {
	Type           string         `json:"type"`
	SequenceNumber int64          `json:"sequence_number,omitempty"`
	Response       ResponseObject `json:"response"`
}

// ResponseErrorEvent is the error payload for response.error events.
type ResponseErrorEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	Code           string `json:"code,omitempty"`
	Message        string `json:"message"`
	Param          string `json:"param,omitempty"`
}

// SummaryPart is one reasoning summary content part (text).
type SummaryPart struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

// ContentPartOut is one content part emitted in content_part.added/done.
type ContentPartOut struct {
	Type        string  `json:"type"` // output_text | refusal
	Text        string  `json:"text"`
	Annotations []any   `json:"annotations,omitempty"`
	Refusal     *string `json:"refusal,omitempty"`
	// Logprobs 仅 output_text done 时可能带上累计 token 概率。
	Logprobs []TokenLogprob `json:"logprobs,omitempty"`
}

// MarshalJSON 按 content part 类型输出互斥的 Responses wire 字段。
func (p ContentPartOut) MarshalJSON() ([]byte, error) {
	if p.Type == ContentTypeRefusal {
		refusal := ""
		if p.Refusal != nil {
			refusal = *p.Refusal
		}
		return json.Marshal(struct {
			Type    string `json:"type"`
			Refusal string `json:"refusal"`
		}{
			Type:    p.Type,
			Refusal: refusal,
		})
	}
	annotations := p.Annotations
	if annotations == nil {
		annotations = []any{}
	}
	return json.Marshal(struct {
		Type        string         `json:"type"`
		Text        string         `json:"text"`
		Annotations []any          `json:"annotations"`
		Logprobs    []TokenLogprob `json:"logprobs,omitempty"`
	}{
		Type:        p.Type,
		Text:        p.Text,
		Annotations: annotations,
		Logprobs:    p.Logprobs,
	})
}

// MarshalEvent marshals any event struct into an SSEEvent.
func MarshalEvent(eventType string, v any) SSEEvent {
	b, _ := json.Marshal(v)
	return SSEEvent{Type: eventType, Data: b}
}

// ErrorEvent 对应 OpenAI Responses 协议的顶层 error 事件。
// 上游 Anthropic error 触发时，与 response.failed 同时发出。
type ErrorEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	Param          string `json:"param,omitempty"`
}

// OutputTextAnnotationAddedEvent 对应 response.output_text.annotation.added 事件，
// 把 Anthropic citations_delta 映射而来的 OpenAI 注解推给客户端。
// Annotation 为 any 以承载 url_citation / file_citation 等不同变体。
type OutputTextAnnotationAddedEvent struct {
	Type            string `json:"type"`
	SequenceNumber  int64  `json:"sequence_number,omitempty"`
	OutputIndex     int    `json:"output_index"`
	ContentIndex    int    `json:"content_index"`
	ItemID          string `json:"item_id"`
	AnnotationIndex int64  `json:"annotation_index"`
	Annotation      any    `json:"annotation"`
}
