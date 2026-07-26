// Package responsesclient implements low-level HTTP client for OpenAI Responses API (streaming only).
// HTTP 共享实现在 internal/upstreamhttp；本包保留导出 API 与 "responsesclient:" 日志前缀。
package responsesclient

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mapleafgo/codex-api-gateway/internal/upstreamhttp"
)

const logPrefix = "responsesclient"

// Client is an OpenAI Responses HTTP client (streaming only).
type Client struct {
	HTTP *http.Client
}

// New creates a new Responses client with default http.Client.
func New() *Client {
	return &Client{HTTP: &http.Client{}}
}

// responsesURL joins the configured base URL to /responses, avoiding duplicate suffix.
func responsesURL(base string) string {
	return upstreamhttp.EndpointURL(base, "/responses")
}

// ModelInfo is a stripped-down model info from upstream /v1/models response, for admin UI dropdown.
type ModelInfo = upstreamhttp.ModelInfo

// Stream sends a streaming Responses request and returns the response body on success.
// Caller closes the body when done reading.
// body is the already marshaled Responses JSON; stream is always true.
func (c *Client) Stream(ctx context.Context, baseURL, apiKey string, body []byte) (io.ReadCloser, error) {
	return upstreamhttp.Stream(ctx, c.HTTP, logPrefix, responsesURL(baseURL), apiKey, body)
}

// ListModels fetches upstream models for admin UI dropdown.
func (c *Client) ListModels(ctx context.Context, baseURL, apiKey string) ([]ModelInfo, error) {
	return upstreamhttp.ListModels(ctx, c.HTTP, logPrefix, baseURL, apiKey)
}

// ScanSSE reads SSE frames from r and calls onEvent for each complete event.
// Scanner buffer starts at 1 MiB, max 16 MiB (upstreamhttp.NewSSEScanner).
// Multi-line data: lines are joined with \n per SSE spec.
// event type is taken from "event:" line; if absent, extracted from JSON "type" field.
// Empty event type is skipped (no onEvent call).
// data: [DONE] ends the stream cleanly.
func ScanSSE(r io.Reader, onEvent func(eventType string, data []byte) error) error {
	scanner := upstreamhttp.NewSSEScanner(r)

	var eventType string
	var dataLines []string
	hasData := false

	flush := func() error {
		if !hasData {
			eventType = ""
			dataLines = nil
			return nil
		}
		data := strings.Join(dataLines, "\n")
		// strip optional leading spaces that follow "data:" after join of multi-line parts
		if data == "[DONE]" {
			eventType = ""
			dataLines = nil
			hasData = false
			return io.EOF
		}

		et := eventType
		if et == "" {
			et = extractEventType([]byte(data))
		}
		if et == "" {
			slog.Debug("responsesclient: skip event with empty type")
			eventType = ""
			dataLines = nil
			hasData = false
			return nil
		}

		if err := onEvent(et, []byte(data)); err != nil {
			return err
		}
		eventType = ""
		dataLines = nil
		hasData = false
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if err := flush(); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		if strings.HasPrefix(line, "data:") {
			// optional single space after colon (SSE)
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			dataLines = append(dataLines, payload)
			hasData = true
			continue
		}

		// ignore id:/retry:/comment lines
	}

	if err := flush(); err != nil && err != io.EOF {
		return err
	}
	return scanner.Err()
}

// extractEventType tries to pull "type" from a JSON object.
func extractEventType(data []byte) string {
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &obj) == nil && obj.Type != "" {
		return obj.Type
	}
	return ""
}
