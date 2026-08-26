package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/copilotclient"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

func TestRouteByEndpoints(t *testing.T) {
	tests := []struct {
		name string
		info *copilotclient.ModelInfo
		want string
	}{
		{
			name: "nil info returns default r",
			info: nil,
			want: "r",
		},
		{
			name: "supports /responses returns r",
			info: &copilotclient.ModelInfo{ID: "m1", SupportedEndpoints: []string{"/responses"}},
			want: "r",
		},
		{
			name: "supports /v1/messages only returns a",
			info: &copilotclient.ModelInfo{ID: "m2", SupportedEndpoints: []string{"/v1/messages"}},
			want: "a",
		},
		{
			name: "supports /chat/completions only returns c",
			info: &copilotclient.ModelInfo{ID: "m3", SupportedEndpoints: []string{"/chat/completions"}},
			want: "c",
		},
		{
			name: "supports both /responses and /v1/messages prefers r",
			info: &copilotclient.ModelInfo{ID: "m4", SupportedEndpoints: []string{"/responses", "/v1/messages"}},
			want: "r",
		},
		{
			name: "supports /v1/messages and /chat/completions prefers a",
			info: &copilotclient.ModelInfo{ID: "m5", SupportedEndpoints: []string{"/v1/messages", "/chat/completions"}},
			want: "a",
		},
		{
			name: "empty SupportedEndpoints returns r",
			info: &copilotclient.ModelInfo{ID: "m6", SupportedEndpoints: []string{}},
			want: "r",
		},
		{
			name: "unknown endpoint only returns r",
			info: &copilotclient.ModelInfo{ID: "m7", SupportedEndpoints: []string{"/v1/embeddings"}},
			want: "r",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeByEndpoints(tt.info); got != tt.want {
				t.Errorf("routeByEndpoints(%+v) = %q, want %q", tt.info, got, tt.want)
			}
		})
	}
}

func TestMergeHeaders(t *testing.T) {
	user := map[string]string{
		"X-Custom":             "value",
		"Editor-Version":       "evil/override",
		"editor-version":       "evil/lower",
		"x-github-api-version": "evil/api",
	}
	copilot := map[string]string{"Editor-Version": "Zed/stable", "X-GitHub-Api-Version": "2025-10-01"}

	got := mergeHeaders(user, copilot)
	if got["X-Custom"] != "value" {
		t.Errorf("user header lost: X-Custom = %q", got["X-Custom"])
	}
	// Copilot 必需 header 优先——不可被用户覆盖
	if got["Editor-Version"] != "Zed/stable" {
		t.Errorf("copilot header overridden: Editor-Version = %q, want Zed/stable", got["Editor-Version"])
	}
	if got["X-GitHub-Api-Version"] != "2025-10-01" {
		t.Errorf("missing copilot header: X-GitHub-Api-Version = %q", got["X-GitHub-Api-Version"])
	}
	if len(got) != 3 {
		t.Errorf("merged headers contain case-equivalent duplicates: %+v", got)
	}
}

func TestExtractModel(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		modelMap     map[string]string
		defaultModel string
		wantClient   string
		wantResolved string
	}{
		{
			name:         "direct model",
			raw:          `{"model":"gpt-5.3-codex","input":[]}`,
			wantClient:   "gpt-5.3-codex",
			wantResolved: "gpt-5.3-codex",
		},
		{
			name:         "model_map maps",
			raw:          `{"model":"gpt-5","input":[]}`,
			modelMap:     map[string]string{"gpt-5": "gpt-5.3-codex"},
			wantClient:   "gpt-5",
			wantResolved: "gpt-5.3-codex",
		},
		{
			name:         "default_model fallback",
			raw:          `{"model":"unknown-model","input":[]}`,
			defaultModel: "gpt-5.3-codex",
			wantClient:   "unknown-model",
			wantResolved: "gpt-5.3-codex",
		},
		{
			name:         "no model field uses default",
			raw:          `{"input":[]}`,
			defaultModel: "gpt-5.3-codex",
			wantClient:   "gpt-5.3-codex",
			wantResolved: "gpt-5.3-codex",
		},
		{
			name: "invalid json",
			raw:  `{broken`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &config.Source{
				ModelMap:     tt.modelMap,
				DefaultModel: tt.defaultModel,
			}
			client, resolved, err := extractModel([]byte(tt.raw), src)
			if tt.name == "invalid json" {
				if err == nil {
					t.Error("expected error for invalid JSON")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if client != tt.wantClient {
				t.Errorf("client = %q, want %q", client, tt.wantClient)
			}
			if resolved != tt.wantResolved {
				t.Errorf("resolved = %q, want %q", resolved, tt.wantResolved)
			}
		})
	}
}

type copilotRequestRecord struct {
	method             string
	path               string
	auth               string
	editor             string
	api                string
	xAPIKey            string
	anthropicVersion   string
	copilotIntegration string
	body               map[string]any
}

type copilotRequestLog struct {
	mu       sync.Mutex
	requests []copilotRequestRecord
}

func (l *copilotRequestLog) record(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	body := map[string]any{}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode upstream body: %v", err)
			}
		}
	}
	rec := copilotRequestRecord{
		method:             r.Method,
		path:               r.URL.Path,
		auth:               r.Header.Get("Authorization"),
		editor:             r.Header.Get("Editor-Version"),
		api:                r.Header.Get("X-GitHub-Api-Version"),
		xAPIKey:            r.Header.Get("x-api-key"),
		anthropicVersion:   r.Header.Get("anthropic-version"),
		copilotIntegration: r.Header.Get("Copilot-Integration-Id"),
		body:               body,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, rec)
	return body
}

func (l *copilotRequestLog) byPath(path string) copilotRequestRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, rec := range l.requests {
		if rec.path == path {
			return rec
		}
	}
	return copilotRequestRecord{}
}

func writeCopilotResponsesSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, "event: response.created\n")
	io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp","model":"upstream"}}`+"\n\n")
	io.WriteString(w, "event: response.completed\n")
	io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp","model":"upstream"}}`+"\n\n")
}

func writeCopilotAnthropicSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, `data: {"type":"message_start","message":{"id":"m","model":"upstream","usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
	io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
	io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
	io.WriteString(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
	io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":1}}`+"\n\n")
	io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
}

func writeCopilotChatSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, `data: {"id":"chatcmpl","choices":[{"delta":{"role":"assistant","content":"hi"}}]}`+"\n\n")
	io.WriteString(w, `data: {"id":"chatcmpl","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`+"\n\n")
	io.WriteString(w, "data: [DONE]\n\n")
}

func TestCopilotExecuteRoutesAndUpstreamContract(t *testing.T) {
	tests := []struct {
		name           string
		supported      []string
		wantPath       string
		clientModel    string
		resolvedModel  string
		wantStreamType string
	}{
		{name: "responses is preferred", supported: []string{"/responses", "/v1/messages"}, wantPath: "/responses", clientModel: "alias", resolvedModel: "r-model", wantStreamType: "response.completed"},
		{name: "messages routes to anthropic", supported: []string{"/v1/messages"}, wantPath: "/v1/messages", clientModel: "alias", resolvedModel: "a-model", wantStreamType: "response.completed"},
		{name: "chat completions routes to chat", supported: []string{"/chat/completions"}, wantPath: "/chat/completions", clientModel: "alias", resolvedModel: "c-model", wantStreamType: "response.completed"},
		{name: "unknown endpoints default to responses", supported: []string{"/future"}, wantPath: "/responses", clientModel: "alias", resolvedModel: "r-model", wantStreamType: "response.completed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &copilotRequestLog{}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				logs.record(t, r)
				switch r.URL.Path {
				case "/models":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": []map[string]any{{
							"id":                   tt.resolvedModel,
							"model_picker_enabled": true,
							"supported_endpoints":  tt.supported,
							"capabilities":         map[string]any{"type": "chat"},
						}},
					})
					return
				case "/responses":
					writeCopilotResponsesSSE(w)
				case "/v1/messages":
					writeCopilotAnthropicSSE(w)
				case "/chat/completions":
					writeCopilotChatSSE(w)
				default:
					t.Errorf("unexpected upstream path %q", r.URL.Path)
				}
			}))
			defer api.Close()

			b := NewCopilot(NewResponses(), NewAnthropic(), NewChat())
			src := config.Source{
				Name:        "copilot",
				BaseURL:     api.URL,
				BackendType: config.BackendGitHubCopilot,
				GithubToken: "github-oauth-token",
				ModelMap:    map[string]string{"alias": tt.resolvedModel},
			}
			var eventTypes []string
			var upstreamEvent UpstreamEvent
			err := b.Execute(context.Background(), []byte(`{"model":"alias","input":"hi","stream":true}`), src, &config.Config{},
				func(ev model.SSEEvent) error {
					eventTypes = append(eventTypes, ev.Type)
					return nil
				}, func(ev UpstreamEvent) { upstreamEvent = ev }, 1)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			upstream := logs.byPath(tt.wantPath)
			if upstream.method != http.MethodPost {
				t.Fatalf("upstream method = %q", upstream.method)
			}
			if upstream.auth != "Bearer github-oauth-token" {
				t.Fatalf("Authorization = %q", upstream.auth)
			}
			if upstream.editor != "Zed/0.1.0" {
				t.Fatalf("Editor-Version = %q", upstream.editor)
			}
			if upstream.api != "2025-10-01" {
				t.Fatalf("X-GitHub-Api-Version = %q", upstream.api)
			}
			if upstream.copilotIntegration != "" {
				t.Fatalf("Copilot-Integration-Id = %q, want empty", upstream.copilotIntegration)
			}
			if tt.wantPath == "/v1/messages" {
				if upstream.xAPIKey != "" || upstream.anthropicVersion != "" {
					t.Fatalf("Copilot Messages anthropic auth headers = %q/%q, want empty",
						upstream.xAPIKey, upstream.anthropicVersion)
				}
			}
			if got := upstream.body["model"]; got != tt.resolvedModel {
				t.Fatalf("upstream model = %v, want %q", got, tt.resolvedModel)
			}
			if !containsString(eventTypes, tt.wantStreamType) {
				t.Fatalf("event types = %v, want %q", eventTypes, tt.wantStreamType)
			}
			if upstreamEvent.BackendType != config.BackendGitHubCopilot {
				t.Fatalf("upstream backend type = %q, want g", upstreamEvent.BackendType)
			}
		})
	}
}

func TestCopilotExecuteDefaultModelRoutesResolvedModel(t *testing.T) {
	logs := &copilotRequestLog{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logs.record(t, r)
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"id":                   "default-model",
					"model_picker_enabled": true,
					"supported_endpoints":  []string{"/chat/completions"},
					"capabilities":         map[string]any{"type": "chat"},
				}},
			})
			return
		}
		writeCopilotChatSSE(w)
	}))
	defer api.Close()

	b := NewCopilot(NewResponses(), NewAnthropic(), NewChat())
	src := config.Source{
		Name:         "copilot",
		BaseURL:      api.URL,
		BackendType:  config.BackendGitHubCopilot,
		GithubToken:  "token",
		DefaultModel: "default-model",
	}
	err := b.Execute(context.Background(), []byte(`{"input":"hi","stream":true}`), src, &config.Config{},
		func(model.SSEEvent) error { return nil }, nil, 1)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	upstream := logs.byPath("/chat/completions")
	if got := upstream.body["model"]; got != "default-model" {
		t.Fatalf("upstream model = %v, want default-model", got)
	}
}

func TestCopilotExecuteContextTierPassthrough(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		wantOK bool
	}{
		{name: "existing contextTier is preserved", raw: `{"model":"m","input":"hi","stream":true,"contextTier":"long_context"}`, wantOK: true},
		{name: "missing contextTier is not injected", raw: `{"model":"m","input":"hi","stream":true}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &copilotRequestLog{}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				logs.record(t, r)
				if r.URL.Path == "/models" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": []map[string]any{{
							"id":                   "m",
							"model_picker_enabled": true,
							"supported_endpoints":  []string{"/responses"},
							"capabilities":         map[string]any{"type": "chat"},
						}},
					})
					return
				}
				writeCopilotResponsesSSE(w)
			}))
			defer api.Close()

			b := NewCopilot(NewResponses(), NewAnthropic(), NewChat())
			src := config.Source{Name: "copilot", BaseURL: api.URL, BackendType: config.BackendGitHubCopilot, GithubToken: "token"}
			err := b.Execute(context.Background(), []byte(tt.raw), src, &config.Config{},
				func(model.SSEEvent) error { return nil }, nil, 1)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			_, exists := logs.byPath("/responses").body["contextTier"]
			if exists != tt.wantOK {
				t.Fatalf("contextTier exists = %v, want %v", exists, tt.wantOK)
			}
		})
	}
}

func TestCopilotExplicitBaseURLBypassesDiscovery(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to explicit endpoint: %s", r.URL.Path)
	}))
	defer api.Close()

	b := NewCopilot(NewResponses(), NewAnthropic(), NewChat())
	src := config.Source{Name: "copilot", BaseURL: api.URL, BackendType: config.BackendGitHubCopilot, GithubToken: "token"}
	if endpoint := b.ResolveEndpoint(context.Background(), &src); endpoint != api.URL {
		t.Fatalf("endpoint = %q, want explicit base URL %q", endpoint, api.URL)
	}
}

func TestCopilotExecuteFallsBackToResponsesWhenModelsFail(t *testing.T) {
	logs := &copilotRequestLog{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logs.record(t, r)
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeCopilotResponsesSSE(w)
	}))
	defer api.Close()

	b := NewCopilot(NewResponses(), NewAnthropic(), NewChat())
	src := config.Source{Name: "copilot", BaseURL: api.URL, BackendType: config.BackendGitHubCopilot, GithubToken: "token"}
	err := b.Execute(context.Background(), []byte(`{"model":"m","input":"hi","stream":true}`), src, &config.Config{},
		func(model.SSEEvent) error { return nil }, nil, 1)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if logs.byPath("/responses").method != http.MethodPost {
		t.Fatal("model-fetch failure did not fall back to Responses route")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
