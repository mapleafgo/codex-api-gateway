package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// preflightStubBackend 同时实现 Backend 与 RequestPreparer，用于验证 server
// 把首源预检分发到插件 RequestPreparer，失败时在建立 SSE 前返回 400。
type preflightStubBackend struct{}

func (preflightStubBackend) Execute(
	_ context.Context,
	_ []byte,
	_ config.Source,
	_ *config.Config,
	_ func(model.SSEEvent) error,
	_ func(plugin.UpstreamEvent),
	_ int,
) error {
	return errPreflightBackendExecuted
}

func (preflightStubBackend) PrepareRequest(_ context.Context, _ *plugin.PrepareRequestInput) error {
	return errPreflightBlocked
}

// preflightStubPlugin 只提供 Descriptor/ValidateSource/Backend，避免引入具体插件实现。
type preflightStubPlugin struct{}

func (preflightStubPlugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:           "preflight-stub",
		Title:        "Preflight Stub",
		Summary:      "server 预检分发测试桩",
		Capabilities: []plugin.Capability{plugin.CapabilityAnthropicMessages},
		Streaming:    plugin.StreamingConverted,
	}
}

func (preflightStubPlugin) ValidateSource(config.Source) error { return nil }
func (preflightStubPlugin) Backend() plugin.Backend            { return preflightStubBackend{} }

var (
	errPreflightBackendExecuted = errors.New("preflight stub backend must not execute before preflight")
	errPreflightBlocked         = errors.New("preflight stub blocked")
)

// TestServerPreflightDispatchesToPluginRequestPreparer 验证首源预检走插件
// RequestPreparer 分发：预检失败时返回 400 且不建立 SSE、不调用 Backend.Execute。
func TestServerPreflightDispatchesToPluginRequestPreparer(t *testing.T) {
	reg, err := plugin.New(preflightStubPlugin{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	cfg := &config.Config{
		Sources: []config.Source{{Name: "stub", Backend: "preflight-stub", BaseURL: "https://example.invalid"}},
	}
	srv := New(cfg, reg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "preflight stub blocked") {
		t.Fatalf("expected preflight error in body, got: %s", body)
	}
}
