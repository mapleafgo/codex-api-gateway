// Package chatclient implements low-level HTTP client for OpenAI Chat Completions API (streaming only).
// HTTP 共享实现在 internal/upstreamhttp；本包保留导出 API 与 "chatclient:" 日志前缀。
package chatclient

import (
	"context"
	"encoding/json"
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

// ScanEvents reads SSE frames from the response body and calls onEvent once per
// complete event payload. 按 SSE 规范，同一事件的多个 data: 行以 "\n" 拼接后
// 才是完整 JSON（与 responsesclient.ScanSSE 的多行帧语义一致）；逐行单独回调
// 会把上游拆行的合法 JSON 当成截断，json.Unmarshal 失败并丢掉该事件内容。
// Chat 流以 data: [DONE] 哨兵收尾。
//
// 为兼容不带空行分隔、每行一个事件的上游，累积缓冲已是完整 JSON 时遇到新的
// data: 行会先交付上一帧，再开始新帧。
func ScanEvents(r io.Reader, onEvent func(data []byte) error) error {
	scanner := upstreamhttp.NewSSEScanner(r)
	var dataLines []string
	flush := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "[DONE]" {
			return true, nil
		}
		return false, onEvent([]byte(data))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// 空行是事件边界：把本帧累积的 data 行拼接后交付。
			if done, err := flush(); err != nil {
				return err
			} else if done {
				return nil
			}
			continue
		}
		// SSE 规范允许 "data:" 后无空格，两种形态都要认。
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			if payload, ok = strings.CutPrefix(line, "data:"); !ok {
				continue // 注释(: /event:/id: 等)不影响 data 累积
			}
		}
		// 无空行分隔的上游：上一帧已是完整 JSON 时先交付再起新帧。
		// 仅当上一行以 } / ] 收尾才做校验，避免多行 pretty JSON 上反复
		// join+parse 退化成 O(n^2)。
		if len(dataLines) > 0 && endsWithCloseBrace(dataLines[len(dataLines)-1]) &&
			json.Valid([]byte(strings.Join(dataLines, "\n"))) {
			if done, err := flush(); err != nil {
				return err
			} else if done {
				return nil
			}
		}
		dataLines = append(dataLines, payload)
	}
	if done, err := flush(); err != nil {
		return err
	} else if done {
		return nil
	}
	return scanner.Err()
}

// endsWithCloseBrace 报告一行（去空白后）是否以 } 或 ] 收尾；用于低成本判断
// 累积缓冲可能已是完整 JSON，避免每次 data: 行都做全量 join+parse。
func endsWithCloseBrace(line string) bool {
	s := strings.TrimRight(line, " \t\r")
	if s == "" {
		return false
	}
	switch s[len(s)-1] {
	case '}', ']':
		return true
	default:
		return false
	}
}
