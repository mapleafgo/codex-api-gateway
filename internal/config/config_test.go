package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadInterpolatesEnv(t *testing.T) {
	os.Setenv("TEST_ANTHROPIC_KEY", "secret123")
	defer os.Unsetenv("TEST_ANTHROPIC_KEY")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
server: {listen: ":9090"}
session: {ttl: 30m, max_entries: 5}
breaker: {first_byte_timeout: 8s, degrade_threshold: 3, recover_threshold: 1, cooldown: 20s, half_open_probes: 1, recovery: normal}
thinking: {effort_budget: {minimal: 1024, low: 8000, medium: 16000, high: 32000}}
sources:
  - name: official
    base_url: https://api.anthropic.com
    api_key: ${TEST_ANTHROPIC_KEY}
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
		t.Fatalf("env not interpolated: %q", cfg.Sources[0].APIKey)
	}
	if cfg.EffortBudget("medium") != 16000 {
		t.Fatalf("bad effort budget")
	}
	if cfg.Breaker.DegradeThreshold != 3 {
		t.Fatalf("bad degrade_threshold: %d", cfg.Breaker.DegradeThreshold)
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
	// Session defaults
	if cfg.Session.MaxEntries != 10000 {
		t.Fatalf("default max_entries: got %d, want 10000", cfg.Session.MaxEntries)
	}
	if cfg.Session.TTL != Duration(time.Hour) {
		t.Fatalf("default ttl: got %v, want 1h", cfg.Session.TTL)
	}
}

func TestSessionMaxEntriesMinusOneMeansUnlimited(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
session:
  max_entries: -1
sources:
  - name: s1
    base_url: http://upstream
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Session.MaxEntries != -1 {
		t.Fatalf("max_entries=-1 should be preserved for unlimited storage, got %d", cfg.Session.MaxEntries)
	}
}

func TestSessionMaxEntriesRejectsLessThanMinusOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
session:
  max_entries: -2
sources:
  - name: s1
    base_url: http://upstream
`), 0644)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected error for max_entries < -1")
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
