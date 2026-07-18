package streamconv

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// TestSummarizedAddedEventCarriesSummary 锁定 summarized 模式下 reasoning 的
// output_item.added 事件 item 必须含 "summary" 字段（即使空数组）。
// 这是端到端回归：model.OutputItem 的 reasoning 分支强制输出 summary，确保
// Codex serde 能解析 reasoning 变体并设置 active_item（否则会报
// "ReasoningSummaryPartAdded without active item"，且后续 summary 事件全部丢失）。
func TestSummarizedAddedEventCarriesSummary(t *testing.T) {
	c := New()
	c.SetSummarized(true)
	c.Feed(&anthropic.MessageStreamEventUnion{Type: "message_start", Message: anthropic.Message{ID: "m", Model: "x"}})
	start, _ := c.Feed(&anthropic.MessageStreamEventUnion{
		Type: "content_block_start", Index: 0,
		ContentBlock: anthropic.ContentBlockStartEventContentBlockUnion{Type: "thinking"},
	})

	var addedData []byte
	for _, e := range start {
		if e.Type == "response.output_item.added" {
			addedData = e.Data
			break
		}
	}
	if addedData == nil {
		t.Fatal("no output_item.added event emitted on thinking block start")
	}
	var ev struct {
		Item struct {
			Type    string `json:"type"`
			Summary []any  `json:"summary"`
		} `json:"item"`
	}
	if err := json.Unmarshal(addedData, &ev); err != nil {
		t.Fatalf("unmarshal added event: %v", err)
	}
	if ev.Item.Type != "reasoning" {
		t.Fatalf("want reasoning item, got %q: %s", ev.Item.Type, addedData)
	}
	if ev.Item.Summary == nil {
		t.Fatalf("added reasoning item must carry summary field (required by OpenAI wire & Codex serde), got %s", addedData)
	}
}
