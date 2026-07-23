package responsesclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/responses"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/responses"},
		{"https://x/v1/responses", "https://x/v1/responses"},
		{"https://x/v1/responses/", "https://x/v1/responses"},
	}
	for _, tc := range cases {
		if got := responsesURL(tc.in); got != tc.want {
			t.Fatalf("in=%q got=%q want=%q", tc.in, got, tc.want)
		}
	}
}

func TestStreamUpstreamError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Fatalf("auth=%s", r.Header.Get("Authorization"))
		}
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer ts.Close()
	_, err := New().Stream(context.Background(), ts.URL+"/v1", "k", []byte(`{"stream":true}`))
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err=%v", err)
	}
}

func TestScanSSE_EventAndTypeFallback(t *testing.T) {
	raw := strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"r1"}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	var types []string
	err := ScanSSE(strings.NewReader(raw), func(et string, data []byte) error {
		types = append(types, et)
		if !json.Valid(data) {
			t.Fatalf("invalid data %s", data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 2 || types[0] != "response.output_text.delta" || types[1] != "response.completed" {
		t.Fatalf("types=%v", types)
	}
}

func TestScanSSE_SkipEmptyType(t *testing.T) {
	raw := "data: {\"foo\":1}\n\n"
	n := 0
	_ = ScanSSE(strings.NewReader(raw), func(et string, data []byte) error {
		n++
		return nil
	})
	if n != 0 {
		t.Fatalf("expected skip, n=%d", n)
	}
}

func TestListModelsParsesData(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b","display_name":"B"}]}`))
	}))
	defer ts.Close()

	ms, err := New().ListModels(context.Background(), ts.URL+"/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != "a" || ms[1].DisplayName != "B" {
		t.Fatalf("models=%+v", ms)
	}
}

func TestScanSSE_LargeFrame(t *testing.T) {
	// 远超默认 Scanner 64KiB，验证 1MiB 缓冲可扫完整帧
	payload := `{"type":"response.completed","response":{"id":"big","model":"m","output":[{"content":[{"text":"` + strings.Repeat("x", 100*1024) + `"}]}]}}`
	raw := "event: response.completed\ndata: " + payload + "\n\n"
	var got []byte
	err := ScanSSE(strings.NewReader(raw), func(et string, data []byte) error {
		if et != "response.completed" {
			t.Fatalf("et=%s", et)
		}
		got = append([]byte(nil), data...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 100*1024 {
		t.Fatalf("frame too small: %d", len(got))
	}
	if !json.Valid(got) {
		t.Fatal("invalid json")
	}
}
