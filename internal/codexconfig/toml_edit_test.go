package codexconfig

import (
	"strings"
	"testing"
)

func splitLines(t *testing.T, s string) []string {
	t.Helper()
	return strings.Split(s, "\n")
}

func joinLines(t *testing.T, lines []string) string {
	t.Helper()
	return strings.Join(lines, "\n")
}

func TestTopLevelKeyReadsValueBeforeFirstTable(t *testing.T) {
	lines := splitLines(t, "model_provider = \"openai\"\n[projects.x]\n")
	v, ok := topLevelKey(lines, "model_provider")
	if !ok || v != "openai" {
		t.Fatalf("topLevelKey=(%q,%v) want (openai,true)", v, ok)
	}
}

func TestTopLevelKeyIgnoresKeysInsideTable(t *testing.T) {
	lines := splitLines(t, "[model_providers.custom]\nmodel_provider = \"nested\"\n")
	if v, ok := topLevelKey(lines, "model_provider"); ok {
		t.Fatalf("表内键不应命中，实际 (%q,%v)", v, ok)
	}
}

func TestUpsertTopLevelKeyReplacesExisting(t *testing.T) {
	lines := splitLines(t, "model_provider = \"openai\"\n# c\n[projects.x]\n")
	got := joinLines(t, upsertTopLevelKey(lines, "model_provider", "codex-api-gateway"))
	want := "model_provider = \"codex-api-gateway\"\n# c\n[projects.x]\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpsertTopLevelKeyInsertsBeforeFirstTable(t *testing.T) {
	lines := splitLines(t, "# c\n[projects.x]\n")
	got := joinLines(t, upsertTopLevelKey(lines, "model_provider", "codex-api-gateway"))
	want := "# c\nmodel_provider = \"codex-api-gateway\"\n[projects.x]\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRemoveTopLevelKey(t *testing.T) {
	lines := splitLines(t, "a = \"1\"\nmodel_provider = \"codex-api-gateway\"\n[projects.x]\n")
	got := joinLines(t, removeTopLevelKey(lines, "model_provider"))
	want := "a = \"1\"\n[projects.x]\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpsertTableBlockReplacesIncludingSubTable(t *testing.T) {
	lines := splitLines(t,
		"a = \"1\"\n"+
			"[model_providers.codex-api-gateway]\n"+
			"old = \"x\"\n"+
			"[model_providers.codex-api-gateway.auth]\n"+
			"command = \"old\"\n"+
			"[projects.x]\n")
	block := []string{
		"[model_providers.codex-api-gateway]",
		`name = "Codex API Gateway"`,
		`base_url = "http://127.0.0.1:8383/v1"`,
		`wire_api = "responses"`,
	}
	got := joinLines(t, upsertTableBlock(lines, "[model_providers.codex-api-gateway]", block))
	if strings.Contains(got, "old =") {
		t.Fatalf("旧块未整体替换:\n%s", got)
	}
	if strings.Contains(got, "codex-api-gateway.auth") || strings.Contains(got, "echo codex-local") {
		t.Fatalf("旧嵌套子表未随块替换清除:\n%s", got)
	}
	if !strings.Contains(got, `base_url = "http://127.0.0.1:8383/v1"`) || !strings.Contains(got, "[projects.x]") {
		t.Fatalf("新块或后续表丢失:\n%s", got)
	}
}

func TestUpsertTableBlockAppendsWhenAbsent(t *testing.T) {
	lines := splitLines(t, "[projects.x]\ntrust_level = \"trusted\"\n")
	got := joinLines(t, upsertTableBlock(lines, "[model_providers.codex-api-gateway]",
		[]string{"[model_providers.codex-api-gateway]", `base_url = "http://x/v1"`}))
	if !strings.Contains(got, "[model_providers.codex-api-gateway]") ||
		!strings.Contains(got, `base_url = "http://x/v1"`) {
		t.Fatalf("新块未追加:\n%s", got)
	}
}

func TestRemoveTableBlock(t *testing.T) {
	lines := splitLines(t,
		"a = \"1\"\n"+
			"[model_providers.codex-api-gateway]\n"+
			"x = \"1\"\n"+
			"[model_providers.codex-api-gateway.nested]\n"+
			"y = \"2\"\n"+
			"[projects.x]\n")
	got := joinLines(t, removeTableBlock(lines, "[model_providers.codex-api-gateway]"))
	if strings.Contains(got, "codex-api-gateway") || strings.Contains(got, `x = "1"`) ||
		strings.Contains(got, `y = "2"`) {
		t.Fatalf("块未完整删除:\n%s", got)
	}
	if !strings.Contains(got, "[projects.x]") {
		t.Fatalf("后续表被误删:\n%s", got)
	}
}

func TestTableValue(t *testing.T) {
	lines := splitLines(t,
		"[model_providers.codex-api-gateway]\n"+
			"base_url = \"http://x/v1\"\n"+
			"[projects.x]\n")
	v, ok := tableValue(lines, "[model_providers.codex-api-gateway]", "base_url")
	if !ok || v != "http://x/v1" {
		t.Fatalf("tableValue=(%q,%v) want (http://x/v1,true)", v, ok)
	}
}

func TestTopLevelKeyNoSpaceAndSingleQuote(t *testing.T) {
	lines := splitLines(t, "model_provider=\"openai\"\n[projects.x]\n")
	v, ok := topLevelKey(lines, "model_provider")
	if !ok || v != "openai" {
		t.Fatalf("topLevelKey=(%q,%v) want (openai,true)", v, ok)
	}
}

func TestUpsertTopLevelKeyReplacesNoSpaceExisting(t *testing.T) {
	lines := splitLines(t, "model_provider=\"openai\"\n[projects.x]\n")
	got := joinLines(t, upsertTopLevelKey(lines, "model_provider", "codex-api-gateway"))
	if strings.Count(got, "model_provider") != 1 {
		t.Fatalf("不应产生重复键:\n%s", got)
	}
	if !strings.Contains(got, `model_provider = "codex-api-gateway"`) {
		t.Fatalf("未替换无空格键:\n%s", got)
	}
}

func TestUpsertTopLevelKeyEscapesWindowsPath(t *testing.T) {
	pathValue := `C:\Users\alice\.codex\models.json`
	lines := splitLines(t, "# c\n[projects.x]\n")
	got := joinLines(t, upsertTopLevelKey(lines, "model_catalog_json", pathValue))
	if !strings.Contains(got, `model_catalog_json = "C:\\Users\\alice\\.codex\\models.json"`) {
		t.Fatalf("Windows 路径未转义:\n%s", got)
	}
	v, ok := topLevelKey(splitLines(t, got), "model_catalog_json")
	if !ok || v != pathValue {
		t.Fatalf("topLevelKey=(%q,%v) want (%q,true)", v, ok, pathValue)
	}
}

func TestTableValueSingleQuote(t *testing.T) {
	lines := splitLines(t,
		"[model_providers.codex-api-gateway]\n"+
			"base_url='http://x/v1'\n"+
			"[projects.x]\n")
	v, ok := tableValue(lines, "[model_providers.codex-api-gateway]", "base_url")
	if !ok || v != "http://x/v1" {
		t.Fatalf("tableValue=(%q,%v) want (http://x/v1,true)", v, ok)
	}
}
