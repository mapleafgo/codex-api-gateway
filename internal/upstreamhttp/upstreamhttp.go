// Package upstreamhttp 承载 chatclient / responsesclient 共享的上游 HTTP 实现
// （L1）：流式 POST、模型列表、URL 拼接、日志截断与 SSE 行扫描缓冲配置。
// 各客户端包保留薄包装，导出 API 与日志前缀语义（"chatclient:"/"responsesclient:"）不变。
package upstreamhttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mapleafgo/codex-api-gateway/internal/logging"
)

// SSE 行扫描缓冲：上游常把完整 content / tool arguments 塞进单个帧，默认
// 64KiB 上限会 token too long 断流并误触发故障转移；chat/responses 两侧统一。
const (
	// ScanInitialBuf 是 SSE 行扫描的初始缓冲（1 MiB）。
	ScanInitialBuf = 1024 * 1024
	// ScanMaxBuf 是 SSE 行扫描的单行上限（16 MiB）。
	ScanMaxBuf = 16 * 1024 * 1024
)

// NewSSEScanner 返回按行扫描 r 的 Scanner，缓冲为 ScanInitialBuf/ScanMaxBuf。
func NewSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, ScanInitialBuf), ScanMaxBuf)
	return scanner
}

// EndpointURL 把 base URL 拼上路径后缀（如 "/chat/completions"），避免重复后缀。
func EndpointURL(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, suffix) {
		return base
	}
	return base + suffix
}

// ModelsURL 把 base URL 拼上 /models，避免重复后缀。
func ModelsURL(base string) string {
	return EndpointURL(base, "/models")
}

// ModelInfo is a stripped-down model info from upstream /v1/models response, for admin UI dropdown.
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

// Stream 以 Bearer/Accept SSE 头向 url POST body，成功时返回响应体（调用方负责 Close）。
// 4xx/5xx 时读取并截断响应体，带 logPrefix 记 Warn 后返回错误。
func Stream(ctx context.Context, hc *http.Client, logPrefix, url, apiKey string, body []byte) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		snippet := TruncForLog(b, 500)
		logging.FromContext(ctx).Warn(logPrefix+": upstream error",
			"status", resp.Status, "url", url, "body", snippet)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, snippet)
	}
	return resp.Body, nil
}

// ListModels fetches upstream models for admin UI dropdown.
// Only returns the ID of each model (display_name is optional if provided by upstream).
func ListModels(ctx context.Context, hc *http.Client, logPrefix, baseURL, apiKey string) ([]ModelInfo, error) {
	url := ModelsURL(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		logging.FromContext(ctx).Warn(logPrefix+": list models failed",
			"status", resp.Status, "url", url)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, TruncForLog(b, 500))
	}

	var body struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, fmt.Errorf("parse response: %w: %s", err, TruncForLog(b, 500))
	}
	logging.FromContext(ctx).Info(logPrefix+": fetched upstream models",
		"url", url, "count", len(body.Data))
	return body.Data, nil
}

// TruncForLog truncates response body to n bytes for logging, avoiding junk in logs.
func TruncForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + fmt.Sprintf("...(+%d bytes)", len(b)-n)
}
