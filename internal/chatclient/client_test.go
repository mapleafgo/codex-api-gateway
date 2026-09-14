package chatclient

import (
	"context"
	"encoding/json"
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

// TestScanEvents_MultiLineDataJoined 复现「chat 输出不完整」的一个根因：
// SSE 允许同一事件的多个 data: 行，按规范需以 "\n" 拼接后才是完整 JSON。
// 逐行单独回调会把上游拆行的合法 JSON 当截断，json.Unmarshal 失败并丢掉该事件
// 内容（表现为上游有内容、客户端却缺一段）。
func TestScanEvents_MultiLineDataJoined(t *testing.T) {
	// 一个事件跨两行 data:（结构换行，JSON 允许 token 间空白）。
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"line1\\n\\\"quoted\\\"\"\n" +
		"data: }}]}\n\n" +
		"data: [DONE]\n\n"
	var got []string
	err := ScanEvents(strings.NewReader(input), func(data []byte) error {
		got = append(got, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("多行 data 帧应拼接为 1 个事件，实际 %d 个: %q", len(got), got)
	}
	// 拼接结果必须是合法 JSON，且换行保留在事件边界处。
	want := "{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"line1\\n\\\"quoted\\\"\"\n}}]}"
	if got[0] != want {
		t.Fatalf("拼接结果 = %q, want %q", got[0], want)
	}
}

// TestScanEvents_MultiLineDataAlwaysValidJSON 覆盖更一般的上游拆行形态：
// pretty-print 的 JSON 被逐行加 data: 前缀。拼接后必须能完整反序列化。
func TestScanEvents_MultiLineDataAlwaysValidJSON(t *testing.T) {
	prettyJSON := "{\n  \"id\": \"chatcmpl-1\",\n  \"choices\": [{\"index\": 0, \"delta\": {\"content\": \"你好\"}}]\n}"
	var sb strings.Builder
	for _, l := range strings.Split(prettyJSON, "\n") {
		sb.WriteString("data: ")
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	sb.WriteString("\ndata: [DONE]\n\n")

	var got []string
	if err := ScanEvents(strings.NewReader(sb.String()), func(data []byte) error {
		got = append(got, string(data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("多行帧应拼接为 1 个事件，实际 %d 个", len(got))
	}
	var parsed struct {
		ID      string `json:"id"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(got[0]), &parsed); err != nil {
		t.Fatalf("拼接结果应为合法 JSON: %v (raw=%q)", err, got[0])
	}
	if parsed.ID != "chatcmpl-1" || parsed.Choices[0].Delta.Content != "你好" {
		t.Fatalf("字段丢失: %+v", parsed)
	}
}

// TestScanEvents_NoBlankLineSeparator 覆盖不带空行分隔、每行一个事件的上游：
// 多行拼接逻辑不得把它们误拼成一个非法 JSON，必须逐帧交付。
func TestScanEvents_NoBlankLineSeparator(t *testing.T) {
	body := "data: {\"a\":1}\ndata: {\"b\":2}\ndata: [DONE]\n"
	var got []string
	err := ScanEvents(strings.NewReader(body), func(data []byte) error {
		got = append(got, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("无空行分隔应逐帧交付，got %q", got)
	}
}
