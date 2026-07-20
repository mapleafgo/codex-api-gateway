//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IsEnabled 报告 LaunchAgents 下是否已有对应 plist。
func (s Spec) IsEnabled() (bool, error) {
	if err := s.validate(); err != nil {
		return false, err
	}
	_, err := os.Stat(s.plistPath())
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Enable 写入 ~/Library/LaunchAgents/<AppID>.plist。
func (s Spec) Enable() error {
	if err := s.validate(); err != nil {
		return err
	}
	dir := s.launchAgentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("autostart: 创建 LaunchAgents: %w", err)
	}
	if err := os.WriteFile(s.plistPath(), []byte(s.plistContent()), 0o644); err != nil {
		return fmt.Errorf("autostart: 写入 plist: %w", err)
	}
	return nil
}

// Disable 删除 plist；不存在视为已关闭。
func (s Spec) Disable() error {
	if err := s.validate(); err != nil {
		return err
	}
	err := os.Remove(s.plistPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("autostart: 删除 plist: %w", err)
	}
	return nil
}

func (s Spec) launchAgentsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents")
}

func (s Spec) plistPath() string {
	return filepath.Join(s.launchAgentsDir(), s.AppID+".plist")
}

func (s Spec) plistContent() string {
	// Label 使用反向 DNS 风格更稳妥，但为与 AppID 一致且简单，直接用 AppID。
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString(`  <dict>` + "\n")
	b.WriteString(`    <key>Label</key>` + "\n")
	b.WriteString(`    <string>` + xmlEscape(s.AppID) + `</string>` + "\n")
	b.WriteString(`    <key>ProgramArguments</key>` + "\n")
	b.WriteString(`    <array>` + "\n")
	b.WriteString(`      <string>` + xmlEscape(s.Exec) + `</string>` + "\n")
	for _, a := range s.Args {
		b.WriteString(`      <string>` + xmlEscape(a) + `</string>` + "\n")
	}
	b.WriteString(`    </array>` + "\n")
	if s.WorkDir != "" {
		b.WriteString(`    <key>WorkingDirectory</key>` + "\n")
		b.WriteString(`    <string>` + xmlEscape(s.WorkDir) + `</string>` + "\n")
	}
	b.WriteString(`    <key>RunAtLoad</key>` + "\n")
	b.WriteString(`    <true/>` + "\n")
	b.WriteString(`    <key>AbandonProcessGroup</key>` + "\n")
	b.WriteString(`    <true/>` + "\n")
	b.WriteString(`  </dict>` + "\n")
	b.WriteString(`</plist>` + "\n")
	return b.String()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer(
		`&`, `&amp;`,
		`<`, `&lt;`,
		`>`, `&gt;`,
		`"`, `&quot;`,
		`'`, `&apos;`,
	)
	return r.Replace(s)
}
