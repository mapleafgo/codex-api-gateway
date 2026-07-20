//go:build unix

package tray

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// withDesktopSessionEnv 返回给子进程使用的环境。
// 若 base 已含 DISPLAY 或 WAYLAND_DISPLAY，返回 nil（沿用默认 os.Environ）。
// 否则尝试从 systemd --user show-environment 注入缺失的桌面会话变量；
// 注入失败时同样返回 nil。
//
// 推荐用 .desktop 自启（packaging/install-autostart.sh），进程会天然
// 继承图形会话环境；本函数是 systemd --user service 等 headless 启动
// 路径的兜底。
func withDesktopSessionEnv(base []string) []string {
	if hasDesktopDisplay(base) {
		return nil
	}
	session := loadDesktopSessionEnv()
	if len(session) == 0 {
		return nil
	}
	return mergeEnv(base, session)
}

var (
	sessionEnvOnce sync.Once
	sessionEnv     map[string]string
)

func loadDesktopSessionEnv() map[string]string {
	sessionEnvOnce.Do(func() {
		sessionEnv = fetchSystemdUserEnv()
	})
	return sessionEnv
}

func fetchSystemdUserEnv() map[string]string {
	out := map[string]string{}
	cmd := exec.Command("systemctl", "--user", "show-environment")
	cmd.Env = minimalUserManagerEnv()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return out
	}
	wanted := make(map[string]struct{}, len(desktopSessionKeys))
	for _, k := range desktopSessionKeys {
		wanted[k] = struct{}{}
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || v == "" {
			continue
		}
		if _, hit := wanted[k]; !hit {
			continue
		}
		out[k] = v
	}
	return out
}

func minimalUserManagerEnv() []string {
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	if v := os.Getenv("HOME"); v != "" {
		env = append(env, "HOME="+v)
	}
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		env = append(env, "XDG_RUNTIME_DIR="+v)
	}
	if v := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); v != "" {
		env = append(env, "DBUS_SESSION_BUS_ADDRESS="+v)
	}
	return env
}

// resetDesktopSessionEnvForTest 仅测试用：清空缓存。
func resetDesktopSessionEnvForTest() {
	sessionEnvOnce = sync.Once{}
	sessionEnv = nil
}
