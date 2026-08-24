package backend

import (
	"fmt"
	"testing"
)

func TestContextLengthExceededCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"wechat context length 400", fmt.Errorf(`upstream 400: {"message":"The input (293789 tokens) is longer than the model's context length (262144 tokens)."}`), true},
		{"openai response error code", fmt.Errorf(`{"error":{"message":"Your input exceeds the context window of this model.","code":"context_length_exceeded"}}`), true},
		{"anthropic stop reason", fmt.Errorf("stop_reason model_context_window_exceeded"), true},
		{"anthropic prompt too long", fmt.Errorf(`upstream 400: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens > 200000 max tokens"}}`), true},
		{"jd tool pairing", fmt.Errorf(`upstream 400: {"code":400001,"message":"insufficient tool messages following tool_calls message"}`), false},
		{"rate limited", fmt.Errorf("upstream 429: rate limit reached"), false},
		{"network error", fmt.Errorf("dial tcp: connection refused"), false},
	}
	for _, tc := range cases {
		got := ContextLengthExceededCode(tc.err)
		if (got != "") != tc.want {
			t.Fatalf("%s: ContextLengthExceededCode(%v)=%q want presence %v", tc.name, tc.err, got, tc.want)
		}
	}
}
