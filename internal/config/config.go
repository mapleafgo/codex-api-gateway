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

// SourceValidator 校验单个 Source 的插件级约束。由插件注册表实现并注入到
// 配置加载/保存/热重载路径，config 包本身不 import 插件实现。
type SourceValidator interface {
	ValidateSource(Source) error
}

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

// AnthropicCfg 配置 backend=anthropic 的 Anthropic Messages 转换行为。
type AnthropicCfg struct {
	// DefaultMaxTokens 是客户端未传 max_output_tokens 时写入上游的 max_tokens。
	// 0 表示使用内置默认值 16384。
	DefaultMaxTokens int `koanf:"default_max_tokens" yaml:"default_max_tokens,omitempty"`
	// CacheEnabled 控制是否自动注入 Anthropic prompt cache 断点。
	// nil 表示使用默认值 true；指针用于区分缺省与显式 false。
	CacheEnabled *bool `koanf:"cache_enabled" yaml:"cache_enabled,omitempty"`
}

// CacheEnabledValue 返回 prompt cache 的有效开关；缺省时保持历史行为并返回 true。
func (a AnthropicCfg) CacheEnabledValue() bool {
	return a.CacheEnabled == nil || *a.CacheEnabled
}

// BreakerCfg configures upstream failover and circuit breaking.
type BreakerCfg struct {
	FirstByteTimeout Duration `koanf:"first_byte_timeout" yaml:"first_byte_timeout,omitempty"`
	// RequestTimeout 是单个源单笔上游调用的总时长上限；0 表示使用全局默认
	// （120s）。与 FirstByteTimeout 不同：总时长不被首个事件停止，到点即终止
	// 该笔调用并按失败处理（未出内容时允许 failover）。
	RequestTimeout   Duration `koanf:"request_timeout" yaml:"request_timeout,omitempty"`
	DegradeThreshold int      `koanf:"degrade_threshold" yaml:"degrade_threshold,omitempty"`
	DegradeInterval  Duration `koanf:"degrade_interval" yaml:"degrade_interval,omitempty"`
	// DegradedRecoveryThreshold 是降级恢复阈值：degraded 恢复到 normal 所需的连续成功次数。
	DegradedRecoveryThreshold int      `koanf:"degraded_recovery_threshold" yaml:"degraded_recovery_threshold,omitempty"`
	CircuitInterval           Duration `koanf:"circuit_interval" yaml:"circuit_interval,omitempty"`
	// CircuitRecoveryThreshold 是熔断恢复阈值：halfOpen 恢复到 normal/degraded
	// 所需的连续探测成功次数。
	CircuitRecoveryThreshold int    `koanf:"circuit_recovery_threshold" yaml:"circuit_recovery_threshold,omitempty"`
	Recovery                 string `koanf:"recovery" yaml:"recovery,omitempty"`
	// MaxRetries 是所有源全部失败后的整轮重试次数（0=不重试；仅全局生效）。
	MaxRetries int `koanf:"max_retries" yaml:"max_retries,omitempty"`
}

// Recovery* 是 BreakerCfg.Recovery 的合法取值（半开探测成功后的恢复目标）。
const (
	RecoveryNormal   = "normal"
	RecoveryDegraded = "degraded"
)

// DefaultLogMaxSizeMB 是日志文件滚动阈值默认值（MiB）。
// config 校验与 logging/rotate 的兜底共用，避免默认值双份分叉。
const DefaultLogMaxSizeMB = 50

// Source configures one upstream (stable plugin backend id + plugin-owned options).
type Source struct {
	Name    string `koanf:"name" yaml:"name"`
	BaseURL string `koanf:"base_url" yaml:"base_url"`
	APIKey  string `koanf:"api_key" yaml:"api_key,omitempty"`
	// Backend 是已注册源插件的稳定 ID，取代旧的 backend_type 短码。
	Backend string `koanf:"backend" yaml:"backend"`
	// Options 是所选插件 schema 声明的源专属配置（敏感值支持 ${ENV_VAR} 插值）。
	Options map[string]any `koanf:"options" yaml:"options,omitempty"`
	// ModelMap 是平台级模型映射：客户端模型名 → 实际上游模型名。
	ModelMap     map[string]string `koanf:"model_map" yaml:"model_map,omitempty"`
	DefaultModel string            `koanf:"default_model" yaml:"default_model,omitempty"`
	Breaker      *BreakerCfg       `koanf:"breaker" yaml:"breaker,omitempty"`
	// Disabled 为 true 时该源不参与调度（人工停用），仍保留在配置与管理页中。
	Disabled      bool `koanf:"disabled" yaml:"disabled,omitempty"`
	OriginalIndex int  `koanf:"-" yaml:"-"`
	// Headers 是追加到上游请求的自定义 header 键值对（如 X-Api-Key、anthropic-beta 覆盖）。
	// 保留头（content-type / authorization / accept / x-api-key / anthropic-version / anthropic-beta）不可被覆盖，静默跳过。
	Headers map[string]string `koanf:"headers" yaml:"headers,omitempty"`
	// SupportsWebSearch 显式声明该上游是否支持 hosted web_search 工具。
	// nil 时按 stable backend 给默认（见 SupportsWebSearchValue）。上游不支持时网关
	// 会在发请求前剥掉 web_search 工具，避免上游 400/断流。
	SupportsWebSearch *bool `koanf:"supports_web_search" yaml:"supports_web_search,omitempty"`
	// ResponsesCompatFold 控制 r 路径是否对 reasoning summary / 明文 agent_message
	// 做兼容折算。原生 OpenAI Responses 兼容端点（仅以 Bearer 认证、接受原生
	// reasoning 形态）置 false 跳过折算；nil/默认 true 保持对 DeepSeek 等兼容上游
	// 的折叠行为。这是跨源通用开关，由分发型插件在委托时按上游形态设置，
	// 共享核心不据此判断具体源身份。
	ResponsesCompatFold *bool `koanf:"responses_compat_fold" yaml:"responses_compat_fold,omitempty"`
}

func (s Source) SupportsWebSearchValue() bool {
	if s.SupportsWebSearch != nil {
		return *s.SupportsWebSearch
	}
	if s.Backend == "openai-chat" {
		return false
	}
	return true
}

// ResponsesCompatFoldValue 返回 r 路径是否需要做 reasoning / agent_message 兼容折算。
// 显式配置优先；nil 时默认折算（兼容折叠是多数 Responses 兼容上游的安全默认）。
func (s Source) ResponsesCompatFoldValue() bool {
	if s.ResponsesCompatFold != nil {
		return *s.ResponsesCompatFold
	}
	return true
}

// ModelOverride 覆盖单个模型 slug 的 Codex ModelInfo 字段。
// 开放 per-model 差异：context_window / accepts_image / supports_image_detail_original。
// 其余能力（parallel_tool_calls / reasoning_summaries /
// use_responses_lite 等）由 codexModelInfo 硬编码统一注入。
// supports_search_tool 始终返回 true：它是 client-side standalone 搜索开关，
// 关闭也无法阻止 hosted web search（由 provider capabilities + web_search_mode 决定），
// 暴露该开关只会误导用户以为能关掉搜索，故移除 per-model 覆盖。
// 所有字段均为指针（nil = 不覆盖，沿用 codexModelInfo 默认）。
type ModelOverride struct {
	// ContextWindow 最大上下文 token 数。同时应用到 CodexModelInfo 的 ContextWindow 与
	// MaxContextWindow（Codex ModelInfo 协议要求两个字段，网关场景二者相等，故 config
	// 只暴露一个 context_window 输入）。
	ContextWindow *int64 `koanf:"context_window" yaml:"context_window"`
	// AcceptsImage 是否接受图片输入（控制 Codex ModelInfo.input_modalities 是否含 image）。
	// 这才是真正控制「模型能否识别图片」的维度：false 时 Codex 会在发请求前
	// strip 掉历史中的全部图片，模型根本看不到图。nil 时沿用默认 true。
	AcceptsImage *bool `koanf:"accepts_image" yaml:"accepts_image"`
	// SupportsImageDetailOriginal 是否发送原图分辨率（detail=original，不压缩）。
	// 这是「清晰度档位」维度，与 AcceptsImage（能否看图）不同：
	// AcceptsImage=false 时模型完全不看图；AcceptsImage=true 但本字段=false 时
	// 模型能看图，但图片被压成 high 质量。默认 false（压缩）。
	SupportsImageDetailOriginal *bool `koanf:"supports_image_detail_original" yaml:"supports_image_detail_original"`
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

// Load reads, parses, env-interpolates and validates config without a
// plugin-level SourceValidator (nil validator). 装配入口应优先使用
// LoadWithValidator，以便配置校验覆盖 Backend 已注册、options 合法等插件约束。
func Load(path string) (*Config, error) {
	return LoadWithValidator(path, nil)
}

// LoadWithValidator 与 Load 相同，但用注入的 SourceValidator 做插件级校验。
// validator 可为 nil（等同于 Load）。校验错误返回时，调用方不得替换运行时状态。
func LoadWithValidator(path string, v SourceValidator) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	data = []byte(expandEnv(string(data)))
	if err := rejectLegacyConfigShape(data); err != nil {
		return nil, err
	}
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
	if err := cfg.validate(v); err != nil {
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
				// 非 NotExist 的真实 I/O 错误（权限等）：用户显式配置的基线
				// 指令被整体丢弃、模型行为静默变化，属重要数据丢失，必须 WARN。
				slog.Warn("base_instructions.md 读取失败，降级为空串（沿用 Codex 内置指令）",
					"path", p, "error", err, "impact", "基线指令不会注入模型")
			}
		} else {
			cfg.BaseInstructions = string(b)
			slog.Info("加载基线指令文件", "path", p, "bytes", len(cfg.BaseInstructions))
		}
	}
	slog.Info("配置加载完成",
		"breaker_max_retries", cfg.Breaker.MaxRetries,
		"anthropic_default_max_tokens", cfg.Anthropic.DefaultMaxTokens,
		"anthropic_cache_enabled", cfg.Anthropic.CacheEnabledValue())
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

// applyServerDefaults 补齐 server 段防误伤默认值（本机场景）。
func applyServerDefaults(s *ServerCfg) {
	if s.Listen == "" {
		s.Listen = ":8383"
	}
	if s.MaxBodyMB == 0 {
		s.MaxBodyMB = 32 // 32 MiB：覆盖长历史 + 少量图片；仍能挡住误传巨型 body
	}
	if s.ReadHeaderTimeout == 0 {
		s.ReadHeaderTimeout = Duration(10 * time.Second)
	}
}

// applyLoggingDefaults 补齐 logging 段的默认值（与 validate 共用，避免分叉）。
func applyLoggingDefaults(l *LoggingCfg) {
	if l.Level == "" {
		l.Level = "info"
	}
	if l.Format == "" {
		l.Format = "text"
	}
	// 仅文件日志需要滚动参数；stderr 模式忽略。
	if l.MaxSizeMB == 0 {
		l.MaxSizeMB = DefaultLogMaxSizeMB
	}
	if l.MaxBackups == 0 {
		l.MaxBackups = 3
	}
}

func transformEnv(key, value string) (string, any) {
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
		{"breaker.first_byte_timeout", &cfg.Breaker.FirstByteTimeout},
		{"breaker.request_timeout", &cfg.Breaker.RequestTimeout},
		{"breaker.circuit_interval", &cfg.Breaker.CircuitInterval},
		{"breaker.degrade_interval", &cfg.Breaker.DegradeInterval},
		{"breaker.degrade_threshold", &cfg.Breaker.DegradeThreshold},
		{"breaker.degraded_recovery_threshold", &cfg.Breaker.DegradedRecoveryThreshold},
		{"breaker.circuit_recovery_threshold", &cfg.Breaker.CircuitRecoveryThreshold},
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
		"first_byte_timeout", "request_timeout", "circuit_interval", "degrade_interval",
		"degrade_threshold", "degraded_recovery_threshold",
		"circuit_recovery_threshold", "recovery") {
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
		{breakerPrefix + ".request_timeout", &src.Breaker.RequestTimeout},
		{breakerPrefix + ".circuit_interval", &src.Breaker.CircuitInterval},
		{breakerPrefix + ".degrade_interval", &src.Breaker.DegradeInterval},
		{breakerPrefix + ".degrade_threshold", &src.Breaker.DegradeThreshold},
		{breakerPrefix + ".degraded_recovery_threshold", &src.Breaker.DegradedRecoveryThreshold},
		{breakerPrefix + ".circuit_recovery_threshold", &src.Breaker.CircuitRecoveryThreshold},
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
	return c.validate(nil)
}

// ValidateWithValidator 用注入的 SourceValidator 校验配置。nil 时跳过插件级校验，
// 仅做内置字段校验（例如 admin 早期版本的调用方未注入 validator）。
func (c *Config) ValidateWithValidator(v SourceValidator) error {
	return c.validate(v)
}

func (c *Config) validate(v SourceValidator) error {
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
	def := BreakerCfg{
		FirstByteTimeout:          Duration(12 * time.Second),
		RequestTimeout:            Duration(120 * time.Second),
		DegradeThreshold:          3,
		DegradeInterval:           Duration(1 * time.Minute),
		DegradedRecoveryThreshold: 1,
		CircuitInterval:           Duration(30 * time.Minute),
		CircuitRecoveryThreshold:  1,
		Recovery:                  RecoveryNormal,
		MaxRetries:                0,
	}
	c.Breaker = applyDefaults(c.Breaker, def)
	if c.Breaker.Recovery != RecoveryNormal && c.Breaker.Recovery != RecoveryDegraded {
		return fmt.Errorf("config: breaker.recovery must be \"normal\" or \"degraded\", got %q", c.Breaker.Recovery)
	}
	if err := validateBreakerNonNegative("breaker", &c.Breaker); err != nil {
		return err
	}
	seenSource := make(map[string]bool, len(c.Sources))
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.Name != "" {
			if seenSource[s.Name] {
				return fmt.Errorf("config: duplicate source name %q", s.Name)
			}
			seenSource[s.Name] = true
		}
		if err := validateSourceIdentity(i, s, v); err != nil {
			return err
		}
		if err := validateSourceHeaders(i, s.Headers); err != nil {
			return err
		}
		if s.Breaker != nil {
			if s.Breaker.Recovery != "" &&
				s.Breaker.Recovery != RecoveryNormal && s.Breaker.Recovery != RecoveryDegraded {
				return fmt.Errorf("config: source %d breaker.recovery must be \"normal\" or \"degraded\", got %q",
					i, s.Breaker.Recovery)
			}
			if err := validateBreakerNonNegative(fmt.Sprintf("source %d breaker", i), s.Breaker); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateSourceIdentity 校验单个 source 的身份与后端字段。
// backend 必须非空；提供 validator 时，插件级必填与 options 校验交给注册表。
// base_url 是否必填由插件 schema 声明，这里只做平台级最小约束（空串允许，
// 插件可动态发现或配置在 options）。
func validateSourceIdentity(i int, s *Source, v SourceValidator) error {
	if s.Name == "" {
		return fmt.Errorf("config: source %d missing name", i)
	}
	if s.Backend == "" {
		return fmt.Errorf("config: source %d missing backend; set backend to a registered source plugin", i)
	}
	if v != nil {
		if err := v.ValidateSource(*s); err != nil {
			return fmt.Errorf("config: source %d: %w", i, err)
		}
	}
	return nil
}

// validateSourceHeaders 校验 source 自定义 header 名称合法性。
func validateSourceHeaders(i int, h map[string]string) error {
	for k := range h {
		if k == "" {
			return fmt.Errorf("config: source %d header name is empty", i)
		}
		if strings.ContainsAny(k, " \t\r\n") {
			return fmt.Errorf("config: source %d header name invalid: %q", i, k)
		}
	}
	return nil
}

// validateBreakerNonNegative 拒绝断路器数值字段的负值：applyDefaults 只补零值，
// 负值会原样进入运行时（负超时立即熔断、负阈值永不降级）。
func validateBreakerNonNegative(scope string, b *BreakerCfg) error {
	checks := []struct {
		name string
		v    int64
	}{
		{"first_byte_timeout", int64(b.FirstByteTimeout)},
		{"request_timeout", int64(b.RequestTimeout)},
		{"circuit_interval", int64(b.CircuitInterval)},
		{"degrade_interval", int64(b.DegradeInterval)},
		{"degrade_threshold", int64(b.DegradeThreshold)},
		{"degraded_recovery_threshold", int64(b.DegradedRecoveryThreshold)},
		{"circuit_recovery_threshold", int64(b.CircuitRecoveryThreshold)},
		{"max_retries", int64(b.MaxRetries)},
	}
	for _, c := range checks {
		if c.v < 0 {
			return fmt.Errorf("config: %s.%s must be >= 0, got %d", scope, c.name, c.v)
		}
	}
	return nil
}

// applyDefaults fills zero-valued fields in b with values from def.
func applyDefaults(b, def BreakerCfg) BreakerCfg {
	if b.FirstByteTimeout == 0 {
		b.FirstByteTimeout = def.FirstByteTimeout
	}
	if b.RequestTimeout == 0 {
		b.RequestTimeout = def.RequestTimeout
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
	if b.DegradedRecoveryThreshold == 0 {
		b.DegradedRecoveryThreshold = def.DegradedRecoveryThreshold
	}
	if b.CircuitRecoveryThreshold == 0 {
		b.CircuitRecoveryThreshold = def.CircuitRecoveryThreshold
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
	if m.RequestTimeout != 0 {
		merged.RequestTimeout = m.RequestTimeout
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
	if m.DegradedRecoveryThreshold != 0 {
		merged.DegradedRecoveryThreshold = m.DegradedRecoveryThreshold
	}
	if m.CircuitRecoveryThreshold != 0 {
		merged.CircuitRecoveryThreshold = m.CircuitRecoveryThreshold
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
		slog.Warn("忽略已废弃配置字段", "field", "cache", "replacement", "anthropic.cache_enabled")
	}
	scanDeprecated(raw)
}

// rejectLegacyConfigShape 在 Config v2 下显式拒绝旧格式：source 级 backend_type、
// 顶层 github_token、source 级 github_token，以及缺少 backend 的 source。
// 返回的迁移错误指明明明新配置形状（stable backend + options），不自动迁移猜测。
func rejectLegacyConfigShape(data []byte) error {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return nil // 语法错误由后续 koanf 解析报告
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
		switch key.Value {
		case "github_token":
			return fmt.Errorf("config: top-level github_token is removed; move it into the corresponding source options.github_token")
		case "backend_type":
			return fmt.Errorf("config: backend_type is removed; set backend to a registered source plugin id")
		case "sources":
			if val.Kind != yamlv3.SequenceNode {
				continue
			}
			for _, item := range val.Content {
				if err := rejectLegacySourceFields(item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// rejectLegacySourceFields 检查单个 source 节点的旧表达字段。
func rejectLegacySourceFields(node *yamlv3.Node) error {
	if node.Kind != yamlv3.MappingNode {
		return nil
	}
	var hasBackend bool
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		switch key.Value {
		case "backend_type":
			return fmt.Errorf("config: source backend_type is removed; set backend to a registered source plugin id and move backend-specific settings into options")
		case "github_token":
			return fmt.Errorf("config: source github_token is removed; move it into options.github_token")
		case "backend":
			hasBackend = true
		}
	}
	if !hasBackend {
		return fmt.Errorf("config: source is missing backend; set backend to a registered source plugin id")
	}
	return nil
}

// scanDeprecated recursively walks a parsed YAML map for deprecated keys.
func scanDeprecated(m map[string]any) {
	for k, v := range m {
		switch k {
		case "priority":
			slog.Warn("忽略已废弃配置字段", "field", "priority", "replacement", "sources list order")
		case "failure_threshold":
			slog.Warn("忽略已废弃配置字段", "field", "failure_threshold", "replacement", "degrade_threshold")
		case "cache_ttl":
			slog.Warn("忽略已废弃配置字段", "field", "anthropic.cache_ttl", "replacement", "固定 5m，不再可配")
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
`

// WriteDefault 写入最小默认配置到 path。目录不存在时创建。
// path 已存在时返回错误而不覆盖：现有文件可能只是暂时解析失败
// （语法笔误等），覆盖会丢失用户的全部 sources 配置。
func WriteDefault(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("write default config: %w", err)
	}
	if _, err := f.Write([]byte(defaultConfigYAML)); err != nil {
		_ = f.Close()
		return fmt.Errorf("write default config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write default config: %w", err)
	}
	return nil
}
