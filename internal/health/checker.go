// Package health 提供源连通性检查功能：给定 base_url + api_key，
// 通过调上游 /v1/models 探测是否可达、key 是否有效。
// 不发真实大模型请求，不消耗 token，仅作健康探针。
package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
	"github.com/mapleafgo/codex-api-gateway/internal/upstreamhttp"
)

// Status 是健康检查结果的状态分类。
type Status string

const (
	// StatusOperational 表示源可达且 key 有效。
	StatusOperational Status = "operational"
	// StatusDegraded 表示源可达但响应慢（超过 degradedThreshold）。
	StatusDegraded Status = "degraded"
	// StatusFailed 表示源不可达或 key 无效。
	StatusFailed Status = "failed"
)

// Result 是单次健康检查的结果。
type Result struct {
	// Status 是检查结论：operational / degraded / failed。
	Status Status `json:"status"`
	// Success 表示检查是否通过（operational 或 degraded）。
	Success bool `json:"success"`
	// Message 是人类可读的结果说明。
	Message string `json:"message"`
	// ResponseTimeMs 是 TTFB（首字节时间，毫秒）。
	ResponseTimeMs int64 `json:"response_time_ms"`
	// HTTPStatus 是上游返回的 HTTP 状态码。
	HTTPStatus int `json:"http_status"`
	// CheckedAt 是检查完成的时间。
	CheckedAt time.Time `json:"checked_at"`
}

// Config 是健康检查的配置参数。
type Config struct {
	// Timeout 是单次检查的超时时间。
	Timeout time.Duration
	// DegradedThreshold 是降级阈值（毫秒）：TTFB 超过该值判定为 degraded。
	DegradedThreshold int64
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Timeout:           10 * time.Second,
		DegradedThreshold: 5000,
	}
}

// Checker 是健康检查器，可并发检查多个源。
type Checker struct {
	hc     *http.Client
	config Config
}

// New 返回使用默认 HTTP 客户端的检查器。
func New(cfg Config) *Checker {
	return &Checker{
		hc:     &http.Client{},
		config: cfg,
	}
}

// NewWithClient 返回使用自定义 HTTP 客户端的检查器。
func NewWithClient(cfg Config, hc *http.Client) *Checker {
	return &Checker{
		hc:     hc,
		config: cfg,
	}
}

// CheckSource 检查单个源的连通性。
// 策略：先 GET /v1/models（不消耗 token）；若 404 则降级发最小 POST 验证 key。
func (c *Checker) CheckSource(ctx context.Context, source config.Source) Result {
	checkedAt := time.Now()
	modelsURL := upstreamhttp.ModelsURL(source.BaseURL)

	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return Result{
			Status:    StatusFailed,
			Success:   false,
			Message:   fmt.Sprintf("创建请求失败: %v", err),
			CheckedAt: checkedAt,
		}
	}
	req.Header.Set("Authorization", "Bearer "+source.APIKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		logging.FromContext(ctx).Warn("health: check failed",
			"source", source.Name, "url", modelsURL, "error", err)
		return Result{
			Status:         StatusFailed,
			Success:        false,
			Message:        fmt.Sprintf("连接失败: %v", err),
			ResponseTimeMs: time.Since(start).Milliseconds(),
			CheckedAt:      checkedAt,
		}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start).Milliseconds()

	// /v1/models 返回 404：后端未实现该接口，降级发最小 POST 验证 key
	if resp.StatusCode == http.StatusNotFound {
		return c.fallbackMinimalProbe(ctx, source, checkedAt)
	}

	return c.classifyResponse(resp, elapsed, checkedAt)
}

// fallbackMinimalProbe 在 /v1/models 不可用时，发最小 POST 请求验证 key。
// 仅消耗 1 token，用于区分"key 无效"和"key 有效但无 /v1/models"。
func (c *Checker) fallbackMinimalProbe(ctx context.Context, source config.Source, checkedAt time.Time) Result {
	start := time.Now()
	endpoint := probeEndpoint(source)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return Result{
			Status:    StatusFailed,
			Success:   false,
			Message:   fmt.Sprintf("创建请求失败: %v", err),
			CheckedAt: checkedAt,
		}
	}
	req.Header.Set("Authorization", "Bearer "+source.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		logging.FromContext(ctx).Warn("health: fallback probe failed",
			"source", source.Name, "url", endpoint, "error", err)
		return Result{
			Status:         StatusFailed,
			Success:        false,
			Message:        fmt.Sprintf("连接失败: %v", err),
			ResponseTimeMs: time.Since(start).Milliseconds(),
			HTTPStatus:     0,
			CheckedAt:      checkedAt,
		}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start).Milliseconds()

	// 只要不是 401/403，就说明 key 有效（404 的 /v1/models 后端通常对正确路径返回 400/422 等）
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return Result{
			Status:         StatusFailed,
			Success:        false,
			Message:        "API Key 无效 (401)",
			ResponseTimeMs: elapsed,
			HTTPStatus:     resp.StatusCode,
			CheckedAt:      checkedAt,
		}
	case http.StatusForbidden:
		return Result{
			Status:         StatusFailed,
			Success:        false,
			Message:        "API Key 无权限 (403)",
			ResponseTimeMs: elapsed,
			HTTPStatus:     resp.StatusCode,
			CheckedAt:      checkedAt,
		}
	default:
		// 200/400/422/5xx 都说明 key 有效、服务可达（只是 /v1/models 未实现）
		status := StatusOperational
		msg := "正常（/v1/models 未实现，已降级验证）"
		if elapsed > c.config.DegradedThreshold {
			status = StatusDegraded
			msg = fmt.Sprintf("可达但较慢 (%dms)", elapsed)
		}
		return Result{
			Status:         status,
			Success:        true,
			Message:        msg,
			ResponseTimeMs: elapsed,
			HTTPStatus:     resp.StatusCode,
			CheckedAt:      checkedAt,
		}
	}
}

// probeEndpoint 根据 stable backend 返回最小探测的目标 URL。
func probeEndpoint(source config.Source) string {
	backend := source.Backend
	if backend == "" {
		if id, ok := config.BackendTypeToID(source.BackendType); ok {
			backend = id
		}
	}
	switch backend {
	case plugin.BackendOpenAIChat:
		return upstreamhttp.EndpointURL(source.BaseURL, "/chat/completions")
	case plugin.BackendOpenAIResponses:
		return upstreamhttp.EndpointURL(source.BaseURL, "/responses")
	default:
		return upstreamhttp.EndpointURL(source.BaseURL, "/v1/messages")
	}
}

// classifyResponse 根据 /v1/models 响应分类结果。
func (c *Checker) classifyResponse(resp *http.Response, elapsed int64, checkedAt time.Time) Result {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		status := StatusOperational
		msg := "正常"
		if elapsed > c.config.DegradedThreshold {
			status = StatusDegraded
			msg = fmt.Sprintf("可达但较慢 (%dms)", elapsed)
		}
		return Result{
			Status:         status,
			Success:        true,
			Message:        msg,
			ResponseTimeMs: elapsed,
			HTTPStatus:     resp.StatusCode,
			CheckedAt:      checkedAt,
		}
	case resp.StatusCode == http.StatusUnauthorized:
		return Result{
			Status:         StatusFailed,
			Success:        false,
			Message:        "API Key 无效 (401)",
			ResponseTimeMs: elapsed,
			HTTPStatus:     resp.StatusCode,
			CheckedAt:      checkedAt,
		}
	case resp.StatusCode == http.StatusForbidden:
		return Result{
			Status:         StatusFailed,
			Success:        false,
			Message:        "API Key 无权限 (403)",
			ResponseTimeMs: elapsed,
			HTTPStatus:     resp.StatusCode,
			CheckedAt:      checkedAt,
		}
	case resp.StatusCode >= 500:
		return Result{
			Status:         StatusFailed,
			Success:        false,
			Message:        fmt.Sprintf("上游服务错误 (%d)", resp.StatusCode),
			ResponseTimeMs: elapsed,
			HTTPStatus:     resp.StatusCode,
			CheckedAt:      checkedAt,
		}
	default:
		return Result{
			Status:         StatusFailed,
			Success:        false,
			Message:        fmt.Sprintf("意外响应 (%d)", resp.StatusCode),
			ResponseTimeMs: elapsed,
			HTTPStatus:     resp.StatusCode,
			CheckedAt:      checkedAt,
		}
	}
}

// CheckAll 并发检查所有源，返回每个源的结果（key = source name）。
func (c *Checker) CheckAll(ctx context.Context, sources []config.Source) map[string]Result {
	results := make(map[string]Result, len(sources))
	type pair struct {
		name   string
		result Result
	}
	ch := make(chan pair, len(sources))

	for _, src := range sources {
		go func(s config.Source) {
			ch <- pair{name: s.Name, result: c.CheckSource(ctx, s)}
		}(src)
	}

	for i := 0; i < len(sources); i++ {
		p := <-ch
		results[p.name] = p.result
	}
	return results
}
