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
)

// Rewriter 清除会话文件 session_meta 行中的 model_provider 标记。
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

// Sync 扫描会话目录，移除全部 session_meta 行的 model_provider 字段。
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

// clearFile 逐行处理 rollout JSONL：仅对 session_meta 行删除全部
// model_provider 键值，其余字节原样保留。
func clearFile(path string) (bool, error) {
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
		next := clearProviderKeys(line)
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
