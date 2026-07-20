//go:build linux

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxEnableDisable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	s := Spec{
		AppID:       "codex-api-gateway",
		DisplayName: "Codex API Gateway",
		Exec:        "/home/user/codex-api-gateway",
		Args:        []string{"-config", "/home/user/config.yaml"},
		WorkDir:     "/home/user",
	}

	on, err := s.IsEnabled()
	if err != nil || on {
		t.Fatalf("初始应未启用: on=%v err=%v", on, err)
	}
	if err := s.Enable(); err != nil {
		t.Fatal(err)
	}
	on, err = s.IsEnabled()
	if err != nil || !on {
		t.Fatalf("Enable 后应启用: on=%v err=%v", on, err)
	}
	path := filepath.Join(tmp, "autostart", "codex-api-gateway.desktop")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"[Desktop Entry]",
		"Name=Codex API Gateway",
		"Exec=/home/user/codex-api-gateway -config /home/user/config.yaml",
		"Path=/home/user",
		"Terminal=false",
		"X-GNOME-Autostart-enabled=true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("desktop 缺少 %q\n%s", want, text)
		}
	}
	// 幂等 Enable
	if err := s.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := s.Disable(); err != nil {
		t.Fatal(err)
	}
	on, err = s.IsEnabled()
	if err != nil || on {
		t.Fatalf("Disable 后应关闭: on=%v err=%v", on, err)
	}
	// 幂等 Disable
	if err := s.Disable(); err != nil {
		t.Fatal(err)
	}
}
