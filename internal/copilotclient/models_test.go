package copilotclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

func TestModelCacheFiltersZedVisibleChatModels(t *testing.T) {
	tests := []struct {
		name        string
		models      []map[string]any
		expectedIDs []string
	}{
		{
			name: "enabled chat model included",
			models: []map[string]any{{
				"id": "gpt-5.3", "model_picker_enabled": true,
				"supported_endpoints": []string{"/responses"},
				"capabilities":        map[string]any{"type": "chat"},
			}},
			expectedIDs: []string{"gpt-5.3"},
		},
		{
			name: "hidden model filtered",
			models: []map[string]any{{
				"id": "hidden-model", "model_picker_enabled": false,
				"supported_endpoints": []string{"/responses"},
				"capabilities":        map[string]any{"type": "chat"},
			}},
		},
		{
			name: "non-chat model filtered",
			models: []map[string]any{{
				"id": "embedding-model", "model_picker_enabled": true,
				"supported_endpoints": []string{"/embeddings"},
				"capabilities":        map[string]any{"type": "embedding"},
			}},
		},
		{
			name: "pending policy filtered",
			models: []map[string]any{{
				"id": "pending-model", "model_picker_enabled": true,
				"supported_endpoints": []string{"/responses"},
				"capabilities":        map[string]any{"type": "chat"},
				"policy":              map[string]any{"state": "pending"},
			}},
		},
		{
			name: "nil policy included",
			models: []map[string]any{{
				"id": "no-policy", "model_picker_enabled": true,
				"supported_endpoints": []string{"/responses"},
				"capabilities":        map[string]any{"type": "chat"},
			}},
			expectedIDs: []string{"no-policy"},
		},
		{
			name: "enabled policy included",
			models: []map[string]any{{
				"id": "enabled-model", "model_picker_enabled": true,
				"supported_endpoints": []string{"/responses"},
				"capabilities":        map[string]any{"type": "chat"},
				"policy":              map[string]any{"state": "enabled"},
			}},
			expectedIDs: []string{"enabled-model"},
		},
		{
			name: "restricted_to does not affect filtering",
			models: []map[string]any{{
				"id": "premium-only", "model_picker_enabled": true,
				"supported_endpoints": []string{"/responses"},
				"capabilities":        map[string]any{"type": "chat"},
				"billing":             map[string]any{"restricted_to": []string{"pro_plus"}},
			}},
			expectedIDs: []string{"premium-only"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
				}
				if r.Header.Get("Editor-Version") != "Zed/"+GatewayVersion {
					t.Errorf("Editor-Version = %q", r.Header.Get("Editor-Version"))
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": tt.models})
			}))
			defer srv.Close()

			cache := newModelCache(srv.Client(), time.Minute)
			models, err := cache.Get(context.Background(), srv.URL, "test-token")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if len(models) != len(tt.expectedIDs) {
				t.Fatalf("models = %+v, want IDs %v", models, tt.expectedIDs)
			}
			for _, id := range tt.expectedIDs {
				if _, ok := models[id]; !ok {
					t.Errorf("model %q missing", id)
				}
			}
		})
	}
}

func TestModelCacheTTLAndSingleflight(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id": "m1", "model_picker_enabled": true,
				"supported_endpoints": []string{"/responses"},
				"capabilities":        map[string]any{"type": "chat"},
			}},
		})
	}))
	defer srv.Close()

	cache := newModelCache(srv.Client(), 20*time.Millisecond)
	const concurrent = 10
	done := make(chan error, concurrent)
	for range concurrent {
		go func() {
			_, err := cache.Get(context.Background(), srv.URL, "token")
			done <- err
		}()
	}
	for range concurrent {
		if err := <-done; err != nil {
			t.Fatalf("concurrent Get: %v", err)
		}
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("initial requests = %d, want 1", got)
	}

	time.Sleep(25 * time.Millisecond)
	if _, err := cache.Get(context.Background(), srv.URL, "token"); err != nil {
		t.Fatalf("expired Get: %v", err)
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("expired requests = %d, want 2", got)
	}
}

func TestModelCacheFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cache := newModelCache(srv.Client(), time.Minute)
	if _, err := cache.Get(context.Background(), srv.URL, "token"); err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestModelCacheSkipsMalformedEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"unexpected":"shape"},
			{"id":"valid","model_picker_enabled":true,"supported_endpoints":["/responses"],"capabilities":{"type":"chat"}},
			{"id":"missing-picker","supported_endpoints":["/responses"],"capabilities":{"type":"chat"}}
		]}`))
	}))
	defer srv.Close()

	cache := newModelCache(srv.Client(), time.Minute)
	models, err := cache.Get(context.Background(), srv.URL, "token")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := models["valid"]; !ok || len(models) != 1 {
		t.Fatalf("models = %+v, want only valid", models)
	}
}

func TestClientDirectoryResolvesEndpointAndSortsModels(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "z-model", "model_picker_enabled": true, "supported_endpoints": []string{"/responses"}, "capabilities": map[string]any{"type": "chat"}},
			{"id": "a-model", "model_picker_enabled": true, "supported_endpoints": []string{"/responses"}, "capabilities": map[string]any{"type": "chat"}},
		}})
	}))
	defer api.Close()

	graphql := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"viewer": map[string]any{"copilotEndpoints": map[string]any{"api": api.URL}},
		}})
	}))
	defer graphql.Close()
	previous := graphqlURL
	graphqlURL = graphql.URL
	t.Cleanup(func() { graphqlURL = previous })

	client := NewWithHTTP(graphql.Client(), "")
	dir, err := client.Directory(context.Background(), config.Source{
		Name: "copilot", BackendType: config.BackendGitHubCopilot, GithubToken: "token",
	})
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if dir.Endpoint != api.URL || len(dir.Models) != 2 || dir.Models[0].ID != "a-model" {
		t.Fatalf("directory = %+v", dir)
	}
}

func TestClientDirectoryReturnsEndpointWhenModelsFail(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer api.Close()

	client := NewWithHTTP(api.Client(), api.URL)
	dir, err := client.Directory(context.Background(), config.Source{
		Name: "copilot", BackendType: config.BackendGitHubCopilot, GithubToken: "token",
	})
	if err == nil {
		t.Fatal("expected model fetch error")
	}
	if dir.Endpoint != api.URL {
		t.Fatalf("endpoint = %q, want fallback endpoint", dir.Endpoint)
	}
}
