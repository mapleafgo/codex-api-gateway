package config

import (
	"os"
	"path/filepath"
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
breaker: {first_byte_timeout: 8s, degrade_threshold: 3, recover_threshold: 1, cooldown: 20s, half_open_probes: 1, recovery: normal}
sources:
  - name: official
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

func TestLoadExpandsInlineEnvPlaceholders(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "secret123")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: official
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

func TestDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: s1
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
	if cfg.Breaker.Cooldown != Duration(30*time.Second) {
		t.Fatalf("default cooldown: got %v, want 30s", cfg.Breaker.Cooldown)
	}
	if cfg.Breaker.DegradeThreshold != 3 {
		t.Fatalf("default degrade_threshold: got %d, want 3", cfg.Breaker.DegradeThreshold)
	}
	if cfg.Breaker.RecoverThreshold != 1 {
		t.Fatalf("default recover_threshold: got %d, want 1", cfg.Breaker.RecoverThreshold)
	}
	if cfg.Breaker.HalfOpenProbes != 1 {
		t.Fatalf("default half_open_probes: got %d, want 1", cfg.Breaker.HalfOpenProbes)
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
	if cfg.Cache.TTL != "5m" {
		t.Fatalf("default cache.ttl: got %q, want 5m", cfg.Cache.TTL)
	}
}

func TestCacheTTLValidatesWhitelist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
sources:
  - name: s1
    base_url: http://upstream
cache:
  ttl: 30m
`), 0644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected cache.ttl whitelist error for 30m, got nil")
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
			FirstByteTimeout: Duration(12 * time.Second),
			Cooldown:         Duration(60 * time.Second),
			DegradeThreshold: 3,
			RecoverThreshold: 1,
			HalfOpenProbes:   1,
			MaxRetries:       2,
			Recovery:         "normal",
		},
	}
	src := &Source{
		Breaker: &BreakerCfg{
			Cooldown:         Duration(10 * time.Second),
			DegradeThreshold: 5,
		},
	}
	merged := c.BreakerFor(src)
	// Overridden by per-source
	if merged.Cooldown != Duration(10*time.Second) {
		t.Fatalf("per-source cooldown not merged: got %v", merged.Cooldown)
	}
	if merged.DegradeThreshold != 5 {
		t.Fatalf("per-source degrade_threshold not merged: got %d", merged.DegradeThreshold)
	}
	// Inherited from global (per-source zero = inherit)
	if merged.FirstByteTimeout != Duration(12*time.Second) {
		t.Fatalf("global first_byte_timeout not inherited: got %v", merged.FirstByteTimeout)
	}
	if merged.RecoverThreshold != 1 {
		t.Fatalf("global recover_threshold not inherited: got %d", merged.RecoverThreshold)
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
			Cooldown:         Duration(60 * time.Second),
			DegradeThreshold: 3,
		},
	}
	src := &Source{}
	merged := c.BreakerFor(src)
	if merged.Cooldown != Duration(60*time.Second) {
		t.Fatalf("should return global when no per-source breaker: got %v", merged.Cooldown)
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

func TestConfiguredModelSlugsEmpty(t *testing.T) {
	c := &Config{}
	models := c.ConfiguredModelSlugs()
	if len(models) != 0 {
		t.Fatalf("expected empty model list, got %v", models)
	}
}

// TestLoadModelOverridesParsesSupportsImage 验证 models.<slug> 下的 supports_image
// 配置能被正确解析到 ModelOverride.SupportsImageDetailOriginal，并经 codexModelInfo
// 输出为 Codex 的 supports_image_detail_original JSON 字段。
// yaml key 故意用 supports_image（简洁），输出仍对齐 Codex 原字段名。
func TestLoadModelOverridesParsesSupportsImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
server: {listen: ":9090"}
sources:
  - name: official
    base_url: https://api.anthropic.com
    api_key: yaml-secret
models:
  gpt-5:
    context_window: 200000
    supports_image: true
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
		t.Fatalf("supports_image 未解析到 SupportsImageDetailOriginal: %v", ov.SupportsImageDetailOriginal)
	}
}

// TestLoadBaseInstructionsFile 验证 base_instructions_file 文件加载：
// 相对路径基于 config 文件目录解析，内容写入 cfg.BaseInstructions。
func TestLoadBaseInstructionsFile(t *testing.T) {
	dir := t.TempDir()
	const content = "You are a test agent with gateway_guidance."
	biPath := filepath.Join(dir, "bi.txt")
	if err := os.WriteFile(biPath, []byte(content), 0644); err != nil {
		t.Fatalf("write bi file: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte(`
server: {listen: ":9090"}
breaker: {first_byte_timeout: 8s, cooldown: 20s, degrade_threshold: 3, recover_threshold: 1, half_open_probes: 1, recovery: normal}
base_instructions_file: bi.txt
sources:
  - name: official
    base_url: https://api.anthropic.com
    api_key: k
`), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.BaseInstructions != content {
		t.Fatalf("BaseInstructions = %q, want %q", cfg.BaseInstructions, content)
	}
	if cfg.BaseInstructionsFile != "bi.txt" {
		t.Fatalf("BaseInstructionsFile = %q, want bi.txt", cfg.BaseInstructionsFile)
	}
}

// TestLoadBaseInstructionsFileMissing 验证降级策略：文件缺失不阻断启动，
// BaseInstructions 保持空串（沿用 Codex 内置指令），仅在日志输出 WARN。
func TestLoadBaseInstructionsFileMissing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte(`
breaker: {first_byte_timeout: 8s, cooldown: 20s, degrade_threshold: 3, recover_threshold: 1, half_open_probes: 1, recovery: normal}
base_instructions_file: nope.txt
sources:
  - name: official
    base_url: https://api.anthropic.com
    api_key: k
`), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("文件缺失应降级而非报错, got: %v", err)
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
breaker: {first_byte_timeout: 8s, cooldown: 20s, degrade_threshold: 3, recover_threshold: 1, half_open_probes: 1, recovery: normal}
system_suffix: "legacy"
sources:
  - name: official
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
	if cfg.Server.Listen != ":8080" {
		t.Errorf("listen = %q, want :8080", cfg.Server.Listen)
	}
	if cfg.Logging.Level != "info" || cfg.Logging.Format != "text" {
		t.Errorf("logging = %+v, want info/text", cfg.Logging)
	}
	if len(cfg.Sources) != 0 {
		t.Errorf("默认配置应零源，实际 %d", len(cfg.Sources))
	}
}
