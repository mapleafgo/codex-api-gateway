// Package config loads and validates YAML configuration.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"
)

// DefaultSessionMaxBytes is used when session.max_bytes is omitted or 0.
const DefaultSessionMaxBytes int64 = 64 * 1024 * 1024

// DefaultSessionMaxEntryBytes is used when session.max_entry_bytes is omitted or 0.
const DefaultSessionMaxEntryBytes int64 = 2 * 1024 * 1024

// DefaultSessionPath is the on-disk Badger path for session state.
const DefaultSessionPath = "data/session"

const envPrefix = "CODEX_API_GATEWAY_"

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Config is the top-level YAML configuration.
type Config struct {
	Server  ServerCfg  `koanf:"server" yaml:"server"`
	Logging LoggingCfg `koanf:"logging" yaml:"logging"`
	Session SessionCfg `koanf:"session" yaml:"session"`
	Breaker BreakerCfg `koanf:"breaker" yaml:"breaker"`
	Cache   CacheCfg   `koanf:"cache" yaml:"cache"`
	// BaseInstructionsFile 指向一个文本文件，其内容作为 Codex ModelInfo 的
	// base_instructions 返回给客户端（非空时整体替换 Codex 内置 BASE_INSTRUCTIONS）。
	// 用于注入网关级指令补强（如 skill 加载纪律）。相对路径基于 config 文件所在目录解析。
	// 为空则 base_instructions 返回空串，沿用 Codex 内置指令。
	// 取代已废弃的 system_suffix：后者在转换层追加 system block，需要每个上游请求都
	// 重传指令；base_instructions 由 Codex 客户端缓存并在 system 中复用，prompt cache 更友好。
	BaseInstructionsFile string   `koanf:"base_instructions_file" yaml:"base_instructions_file"`
	Sources              []Source `koanf:"sources" yaml:"sources"`

	// Models 为 per-slug 模型能力覆盖表。key 是模型 slug（如 gpt-5.5、glm-5.2），
	// 对应 /v1/models 返回的每条 CodexModelInfo 字段。仅覆盖显式给出的字段，
	// 其余保持 codexModelInfo 的内置默认。上游 /v1/models 不提供 context_window
	// 等能力字段，故用此处补充。
	ModelOverrides map[string]ModelOverride `koanf:"models" yaml:"models"`

	// BaseInstructions 是 BaseInstructionsFile 加载后的内容，不参与 YAML 序列化。
	// 由 Load 一次性读入；为空则 /v1/models 返回空 base_instructions。
	BaseInstructions string `koanf:"-" yaml:"-"`
}

// ServerCfg configures the HTTP listener.
type ServerCfg struct {
	Listen string `koanf:"listen" yaml:"listen"`
}

// LoggingCfg 配置进程级结构化日志。
type LoggingCfg struct {
	Level  string `koanf:"level" yaml:"level"`
	Format string `koanf:"format" yaml:"format"`
	// File 非空时日志写入该文件（追加，进程生命周期常开）；为空则写 stderr。
	File string `koanf:"file" yaml:"file"`
}

// SessionCfg configures previous_response_id session storage.
type SessionCfg struct {
	Path          string   `koanf:"path" yaml:"path"`
	TTL           Duration `koanf:"ttl" yaml:"ttl"`
	MaxBytes      int64    `koanf:"max_bytes" yaml:"max_bytes"`
	MaxEntryBytes int64    `koanf:"max_entry_bytes" yaml:"max_entry_bytes"`
}

// CacheCfg 配置 Anthropic prompt cache 的 TTL。
type CacheCfg struct {
	TTL string `koanf:"ttl" yaml:"ttl"` // "5m"(默认)或 "1h"
}

// BreakerCfg configures upstream failover and circuit breaking.
type BreakerCfg struct {
	FirstByteTimeout Duration `koanf:"first_byte_timeout" yaml:"first_byte_timeout"`
	Cooldown         Duration `koanf:"cooldown" yaml:"cooldown"`
	DegradeThreshold int      `koanf:"degrade_threshold" yaml:"degrade_threshold"`
	RecoverThreshold int      `koanf:"recover_threshold" yaml:"recover_threshold"`
	HalfOpenProbes   int      `koanf:"half_open_probes" yaml:"half_open_probes"`
	MaxRetries       int      `koanf:"max_retries" yaml:"max_retries"`
	Recovery         string   `koanf:"recovery" yaml:"recovery"`
}

// Source configures one Anthropic-compatible upstream.
type Source struct {
	Name          string            `koanf:"name" yaml:"name"`
	BaseURL       string            `koanf:"base_url" yaml:"base_url"`
	APIKey        string            `koanf:"api_key" yaml:"api_key"`
	ModelMap      map[string]string `koanf:"model_map" yaml:"model_map"`
	DefaultModel  string            `koanf:"default_model" yaml:"default_model"`
	Breaker       *BreakerCfg       `koanf:"breaker" yaml:"breaker"`
	OriginalIndex int               `koanf:"-" yaml:"-"`
}

// ModelOverride 覆盖单个模型 slug 的 Codex ModelInfo 字段。
// 仅保留真正存在 per-model 差异的字段（context_window / supports_image）；其余能力
// （search_tool / parallel_tool_calls / reasoning_summaries / web_search_tool_type /
// input_modalities / use_responses_lite 等）由 codexModelInfo 硬编码统一注入，不开放
// per-slug 覆盖。上游 /v1/models 无 context_window 等能力，需在 models.<slug> 下显式补充。
// 所有字段均为指针（nil = 不覆盖）。
type ModelOverride struct {
	// ContextWindow 最大上下文 token 数。同时应用到 CodexModelInfo 的 ContextWindow 与
	// MaxContextWindow（Codex ModelInfo 协议要求两个字段，网关场景二者相等，故 config
	// 只暴露一个 context_window 输入）。
	ContextWindow *int64 `koanf:"context_window" yaml:"context_window"`
	// SupportsImageDetailOriginal 是否支持图片识别（原尺寸 detail）。默认 false。
	// 配置 yaml key 用 supports_image（更简洁），输出给 Codex 的 JSON 仍为
	// supports_image_detail_original（对齐 codex ModelInfo 字段名）。
	SupportsImageDetailOriginal *bool `koanf:"supports_image" yaml:"supports_image"`
}

// Duration wraps time.Duration for YAML parsing.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string from YAML.
func (d *Duration) UnmarshalYAML(value *yamlv3.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	return d.UnmarshalText([]byte(s))
}

// UnmarshalText 从 koanf/mapstructure 提供的字符串解析 Go duration。
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func expandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		return os.Getenv(name)
	})
}

// Load reads, parses, env-interpolates and validates config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	data = []byte(expandEnv(string(data)))
	warnDeprecatedFields(data)
	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider(data), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	envCfg := koanf.New(".")
	if err := envCfg.Load(env.ProviderWithValue(envPrefix, ".", transformEnv), nil); err != nil {
		return nil, fmt.Errorf("load env config: %w", err)
	}
	if err := applyEnvOverrides(&cfg, envCfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.BaseInstructionsFile != "" {
		p := cfg.BaseInstructionsFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(path), p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			// 降级策略：base_instructions_file 读取失败不阻断启动，
			// BaseInstructions 保持空串（沿用 Codex 内置指令）。
			slog.Info("base_instructions_file 读取失败，降级为空串（沿用 Codex 内置指令）",
				"path", p,
				"base_instructions_file", cfg.BaseInstructionsFile,
				"error", err)
		} else {
			cfg.BaseInstructions = string(b)
			slog.Info("加载 base_instructions 文件", "path", p, "bytes", len(cfg.BaseInstructions))
		}
	}
	slog.Info("配置加载完成",
		"sources", cfg.sourceNames(),
		"session_path", cfg.Session.Path,
		"session_max_bytes", cfg.Session.MaxBytes,
		"session_ttl", time.Duration(cfg.Session.TTL).String(),
		"breaker_max_retries", cfg.Breaker.MaxRetries,
		"cache_ttl", cfg.Cache.TTL)
	return &cfg, nil
}

func transformEnv(key, value string) (string, interface{}) {
	key = strings.TrimPrefix(key, envPrefix)
	key = strings.ToLower(key)
	key = strings.ReplaceAll(key, "__", ".")
	return key, value
}

func applyEnvOverrides(cfg *Config, k *koanf.Koanf) error {
	overrides := []struct {
		path   string
		target any
	}{
		{"server.listen", &cfg.Server.Listen},
		{"logging.level", &cfg.Logging.Level},
		{"logging.format", &cfg.Logging.Format},
		{"logging.file", &cfg.Logging.File},
		{"session.path", &cfg.Session.Path},
		{"session.ttl", &cfg.Session.TTL},
		{"session.max_bytes", &cfg.Session.MaxBytes},
		{"session.max_entry_bytes", &cfg.Session.MaxEntryBytes},
		{"breaker.first_byte_timeout", &cfg.Breaker.FirstByteTimeout},
		{"breaker.cooldown", &cfg.Breaker.Cooldown},
		{"breaker.degrade_threshold", &cfg.Breaker.DegradeThreshold},
		{"breaker.recover_threshold", &cfg.Breaker.RecoverThreshold},
		{"breaker.half_open_probes", &cfg.Breaker.HalfOpenProbes},
		{"breaker.max_retries", &cfg.Breaker.MaxRetries},
		{"breaker.recovery", &cfg.Breaker.Recovery},
	}
	for _, override := range overrides {
		if err := unmarshalEnvPath(k, override.path, override.target); err != nil {
			return err
		}
	}
	for i := range cfg.Sources {
		if err := applySourceEnvOverrides(&cfg.Sources[i], k, fmt.Sprintf("sources.%d", i)); err != nil {
			return err
		}
	}
	return nil
}

func applySourceEnvOverrides(src *Source, k *koanf.Koanf, prefix string) error {
	overrides := []struct {
		path   string
		target any
	}{
		{prefix + ".name", &src.Name},
		{prefix + ".base_url", &src.BaseURL},
		{prefix + ".api_key", &src.APIKey},
		{prefix + ".default_model", &src.DefaultModel},
	}
	for _, override := range overrides {
		if err := unmarshalEnvPath(k, override.path, override.target); err != nil {
			return err
		}
	}
	breakerPrefix := prefix + ".breaker"
	if !hasAnyEnv(k, breakerPrefix,
		"first_byte_timeout", "cooldown", "degrade_threshold", "recover_threshold",
		"half_open_probes", "recovery") {
		return nil
	}
	if src.Breaker == nil {
		src.Breaker = &BreakerCfg{}
	}
	overrides = []struct {
		path   string
		target any
	}{
		{breakerPrefix + ".first_byte_timeout", &src.Breaker.FirstByteTimeout},
		{breakerPrefix + ".cooldown", &src.Breaker.Cooldown},
		{breakerPrefix + ".degrade_threshold", &src.Breaker.DegradeThreshold},
		{breakerPrefix + ".recover_threshold", &src.Breaker.RecoverThreshold},
		{breakerPrefix + ".half_open_probes", &src.Breaker.HalfOpenProbes},
		{breakerPrefix + ".recovery", &src.Breaker.Recovery},
	}
	for _, override := range overrides {
		if err := unmarshalEnvPath(k, override.path, override.target); err != nil {
			return err
		}
	}
	return nil
}

func hasAnyEnv(k *koanf.Koanf, prefix string, names ...string) bool {
	for _, name := range names {
		if k.Exists(prefix + "." + name) {
			return true
		}
	}
	return false
}

func unmarshalEnvPath(k *koanf.Koanf, path string, target any) error {
	if !k.Exists(path) {
		return nil
	}
	if err := k.Unmarshal(path, target); err != nil {
		return fmt.Errorf("parse env config %s: %w", path, err)
	}
	return nil
}

func (c *Config) validate() error {
	if len(c.Sources) == 0 {
		return fmt.Errorf("config: at least one source required")
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: logging.level must be debug, info, warn, or error, got %q", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "text", "json":
	default:
		return fmt.Errorf("config: logging.format must be text or json, got %q", c.Logging.Format)
	}
	if c.Session.MaxBytes < 0 {
		return fmt.Errorf("config: session.max_bytes must be 0 (default) or positive, got %d", c.Session.MaxBytes)
	}
	if c.Session.MaxEntryBytes < 0 {
		return fmt.Errorf("config: session.max_entry_bytes must be 0 (default) or positive, got %d", c.Session.MaxEntryBytes)
	}
	if c.Session.MaxBytes == 0 {
		c.Session.MaxBytes = DefaultSessionMaxBytes
	}
	if c.Session.MaxEntryBytes == 0 {
		c.Session.MaxEntryBytes = DefaultSessionMaxEntryBytes
	}
	if c.Session.Path == "" {
		c.Session.Path = DefaultSessionPath
	}
	if c.Session.TTL == 0 {
		c.Session.TTL = Duration(time.Hour)
	}
	if c.Cache.TTL == "" {
		c.Cache.TTL = "5m"
	}
	if c.Cache.TTL != "5m" && c.Cache.TTL != "1h" {
		return fmt.Errorf("config: cache.ttl must be \"5m\" or \"1h\", got %q", c.Cache.TTL)
	}
	def := BreakerCfg{
		FirstByteTimeout: Duration(12 * time.Second),
		Cooldown:         Duration(30 * time.Second),
		DegradeThreshold: 3,
		RecoverThreshold: 1,
		HalfOpenProbes:   1,
		MaxRetries:       0,
		Recovery:         "normal",
	}
	c.Breaker = applyDefaults(c.Breaker, def)
	if c.Breaker.Recovery != "normal" && c.Breaker.Recovery != "degraded" {
		return fmt.Errorf("config: breaker.recovery must be \"normal\" or \"degraded\", got %q", c.Breaker.Recovery)
	}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.Name == "" || s.BaseURL == "" {
			return fmt.Errorf("config: source %d missing name/base_url", i)
		}
		if s.Breaker != nil && s.Breaker.Recovery != "" &&
			s.Breaker.Recovery != "normal" && s.Breaker.Recovery != "degraded" {
			return fmt.Errorf("config: source %d breaker.recovery must be \"normal\" or \"degraded\", got %q",
				i, s.Breaker.Recovery)
		}
	}
	return nil
}

// applyDefaults fills zero-valued fields in b with values from def.
func applyDefaults(b, def BreakerCfg) BreakerCfg {
	if b.FirstByteTimeout == 0 {
		b.FirstByteTimeout = def.FirstByteTimeout
	}
	if b.Cooldown == 0 {
		b.Cooldown = def.Cooldown
	}
	if b.DegradeThreshold == 0 {
		b.DegradeThreshold = def.DegradeThreshold
	}
	if b.RecoverThreshold == 0 {
		b.RecoverThreshold = def.RecoverThreshold
	}
	if b.HalfOpenProbes == 0 {
		b.HalfOpenProbes = def.HalfOpenProbes
	}
	if b.MaxRetries == 0 {
		b.MaxRetries = def.MaxRetries
	}
	if b.Recovery == "" {
		b.Recovery = def.Recovery
	}
	return b
}

// OrderedSources returns sources in list order (list order = priority) with
// OriginalIndex set to each source's position in the list.
func (c *Config) OrderedSources() []Source {
	out := make([]Source, len(c.Sources))
	copy(out, c.Sources)
	for i := range out {
		out[i].OriginalIndex = i
	}
	return out
}

// sourceNames 返回所有源名称，按配置顺序，仅用于日志展示。
func (c *Config) sourceNames() []string {
	out := make([]string, len(c.Sources))
	for i, s := range c.Sources {
		out[i] = s.Name
	}
	return out
}

// BreakerFor merges global breaker with per-source override. Per-source
// zero-valued fields inherit from global. MaxRetries is never overridden
// from per-source (global only).
func (c *Config) BreakerFor(s *Source) BreakerCfg {
	if s.Breaker == nil {
		return c.Breaker
	}
	merged := c.Breaker
	m := *s.Breaker
	if m.FirstByteTimeout != 0 {
		merged.FirstByteTimeout = m.FirstByteTimeout
	}
	if m.Cooldown != 0 {
		merged.Cooldown = m.Cooldown
	}
	if m.DegradeThreshold != 0 {
		merged.DegradeThreshold = m.DegradeThreshold
	}
	if m.RecoverThreshold != 0 {
		merged.RecoverThreshold = m.RecoverThreshold
	}
	if m.HalfOpenProbes != 0 {
		merged.HalfOpenProbes = m.HalfOpenProbes
	}
	if m.Recovery != "" {
		merged.Recovery = m.Recovery
	}
	// MaxRetries: global only, never overridden by per-source.
	return merged
}

// warnDeprecatedFields scans raw YAML for deprecated keys and logs warnings.
func warnDeprecatedFields(data []byte) {
	var raw map[string]any
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return // real parse will report the error
	}
	scanDeprecated(raw)
}

// scanDeprecated recursively walks a parsed YAML map for deprecated keys.
func scanDeprecated(m map[string]any) {
	for k, v := range m {
		switch k {
		case "priority":
			slog.Warn("忽略已废弃配置字段", "field", "priority", "replacement", "sources list order")
		case "failure_threshold":
			slog.Warn("忽略已废弃配置字段", "field", "failure_threshold", "replacement", "degrade_threshold")
		case "max_entries":
			slog.Warn("忽略已废弃配置字段", "field", "max_entries", "replacement", "session.max_bytes")
		case "system_suffix":
			slog.Warn("忽略已废弃配置字段", "field", "system_suffix", "replacement", "base_instructions_file")
		}
		switch sub := v.(type) {
		case map[string]any:
			scanDeprecated(sub)
		case []any:
			for _, item := range sub {
				if subMap, ok := item.(map[string]any); ok {
					scanDeprecated(subMap)
				}
			}
		}
	}
}

// ConfiguredModelSlugs 返回 config.yaml 中 models.<slug> 显式配置的模型 slug，
// 按字母序排序。/v1/models 接口只返回这些模型，不再合并上游 model_map 或上游列表。
func (c *Config) ConfiguredModelSlugs() []string {
	names := make([]string, 0, len(c.ModelOverrides))
	for name := range c.ModelOverrides {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
