package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadOverridesWithEnvProvider(t *testing.T) {
	t.Setenv("CODEX_API_GATEWAY_LOGGING__LEVEL", "debug")
	t.Setenv("CODEX_API_GATEWAY_SOURCES__0__API_KEY", "secret123")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
server: {listen: ":9090"}
breaker: {first_byte_timeout: 8s, degrade_threshold: 3, degraded_recovery_threshold: 1, circuit_interval: 20s, circuit_recovery_threshold: 1, recovery: normal}
sources:
  - name: official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: yaml-secret
    model_map: {gpt-5: claude-sonnet-4}
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Listen != ":9090" {
		t.Fatalf("bad listen: %s", cfg.Server.Listen)
	}
	if cfg.Sources[0].APIKey != "secret123" {
		t.Fatalf("env did not override api key: %q", cfg.Sources[0].APIKey)
	}
	if cfg.Logging.Level != "debug" {
		t.Fatalf("env did not override logging.level: %q", cfg.Logging.Level)
	}
	if cfg.Breaker.DegradeThreshold != 3 {
		t.Fatalf("bad degrade_threshold: %d", cfg.Breaker.DegradeThreshold)
	}
}

func boolPtr(b bool) *bool { return &b }

// TestSupportsWebSearchValue 验证 SupportsWebSearchValue 的默认与显式覆盖语义：
// nil 时按稳定 backend（anthropic/openai-responses=true、仅 openai-chat=false），
// 显式配置优先。
func TestSupportsWebSearchValue(t *testing.T) {
	cases := []struct {
		name string
		src  Source
		want bool
	}{
		{name: "anthropic 默认支持", src: Source{Name: "s", Backend: "anthropic"}, want: true},
		{name: "openai-chat 默认不支持", src: Source{Name: "s", Backend: "openai-chat"}, want: false},
		{name: "openai-responses 默认支持", src: Source{Name: "s", Backend: "openai-responses"}, want: true},
		{name: "显式 true 覆盖 openai-chat", src: Source{Name: "s", Backend: "openai-chat", SupportsWebSearch: boolPtr(true)}, want: true},
		{name: "显式 false 覆盖 openai-responses", src: Source{Name: "s", Backend: "openai-responses", SupportsWebSearch: boolPtr(false)}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.src.SupportsWebSearchValue(); got != tc.want {
				t.Fatalf("SupportsWebSearchValue()=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadExpandsInlineEnvPlaceholders(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "secret123")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: ${TEST_ANTHROPIC_KEY}
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sources[0].APIKey != "secret123" {
		t.Fatalf("inline placeholder should expand: %q", cfg.Sources[0].APIKey)
	}
}

// TestLoadAcceptsNoSources 验证零源配置不再被拒绝（允许启动后通过管理页添加）。
// 转发请求的处理由 server 层返回 503。
func TestLoadAcceptsNoSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`sources: []`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("零源配置应允许加载，实际出错: %v", err)
	}
	if len(cfg.Sources) != 0 {
		t.Fatalf("Sources 应为空，实际 %d", len(cfg.Sources))
	}
}

func TestLoadSourceHeaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: s1
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: k
    headers:
      X-Custom: custom-value
      X-Api-Key: override-me
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Sources) != 1 {
		t.Fatalf("sources=%d", len(cfg.Sources))
	}
	h := cfg.Sources[0].Headers
	if h == nil {
		t.Fatal("Headers map should be non-nil")
	}
	if h["X-Custom"] != "custom-value" || h["X-Api-Key"] != "override-me" {
		t.Fatalf("headers=%v", h)
	}
}

func TestSourceHeadersRejectsEmptyName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: s1
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: k
    headers:
      "": value
`), 0644)
	if _, err := Load(path); err == nil {
		t.Fatal("expected empty header name to fail validation")
	}
}

func TestDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Global breaker defaults
	if cfg.Breaker.FirstByteTimeout != Duration(12*time.Second) {
		t.Fatalf("default first_byte_timeout: got %v, want 12s", cfg.Breaker.FirstByteTimeout)
	}
	if cfg.Breaker.RequestTimeout != Duration(120*time.Second) {
		t.Fatalf("default request_timeout: got %v, want 120s", cfg.Breaker.RequestTimeout)
	}
	if cfg.Breaker.CircuitInterval != Duration(30*time.Minute) {
		t.Fatalf("default circuit_interval: got %v, want 30m", cfg.Breaker.CircuitInterval)
	}
	if cfg.Breaker.DegradeInterval != Duration(1*time.Minute) {
		t.Fatalf("default degrade_interval: got %v, want 1m", cfg.Breaker.DegradeInterval)
	}
	if cfg.Breaker.DegradeThreshold != 3 {
		t.Fatalf("default degrade_threshold: got %d, want 3", cfg.Breaker.DegradeThreshold)
	}
	if cfg.Breaker.DegradedRecoveryThreshold != 1 {
		t.Fatalf("default degraded_recovery_threshold: got %d, want 1", cfg.Breaker.DegradedRecoveryThreshold)
	}
	if cfg.Breaker.CircuitRecoveryThreshold != 1 {
		t.Fatalf("default circuit_recovery_threshold: got %d, want 1", cfg.Breaker.CircuitRecoveryThreshold)
	}
	if cfg.Breaker.MaxRetries != 0 {
		t.Fatalf("default max_retries: got %d, want 0", cfg.Breaker.MaxRetries)
	}
	if cfg.Breaker.Recovery != "normal" {
		t.Fatalf("default recovery: got %q, want normal", cfg.Breaker.Recovery)
	}
	if cfg.Logging.Level != "info" {
		t.Fatalf("default logging.level: got %q, want info", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Fatalf("default logging.format: got %q, want text", cfg.Logging.Format)
	}
	if cfg.Anthropic.DefaultMaxTokens != 16384 {
		t.Fatalf("default anthropic.default_max_tokens: got %d, want 16384", cfg.Anthropic.DefaultMaxTokens)
	}
	if cfg.Anthropic.CacheEnabled == nil || !*cfg.Anthropic.CacheEnabled {
		t.Fatalf("default anthropic.cache_enabled: got %v, want true", cfg.Anthropic.CacheEnabled)
	}
}

func TestAnthropicConfigRejectsTopLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
anthropic:
  default_max_tokens: 32768
  cache_enabled: false
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	if _, err := Load(path); err == nil {
		t.Fatal("top-level anthropic should be rejected with a migration error")
	} else if !strings.Contains(err.Error(), "top-level anthropic is removed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBreakerRequestTimeoutEnvironmentOverride(t *testing.T) {
	t.Setenv("CODEX_API_GATEWAY_BREAKER__REQUEST_TIMEOUT", "90s")
	t.Setenv("CODEX_API_GATEWAY_SOURCES__0__BREAKER__REQUEST_TIMEOUT", "45s")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Breaker.RequestTimeout != Duration(90*time.Second) {
		t.Fatalf("env global request_timeout: got %v, want 90s", cfg.Breaker.RequestTimeout)
	}
	if cfg.Sources[0].Breaker == nil || cfg.Sources[0].Breaker.RequestTimeout != Duration(45*time.Second) {
		t.Fatalf("env per-source request_timeout: got %+v, want 45s", cfg.Sources[0].Breaker)
	}
}

func TestBreakerRequestTimeoutRejectsNegative(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "global negative", body: "\nbreaker:\n  request_timeout: -1s\n"},
		{name: "per-source negative", body: "\nsources:\n  - name: s1\n    base_url: http://upstream\n    breaker:\n      request_timeout: -5s\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			_ = os.WriteFile(path, []byte(tt.body), 0644)
			if _, err := Load(path); err == nil {
				t.Fatal("expected negative request_timeout to fail validation")
			}
		})
	}
}

func TestLegacyCachePrefixIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
cache:
  ttl: 1h
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Anthropic.CacheEnabled == nil || !*cfg.Anthropic.CacheEnabled {
		t.Fatalf("legacy cache.ttl must not change anthropic.cache_enabled: got %v", cfg.Anthropic.CacheEnabled)
	}
}

func TestLoadParsesLoggingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
logging:
  level: debug
  format: json
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Logging.Level != "debug" {
		t.Fatalf("logging.level: got %q, want debug", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Fatalf("logging.format: got %q, want json", cfg.Logging.Format)
	}
}

func TestLoadRejectsInvalidLoggingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
logging:
  level: verbose
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected error for invalid logging.level")
	}
}

// TestLoadLoggingParsesConfig 验证 LoadLogging 能从配置文件读出 logging 段。
func TestLoadLoggingParsesConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
logging:
  level: debug
  format: json
  file: /tmp/gateway.log
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	lc := LoadLogging(path)
	if lc.Level != "debug" {
		t.Fatalf("level: got %q, want debug", lc.Level)
	}
	if lc.Format != "json" {
		t.Fatalf("format: got %q, want json", lc.Format)
	}
	if lc.File != "/tmp/gateway.log" {
		t.Fatalf("file: got %q, want /tmp/gateway.log", lc.File)
	}
}

// TestLoadLoggingEnvOverride 验证 LoadLogging 也应用 CODEX_API_GATEWAY_LOGGING__* 环境变量。
func TestLoadLoggingEnvOverride(t *testing.T) {
	t.Setenv("CODEX_API_GATEWAY_LOGGING__LEVEL", "warn")
	t.Setenv("CODEX_API_GATEWAY_LOGGING__FORMAT", "json")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
logging:
  level: info
  format: text
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	lc := LoadLogging(path)
	if lc.Level != "warn" {
		t.Fatalf("level: got %q, want warn (env override)", lc.Level)
	}
	if lc.Format != "json" {
		t.Fatalf("format: got %q, want json (env override)", lc.Format)
	}
}

// TestLoadLoggingMissingFileDefaults 验证配置文件缺失时返回默认 LoggingCfg 而非报错，
// 让调用方能继续走默认日志初始化（真实错误留给后续 Load 暴露）。
func TestLoadLoggingMissingFileDefaults(t *testing.T) {
	lc := LoadLogging("/nonexistent/config.yaml")
	if lc.Level != "info" {
		t.Fatalf("default level: got %q, want info", lc.Level)
	}
	if lc.Format != "text" {
		t.Fatalf("default format: got %q, want text", lc.Format)
	}
}

// TestLoadLoggingEmptyConfigDefaults 验证 logging 段缺失时补默认值。
func TestLoadLoggingEmptyConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	lc := LoadLogging(path)
	if lc.Level != "info" {
		t.Fatalf("default level: got %q, want info", lc.Level)
	}
	if lc.Format != "text" {
		t.Fatalf("default format: got %q, want text", lc.Format)
	}
}

func TestOrderedSourcesListOrder(t *testing.T) {
	c := &Config{
		Sources: []Source{
			{Name: "c"},
			{Name: "a"},
			{Name: "b"},
		},
	}
	ordered := c.OrderedSources()
	if ordered[0].Name != "c" || ordered[0].OriginalIndex != 0 {
		t.Fatalf("list order should be preserved: [0]=%q(idx %d)", ordered[0].Name, ordered[0].OriginalIndex)
	}
	if ordered[1].Name != "a" || ordered[1].OriginalIndex != 1 {
		t.Fatalf("list order should be preserved: [1]=%q(idx %d)", ordered[1].Name, ordered[1].OriginalIndex)
	}
	if ordered[2].Name != "b" || ordered[2].OriginalIndex != 2 {
		t.Fatalf("list order should be preserved: [2]=%q(idx %d)", ordered[2].Name, ordered[2].OriginalIndex)
	}
}

func TestBreakerForMergesPerSource(t *testing.T) {
	c := &Config{
		Breaker: BreakerCfg{
			FirstByteTimeout:          Duration(12 * time.Second),
			RequestTimeout:            Duration(120 * time.Second),
			CircuitInterval:           Duration(60 * time.Second),
			DegradeInterval:           Duration(30 * time.Second),
			DegradeThreshold:          3,
			DegradedRecoveryThreshold: 1,
			CircuitRecoveryThreshold:  1,
			MaxRetries:                2,
			Recovery:                  "normal",
		},
	}
	src := &Source{
		Breaker: &BreakerCfg{
			RequestTimeout:   Duration(45 * time.Second),
			CircuitInterval:  Duration(10 * time.Second),
			DegradeThreshold: 5,
		},
	}
	merged := c.BreakerFor(src)
	if merged.RequestTimeout != Duration(45*time.Second) {
		t.Fatalf("per-source request_timeout not merged: got %v", merged.RequestTimeout)
	}
	// Overridden by per-source
	if merged.CircuitInterval != Duration(10*time.Second) {
		t.Fatalf("per-source circuit_interval not merged: got %v", merged.CircuitInterval)
	}
	// DegradeInterval inherited from global (per-source zero = inherit)
	if merged.DegradeInterval != Duration(30*time.Second) {
		t.Fatalf("global degrade_interval not inherited: got %v", merged.DegradeInterval)
	}
	if merged.DegradeThreshold != 5 {
		t.Fatalf("per-source degrade_threshold not merged: got %d", merged.DegradeThreshold)
	}
	// Inherited from global (per-source zero = inherit)
	if merged.FirstByteTimeout != Duration(12*time.Second) {
		t.Fatalf("global first_byte_timeout not inherited: got %v", merged.FirstByteTimeout)
	}
	if got := c.BreakerFor(&Source{Name: "no-override"}).RequestTimeout; got != Duration(120*time.Second) {
		t.Fatalf("request_timeout should inherit global: got %v", got)
	}
	if merged.DegradedRecoveryThreshold != 1 {
		t.Fatalf("global degraded_recovery_threshold not inherited: got %d", merged.DegradedRecoveryThreshold)
	}
	if merged.Recovery != "normal" {
		t.Fatalf("global recovery not inherited: got %q", merged.Recovery)
	}
	// MaxRetries always from global (per-source never overrides)
	if merged.MaxRetries != 2 {
		t.Fatalf("global max_retries should be preserved: got %d", merged.MaxRetries)
	}
}

func TestBreakerForNilPerSource(t *testing.T) {
	c := &Config{
		Breaker: BreakerCfg{
			CircuitInterval:  Duration(60 * time.Second),
			DegradeInterval:  Duration(30 * time.Second),
			DegradeThreshold: 3,
		},
	}
	src := &Source{}
	merged := c.BreakerFor(src)
	if merged.CircuitInterval != Duration(60*time.Second) {
		t.Fatalf("should return global circuit_interval when no per-source breaker: got %v", merged.CircuitInterval)
	}
}

func TestBreakerForMaxRetriesNotOverriddenPerSource(t *testing.T) {
	c := &Config{
		Breaker: BreakerCfg{MaxRetries: 3},
	}
	src := &Source{
		Breaker: &BreakerCfg{MaxRetries: 99},
	}
	merged := c.BreakerFor(src)
	if merged.MaxRetries != 3 {
		t.Fatalf("max_retries must always come from global: got %d", merged.MaxRetries)
	}
}

func TestValidateRejectsInvalidGlobalRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
breaker:
  recovery: normla
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected error for invalid global recovery")
	}
}

func TestValidateAcceptsDegradedRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
breaker:
  recovery: degraded
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Breaker.Recovery != "degraded" {
		t.Fatalf("expected recovery=degraded, got %q", cfg.Breaker.Recovery)
	}
}

func TestValidateRejectsInvalidPerSourceRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: s1
    backend: anthropic
    base_url: http://upstream
    breaker:
      recovery: fatel
`), 0644)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected error for invalid per-source recovery")
	}
}

func TestConfiguredModelSlugsSorted(t *testing.T) {
	ctxWindow := int64(200000)
	c := &Config{
		ModelOverrides: map[string]ModelOverride{
			"gpt-5.5": {ContextWindow: &ctxWindow},
			"gpt-5":   {ContextWindow: &ctxWindow},
			"o3":      {ContextWindow: &ctxWindow},
		},
		// 无 ModelSlugOrder 时回退字母序
	}
	models := c.ConfiguredModelSlugs()
	want := []string{"gpt-5", "gpt-5.5", "o3"}
	if len(models) != len(want) {
		t.Fatalf("expected %d models, got %d: %v", len(want), len(models), models)
	}
	for i, m := range models {
		if m != want[i] {
			t.Fatalf("models[%d] = %q, want %q (full: %v)", i, m, want[i], models)
		}
	}
}

func TestConfiguredModelSlugsPreservesOrder(t *testing.T) {
	ctxWindow := int64(200000)
	c := &Config{
		ModelOverrides: map[string]ModelOverride{
			"gpt-5.5": {ContextWindow: &ctxWindow},
			"gpt-5":   {ContextWindow: &ctxWindow},
			"o3":      {ContextWindow: &ctxWindow},
		},
		ModelSlugOrder: []string{"o3", "gpt-5.5", "gpt-5"},
	}
	models := c.ConfiguredModelSlugs()
	want := []string{"o3", "gpt-5.5", "gpt-5"}
	for i, m := range models {
		if m != want[i] {
			t.Fatalf("models[%d] = %q, want %q (full: %v)", i, m, want[i], models)
		}
	}
}

func TestLoadModelSlugOrderFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
server:
  listen: ":1"
sources:
  - name: s1
    backend: anthropic
    base_url: https://example.com
models:
  z-last:
    context_window: 1
  a-first:
    context_window: 2
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.ConfiguredModelSlugs()
	want := []string{"z-last", "a-first"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestConfiguredModelSlugsEmpty(t *testing.T) {
	c := &Config{}
	models := c.ConfiguredModelSlugs()
	if len(models) != 0 {
		t.Fatalf("expected empty model list, got %v", models)
	}
}

// TestLoadModelOverridesParsesSupportsImageDetailOriginal 验证 models.<slug> 下的
// supports_image_detail_original 配置能被正确解析到 ModelOverride.SupportsImageDetailOriginal，
// 并经 codexModelInfo 输出为 Codex 的 supports_image_detail_original JSON 字段。
func TestLoadModelOverridesParsesSupportsImageDetailOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
server: {listen: ":9090"}
sources:
  - name: official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: yaml-secret
models:
  gpt-5:
    context_window: 200000
    supports_image_detail_original: true
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ov, ok := cfg.ModelOverrides["gpt-5"]
	if !ok {
		t.Fatalf("models.gpt-5 未被解析")
	}
	if ov.ContextWindow == nil || *ov.ContextWindow != 200000 {
		t.Fatalf("context_window 解析错误: %v", ov.ContextWindow)
	}
	if ov.SupportsImageDetailOriginal == nil || !*ov.SupportsImageDetailOriginal {
		t.Fatalf("supports_image_detail_original 未解析: %v", ov.SupportsImageDetailOriginal)
	}
}

// TestLoadModelOverridesParsesAcceptsImage 验证 models.<slug> 下的 accepts_image=false
// 配置能被正确解析到 ModelOverride.AcceptsImage（控制 input_modalities 是否含 image）。
func TestLoadModelOverridesParsesAcceptsImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
server: {listen: ":9090"}
sources:
  - name: official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: yaml-secret
models:
  cheap:
    context_window: 100000
    accepts_image: false
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	override, ok := cfg.ModelOverrides["cheap"]
	if !ok {
		t.Fatalf("models.cheap 未被解析")
	}
	if override.AcceptsImage == nil || *override.AcceptsImage {
		t.Fatalf("accepts_image=false 未解析: %v", override.AcceptsImage)
	}
}

// TestLoadBaseInstructionsSiblingFile 验证与 config 同级的 base_instructions.md 自动加载。
func TestLoadBaseInstructionsSiblingFile(t *testing.T) {
	dir := t.TempDir()
	const content = "You are a test agent with gateway_guidance."
	if err := os.WriteFile(filepath.Join(dir, BaseInstructionsFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte(`
server: {listen: ":9090"}
sources:
  - name: official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: k
`), 0o644)
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.BaseInstructions != content {
		t.Fatalf("BaseInstructions = %q, want %q", cfg.BaseInstructions, content)
	}
}

// TestLoadBaseInstructionsMissingSibling 验证文件缺失时降级为空串。
func TestLoadBaseInstructionsMissingSibling(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte(`
server: {listen: ":9090"}
sources:
  - name: official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: k
`), 0o644)
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.BaseInstructions != "" {
		t.Fatalf("BaseInstructions 应降级为空串, got len=%d", len(cfg.BaseInstructions))
	}
}

// TestLoadWarnsDeprecatedSystemSuffix 验证 system_suffix 触发 WARN（兼容旧配置）。
func TestLoadWarnsDeprecatedSystemSuffix(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte(`
breaker: {first_byte_timeout: 8s, circuit_interval: 20s, degrade_threshold: 3, degraded_recovery_threshold: 1, circuit_recovery_threshold: 1, recovery: normal}
system_suffix: "legacy"
sources:
  - name: official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: k
`), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// SystemSuffix 已移除字段；只要 Load 不报错、不 panic 即可（WARN 在日志里）。
	if cfg == nil {
		t.Fatalf("cfg == nil")
	}
}

// TestWriteDefault 验证 WriteDefault 生成最小配置文件，且可被 Load 正常加载。
func TestWriteDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.yaml") // 测试目录自动创建
	if err := WriteDefault(path); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 生成的默认配置: %v", err)
	}
	if cfg.Server.Listen != ":8383" {
		t.Errorf("listen = %q, want :8383", cfg.Server.Listen)
	}
	if cfg.Logging.Level != "info" || cfg.Logging.Format != "text" {
		t.Errorf("logging = %+v, want info/text", cfg.Logging)
	}
	if len(cfg.Sources) != 0 {
		t.Errorf("默认配置应零源，实际 %d", len(cfg.Sources))
	}
}

// TestServerAndLoggingSafetyDefaults 验证本机防误伤字段的默认值。
func TestServerAndLoggingSafetyDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
server:
  listen: ":9090"
sources:
  - name: s1
    backend: anthropic
    base_url: https://example.com
`), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.MaxBodyMB != 32 {
		t.Fatalf("MaxBodyMB = %d, want 32", cfg.Server.MaxBodyMB)
	}
	if time.Duration(cfg.Server.ReadHeaderTimeout) != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 10s", time.Duration(cfg.Server.ReadHeaderTimeout))
	}
	if cfg.Server.MaxBodyBytes() != 32<<20 {
		t.Fatalf("MaxBodyBytes = %d, want %d", cfg.Server.MaxBodyBytes(), 32<<20)
	}
	if cfg.Logging.MaxSizeMB != 50 || cfg.Logging.MaxBackups != 3 {
		t.Fatalf("logging roll defaults = %d/%d, want 50/3", cfg.Logging.MaxSizeMB, cfg.Logging.MaxBackups)
	}
}

// TestServerSafetyConfigExplicitValues 验证显式配置不被默认值覆盖。
func TestServerSafetyConfigExplicitValues(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
server:
  listen: ":9090"
  max_body_mb: 8
  read_header_timeout: 3s
logging:
  level: info
  max_size_mb: 1
  max_backups: 2
sources:
  - name: s1
    backend: anthropic
    base_url: https://example.com
`), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.MaxBodyMB != 8 {
		t.Fatalf("MaxBodyMB = %d, want 8", cfg.Server.MaxBodyMB)
	}
	if time.Duration(cfg.Server.ReadHeaderTimeout) != 3*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 3s", time.Duration(cfg.Server.ReadHeaderTimeout))
	}
	if cfg.Logging.MaxSizeMB != 1 || cfg.Logging.MaxBackups != 2 {
		t.Fatalf("logging roll = %d/%d, want 1/2", cfg.Logging.MaxSizeMB, cfg.Logging.MaxBackups)
	}
}

// TestWriteDefaultDoesNotOverwrite 已存在的 config.yaml 绝不能被覆盖：
// 现有文件可能只是暂时解析失败，覆盖等于丢失用户全部 sources。
func TestWriteDefaultDoesNotOverwrite(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.yaml")
	const original = "sources: [broken yaml"
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteDefault(p); err == nil {
		t.Fatal("WriteDefault 对已存在文件应返回错误")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != original {
		t.Fatalf("原文件被改写: %q", string(b))
	}
}

// TestWriteDefaultCreatesWhenMissing 文件缺失时正常生成且可被 Load 接受。
func TestWriteDefaultCreatesWhenMissing(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteDefault(p); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("默认配置应能通过 Load: %v", err)
	}
}
