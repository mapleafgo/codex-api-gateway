// Package codexconfig 读写 Codex CLI 的用户配置目录与 config.toml，
// 供托盘「应用 Codex」开关复用 codex-rs 的配置目录判定逻辑。
package codexconfig

import (
	"fmt"
	"os"
	"path/filepath"
)

// FindCodexHome 返回 Codex 配置目录，判定逻辑与 codex-rs
// utils/home-dir/src/lib.rs 的 find_codex_home 对齐：
// CODEX_HOME 非空时必须存在且是目录并 canonicalize；未设置时默认 $HOME/.codex。
func FindCodexHome() (string, error) {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return resolveCodexHome(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codexconfig: 无法确定用户主目录: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// resolveCodexHome 处理 CODEX_HOME 分支：镜像 codex 的路径存在性/目录校验。
func resolveCodexHome(val string) (string, error) {
	info, err := os.Stat(val)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("codexconfig: CODEX_HOME 指向 %q，但该路径不存在", val)
		}
		return "", fmt.Errorf("codexconfig: 读取 CODEX_HOME %q 失败: %w", val, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("codexconfig: CODEX_HOME 指向 %q，但不是目录", val)
	}
	abs, err := filepath.Abs(val)
	if err != nil {
		return "", fmt.Errorf("codexconfig: 解析 CODEX_HOME %q 失败: %w", val, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("codexconfig: 解析符号链接 %q 失败: %w", val, err)
	}
	return resolved, nil
}
