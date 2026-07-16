package model

import "encoding/json"

// OutputItem is a self-contained output item (message/tool call/reasoning)
// used both for emitted output_item.added/done events and for session storage.
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
	Type        string  `json:"type"` // output_text | refusal | summary_text
	Text        string  `json:"text"`
	Annotations []any   `json:"annotations,omitempty"`
	Refusal     *string `json:"refusal,omitempty"`
}

// MarshalJSON 保证 message item 的必填 content 字段即使为空也会写入 wire payload。
func (i OutputItem) MarshalJSON() ([]byte, error) {
	if i.Type != ItemTypeMessage {
		type outputItem OutputItem
		return json.Marshal(outputItem(i))
	}
	return json.Marshal(struct {
		Type    string       `json:"type"`
		ID      string       `json:"id"`
		Status  string       `json:"status,omitempty"`
		Role    string       `json:"role,omitempty"`
		Phase   string       `json:"phase,omitempty"`
		Content []OutputText `json:"content"`
	}{
		Type: i.Type, ID: i.ID, Status: i.Status, Role: i.Role, Phase: i.Phase,
		Content: i.Content,
	})
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
	if p.Type != ContentTypeOutputText {
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{
			Type: p.Type,
			Text: p.Text,
		})
	}
	annotations := p.Annotations
	if annotations == nil {
		annotations = []any{}
	}
	return json.Marshal(struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Annotations []any  `json:"annotations"`
	}{
		Type:        p.Type,
		Text:        p.Text,
		Annotations: annotations,
	})
}
