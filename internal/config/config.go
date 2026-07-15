// Package config loads and validates YAML configuration.
package config

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultSessionMaxEntries is used when session.max_entries is omitted or 0.
const DefaultSessionMaxEntries = 10000

// Config is the top-level YAML configuration.
type Config struct {
	Server   ServerCfg   `yaml:"server"`
	Session  SessionCfg  `yaml:"session"`
	Breaker  BreakerCfg  `yaml:"breaker"`
	Thinking ThinkingCfg `yaml:"thinking"`
	Sources  []Source    `yaml:"sources"`
}

// ServerCfg configures the HTTP listener.
type ServerCfg struct {
	Listen string `yaml:"listen"`
}

// SessionCfg configures previous_response_id session storage.
type SessionCfg struct {
	TTL        Duration `yaml:"ttl"`
	MaxEntries int      `yaml:"max_entries"`
}

// BreakerCfg configures upstream failover and circuit breaking.
type BreakerCfg struct {
	FirstByteTimeout Duration `yaml:"first_byte_timeout"`
	Cooldown         Duration `yaml:"cooldown"`
	DegradeThreshold int      `yaml:"degrade_threshold"`
	RecoverThreshold int      `yaml:"recover_threshold"`
	HalfOpenProbes   int      `yaml:"half_open_probes"`
	MaxRetries       int      `yaml:"max_retries"`
	Recovery         string   `yaml:"recovery"`
}

// ThinkingCfg maps Responses reasoning effort values to Anthropic thinking budgets.
type ThinkingCfg struct {
	EffortBudget map[string]int `yaml:"effort_budget"`
}

// Source configures one Anthropic-compatible upstream.
type Source struct {
	Name          string            `yaml:"name"`
	BaseURL       string            `yaml:"base_url"`
	APIKey        string            `yaml:"api_key"`
	ModelMap      map[string]string `yaml:"model_map"`
	DefaultModel  string            `yaml:"default_model"`
	Breaker       *BreakerCfg       `yaml:"breaker"`
	OriginalIndex int               `yaml:"-"`
}

// Duration wraps time.Duration for YAML parsing.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string from YAML.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

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
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Sources) == 0 {
		return fmt.Errorf("config: at least one source required")
	}
	if c.Session.MaxEntries < -1 {
		return fmt.Errorf("config: session.max_entries must be -1 (unlimited), 0 (default), or positive, got %d", c.Session.MaxEntries)
	}
	if c.Session.MaxEntries == 0 {
		c.Session.MaxEntries = DefaultSessionMaxEntries
	}
	if c.Session.TTL == 0 {
		c.Session.TTL = Duration(time.Hour)
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

// EffortBudget returns budget_tokens for an effort level (default 8000).
func (c *Config) EffortBudget(effort string) int {
	if v, ok := c.Thinking.EffortBudget[effort]; ok {
		return v
	}
	return 8000
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
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return // real parse will report the error
	}
	scanDeprecated(raw)
}

// scanDeprecated recursively walks a parsed YAML map for deprecated keys.
func scanDeprecated(m map[string]any) {
	for k, v := range m {
		switch k {
		case "priority":
			log.Printf("[config] ignored deprecated field 'priority' (sources now use list order)")
		case "failure_threshold":
			log.Printf("[config] ignored deprecated field 'failure_threshold' (use degrade_threshold)")
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

// Models 收集所有源 model_map 中的 OpenAI 侧模型名称（去重并按字母序排序）。
// 这些是网关向客户端暴露的本地别名，与上游返回的模型列表合并后供 /v1/models 接口返回。
func (c *Config) Models() []string {
	seen := make(map[string]bool)
	var models []string
	for _, s := range c.Sources {
		for name := range s.ModelMap {
			if !seen[name] {
				seen[name] = true
				models = append(models, name)
			}
		}
	}
	sort.Strings(models)
	return models
}
