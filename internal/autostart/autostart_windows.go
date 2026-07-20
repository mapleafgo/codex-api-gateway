//go:build windows

package autostart

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// IsEnabled 查询 HKCU Run 是否存在本 AppID 值。
func (s Spec) IsEnabled() (bool, error) {
	if err := s.validate(); err != nil {
		return false, err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, fmt.Errorf("autostart: 打开 Run 键: %w", err)
	}
	defer k.Close()
	_, _, err = k.GetStringValue(s.AppID)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("autostart: 读取 Run 值: %w", err)
	}
	return true, nil
}

// Enable 在 HKCU Run 写入命令行。
func (s Spec) Enable() error {
	if err := s.validate(); err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: 创建/打开 Run 键: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(s.AppID, s.commandLine()); err != nil {
		return fmt.Errorf("autostart: 写入 Run 值: %w", err)
	}
	return nil
}

// Disable 删除 Run 值；不存在视为已关闭。
func (s Spec) Disable() error {
	if err := s.validate(); err != nil {
		return err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return fmt.Errorf("autostart: 打开 Run 键: %w", err)
	}
	defer k.Close()
	err = k.DeleteValue(s.AppID)
	if err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("autostart: 删除 Run 值: %w", err)
	}
	return nil
}
