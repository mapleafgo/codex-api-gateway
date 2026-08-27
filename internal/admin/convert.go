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
func buildConfigFromInput(in adminConfigInput, current *config.Config, sensitiveKeys func(string) map[string]bool) *config.Config {
	cacheEnabled := in.Anthropic.CacheEnabled
	cfg := &config.Config{
		Server: config.ServerCfg{
			Listen:            in.Server.Listen,
			MaxBodyMB:         in.Server.MaxBodyMB,
			ReadHeaderTimeout: config.Duration(parseDur(in.Server.ReadHeaderTimeout, 10*time.Second)),
		},
		Logging: config.LoggingCfg{
			Level: in.Logging.Level, Format: in.Logging.Format, File: in.Logging.File,
			MaxSizeMB: in.Logging.MaxSizeMB, MaxBackups: in.Logging.MaxBackups,
		},
		Breaker: breakerViewToCfg(in.Breaker),
		Anthropic: config.AnthropicCfg{
			DefaultMaxTokens: in.Anthropic.DefaultMaxTokens,
			CacheEnabled:     &cacheEnabled,
		},
	}
	for _, sv := range in.Sources {
		backend := sv.Backend
		if backend == "" {
			// 兼容旧管理页请求：backend_type 短码。
			if n, err := config.NormalizeBackendType(sv.BackendType); err == nil {
				if id, ok := config.BackendTypeToID(n); ok {
					backend = id
				}
			}
		}
		options := sv.Options
		if options == nil {
			options = map[string]any{}
		}
		// 通用敏感 option 回写：管理页 GET 时脱敏了敏感 option（凭据不回显），
		// POST 全量保存时从已保存配置中恢复被脱敏的 option 键，避免凭据丢失。
		if current != nil && sensitiveKeys != nil {
			sensitive := sensitiveKeys(backend)
			if len(sensitive) > 0 {
				for _, prev := range current.Sources {
					if prev.Name == sv.Name {
						for k, v := range prev.Options {
							if sensitive[k] {
								if _, exists := options[k]; !exists {
									options[k] = v
								}
							}
						}
						break
					}
				}
			}
		}
		src := config.Source{
			Name: sv.Name, BaseURL: sv.BaseURL, APIKey: sv.APIKey,
			Backend:  backend,
			Options:  options,
			ModelMap: sv.ModelMap, DefaultModel: sv.DefaultModel,
			Disabled:          sv.Disabled,
			Headers:           sv.Headers,
			SupportsWebSearch: sv.SupportsWebSearch,
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
				AcceptsImage:                mv.AcceptsImage,
				SupportsImageDetailOriginal: mv.SupportsImageDetailOriginal,
			}
		}
		cfg.ModelSlugOrder = order
	}
	return cfg
}

func breakerViewToCfg(b breakerView) config.BreakerCfg {
	return config.BreakerCfg{
		FirstByteTimeout:          config.Duration(parseDur(b.FirstByteTimeout, 12*time.Second)),
		RequestTimeout:            config.Duration(parseDur(b.RequestTimeout, 120*time.Second)),
		DegradeThreshold:          b.DegradeThreshold,
		DegradeInterval:           config.Duration(parseDur(b.DegradeInterval, 1*time.Minute)),
		DegradedRecoveryThreshold: b.DegradedRecoveryThreshold,
		CircuitInterval:           config.Duration(parseDur(b.CircuitInterval, 30*time.Minute)),
		CircuitRecoveryThreshold:  b.CircuitRecoveryThreshold,
		Recovery:                  b.Recovery,
		MaxRetries:                b.MaxRetries,
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
