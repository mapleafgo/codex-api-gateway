package codexconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const seedConfig = `# 用户注释
model = "glm-latest"
model_provider = "openai"

[model_providers.custom]
name = "custom"
base_url = "http://127.0.0.1:9870/v1"

[projects."/tmp/x"]
trust_level = "trusted"
`

func configPath(home string) string {
	return filepath.Join(home, ".codex", "config.toml")
}

func backupPath(home string) string {
	return filepath.Join(home, ".codex", backupFileName)
}

func seedConfigFile(t *testing.T, home, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := configPath(home)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(data)
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s 应不存在，实际 err=%v", path, err)
	}
}

func countOccurrences(s, sub string) int {
	return strings.Count(s, sub)
}

func TestEnableMissingConfigErrorsWithoutCreating(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	m := New(func() string { return "http://127.0.0.1:8383/v1" })

	err := m.Enable()
	if err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("缺失 config.toml 时应报错，实际 %v", err)
	}
	assertNotExist(t, configPath(home))
	assertNotExist(t, backupPath(home))
}

func TestEnablePreservesExistingConfigAndBacksUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := seedConfigFile(t, home, seedConfig)
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	for _, want := range []string{
		"# 用户注释",
		`model = "glm-latest"`,
		`model_provider = "codex-api-gateway"`,
		"[model_providers.custom]",
		"[model_providers.codex-api-gateway]",
		`base_url = "http://127.0.0.1:8383/v1"`,
		"[model_providers.codex-api-gateway.auth]",
		`command = "echo codex-local"`,
		`[projects."/tmp/x"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("启用后缺少 %q，实际内容:\n%s", want, got)
		}
	}
	data, err := os.ReadFile(backupPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var state backupState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.ModelProvider == nil || *state.ModelProvider != "openai" {
		t.Fatalf("备份应保存 openai，实际 %+v", state)
	}
}

func TestEnableIdempotentKeepsOriginalBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := seedConfigFile(t, home, seedConfig)
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, path); after != before {
		t.Fatalf("重复启用应保持内容不变:\n%s", after)
	}
	if n := countOccurrences(before, "[model_providers.codex-api-gateway]"); n != 1 {
		t.Fatalf("provider 块应只出现 1 次，实际 %d", n)
	}
	data, err := os.ReadFile(backupPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var state backupState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.ModelProvider == nil || *state.ModelProvider != "openai" {
		t.Fatalf("备份应保持 openai，实际 %+v", state)
	}
}

func TestEnableRefreshesBaseURLAfterPortChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := seedConfigFile(t, home, seedConfig)
	base := "http://127.0.0.1:8383/v1"
	m := New(func() string { return base })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	base = "http://127.0.0.1:9870/v1"
	if on, err := m.IsEnabled(); err != nil || on {
		t.Fatalf("端口变更后应为未启用，on=%v err=%v", on, err)
	}
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, `base_url = "http://127.0.0.1:9870/v1"`) {
		t.Fatalf("端口变更后未覆盖 base_url:\n%s", got)
	}
}

func TestDisableRestoresModelProviderAndKeepsBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := seedConfigFile(t, home, seedConfig)
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, `model_provider = "openai"`) {
		t.Fatalf("禁用后应恢复 model_provider=openai:\n%s", got)
	}
	if !strings.Contains(got, "[model_providers.codex-api-gateway]") {
		t.Fatalf("禁用后 provider 块应保留:\n%s", got)
	}
	assertNotExist(t, backupPath(home))
}

func TestDisableFailsWhenBackupMissingWhileEnabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := seedConfigFile(t, home, seedConfig)
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backupPath(home)); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	if err := m.Disable(); err == nil {
		t.Fatal("备份缺失且启用态时应报错")
	}
	if after := readFile(t, path); after != before {
		t.Fatalf("报错时不得改动文件:\n%s", after)
	}
}

func TestDisableNoopWhenNotEnabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := seedConfigFile(t, home, seedConfig)
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != seedConfig {
		t.Fatalf("未启用时 Disable 不应改动文件:\n%s", got)
	}
}

func TestDisableRemovesModelProviderWhenOriginallyAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	noProvider := "# 无 model_provider\nmodel = \"glm-latest\"\n"
	path := seedConfigFile(t, home, noProvider)
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if strings.Contains(got, "model_provider =") {
		t.Fatalf("原无 model_provider 时应删除该行:\n%s", got)
	}
	if !strings.Contains(got, "[model_providers.codex-api-gateway]") {
		t.Fatalf("provider 块应保留:\n%s", got)
	}
}

func TestIsEnabledRequiresMatchingBaseURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	seedConfigFile(t, home, seedConfig)
	base := "http://127.0.0.1:8383/v1"
	m := New(func() string { return base })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	if on, err := m.IsEnabled(); err != nil || !on {
		t.Fatalf("启用后 IsEnabled 应为 true，on=%v err=%v", on, err)
	}
	base = "http://127.0.0.1:9870/v1"
	if on, err := m.IsEnabled(); err != nil || on {
		t.Fatalf("端口变更后 IsEnabled 应为 false，on=%v err=%v", on, err)
	}
}

func TestIsEnabledFalseWhenConfigMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	on, err := m.IsEnabled()
	if err != nil || on {
		t.Fatalf("缺失配置时 IsEnabled=(%v,%v) 应为 (false,nil)", on, err)
	}
}
