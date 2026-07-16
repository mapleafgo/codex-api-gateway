package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

func TestEnrichFillsToolCallAndThinking(t *testing.T) {
	s := New(0, 0, 0)
	s.Save("resp_1", "official", []model.OutputItem{
		{Type: "reasoning", ID: "rs_0", Summary: []model.OutputText{{Type: "summary_text", Text: "think"}}},
		{Type: "function_call", ID: "fc_0", CallID: "c1", Name: "run", Arguments: "{}"},
	})

	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_1"),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: oairesponses.ResponseInputParam{
				{OfFunctionCallOutput: &oairesponses.ResponseInputItemFunctionCallOutputParam{
					CallID: "c1",
					Output: oairesponses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: oparam.NewOpt("ok"),
					},
				}},
			},
		},
	}
	_ = s.Enrich(req, "official")
	if len(req.Input.OfInputItemList) != 3 {
		t.Fatalf("want 3 items after enrich, got %d: %+v", len(req.Input.OfInputItemList), req.Input.OfInputItemList)
	}
	// Check ordering: reasoning, function_call, function_call_output
	if req.Input.OfInputItemList[0].OfReasoning == nil {
		t.Fatalf("expected reasoning first")
	}
	if req.Input.OfInputItemList[1].OfFunctionCall == nil {
		t.Fatalf("expected function_call second")
	}
	if req.Input.OfInputItemList[2].OfFunctionCallOutput == nil {
		t.Fatalf("expected function_call_output third")
	}
}

func TestEnrichFillsCustomToolCall(t *testing.T) {
	s := New(0, 0, 0)
	s.Save("resp_1", "official", []model.OutputItem{
		{Type: "custom_tool_call", ID: "ctc_0", CallID: "c1", Name: "apply_patch", Input: "*** Begin Patch"},
	})

	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_1"),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: oairesponses.ResponseInputParam{
				{OfCustomToolCallOutput: &oairesponses.ResponseCustomToolCallOutputParam{
					CallID: "c1",
					Output: oairesponses.ResponseCustomToolCallOutputOutputUnionParam{
						OfString: oparam.NewOpt("ok"),
					},
				}},
			},
		},
	}
	_ = s.Enrich(req, "official")
	if len(req.Input.OfInputItemList) != 2 {
		t.Fatalf("want 2 items after enrich, got %d: %+v", len(req.Input.OfInputItemList), req.Input.OfInputItemList)
	}
	call := req.Input.OfInputItemList[0].OfCustomToolCall
	if call == nil {
		t.Fatalf("expected custom_tool_call first: %+v", req.Input.OfInputItemList[0])
	}
	if call.CallID != "c1" || call.Name != "apply_patch" || call.Input != "*** Begin Patch" {
		t.Fatalf("bad custom_tool_call: %+v", call)
	}
	if req.Input.OfInputItemList[1].OfCustomToolCallOutput == nil {
		t.Fatalf("expected custom_tool_call_output second")
	}
}

func TestEnrichPreservesAssistantPhase(t *testing.T) {
	s := New(0, 0, 0)
	s.Save("resp_1", "official", []model.OutputItem{
		{
			Type:  "message",
			ID:    "msg_0",
			Role:  "assistant",
			Phase: "final_answer",
			Content: []model.OutputText{{
				Type: "output_text",
				Text: "done",
			}},
		},
	})

	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_1"),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfString: oparam.NewOpt("next"),
		},
	}
	_ = s.Enrich(req, "official")
	if len(req.Input.OfInputItemList) < 1 {
		t.Fatalf("expected enriched assistant message")
	}
	msg := req.Input.OfInputItemList[0].OfMessage
	if msg == nil {
		t.Fatalf("expected message input item: %+v", req.Input.OfInputItemList[0])
	}
	if string(msg.Phase) != "final_answer" {
		t.Fatalf("assistant phase not preserved: %+v", msg)
	}
}

func TestSaveResponseAndEnrichPreservesRefusalMessage(t *testing.T) {
	s := New(0, 0, 0)
	refusal := "I can't help with that."
	s.SaveResponse("resp_refusal", "official", &oairesponses.ResponseNewParams{
		Input: oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("unsafe request")},
	}, []model.OutputItem{{
		Type:  model.ItemTypeMessage,
		ID:    "msg_refusal",
		Role:  model.RoleAssistant,
		Phase: model.AssistantPhaseFinalAnswer,
		Content: []model.OutputText{{
			Type:    model.ContentTypeRefusal,
			Refusal: &refusal,
		}},
	}})

	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_refusal"),
		Input:              oairesponses.ResponseNewParamsInputUnion{OfString: oparam.NewOpt("next request")},
	}
	s.Enrich(req, "official")

	if len(req.Input.OfInputItemList) != 3 {
		t.Fatalf("want previous input, refusal, and current input; got %d items", len(req.Input.OfInputItemList))
	}
	if got := messageText(req.Input.OfInputItemList[1]); got != "I can't help with that." {
		t.Fatalf("replayed refusal message = %q, want refusal text", got)
	}
}

func TestEnrichRoundTripsUnhandledInputItemRaw(t *testing.T) {
	s := New(0, 0, 0)
	compaction := oairesponses.ResponseInputItemParamOfCompaction("sealed-context")
	s.SaveContext("resp_1", "official", []oairesponses.ResponseInputItemUnionParam{compaction}, nil)

	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_1"),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfString: oparam.NewOpt("next"),
		},
	}
	_ = s.Enrich(req, "official")
	if len(req.Input.OfInputItemList) < 1 {
		t.Fatalf("expected enriched raw item")
	}
	b, err := json.Marshal(req.Input.OfInputItemList[0])
	if err != nil {
		t.Fatalf("marshal enriched item: %v", err)
	}
	if !strings.Contains(string(b), `"type":"compaction"`) || !strings.Contains(string(b), `"encrypted_content":"sealed-context"`) {
		t.Fatalf("raw compaction item not round-tripped: %s", b)
	}
}

func TestEnrichRoundTripsShellCallRaw(t *testing.T) {
	s := New(0, 0, time.Hour)
	input := []oairesponses.ResponseInputItemUnionParam{{
		OfShellCall: &oairesponses.ResponseInputItemShellCallParam{
			CallID: "call_shell",
			Action: oairesponses.ResponseInputItemShellCallActionParam{
				Commands: []string{"echo hi"},
			},
		},
	}}
	s.SaveContext("resp_1", "src", input, nil)

	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_1"),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfString: oparam.NewOpt("next"),
		},
	}
	s.Enrich(req, "src")
	if req.Input.OfInputItemList[0].OfShellCall == nil {
		t.Fatalf("shell_call was not replayed: %+v", req.Input.OfInputItemList[0])
	}
}

func TestEnrichRoundTripsShellAndApplyPatchRawItems(t *testing.T) {
	tests := []struct {
		name  string
		input oairesponses.ResponseInputItemUnionParam
		check func(t *testing.T, item oairesponses.ResponseInputItemUnionParam)
	}{
		{
			name: "local_shell_call",
			input: oairesponses.ResponseInputItemUnionParam{
				OfLocalShellCall: &oairesponses.ResponseInputItemLocalShellCallParam{
					ID:     "local_shell_1",
					CallID: "call_local_shell",
					Action: oairesponses.ResponseInputItemLocalShellCallActionParam{
						Command: []string{"echo", "hi"},
					},
				},
			},
			check: func(t *testing.T, item oairesponses.ResponseInputItemUnionParam) {
				t.Helper()
				if item.OfLocalShellCall == nil || item.OfLocalShellCall.CallID != "call_local_shell" {
					t.Fatalf("local_shell_call was not replayed: %+v", item)
				}
			},
		},
		{
			name: "local_shell_call_output",
			input: oairesponses.ResponseInputItemUnionParam{
				OfLocalShellCallOutput: &oairesponses.ResponseInputItemLocalShellCallOutputParam{
					ID:     "call_local_shell",
					Output: "local ok",
				},
			},
			check: func(t *testing.T, item oairesponses.ResponseInputItemUnionParam) {
				t.Helper()
				if item.OfLocalShellCallOutput == nil || item.OfLocalShellCallOutput.ID != "call_local_shell" {
					t.Fatalf("local_shell_call_output was not replayed: %+v", item)
				}
			},
		},
		{
			name: "shell_call",
			input: oairesponses.ResponseInputItemUnionParam{
				OfShellCall: &oairesponses.ResponseInputItemShellCallParam{
					CallID: "call_shell",
					Action: oairesponses.ResponseInputItemShellCallActionParam{
						Commands: []string{"echo hi"},
					},
				},
			},
			check: func(t *testing.T, item oairesponses.ResponseInputItemUnionParam) {
				t.Helper()
				if item.OfShellCall == nil || item.OfShellCall.CallID != "call_shell" {
					t.Fatalf("shell_call was not replayed: %+v", item)
				}
			},
		},
		{
			name: "shell_call_output",
			input: oairesponses.ResponseInputItemUnionParam{
				OfShellCallOutput: &oairesponses.ResponseInputItemShellCallOutputParam{
					CallID: "call_shell",
					Output: []oairesponses.ResponseFunctionShellCallOutputContentParam{{
						Stdout: "ok",
						Stderr: "warn",
					}},
				},
			},
			check: func(t *testing.T, item oairesponses.ResponseInputItemUnionParam) {
				t.Helper()
				if item.OfShellCallOutput == nil || item.OfShellCallOutput.CallID != "call_shell" {
					t.Fatalf("shell_call_output was not replayed: %+v", item)
				}
				if got := item.OfShellCallOutput.Output[0]; got.Stdout != "ok" || got.Stderr != "warn" {
					t.Fatalf("shell_call_output lost stdout/stderr: %+v", got)
				}
			},
		},
		{
			name: "apply_patch_call",
			input: oairesponses.ResponseInputItemUnionParam{
				OfApplyPatchCall: &oairesponses.ResponseInputItemApplyPatchCallParam{
					CallID: "call_patch",
					Status: "completed",
					Operation: oairesponses.ResponseInputItemApplyPatchCallOperationUnionParam{
						OfUpdateFile: &oairesponses.ResponseInputItemApplyPatchCallOperationUpdateFileParam{
							Path: "README.md",
							Diff: "*** Begin Patch\n*** Update File: README.md\n@@\n-old\n+new\n*** End Patch\n",
						},
					},
				},
			},
			check: func(t *testing.T, item oairesponses.ResponseInputItemUnionParam) {
				t.Helper()
				if item.OfApplyPatchCall == nil || item.OfApplyPatchCall.CallID != "call_patch" {
					t.Fatalf("apply_patch_call was not replayed: %+v", item)
				}
				if diff := item.OfApplyPatchCall.Operation.GetDiff(); diff == nil || !strings.Contains(*diff, "*** Begin Patch") {
					t.Fatalf("apply_patch_call lost diff: %+v", item.OfApplyPatchCall.Operation)
				}
			},
		},
		{
			name: "apply_patch_call_output",
			input: oairesponses.ResponseInputItemUnionParam{
				OfApplyPatchCallOutput: &oairesponses.ResponseInputItemApplyPatchCallOutputParam{
					CallID: "call_patch",
					Status: "completed",
					Output: oparam.NewOpt("Done"),
				},
			},
			check: func(t *testing.T, item oairesponses.ResponseInputItemUnionParam) {
				t.Helper()
				if item.OfApplyPatchCallOutput == nil || item.OfApplyPatchCallOutput.CallID != "call_patch" {
					t.Fatalf("apply_patch_call_output was not replayed: %+v", item)
				}
				if !item.OfApplyPatchCallOutput.Output.Valid() || item.OfApplyPatchCallOutput.Output.Value != "Done" {
					t.Fatalf("apply_patch_call_output lost output: %+v", item.OfApplyPatchCallOutput.Output)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(0, 0, time.Hour)
			s.SaveContext("resp_"+tt.name, "src", []oairesponses.ResponseInputItemUnionParam{tt.input}, nil)
			req := &oairesponses.ResponseNewParams{
				PreviousResponseID: oparam.NewOpt("resp_" + tt.name),
				Input: oairesponses.ResponseNewParamsInputUnion{
					OfString: oparam.NewOpt("next"),
				},
			}
			s.Enrich(req, "src")
			if len(req.Input.OfInputItemList) == 0 {
				t.Fatalf("expected replayed item")
			}
			tt.check(t, req.Input.OfInputItemList[0])
		})
	}
}

func TestEnrichPrependsStoredInputAndOutputContext(t *testing.T) {
	s := New(0, 0, 0)
	s.SaveContext("resp_1", "official", []oairesponses.ResponseInputItemUnionParam{
		messageInput("user", "first question"),
	}, []model.OutputItem{
		{Type: "message", Role: "assistant", Content: []model.OutputText{{Type: "output_text", Text: "first answer"}}},
	})

	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_1"),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: oairesponses.ResponseInputParam{
				messageInput("user", "second question"),
			},
		},
	}

	_ = s.Enrich(req, "official")

	if got := len(req.Input.OfInputItemList); got != 3 {
		t.Fatalf("want previous input + output + new input, got %d items: %+v", got, req.Input.OfInputItemList)
	}
	if got := messageText(req.Input.OfInputItemList[0]); got != "first question" {
		t.Fatalf("first item text = %q, want first question", got)
	}
	if got := messageText(req.Input.OfInputItemList[1]); got != "first answer" {
		t.Fatalf("second item text = %q, want first answer", got)
	}
	if got := messageText(req.Input.OfInputItemList[2]); got != "second question" {
		t.Fatalf("third item text = %q, want second question", got)
	}
}

func TestOpenPersistsContextAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 0, 0, time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.SaveContext("resp_1", "official", []oairesponses.ResponseInputItemUnionParam{
		messageInput("user", "first question"),
	}, []model.OutputItem{
		{Type: "message", Role: "assistant", Content: []model.OutputText{{Type: "output_text", Text: "first answer"}}},
	})
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(dir, 0, 0, time.Hour)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_1"),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: oairesponses.ResponseInputParam{
				messageInput("user", "second question"),
			},
		},
	}
	_ = reopened.Enrich(req, "official")

	if got := len(req.Input.OfInputItemList); got != 3 {
		t.Fatalf("want persisted previous input + output + new input, got %d items", got)
	}
	if got := messageText(req.Input.OfInputItemList[0]); got != "first question" {
		t.Fatalf("first item text = %q, want first question", got)
	}
	if got := messageText(req.Input.OfInputItemList[1]); got != "first answer" {
		t.Fatalf("second item text = %q, want first answer", got)
	}
}

func TestOpenUsesBadgerTTL(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 0, 0, 2*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.Save("resp_1", "official", textItems("old", 10))
	time.Sleep(3 * time.Second)
	if _, ok := s.Get("resp_1"); ok {
		t.Fatalf("badger TTL should make expired entry unretrievable")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestEnrichDropsThinkingCrossSource(t *testing.T) {
	s := New(0, 0, 0)
	s.Save("resp_1", "official", []model.OutputItem{
		{Type: "reasoning", ID: "rs_0", Summary: []model.OutputText{{Type: "summary_text", Text: "think"}}},
		{Type: "function_call", ID: "fc_0", CallID: "c1", Name: "run"},
	})
	req := &oairesponses.ResponseNewParams{
		PreviousResponseID: oparam.NewOpt("resp_1"),
		Input: oairesponses.ResponseNewParamsInputUnion{
			OfInputItemList: oairesponses.ResponseInputParam{
				{OfFunctionCallOutput: &oairesponses.ResponseInputItemFunctionCallOutputParam{
					CallID: "c1",
					Output: oairesponses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: oparam.NewOpt("ok"),
					},
				}},
			},
		},
	}
	_ = s.Enrich(req, "other")
	hasReason := false
	hasCall := false
	for _, it := range req.Input.OfInputItemList {
		if it.OfReasoning != nil {
			hasReason = true
		}
		if it.OfFunctionCall != nil {
			hasCall = true
		}
	}
	if hasReason {
		t.Fatalf("cross-source thinking should be dropped")
	}
	if !hasCall {
		t.Fatalf("tool_call must be kept across sources")
	}
}

func TestSaveEvictsLeastRecentlyUsedWhenMaxBytesExceeded(t *testing.T) {
	s := New(850, 0, 0)
	s.Save("resp_a", "official", textItems("a", 100))
	s.Save("resp_b", "official", textItems("b", 100))

	if _, ok := s.Get("resp_a"); !ok {
		t.Fatalf("expected resp_a before eviction")
	}

	s.Save("resp_c", "official", textItems("c", 100))

	if _, ok := s.Get("resp_a"); !ok {
		t.Fatalf("recently used resp_a should be retained")
	}
	if _, ok := s.Get("resp_b"); ok {
		t.Fatalf("least recently used resp_b should be evicted")
	}
	if _, ok := s.Get("resp_c"); !ok {
		t.Fatalf("new resp_c should be retained")
	}
}

func TestSaveSkipsEntryLargerThanMaxEntryBytes(t *testing.T) {
	s := New(0, 120, 0)

	s.Save("resp_big", "official", textItems("big", 200))

	if _, ok := s.Get("resp_big"); ok {
		t.Fatalf("entry larger than max_entry_bytes should not be stored")
	}
}

func TestGetReturnsIndependentEntryCopy(t *testing.T) {
	s := New(0, 0, 0)
	s.Save("resp_1", "official", textItems("hello", 5))

	entry, ok := s.Get("resp_1")
	if !ok {
		t.Fatalf("expected stored entry")
	}
	entry.Items[0].Content[0].Text = "mutated"

	got, ok := s.Get("resp_1")
	if !ok {
		t.Fatalf("expected stored entry after mutation")
	}
	if got.Items[0].Content[0].Text == "mutated" {
		t.Fatalf("Get must return a copy, not the stored slice")
	}
}

func textItems(id string, n int) []model.OutputItem {
	return []model.OutputItem{{
		Type:    "message",
		ID:      id,
		Role:    "assistant",
		Content: []model.OutputText{{Type: "output_text", Text: strings.Repeat("x", n)}},
	}}
}

func messageInput(role, text string) oairesponses.ResponseInputItemUnionParam {
	return oairesponses.ResponseInputItemUnionParam{
		OfMessage: &oairesponses.EasyInputMessageParam{
			Role: oairesponses.EasyInputMessageRole(role),
			Content: oairesponses.EasyInputMessageContentUnionParam{
				OfString: oparam.NewOpt(text),
			},
		},
	}
}

func messageText(item oairesponses.ResponseInputItemUnionParam) string {
	if item.OfMessage == nil {
		return ""
	}
	return item.OfMessage.Content.OfString.Value
}

func TestDeleteRemovesEntry(t *testing.T) {
	s := New(0, 0, 0)
	s.Save("resp_1", "official", textItems("hello", 5))

	if _, ok := s.Get("resp_1"); !ok {
		t.Fatalf("expected entry before delete")
	}
	s.Delete("resp_1")
	if _, ok := s.Get("resp_1"); ok {
		t.Fatalf("entry should be gone after delete")
	}
}

func TestDeleteFreesBytesForEviction(t *testing.T) {
	s := New(850, 0, 0)
	s.Save("resp_a", "official", textItems("a", 100))
	s.Save("resp_b", "official", textItems("b", 100))
	s.Delete("resp_a")

	s.Save("resp_c", "official", textItems("c", 100))

	if _, ok := s.Get("resp_b"); !ok {
		t.Fatalf("resp_b should survive when resp_a was deleted")
	}
	if _, ok := s.Get("resp_c"); !ok {
		t.Fatalf("resp_c should be stored")
	}
}
