package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

func TestDiscoverAPIEndpointSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", auth)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"viewer": map[string]any{
					"copilotEndpoints": map[string]any{"api": "https://custom-endpoint.example.com"},
				},
			},
		})
	}))
	defer srv.Close()

	previous := graphqlURL
	graphqlURL = srv.URL
	t.Cleanup(func() { graphqlURL = previous })

	endpoint, err := discoverAPIEndpoint(context.Background(), srv.Client(), "test-token")
	if err != nil {
		t.Fatalf("discoverAPIEndpoint: %v", err)
	}
	if endpoint != "https://custom-endpoint.example.com" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestDiscoverAPIEndpointFallbacks(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "http error", status: http.StatusInternalServerError},
		{name: "empty response", body: `{"data":{}}`},
		{name: "graphql error", body: `{"errors":[{"message":"bad token"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status != 0 {
					w.WriteHeader(tt.status)
					return
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			previous := graphqlURL
			graphqlURL = srv.URL
			t.Cleanup(func() { graphqlURL = previous })

			endpoint, err := discoverAPIEndpoint(context.Background(), srv.Client(), "test-token")
			if err == nil {
				t.Fatal("expected discovery error")
			}
			if endpoint != DefaultEndpoint {
				t.Fatalf("fallback endpoint = %q, want %q", endpoint, DefaultEndpoint)
			}
		})
	}
}

func TestDiscoverAPIEndpointFallbackOnNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	previous := graphqlURL
	graphqlURL = srv.URL
	t.Cleanup(func() { graphqlURL = previous })

	endpoint, err := discoverAPIEndpoint(context.Background(), srv.Client(), "test-token")
	if err == nil {
		t.Fatal("expected network error")
	}
	if endpoint != DefaultEndpoint {
		t.Fatalf("fallback endpoint = %q, want %q", endpoint, DefaultEndpoint)
	}
}

func TestClientCachesEndpointPerSourceAndRebuildsOnTokenChange(t *testing.T) {
	var calls atomic.Int32
	graphql := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		endpoint := "https://one.example.com"
		if r.Header.Get("Authorization") == "Bearer token-two" {
			endpoint = "https://two.example.com"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"viewer": map[string]any{"copilotEndpoints": map[string]any{"api": endpoint}},
		}})
	}))
	defer graphql.Close()
	overrideGraphQLURL(t, graphql.URL)

	client := NewClient()
	ctx := context.Background()
	one := config.Source{Name: "one", BackendType: config.BackendGitHubCopilot, GithubToken: "token-one"}
	for range 2 {
		if got := client.ResolveEndpoint(ctx, one); got != "https://one.example.com" {
			t.Fatalf("one endpoint = %q", got)
		}
	}
	two := config.Source{Name: "two", BackendType: config.BackendGitHubCopilot, GithubToken: "token-two"}
	if got := client.ResolveEndpoint(ctx, two); got != "https://two.example.com" {
		t.Fatalf("two endpoint = %q", got)
	}
	one.GithubToken = "token-three"
	if got := client.ResolveEndpoint(ctx, one); got != "https://one.example.com" {
		t.Fatalf("updated one endpoint = %q", got)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("graphql calls = %d, want 3", got)
	}
}

func TestClientUsesFallbackEndpointWhenDiscoveryFails(t *testing.T) {
	graphql := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer graphql.Close()
	overrideGraphQLURL(t, graphql.URL)

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("fallback path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
	}))
	defer fallback.Close()

	client := NewWithHTTP(graphql.Client(), fallback.URL)
	dir, err := client.Directory(context.Background(), config.Source{
		Name: "copilot", BackendType: config.BackendGitHubCopilot, GithubToken: "token",
	})
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if dir.Endpoint != fallback.URL {
		t.Fatalf("endpoint = %q, want fallback", dir.Endpoint)
	}
}

func overrideGraphQLURL(t *testing.T, url string) {
	t.Helper()
	previous := graphqlURL
	graphqlURL = url
	t.Cleanup(func() { graphqlURL = previous })
}
