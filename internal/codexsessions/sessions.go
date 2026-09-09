// Package codexsessions 清除 Codex 会话历史中的 model_provider 归属标记，
// 使切换提供商后历史会话仍可在 codex 恢复选择器中看到。清除包含两个存储：
// rollout JSONL 的 session_meta 字段，以及本地会话索引
// state_*.sqlite 的 threads.model_provider 列。索引必须把受管会话改写为
// 当前默认 provider（codex 恢复选择器按该列精确匹配），JSONL 清除
// 用于会话被重新索引时按当前默认 provider 归入。
package codexsessions

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	sessionsDir = "sessions"
	// sessionMetaType 用于识别 rollout JSONL 中的 session_meta 事件行。
	sessionMetaType = `"type":"session_meta"`
)

// Rewriter 按需清除会话文件 session_meta 行中的 model_provider 标记。
type Rewriter struct {
	home         string
	sessionsRoot string
	clearable    map[string]struct{}
}

// New 创建会话清除器。home 为 Codex 配置目录（FindCodexHome 的结果），
// sources 是需要清除 model_provider 的 provider 集合（通常为
// codex-api-gateway 与启用前的原 provider）。
func New(home string, sources ...string) *Rewriter {
	set := make(map[string]struct{}, len(sources))
	for _, s := range sources {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	return &Rewriter{
		home:         home,
		sessionsRoot: filepath.Join(home, sessionsDir),
		clearable:    set,
	}
}

// Sync 扫描会话目录，清除 clearable 集合中 provider 标记的
// model_provider 字段。返回清除的文件数；跳过其他 provider 的会话。
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
		changed, err := clearFile(path, r.clearable)
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

// clearFile 逐行处理 rollout JSONL：仅对 session_meta 行删除 clearable
// 集合中 provider 对应的 model_provider 键值，其余字节原样保留。
func clearFile(path string, clearable map[string]struct{}) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var out bytes.Buffer
	changed := false
	for _, line := range bytes.SplitAfter(raw, []byte{'\n'}) {
		if !bytes.Contains(line, []byte(sessionMetaType)) {
			out.Write(line)
			continue
		}
		next := clearProviderKeys(line, clearable)
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

// clearProviderKeys 删除行内 clearable 对应值的 model_provider 键。
// 三个模式覆盖 compact JSON 中键值位于对象任意位置（首个、中间、唯一个）的情况。
func clearProviderKeys(line []byte, clearable map[string]struct{}) []byte {
	out := line
	for id := range clearable {
		kv := `"model_provider":"` + id + `"`
		out = bytes.ReplaceAll(out, []byte(kv+","), nil)
		out = bytes.ReplaceAll(out, []byte(","+kv), nil)
		out = bytes.ReplaceAll(out, []byte(kv+"}"), []byte("}"))
	}
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
