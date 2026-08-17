package convert

import (
	"testing"
)

// agent_message 表示另一个 agent 的通信内容（NEW_TASK / MESSAGE / FINAL_ANSWER）。
// 在父 agent 历史里它由 wait_agent 等协作工具触发返回，因此协议上最接近的
// 等价物是工具结果：合并进前一个 function_call_output，而不是独立成一条消息，
// 避免污染 user/assistant turn 边界或产生连续同 role 消息。
func TestRestoreAgentMessageMergeIntoPreviousToolResult(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantInPrev string
	}{
		{
			name: "agent_message after wait_agent output → merged into tool result",
			input: `[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"do work"}]},
				{"type":"function_call","id":"fc_1","call_id":"c1","name":"wait_agent","arguments":"{}"},
				{"type":"function_call_output","call_id":"c1","output":"{\"message\":\"Wait completed.\"}"},
				{"type":"agent_message","content":[{"type":"input_text","text":"Message Type: FINAL_ANSWER\n子 agent 已完成"}]}
			]`,
			wantInPrev: "Message Type: FINAL_ANSWER\n子 agent 已完成",
		},
		{
			name: "agent_message encrypted content envelope → text merged",
			input: `[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"do work"}]},
				{"type":"function_call","id":"fc_1","call_id":"c1","name":"wait_agent","arguments":"{}"},
				{"type":"function_call_output","call_id":"c1","output":"done"},
				{"type":"agent_message","content":[
					{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},
					{"type":"encrypted_content","encrypted_content":"secret"}
				]}
			]`,
			wantInPrev: "Message Type: NEW_TASK\nPayload:\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"input":` + tc.input + `}`)
			req, err := DecodeResponseNewParams(body)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			merged := false
			for _, item := range req.Input.OfInputItemList {
				fco := item.OfFunctionCallOutput
				if fco == nil {
					continue
				}
				out := fco.Output.OfString.Value
				if out != "" && out != "done" && out != `{"message":"Wait completed."}` {
					merged = true
					break
				}
			}
			if !merged {
				t.Errorf("agent_message not merged into previous function_call_output, items=%d", len(req.Input.OfInputItemList))
			}
		})
	}
}

// 没有前置工具结果时，agent_message 回退为独立 user 消息，保证内容不丢。
func TestRestoreAgentMessageFallbackUser(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev1"}]},
		{"type":"agent_message","content":[{"type":"input_text","text":"独立通知"}]}
	]}`)

	req, err := DecodeResponseNewParams(body)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	last := req.Input.OfInputItemList[len(req.Input.OfInputItemList)-1]
	if last.OfMessage == nil {
		t.Fatalf("last item should be fallback user message")
	}
	if string(last.OfMessage.Role) != "user" {
		t.Errorf("want user role, got %s", last.OfMessage.Role)
	}
	got := last.OfMessage.Content.OfString.Value
	if got != "独立通知" {
		t.Errorf("want content %q, got %q", "独立通知", got)
	}
}
