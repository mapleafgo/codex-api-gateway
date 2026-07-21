package chatclient

import "testing"

func TestChatCompletionsURL(t *testing.T) {
	cases := []struct{
		in, want string
	}{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/chat/completions"},
		{"https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4/chat/completions"},
		{"https://ark.cn-beijing.volces.com/api/v3", "https://ark.cn-beijing.volces.com/api/v3/chat/completions"},
		{"https://dashscope.aliyuncs.com/compatible-mode/v1", "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"},
		{"https://api.deepseek.com", "https://api.deepseek.com/chat/completions"},
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/chat/completions/", "https://api.openai.com/v1/chat/completions"},
	}
	for _, tc := range cases {
		got := chatCompletionsURL(tc.in)
		if got != tc.want {
			t.Errorf("chatCompletionsURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestModelsURL(t *testing.T) {
	cases := []struct{
		in, want string
	}{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/models"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/models"},
		{"https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4/models"},
		{"https://api.openai.com/v1/models", "https://api.openai.com/v1/models"},
	}
	for _, tc := range cases {
		got := modelsURL(tc.in)
		if got != tc.want {
			t.Errorf("modelsURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
