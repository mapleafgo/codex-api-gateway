package convert

import (
	"testing"
)

// Codex 源码将 InterAgentMessage 与 InterAgentCompletionMessage 的 role 固定为
// assistant；恢复时必须保留 raw input 位置，不能并入其他工具结果。
func TestRestoreAgentMessageContentRoleAndPosition(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev1"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"do work"}]},
		{"type":"function_call","id":"fc_1","call_id":"c1","name":"wait_agent","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"done"},
		{"type":"agent_message","content":[{"type":"input_text","text":"Message Type: FINAL_ANSWER\nPayload:\nCHILD_FINAL_A"}]}
	]}`)

	req, err := DecodeResponseNewParams(body)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	items := req.Input.OfInputItemList
	if len(items) != 5 {
		t.Fatalf("want 5 items, got %d", len(items))
	}
	last := items[len(items)-1]
	if last.OfMessage == nil {
		t.Fatalf("last item should be restored agent_message, got %+v", last)
	}
	if string(last.OfMessage.Role) != "assistant" {
		t.Fatalf("want assistant role, got %s", last.OfMessage.Role)
	}
	got := last.OfMessage.Content.OfString.Value
	want := "Message Type: FINAL_ANSWER\nPayload:\nCHILD_FINAL_A"
	if got != want {
		t.Fatalf("want content %q, got %q", want, got)
	}
}

func TestRestoreMultipleAgentMessagesPreserveRawOrder(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"do work"}]},
		{"type":"function_call","id":"fc_1","call_id":"c1","name":"wait_agent","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"wait-1"},
		{"type":"agent_message","content":[{"type":"input_text","text":"FINAL_ANSWER_1"}]},
		{"type":"function_call","id":"fc_2","call_id":"c2","name":"wait_agent","arguments":"{}"},
		{"type":"function_call_output","call_id":"c2","output":"wait-2"},
		{"type":"agent_message","content":[{"type":"input_text","text":"FINAL_ANSWER_2"}]}
	]}`)

	req, err := DecodeResponseNewParams(body)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	items := req.Input.OfInputItemList
	if len(items) != 7 {
		t.Fatalf("want 7 items, got %d: %+v", len(items), items)
	}
	for _, tc := range []struct {
		index int
		want  string
	}{
		{index: 3, want: "FINAL_ANSWER_1"},
		{index: 6, want: "FINAL_ANSWER_2"},
	} {
		item := items[tc.index]
		if item.OfMessage == nil {
			t.Fatalf("items[%d] should be message, got %+v", tc.index, item)
		}
		if string(item.OfMessage.Role) != "assistant" {
			t.Fatalf("items[%d] role = %s, want assistant", tc.index, item.OfMessage.Role)
		}
		if got := item.OfMessage.Content.OfString.Value; got != tc.want {
			t.Fatalf("items[%d] content = %q, want %q", tc.index, got, tc.want)
		}
	}
	if got := items[2].OfFunctionCallOutput.Output.OfString.Value; got != "wait-1" {
		t.Fatalf("first wait output mutated: %q", got)
	}
	if got := items[5].OfFunctionCallOutput.Output.OfString.Value; got != "wait-2" {
		t.Fatalf("second wait output mutated: %q", got)
	}
}

func TestRestoreAgentMessageKeepsLaterAssistantOutputText(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"agent_message","content":[{"type":"input_text","text":"NEW_TASK"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PARENT_FINAL"}]}
	]}`)

	req, err := DecodeResponseNewParams(body)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	items := req.Input.OfInputItemList
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d: %+v", len(items), items)
	}
	if items[0].OfMessage == nil || string(items[0].OfMessage.Role) != "assistant" || items[0].OfMessage.Content.OfString.Value != "NEW_TASK" {
		t.Fatalf("agent_message restored incorrectly: %+v", items[0])
	}
	if items[1].OfMessage == nil || string(items[1].OfMessage.Role) != "assistant" {
		t.Fatalf("later assistant message missing: %+v", items[1])
	}
	var got string
	if content := items[1].OfMessage.Content; content.OfString.Valid() {
		got = content.OfString.Value
	} else if len(content.OfInputItemContentList) > 0 && content.OfInputItemContentList[0].OfInputText != nil {
		got = content.OfInputItemContentList[0].OfInputText.Text
	}
	if got != "PARENT_FINAL" {
		t.Fatalf("later assistant text = %q, want PARENT_FINAL", got)
	}
}

func TestRestoreAgentMessageKeepsLaterUserTurn(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"PARENT_QUESTION"}]},
		{"type":"agent_message","content":[{"type":"input_text","text":"CHILD_FINAL"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PARENT_FINAL"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"FOLLOWUP"}]}
	]}`)

	req, err := DecodeResponseNewParams(body)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	items := req.Input.OfInputItemList
	if len(items) != 4 {
		t.Fatalf("want 4 items, got %d: %+v", len(items), items)
	}
	last := items[3]
	if last.OfMessage == nil || string(last.OfMessage.Role) != "user" {
		t.Fatalf("last item should remain user, got %+v", last)
	}
	got := ""
	if last.OfMessage.Content.OfString.Valid() {
		got = last.OfMessage.Content.OfString.Value
	} else {
		for _, part := range last.OfMessage.Content.OfInputItemContentList {
			if part.OfInputText != nil {
				got += part.OfInputText.Text
			}
		}
	}
	if got != "FOLLOWUP" {
		for i, item := range items {
			if item.OfMessage == nil {
				continue
			}
			text := ""
			if item.OfMessage.Content.OfString.Valid() {
				text = item.OfMessage.Content.OfString.Value
			} else {
				for _, part := range item.OfMessage.Content.OfInputItemContentList {
					if part.OfInputText != nil {
						text += part.OfInputText.Text
					}
				}
			}
			t.Logf("item[%d] role=%s text=%q", i, item.OfMessage.Role, text)
		}
		t.Fatalf("last user text = %q, want %q; items=%+v", got, "FOLLOWUP", items)
	}
}
