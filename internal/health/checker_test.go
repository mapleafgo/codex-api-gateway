package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// 返回一个返回固定状态码和响应体的 mock /v1/models 服务器。
func mockModelsServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证 Authorization 头存在（不验证具体值）。
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// 返回一个慢速服务器（延迟 delay 后返回 200）。
func slowModelsServer(delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4"}]}`))
	}))
}

func TestCheckSource_Operational(t *testing.T) {
	srv := mockModelsServer(http.StatusOK, `{"data":[{"id":"claude-3"}]}`)
	defer srv.Close()

	c := New(DefaultConfig())
	src := config.Source{Name: "test", BaseURL: srv.URL, APIKey: "sk-test"}
	res := c.CheckSource(context.Background(), src)

	if res.Status != StatusOperational {
		t.Fatalf("期望 operational，得到 %s", res.Status)
	}
	if !res.Success {
		t.Fatal("期望 success=true")
	}
	if res.HTTPStatus != 200 {
		t.Fatalf("期望 http 200，得到 %d", res.HTTPStatus)
	}
	if res.ResponseTimeMs <= 0 {
		t.Fatalf("期望正响应时间，得到 %d", res.ResponseTimeMs)
	}
}

func TestCheckSource_Unauthorized(t *testing.T) {
	srv := mockModelsServer(http.StatusUnauthorized, `{"error":"invalid key"}`)
	defer srv.Close()

	c := New(DefaultConfig())
	src := config.Source{Name: "bad-key", BaseURL: srv.URL, APIKey: "wrong"}
	res := c.CheckSource(context.Background(), src)

	if res.Status != StatusFailed {
		t.Fatalf("期望 failed，得到 %s", res.Status)
	}
	if res.Success {
		t.Fatal("期望 success=false")
	}
	if res.HTTPStatus != 401 {
		t.Fatalf("期望 http 401，得到 %d", res.HTTPStatus)
	}
}

func TestCheckSource_Forbidden(t *testing.T) {
	srv := mockModelsServer(http.StatusForbidden, `{"error":"forbidden"}`)
	defer srv.Close()

	c := New(DefaultConfig())
	src := config.Source{Name: "no-perm", BaseURL: srv.URL, APIKey: "sk-limited"}
	res := c.CheckSource(context.Background(), src)

	if res.Status != StatusFailed {
		t.Fatalf("期望 failed，得到 %s", res.Status)
	}
	if res.HTTPStatus != 403 {
		t.Fatalf("期望 http 403，得到 %d", res.HTTPStatus)
	}
}

func TestCheckSource_ServerError(t *testing.T) {
	srv := mockModelsServer(http.StatusInternalServerError, `{"error":"boom"}`)
	defer srv.Close()

	c := New(DefaultConfig())
	src := config.Source{Name: "server-err", BaseURL: srv.URL, APIKey: "sk-x"}
	res := c.CheckSource(context.Background(), src)

	if res.Status != StatusFailed {
		t.Fatalf("期望 failed，得到 %s", res.Status)
	}
	if res.HTTPStatus != 500 {
		t.Fatalf("期望 http 500，得到 %d", res.HTTPStatus)
	}
}

func TestCheckSource_Unreachable(t *testing.T) {
	// 使用一个肯定不通的端口。
	c := New(DefaultConfig())
	src := config.Source{Name: "dead", BaseURL: "http://127.0.0.1:1", APIKey: "sk-x"}
	res := c.CheckSource(context.Background(), src)

	if res.Status != StatusFailed {
		t.Fatalf("期望 failed，得到 %s", res.Status)
	}
	if res.Success {
		t.Fatal("期望 success=false")
	}
}

func TestCheckSource_Degraded(t *testing.T) {
	// 延迟 600ms，超过默认阈值 5000ms 的 1/10 用于测试。
	srv := slowModelsServer(600 * time.Millisecond)
	defer srv.Close()

	cfg := Config{Timeout: 5 * time.Second, DegradedThreshold: 500}
	c := New(cfg)
	src := config.Source{Name: "slow", BaseURL: srv.URL, APIKey: "sk-x"}
	res := c.CheckSource(context.Background(), src)

	if res.Status != StatusDegraded {
		t.Fatalf("期望 degraded，得到 %s (%s)", res.Status, res.Message)
	}
	if !res.Success {
		t.Fatal("degraded 仍应视为 success=true")
	}
}

func TestCheckAll_Concurrent(t *testing.T) {
	good := mockModelsServer(http.StatusOK, `{"data":[{"id":"gpt-4"}]}`)
	defer good.Close()
	bad := mockModelsServer(http.StatusUnauthorized, `{"error":"bad"}`)
	defer bad.Close()

	c := New(DefaultConfig())
	sources := []config.Source{
		{Name: "good", BaseURL: good.URL, APIKey: "ok"},
		{Name: "bad", BaseURL: bad.URL, APIKey: "wrong"},
	}
	results := c.CheckAll(context.Background(), sources)

	if len(results) != 2 {
		t.Fatalf("期望 2 个结果，得到 %d", len(results))
	}
	if results["good"].Status != StatusOperational {
		t.Fatalf("good 源期望 operational，得到 %s", results["good"].Status)
	}
	if results["bad"].Status != StatusFailed {
		t.Fatalf("bad 源期望 failed，得到 %s", results["bad"].Status)
	}
}

func TestCheckSource_ResultJSON(t *testing.T) {
	// 验证 Result 结构可正确序列化为 JSON（供 admin API 使用）。
	srv := mockModelsServer(http.StatusOK, `{"data":[{"id":"gpt-4"}]}`)
	defer srv.Close()

	c := New(DefaultConfig())
	src := config.Source{Name: "json-test", BaseURL: srv.URL, APIKey: "sk-x"}
	res := c.CheckSource(context.Background(), src)

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("JSON 序列化失败: %v", err)
	}

	var decoded Result
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("JSON 反序列化失败: %v", err)
	}
	if decoded.Status != res.Status {
		t.Fatalf("JSON 往返后 status 不一致: %s vs %s", decoded.Status, res.Status)
	}
}

func TestCheckSource_Models404_FallbackValidKey(t *testing.T) {
    // /v1/models 返回 404，但 /v1/messages 返回 400（说明 key 有效）
    mux := http.NewServeMux()
    mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusNotFound)
    })
    mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
        // 400 = 请求体为空但 key 有效（区别于 401）
        w.WriteHeader(http.StatusBadRequest)
    })
    srv := httptest.NewServer(mux)
    defer srv.Close()

    c := New(DefaultConfig())
    src := config.Source{Name: "no-models", BaseURL: srv.URL, APIKey: "sk-valid"}
    res := c.CheckSource(context.Background(), src)

    if res.Status != StatusOperational {
        t.Fatalf("期望 operational，得到 %s (%s)", res.Status, res.Message)
    }
    if !res.Success {
        t.Fatal("期望 success=true")
    }
}

func TestCheckSource_Models404_FallbackInvalidKey(t *testing.T) {
    // /v1/models 返回 404，/v1/messages 返回 401（key 无效）
    mux := http.NewServeMux()
    mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusNotFound)
    })
    mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusUnauthorized)
    })
    srv := httptest.NewServer(mux)
    defer srv.Close()

    c := New(DefaultConfig())
    src := config.Source{Name: "bad-key-no-models", BaseURL: srv.URL, APIKey: "sk-wrong"}
    res := c.CheckSource(context.Background(), src)

    if res.Status != StatusFailed {
        t.Fatalf("期望 failed，得到 %s (%s)", res.Status, res.Message)
    }
    if res.Success {
        t.Fatal("期望 success=false")
    }
    if res.HTTPStatus != 401 {
        t.Fatalf("期望 http 401，得到 %d", res.HTTPStatus)
    }
}

func TestCheckSource_Models404_ChatBackend(t *testing.T) {
    // Chat 后端：/v1/models 404，降级到 /chat/completions
    mux := http.NewServeMux()
    mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusNotFound)
    })
    mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusBadRequest) // key 有效但 body 为空
    })
    srv := httptest.NewServer(mux)
    defer srv.Close()

    c := New(DefaultConfig())
    src := config.Source{Name: "chat-no-models", BaseURL: srv.URL, APIKey: "sk-valid", BackendType: "c"}
    res := c.CheckSource(context.Background(), src)

    if res.Status != StatusOperational {
        t.Fatalf("期望 operational，得到 %s (%s)", res.Status, res.Message)
    }
    if !res.Success {
        t.Fatal("期望 success=true")
    }
}
