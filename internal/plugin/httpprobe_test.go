package plugin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// newProbeServer 启动一个可编程响应 /v1/models 与降级 POST 端点的测试服务器。
func newProbeServer(modelsStatus, fallbackStatus int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.WriteHeader(modelsStatus)
			return
		}
		w.WriteHeader(fallbackStatus)
	}))
}

func TestHTTPProbeOperational(t *testing.T) {
	srv := newProbeServer(http.StatusOK, http.StatusOK)
	defer srv.Close()

	res := HTTPProbe(context.Background(), config.Source{BaseURL: srv.URL, APIKey: "k"},
		ProbeHTTPConfig{ModelsURL: srv.URL + "/models", FallbackPostURL: srv.URL + "/ping"})
	if res.Status != ProbeOperational || res.Code != http.StatusOK {
		t.Fatalf("status=%q code=%d want operational/200", res.Status, res.Code)
	}
}

func TestHTTPProbeClassifiesUnauthorizedForbiddenServerError(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantMsg string
	}{
		{"unauthorized", http.StatusUnauthorized, "API Key 无效 (401)"},
		{"forbidden", http.StatusForbidden, "API Key 无权限 (403)"},
		{"server-error", http.StatusInternalServerError, "上游服务错误 (500)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newProbeServer(tc.status, http.StatusOK)
			defer srv.Close()
			res := HTTPProbe(context.Background(), config.Source{BaseURL: srv.URL, APIKey: "k"},
				ProbeHTTPConfig{ModelsURL: srv.URL + "/models", FallbackPostURL: srv.URL + "/ping"})
			if res.Status != ProbeFailed || res.Message != tc.wantMsg {
				t.Fatalf("status=%q msg=%q want failed/%q", res.Status, res.Message, tc.wantMsg)
			}
		})
	}
}

func TestHTTPProbeDegradedOnSlowResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := HTTPProbe(context.Background(), config.Source{BaseURL: srv.URL, APIKey: "k"},
		ProbeHTTPConfig{ModelsURL: srv.URL + "/models", DegradedMS: 10})
	if res.Status != ProbeDegraded {
		t.Fatalf("status=%q want degraded", res.Status)
	}
}

func TestHTTPProbeModels404FallsBackValidKey(t *testing.T) {
	srv := newProbeServer(http.StatusNotFound, http.StatusBadRequest)
	defer srv.Close()

	res := HTTPProbe(context.Background(), config.Source{BaseURL: srv.URL, APIKey: "k"},
		ProbeHTTPConfig{ModelsURL: srv.URL + "/models", FallbackPostURL: srv.URL + "/ping"})
	if res.Status != ProbeOperational || res.Message != "正常（/v1/models 未实现，已降级验证）" {
		t.Fatalf("status=%q msg=%q want operational fallback", res.Status, res.Message)
	}
}

func TestHTTPProbeModels404FallbackInvalidKey(t *testing.T) {
	srv := newProbeServer(http.StatusNotFound, http.StatusUnauthorized)
	defer srv.Close()

	res := HTTPProbe(context.Background(), config.Source{BaseURL: srv.URL, APIKey: "bad"},
		ProbeHTTPConfig{ModelsURL: srv.URL + "/models", FallbackPostURL: srv.URL + "/ping"})
	if res.Status != ProbeFailed || res.Message != "API Key 无效 (401)" {
		t.Fatalf("status=%q msg=%q want failed invalid key", res.Status, res.Message)
	}
}

func TestHTTPProbeAppliesSourceHeaders(t *testing.T) {
	var gotAuth, gotXKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	HTTPProbe(context.Background(), config.Source{
		APIKey:  "bearer-key",
		Headers: map[string]string{"X-Api-Key": "custom"},
	}, ProbeHTTPConfig{
		ModelsURL:       srv.URL + "/models",
		FallbackPostURL: fmt.Sprintf("%s/ping", srv.URL),
	})
	if gotAuth != "Bearer bearer-key" || gotXKey != "custom" {
		t.Fatalf("Authorization=%q X-Api-Key=%q, want bearer/custom", gotAuth, gotXKey)
	}
}
