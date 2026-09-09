package codexsessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// seedStateDB 在 home 下创建 state_5.sqlite，rows 的元素为
// (id, provider)，provider 为 "" 时写空字符串，为 "<null>" 时写 NULL。
func seedStateDB(t *testing.T, home string, rows ...[2]string) string {
	t.Helper()
	path := filepath.Join(home, "state_5.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(
		"CREATE TABLE threads (id TEXT PRIMARY KEY, model_provider TEXT, title TEXT)",
	); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		var provider any = row[1]
		if row[1] == "<null>" {
			provider = nil
		}
		if _, err := db.Exec(
			"INSERT INTO threads (id, model_provider, title) VALUES (?, ?, ?)",
			xid(i), provider, "title-"+row[0],
		); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func xid(i int) string {
	return string(rune('a' + i))
}

func readStateProvider(t *testing.T, path, id string) (string, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var provider sql.NullString
	if err := db.QueryRow(
		"SELECT model_provider FROM threads WHERE id = ?", id,
	).Scan(&provider); err != nil {
		t.Fatal(err)
	}
	return provider.String, provider.Valid
}

// TestSyncStateDBRewritesAllProviders 验证全部会话行被改写为当前默认，
// 第三方 provider、空与 NULL provider 同样处理。
func TestSyncStateDBRewritesAllProviders(t *testing.T) {
	home := t.TempDir()
	path := seedStateDB(t, home,
		[2]string{"openai", "openai"},
		[2]string{"gateway", "codex-api-gateway"},
		[2]string{"third", "anthropic"},
		[2]string{"empty", ""},
		[2]string{"null", "<null>"},
	)

	rw := New(home)
	n, err := rw.SyncStateDB("codex-api-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("5 行应全部被改写，实际 %d", n)
	}
	if got, _ := readStateProvider(t, path, "a"); got != "codex-api-gateway" {
		t.Fatalf("openai 行应为 codex-api-gateway，实际 %q", got)
	}
	if got, _ := readStateProvider(t, path, "c"); got != "codex-api-gateway" {
		t.Fatalf("第三方行应被改写为 codex-api-gateway，实际 %q", got)
	}
	if got, _ := readStateProvider(t, path, "d"); got != "codex-api-gateway" {
		t.Fatalf("空 provider 行应为 codex-api-gateway，实际 %q", got)
	}
	if got, valid := readStateProvider(t, path, "e"); !valid || got != "codex-api-gateway" {
		t.Fatalf("NULL provider 行应为 codex-api-gateway，实际 valid=%v value=%q", valid, got)
	}
}

// TestSyncStateRestoresToOriginalProvider 验证还原时把全部会话行改写回
// 默认 provider（openai），包括第三方 provider 行。
func TestSyncStateRestoresToOriginalProvider(t *testing.T) {
	home := t.TempDir()
	path := seedStateDB(t, home,
		[2]string{"gateway", "codex-api-gateway"},
		[2]string{"third", "anthropic"},
	)

	rw := New(home)
	if _, err := rw.SyncStateDB("openai"); err != nil {
		t.Fatal(err)
	}
	if got, _ := readStateProvider(t, path, "a"); got != "openai" {
		t.Fatalf("网关行应改写为 openai，实际 %q", got)
	}
	if got, _ := readStateProvider(t, path, "b"); got != "openai" {
		t.Fatalf("第三方行也应改写为 openai，实际 %q", got)
	}
}

// TestSyncStateMissingStateFilesNoop 验证无 state_*.sqlite 时无操作。
func TestSyncStateMissingStateFilesNoop(t *testing.T) {
	home := t.TempDir()
	rw := New(home)
	n, err := rw.SyncStateDB("codex-api-gateway")
	if err != nil || n != 0 {
		t.Fatalf("缺失状态库应为 no-op (n=%d err=%v)", n, err)
	}
}

// TestSyncStateMissingThreadsTableNoop 验证索引库无 threads 表时无操作。
func TestSyncStateMissingThreadsTableNoop(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "state_5.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other (id TEXT)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	rw := New(home)
	n, err := rw.SyncStateDB("codex-api-gateway")
	if err != nil || n != 0 {
		t.Fatalf("无 threads 表应为 no-op (n=%d err=%v)", n, err)
	}
}

// TestSyncStateEmptyDefaultNoop 验证默认 provider 为空时无操作。
func TestSyncStateEmptyDefaultNoop(t *testing.T) {
	home := t.TempDir()
	seedStateDB(t, home, [2]string{"openai", "openai"})
	rw := New(home)
	n, err := rw.SyncStateDB("")
	if err != nil || n != 0 {
		t.Fatalf("空默认 provider 应为 no-op (n=%d err=%v)", n, err)
	}
}

// TestSyncStateMultiStateFiles 验证多个 state_*.sqlite 都被处理。
func TestSyncStateMultiStateFiles(t *testing.T) {
	home := t.TempDir()
	path1 := seedStateDB(t, home, [2]string{"a", "openai"})
	path2 := filepath.Join(home, "state_6.sqlite")
	if err := os.Rename(path1, path2); err != nil {
		t.Fatal(err)
	}
	path1b := filepath.Join(home, "state_7.sqlite")
	db, err := sql.Open("sqlite", path1b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"CREATE TABLE threads (id TEXT PRIMARY KEY, model_provider TEXT)",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"INSERT INTO threads (id, model_provider) VALUES ('b', 'openai')",
	); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	rw := New(home)
	n, err := rw.SyncStateDB("codex-api-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应改写 2 个库各 1 行，实际 %d", n)
	}
	if got, _ := readStateProvider(t, path2, "a"); got != "codex-api-gateway" {
		t.Fatalf("state_6 行应为 codex-api-gateway，实际 %q", got)
	}
	if got, _ := readStateProvider(t, path1b, "b"); got != "codex-api-gateway" {
		t.Fatalf("state_7 行应为 codex-api-gateway，实际 %q", got)
	}
}
