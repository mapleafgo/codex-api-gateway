package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// stubValidator 记录被校验的 source，模拟插件级校验。
type stubValidator struct {
	ok   bool
	seen []Source
}

func (s *stubValidator) ValidateSource(src Source) error {
	s.seen = append(s.seen, src)
	if !s.ok {
		return &stubError{src: src.Name}
	}
	return nil
}

type stubError struct{ src string }

func (e *stubError) Error() string { return "stub: source " + e.src }

// registryStub 模拟真实 Registry：只认已知 backend ID，拒绝 schema 外 option。
type registryStub struct {
	known   map[string]bool
	schemas map[string]map[string]bool
}

func newRegistryStub() *registryStub {
	return &registryStub{
		known: map[string]bool{
			"anthropic": true, "openai-chat": true,
			"openai-responses": true, "github-copilot": true,
		},
		schemas: map[string]map[string]bool{
			"anthropic":        {"default_max_tokens": true, "cache_enabled": true},
			"openai-chat":      {},
			"openai-responses": {},
			"github-copilot":   {"github_token": true},
		},
	}
}

func (r *registryStub) ValidateSource(src Source) error {
	if !r.known[src.Backend] {
		return fmt.Errorf("source %q: unknown backend %q; registered backends: anthropic, github-copilot, openai-chat, openai-responses", src.Name, src.Backend)
	}
	for k := range src.Options {
		if !r.schemas[src.Backend][k] {
			return fmt.Errorf("source %q: options.%s: not declared in plugin schema", src.Name, k)
		}
	}
	return nil
}

// TestLoadFourValidBuiltins 验证四个内置源类型的合法配置均可加载。
func TestLoadFourValidBuiltins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
sources:
  - name: anthropic-official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: sk-ant-key
    options:
      default_max_tokens: 16384
      cache_enabled: true
  - name: chat-source
    backend: openai-chat
    base_url: https://api.openai.com
    api_key: sk-key
  - name: responses-source
    backend: openai-responses
    base_url: https://api.openai.com
    api_key: sk-key
  - name: copilot
    backend: github-copilot
    options:
      github_token: gho_token
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	v := newRegistryStub()
	cfg, err := LoadWithValidator(path, v)
	if err != nil {
		t.Fatalf("LoadWithValidator: %v", err)
	}
	if len(cfg.Sources) != 4 {
		t.Fatalf("Sources = %d, want 4", len(cfg.Sources))
	}
	want := []string{"anthropic", "openai-chat", "openai-responses", "github-copilot"}
	for i, w := range want {
		if got := cfg.Sources[i].Backend; got != w {
			t.Errorf("Sources[%d].Backend = %q, want %q", i, got, w)
		}
	}
}

// TestLoadRejectsUnregisteredBackend 验证注入 registry 时未注册 backend 被拒绝。
func TestLoadRejectsUnregisteredBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
sources:
  - name: bad
    backend: fake-provider
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithValidator(path, newRegistryStub()); err == nil {
		t.Fatal("expected error for unregistered backend, got nil")
	}
}

// TestLoadRejectsSchemaForeignOption 验证注入 registry 时 schema 外 option 被拒绝。
func TestLoadRejectsSchemaForeignOption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
sources:
  - name: anthropic-official
    backend: anthropic
    options:
      bogus_field: 123
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithValidator(path, newRegistryStub()); err == nil {
		t.Fatal("expected error for schema-foreign option, got nil")
	}
}

// TestLoadWithValidatorParsesV2Source 验证 Config v2 的 backend + options 解析，
// 并把校验委托给注入的 validator。
func TestLoadWithValidatorParsesV2Source(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
sources:
  - name: anthropic-official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: ${KEY}
    default_model: claude-sonnet-4-20250514
    options:
      default_max_tokens: 32768
      cache_enabled: false
  - name: copilot
    backend: github-copilot
    default_model: gpt-5.3-codex
    options:
      github_token: ${GH_TOKEN}
`
	t.Setenv("KEY", "sk-ant-secret")
	t.Setenv("GH_TOKEN", "gho_secret")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	v := &stubValidator{ok: true}
	cfg, err := LoadWithValidator(path, v)
	if err != nil {
		t.Fatalf("LoadWithValidator: %v", err)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("Sources = %d, want 2", len(cfg.Sources))
	}
	if cfg.Sources[0].Backend != "anthropic" {
		t.Errorf("Sources[0].Backend = %q, want anthropic", cfg.Sources[0].Backend)
	}
	if cfg.Sources[0].APIKey != "sk-ant-secret" {
		t.Errorf("inline ${VAR} not expanded: %q", cfg.Sources[0].APIKey)
	}
	if cfg.Sources[1].Options["github_token"] != "gho_secret" {
		t.Errorf("copilot github_token option not expanded: %v", cfg.Sources[1].Options)
	}
	if len(v.seen) != 2 {
		t.Fatalf("validator called %d times, want 2", len(v.seen))
	}
}

// TestLoadValidatorErrorAborts 验证插件级校验错误会中止加载（不替换运行时状态）。
func TestLoadValidatorErrorAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
sources:
  - name: bad
    backend: unknown-backend
`), 0o644); err != nil {
		t.Fatal(err)
	}

	v := &stubValidator{ok: false}
	if _, err := LoadWithValidator(path, v); err == nil {
		t.Fatal("expected error from validator, got nil")
	}
}

// TestDuplicateSourceNameRejected 验证 source 名称唯一性约束。
func TestDuplicateSourceNameRejected(t *testing.T) {
	cfg := Config{Sources: []Source{
		{Name: "dup", Backend: "anthropic"},
		{Name: "dup", Backend: "anthropic"},
	}}
	if err := cfg.validate(nil); err == nil {
		t.Fatal("expected duplicate source name error")
	}
}

// TestOptionsValueTypes 验证 koanf 解析后 options 值的 Go 类型，供插件归一化使用。
func TestOptionsValueTypes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
sources:
  - name: a
    backend: anthropic
    options:
      default_max_tokens: 32768
      cache_enabled: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	opts := cfg.Sources[0].Options
	if _, ok := opts["default_max_tokens"].(int); !ok {
		t.Errorf("default_max_tokens type = %T, want int", opts["default_max_tokens"])
	}
	if _, ok := opts["cache_enabled"].(bool); !ok {
		t.Errorf("cache_enabled type = %T, want bool", opts["cache_enabled"])
	}
}

// TestLoadRejectsLegacyBackendType 验证 Config v2 磁盘加载严格拒绝 source 级 backend_type。
// Source 结构体已不含该字段，磁盘配置必须使用稳定 backend。
func TestLoadRejectsLegacyBackendType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
sources:
  - name: copilot
    backend_type: g
    github_token: x
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for legacy backend_type, got nil")
	}
}

// TestLoadRejectsTopLevelGithubToken 验证 Config v2 磁盘加载严格拒绝顶层 github_token。
func TestLoadRejectsTopLevelGithubToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
github_token: gho_x
sources:
  - name: copilot
    backend: github-copilot
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for top-level github_token, got nil")
	}
}

// TestLoadRejectsSourceGithubToken 验证 Config v2 磁盘加载严格拒绝 source 级 github_token。
func TestLoadRejectsSourceGithubToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
sources:
  - name: copilot
    backend: github-copilot
    github_token: gho_x
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for source github_token, got nil")
	}
}

// TestLoadRejectsMissingBackend 验证 Config v2 磁盘加载拒绝缺少 backend 的 source。
func TestLoadRejectsMissingBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
sources:
  - name: copilot
    base_url: https://example.com
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing backend, got nil")
	}
}

// TestGithubTokenYAMLRoundTrip 验证 github-copilot 源的序列化形状：
// Backend 使用稳定 ID，github_token 只落在 options.github_token，
// 绝不写出过时的 backend_type 或顶层 github_token。
func TestGithubTokenYAMLRoundTrip(t *testing.T) {
	cfg := Config{
		Sources: []Source{{
			Name:    "copilot",
			Backend: "github-copilot",
			Options: map[string]any{"github_token": "gho_test_token_123"},
		}},
	}
	if err := cfg.validate(nil); err != nil {
		t.Fatalf("validate failed: %v", err)
	}

	data, err := yaml.Marshal(cfg.Sources[0])
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal doc: %v", err)
	}
	if _, ok := doc["github_token"]; ok {
		t.Fatalf("top-level github_token must not be written:\n%s", data)
	}
	if _, ok := doc["backend_type"]; ok {
		t.Fatalf("backend_type must not be written:\n%s", data)
	}
	opts, ok := doc["options"].(map[string]any)
	if !ok {
		t.Fatalf("options missing from written YAML:\n%s", data)
	}
	if got := opts["github_token"]; got != "gho_test_token_123" {
		t.Fatalf("options.github_token = %v (%T), want gho_test_token_123\n%s", got, got, data)
	}

	var src Source
	if err := yaml.Unmarshal(data, &src); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if src.Backend != "github-copilot" {
		t.Errorf("Backend = %q, want github-copilot", src.Backend)
	}
	if got := src.Options["github_token"]; got != "gho_test_token_123" {
		t.Errorf("Options[github_token] = %v (%T), want gho_test_token_123", got, got)
	}
}
