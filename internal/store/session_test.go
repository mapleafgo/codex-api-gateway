package store

import (
	"strconv"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

func TestEnrichFillsToolCallAndThinking(t *testing.T) {
	s := New(1000, 0)
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

func TestSaveWithMaxEntriesNegativeKeepsEntry(t *testing.T) {
	s := New(-1, 0) // max<0 means unlimited
	s.Save("resp_1", "official", []model.OutputItem{
		{Type: "message", Role: "assistant", Content: []model.OutputText{{Type: "output_text", Text: "hello"}}},
	})
	entry, ok := s.Get("resp_1")
	if !ok {
		t.Fatal("entry should be retained when MaxEntries<0 (unlimited)")
	}
	if len(entry.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(entry.Items))
	}
}

func TestSaveWithMaxEntriesZeroUsesDefaultLimit(t *testing.T) {
	s := New(0, 0)
	for i := 0; i <= DefaultMaxEntries; i++ {
		s.Save("resp_"+strconv.Itoa(i), "official", []model.OutputItem{
			{Type: "message", Role: "assistant"},
		})
	}
	var missing int
	for i := 0; i <= DefaultMaxEntries; i++ {
		if _, ok := s.Get("resp_" + strconv.Itoa(i)); !ok {
			missing++
		}
	}
	if missing == 0 {
		t.Fatalf("at least one entry should be evicted when MaxEntries=0 uses default limit")
	}
}

func TestEnrichDropsThinkingCrossSource(t *testing.T) {
	s := New(1000, 0)
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
