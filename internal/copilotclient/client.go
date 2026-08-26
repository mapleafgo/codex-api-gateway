// Package copilotclient 提供 GitHub Copilot 的 endpoint 发现与模型目录客户端。
package copilotclient

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// GatewayVersion 对齐 Zed copilot_chat crate 的 CARGO_PKG_VERSION。
const GatewayVersion = "0.1.0"

// DefaultEndpoint 是 GraphQL 发现失败时的官方回退地址。
const DefaultEndpoint = "https://api.githubcopilot.com"

// Client resolves and fetches the GitHub Copilot API directory for sources.
// It caches endpoint discovery and model catalogs per source; a Client is safe
// for concurrent use.
type Client struct {
	http            *http.Client
	defaultEndpoint string
	statesMu        sync.Mutex
	states          map[string]*sourceState
}

type sourceState struct {
	mu               sync.Mutex
	githubToken      string
	endpointOverride string
	endpoint         string
	discovered       bool
	modelsCache      *modelCache
}

// Directory is one source's resolved Copilot endpoint and filtered models.
// Endpoint remains usable when Models fetching fails so callers can fall back
// to the Responses route at the same endpoint.
type Directory struct {
	Endpoint string
	Models   []ModelInfo
}

// New returns a Client using the official Copilot fallback endpoint.
func New() *Client {
	return NewWithHTTP(nil, "")
}

// NewWithHTTP returns a Client with an injectable HTTP client and fallback
// endpoint. A nil client creates a default client; an empty fallback uses
// DefaultEndpoint. It is primarily intended for tests that must avoid GitHub.
func NewWithHTTP(hc *http.Client, fallbackEndpoint string) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	if fallbackEndpoint == "" {
		fallbackEndpoint = DefaultEndpoint
	}
	return &Client{
		http:            hc,
		defaultEndpoint: fallbackEndpoint,
		states:          map[string]*sourceState{},
	}
}

// Headers returns the Zed-style headers required by Copilot API requests.
// Authorization is intentionally omitted because protocol clients inject it.
func Headers() map[string]string {
	return map[string]string{
		"Editor-Version":       "Zed/" + GatewayVersion,
		"X-GitHub-Api-Version": "2025-10-01",
	}
}

// ApplyHeaders sets the Zed-style headers on an HTTP request.
func ApplyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Editor-Version", "Zed/"+GatewayVersion)
	req.Header.Set("X-GitHub-Api-Version", "2025-10-01")
}

// ResolveEndpoint returns the source's Copilot endpoint. An explicit BaseURL
// wins; otherwise GraphQL discovery runs lazily once per source and failures
// fall back to the client's default endpoint.
func (c *Client) ResolveEndpoint(ctx context.Context, src config.Source) string {
	return c.resolveEndpoint(ctx, c.getState(src))
}

func (c *Client) resolveEndpoint(ctx context.Context, st *sourceState) string {
	if st.endpointOverride != "" {
		return st.endpointOverride
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.discovered {
		st.endpoint, _ = discoverAPIEndpointWithDefault(ctx, c.http, st.githubToken, c.defaultEndpoint)
		st.discovered = true
	}
	return st.endpoint
}

// Directory resolves the endpoint and returns the filtered, ID-sorted model
// catalog. A model-fetch error is returned with Directory.Endpoint populated.
func (c *Client) Directory(ctx context.Context, src config.Source) (Directory, error) {
	if src.GithubToken == "" {
		return Directory{}, fmt.Errorf("copilot: source %q missing github_token", src.Name)
	}
	st := c.getState(src)
	endpoint := c.resolveEndpoint(ctx, st)
	models, err := st.modelsCache.Get(ctx, endpoint, src.GithubToken)
	if err != nil {
		return Directory{Endpoint: endpoint}, err
	}
	out := make([]ModelInfo, 0, len(models))
	for _, info := range models {
		out = append(out, *info)
	}
	slices.SortFunc(out, func(a, b ModelInfo) int {
		return strings.Compare(a.ID, b.ID)
	})
	return Directory{Endpoint: endpoint, Models: out}, nil
}

// ListModels returns the filtered Copilot models for a source. It is a
// convenience wrapper around Directory for callers that do not route requests.
func (c *Client) ListModels(ctx context.Context, src config.Source) ([]ModelInfo, error) {
	dir, err := c.Directory(ctx, src)
	if err != nil {
		return nil, err
	}
	return dir.Models, nil
}

func (c *Client) getState(src config.Source) *sourceState {
	c.statesMu.Lock()
	defer c.statesMu.Unlock()
	if st := c.states[src.Name]; st != nil {
		if st.githubToken == src.GithubToken && st.endpointOverride == src.BaseURL {
			return st
		}
	}
	st := &sourceState{
		githubToken:      src.GithubToken,
		endpointOverride: src.BaseURL,
		modelsCache:      newModelCache(c.http, 0),
	}
	c.states[src.Name] = st
	return st
}
