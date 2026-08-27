package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

func TestPluginProbeOperational(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := New().Probe(context.Background(), config.Source{BaseURL: srv.URL + "/v1", APIKey: "k"})
	if res.Status != plugin.ProbeOperational {
		t.Fatalf("status=%q want operational", res.Status)
	}
}
