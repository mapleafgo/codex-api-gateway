package tray

import "strings"

// desktopSessionKeys 是打开 GUI 浏览器冷启动所需的会话变量。
// 只在进程自身缺失时才从桌面会话环境源补齐。
var desktopSessionKeys = []string{
	"DISPLAY",
	"WAYLAND_DISPLAY",
	"XAUTHORITY",
	"XDG_SESSION_TYPE",
	"XDG_SESSION_DESKTOP",
	"XDG_CURRENT_DESKTOP",
}

func hasDesktopDisplay(env []string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, "DISPLAY=") && len(e) > len("DISPLAY=") {
			return true
		}
		if strings.HasPrefix(e, "WAYLAND_DISPLAY=") && len(e) > len("WAYLAND_DISPLAY=") {
			return true
		}
	}
	return false
}

// mergeEnv 把 extras 中 base 尚未拥有的键补进去。
// 若没有任何新增键则返回 nil，表示调用方应沿用默认 os.Environ。
func mergeEnv(base []string, extras map[string]string) []string {
	if len(extras) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(base))
	for _, e := range base {
		if k, _, ok := strings.Cut(e, "="); ok {
			have[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(base)+len(extras))
	out = append(out, base...)
	added := false
	for k, v := range extras {
		if _, ok := have[k]; ok {
			continue
		}
		out = append(out, k+"="+v)
		added = true
	}
	if !added {
		return nil
	}
	return out
}
