package codexsessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sessionLine = `{"type":"session_meta","payload":{"session_id":"s1","id":"s1","timestamp":"2026-09-09T01:49:59.523Z","cwd":"/tmp/x","originator":"codex-cli","cli_version":"0.153.4","source":"cli","thread_source":"user","model_provider":"codex-api-gateway","history_mode":"paginated"}}`

const sessionsRoot = "sessions"

// decodeLine 解析单行 JSON，供断言使用。
func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(line))
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("解析 JSON 失败: %v\n%s", err, line)
	}
	return obj
}

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

	rw := New(home)
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
			changed, err := clearFile(path)
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

// TestSyncClearsThirdPartyProviders 验证第三方 provider 的会话同样被清除。
func TestSyncClearsThirdPartyProviders(t *testing.T) {
	home := t.TempDir()
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-b.jsonl"),
		`{"type":"session_meta","payload":{"model_provider":"anthropic"}}`+"\n")

	rw := New(home)
	n, err := rw.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应清除第三方 provider 会话，实际 %d", n)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `"model_provider"`) {
		t.Fatalf("第三方 provider 的 model_provider 应被清除:\n%s", got)
	}
}

// TestSyncClearsMissingProvider 验证缺失 model_provider 键的会话
// 同样被处理（不报错、不损坏原内容）。
func TestSyncClearsMissingProvider(t *testing.T) {
	home := t.TempDir()
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-f.jsonl"),
		`{"type":"session_meta","payload":{"session_id":"s9"}}`+"\n")

	rw := New(home)
	n, err := rw.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("无 provider 键的会话未发生字节变化，实际 %d", n)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"session_id":"s9"`) {
		t.Fatalf("无 provider 键会话应原样保留:\n%s", got)
	}
}

// TestSyncSkipsNonSessionMetaEvents 验证只处理 session_meta 行。
func TestSyncSkipsNonSessionMetaEvents(t *testing.T) {
	home := t.TempDir()
	event := `{"type":"event_msg","payload":{"model_provider":"anthropic","turn_id":"t1"}}` + "\n"
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-c.jsonl"), event+sessionLine+"\n")

	rw := New(home)
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

	rw := New(home)
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
	rw := New(home)
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
	rw := New(home)
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

// TestClearReasoningFoldsContentToSummary 验证明文 reasoning content
// （对象 part 形态）被折算进 summary、content 置空、id 剥离。
func TestClearReasoningFoldsContentToSummary(t *testing.T) {
	home := t.TempDir()
	line := `{"timestamp":"2026-09-09T15:21:12.778Z","ordinal":163,"type":"response_item","payload":{"type":"reasoning","id":"rs_resp_1","summary":[],"content":[{"type":"reasoning_text","text":"Need more context here."}],"encrypted_content":null}}`
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-r1.jsonl"), line+"\n")

	changed, err := clearFile(path)
	if err != nil || !changed {
		t.Fatalf("reasoning content 应被清洗 (changed=%v err=%v)", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := decodeLine(t, string(got))
	payload := obj["payload"].(map[string]any)
	if payload["type"] != "reasoning" {
		t.Fatalf("reasoning 类型应保留:\n%s", got)
	}
	if _, ok := payload["id"]; ok {
		t.Fatalf("第三方 id 应剥离:\n%s", got)
	}
	if payload["content"] != nil {
		t.Fatalf("明文 content 应置空:\n%s", got)
	}
	summary := payload["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("summary 应包含折算后正文，实际 %d 项:\n%s", len(summary), got)
	}
	if summary[0].(map[string]any)["text"] != "Need more context here." {
		t.Fatalf("summary 文本应来自 content:\n%s", got)
	}
}

// TestClearReasoningStringShorthand 验证字符串简写 content 同样折算。
func TestClearReasoningStringShorthand(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"response_item","payload":{"type":"reasoning","id":"rs_2","summary":[],"content":["Plan the next step."]}}`
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-r2.jsonl"), line+"\n")

	changed, err := clearFile(path)
	if err != nil || !changed {
		t.Fatalf("字符串简写 content 应被清洗 (changed=%v err=%v)", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := decodeLine(t, string(got))
	payload := obj["payload"].(map[string]any)
	if payload["content"] != nil {
		t.Fatalf("content 应置空:\n%s", got)
	}
	summary := payload["summary"].([]any)
	if len(summary) != 1 || summary[0].(map[string]any)["text"] != "Plan the next step." {
		t.Fatalf("summary 应包含折算后的字符串正文:\n%s", got)
	}
}

// TestClearReasoningStripsEncryptedContent 验证带 encrypted_content 的
// reasoning 剥离密文，明文 content 折入 summary（便携形态）。
func TestClearReasoningStripsEncryptedContent(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"response_item","payload":{"type":"reasoning","id":"rs_3","summary":[],"content":[{"type":"reasoning_text","text":"leak"}],"encrypted_content":"enc-abc"}}`
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-r3.jsonl"), line+"\n")

	if _, err := clearFile(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := decodeLine(t, string(got))
	payload := obj["payload"].(map[string]any)
	if _, ok := payload["encrypted_content"]; ok {
		t.Fatalf("encrypted_content 应被剥离:\n%s", got)
	}
	if payload["content"] != nil {
		t.Fatalf("明文 content 应置空:\n%s", got)
	}
	summary := payload["summary"].([]any)
	if len(summary) != 1 || summary[0].(map[string]any)["text"] != "leak" {
		t.Fatalf("明文应折入 summary:\n%s", got)
	}
}

// TestClearReasoningStripsForeignIDFromPlainForm 验证无明文的 reasoning
// 仍会剥离遗留的第三方 id，且不动原有 summary 与 status。
func TestClearReasoningStripsForeignIDFromPlainForm(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"response_item","payload":{"type":"reasoning","id":"rs_4","summary":[{"type":"summary_text","text":"ok"}],"content":null,"status":"completed"}}`
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-r4.jsonl"), line+"\n")

	changed, err := clearFile(path)
	if err != nil || !changed {
		t.Fatalf("残留 id 应被剥离 (changed=%v err=%v)", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := decodeLine(t, string(got))
	payload := obj["payload"].(map[string]any)
	if _, ok := payload["id"]; ok {
		t.Fatalf("第三方 id 应剥离:\n%s", got)
	}
	summary := payload["summary"].([]any)
	if len(summary) != 1 || summary[0].(map[string]any)["text"] != "ok" {
		t.Fatalf("无明文时不应改动 summary:\n%s", got)
	}
	if payload["status"] != "completed" {
		t.Fatalf("status 应保留:\n%s", got)
	}
}

// TestClearReasoningLeavesCleanFormUntouched 验证无明文、无 id、无密文的
// 便携形态 reasoning 保持字节稳定（幂等前置条件）。
func TestClearReasoningLeavesCleanFormUntouched(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"ok"}],"content":null}}`
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-r4b.jsonl"), line+"\n")

	changed, err := clearFile(path)
	if err != nil || changed {
		t.Fatalf("干净形态不应被改动 (changed=%v err=%v)", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != line+"\n" {
		t.Fatalf("文件字节应原样保留:\n%s", got)
	}
}

// TestClearReasoningSkipsNonReasoningItem 验证 message 等其他
// response_item 行即使带 content 也不改动。
func TestClearReasoningSkipsNonReasoningItem(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-r5.jsonl"), line+"\n")

	changed, err := clearFile(path)
	if err != nil || changed {
		t.Fatalf("非 reasoning item 不应被改动 (changed=%v err=%v)", changed, err)
	}
}

// TestClearReasoningIdempotent 验证二次同步不再产生变化。
func TestClearReasoningIdempotent(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"response_item","payload":{"type":"reasoning","id":"rs_6","summary":[],"content":[{"type":"reasoning_text","text":"once"}]}}`
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-r6.jsonl"), line+"\n")
	if _, err := clearFile(path); err != nil {
		t.Fatal(err)
	}
	changed, err := clearFile(path)
	if err != nil || changed {
		t.Fatalf("二次同步应无变化 (changed=%v err=%v)", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := decodeLine(t, string(got))
	payload := obj["payload"].(map[string]any)
	if len(payload["summary"].([]any)) != 1 {
		t.Fatalf("幂等同步不应重复追加 summary:\n%s", got)
	}
}

// TestSyncFoldsReasoningAndClearsProvider 验证一次同步同时处理
// model_provider 标记与 reasoning content。
func TestSyncFoldsReasoningAndClearsProvider(t *testing.T) {
	home := t.TempDir()
	reasoning := `{"type":"response_item","payload":{"type":"reasoning","id":"rs_7","summary":[],"content":["fold me"]}}` + "\n"
	path := seedSession(t, home, filepath.Join(sessionsRoot, "2026/09/09", "rollout-r7.jsonl"), sessionLine+"\n"+reasoning)

	rw := New(home)
	n, err := rw.Sync()
	if err != nil || n != 1 {
		t.Fatalf("应处理 1 个会话 (n=%d err=%v)", n, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(got)
	if strings.Contains(content, `"model_provider"`) {
		t.Fatalf("model_provider 应被清除:\n%s", content)
	}
	if !strings.Contains(content, `"type":"summary_text"`) || strings.Contains(content, `"content": ["fold me"]`) {
		t.Fatalf("reasoning content 应折算进 summary:\n%s", content)
	}
}
