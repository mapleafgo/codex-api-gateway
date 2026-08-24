package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

func TestFailedResponseErrorContextCode(t *testing.T) {
	ctxErr := errors.New(`upstream 400: {"message":"The input (293789 tokens) is longer than the model's context length (262144 tokens)."}`)
	got := failedResponseError(ctxErr)
	if got.Code != model.ErrorCodeContextLengthExceeded {
		t.Fatalf("want code=context_length_exceeded, got %q", got.Code)
	}
	if !strings.Contains(got.Message, "context length") {
		t.Fatalf("message must keep diagnostics, got %q", got.Message)
	}

	other := errors.New("upstream 429: rate limit reached")
	if got := failedResponseError(other); got.Code != "" {
		t.Fatalf("non-context error must not carry context code, got %q", got.Code)
	}
}
