// Package responsesclient implements low-level HTTP client for OpenAI Responses API (streaming only).
package responsesclient

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
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	return base + "/responses"
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

// Stream sends a streaming Responses request and returns the response body on success.
// Caller closes the body when done reading.
// body is the already marshaled Responses JSON; stream is always true.
func (c *Client) Stream(ctx context.Context, baseURL, apiKey string, body []byte) (io.ReadCloser, error) {
	url := responsesURL(baseURL)
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
		slog.Warn("responsesclient: upstream error",
			"status", resp.Status, "url", url, "body", snippet)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, snippet)
	}
	return resp.Body, nil
}

// ListModels fetches upstream models for admin UI dropdown.
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
		slog.Warn("responsesclient: list models failed", "status", resp.Status, "url", url)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncForLog(b, 500))
	}

	var body struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, fmt.Errorf("parse response: %w: %s", err, truncForLog(b, 500))
	}
	slog.Info("responsesclient: fetched upstream models", "url", url, "count", len(body.Data))
	return body.Data, nil
}

// ScanSSE reads SSE frames from r and calls onEvent for each complete event.
// Scanner buffer starts at 1 MiB, max 16 MiB per the plan spec.
// Multi-line data: lines are joined with \n per SSE spec.
// event type is taken from "event:" line; if absent, extracted from JSON "type" field.
// Empty event type is skipped (no onEvent call).
// data: [DONE] ends the stream cleanly.
func ScanSSE(r io.Reader, onEvent func(eventType string, data []byte) error) error {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 1024*1024) // 1 MiB initial
	scanner.Buffer(buf, 16*1024*1024) // 16 MiB max

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
			if strings.HasPrefix(payload, " ") {
				payload = payload[1:]
			}
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

func bytesReader(b []byte) io.Reader {
	return bytes.NewBuffer(b)
}

func truncForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + fmt.Sprintf("...(+%d bytes)", len(b)-n)
}
