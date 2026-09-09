package codexsessions

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	// modernc.org/sqlite 是纯 Go 的 sqlite 驱动，注册名 "sqlite"。
	_ "modernc.org/sqlite"
)

const stateDBPattern = "state_*.sqlite"

// SyncStateDB 把本地会话索引（state_*.sqlite 的 threads 表）中
// clearable 集合对应 provider 或缺失/空 provider 的会话行，改写为
// defaultProvider。codex 恢复选择器按 threads.model_provider 精确匹配
// 当前默认 provider（StateDbOnly 模式），只清 JSONL 标记无法让历史会话
// 在切换后可见，因此索引行必须落到当前默认 provider。
// 返回改写行数；目录内没有 state_*.sqlite 或表不存在时视为无操作。
func (r *Rewriter) SyncStateDB(defaultProvider string) (int, error) {
	if defaultProvider == "" || len(r.clearable) == 0 {
		return 0, nil
	}
	paths, err := filepath.Glob(filepath.Join(r.home, stateDBPattern))
	if err != nil {
		return 0, fmt.Errorf("codexsessions: 扫描会话索引失败: %w", err)
	}
	providers := make([]string, 0, len(r.clearable))
	for id := range r.clearable {
		providers = append(providers, id)
	}
	sort.Strings(providers)
	var updated int
	for _, path := range paths {
		n, err := rewriteThreadsProvider(path, defaultProvider, providers)
		if err != nil {
			return updated, err
		}
		updated += n
	}
	return updated, nil
}

// rewriteThreadsProvider 改写单个 state 库中受管会话行的 provider。
// 只处理 clearable 集合内的 provider 与无 provider（NULL/空）的行，
// 其他 provider 的第三方会话原样保留。
func rewriteThreadsProvider(path, defaultProvider string, providers []string) (int, error) {
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

	placeholders := make([]string, len(providers))
	args := make([]any, 0, len(providers)+1)
	args = append(args, defaultProvider)
	for i, provider := range providers {
		placeholders[i] = "?"
		args = append(args, provider)
	}
	query := "UPDATE threads SET model_provider = ? WHERE model_provider IN (" +
		strings.Join(placeholders, ", ") +
		") OR model_provider IS NULL OR model_provider = ''"
	res, err := db.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("codexsessions: 更新会话索引 %s 失败: %w", path, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("codexsessions: 读取 %s 更新行数失败: %w", path, err)
	}
	return int(n), nil
}
