package plugin

import (
	"errors"
	"testing"
)

func TestMapDelegateHost(t *testing.T) {
	delegate := &recordingBackend{}
	host := MapDelegateHost{"anthropic": delegate}
	got, err := host.BackendByID("anthropic")
	if err != nil || got != delegate {
		t.Fatalf("BackendByID() = %v, %v", got, err)
	}
	if _, err := host.BackendByID("missing"); !errors.Is(err, ErrDelegateNotFound) {
		t.Fatalf("missing error = %v", err)
	}
}

func TestWrapDelegatedEventRewritesIdentityOnly(t *testing.T) {
	in := UpstreamEvent{
		SourceName:    "copilot",
		Model:         "gpt-5",
		ResolvedModel: "claude",
		Status:        "completed",
		Code:          200,
		InputTokens:   12,
		OutputTokens:  4,
		CacheRead:     3,
		CacheCreate:   2,
		Error:         "",
		Attempt:       7,
		Backend:       "openai-responses",
	}
	out := WrapDelegatedEvent("github-copilot", in)
	if out.Backend != "github-copilot" {
		t.Fatalf("Backend = %q", out.Backend)
	}
	if out.ResolvedModel != in.ResolvedModel || out.Status != in.Status || out.InputTokens != in.InputTokens ||
		out.CacheRead != in.CacheRead || out.Attempt != in.Attempt {
		t.Fatalf("event changed: %+v -> %+v", in, out)
	}
}
