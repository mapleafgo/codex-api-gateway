package plugin

import (
	"context"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// UpstreamEvent 描述单次上游尝试的观测数据。Backend 字段必须携带
// 分发型插件的稳定 ID（委托场景由 WrapDelegatedEvent 重写）。
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
	Backend       string
}

// Backend 对单个 source 执行一次上游流式请求。
// 返回 error 表示本源失败；若 onEvent 从未被调用，调度器可 failover。
// onUpstream 在单次尝试结束时回调（可 nil），不影响用户可见流。
type Backend interface {
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

// RequestPreparer 允许插件在发送前预检一次请求，用于提前暴露不可映射字段。
// 它不是能力裁决点：是否可以执行仍由上游决定。
type RequestPreparer interface {
	PrepareRequest(ctx context.Context, req *PrepareRequestInput) error
}

// PrepareRequestInput 是对服务器预检的输入。
type PrepareRequestInput struct {
	RawBody []byte
	Source  config.Source
	Config  *config.Config
}
