package codexsessions

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	// modernc.org/sqlite 是纯 Go 的 sqlite 驱动，注册名 "sqlite"。
	_ "modernc.org/sqlite"
)

const stateDBPattern = "state_*.sqlite"

// SyncStateDB 把本地会话索引（state_*.sqlite 的 threads 表）的全部会话行
// 改写为 defaultProvider，不区分 provider 归属：第三方 provider、
// 缺失（NULL）与空 provider 的会话同样处理。codex 恢复选择器按
// threads.model_provider 精确匹配当前默认 provider（StateDbOnly 模式），
// 只清 JSONL 标记无法让历史会话在切换后可见，因此索引行必须落到当前
// 默认 provider。返回改写行数；目录内没有 state_*.sqlite 或表不存在时
// 视为无操作。
func (r *Rewriter) SyncStateDB(defaultProvider string) (int, error) {
	if defaultProvider == "" {
		return 0, nil
	}
	paths, err := filepath.Glob(filepath.Join(r.home, stateDBPattern))
	if err != nil {
		return 0, fmt.Errorf("codexsessions: 扫描会话索引失败: %w", err)
	}
	var updated int
	for _, path := range paths {
		n, err := rewriteThreadsProvider(path, defaultProvider)
		if err != nil {
			return updated, err
		}
		updated += n
	}
	return updated, nil
}

// rewriteThreadsProvider 把单个 state 库 threads 表全部会话行改写为
// 当前默认 provider。
func rewriteThreadsProvider(path, defaultProvider string) (int, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, fmt.Errorf("codexsessions: 打开会话索引 %s 失败: %w", path, err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		return 0, fmt.Errorf("codexsessions: 配置会话索引 %s 失败: %w", path, err)
	}
	var one int
	err = db.QueryRow(
		"SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'threads'",
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("codexsessions: 检查会话索引表 %s 失败: %w", path, err)
	}

	res, err := db.Exec(
		"UPDATE threads SET model_provider = ?", defaultProvider,
	)
	if err != nil {
		return 0, fmt.Errorf("codexsessions: 更新会话索引 %s 失败: %w", path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("codexsessions: 读取 %s 更新行数失败: %w", path, err)
	}
	return int(n), nil
}
