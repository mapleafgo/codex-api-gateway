package copilot

import (
	"context"
	"fmt"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// Probe 健康探测：复用 Copilot 的 endpoint 发现 + token 交换拉取模型目录，
// 成功即可达；十秒管理超时与降级阈值与其它源一致。
func (p *Plugin) Probe(ctx context.Context, src config.Source) plugin.ProbeResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	models, err := p.b.ListModels(ctx, src)
	latency := time.Since(start)
	if err != nil {
		return plugin.ProbeResult{Status: plugin.ProbeFailed, Message: fmt.Sprintf("探活失败: %v", err), Latency: latency, Time: time.Now(), Err: err}
	}
	status := plugin.ProbeOperational
	msg := fmt.Sprintf("正常（%d 个模型）", len(models))
	if latency.Milliseconds() > 5000 {
		status = plugin.ProbeDegraded
		msg = fmt.Sprintf("可达但较慢 (%dms)", latency.Milliseconds())
	}
	return plugin.ProbeResult{Status: status, Code: 200, Latency: latency, Message: msg, Time: time.Now()}
}

var _ plugin.HealthProbe = (*Plugin)(nil)
