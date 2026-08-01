package anthropic

import (
	"strings"
	"testing"
)

func TestAppendBeta(t *testing.T) {
	if got := appendBeta("", ExtendedCacheTTLBetaHeader); got != ExtendedCacheTTLBetaHeader {
		t.Fatalf("empty base: %q", got)
	}
	if got := appendBeta("interleaved-thinking-2025-05-14", ExtendedCacheTTLBetaHeader); !strings.Contains(got, ExtendedCacheTTLBetaHeader) || !strings.Contains(got, "interleaved-thinking") {
		t.Fatalf("must merge: %q", got)
	}
	if got := appendBeta(ExtendedCacheTTLBetaHeader, ExtendedCacheTTLBetaHeader); got != ExtendedCacheTTLBetaHeader {
		t.Fatalf("must dedupe: %q", got)
	}
}
