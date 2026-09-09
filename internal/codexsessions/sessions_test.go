package codexsessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sessionLine = `{"type":"session_meta","payload":{"session_id":"s1","id":"s1","timestamp":"2026-09-09T01:49:59.523Z","cwd":"/tmp/x","originator":"codex-cli","cli_version":"0.153.4","source":"cli","thread_source":"user","model_provider":"codex-api-gateway","history_mode":"paginated"}}`

const sessionsRoot = "sessions"

func seedSession(t *testing.T, home, rel, content string) string {
	t.Helper()
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSyncRemovesGatewayProvider 验证清除 codex-api-gateway 会话的
// model_provider，且保留其他字段。
func TestSyncRemovesGatewayProvider(t *testing.T) {
	home := t.TempDir()
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-a.jsonl"), sessionLine+"\n")

	rw := New(home, "codex-api-gateway", "openai")
	n, err := rw.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应清除 1 个会话，实际 %d", n)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `"model_provider"`) {
		t.Fatalf("model_provider 应被清除:\n%s", got)
	}
	if !strings.Contains(string(got), `"cwd":"/tmp/x"`) || !strings.Contains(string(got), `"originator":"codex-cli"`) {
		t.Fatalf("清除破坏了其他字段:\n%s", got)
	}
}

// TestClearProviderKeyPositions 覆盖 compact JSON 中键值位于对象
// 首、中间、尾以及单独出现四种位置的情况。
func TestClearProviderKeyPositions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "middle",
			in:   `{"type":"session_meta","payload":{"a":1,"model_provider":"openai","b":2}}`,
			want: `{"type":"session_meta","payload":{"a":1,"b":2}}`,
		},
		{
			name: "first",
			in:   `{"type":"session_meta","payload":{"model_provider":"openai","b":2}}`,
			want: `{"type":"session_meta","payload":{"b":2}}`,
		},
		{
			name: "last",
			in:   `{"type":"session_meta","payload":{"a":1,"model_provider":"openai"}}`,
			want: `{"type":"session_meta","payload":{"a":1}}`,
		},
		{
			name: "only",
			in:   `{"type":"session_meta","payload":{"model_provider":"openai"}}`,
			want: `{"type":"session_meta","payload":{}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-x.jsonl"), tc.in+"\n")
			changed, err := clearFile(path, map[string]struct{}{"openai": {}})
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("应发生清除")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want+"\n" {
				t.Fatalf("清除结果不符\nwant: %s\ngot : %s", tc.want, got)
			}
		})
	}
}

// TestSyncSkipsNonRewritableProviders 验证目标之外的 provider 会话不被清除。
func TestSyncSkipsNonRewritableProviders(t *testing.T) {
	home := t.TempDir()
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-b.jsonl"),
		`{"type":"session_meta","payload":{"model_provider":"anthropic"}}`+"\n")

	rw := New(home, "codex-api-gateway", "openai")
	n, err := rw.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("不应清除外部 provider 会话，实际 %d", n)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"model_provider":"anthropic"`) {
		t.Fatalf("原内容应保留:\n%s", got)
	}
}

// TestSyncSkipsNonSessionMetaEvents 验证只处理 session_meta 行。
func TestSyncSkipsNonSessionMetaEvents(t *testing.T) {
	home := t.TempDir()
	event := `{"type":"event_msg","payload":{"model_provider":"anthropic","turn_id":"t1"}}` + "\n"
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-c.jsonl"), event+sessionLine+"\n")

	rw := New(home, "codex-api-gateway", "openai")
	n, err := rw.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应只处理 1 个会话，实际 %d", n)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"model_provider":"anthropic"`) {
		t.Fatalf("非 session_meta 行不应被清除:\n%s", got)
	}
	if strings.Contains(string(got), `"model_provider":"codex-api-gateway"`) {
		t.Fatalf("session_meta 行应被清除:\n%s", got)
	}
}

// TestSyncIdempotent 验证再次同步不重复处理。
func TestSyncIdempotent(t *testing.T) {
	home := t.TempDir()
	seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-d.jsonl"), sessionLine+"\n")

	rw := New(home, "codex-api-gateway", "openai")
	if _, err := rw.Sync(); err != nil {
		t.Fatal(err)
	}
	n, err := rw.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("重复同步应无处理，实际 %d", n)
	}
}

// TestSyncMissingSessionsRootNoop 验证会话目录缺失时不报错、不处理。
func TestSyncMissingSessionsRootNoop(t *testing.T) {
	home := t.TempDir()
	rw := New(home, "codex-api-gateway", "openai")
	n, err := rw.Sync()
	if err != nil || n != 0 {
		t.Fatalf("缺失会话目录应为 no-op (n=%d err=%v)", n, err)
	}
}

// TestSyncPreservesFileMode 验证处理保留原文件权限。
func TestSyncPreservesFileMode(t *testing.T) {
	home := t.TempDir()
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-e.jsonl"), sessionLine+"\n")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	rw := New(home, "codex-api-gateway", "openai")
	if _, err := rw.Sync(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("文件权限应为 0640，实际 %o", info.Mode().Perm())
	}
}
