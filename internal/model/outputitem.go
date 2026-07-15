package model

// OutputItem is a self-contained output item (message/function_call/reasoning)
// used both for emitted output_item.added/done events and for session storage.
// It uses omitempty so Marshal stays clean (unlike SDK ResponseOutputItemUnion).
type OutputItem struct {
	Type             string       `json:"type"` // message | function_call | reasoning
	ID               string       `json:"id"`
	Status           string       `json:"status,omitempty"`
	Role             string       `json:"role,omitempty"`    // message
	Content          []OutputText `json:"content,omitempty"` // message
	CallID           string       `json:"call_id,omitempty"` // function_call
	Name             string       `json:"name,omitempty"`    // function_call
	Arguments        string       `json:"arguments,omitempty"`
	Summary          []OutputText `json:"summary,omitempty"`           // reasoning
	EncryptedContent string       `json:"encrypted_content,omitempty"` // reasoning (redacted)
	Signature        string       `json:"signature,omitempty"`         // reasoning (plaintext thinking)
}

// OutputText is one text content/summary part.
type OutputText struct {
	Type string `json:"type"` // output_text | summary_text
	Text string `json:"text"`
}
