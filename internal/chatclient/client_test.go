package chatclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/upstreamhttp"
)

func TestChatCompletionsURL(t *testing.T) {
	cases := []struct {
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
	cases := []struct {
		in, want string
	}{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/models"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/models"},
		{"https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4/models"},
		{"https://api.openai.com/v1/models", "https://api.openai.com/v1/models"},
	}
	for _, tc := range cases {
		got := upstreamhttp.ModelsURL(tc.in)
		if got != tc.want {
			t.Errorf("modelsURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStreamBearerAndErrorBody(t *testing.T) {
	var gotAuth, gotAccept string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if strings.Contains(gotBody, `"fail":true`) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\"}\n\n"))
	}))
	defer srv.Close()

	c := New()
	// success
	body, err := c.Stream(context.Background(), srv.URL+"/v1", "sk-test", []byte(`{"model":"m","stream":true}`), nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer body.Close()
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("accept=%q", gotAccept)
	}
	// 4xx
	_, err = c.Stream(context.Background(), srv.URL+"/v1", "sk-test", []byte(`{"fail":true}`), nil)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want 429 error, got %v", err)
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("want body in error, got %v", err)
	}
}

func TestListModelsParsesData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b","display_name":"B"}]}`))
	}))
	defer srv.Close()

	ms, err := New().ListModels(context.Background(), srv.URL+"/v1", "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != "a" || ms[1].DisplayName != "B" {
		t.Fatalf("models=%+v", ms)
	}
}

func TestScanEventsDone(t *testing.T) {
	r := strings.NewReader("data: {\"x\":1}\n\ndata: [DONE]\n\ndata: {\"y\":2}\n\n")
	var chunks []string
	err := ScanEvents(r, func(data []byte) error {
		chunks = append(chunks, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0] != `{"x":1}` {
		t.Fatalf("chunks=%v (must stop at [DONE])", chunks)
	}
}

// TestScanEvents_LargeFrame 单行超过 bufio.Scanner 默认 64KiB 上限时不得断流。
func TestScanEvents_LargeFrame(t *testing.T) {
	big := strings.Repeat("x", 200*1024)
	input := "data: {\"content\":\"" + big + "\"}\n\ndata: [DONE]\n\n"
	var got []string
	err := ScanEvents(strings.NewReader(input), func(data []byte) error {
		got = append(got, string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("大帧不应报错: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0], big) {
		t.Fatalf("大帧应完整送达 onEvent，got %d 条", len(got))
	}
}

// TestScanEvents_NoSpaceAfterColon SSE 规范允许 "data:" 后无空格。
func TestScanEvents_NoSpaceAfterColon(t *testing.T) {
	input := "data:{\"a\":1}\n\ndata:[DONE]\n\n"
	var got []string
	err := ScanEvents(strings.NewReader(input), func(data []byte) error {
		got = append(got, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("无空格形态应被解析，got %v", got)
	}
}
