package plugin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
)

// ProbeHTTPConfig 是 HTTPProbe 的参数。
type ProbeHTTPConfig struct {
	// HTTPClient 为 nil 时使用零配置 http.Client。
	HTTPClient *http.Client
	// Timeout 是单次探测总超时；<=0 取 10s。
	Timeout time.Duration
	// DegradedMS 是降级阈值（毫秒），TTFB 超过它判为 degraded；<=0 取 5000。
	DegradedMS int64
	// ModelsURL 是 /v1/models（或等价）的绝对地址。
	ModelsURL string
	// FallbackPostURL 在 ModelsURL 返回 404 时降级发最小 POST 验证凭据；空则不上报降级。
	FallbackPostURL string
}

// HTTPProbe 用 GET ModelsURL 探测源连通性与凭据有效性，返回 404 时降级发
// 最小 POST 到 FallbackPostURL。它不发起大模型调用，仅验证可达性与 key。
// 源的自定义 Headers 会追加到探测请求。
func HTTPProbe(ctx context.Context, src config.Source, cfg ProbeHTTPConfig) ProbeResult {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.DegradedMS <= 0 {
		cfg.DegradedMS = 5000
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = new(http.Client)
	}
	checkedAt := time.Now()

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ModelsURL, nil)
	if err != nil {
		return ProbeResult{Status: ProbeFailed, Message: fmt.Sprintf("创建请求失败: %v", err), Time: checkedAt, Err: err}
	}
	applyProbeHeaders(req, src)

	resp, err := hc.Do(req)
	if err != nil {
		logging.FromContext(ctx).Warn("plugin: health probe failed",
			"source", src.Name, "url", cfg.ModelsURL, "error", err)
		return ProbeResult{Status: ProbeFailed, Message: fmt.Sprintf("连接失败: %v", err), Latency: time.Since(start), Time: checkedAt, Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start).Milliseconds()

	if resp.StatusCode == http.StatusNotFound && cfg.FallbackPostURL != "" {
		return httpFallbackProbe(ctx, hc, src, cfg, checkedAt)
	}
	return classifyModelsProbe(resp.StatusCode, elapsed, cfg.DegradedMS, checkedAt)
}

// httpFallbackProbe 在 /v1/models 不可用时发最小 POST 验证凭据，仅消耗 1 token。
func httpFallbackProbe(ctx context.Context, hc *http.Client, src config.Source, cfg ProbeHTTPConfig, checkedAt time.Time) ProbeResult {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.FallbackPostURL, nil)
	if err != nil {
		return ProbeResult{Status: ProbeFailed, Message: fmt.Sprintf("创建请求失败: %v", err), Time: checkedAt, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	applyProbeHeaders(req, src)

	resp, err := hc.Do(req)
	if err != nil {
		logging.FromContext(ctx).Warn("plugin: fallback probe failed",
			"source", src.Name, "url", cfg.FallbackPostURL, "error", err)
		return ProbeResult{Status: ProbeFailed, Message: fmt.Sprintf("连接失败: %v", err), Latency: time.Since(start), Time: checkedAt, Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start).Milliseconds()

	// 只要不是 401/403，就说明 key 有效（404 的 /v1/models 后端通常对正确路径返回 400/422 等）。
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ProbeResult{Status: ProbeFailed, Code: resp.StatusCode, Message: "API Key 无效 (401)", Time: checkedAt}
	case http.StatusForbidden:
		return ProbeResult{Status: ProbeFailed, Code: resp.StatusCode, Message: "API Key 无权限 (403)", Time: checkedAt}
	default:
		status := ProbeOperational
		msg := "正常（/v1/models 未实现，已降级验证）"
		if elapsed > cfg.DegradedMS {
			status = ProbeDegraded
			msg = fmt.Sprintf("可达但较慢 (%dms)", elapsed)
		}
		return ProbeResult{Status: status, Code: resp.StatusCode, Latency: time.Duration(elapsed) * time.Millisecond, Message: msg, Time: checkedAt}
	}
}

// classifyModelsProbe 把 /v1/models 响应码映射为三态。
func classifyModelsProbe(code int, elapsedMs int64, degradedMS int64, checkedAt time.Time) ProbeResult {
	switch {
	case code >= 200 && code < 300:
		status := ProbeOperational
		msg := "正常"
		if elapsedMs > degradedMS {
			status = ProbeDegraded
			msg = fmt.Sprintf("可达但较慢 (%dms)", elapsedMs)
		}
		return ProbeResult{Status: status, Code: code, Latency: time.Duration(elapsedMs) * time.Millisecond, Message: msg, Time: checkedAt}
	case code == http.StatusUnauthorized:
		return ProbeResult{Status: ProbeFailed, Code: code, Message: "API Key 无效 (401)", Time: checkedAt}
	case code == http.StatusForbidden:
		return ProbeResult{Status: ProbeFailed, Code: code, Message: "API Key 无权限 (403)", Time: checkedAt}
	case code >= 500:
		return ProbeResult{Status: ProbeFailed, Code: code, Message: fmt.Sprintf("上游服务错误 (%d)", code), Time: checkedAt}
	default:
		return ProbeResult{Status: ProbeFailed, Code: code, Message: fmt.Sprintf("意外响应 (%d)", code), Time: checkedAt}
	}
}

// applyProbeHeaders 写入 Bearer 认证与源自定义 header（探测与常规请求同权）。
func applyProbeHeaders(req *http.Request, src config.Source) {
	if src.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+src.APIKey)
	}
	for k, v := range src.Headers {
		req.Header.Set(k, v)
	}
}
