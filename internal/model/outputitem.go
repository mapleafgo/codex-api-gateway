package model

import "encoding/json"

// plaintextCollabTools 是 Codex collaboration namespace 下通过 inter-agent
// 消息投递任务/通信的工具。网关对这些 function_call 注入空的
// encrypted_function_args 信号，让 Codex 走明文投递（DirectPlaintextMessage），
// 而不是把内容塞进 encrypted_content——后者 openai-go SDK 不识别，子 agent 收不到。
var plaintextCollabTools = map[string]bool{
	"spawn_agent":   true,
	"send_message":  true,
	"followup_task": true,
}

// IsPlaintextCollabTool 报告 namespace/name 是否是走 plaintext 投递的
// collaboration 工具。Codex 的 ToolRouter.direct_source 依此切明文/密文分支。
func IsPlaintextCollabTool(namespace, name string) bool {
	return namespace == "collaboration" && plaintextCollabTools[name]
}

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
	// Logprobs 仅 output_text；Chat 路径在上游返回 token logprobs 时填充。
	Logprobs []TokenLogprob `json:"logprobs,omitempty"`
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
	if i.Type == ItemTypeReasoning {
		// reasoning 的 summary 是 OpenAI wire api:"required"，且 Codex 的
		// ResponseItem Reasoning 变体把 summary 定义为无 #[serde(default)] 的
		// required Vec —— 缺失该字段会让 Codex serde 解析失败，导致
		// output_item.added 被丢弃、active_item 不被设置，表现为
		// "ReasoningSummaryPartAdded without active item"。故即使空也必须输出 "summary":[]。
		summary := i.Summary
		if summary == nil {
			summary = []OutputText{}
		}
		return json.Marshal(struct {
			Type             string       `json:"type"`
			ID               string       `json:"id"`
			Summary          []OutputText `json:"summary"`
			EncryptedContent string       `json:"encrypted_content,omitempty"`
			Signature        string       `json:"signature,omitempty"`
			Status           string       `json:"status,omitempty"`
		}{
			Type: i.Type, ID: i.ID, Summary: summary,
			EncryptedContent: i.EncryptedContent, Signature: i.Signature,
			Status: i.Status,
		})
	}
	if i.Type == ItemTypeFunctionCall {
		// arguments 是 OpenAI wire api:"required"，且 Codex 的 ResponseItem
		// FunctionCall 变体 arguments 是无 #[serde(default)] 的 required String ——
		// added 时空串经 omitempty 不输出会导致 serde 反序列化失败、item 被丢弃、
		// active_item 不被设置，表现为 "FunctionCallArgumentsDelta without active item"。
		// call_id/name 同为 required，始终输出。
		type functionCallWire struct {
			Type                  string          `json:"type"`
			ID                    string          `json:"id"`
			CallID                string          `json:"call_id"`
			Name                  string          `json:"name"`
			Arguments             string          `json:"arguments"`
			Status                string          `json:"status,omitempty"`
			Namespace             string          `json:"namespace,omitempty"`
			EncryptedFunctionArgs json.RawMessage `json:"encrypted_function_args,omitempty"`
		}
		w := functionCallWire{
			Type: i.Type, ID: i.ID, CallID: i.CallID, Name: i.Name,
			Arguments: i.Arguments, Status: i.Status, Namespace: i.Namespace,
		}
		// collaboration 工具的 function_call 注空数组触发 Codex 明文投递
		// （ConventionalCollabTool），openai-go 不识别 encrypted_content。
		if IsPlaintextCollabTool(i.Namespace, i.Name) {
			w.EncryptedFunctionArgs = json.RawMessage("[]")
		}
		return json.Marshal(w)
	}
	if i.Type == ItemTypeCustomToolCall {
		// input 同 arguments，是 required String，空串经 omitempty 不输出会导致
		// Codex serde 反序列化失败、active_item 不被设置。
		return json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Input     string `json:"input"`
			Status    string `json:"status,omitempty"`
			Namespace string `json:"namespace,omitempty"`
		}{
			Type: i.Type, ID: i.ID, CallID: i.CallID, Name: i.Name,
			Input: i.Input, Status: i.Status, Namespace: i.Namespace,
		})
	}
	if i.Type != ItemTypeMessage {
		type outputItem OutputItem
		return json.Marshal(outputItem(i))
	}
	// content 是 OpenAI wire api:"required"，且 Codex 的 ResponseItem Message
	// 变体 content 是无 #[serde(default)] 的 required Vec —— nil 序列化为
	// "content":null 会让 Codex serde 反序列化失败、output_item.added 被丢弃、
	// active_item 不被设置，表现为 "OutputTextDelta without active item"。
	// 故即使空也必须输出 "content":[]。
	content := i.Content
	if content == nil {
		content = []OutputText{}
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
		Content: content,
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
