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
