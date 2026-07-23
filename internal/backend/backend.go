// Package backend 定义上游协议适配器：把 Responses 请求转到具体后端，再流式产出 Responses SSE。
package backend

import (
	"context"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// UpstreamEvent 描述单次上游尝试的观测数据（对齐 scheduler 观测字段）。
type UpstreamEvent struct {
	SourceName    string
	Model         string
	ResolvedModel string
	StartedAt     time.Time
	Duration      time.Duration
	TTFB          time.Duration
	Status        string // completed | failed | canceled
	Code          int
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheCreate   int
	Error         string
	Attempt       int
	BackendType   string // a | c | r
}

// Backend 对单个 source 执行一次上游流式请求。
type Backend interface {
	// Execute 解析 rawBody（Responses JSON），请求 src 对应上游，经 onEvent 产出 Responses SSE。
	// 返回 error 表示本源失败；若 onEvent 从未被调用，scheduler 可 failover。
	// onUpstream 在单次尝试结束时回调（可 nil）。
	Execute(
		ctx context.Context,
		rawBody []byte,
		src config.Source,
		cfg *config.Config,
		onEvent func(model.SSEEvent) error,
		onUpstream func(UpstreamEvent),
		attempt int,
	) error
}
