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
}

// OutputTextDoneEvent closes an output text content part.
type OutputTextDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Text           string `json:"text"`
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
	Type string `json:"type"` // output_text | refusal
	Text string `json:"text,omitempty"`
}

// MarshalEvent marshals any event struct into an SSEEvent.
func MarshalEvent(eventType string, v any) SSEEvent {
	b, _ := json.Marshal(v)
	return SSEEvent{Type: eventType, Data: b}
}
