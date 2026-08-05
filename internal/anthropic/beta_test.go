package anthropic

import (
	"strings"
	"testing"
)

func TestAppendBeta(t *testing.T) {
	if got := appendBeta("", "interleaved-thinking-2025-05-14"); got != "interleaved-thinking-2025-05-14" {
		t.Fatalf("empty base: %q", got)
	}
	if got := appendBeta("interleaved-thinking-2025-05-14", "some-custom-beta"); !strings.Contains(got, "some-custom-beta") || !strings.Contains(got, "interleaved-thinking") {
		t.Fatalf("must merge: %q", got)
	}
	if got := appendBeta("interleaved-thinking-2025-05-14", "interleaved-thinking-2025-05-14"); got != "interleaved-thinking-2025-05-14" {
		t.Fatalf("must dedupe: %q", got)
	}
}
