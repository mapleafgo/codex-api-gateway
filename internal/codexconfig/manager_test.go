package codexconfig

import (
	"database/sql"
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

func codexModelsPath(home string) string {
	return filepath.Join(home, ".codex", "models.json")
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

func TestEnableWithNoSpaceModelProviderProducesSingleKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := seedConfigFile(t, home, "model_provider=\"openai\"\n")
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if strings.Count(got, "model_provider =") != 1 {
		t.Fatalf("无空格写法下启用不应产生重复键:\n%s", got)
	}
	if !strings.Contains(got, `model_provider = "codex-api-gateway"`) {
		t.Fatalf("未正确替换 model_provider:\n%s", got)
	}
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
		`[projects."/tmp/x"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("启用后缺少 %q，实际内容:\n%s", want, got)
		}
	}
	if strings.Contains(got, "codex-api-gateway.auth") || strings.Contains(got, "echo codex-local") {
		t.Fatalf("启用后不应残留 auth 子表:\n%s", got)
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

func TestDisableRecoversWhenBackupMissingWhileEnabled(t *testing.T) {
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
	if err := m.Disable(); err != nil {
		t.Fatalf("备份缺失时也应能关闭，实际 %v", err)
	}
	got := readFile(t, path)
	if strings.Contains(got, "model_provider =") {
		t.Fatalf("备份缺失关闭后应移除 model_provider 键:\n%s", got)
	}
	if strings.Contains(got, "model_catalog_json =") {
		t.Fatalf("备份缺失关闭后应移除 model_catalog_json 键:\n%s", got)
	}
	if !strings.Contains(got, "[model_providers.codex-api-gateway]") {
		t.Fatalf("关闭后 provider 块应保留:\n%s", got)
	}
	assertNotExist(t, backupPath(home))
}

func TestEnableStripsLegacyAuthSubtable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	legacy := `model_provider = "codex-api-gateway"

[model_providers.codex-api-gateway]
name = "Codex API Gateway"
base_url = "http://127.0.0.1:9870/v1"
wire_api = "responses"

[model_providers.codex-api-gateway.auth]
command = "echo codex-local"

[projects.x]
`
	path := seedConfigFile(t, home, legacy)
	m := New(func() string { return "http://127.0.0.1:9870/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if strings.Contains(got, "codex-api-gateway.auth") || strings.Contains(got, "echo codex-local") {
		t.Fatalf("重新启用后应清除旧版残留的 auth 子表:\n%s", got)
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

func TestEnableWritesModelCatalogJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := seedConfigFile(t, home, seedConfig)
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	want := `model_catalog_json = "` + codexModelsPath(home) + `"`
	if !strings.Contains(got, want) {
		t.Fatalf("启用后缺少 %q，实际内容:\n%s", want, got)
	}
	data, err := os.ReadFile(backupPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var state backupState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.ModelCatalogJSON != nil {
		t.Fatalf("原无 model_catalog_json 时备份应为 null，实际 %+v", state)
	}
}

func TestDisableRemovesModelCatalogJSONWhenOriginallyAbsent(t *testing.T) {
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
	if strings.Contains(got, "model_catalog_json =") {
		t.Fatalf("原无 model_catalog_json 时应删除该键:\n%s", got)
	}
	assertNotExist(t, backupPath(home))
}

func TestEnableBacksUpAndDisableRestoresExistingModelCatalogJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	custom := `model_catalog_json = "/custom/models.json"
` + seedConfig
	path := seedConfigFile(t, home, custom)
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, `model_catalog_json = "`+codexModelsPath(home)+`"`) {
		t.Fatalf("启用后未覆盖 model_catalog_json:\n%s", got)
	}
	data, err := os.ReadFile(backupPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var state backupState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.ModelCatalogJSON == nil || *state.ModelCatalogJSON != "/custom/models.json" {
		t.Fatalf("备份应保存自定义路径 /custom/models.json，实际 %+v", state)
	}
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	got = readFile(t, path)
	if !strings.Contains(got, `model_catalog_json = "/custom/models.json"`) {
		t.Fatalf("禁用后未恢复自定义 model_catalog_json:\n%s", got)
	}
}

func TestWriteModelsCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	data := []byte(`{"models":[]}`)
	if err := m.WriteModelsCatalog(data); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, codexModelsPath(home))
	if got != string(data) {
		t.Fatalf("models.json = %q, want %q", got, data)
	}
	if on, err := m.IsEnabled(); err != nil || on {
		t.Fatalf("仅写模型目录不应启用 Codex，on=%v err=%v", on, err)
	}
}

func TestEnableSyncsSessionHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	seedConfigFile(t, home, seedConfig)
	seedSessionForSync(t, home, "openai", "openai-session")
	seedSessionForSync(t, home, "codex-api-gateway", "gateway-session")
	seedSessionForSync(t, home, "anthropic", "other-session")
	statePath := seedManagerStateDB(t, home,
		[2]string{"openai-session", "openai"},
		[2]string{"gateway-session", "codex-api-gateway"},
		[2]string{"other-session", "anthropic"},
	)

	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"openai-session", "gateway-session", "other-session"} {
		if got := readSession(t, home, name); strings.Contains(got, `"model_provider"`) {
			t.Fatalf("应用后 %s 的 model_provider 应被自动清除:\n%s", name, got)
		}
	}
	if got := readManagerState(t, statePath, "openai-session"); got != "codex-api-gateway" {
		t.Fatalf("state 索引中 openai 会话应改写为 codex-api-gateway，实际 %q", got)
	}
	if got := readManagerState(t, statePath, "gateway-session"); got != "codex-api-gateway" {
		t.Fatalf("state 索引中网关会话应保持 codex-api-gateway，实际 %q", got)
	}
	if got := readManagerState(t, statePath, "other-session"); got != "codex-api-gateway" {
		t.Fatalf("state 索引中第三方会话应改写为 codex-api-gateway，实际 %q", got)
	}
}

func TestDisableSyncsSessionHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	_ = seedConfigFile(t, home, seedConfig)

	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	// 启用期间新产生的网关与第三方会话，用于验证还原时的自动同步。
	seedSessionForSync(t, home, "codex-api-gateway", "gateway-session")
	seedSessionForSync(t, home, "anthropic", "other-session")
	statePath := seedManagerStateDB(t, home,
		[2]string{"gateway-session", "codex-api-gateway"},
		[2]string{"other-session", "anthropic"},
	)
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gateway-session", "other-session"} {
		if got := readSession(t, home, name); strings.Contains(got, `"model_provider"`) {
			t.Fatalf("还原后 %s 的 model_provider 应被自动清除:\n%s", name, got)
		}
	}
	if got := readManagerState(t, statePath, "gateway-session"); got != "openai" {
		t.Fatalf("还原后 state 索引中网关会话应改写为 openai，实际 %q", got)
	}
	if got := readManagerState(t, statePath, "other-session"); got != "openai" {
		t.Fatalf("还原后 state 索引中第三方会话应改写为 openai，实际 %q", got)
	}
}

// seedManagerStateDB 在 home/.codex 下写入 state_5.sqlite 并填充
// threads 行；返回 state 库相对 home 的路径。
func seedManagerStateDB(t *testing.T, home string, rows ...[2]string) string {
	t.Helper()
	rel := filepath.Join(".codex", "state_5.sqlite")
	path := filepath.Join(home, rel)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(
		"CREATE TABLE IF NOT EXISTS threads (id TEXT PRIMARY KEY, model_provider TEXT, title TEXT)",
	); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, err := db.Exec(
			"INSERT OR REPLACE INTO threads (id, model_provider, title) VALUES (?, ?, ?)",
			row[0], row[1], "title-"+row[0],
		); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func readManagerState(t *testing.T, path, id string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var got string
	if err := db.QueryRow(
		"SELECT model_provider FROM threads WHERE id = ?", id,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func seedSessionForSync(t *testing.T, home, provider, name string) {
	t.Helper()
	rel := filepath.Join(".codex", "sessions", "2026", "09", "09", name+".jsonl")
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"session_meta","payload":{"model_provider":"` + provider + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSession(t *testing.T, home, name string) string {
	t.Helper()
	path := filepath.Join(home, ".codex", "sessions", "2026", "09", "09", name+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
