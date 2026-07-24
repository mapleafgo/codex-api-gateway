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

const (
	envPrefix = "CODEX_API_GATEWAY_"
	// DefaultAnthropicMaxTokens 是客户端未指定输出额度时的内置 Anthropic 上限。
	DefaultAnthropicMaxTokens = 16384

	// BaseInstructionsFileName 是与 config.yaml 同级的基线指令文件名。
	// 不走配置项；管理页与 Load 均固定读写此文件。
	BaseInstructionsFileName = "base_instructions.md"
)

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Config is the top-level YAML configuration.
type Config struct {
	Server    ServerCfg    `koanf:"server" yaml:"server"`
	Logging   LoggingCfg   `koanf:"logging" yaml:"logging"`
	Breaker   BreakerCfg   `koanf:"breaker" yaml:"breaker,omitempty"`
	Anthropic AnthropicCfg `koanf:"anthropic" yaml:"anthropic,omitempty"`
	Sources   []Source     `koanf:"sources" yaml:"sources,omitempty"`

	// Models 为 per-slug 模型能力覆盖表。key 是模型 slug（如 gpt-5.5、glm-5.2），
	// 对应 /v1/models 返回的每条 CodexModelInfo 字段。仅覆盖显式给出的字段，
	// 其余保持 codexModelInfo 的内置默认。上游 /v1/models 不提供 context_window
	// 等能力字段，故用此处补充。
	ModelOverrides map[string]ModelOverride `koanf:"models" yaml:"models,omitempty"`

	// ModelSlugOrder 保留 YAML/管理页中 models 的声明顺序，供 /v1/models
	// 分配 Priority（越靠前越高）。
	// 不参与 YAML 字段本身；Load 从文档顺序提取，写回时按此顺序序列化。
	ModelSlugOrder []string `koanf:"-" yaml:"-"`

	// BaseInstructions 是与 config.yaml 同级的 base_instructions.md 加载内容，
	// 不参与 YAML 序列化。由 Load 一次性读入；为空则 /v1/models 返回空 base_instructions。
	// 非空时整体替换 Codex 内置 BASE_INSTRUCTIONS（由客户端注入，prompt cache 友好）。
	// 取代已废弃的 system_suffix / base_instructions_file 配置项。
	BaseInstructions string `koanf:"-" yaml:"-"`
}

// ServerCfg configures the HTTP listener.
type ServerCfg struct {
	Listen string `koanf:"listen" yaml:"listen"`
	// MaxBodyMB 是 /v1/responses 请求体大小上限（MiB）。0 表示走默认值。
	// 本机场景用于防止超大历史/图片 base64 把进程内存打爆，不是公网限流。
	MaxBodyMB int `koanf:"max_body_mb" yaml:"max_body_mb,omitempty"`
	// ReadHeaderTimeout 读完请求头的最长时间。防止慢连/半开连接长期占用。
	// 不影响已建立的 SSE 长流写超时（写超时仍刻意不设）。
	ReadHeaderTimeout Duration `koanf:"read_header_timeout" yaml:"read_header_timeout,omitempty"`
}

// LoggingCfg 配置进程级结构化日志。
type LoggingCfg struct {
	Level  string `koanf:"level" yaml:"level"`
	Format string `koanf:"format" yaml:"format,omitempty"`
	// File 非空时日志写入该文件（追加，进程生命周期常开）；为空则写 stderr。
	File string `koanf:"file" yaml:"file,omitempty"`
	// MaxSizeMB 单日志文件滚动阈值（MiB）。仅 File 非空时生效；0 表示走默认值。
	// 超过后将当前文件轮转为 .1/.2…，避免本机 gateway.log 无限膨胀。
	MaxSizeMB int `koanf:"max_size_mb" yaml:"max_size_mb,omitempty"`
	// MaxBackups 滚动后保留的历史文件个数（不含当前写入文件）。0 表示走默认值。
	MaxBackups int `koanf:"max_backups" yaml:"max_backups,omitempty"`
}

// MaxBodyBytes 返回请求体上限字节数。调用方应在 validate 之后使用。
func (s ServerCfg) MaxBodyBytes() int64 {
	if s.MaxBodyMB <= 0 {
		return 0
	}
	return int64(s.MaxBodyMB) << 20
}

// AnthropicCfg 配置 backend_type=a 的 Anthropic Messages 转换行为。
type AnthropicCfg struct {
	// DefaultMaxTokens 是客户端未传 max_output_tokens 时写入上游的 max_tokens。
	// 0 表示使用内置默认值 16384。
	DefaultMaxTokens int `koanf:"default_max_tokens" yaml:"default_max_tokens,omitempty"`
	// CacheEnabled 控制是否自动注入 Anthropic prompt cache 断点。
	// nil 表示使用默认值 true；指针用于区分缺省与显式 false。
	CacheEnabled *bool `koanf:"cache_enabled" yaml:"cache_enabled,omitempty"`
	// CacheTTL 是 prompt cache 时效，仅允许 5m 或 1h；空值默认 5m。
	CacheTTL string `koanf:"cache_ttl" yaml:"cache_ttl,omitempty"`
}

// CacheEnabledValue 返回 prompt cache 的有效开关；缺省时保持历史行为并返回 true。
func (a AnthropicCfg) CacheEnabledValue() bool {
	return a.CacheEnabled == nil || *a.CacheEnabled
}

// BreakerCfg configures upstream failover and circuit breaking.
type BreakerCfg struct {
	FirstByteTimeout Duration `koanf:"first_byte_timeout" yaml:"first_byte_timeout,omitempty"`
	CircuitInterval  Duration `koanf:"circuit_interval" yaml:"circuit_interval,omitempty"`
	DegradeInterval  Duration `koanf:"degrade_interval" yaml:"degrade_interval,omitempty"`
	DegradeThreshold int      `koanf:"degrade_threshold" yaml:"degrade_threshold,omitempty"`
	RecoverThreshold int      `koanf:"recover_threshold" yaml:"recover_threshold,omitempty"`
	HalfOpenProbes   int      `koanf:"half_open_probes" yaml:"half_open_probes,omitempty"`
	MaxRetries       int      `koanf:"max_retries" yaml:"max_retries,omitempty"`
	Recovery         string   `koanf:"recovery" yaml:"recovery,omitempty"`
}

// Source configures one upstream.
// backend_type: 'a' = Anthropic Messages, 'c' = OpenAI Chat Completions (only streaming),
// 'r' = OpenAI Responses (passthrough, only streaming)
const (
	BackendAnthropic       = "a"
	BackendOpenAIChat      = "c"
	BackendOpenAIResponses = "r"
)

// Source configures one upstream (backend_type a | c | r).
type Source struct {
	Name         string            `koanf:"name" yaml:"name"`
	BaseURL      string            `koanf:"base_url" yaml:"base_url"`
	APIKey       string            `koanf:"api_key" yaml:"api_key,omitempty"`
	BackendType  string            `koanf:"backend_type" yaml:"backend_type,omitempty"`
	ModelMap     map[string]string `koanf:"model_map" yaml:"model_map,omitempty"`
	DefaultModel string            `koanf:"default_model" yaml:"default_model,omitempty"`
	Breaker      *BreakerCfg       `koanf:"breaker" yaml:"breaker,omitempty"`
	// Disabled 为 true 时该源不参与调度（人工停用），仍保留在配置与管理页中。
	Disabled      bool `koanf:"disabled" yaml:"disabled,omitempty"`
	OriginalIndex int  `koanf:"-" yaml:"-"`
}

// NormalizeBackendType normalizes and validates the backend_type value.
// Returns normalized a/c/r or error if invalid.
func NormalizeBackendType(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", BackendAnthropic:
		return BackendAnthropic, nil
	case BackendOpenAIChat:
		return BackendOpenAIChat, nil
	case BackendOpenAIResponses:
		return BackendOpenAIResponses, nil
	default:
		return "", fmt.Errorf("config: invalid backend_type %q (allowed: a, c, r)", s)
	}
}

// ModelOverride 覆盖单个模型 slug 的 Codex ModelInfo 字段。
// 开放 per-model 差异：context_window / supports_image / supports_search。
// 其余能力（parallel_tool_calls / reasoning_summaries / input_modalities /
// use_responses_lite 等）由 codexModelInfo 硬编码统一注入。
// 所有字段均为指针（nil = 不覆盖，沿用 codexModelInfo 默认）。
type ModelOverride struct {
	// ContextWindow 最大上下文 token 数。同时应用到 CodexModelInfo 的 ContextWindow 与
	// MaxContextWindow（Codex ModelInfo 协议要求两个字段，网关场景二者相等，故 config
	// 只暴露一个 context_window 输入）。
	ContextWindow *int64 `koanf:"context_window" yaml:"context_window"`
	// SupportsImageDetailOriginal 是否支持图片识别（原尺寸 detail）。默认 false。
	// 配置 yaml key 用 supports_image（更简洁），输出给 Codex 的 JSON 仍为
	// supports_image_detail_original（对齐 codex ModelInfo 字段名）。
	SupportsImageDetailOriginal *bool `koanf:"supports_image" yaml:"supports_image"`
	// SupportsSearchTool 是否启用 tool_search / 延迟加载工具与 web 搜索声明。
	// yaml/json 用 supports_search；输出给 Codex 为 supports_search_tool。
	// nil 时沿用 codexModelInfo 默认 true；显式 false 时关闭搜索能力。
	SupportsSearchTool *bool `koanf:"supports_search" yaml:"supports_search"`
}

// MarshalYAML 序列化为 YAML。BaseInstructions / ModelSlugOrder 是运行时字段，
// 不参与序列化；models 按 ModelSlugOrder（或 ConfiguredModelSlugs）有序输出。
func (c Config) MarshalYAML() (any, error) {
	type out struct {
		Server    ServerCfg    `yaml:"server"`
		Logging   LoggingCfg   `yaml:"logging,omitempty"`
		Breaker   BreakerCfg   `yaml:"breaker,omitempty"`
		Anthropic AnthropicCfg `yaml:"anthropic,omitempty"`
		Sources   []Source     `yaml:"sources,omitempty"`
		Models    *yamlv3.Node `yaml:"models,omitempty"`
	}
	o := out{
		Server:    c.Server,
		Logging:   c.Logging,
		Breaker:   c.Breaker,
		Anthropic: c.Anthropic,
		Sources:   c.Sources,
	}
	if n := orderedModelsYAMLNode(c); n != nil {
		o.Models = n
	}
	return o, nil
}

// orderedModelsYAMLNode 按 ConfiguredModelSlugs 顺序输出 models mapping。
func orderedModelsYAMLNode(c Config) *yamlv3.Node {
	slugs := c.ConfiguredModelSlugs()
	if len(slugs) == 0 {
		return nil
	}
	n := &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
	for _, slug := range slugs {
		override, ok := c.ModelOverrides[slug]
		if !ok {
			continue
		}
		var key, val yamlv3.Node
		key.SetString(slug)
		if err := val.Encode(override); err != nil {
			continue
		}
		n.Content = append(n.Content, &key, &val)
	}
	if len(n.Content) == 0 {
		return nil
	}
	return n
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

// MarshalYAML 序列化为 Go duration 字符串（如 "12s"、"30s"），
// 避免默认输出纳秒数字导致下次 Load 时 ParseDuration 失败。
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
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
	cfg.ModelSlugOrder = modelSlugOrderFromYAML(data)
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
	// 以下日志应在调用方完成 logging.Configure 后输出，否则会走 Go 默认
	// handler 直接打到终端（见 cmd/server/main.go 的两阶段初始化）。
	// 基线指令：固定读取与 config.yaml 同级的 base_instructions.md。
	// 不在配置项中声明路径；文件缺失时降级为空串（沿用 Codex 内置指令）。
	{
		p := filepath.Join(filepath.Dir(path), BaseInstructionsFileName)
		b, err := os.ReadFile(p)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Info("base_instructions.md 读取失败，降级为空串（沿用 Codex 内置指令）",
					"path", p, "error", err)
			}
		} else {
			cfg.BaseInstructions = string(b)
			slog.Info("加载基线指令文件", "path", p, "bytes", len(cfg.BaseInstructions))
		}
	}
	slog.Info("配置加载完成",
		"breaker_max_retries", cfg.Breaker.MaxRetries,
		"anthropic_default_max_tokens", cfg.Anthropic.DefaultMaxTokens,
		"anthropic_cache_enabled", cfg.Anthropic.CacheEnabledValue(),
		"anthropic_cache_ttl", cfg.Anthropic.CacheTTL)
	return &cfg, nil
}

// LoadLogging 仅解析 logging 段（含环境变量覆盖与默认值），供进程启动早期
// 先初始化日志系统。与 Load 使用同一套解析/展开/覆盖规则，保证两阶段一致。
// 文件不存在或解析失败时返回默认 LoggingCfg（level=info, format=text），不报错，
// 让调用方能继续走默认日志；后续 Load 会以同样的规则再次校验并暴露真实错误。
func LoadLogging(path string) LoggingCfg {
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultLoggingCfg()
	}
	data = []byte(expandEnv(string(data)))
	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider(data), yaml.Parser()); err != nil {
		return defaultLoggingCfg()
	}
	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return defaultLoggingCfg()
	}
	envCfg := koanf.New(".")
	if err := envCfg.Load(env.ProviderWithValue(envPrefix, ".", transformEnv), nil); err != nil {
		return cfg.Logging
	}
	_ = applyEnvOverrides(&cfg, envCfg)
	applyLoggingDefaults(&cfg.Logging)
	return cfg.Logging
}

// defaultLoggingCfg 返回 logging 的内置默认值，与 validate 保持一致。
func defaultLoggingCfg() LoggingCfg {
	cfg := LoggingCfg{}
	applyLoggingDefaults(&cfg)
	return cfg
}

// applyLoggingDefaults 补齐 logging 段的默认值（与 validate 共用，避免分叉）。
// applyServerDefaults 补齐 server 段防误伤默认值（本机场景）。
func applyServerDefaults(s *ServerCfg) {
	if s.MaxBodyMB == 0 {
		s.MaxBodyMB = 32 // 32 MiB：覆盖长历史 + 少量图片；仍能挡住误传巨型 body
	}
	if s.ReadHeaderTimeout == 0 {
		s.ReadHeaderTimeout = Duration(10 * time.Second)
	}
}

func applyLoggingDefaults(l *LoggingCfg) {
	if l.Level == "" {
		l.Level = "info"
	}
	if l.Format == "" {
		l.Format = "text"
	}
	// 仅文件日志需要滚动参数；stderr 模式忽略。
	if l.MaxSizeMB == 0 {
		l.MaxSizeMB = 50
	}
	if l.MaxBackups == 0 {
		l.MaxBackups = 3
	}
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
		{"server.max_body_mb", &cfg.Server.MaxBodyMB},
		{"server.read_header_timeout", &cfg.Server.ReadHeaderTimeout},
		{"logging.level", &cfg.Logging.Level},
		{"logging.format", &cfg.Logging.Format},
		{"logging.file", &cfg.Logging.File},
		{"logging.max_size_mb", &cfg.Logging.MaxSizeMB},
		{"logging.max_backups", &cfg.Logging.MaxBackups},
		{"anthropic.default_max_tokens", &cfg.Anthropic.DefaultMaxTokens},
		{"anthropic.cache_ttl", &cfg.Anthropic.CacheTTL},
		{"breaker.first_byte_timeout", &cfg.Breaker.FirstByteTimeout},
		{"breaker.circuit_interval", &cfg.Breaker.CircuitInterval},
		{"breaker.degrade_interval", &cfg.Breaker.DegradeInterval},
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
	if k.Exists("anthropic.cache_enabled") {
		var enabled bool
		if err := unmarshalEnvPath(k, "anthropic.cache_enabled", &enabled); err != nil {
			return err
		}
		cfg.Anthropic.CacheEnabled = &enabled
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
		"first_byte_timeout", "circuit_interval", "degrade_interval",
		"degrade_threshold", "recover_threshold",
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
		{breakerPrefix + ".circuit_interval", &src.Breaker.CircuitInterval},
		{breakerPrefix + ".degrade_interval", &src.Breaker.DegradeInterval},
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

// Validate 暴露给 admin 包做配置校验（与启动时的 validate 相同）。
func (c *Config) Validate() error {
	return c.validate()
}

func (c *Config) validate() error {
	if len(c.Sources) == 0 {
		slog.Warn("配置未配置任何上游源，转发请求将返回 503；请在管理页添加 source")
	}
	applyLoggingDefaults(&c.Logging)
	applyServerDefaults(&c.Server)
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
	if c.Logging.MaxSizeMB < 0 {
		return fmt.Errorf("config: logging.max_size_mb must be >= 0, got %d", c.Logging.MaxSizeMB)
	}
	if c.Logging.MaxBackups < 0 {
		return fmt.Errorf("config: logging.max_backups must be >= 0, got %d", c.Logging.MaxBackups)
	}
	if c.Server.MaxBodyMB < 0 {
		return fmt.Errorf("config: server.max_body_mb must be >= 0, got %d", c.Server.MaxBodyMB)
	}
	if c.Server.ReadHeaderTimeout < 0 {
		return fmt.Errorf("config: server.read_header_timeout must be >= 0")
	}
	if c.Anthropic.DefaultMaxTokens == 0 {
		c.Anthropic.DefaultMaxTokens = DefaultAnthropicMaxTokens
	}
	if c.Anthropic.DefaultMaxTokens < 0 {
		return fmt.Errorf("config: anthropic.default_max_tokens must be >= 0, got %d", c.Anthropic.DefaultMaxTokens)
	}
	if c.Anthropic.CacheEnabled == nil {
		enabled := true
		c.Anthropic.CacheEnabled = &enabled
	}
	if c.Anthropic.CacheTTL == "" {
		c.Anthropic.CacheTTL = "5m"
	}
	if c.Anthropic.CacheTTL != "5m" && c.Anthropic.CacheTTL != "1h" {
		return fmt.Errorf("config: anthropic.cache_ttl must be \"5m\" or \"1h\", got %q", c.Anthropic.CacheTTL)
	}
	def := BreakerCfg{
		FirstByteTimeout: Duration(12 * time.Second),
		CircuitInterval:  Duration(1 * time.Minute),
		DegradeInterval:  Duration(30 * time.Second),
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
		norm, err := NormalizeBackendType(s.BackendType)
		if err != nil {
			return fmt.Errorf("config: source %d: %w", i, err)
		}
		s.BackendType = norm
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
	if b.CircuitInterval == 0 {
		b.CircuitInterval = def.CircuitInterval
	}
	if b.DegradeInterval == 0 {
		b.DegradeInterval = def.DegradeInterval
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
//
//nolint:unused // 保留供日志/调试场景调用
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
	if m.CircuitInterval != 0 {
		merged.CircuitInterval = m.CircuitInterval
	}
	if m.DegradeInterval != 0 {
		merged.DegradeInterval = m.DegradeInterval
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
	if _, ok := raw["cache"]; ok {
		slog.Warn("忽略已废弃配置字段", "field", "cache", "replacement", "anthropic.cache_enabled / anthropic.cache_ttl")
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
		case "system_suffix":
			slog.Warn("忽略已废弃配置字段", "field", "system_suffix", "replacement", "base_instructions.md（与 config 同级）")
		case "base_instructions_file":
			slog.Warn("忽略已废弃配置字段", "field", "base_instructions_file", "replacement", "将文件移到 config.yaml 同级目录，自动读取 base_instructions.md")
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

// ConfiguredModelSlugs 返回 config.yaml 中 models.<slug> 显式配置的模型 slug。
// 优先保留 YAML/管理页声明顺序（ModelSlugOrder）；顺序外的 slug 按字母序追加。
// /v1/models 只返回这些模型，并按此顺序分配 Priority。
func (c *Config) ConfiguredModelSlugs() []string {
	if c == nil || len(c.ModelOverrides) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(c.ModelOverrides))
	out := make([]string, 0, len(c.ModelOverrides))
	for _, name := range c.ModelSlugOrder {
		if _, ok := c.ModelOverrides[name]; !ok || seen[name] {
			continue
		}
		out = append(out, name)
		seen[name] = true
	}
	extras := make([]string, 0)
	for name := range c.ModelOverrides {
		if !seen[name] {
			extras = append(extras, name)
		}
	}
	sort.Strings(extras)
	return append(out, extras...)
}

// modelSlugOrderFromYAML 从原始 YAML 文档中提取 models mapping 的 key 顺序。
func modelSlugOrderFromYAML(data []byte) []string {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return nil
	}
	root := &doc
	if doc.Kind == yamlv3.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	if root.Kind != yamlv3.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		val := root.Content[i+1]
		if key.Value != "models" || val.Kind != yamlv3.MappingNode {
			continue
		}
		out := make([]string, 0, len(val.Content)/2)
		for j := 0; j+1 < len(val.Content); j += 2 {
			slug := val.Content[j].Value
			if slug != "" {
				out = append(out, slug)
			}
		}
		return out
	}
	return nil
}

// defaultConfigYAML 是自动生成的最小配置文件内容。零上游源，仅含必要默认值
// 与引导注释，让用户知道去管理页添加服务商。打包为单文件后，首次运行若
// 找不到 config.yaml 即写入此内容，保证进程可启动（转发请求返回 503 直到
// 用户配置好 source）。
const defaultConfigYAML = `# codex-api-gateway 自动生成的默认配置
# 首次运行（未找到 config.yaml）时写入。请通过管理页添加上游源。
# 管理页地址：http://localhost:8383/  （listen 改动后同步）
server:
  listen: ":8383"
  # max_body_mb: 32              # /v1/responses 请求体上限（MiB）
  # read_header_timeout: 10s     # 读完请求头超时；不影响 SSE 长流

logging:
  level: info
  format: text
  # file: gateway.log
  # max_size_mb: 50              # 单日志文件滚动阈值（MiB，仅 file 模式）
  # max_backups: 3               # 保留历史日志个数

anthropic:
  default_max_tokens: 16384      # 客户端未传 max_output_tokens 时使用
  cache_enabled: true            # 自动注入 Anthropic prompt cache 断点
  cache_ttl: 5m                  # 5m | 1h
`

// WriteDefault 写入最小默认配置到 path。目录不存在时创建。
// 已存在 path 不会覆盖（调用方负责只在文件缺失时调用）。
func WriteDefault(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(defaultConfigYAML), 0o644); err != nil {
		return fmt.Errorf("write default config: %w", err)
	}
	return nil
}
