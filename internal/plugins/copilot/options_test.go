package copilot

import (
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"testing"
)

func TestValidateSourceRejectsMissingToken(t *testing.T) {
	p := New()
	err := p.ValidateSource(config.Source{Name: "copilot", Backend: "github-copilot"})
	if err == nil {
		t.Fatal("expected error for missing github_token, got nil")
	}
}

func TestValidateSourceRejectsEmptyToken(t *testing.T) {
	p := New()
	err := p.ValidateSource(config.Source{
		Name: "copilot", Backend: "github-copilot",
		Options: map[string]any{"github_token": ""},
	})
	if err == nil {
		t.Fatal("expected error for empty github_token, got nil")
	}
}

func TestValidateSourceAcceptsTokenInOptions(t *testing.T) {
	p := New()
	err := p.ValidateSource(config.Source{
		Name: "copilot", Backend: "github-copilot",
		Options: map[string]any{"github_token": "gho_secret"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSourceMissingTokenReturnsPluginError(t *testing.T) {
	p := New()
	err := p.ValidateSource(config.Source{Name: "copilot", Backend: "github-copilot"})
	if err != errMissingGithubToken {
		t.Fatalf("error = %v, want ErrMissingGithubToken", err)
	}
}
