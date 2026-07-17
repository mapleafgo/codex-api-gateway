package model

import "encoding/json"

// OutputItem is a self-contained output item (message/tool call/reasoning)
// used both for emitted output_item.added/done events and for session storage.
type OutputItem struct {
	Type             string                  `json:"type"` // message | function_call | custom_tool_call | reasoning
	ID               string                  `json:"id"`
	Status           string                  `json:"status,omitempty"`
	Role             string                  `json:"role,omitempty"`    // message
	Phase            string                  `json:"phase,omitempty"`   // assistant message
	Content          []OutputText            `json:"content,omitempty"` // message
	CallID           string                  `json:"call_id,omitempty"` // tool call
	Name             string                  `json:"name,omitempty"`    // tool call
	Arguments        string                  `json:"arguments,omitempty"`
	Input            string                  `json:"input,omitempty"`             // custom_tool_call
	Output           string                  `json:"output,omitempty"`            // tool call output
	Namespace        string                  `json:"namespace,omitempty"`         // namespaced tool call
	Summary          []OutputText            `json:"summary,omitempty"`           // reasoning
	EncryptedContent string                  `json:"encrypted_content,omitempty"` // reasoning (redacted)
	Signature        string                  `json:"signature,omitempty"`         // reasoning (plaintext thinking)
	Action           *WebSearchAction        `json:"action,omitempty"`            // web_search_call
	ContainerID      string                  `json:"container_id,omitempty"`      // code_interpreter_call
	Code             string                  `json:"code,omitempty"`              // code_interpreter_call
	Outputs          []CodeInterpreterOutput `json:"outputs,omitempty"`           // code_interpreter_call
	ServerLabel      string                  `json:"server_label,omitempty"`      // mcp_call
	Execution        string                  `json:"execution,omitempty"`         // tool_search_call（client/server）
}

// WebSearchAction describes the action taken by a web_search_call output item.
type WebSearchAction struct {
	Type    string            `json:"type"`              // "search"
	Query   string            `json:"query,omitempty"`   // search query (Codex reads query)
	Queries []string          `json:"queries,omitempty"` // search queries
	Sources []WebSearchSource `json:"sources,omitempty"` // result sources (filled on completion)
}

// WebSearchSource is one result source of a web_search_call.
type WebSearchSource struct {
	Type string `json:"type"` // "url"
	URL  string `json:"url"`
}

// CodeInterpreterOutput is one output of a code_interpreter_call (logs / image).
// 本批仅承载 logs；image（file_id→url）不可转换，丢弃 + WARN。
type CodeInterpreterOutput struct {
	Type string `json:"type"` // "logs"
	Logs string `json:"logs,omitempty"`
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
	if i.Type == ItemTypeCodeInterpreterCall {
		outputs := i.Outputs
		if outputs == nil {
			outputs = []CodeInterpreterOutput{}
		}
		return json.Marshal(struct {
			Type        string                  `json:"type"`
			ID          string                  `json:"id"`
			Status      string                  `json:"status"`
			ContainerID string                  `json:"container_id"`
			Code        string                  `json:"code"`
			Outputs     []CodeInterpreterOutput `json:"outputs"`
		}{
			Type: i.Type, ID: i.ID, Status: i.Status,
			ContainerID: i.ContainerID, Code: i.Code,
			Outputs: outputs,
		})
	}
	if i.Type == ItemTypeMcpCall {
		// mcp_call failed 由 Status=failed 表达，错误文本并入 Output。
		// （OpenAI wire 的 error 字段为 nullable；本网关不单独产出 error 字段。）
		return json.Marshal(struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			Status      string `json:"status,omitempty"`
			ServerLabel string `json:"server_label"`
			Name        string `json:"name"`
			Arguments   string `json:"arguments"`
			Output      string `json:"output,omitempty"`
		}{
			Type: i.Type, ID: i.ID, Status: i.Status,
			ServerLabel: i.ServerLabel, Name: i.Name, Arguments: i.Arguments,
			Output: i.Output,
		})
	}
	if i.Type == ItemTypeToolSearchCall {
		// tool_search_call 的 required keys（id/call_id/arguments/execution）即使
		// 为空也必须输出（OpenAI wire api:"required"）；无 name 字段（区别于 function_call）。
		// arguments 必须是 JSON object（不是 string）——Codex 用 serde 反序列化成
		// SearchToolCallParams struct，string 会导致 parse 失败 → tool_search 报错返回空。
		args := i.Arguments
		if args == "" {
			args = "{}"
		}
		return json.Marshal(struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			CallID    string          `json:"call_id"`
			Arguments json.RawMessage `json:"arguments"`
			Execution string          `json:"execution"`
			Status    string          `json:"status,omitempty"`
		}{
			Type: i.Type, ID: i.ID, CallID: i.CallID,
			Arguments: json.RawMessage(args), Execution: i.Execution, Status: i.Status,
		})
	}
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
