// Package autostart 在用户登录图形会话时注册/取消本应用的开机自启。
//
// 平台实现（均保持 CGO_ENABLED=0）：
//   - Linux: XDG ~/.config/autostart/<AppID>.desktop
//   - Windows: HKCU\...\CurrentVersion\Run 注册表值
//   - macOS: ~/Library/LaunchAgents/<AppID>.plist
//
// 真相源是 OS 注册本身，不写入 config.yaml。
package autostart

import (
	"fmt"
	"strings"
)

// Spec 描述要注册的自启应用。
type Spec struct {
	// AppID 用作 desktop/plist 文件名与注册表值名，须为安全标识符
	//（字母数字、点、连字符、下划线），例如 "codex-api-gateway"。
	AppID string
	// DisplayName 用户可见名称（desktop Name / 日志）。
	DisplayName string
	// Exec 可执行文件绝对路径。
	Exec string
	// Args 传给 Exec 的参数（不含可执行文件本身）。
	Args []string
	// WorkDir 工作目录；Linux desktop 的 Path=；空则省略。
	WorkDir string
}

// validate 检查必填字段与 AppID 字符集，防止路径穿越。
func (s Spec) validate() error {
	if s.AppID == "" {
		return fmt.Errorf("autostart: AppID 不能为空")
	}
	for _, r := range s.AppID {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("autostart: AppID 含非法字符 %q", s.AppID)
		}
	}
	if s.Exec == "" {
		return fmt.Errorf("autostart: Exec 不能为空")
	}
	if s.DisplayName == "" {
		return fmt.Errorf("autostart: DisplayName 不能为空")
	}
	return nil
}

// commandLine 返回拼接后的命令行（可执行文件 + 参数），用于日志与 Windows 注册表。
// 含空格的片段用双引号包裹；内部双引号转义为 \".
func (s Spec) commandLine() string {
	parts := make([]string, 0, 1+len(s.Args))
	parts = append(parts, s.Exec)
	parts = append(parts, s.Args...)
	return joinQuoted(parts)
}

func joinQuoted(parts []string) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		if p == "" || strings.ContainsAny(p, " \t\"") {
			out[i] = `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
		} else {
			out[i] = p
		}
	}
	return strings.Join(out, " ")
}
