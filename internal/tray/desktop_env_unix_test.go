//go:build unix

package tray

import (
	"strings"
	"testing"
)

// TestWithDesktopSessionEnvInjectsFromSystemd 在无 DISPLAY 时从 user manager 注入。
// 依赖本机 systemctl --user show-environment 可用；CI headless 无桌面时
// show-environment 可能没有 DISPLAY，此时跳过。
func TestWithDesktopSessionEnvInjectsFromSystemd(t *testing.T) {
	resetDesktopSessionEnvForTest()
	base := []string{
		"HOME=/tmp",
		"PATH=/usr/bin",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
	}
	got := withDesktopSessionEnv(base)
	if got == nil {
		t.Skip("当前 user manager 未提供 DISPLAY/WAYLAND_DISPLAY，跳过")
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "DISPLAY=") && !strings.Contains(joined, "WAYLAND_DISPLAY=") {
		t.Fatalf("注入后应含 DISPLAY 或 WAYLAND_DISPLAY，实际: %v", got)
	}
	if !strings.Contains(joined, "HOME=/tmp") {
		t.Fatalf("应保留 base HOME: %v", got)
	}
}
