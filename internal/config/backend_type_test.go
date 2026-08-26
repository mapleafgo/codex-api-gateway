package config

import (
	"errors"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNormalizeBackendType(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "a", true},
		{"a", "a", true},
		{"c", "c", true},
		{"r", "r", true},
		{"g", "g", true},
		{" a ", "a", true},
		{" c ", "c", true},
		{" r ", "r", true},
		{" g ", "g", true},
		{"anthropic", "", false},
		{"openai_chat", "", false},
		{"x", "", false},
	}
	for _, tc := range cases {
		got, err := NormalizeBackendType(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Fatalf("in=%q got=%q err=%v want=%q", tc.in, got, err, tc.want)
			}
		} else {
			if err == nil {
				t.Fatalf("in=%q expected error", tc.in)
			}
		}
	}
}

func TestBackendIDToType(t *testing.T) {
	cases := []struct {
		id, want string
		ok       bool
	}{
		{"anthropic", "a", true},
		{"openai-chat", "c", true},
		{"openai-responses", "r", true},
		{"github-copilot", "g", true},
		{"unknown", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := BackendIDToType(tc.id)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("id=%q got=%q ok=%v want=%q ok=%v", tc.id, got, ok, tc.want, ok)
		}
	}
}

func TestValidateRejectsUnknownBackendType(t *testing.T) {
	cfg := Config{
		Sources: []Source{{
			Name:        "test",
			BaseURL:     "https://example.com",
			BackendType: "invalid",
		}},
	}
	if err := cfg.validate(nil); err == nil {
		t.Fatalf("expected error for invalid backend_type, got nil")
	}
}

func TestValidateAcceptsBoth(t *testing.T) {
	cases := []string{"a", "c", "r", "g", ""}
	for _, bt := range cases {
		cfg := Config{
			Sources: []Source{{
				Name:        "test",
				BackendType: bt,
			}},
		}
		if bt != "g" {
			cfg.Sources[0].BaseURL = "https://example.com"
		} else {
			cfg.Sources[0].GithubToken = "test-token"
		}
		if err := cfg.validate(nil); err != nil {
			t.Fatalf("backend_type=%q: validate failed: %v", bt, err)
		}
	}
}

func TestGithubTokenYAMLRoundTrip(t *testing.T) {
	cfg := Config{
		Sources: []Source{{
			Name:        "copilot",
			BackendType: "g",
			GithubToken: "gho_test_token_123",
		}},
	}
	if err := cfg.validate(nil); err != nil {
		t.Fatalf("validate failed: %v", err)
	}

	// YAML 序列化/反序列化往返
	data, err := yaml.Marshal(cfg.Sources[0])
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var src Source
	if err := yaml.Unmarshal(data, &src); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if src.GithubToken != "gho_test_token_123" {
		t.Errorf("GithubToken = %q, want gho_test_token_123", src.GithubToken)
	}
	if src.BackendType != "g" {
		t.Errorf("BackendType = %q, want g", src.BackendType)
	}
}

func TestValidateRequiresGithubTokenForG(t *testing.T) {
	cfg := Config{
		Sources: []Source{{
			Name:        "copilot",
			BackendType: "g",
			// GithubToken intentionally omitted
		}},
	}
	// legacy g 短码应先映射到稳定 Backend ID。
	if err := cfg.validate(nil); err != nil {
		t.Fatalf("legacy g source should be accepted at platform level: %v", err)
	}
	if got := cfg.Sources[0].Backend; got != "github-copilot" {
		t.Fatalf("Backend = %q, want github-copilot after legacy mapping", got)
	}
	// 缺 github_token 属于插件级必填，由注入的 SourceValidator 拒绝。
	v := fakeValidator{err: errors.New("copilot: missing github_token")}
	if err := cfg.validate(v); err == nil {
		t.Fatal("expected error for missing github_token via validator")
	}
}

type fakeValidator struct{ err error }

func (f fakeValidator) ValidateSource(Source) error { return f.err }
