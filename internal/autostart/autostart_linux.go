//go:build linux

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IsEnabled 报告 XDG autostart 下是否已有对应 .desktop。
func (s Spec) IsEnabled() (bool, error) {
	if err := s.validate(); err != nil {
		return false, err
	}
	_, err := os.Stat(s.desktopPath())
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Enable 写入 ~/.config/autostart/<AppID>.desktop。
func (s Spec) Enable() error {
	if err := s.validate(); err != nil {
		return err
	}
	dir := s.autostartDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("autostart: 创建目录: %w", err)
	}
	body := s.desktopContent()
	path := s.desktopPath()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("autostart: 写入 desktop: %w", err)
	}
	return nil
}

// Disable 删除 .desktop；文件不存在视为已关闭（幂等）。
func (s Spec) Disable() error {
	if err := s.validate(); err != nil {
		return err
	}
	err := os.Remove(s.desktopPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("autostart: 删除 desktop: %w", err)
	}
	return nil
}

func (s Spec) autostartDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "autostart")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "autostart")
}

func (s Spec) desktopPath() string {
	return filepath.Join(s.autostartDir(), s.AppID+".desktop")
}

// desktopContent 生成 .desktop 文件正文。
func (s Spec) desktopContent() string {
	var b strings.Builder
	b.WriteString("[Desktop Entry]\n")
	b.WriteString("Type=Application\n")
	b.WriteString("Version=1.0\n")
	b.WriteString("Name=" + s.DisplayName + "\n")
	b.WriteString("Exec=" + joinQuoted(append([]string{s.Exec}, s.Args...)) + "\n")
	if s.WorkDir != "" {
		b.WriteString("Path=" + s.WorkDir + "\n")
	}
	b.WriteString("Terminal=false\n")
	b.WriteString("Categories=Network;Utility;\n")
	b.WriteString("StartupNotify=false\n")
	b.WriteString("X-GNOME-Autostart-enabled=true\n")
	return b.String()
}
