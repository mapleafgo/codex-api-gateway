package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 自启 Spec 的工作目录必须等于用户目录，与直接从 $HOME 启动一致。
func TestAutostartSpecWorkDirIsUserHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".config", "codex-api-gateway", "config.yaml")

	spec := autostartSpec(configPath)
	if spec == nil {
		t.Fatal("autostartSpec returned nil")
	}
	if spec.WorkDir != home {
		t.Fatalf("WorkDir want user home %q, got %q", home, spec.WorkDir)
	}
	if len(spec.Args) != 3 || spec.Args[0] != "-config" || spec.Args[1] != configPath || spec.Args[2] != "-chdir-home" {
		t.Fatalf("Args want [-config <config> -chdir-home], got %v", spec.Args)
	}
}

func TestChdirUserHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	other := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	if err := chdirUserHome(); err != nil {
		t.Fatalf("chdirUserHome: %v", err)
	}
	got, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got != home {
		t.Fatalf("cwd want user home %q, got %q", home, got)
	}
}
