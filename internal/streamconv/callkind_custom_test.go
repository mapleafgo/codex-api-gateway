package streamconv

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/toolcatalog"
)

// TestCustomToolInputPreservesV4APatch 锁定 freeform 解包透传 V4A patch 时不
// 修改正文——尤其空内容行 "+"（单加号）必须原样保留。
func TestCustomToolInputPreservesV4APatch(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: /tmp/x\n+# title\n+\n+body line\n*** End Patch"
	rawBytes, err := json.Marshal(map[string]string{"input": patch})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := toolcatalog.SanitizeClientToolInput("apply_patch", true, string(rawBytes))
	if got != patch {
		t.Fatalf("sanitize 修改了合法 patch 正文:\nwant=%q\ngot =%q", patch, got)
	}
}

// TestCustomToolInputPreservesBlankLines 验证含连续空行的 patch 也能原样透传。
func TestCustomToolInputPreservesBlankLines(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: /tmp/x\n+# title\n\n+\n\n+## section\n*** End Patch"
	rawBytes, _ := json.Marshal(map[string]string{"input": patch})
	got := toolcatalog.SanitizeClientToolInput("apply_patch", true, string(rawBytes))
	if got != patch {
		t.Fatalf("blank-line patch not preserved:\nwant=%q\ngot =%q", patch, got)
	}
}

func TestApplyPatchSanitizeStripsExtraStars(t *testing.T) {
	rawBytes, _ := json.Marshal(map[string]string{
		"input": "*** Begin Patch ***\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch ***",
	})
	got := toolcatalog.SanitizeClientToolInput("apply_patch", true, string(rawBytes))
	if !strings.HasPrefix(got, "*** Begin Patch\n") {
		t.Fatalf("begin: %q", got)
	}
	if strings.Contains(got, "Patch ***") {
		t.Fatalf("extra stars remain: %q", got)
	}
	if !strings.HasSuffix(got, "*** End Patch") {
		t.Fatalf("end: %q", got)
	}
}

func TestFunctionArgsSanitizeIntegerFloats(t *testing.T) {
	raw := `{"session_id":85100.0,"yield_time_ms":300000.0}`
	got := toolcatalog.SanitizeClientToolInput("write_stdin", false, raw)
	if strings.Contains(got, ".0") {
		t.Fatalf("float ints remain: %s", got)
	}
	if !strings.Contains(got, "85100") || !strings.Contains(got, "300000") {
		t.Fatalf("ints lost: %s", got)
	}
}
