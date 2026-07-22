// Package chatclient implements low-level HTTP client for OpenAI Chat Completions API (streaming only).
package chatclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

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
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// modelsURL joins the configured base URL to /models, avoiding duplicate suffix.
func modelsURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/models") {
		return base
	}
	return base + "/models"
}

// ModelInfo is a stripped-down model info from upstream /v1/models response, for admin UI dropdown.
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

// Stream sends a streaming chat completion request and returns the response body on success.
// Caller closes the body when done reading.
// body is the already marshaled ChatRequest JSON; stream is always true with include_usage: true.
func (c *Client) Stream(ctx context.Context, baseURL, apiKey string, body []byte) (io.ReadCloser, error) {
	url := chatCompletionsURL(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytesReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		snippet := truncForLog(b, 500)
		slog.Warn("chatclient: upstream error",
			"status", resp.Status, "url", url, "body", snippet)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, snippet)
	}
	return resp.Body, nil
}

// ListModels fetches upstream models for admin UI dropdown.
// Only returns the ID of each model (display_name is optional if provided by upstream).
func (c *Client) ListModels(ctx context.Context, baseURL, apiKey string) ([]ModelInfo, error) {
	url := modelsURL(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		slog.Warn("chatclient: list models failed", "status", resp.Status, "url", url)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncForLog(b, 500))
	}

	var body struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, fmt.Errorf("parse response: %w: %s", err, truncForLog(b, 500))
	}
	slog.Info("chatclient: fetched upstream models", "url", url, "count", len(body.Data))
	return body.Data, nil
}

// bytesReader wraps a []byte in io.Reader.
func bytesReader(b []byte) io.Reader {
	return bytes.NewBuffer(b)
}

// truncForLog truncates response body to n bytes for logging, avoiding junk in logs.
func truncForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + fmt.Sprintf("...(+%d bytes)", len(b)-n)
}

// ScanEvents reads SSE lines from the response body and calls onEvent for each "data:" line.
// Used by backend to feed each chunk to converter.
func ScanEvents(r io.Reader, onEvent func(data []byte) error) error {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if !strings.HasPrefix(string(line), "data: ") {
			continue
		}
		data := strings.TrimPrefix(string(line), "data: ")
		if data == "[DONE]" {
			break
		}
		if err := onEvent([]byte(data)); err != nil {
			return err
		}
	}
	return scanner.Err()
}
