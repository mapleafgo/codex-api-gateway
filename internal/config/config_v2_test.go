package config

import (
	"os"
	"path/filepath"
	"testing"
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

// TestLegacyBackendTypeMapsToID 验证过渡期旧短码被映射到稳定 Backend ID。
func TestLegacyBackendTypeMapsToID(t *testing.T) {
	cases := []struct {
		bt   string
		want string
	}{
		{"a", "anthropic"},
		{"c", "openai-chat"},
		{"r", "openai-responses"},
		{"g", "github-copilot"},
	}
	for _, tc := range cases {
		cfg := Config{Sources: []Source{{Name: "s", BackendType: tc.bt, BaseURL: "https://x"}}}
		if err := cfg.validate(nil); err != nil {
			t.Fatalf("backend_type=%q: %v", tc.bt, err)
		}
		if got := cfg.Sources[0].Backend; got != tc.want {
			t.Errorf("backend_type=%q mapped Backend=%q want %q", tc.bt, got, tc.want)
		}
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

// TestUnknownBackendTypeRejected 验证不认识的旧短码被拒绝，提示改用 backend。
func TestUnknownBackendTypeRejected(t *testing.T) {
	cfg := Config{Sources: []Source{{Name: "s", BackendType: "z", BaseURL: "https://x"}}}
	if err := cfg.validate(nil); err == nil {
		t.Fatal("expected error for unknown backend_type")
	}
}
