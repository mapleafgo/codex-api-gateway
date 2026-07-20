//go:build windows

package tray

// withDesktopSessionEnv 在 Windows 上无需注入桌面会话变量。
// rundll32 打开 URL 不依赖 DISPLAY 一类 Unix 会话环境。
func withDesktopSessionEnv(base []string) []string {
	return nil
}
