package toolcatalog

import (
	"strings"
	"testing"
)

func TestSanitizeClientToolInputFreeformPassthrough(t *testing.T) {
	raw := `{"s":"echo hi"}`
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

func TestSanitizeApplyPatchStructuredFallback(t *testing.T) {
	// 模型若按历史回灌形态输出 structured operation/path/diff，
	// 回程必须兜底折成 V4A 文本，否则 Codex apply_patch 无法执行。
	got := SanitizeClientToolInput("apply_patch", true, `{"operation":"update_file","path":"a.go","diff":"@@\n-old\n+new\n"}`)
	want := "*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"
	if got != want {
		t.Fatalf("structured apply_patch must convert to V4A, got %q", got)
	}
}

func TestSanitizeApplyPatchFreeformPassthrough(t *testing.T) {
	raw := `{"s":"*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"}`
	got := SanitizeClientToolInput("apply_patch", true, raw)
	want := "*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"
	if got != want {
		t.Fatalf("freeform apply_patch input must pass through, got %q", got)
	}
}

func TestSanitizeApplyPatchPatchField(t *testing.T) {
	// opencode 等 Chat 上游把 V4A 文本包进 {"patch": ...} 输出，
	// 回程必须解包为裸文本，否则 Codex 把整段 JSON 当 patch，首行校验失败。
	raw := `{"patch":"*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"}`
	got := SanitizeClientToolInput("apply_patch", true, raw)
	want := "*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"
	if got != want {
		t.Fatalf("apply_patch patch field must unwrap to V4A text, got %q", got)
	}
}

func TestSanitizeSingleValueAnyKey(t *testing.T) {
	// freeform 解包不限定键名：单键对象直接取唯一字符串值，
	// 兼容 opencode 等上游用 patch/cmd/content 等任意键包装文本。
	cases := []struct {
		tool, raw, want string
	}{
		{"shell", `{"cmd":"ls -la"}`, "ls -la"},
		{"apply_patch", `{"content":"*** Begin Patch\n*** End Patch"}`, "*** Begin Patch\n*** End Patch"},
		{"custom_tool", `{"text":"hello"}`, "hello"},
	}
	for _, tc := range cases {
		if got := SanitizeClientToolInput(tc.tool, true, tc.raw); got != tc.want {
			t.Fatalf("%s single-key unwrap: want %q, got %q", tc.tool, tc.want, got)
		}
	}
}

func TestSanitizeMultiKeyObjectPreserved(t *testing.T) {
	// 多键对象不是 freeform 文本包装，必须原样返回（structured 兜底自行处理）。
	raw := `{"a":"1","b":"2"}`
	if got := SanitizeClientToolInput("apply_patch", true, raw); got != raw {
		t.Fatalf("multi-key object must be preserved, got %q", got)
	}
}
