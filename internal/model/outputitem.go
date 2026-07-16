package model

import "encoding/json"

// OutputItem is a self-contained output item (message/tool call/reasoning)
// used both for emitted output_item.added/done events and for session storage.
// It uses omitempty so Marshal stays clean (unlike SDK ResponseOutputItemUnion).
type OutputItem struct {
	Type             string       `json:"type"` // message | function_call | custom_tool_call | reasoning
	ID               string       `json:"id"`
	Status           string       `json:"status,omitempty"`
	Role             string       `json:"role,omitempty"`    // message
	Phase            string       `json:"phase,omitempty"`   // assistant message
	Content          []OutputText `json:"content,omitempty"` // message
	CallID           string       `json:"call_id,omitempty"` // tool call
	Name             string       `json:"name,omitempty"`    // tool call
	Arguments        string       `json:"arguments,omitempty"`
	Input            string       `json:"input,omitempty"`             // custom_tool_call
	Output           string       `json:"output,omitempty"`            // tool call output
	Namespace        string       `json:"namespace,omitempty"`         // namespaced tool call
	Summary          []OutputText `json:"summary,omitempty"`           // reasoning
	EncryptedContent string       `json:"encrypted_content,omitempty"` // reasoning (redacted)
	Signature        string       `json:"signature,omitempty"`         // reasoning (plaintext thinking)
}

// OutputText is one message content or reasoning summary part.
type OutputText struct {
	Type    string  `json:"type"` // output_text | refusal | summary_text
	Text    string  `json:"text"`
	Refusal *string `json:"refusal,omitempty"`
}

// MarshalJSON 按 content 类型输出互斥的 Responses wire 字段。
func (p OutputText) MarshalJSON() ([]byte, error) {
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
	return json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{
		Type: p.Type,
		Text: p.Text,
	})
}
