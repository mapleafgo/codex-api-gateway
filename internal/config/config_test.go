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
session: {ttl: 30m, path: /tmp/codex-session-test, max_bytes: 1048576, max_entry_bytes: 262144}
breaker: {first_byte_timeout: 8s, degrade_threshold: 3, recover_threshold: 1, cooldown: 20s, half_open_probes: 1, recovery: normal}
thinking: {effort_budget: {minimal: 1024, low: 8000, medium: 16000, high: 32000}}
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
	if cfg.EffortBudget("medium") != 16000 {
		t.Fatalf("bad effort budget")
	}
	if cfg.Breaker.DegradeThreshold != 3 {
		t.Fatalf("bad degrade_threshold: %d", cfg.Breaker.DegradeThreshold)
	}
	if cfg.Session.MaxBytes != 1048576 {
		t.Fatalf("bad max_bytes: %d", cfg.Session.MaxBytes)
	}
	if cfg.Session.MaxEntryBytes != 262144 {
		t.Fatalf("bad max_entry_bytes: %d", cfg.Session.MaxEntryBytes)
	}
	if cfg.Session.Path != "/tmp/codex-session-test" {
		t.Fatalf("bad session path: %q", cfg.Session.Path)
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

func TestLoadRejectsNoSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`sources: []`), 0644)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected error for no sources")
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
	if cfg.Session.MaxBytes != DefaultSessionMaxBytes {
		t.Fatalf("default max_bytes: got %d, want %d", cfg.Session.MaxBytes, DefaultSessionMaxBytes)
	}
	if cfg.Session.MaxEntryBytes != DefaultSessionMaxEntryBytes {
		t.Fatalf("default max_entry_bytes: got %d, want %d", cfg.Session.MaxEntryBytes, DefaultSessionMaxEntryBytes)
	}
	if cfg.Session.Path != DefaultSessionPath {
		t.Fatalf("default session path: got %q, want %q", cfg.Session.Path, DefaultSessionPath)
	}
	if cfg.Session.TTL != Duration(time.Hour) {
		t.Fatalf("default ttl: got %v, want 1h", cfg.Session.TTL)
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

func TestLoadOverridesSessionByteBudgetFromEnv(t *testing.T) {
	t.Setenv("CODEX_API_GATEWAY_SESSION__MAX_BYTES", "2097152")
	t.Setenv("CODEX_API_GATEWAY_SESSION__MAX_ENTRY_BYTES", "524288")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
session:
  max_bytes: 1048576
  max_entry_bytes: 262144
sources:
  - name: s1
    base_url: http://upstream
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Session.MaxBytes != 2097152 {
		t.Fatalf("env max_bytes override: got %d, want 2097152", cfg.Session.MaxBytes)
	}
	if cfg.Session.MaxEntryBytes != 524288 {
		t.Fatalf("env max_entry_bytes override: got %d, want 524288", cfg.Session.MaxEntryBytes)
	}
}

func TestSessionRejectsNegativeByteBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
session:
  max_bytes: -1
sources:
  - name: s1
    base_url: http://upstream
`), 0644)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected error for negative max_bytes")
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

func TestModelsDedupAndSorted(t *testing.T) {
	c := &Config{
		Sources: []Source{
			{Name: "s1", ModelMap: map[string]string{"gpt-5": "claude-sonnet-4", "gpt-5.5": "glm-5.2"}},
			{Name: "s2", ModelMap: map[string]string{"gpt-5": "claude-opus-4", "o3": "claude-haiku"}},
			{Name: "s3"}, // 空 model_map
		},
	}
	models := c.Models()
	if len(models) != 3 {
		t.Fatalf("expected 3 unique models, got %d: %v", len(models), models)
	}
	// 应按字母序排序
	want := []string{"gpt-5", "gpt-5.5", "o3"}
	for i, m := range models {
		if m != want[i] {
			t.Fatalf("models[%d] = %q, want %q (full: %v)", i, m, want[i], models)
		}
	}
}

func TestModelsEmptyWhenNoMaps(t *testing.T) {
	c := &Config{
		Sources: []Source{{Name: "s1"}, {Name: "s2"}},
	}
	models := c.Models()
	if len(models) != 0 {
		t.Fatalf("expected empty model list, got %v", models)
	}
}
