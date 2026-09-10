// Package codexsessions 清除 Codex 会话历史中的 model_provider 归属标记，
// 使切换提供商后历史会话仍可在 codex 恢复选择器中看到。清除包含两个存储：
// rollout JSONL 的 session_meta 字段，以及本地会话索引
// state_*.sqlite 的 threads.model_provider 列。索引必须把受管会话改写为
// 当前默认 provider（codex 恢复选择器按该列精确匹配），JSONL 同步移除
// 全部 provider 标记，保证会话被重新索引时按当前默认 provider 归入。
// 处理不区分 provider 归属：第三方 provider 与缺失 provider 的会话同样处理。
package codexsessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	sessionsDir = "sessions"
	// sessionMetaType 用于识别 rollout JSONL 中的 session_meta 事件行。
	sessionMetaType = `"type":"session_meta"`
	// responseItemType 用于识别 rollout JSONL 中的 response_item 事件行。
	responseItemType = `"type":"response_item"`
)

// Rewriter 清除会话历史中的 provider 归属标记与第三方明文推理内容，使切换
// 提供商后历史会话仍可在 codex 恢复选择器中看到且可被 OpenAI 协议接受。
type Rewriter struct {
	home         string
	sessionsRoot string
}

// New 创建会话清除器。home 为 Codex 配置目录（FindCodex 的结果）。
func New(home string) *Rewriter {
	return &Rewriter{
		home:         home,
		sessionsRoot: filepath.Join(home, sessionsDir),
	}
}

// Sync 扫描会话目录，移除全部 session_meta 行的 model_provider 字段，并把
// response_item 行中 reasoning item 的明文 content 折算进 summary。
// 返回清除的文件数；空的会话目录不报错。
func (r *Rewriter) Sync() (int, error) {
	info, err := os.Stat(r.sessionsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("codexsessions: 读取 %s 失败: %w", r.sessionsRoot, err)
	}
	if !info.IsDir() {
		return 0, nil
	}

	var rewritten int
	err = filepath.WalkDir(r.sessionsRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		changed, err := clearFile(path)
		if err != nil {
			return err
		}
		if changed {
			rewritten++
		}
		return nil
	})
	if err != nil {
		return rewritten, fmt.Errorf("codexsessions: 扫描会话目录失败: %w", err)
	}
	return rewritten, nil
}

// clearFile 逐行处理 rollout JSONL：对 session_meta 行删除全部
// model_provider 键值，对 response_item 行清洗明文 reasoning content，
// 其余字节原样保留。
func clearFile(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var out bytes.Buffer
	changed := false
	for _, line := range bytes.SplitAfter(raw, []byte{'\n'}) {
		if !bytes.Contains(line, []byte(sessionMetaType)) && !bytes.Contains(line, []byte(responseItemType)) {
			out.Write(line)
			continue
		}
		next := line
		if bytes.Contains(line, []byte(sessionMetaType)) {
			next = clearProviderKeys(line)
		}
		if bytes.Contains(line, []byte(responseItemType)) && bytes.Contains(line, []byte(`"type":"reasoning"`)) {
			next = clearReasoningContent(next)
		}
		if !bytes.Equal(next, line) {
			changed = true
		}
		out.Write(next)
	}
	if !changed {
		return false, nil
	}
	if err := writeFile(path, out.Bytes()); err != nil {
		return false, err
	}
	return true, nil
}

// clearReasoningContent 把 response_item 行中 reasoning item 归一化为
// 便携形态：明文 content（第三方 provider 写入的 reasoning_text part）
// 折入 summary、content 置空，并剥离 encrypted_content 与第三方 id。
// OpenAI Responses 协议只允许 reasoning 携带 summary 或官方签发的
// encrypted_content，携带明文 content 回灌会被上游以
// array_above_max_length 拒绝，无效密文与 id 也会被拒。
func clearReasoningContent(line []byte) []byte {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return line
	}
	if obj["type"] != "response_item" {
		return line
	}
	payload, ok := obj["payload"].(map[string]any)
	if !ok || payload["type"] != "reasoning" {
		return line
	}
	changed := false
	if texts := reasoningTexts(payload["content"]); len(texts) > 0 {
		payload["summary"] = appendSummaryTexts(payload["summary"], texts)
		payload["content"] = nil
		changed = true
	}
	if _, ok := payload["encrypted_content"]; ok {
		delete(payload, "encrypted_content")
		changed = true
	}
	if _, ok := payload["id"]; ok {
		delete(payload, "id")
		changed = true
	}
	if !changed {
		return line
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return line
	}
	if bytes.HasSuffix(line, []byte{'\n'}) {
		out = append(out, '\n')
	}
	return out
}

// reasoningTexts 提取 reasoning content 的明文文本，兼容对象 part
// （{"type":"reasoning_text","text":...}）与字符串简写（[...]）两种形态。
func reasoningTexts(content any) []string {
	arr, ok := content.([]any)
	if !ok {
		return nil
	}
	var texts []string
	for _, raw := range arr {
		switch part := raw.(type) {
		case string:
			if part != "" {
				texts = append(texts, part)
			}
		case map[string]any:
			if text, ok := part["text"].(string); ok && text != "" {
				texts = append(texts, text)
			}
		}
	}
	return texts
}

// appendSummaryTexts 把明文正文追加到 summary（summary_text part），
// 已存在的文本不重复追加，保证重复同步幂等。
func appendSummaryTexts(summary any, texts []string) []any {
	existing := map[string]bool{}
	var out []any
	if arr, ok := summary.([]any); ok {
		for _, raw := range arr {
			if part, ok := raw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					existing[text] = true
				}
			}
			out = append(out, raw)
		}
	}
	for _, text := range texts {
		if existing[text] {
			continue
		}
		out = append(out, map[string]any{
			"type": "summary_text",
			"text": text,
		})
	}
	return out
}

// clearProviderKeys 删除行内任意值的 model_provider 键，与 provider 归属
// 无关。三个模式覆盖 compact JSON 中键值位于对象首位、中间、末尾以及
// 唯一键四种情况，替换后仍是合法 JSON（provider 值是简单标识符，
// 不含引号转义）。
var (
	providerLeadingComma  = regexp.MustCompile(`,\s*"model_provider"\s*:\s*"[^"]*"`)
	providerTrailingComma = regexp.MustCompile(`"model_provider"\s*:\s*"[^"]*",\s*`)
	providerOnlyKey       = regexp.MustCompile(`"model_provider"\s*:\s*"[^"]*"`)
)

func clearProviderKeys(line []byte) []byte {
	out := line
	out = providerLeadingComma.ReplaceAll(out, nil)
	out = providerTrailingComma.ReplaceAll(out, nil)
	out = providerOnlyKey.ReplaceAll(out, nil)
	return out
}

// writeFile 原子写回会话文件：临时文件 + rename，保留原权限。
func writeFile(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rollout-sync-*.tmp")
	if err != nil {
		return fmt.Errorf("codexsessions: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codexsessions: 设置临时文件权限失败: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codexsessions: 写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("codexsessions: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("codexsessions: 替换 %s 失败: %w", path, err)
	}
	return nil
}
