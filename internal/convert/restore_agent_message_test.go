package convert

import (
	"testing"
)

// 按 Codex 源码，agent_message 始终映射为 assistant role
// （InterAgentMessage::role() 和 InterAgentCompletionMessage::role() 都返回 "assistant"）。
func TestRestoreAgentMessageRoleByContext(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantRole string
	}{
		{
			name: "task delivery: agent_message after developer messages → assistant",
			input: `[
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev1"}]},
				{"type":"agent_message","content":[{"type":"input_text","text":"task"}]}
			]`,
			wantRole: "assistant",
		},
		{
			name: "sub-agent reply: agent_message after function_call_output → assistant",
			input: `[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"do work"}]},
				{"type":"function_call","id":"fc_1","call_id":"c1","name":"wait_agent","arguments":"{}"},
				{"type":"function_call_output","call_id":"c1","output":"done"},
				{"type":"agent_message","content":[{"type":"input_text","text":"FINAL_ANSWER"}]}
			]`,
			wantRole: "assistant",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"input":` + tc.input + `}`)
			req, err := DecodeResponseNewParams(body)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			found := false
			for _, item := range req.Input.OfInputItemList {
				if item.OfMessage == nil {
					continue
				}
				if string(item.OfMessage.Role) == tc.wantRole {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("want role %q not found among %d items", tc.wantRole, len(req.Input.OfInputItemList))
			}
		})
	}
}

// 恢复后的 agent_message 必须包含原始内容文本，且位置正确。
func TestRestoreAgentMessageContentAndPosition(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev1"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"do work"}]},
		{"type":"function_call","id":"fc_1","call_id":"c1","name":"wait_agent","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"done"},
		{"type":"agent_message","content":[{"type":"input_text","text":"FINAL_ANSWER: 子agent已完成"}]}
	]}`)

	req, err := DecodeResponseNewParams(body)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	items := req.Input.OfInputItemList
	if len(items) != 5 {
		t.Fatalf("want 5 items, got %d", len(items))
	}

	// 位置 3 是 function_call_output，位置 4 是恢复的 agent_message。
	last := items[len(items)-1]
	if last.OfMessage == nil {
		t.Fatalf("last item should be restored agent_message")
	}
	if string(last.OfMessage.Role) != "assistant" {
		t.Errorf("want assistant role, got %s", last.OfMessage.Role)
	}
	got := last.OfMessage.Content.OfString.Value
	if got != "FINAL_ANSWER: 子agent已完成" {
		t.Errorf("want content %q, got %q", "FINAL_ANSWER: 子agent已完成", got)
	}
}
