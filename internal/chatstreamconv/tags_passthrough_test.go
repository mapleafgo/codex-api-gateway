package chatstreamconv

import (
	"encoding/json"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// TestContentThinkTagsPassThroughRaw 验证剔除正文思维标签后，content 中的标签文本
// 逐字原样进入 output_text，不产生因标签生成的 reasoning（FR-001/FR-002）。
// 标签字面量用 \x3c 转义构造，避免源码内嵌 HTML 样式标签。
func TestContentThinkTagsPassThroughRaw(t *testing.T) {
	openTag := "\x3cthinking>"
	closeTag := "\x3c/thinking>"
	finish := `{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	cases := []struct {
		name       string
		chunks     []string
		wantText   string
		wantReason int
	}{
		{
			name: "complete tags single chunk",
			chunks: []string{`{"id":"c1","choices":[{"delta":{"content":"` +
				openTag + `内部思考` + closeTag + `最终回答"}}]}`},
			wantText:   openTag + "内部思考" + closeTag + "最终回答",
			wantReason: 0,
		},
		{
			name: "tags split across chunks",
			chunks: []string{
				`{"id":"c1","choices":[{"delta":{"content":"` + openTag[:4] + `"}}]}`,
				`{"id":"c1","choices":[{"delta":{"content":"` + openTag[4:] + `想` + closeTag[:4] + `"}}]}`,
				`{"id":"c1","choices":[{"delta":{"content":"` + closeTag[4:] + `答"}}]}`,
			},
			wantText:   openTag + "想" + closeTag + "答",
			wantReason: 0,
		},
		{
			name: "partial tag at stream end",
			chunks: []string{`{"id":"c1","choices":[{"delta":{"content":"答案` +
				openTag[:6] + `"}}]}`},
			wantText:   "答案" + openTag[:6],
			wantReason: 0,
		},
		{
			name: "native reasoning keeps reasoning and tags raw",
			chunks: []string{`{"id":"c1","choices":[{"delta":{"reasoning_content":"r","content":"` +
				openTag + `x` + closeTag + `y"}}]}`},
			wantText:   openTag + "x" + closeTag + "y",
			wantReason: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			var text, reason string
			var joined string
			var reasoningAdded int
			collect := func(evs []model.SSEEvent) {
				for _, e := range evs {
					joined += string(e.Data)
					typ := evTypes(t, e.Data)
					if evTypeIs(typ, string([]byte("response.output_item.added"))) {
						var m struct {
							Item struct {
								Type string `json:"type"`
							} `json:"item"`
						}
						if err := json.Unmarshal(e.Data, &m); err != nil {
							t.Fatal(err)
						}
						if m.Item.Type == string([]byte("reasoning")) {
							reasoningAdded++
						}
					}
					var m struct {
						Delta string `json:"delta"`
					}
					if evTypeIs(typ, string([]byte("response.output_text.delta"))) {
						if err := json.Unmarshal(e.Data, &m); err != nil {
							t.Fatal(err)
						}
						text += m.Delta
					}
					if evTypeIs(typ, string([]byte("response.reasoning_text.delta"))) {
						if err := json.Unmarshal(e.Data, &m); err != nil {
							t.Fatal(err)
						}
						reason += m.Delta
					}
				}
			}
			for _, ch := range tc.chunks {
				evs, err := c.Feed([]byte(ch))
				if err != nil {
					t.Fatal(err)
				}
				collect(evs)
			}
			evs, err := c.Feed([]byte(finish))
			if err != nil {
				t.Fatal(err)
			}
			collect(evs)
			evs = c.FeedDone()
			collect(evs)
			if text != tc.wantText {
				t.Fatalf("output_text=%q want %q", text, tc.wantText)
			}
			if reasoningAdded != tc.wantReason {
				t.Fatalf("reasoning items=%d want %d: %s", reasoningAdded, tc.wantReason, joined)
			}
			if tc.wantReason == 1 && reason != "r" {
				t.Fatalf("reasoning text=%q want %q", reason, "r")
			}
		})
	}
}
