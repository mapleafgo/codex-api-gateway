// Package chatclient implements low-level HTTP client for OpenAI Chat Completions API (streaming only).
// HTTP 共享实现在 internal/upstreamhttp；本包保留导出 API 与 "chatclient:" 日志前缀。
package chatclient

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/mapleafgo/codex-api-gateway/internal/upstreamhttp"
)

const logPrefix = "chatclient"

// Client is an OpenAI Chat Completions HTTP client (streaming only).
type Client struct {
	HTTP *http.Client
}

// New creates a new Chat client with default http.Client.
func New() *Client {
	return &Client{HTTP: &http.Client{}}
}

// chatCompletionsURL joins the configured base URL to /chat/completions, avoiding duplicate suffix.
func chatCompletionsURL(base string) string {
	return upstreamhttp.EndpointURL(base, "/chat/completions")
}

// ModelInfo is a stripped-down model info from upstream /v1/models response, for admin UI dropdown.
type ModelInfo = upstreamhttp.ModelInfo

// Stream sends a streaming chat completion request and returns the response body on success.
// Caller closes the body when done reading.
// body is the already marshaled ChatRequest JSON; stream is always true with include_usage: true.
func (c *Client) Stream(ctx context.Context, baseURL, apiKey string, body []byte, headers map[string]string) (io.ReadCloser, error) {
	return upstreamhttp.Stream(ctx, c.HTTP, logPrefix, chatCompletionsURL(baseURL), apiKey, body, headers)
}

// ListModels fetches upstream models for admin UI dropdown.
// Only returns the ID of each model (display_name is optional if provided by upstream).
func (c *Client) ListModels(ctx context.Context, baseURL, apiKey string, headers map[string]string) ([]ModelInfo, error) {
	return upstreamhttp.ListModels(ctx, c.HTTP, logPrefix, baseURL, apiKey, headers)
}

// ScanEvents reads SSE lines from the response body and calls onEvent for each "data:" line.
// Used by backend to feed each chunk to converter.
// Chat 流以 data: [DONE] 哨兵收尾（与 responsesclient.ScanSSE 的多行帧语义不同）。
func ScanEvents(r io.Reader, onEvent func(data []byte) error) error {
	scanner := upstreamhttp.NewSSEScanner(r)
	for scanner.Scan() {
		line := string(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// SSE 规范允许 "data:" 后无空格，两种形态都要认。
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			if data, ok = strings.CutPrefix(line, "data:"); !ok {
				continue
			}
		}
		if data == "[DONE]" {
			break
		}
		if err := onEvent([]byte(data)); err != nil {
			return err
		}
	}
	return scanner.Err()
}
