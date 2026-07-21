package toolcatalog

import (
	"strings"
	"testing"
)

func TestSanitizeClientToolInputApplyPatchStripsExtraStars(t *testing.T) {
	raw := `{"input":"*** Begin Patch ***\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch ***"}`
	got := SanitizeClientToolInput("apply_patch", true, raw)
	if !strings.HasPrefix(got, "*** Begin Patch\n") {
		t.Fatalf("begin marker: %q", got[:min(40, len(got))])
	}
	if !strings.HasSuffix(got, "\n*** End Patch") && !strings.HasSuffix(got, "*** End Patch") {
		t.Fatalf("end marker: %q", got[max(0, len(got)-40):])
	}
	if strings.Contains(got, "Begin Patch ***") || strings.Contains(got, "End Patch ***") {
		t.Fatalf("extra stars not stripped: %q", got)
	}
}

func TestSanitizeClientToolInputApplyPatchFromStructured(t *testing.T) {
	raw := `{"operation":"update_file","path":"a.go","diff":"@@\n-old\n+new\n"}`
	got := SanitizeClientToolInput("apply_patch", true, raw)
	wantPrefix := "*** Begin Patch\n*** Update File: a.go\n"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(got, "+new") || !strings.HasSuffix(strings.TrimRight(got, "\n"), "*** End Patch") {
		t.Fatalf("body/end: %q", got)
	}
}

func TestSanitizeClientToolInputFreeformPassthrough(t *testing.T) {
	raw := `{"input":"echo hi"}`
	got := SanitizeClientToolInput("shell", true, raw)
	if got != "echo hi" {
		t.Fatalf("shell freeform unwrap: %q", got)
	}
}

func TestSanitizeJSONIntegerNumbers(t *testing.T) {
	in := `{"session_id":85100.0,"yield_time_ms":300000.0,"cmd":"ls","nested":{"n":1.0,"f":1.5}}`
	got := SanitizeJSONIntegerNumbers(in)
	if strings.Contains(got, "85100.0") || strings.Contains(got, "300000.0") || strings.Contains(got, `"n":1.0`) {
		t.Fatalf("integers not coerced: %s", got)
	}
	if !strings.Contains(got, `"session_id":85100`) || !strings.Contains(got, `"yield_time_ms":300000`) {
		t.Fatalf("missing int literals: %s", got)
	}
	if !strings.Contains(got, `"f":1.5`) {
		t.Fatalf("non-integer float must stay: %s", got)
	}
}

func TestSanitizeJSONIntegerNumbersInvalidPassthrough(t *testing.T) {
	raw := `not-json`
	if SanitizeJSONIntegerNumbers(raw) != raw {
		t.Fatal("invalid json must passthrough")
	}
}

func TestSanitizeClientToolInputFunctionPath(t *testing.T) {
	got := SanitizeClientToolInput("exec_command", false, `{"yield_time_ms":120000.0}`)
	if got != `{"yield_time_ms":120000}` {
		t.Fatalf("got %s", got)
	}
}

func TestFormatApplyPatchV4ADelete(t *testing.T) {
	got := FormatApplyPatchV4A("delete_file", "x.txt", "")
	if got != "*** Begin Patch\n*** Delete File: x.txt\n*** End Patch" {
		t.Fatalf("%q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
