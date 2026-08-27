package copilot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

func TestPluginProbeOperational(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsPayload))
	}))
	defer srv.Close()

	p := New()
	res := p.Probe(context.Background(), tokenSource("probe", "token", srv.URL))
	if res.Status != plugin.ProbeOperational {
		t.Fatalf("status = %q, message = %q", res.Status, res.Message)
	}
	if res.Code != 200 || res.Err != nil {
		t.Fatalf("code/err = %d/%v", res.Code, res.Err)
	}
}

func TestPluginProbeFailedOnUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := New()
	res := p.Probe(context.Background(), tokenSource("probe", "token", srv.URL))
	if res.Status != plugin.ProbeFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if res.Err == nil {
		t.Fatal("expected probe error")
	}
}
