package admin

import (
	"strings"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	yamlv3 "gopkg.in/yaml.v3"
)

// buildConfigFromInput 把管理端 POST 的视图组装回 *config.Config。
// 管理端做全量覆盖：input 不携带的字段会写回为零值/默认值。
// 这是用户接受的语义（管理页即权威配置）。
func buildConfigFromInput(in adminConfigInput) *config.Config {
	cfg := &config.Config{
		Server: config.ServerCfg{Listen: in.Server.Listen},
		Logging: config.LoggingCfg{
			Level: in.Logging.Level, Format: in.Logging.Format, File: in.Logging.File,
		},
		Breaker:              breakerViewToCfg(in.Breaker),
		Cache:                config.CacheCfg{TTL: in.Cache.TTL},
		BaseInstructionsFile: in.BaseInstructionsFile,
	}
	for _, sv := range in.Sources {
		bt := sv.BackendType
		if n, err := config.NormalizeBackendType(sv.BackendType); err == nil {
			bt = n
		}
		src := config.Source{
			Name: sv.Name, BaseURL: sv.BaseURL, APIKey: sv.APIKey,
			BackendType: bt, ModelMap: sv.ModelMap, DefaultModel: sv.DefaultModel,
		}
		if sv.Breaker != nil {
			b := breakerViewToCfg(*sv.Breaker)
			src.Breaker = &b
		}
		cfg.Sources = append(cfg.Sources, src)
	}
	if len(in.Models) > 0 {
		cfg.ModelOverrides = map[string]config.ModelOverride{}
		order := make([]string, 0, len(in.Models))
		seen := map[string]bool{}
		for _, mv := range in.Models {
			slug := strings.TrimSpace(mv.Slug)
			if slug == "" || seen[slug] {
				continue
			}
			seen[slug] = true
			order = append(order, slug)
			cfg.ModelOverrides[slug] = config.ModelOverride{
				ContextWindow:               mv.ContextWindow,
				SupportsImageDetailOriginal: mv.SupportsImage,
				SupportsSearchTool:          mv.SupportsSearch,
			}
		}
		cfg.ModelSlugOrder = order
	}
	return cfg
}

func breakerViewToCfg(b breakerView) config.BreakerCfg {
	return config.BreakerCfg{
		FirstByteTimeout: config.Duration(parseDur(b.FirstByteTimeout, 12*time.Second)),
		Cooldown:         config.Duration(parseDur(b.Cooldown, 30*time.Second)),
		DegradeThreshold: b.DegradeThreshold,
		RecoverThreshold: b.RecoverThreshold,
		HalfOpenProbes:   b.HalfOpenProbes,
		MaxRetries:       b.MaxRetries,
		Recovery:         b.Recovery,
	}
}

// parseDur 解析 duration 字符串，失败时返回 fallback。
// 空串返回零值（让 validate 用默认值）。
func parseDur(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// yamlMarshal 用 yaml.v3 输出，header 注释保留示例风格。
// 空值字段（空串/0/nil map/slice）因 yaml tag 带 omitempty 被省略，
// 写回的 config.yaml 只保留用户实际填写的字段，避免噪音。
func yamlMarshal(cfg *config.Config) ([]byte, error) {
	out, err := yamlv3.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	header := []byte("# 由管理页生成（codex-api-gateway admin）\n")
	return append(header, out...), nil
}
