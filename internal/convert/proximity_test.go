package convert

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func tuBlock(id string) anthropic.ContentBlockParamUnion {
	return anthropic.ContentBlockParamUnion{OfToolUse: &anthropic.ToolUseBlockParam{ID: id, Name: "fn", Input: map[string]any{}}}
}

func trBlock(id string) anthropic.ContentBlockParamUnion {
	return anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{ToolUseID: id}}
}

func txtBlock(s string) anthropic.ContentBlockParamUnion {
	return anthropic.ContentBlockParamUnion{OfText: &anthropic.TextBlockParam{Text: s}}
}

func aMsg(blocks ...anthropic.ContentBlockParamUnion) anthropic.MessageParam {
	return anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant, Content: blocks}
}

func uMsg(blocks ...anthropic.ContentBlockParamUnion) anthropic.MessageParam {
	return anthropic.MessageParam{Role: anthropic.MessageParamRoleUser, Content: blocks}
}

// assertProximity 校验修复后的不变量：每条 assistant 的每个 tool_use，
// 其 tool_result 必须出现在紧接的下一条消息中；且不存在空 content 消息。
func assertProximity(t *testing.T, msgs []anthropic.MessageParam) {
	t.Helper()
	for i := range msgs {
		if len(msgs[i].Content) == 0 {
			t.Fatalf("msg %d content 为空", i)
		}
		if msgs[i].Role != anthropic.MessageParamRoleAssistant {
			continue
		}
		for _, b := range msgs[i].Content {
			if b.OfToolUse == nil {
				continue
			}
			id := b.OfToolUse.ID
			found := false
			if i+1 < len(msgs) {
				for _, nb := range msgs[i+1].Content {
					if nb.OfToolResult != nil && nb.OfToolResult.ToolUseID == id {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("tool_use %q 未在紧邻的下一条消息中闭环（msg %d）", id, i)
			}
		}
	}
}

func countToolUse(msgs []anthropic.MessageParam) int {
	n := 0
	for i := range msgs {
		for _, b := range msgs[i].Content {
			if b.OfToolUse != nil {
				n++
			}
		}
	}
	return n
}

func TestEnsureToolResultProximity(t *testing.T) {
	cases := []struct {
		name string
		msgs []anthropic.MessageParam
	}{
		{
			name: "已合规不动",
			msgs: []anthropic.MessageParam{
				uMsg(txtBlock("u0")),
				aMsg(tuBlock("t1")),
				uMsg(trBlock("t1")),
			},
		},
		{
			name: "单迁移：结果在更远的 user",
			msgs: []anthropic.MessageParam{
				uMsg(txtBlock("u0")),
				aMsg(tuBlock("t1"), tuBlock("t2")),
				uMsg(trBlock("t1"), txtBlock("mid")),
				aMsg(txtBlock("say")),
				uMsg(trBlock("t2")),
			},
		},
		{
			name: "多批迁移：第一批插入不得让后续 assistant 被跳过",
			// 修复前实现的已知漏修形态：A1 的 tuB 前插到 u2 后，
			// A2 的后继变成新 assistant，A2 被跳过，tuD 留在 u2 前而 rD 在 u3。
			msgs: []anthropic.MessageParam{
				uMsg(txtBlock("u0")),
				aMsg(tuBlock("tA"), tuBlock("tB")),
				uMsg(trBlock("tA")),
				aMsg(tuBlock("tC"), tuBlock("tD")),
				uMsg(trBlock("tC"), trBlock("tB")),
				aMsg(txtBlock("say")),
				uMsg(trBlock("tD")),
			},
		},
		{
			name: "assistant 清空整条移除",
			msgs: []anthropic.MessageParam{
				aMsg(tuBlock("t1")),
				uMsg(txtBlock("unrelated")),
				uMsg(trBlock("t1")),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &anthropic.MessageNewParams{Messages: tc.msgs}
			before := countToolUse(out.Messages)
			ensureToolResultProximity(out)
			// 与生产调用序一致：proximity 之后必跑 coalesce。
			coalesceSameRoleMessages(out)
			if got := countToolUse(out.Messages); got != before {
				t.Fatalf("tool_use 数量不守恒：before=%d after=%d", before, got)
			}
			assertProximity(t, out.Messages)
		})
	}
}

// TestDecodeHostedToolChoiceRestored SDK union 会把 hosted tool_choice JSON
// 误解到 OfAllowedTools；DecodeResponseNewParams 必须从 raw 恢复为 OfHostedTool。
func TestDecodeHostedToolChoiceRestored(t *testing.T) {
	for _, typ := range []string{"web_search", "web_search_preview", "code_interpreter"} {
		req, err := DecodeResponseNewParams([]byte(
			`{"model":"gpt-5","input":"hi","stream":true,"tool_choice":{"type":"` + typ + `"}}`))
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if req.ToolChoice.OfHostedTool == nil || string(req.ToolChoice.OfHostedTool.Type) != typ {
			t.Fatalf("%s: 应恢复为 OfHostedTool，got %+v", typ, req.ToolChoice)
		}
	}
}

// TestDecodeMcpToolChoiceRestored mcp tool_choice 恢复为 OfMcpTool 并保留 server_label/name。
func TestDecodeMcpToolChoiceRestored(t *testing.T) {
	req, err := DecodeResponseNewParams([]byte(
		`{"model":"gpt-5","input":"hi","stream":true,"tool_choice":{"type":"mcp","server_label":"srv","name":"fn"}}`))
	if err != nil {
		t.Fatal(err)
	}
	mcp := req.ToolChoice.OfMcpTool
	if mcp == nil || mcp.ServerLabel != "srv" || mcp.Name.Value != "fn" {
		t.Fatalf("应恢复为 OfMcpTool(srv/fn)，got %+v", req.ToolChoice)
	}
}
