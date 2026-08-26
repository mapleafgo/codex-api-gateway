package plugin

import (
	"context"
	"errors"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// ErrCapabilityNotSupported 表示源插件没有承载某个可选能力（目录/探测/动作）。
var ErrCapabilityNotSupported = errors.New("capability not supported by source plugin")

// Model 是统一模型目录项；Metadata 不得携带凭据。
type Model struct {
	ID          string
	DisplayName string
	Metadata    map[string]any
}

// ModelCatalog 为已保存源提供模型目录。
type ModelCatalog interface {
	ListModels(ctx context.Context, src config.Source) ([]Model, error)
}

// DraftModelCatalog 为管理页未保存草稿提供模型目录，敏感 options 已合并保留值。
type DraftModelCatalog interface {
	ListDraftModels(ctx context.Context, src config.Source) ([]Model, error)
}

// ProbeStatus 是健康探测的三态结果。
type ProbeStatus string

const (
	ProbeOperational ProbeStatus = "operational"
	ProbeDegraded    ProbeStatus = "degraded"
	ProbeFailed      ProbeStatus = "failed"
)

// ProbeResult 是健康探测结果；ErrCapabilityNotSupported 表示不支持探测。
type ProbeResult struct {
	Status  ProbeStatus
	Code    int
	Latency time.Duration
	Message string
	Time    time.Time
	Err     error
}

// HealthProbe 由插件决定探测目标、认证方式与降级解释。
type HealthProbe interface {
	Probe(ctx context.Context, src config.Source) ProbeResult
}
