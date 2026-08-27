package openairesponses

import (
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

func TestValidateSourceAcceptsBasicConfig(t *testing.T) {
	p := New()
	if err := p.ValidateSource(config.Source{Name: "resp", Backend: "openai-responses", BaseURL: "https://x", APIKey: "k"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSourceAcceptsEmptyOptions(t *testing.T) {
	p := New()
	if err := p.ValidateSource(config.Source{Name: "resp", Backend: "openai-responses"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
