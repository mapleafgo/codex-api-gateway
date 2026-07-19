package configwatch

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

func TestWatcherReloadsOnFileChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, minimalYAML(":9999", "src1"))

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	holder := config.NewHolder(cfg)

	var reloads atomic.Int32
	w, err := New(path, holder, func() { reloads.Add(1) })
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// 修改文件
	writeFile(t, path, minimalYAML(":8888", "src2"))

	// 等待 fsnotify + debounce 触发
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if holder.Current().Server.Listen == ":8888" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if holder.Current().Server.Listen != ":8888" {
		t.Fatalf("Server.Listen = %q, want :8888", holder.Current().Server.Listen)
	}
	// sources 顺序也应是新的
	if len(holder.Current().Sources) != 1 || holder.Current().Sources[0].Name != "src2" {
		t.Errorf("Sources = %+v", holder.Current().Sources)
	}
}

func TestWatcherKeepsOldConfigOnBadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, minimalYAML(":9999", "src1"))

	cfg, _ := config.Load(path)
	holder := config.NewHolder(cfg)

	w, err := New(path, holder, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// 写入非法 YAML（重复键，koanf/yaml 会拒绝解析）
	writeFile(t, path, []byte("server:\n  listen: :oops\n  listen: :dup\n"))

	// 等待 reload 尝试
	time.Sleep(500 * time.Millisecond)

	// 应该保留旧配置
	if holder.Current().Server.Listen != ":9999" {
		t.Errorf("Server.Listen = %q, want :9999 (保留旧配置)", holder.Current().Server.Listen)
	}
	if err := w.LastLoadErr(); err == nil {
		t.Errorf("LastLoadErr 应非空")
	}
}

func TestWatcherManualReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, minimalYAML(":9999", "src1"))

	cfg, _ := config.Load(path)
	holder := config.NewHolder(cfg)

	w, _ := New(path, holder, nil)
	t.Cleanup(func() { _ = w.Close() })

	writeFile(t, path, minimalYAML(":7777", "src2"))
	w.Reload() // 手动触发

	// 等待 reload 完成（reload 是同步的，但保守起见稍等）
	time.Sleep(100 * time.Millisecond)
	if holder.Current().Server.Listen != ":7777" {
		t.Fatalf("Server.Listen = %q, want :7777", holder.Current().Server.Listen)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func minimalYAML(listen, srcName string) []byte {
	return []byte("server:\n  listen: " + listen + "\nsources:\n  - name: " + srcName + "\n    base_url: https://example.com\n    api_key: k\n    default_model: m\n")
}

// TestCloseIdempotent 验证多次调用 Close 不会 panic。
// 场景：shutdownHandler 与 main 的 defer 都会调用 Close，必须幂等。
func TestCloseIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, minimalYAML(":9999", "src1"))

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	holder := config.NewHolder(cfg)

	w, err := New(path, holder, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}

	// 第一次关闭应当成功。
	if err := w.Close(); err != nil {
		t.Fatalf("首次 Close 失败: %v", err)
	}
	// 第二次关闭不得 panic（修复点：曾因 close(w.stop) 重复而 panic）。
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("重复 Close 触发 panic: %v", rec)
		}
	}()
	if err := w.Close(); err != nil {
		t.Fatalf("二次 Close 应返回 nil，实际 %v", err)
	}
	// 第三次同样安全。
	_ = w.Close()
}
