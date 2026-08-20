# Codex「应用到 Codex」托盘开关 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 系统托盘新增「应用到 Codex」勾选项，把 Codex CLI 的 `$CODEX_HOME/config.toml` 指向本网关；取消勾选恢复启用前的 `model_provider` 原值。

**Architecture:** 新增 `internal/codexconfig`：`FindCodexHome` 一比一复刻 codex-rs 的 `CODEX_HOME` 判定；行级精确编辑 `config.toml`（不重排、不解析重写），备份原 `model_provider` 到侧车 JSON；启用时整体覆盖 `codex-api-gateway` provider 块并置 `model_provider`，禁用只恢复 `model_provider`。托盘复用「开机自启」的 `AddCheckbox` + 重建菜单模式，`main` 注入 base URL 并在配置就绪后刷新菜单。

**Tech Stack:** Go 1.26 标准库（`os`/`path/filepath`/`encoding/json`/`sync`/`log/slog`），零新依赖；`gogpu/systray` 托盘；`task check` + `task test-race` 门禁。

## Global Constraints

- 零新依赖：只允许标准库完成 `internal/codexconfig`。
- 不自动创建 `~/.codex` 或 `config.toml`：`Enable` 在文件缺失时返回错误；`Disable` 在文件/备份缺失且未启用时 no-op。
- provider 标识固定 `codex-api-gateway`；每次启用整体覆盖该块（旧版嵌套 `auth` 表随覆盖清除），`base_url` 取当前网关监听。
- 写入的 provider 块不声明 `auth` / `env_key` / `requires_openai_auth`（网关 `/v1/*` 不校验入站 Authorization，codex `validate` 允许无 auth 的 provider）。
- 备份文件 `~/.codex/codex-api-gateway-backup.json` 权限 0600；`config.toml` 原子写回（临时文件 + rename）并保留原权限，新文件 0600。
- 代码注释、commit message 用中文；日志走 `log/slog`，禁止 `fmt.Print*`。
- `README.md` 当前含用户未提交改动：不得暂存/提交用户改动；本次 README 变更留在工作区，最终向用户说明。
- 每个任务结束跑对应 `go test`；全部完成跑 `task check` 与 `task test-race`。

## File Structure

| 文件 | 职责 |
|---|---|
| `internal/codexconfig/home.go` | `FindCodexHome`：复刻 codex `find_codex_home` |
| `internal/codexconfig/home_test.go` | 对齐 codex 单测的 4 个用例 |
| `internal/codexconfig/toml_edit.go` | 行级编辑助手：顶层键与表块的增删改查 |
| `internal/codexconfig/toml_edit_test.go` | 行级编辑助手测试 |
| `internal/codexconfig/manager.go` | `Manager`：`IsEnabled/Enable/Disable` + 备份 |
| `internal/codexconfig/manager_test.go` | 开关全流程测试 |
| `internal/tray/tray.go` | `Config.Codex` 字段、「应用到 Codex」勾选项、`RefreshMenu` |
| `internal/tray/tray_test.go` | 托盘开关回调测试 |
| `cmd/server/main.go` | 注入 base URL 闭包、config 加载后刷新菜单 |
| `cmd/server/main_test.go` | `codexBaseURL` 测试 |
| `README.md` | 系统托盘节补充「应用到 Codex」说明（不提交，见全局约束） |

---

### Task 1: `FindCodexHome`（配置目录判定）

**Files:**
- Create: `internal/codexconfig/home.go`
- Create: `internal/codexconfig/home_test.go`

**Interfaces:**
- Consumes: 无
- Produces: `func FindCodexHome() (string, error)` —— 后续 Task 3 依赖

- [ ] **Step 1: 写失败测试**

创建 `internal/codexconfig/home_test.go`：

```go
package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindCodexHomeEnvMissingPathIsFatal(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex-home"))
	_, err := FindCodexHome()
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失 CODEX_HOME 应报不存在错误，实际 %v", err)
	}
}

func TestFindCodexHomeEnvFilePathIsFatal(t *testing.T) {
	file := filepath.Join(t.TempDir(), "codex-home.txt")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", file)
	_, err := FindCodexHome()
	if err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("文件型 CODEX_HOME 应报非目录错误，实际 %v", err)
	}
}

func TestFindCodexHomeEnvValidDirectoryCanonicalizes(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "codex-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("当前平台不支持符号链接: %v", err)
	}
	t.Setenv("CODEX_HOME", link)
	got, err := FindCodexHome()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("FindCodexHome=%q want %q", got, want)
	}
}

func TestFindCodexHomeWithoutEnvUsesDefaultHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	got, err := FindCodexHome()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".codex"); got != want {
		t.Fatalf("FindCodexHome=%q want %q", got, want)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/codexconfig -run TestFindCodexHome -count=1`
Expected: `FAIL`，`undefined: FindCodexHome`（包目录刚创建）。

- [ ] **Step 3: 最小实现**

创建 `internal/codexconfig/home.go`：

```go
// Package codexconfig 读写 Codex CLI 的用户配置目录与 config.toml，
// 供托盘「应用到 Codex」开关复用 codex-rs 的配置目录判定逻辑。
package codexconfig

import (
	"fmt"
	"os"
	"path/filepath"
)

// FindCodexHome 返回 Codex 配置目录，判定逻辑与 codex-rs
// utils/home-dir/src/lib.rs 的 find_codex_home 对齐：
// CODEX_HOME 非空时必须存在且是目录并 canonicalize；未设置时默认 $HOME/.codex。
func FindCodexHome() (string, error) {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return resolveCodexHome(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codexconfig: 无法确定用户主目录: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// resolveCodexHome 处理 CODEX_HOME 分支：镜像 codex 的路径存在性/目录校验。
func resolveCodexHome(val string) (string, error) {
	info, err := os.Stat(val)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("codexconfig: CODEX_HOME 指向 %q，但该路径不存在", val)
		}
		return "", fmt.Errorf("codexconfig: 读取 CODEX_HOME %q 失败: %w", val, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("codexconfig: CODEX_HOME 指向 %q，但不是目录", val)
	}
	abs, err := filepath.Abs(val)
	if err != nil {
		return "", fmt.Errorf("codexconfig: 解析 CODEX_HOME %q 失败: %w", val, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("codexconfig: 解析符号链接 %q 失败: %w", val, err)
	}
	return resolved, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/codexconfig -run TestFindCodexHome -count=1`
Expected: `PASS`

- [ ] **Step 5: 提交**

```bash
git add internal/codexconfig/home.go internal/codexconfig/home_test.go
git commit -m "feat(codexconfig): 复刻 codex CODEX_HOME 配置目录判定"
```

---

### Task 2: `config.toml` 行级编辑助手

**Files:**
- Create: `internal/codexconfig/toml_edit.go`
- Create: `internal/codexconfig/toml_edit_test.go`

**Interfaces:**
- Consumes: Task 1 的包目录（无直接依赖）
- Produces:
  - `func topLevelKey(lines []string, key string) (string, bool)`
  - `func upsertTopLevelKey(lines []string, key, value string) []string`
  - `func removeTopLevelKey(lines []string, key string) []string`
  - `func upsertTableBlock(lines []string, header string, block []string) []string`
  - `func removeTableBlock(lines []string, header string) []string`
  - `func tableValue(lines []string, header, key string) (string, bool)`

- [ ] **Step 1: 写失败测试**

创建 `internal/codexconfig/toml_edit_test.go`：

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/codexconfig -run 'TestTopLevelKey|TestUpsert|TestRemove|TestTableValue' -count=1`
Expected: `FAIL`，`undefined: topLevelKey` 等编译错误。

- [ ] **Step 3: 最小实现**

创建 `internal/codexconfig/toml_edit.go`：

```go
package codexconfig

import "strings"

// isTableHeader 判断行是否为 TOML 表头（[xxx] 或 [[xxx]]）。
func isTableHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")
}

// topLevelKey 读取顶层键值（只扫描首个表头之前的行），返回 (值, 是否存在)。
func topLevelKey(lines []string, key string) (string, bool) {
	prefix := key + " ="
	for _, line := range lines {
		if isTableHeader(line) {
			return "", false
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return parseStringValue(trimmed[len(prefix):]), true
		}
	}
	return "", false
}

// parseStringValue 从 "xxx" 形式提取字符串值；无法解析时返回空串。
func parseStringValue(rest string) string {
	start := strings.IndexByte(rest, '"')
	if start < 0 {
		return ""
	}
	end := strings.LastIndexByte(rest, '"')
	if end <= start {
		return ""
	}
	return rest[start+1 : end]
}

// upsertTopLevelKey 设置顶层键：已存在则整行替换，否则插入到首个表头之前。
func upsertTopLevelKey(lines []string, key, value string) []string {
	replacement := key + ` = "` + value + `"`
	for i, line := range lines {
		if isTableHeader(line) {
			lines = append(lines[:i], append([]string{replacement}, lines[i:]...)...)
			return lines
		}
		if strings.HasPrefix(strings.TrimSpace(line), key+" =") {
			lines[i] = replacement
			return lines
		}
	}
	return append(lines, replacement)
}

// removeTopLevelKey 删除顶层键行（只删首个表头之前出现的第一个）。
func removeTopLevelKey(lines []string, key string) []string {
	for i, line := range lines {
		if isTableHeader(line) {
			return lines
		}
		if strings.HasPrefix(strings.TrimSpace(line), key+" =") {
			return append(lines[:i], lines[i+1:]...)
		}
	}
	return lines
}

// blockEndIndex 返回从表头 start 开始的块结束下标：
// 遇到非当前表子表的表头或文件末尾为止。
func blockEndIndex(lines []string, start int) int {
	header := strings.TrimSpace(lines[start])
	prefix := header + "."
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if isTableHeader(trimmed) {
			if strings.HasPrefix(trimmed, prefix) {
				continue
			}
			return i
		}
	}
	return len(lines)
}

// upsertTableBlock 整体覆盖 table 块（含嵌套子表），不存在时追加到文件末尾。
func upsertTableBlock(lines []string, header string, block []string) []string {
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			start = i
			break
		}
	}
	if start < 0 {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		return append(lines, block...)
	}
	end := blockEndIndex(lines, start)
	out := make([]string, 0, start+len(block)+(len(lines)-end))
	out = append(out, lines[:start]...)
	out = append(out, block...)
	out = append(out, lines[end:]...)
	return out
}

// removeTableBlock 删除 table 块（含嵌套子表）。
func removeTableBlock(lines []string, header string) []string {
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			start = i
			break
		}
	}
	if start < 0 {
		return lines
	}
	end := blockEndIndex(lines, start)
	out := make([]string, 0, start+(len(lines)-end))
	out = append(out, lines[:start]...)
	out = append(out, lines[end:]...)
	return out
}

// tableValue 读取 table 块内键值（含嵌套子表前）。
func tableValue(lines []string, header, key string) (string, bool) {
	for i, line := range lines {
		if strings.TrimSpace(line) != header {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if isTableHeader(trimmed) {
				return "", false
			}
			if strings.HasPrefix(trimmed, key+" =") {
				return parseStringValue(trimmed[len(key)+2:]), true
			}
		}
	}
	return "", false
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/codexconfig -run 'TestTopLevelKey|TestUpsert|TestRemove|TestTableValue' -count=1`
Expected: `PASS`

- [ ] **Step 5: 提交**

```bash
git add internal/codexconfig/toml_edit.go internal/codexconfig/toml_edit_test.go
git commit -m "feat(codexconfig): 新增 config.toml 行级编辑助手"
```

---

### Task 3: `Manager`（启用/禁用/勾选态）

**Files:**
- Create: `internal/codexconfig/manager.go`
- Create: `internal/codexconfig/manager_test.go`

**Interfaces:**
- Consumes: `FindCodexHome`（Task 1）、行级编辑助手（Task 2）
- Produces:
  - `func New(baseURL func() string) *Manager`
  - `func (m *Manager) IsEnabled() (bool, error)`
  - `func (m *Manager) Enable() error`
  - `func (m *Manager) Disable() error`

- [ ] **Step 1: 写失败测试**

创建 `internal/codexconfig/manager_test.go`：

```go
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
	return filepath.Join(home, "config.toml")
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
	assertNotExist(t, filepath.Join(home, backupFileName))
}

func TestEnablePreservesExistingConfigAndBacksUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := configPath(home)
	if err := os.WriteFile(path, []byte(seedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
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
	data, err := os.ReadFile(filepath.Join(home, backupFileName))
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
	path := configPath(home)
	if err := os.WriteFile(path, []byte(seedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
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
	data, err := os.ReadFile(filepath.Join(home, backupFileName))
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
	path := configPath(home)
	if err := os.WriteFile(path, []byte(seedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
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
	path := configPath(home)
	if err := os.WriteFile(path, []byte(seedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
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
	assertNotExist(t, filepath.Join(home, backupFileName))
}

func TestDisableFailsWhenBackupMissingWhileEnabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := configPath(home)
	if err := os.WriteFile(path, []byte(seedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(func() string { return "http://127.0.0.1:8383/v1" })
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(home, backupFileName)
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable(); err != nil {
		t.Fatalf("备份缺失时也应能关闭，实际 %v", err)
	}
	got := readFile(t, path)
	if strings.Contains(got, "model_provider =") {
		t.Fatalf("备份缺失关闭后应移除 model_provider 键:\n%s", got)
	}
	if !strings.Contains(got, "[model_providers.codex-api-gateway]") {
		t.Fatalf("关闭后 provider 块应保留:\n%s", got)
	}
}

func TestDisableNoopWhenNotEnabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := configPath(home)
	if err := os.WriteFile(path, []byte(seedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
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
	path := configPath(home)
	noProvider := "# 无 model_provider\nmodel = \"glm-latest\"\n"
	if err := os.WriteFile(path, []byte(noProvider), 0o600); err != nil {
		t.Fatal(err)
	}
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
	path := configPath(home)
	if err := os.WriteFile(path, []byte(seedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/codexconfig -run 'TestEnable|TestDisable|TestIsEnabled' -count=1`
Expected: `FAIL`，`undefined: New` / `undefined: backupState` 编译错误。

- [ ] **Step 3: 最小实现**

创建 `internal/codexconfig/manager.go`：

```go
package codexconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	providerID   = "codex-api-gateway"
	providerName = "Codex API Gateway"
	wireAPI      = "responses"

	backupFileName = "codex-api-gateway-backup.json"
)

// providerHeader 是我们要整体覆盖的表头。
var providerHeader = "[model_providers." + providerID + "]"

// backupState 记录启用前的 model_provider 原值，null 表示原文件无该键。
type backupState struct {
	ModelProvider *string `json:"model_provider"`
}

// Manager 管理 Codex「应用到 Codex」开关：修改 $CODEX_HOME/config.toml 的
// model_provider 与 codex-api-gateway provider 块。方法并发安全。
type Manager struct {
	baseURL func() string
	mu      sync.Mutex
}

// New 创建 Manager。baseURL 返回网关 base URL（含 /v1），
// 为空时 Enable/IsEnabled 返回明确错误。
func New(baseURL func() string) *Manager {
	return &Manager{baseURL: baseURL}
}

// IsEnabled 报告 Codex 是否已指向本网关：model_provider 为 codex-api-gateway
// 且块内 base_url 与当前网关地址一致。config.toml 缺失时返回 (false, nil)。
func (m *Manager) IsEnabled() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	home, path, err := resolveConfigPaths()
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("codexconfig: 读取 %s 失败: %w", path, err)
	}
	return isEnabled(raw, m.currentBaseURL()), nil
}

// Enable 把 Codex 指向本网关：备份原 model_provider、整体覆盖 provider 块、
// 设置 model_provider。config.toml 缺失时返回错误，不自建文件。
func (m *Manager) Enable() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	base := m.currentBaseURL()
	if base == "" {
		return errors.New("codexconfig: 网关地址尚未就绪")
	}
	home, path, err := resolveConfigPaths()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("codexconfig: 未找到 %s，请先运行一次 codex 生成配置", path)
		}
		return fmt.Errorf("codexconfig: 读取 %s 失败: %w", path, err)
	}
	if isEnabled(raw, base) {
		return nil
	}
	lines := strings.Split(string(raw), "\n")

	// 原 model_provider 已是我们的值时不再写备份，避免丢失真正的原值。
	if value, ok := topLevelKey(lines, "model_provider"); !ok || value != providerID {
		if err := writeBackup(filepath.Join(home, backupFileName), lines); err != nil {
			return err
		}
	}

	lines = upsertTableBlock(lines, providerHeader, providerBlock(base))
	lines = upsertTopLevelKey(lines, "model_provider", providerID)
	return writeConfig(path, lines)
}

// Disable 恢复启用前的 model_provider 原值并删除备份；provider 块保留。
func (m *Manager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	home, path, err := resolveConfigPaths()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // config.toml 缺失：no-op，不创建文件
		}
		return fmt.Errorf("codexconfig: 读取 %s 失败: %w", path, err)
	}
	backupPath := filepath.Join(home, backupFileName)
	data, err := os.ReadFile(backupPath)
	if err != nil {
		if os.IsNotExist(err) {
			lines := strings.Split(string(raw), "\n")
			if value, ok := topLevelKey(lines, "model_provider"); ok && value == providerID {
				// 备份缺失但仍指向网关：原值不可考，移除网关注入键并回落默认 provider。
				lines = removeTopLevelKey(lines, "model_provider")
				lines = removeTopLevelKey(lines, "model_catalog_json")
				if err := writeConfig(path, lines); err != nil {
					return err
				}
				slog.Warn("codexconfig: 备份缺失，已移除网关注入键，model_provider 回落 Codex 默认", "backup", backupPath)
				return nil
			}
			return nil
		}
		return fmt.Errorf("codexconfig: 读取备份 %s 失败: %w", backupPath, err)
	}
	var state backupState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("codexconfig: 解析备份 %s 失败: %w", backupPath, err)
	}
	lines := strings.Split(string(raw), "\n")
	if state.ModelProvider == nil {
		lines = removeTopLevelKey(lines, "model_provider")
	} else {
		lines = upsertTopLevelKey(lines, "model_provider", *state.ModelProvider)
	}
	if err := writeConfig(path, lines); err != nil {
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return fmt.Errorf("codexconfig: 删除备份 %s 失败: %w", backupPath, err)
	}
	return nil
}

// resolveConfigPaths 返回 codex 主目录与 config.toml 绝对路径。
func resolveConfigPaths() (home, configPath string, err error) {
	home, err = FindCodexHome()
	if err != nil {
		return "", "", err
	}
	return home, filepath.Join(home, "config.toml"), nil
}

func (m *Manager) currentBaseURL() string {
	if m.baseURL == nil {
		return ""
	}
	return m.baseURL()
}

// isEnabled 判断给定文件内容是否已指向本网关。
func isEnabled(raw []byte, base string) bool {
	if base == "" {
		return false
	}
	lines := strings.Split(string(raw), "\n")
	value, ok := topLevelKey(lines, "model_provider")
	if !ok || value != providerID {
		return false
	}
	blockBase, ok := tableValue(lines, providerHeader, "base_url")
	return ok && blockBase == base
}

// providerBlock 返回 provider 表块。网关 /v1/* 不校验入站 Authorization，
// 因此不声明 auth/env_key（codex validate 允许无 auth 的 provider）。
func providerBlock(base string) []string {
	return []string{
		providerHeader,
		`name = "` + providerName + `"`,
		`base_url = "` + base + `"`,
		`wire_api = "` + wireAPI + `"`,
	}
}

// writeBackup 把当前 model_provider 原值写入侧车文件（0600）。
func writeBackup(path string, lines []string) error {
	value, ok := topLevelKey(lines, "model_provider")
	var state backupState
	if ok {
		state.ModelProvider = &value
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("codexconfig: 序列化备份失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("codexconfig: 写入备份 %s 失败: %w", path, err)
	}
	return nil
}

// writeConfig 原子写回 config.toml：临时文件 + rename，保留原文件权限。
func writeConfig(path string, lines []string) error {
	content := []byte(strings.Join(lines, "\n") + "\n")
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.toml.tmp-*")
	if err != nil {
		return fmt.Errorf("codexconfig: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codexconfig: 设置临时文件权限失败: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codexconfig: 写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("codexconfig: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("codexconfig: 替换 %s 失败: %w", path, err)
	}
	return nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/codexconfig -count=1`
Expected: `PASS`

- [ ] **Step 5: 提交**

```bash
git add internal/codexconfig/manager.go internal/codexconfig/manager_test.go
git commit -m "feat(codexconfig): 新增「应用到 Codex」开关管理器"
```

---

### Task 4: 托盘与 main 接线

**Files:**
- Modify: `internal/tray/tray.go`
- Modify: `internal/tray/tray_test.go`
- Modify: `cmd/server/main.go`
- Create: `cmd/server/main_test.go`

**Interfaces:**
- Consumes: `codexconfig.New/IsEnabled/Enable/Disable`（Task 3）
- Produces:
  - `tray.Config.Codex *codexconfig.Manager`
  - `func (t *Tray) RefreshMenu()`
  - `func codexBaseURL(listen string) string`

- [ ] **Step 1: 写 `codexBaseURL` 失败测试**

创建 `cmd/server/main_test.go`：

```go
package main

import "testing"

func TestCodexBaseURL(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		{":8383", "http://localhost:8383/v1"},
		{"127.0.0.1:9870", "http://127.0.0.1:9870/v1"},
		{"0.0.0.0:8383", "http://localhost:8383/v1"},
	}
	for _, tc := range cases {
		if got := codexBaseURL(tc.listen); got != tc.want {
			t.Errorf("codexBaseURL(%q)=%q want %q", tc.listen, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./cmd/server -run TestCodexBaseURL -count=1`
Expected: `FAIL`，`undefined: codexBaseURL`。

- [ ] **Step 3: 实现 `codexBaseURL`**

在 `cmd/server/main.go` 的 `adminURLFromListen` 之后追加：

```go
// codexBaseURL 把 server.listen 转成 Codex provider 的 base_url（含 /v1）。
func codexBaseURL(listen string) string {
	return strings.TrimSuffix(adminURLFromListen(listen), "/") + "/v1"
}
```

并在 `main.go` import 块中加入 `"strings"`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./cmd/server -run TestCodexBaseURL -count=1`
Expected: `PASS`

- [ ] **Step 5: 写托盘回调失败测试**

在 `internal/tray/tray_test.go` 末尾追加：

```go
func TestOnCodexToggleEnablesAndRestores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("model_provider = \"openai\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := codexconfig.New(func() string { return "http://127.0.0.1:8383/v1" })
	tr := New(Config{Codex: mgr})

	tr.onCodexToggle()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `model_provider = "codex-api-gateway"`) {
		t.Fatalf("勾选后应启用 Codex:\n%s", data)
	}

	tr.onCodexToggle()
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `model_provider = "openai"`) {
		t.Fatalf("取消勾选后应恢复:\n%s", data)
	}
}

func TestRefreshMenuSafeWhenNoTray(t *testing.T) {
	tr := New(Config{})
	tr.RefreshMenu() // 不应 panic
}
```

对应 import 追加：`"os"`、`"path/filepath"`、`"github.com/mapleafgo/codex-api-gateway/internal/codexconfig"`。

- [ ] **Step 6: 运行测试确认失败**

Run: `go test ./internal/tray -run 'TestOnCodexToggle|TestRefreshMenu' -count=1`
Expected: `FAIL`，`unknown field Codex in struct literal` / `undefined: RefreshMenu`。

- [ ] **Step 7: 实现托盘接线**

修改 `internal/tray/tray.go`：

import 追加：

```go
	"github.com/mapleafgo/codex-api-gateway/internal/codexconfig"
```

`Config` 结构体追加字段（`Autostart` 之后）：

```go
	// Codex 非 nil 时显示「应用到 Codex」勾选菜单，把 Codex CLI 指向本网关。
	Codex *codexconfig.Manager
```

`buildMenu` 改为：

```go
func (t *Tray) buildMenu() *systray.Menu {
	menu := systray.NewMenu()
	if t.cfg.OpenURLFunc != nil {
		menu.Add("打开", t.onOpen)
		menu.AddSeparator()
	}
	if t.cfg.Codex != nil {
		enabled := false
		if on, err := t.cfg.Codex.IsEnabled(); err != nil {
			slog.Debug("查询 Codex 接入状态失败", "error", err)
		} else {
			enabled = on
		}
		menu.AddCheckbox("应用到 Codex", enabled, t.onCodexToggle)
	}
	if t.cfg.Autostart != nil {
		enabled := false
		if on, err := t.cfg.Autostart.IsEnabled(); err != nil {
			slog.Debug("查询开机自启状态失败", "error", err)
		} else {
			enabled = on
		}
		menu.AddCheckbox("开机自启", enabled, t.onAutostartToggle)
	}
	if t.cfg.Codex != nil || t.cfg.Autostart != nil {
		menu.AddSeparator()
	}
	menu.Add("退出", t.onQuit)
	return menu
}
```

`refreshMenu` 之后追加导出方法与回调：

```go
// RefreshMenu 重建托盘菜单（用于 base URL 就绪后刷新「应用到 Codex」勾选态）。
func (t *Tray) RefreshMenu() {
	t.refreshMenu()
}

// onCodexToggle 切换 Codex 接入；失败时保持原勾选并记 WARN。
func (t *Tray) onCodexToggle() {
	if t.cfg.Codex == nil {
		return
	}
	on, err := t.cfg.Codex.IsEnabled()
	if err != nil {
		slog.Warn("查询 Codex 接入状态失败", "error", err)
		return
	}
	if on {
		if err := t.cfg.Codex.Disable(); err != nil {
			slog.Warn("关闭 Codex 接入失败", "error", err)
			return
		}
		slog.Info("已关闭 Codex 接入")
	} else {
		if err := t.cfg.Codex.Enable(); err != nil {
			slog.Warn("开启 Codex 接入失败", "error", err)
			return
		}
		slog.Info("已开启 Codex 接入")
	}
	t.refreshMenu()
}
```

- [ ] **Step 8: 运行测试确认通过**

Run: `go test ./internal/tray -count=1`
Expected: `PASS`

- [ ] **Step 9: main 注入**

修改 `cmd/server/main.go`：

import 追加：

```go
	"github.com/mapleafgo/codex-api-gateway/internal/codexconfig"
```

`tray.New` 调用之前追加：

```go
	// Codex base URL 在 config.Load 前未知：托盘先创建，配置就绪后
	// 写入 codexBase 并刷新菜单，勾选态按真实监听地址判定。
	var (
		codexMu   sync.RWMutex
		codexBase string
	)
	codexMgr := codexconfig.New(func() string {
		codexMu.RLock()
		defer codexMu.RUnlock()
		return codexBase
	})
```

`tray.New(tray.Config{...})` 中追加：

```go
		Codex: codexMgr,
```

`adminURL` 赋值之后追加：

```go
	codexMu.Lock()
	codexBase = codexBaseURL(cfg.Server.Listen)
	codexMu.Unlock()
	t.RefreshMenu()
```

- [ ] **Step 10: 编译与测试**

Run: `go build ./... && go test ./cmd/server ./internal/tray ./internal/codexconfig -count=1`
Expected: 全部 `PASS`。

- [ ] **Step 11: 提交**

```bash
git add cmd/server/main.go cmd/server/main_test.go internal/tray/tray.go internal/tray/tray_test.go
git commit -m "feat(tray): 托盘新增「应用到 Codex」勾选项"
```

---

### Task 5: 文档与门禁

**Files:**
- Modify: `README.md`（不提交，见全局约束）

- [ ] **Step 1: 补充 README**

在 README「开机自启（推荐托盘勾选）」小节之后追加：

```markdown
### 应用到 Codex（推荐托盘勾选）

托盘菜单提供 **「应用到 Codex」** 勾选项：勾选后把 Codex CLI 的用户配置
`$CODEX_HOME/config.toml` 指向本网关（新增 `model_providers.codex-api-gateway`
并置 `model_provider = "codex-api-gateway"`，`base_url` 自动取当前监听端口）；
取消勾选恢复启用前的 `model_provider` 原值。

- 启用前的原值备份在 `~/.codex/codex-api-gateway-backup.json`，恢复后自动删除。
- `config.toml` 不存在时不自动创建，请先运行一次 codex 生成配置。
- 网关监听端口变更后，重新勾选一次即可刷新 provider 块的 `base_url`。
```

- [ ] **Step 2: 检查 README 未提交改动**

Run: `git status --short`
Expected: `README.md` 仍显示 ` M`（含用户已有改动与本次新增），不要 `git add README.md`。

- [ ] **Step 3: 全量门禁**

Run: `task check`
Expected: 全部通过。

Run: `task test-race`
Expected: 全部通过。

- [ ] **Step 4: 提交（不含 README）**

```bash
git status --short
git add internal/ cmd/ docs/superpowers/specs/2026-08-06-codex-config-toggle-design.md docs/superpowers/plans/2026-08-06-codex-config-toggle.md
git commit -m "feat(codexconfig): 托盘「应用到 Codex」开关实现与设计文档"
```

若工作区此时只有本任务的产物且无用户改动，可另行提交 README：

```bash
git add README.md
git commit -m "docs: README 补充「应用到 Codex」托盘开关说明"
```

否则 README 变更留给用户确认，最终答复中说明。
