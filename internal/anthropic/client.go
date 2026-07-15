// Package anthropic contains the low-level Anthropic-compatible HTTP client.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	aconstant "github.com/anthropics/anthropic-sdk-go/shared/constant"
)

var streamErrorType = string(aconstant.ValueOf[aconstant.Error]())

// Client posts Anthropic Messages requests and returns SSE bodies.
type Client struct {
	HTTP *http.Client
}

// New returns a client with a default http.Client.
func New() *Client { return &Client{HTTP: &http.Client{}} }

// thinkingEnabled returns true if the request has thinking configured.
func thinkingEnabled(req *anthropic.MessageNewParams) bool {
	return req.Thinking.OfEnabled != nil || req.Thinking.OfAdaptive != nil
}

// injectStream sets "stream":true on a marshaled Anthropic request body.
// MessageNewParams has no Stream field — the SDK controls streaming at the
// method layer (NewStreaming), not via request params — so it must be added
// here. json.Number preserves numeric fidelity (e.g. max_tokens).
func injectStream(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	obj["stream"] = true
	return json.Marshal(obj)
}

// messagesURL 在配置的 base_url 上补全 Anthropic Messages 路径 /v1/messages。
// base_url 约定只写到网关根（不含该后缀），由本函数统一拼接；若 base_url 已以
// /v1/messages 结尾（向后兼容旧配置），则原样返回避免路径重复。
func messagesURL(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(endpoint, "/v1/messages") {
		return endpoint
	}
	return endpoint + "/v1/messages"
}

// modelsURL 补全 Anthropic 模型列表路径 /v1/models，逻辑同 messagesURL。
func modelsURL(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(endpoint, "/v1/models") {
		return endpoint
	}
	return endpoint + "/v1/models"
}

// ListModels 向上游发起 GET /v1/models 请求，返回响应体。
func (c *Client) ListModels(ctx context.Context, endpoint, apiKey string) (io.ReadCloser, error) {
	url := modelsURL(endpoint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		log.Printf("[anthropic] upstream %d GET %s response: %s",
			resp.StatusCode, url, truncForLog(b, 1000))
		return nil, fmt.Errorf("anthropic upstream %d: %s", resp.StatusCode, string(b))
	}
	log.Printf("[anthropic] %d GET %s", resp.StatusCode, url)
	return resp.Body, nil
}

// truncForLog returns b as a string truncated to n bytes with a tail marker,
// for embedding large request/response bodies in log lines.
func truncForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + fmt.Sprintf("...(+%d bytes)", len(b)-n)
}

// Stream POSTs the request and returns the streaming response body.
func (c *Client) Stream(ctx context.Context, endpoint, apiKey string, req *anthropic.MessageNewParams) (io.ReadCloser, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// anthropic-sdk-go 的 MessageNewParams 不含 stream 字段（官方 SDK 把流式放在
	// NewStreaming 方法层而非请求参数），而本服务是纯流式中继网关，须显式注入
	// stream:true，否则上游按非流式返回完整 JSON 而非 SSE。
	if body, err = injectStream(body); err != nil {
		return nil, err
	}
	// base_url 在配置里只写到各网关根地址，Messages 路径 /v1/messages 由
	// 代码统一补全。各 Anthropic 兼容后端根地址不同（官方
	// https://api.anthropic.com、智谱 https://open.bigmodel.cn/api/anthropic），
	// 但 Messages 路径同为 /v1/messages，故配置不写该后缀、由 messagesURL 补全。
	url := messagesURL(endpoint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	// Auth: send both credential headers. The official Anthropic API reads
	// "x-api-key"; many Anthropic-compatible gateways (智谱/Kimi/方舟) instead
	// read "Authorization: Bearer" and ignore x-api-key. Sending both lets any
	// compatible backend authenticate the request — each endpoint only honors
	// the one it knows and ignores the other.
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	if thinkingEnabled(req) {
		httpReq.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		log.Printf("[anthropic] upstream %d POST %s\n  request: %s\n  response: %s",
			resp.StatusCode, url, truncForLog(body, 2000), truncForLog(b, 1000))
		return nil, fmt.Errorf("anthropic upstream %d: %s", resp.StatusCode, string(b))
	}
	// 部分网关（如智谱）对错误路径或非法请求返回 HTTP 200 + JSON 错误体（非 4xx），
	// 仅靠状态码无法识别；content-type 非 SSE 时视为失败，附带响应体辅助排查。
	if ct := resp.Header.Get("content-type"); !strings.Contains(ct, "text/event-stream") {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		log.Printf("[anthropic] unexpected content-type %q (%d) POST %s\n  request: %s\n  response: %s",
			ct, resp.StatusCode, url, truncForLog(body, 2000), truncForLog(b, 1000))
		if len(b) > 500 {
			b = append(b[:500:500], []byte("...(truncated)")...)
		}
		return nil, fmt.Errorf("anthropic upstream %d: unexpected content-type %q: %s", resp.StatusCode, ct, string(b))
	}
	log.Printf("[anthropic] %d POST %s (streaming)", resp.StatusCode, url)
	return resp.Body, nil
}

// ScanEvents parses an SSE body and calls fn for each data event.
// Error events (type=error) are detected and their message is injected
// into a synthetic MessageStreamEventUnion with Type="error" and the
// error message in Delta.Text, so the converter can surface them as
// response.failed.
func ScanEvents(r io.Reader, fn func(*anthropic.MessageStreamEventUnion) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		// Pre-check for error events — these are NOT MessageStreamEventUnion
		// variants and the error payload would be silently dropped.
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(payload), &probe) == nil && probe.Type == streamErrorType {
			var errInfo struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal([]byte(payload), &errInfo)
			ev := &anthropic.MessageStreamEventUnion{
				Type: streamErrorType,
			}
			ev.Delta.Text = errInfo.Error.Message
			if err := fn(ev); err != nil {
				return err
			}
			continue
		}

		var ev anthropic.MessageStreamEventUnion
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return fmt.Errorf("parse SSE data: %w: %s", err, truncForLog([]byte(payload), 500))
		}
		if err := fn(&ev); err != nil {
			return err
		}
	}
	return sc.Err()
}
