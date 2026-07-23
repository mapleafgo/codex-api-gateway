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
	var logCalls atomic.Int32
	w, err := New(path, holder, func() { reloads.Add(1) }, func(config.LoggingCfg) { logCalls.Add(1) })
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
	// 日志回调应随 reload 触发（管理页改日志配置需即时生效）。
	if logCalls.Load() == 0 {
		t.Errorf("日志回调未被调用")
	}
}

func TestWatcherKeepsOldConfigOnBadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, minimalYAML(":9999", "src1"))

	cfg, _ := config.Load(path)
	holder := config.NewHolder(cfg)

	w, err := New(path, holder, nil, nil)
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

	w, _ := New(path, holder, nil, nil)
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

	w, err := New(path, holder, nil, nil)
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

// TestWatcherLoggingCallbackGetsNewConfig 验证 reload 后日志回调收到最新 logging 配置。
func TestWatcherLoggingCallbackGetsNewConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, minimalYAML(":9999", "src1"))

	cfg, _ := config.Load(path)
	holder := config.NewHolder(cfg)

	var gotLevel atomic.Value
	w, _ := New(path, holder, nil, func(lc config.LoggingCfg) {
		gotLevel.Store(lc.Level)
	})
	t.Cleanup(func() { _ = w.Close() })

	// 把日志等级改成 debug 并写回
	writeFile(t, path, []byte("server:\n  listen: :9999\nlogging:\n  level: debug\nsources:\n  - name: src1\n    base_url: https://example.com\n    api_key: k\n    default_model: m\n"))
	w.Reload()

	// reload 同步，稍等确保回调执行
	time.Sleep(100 * time.Millisecond)
	if gotLevel.Load() != "debug" {
		t.Fatalf("日志回调收到的 level = %v, want debug", gotLevel.Load())
	}
}

// TestWatcherReloadsOnBaseInstructionsChange 验证本地编辑 base_instructions.md
// （与 config 同级）也会触发热重载，内存 BaseInstructions 更新。
func TestWatcherReloadsOnBaseInstructionsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, minimalYAML(":9999", "src1"))
	// 初始无基线指令文件：BaseInstructions 为空
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.BaseInstructions != "" {
		t.Fatalf("初始 BaseInstructions 应为空")
	}
	holder := config.NewHolder(cfg)

	w, err := New(path, holder, nil, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// 本地编辑器创建/写入同级 base_instructions.md
	biPath := filepath.Join(dir, config.BaseInstructionsFileName)
	const content = "You are a hot-reloaded base instruction."
	writeFile(t, biPath, []byte(content))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if holder.Current().BaseInstructions == content {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("BaseInstructions = %q, want %q", holder.Current().BaseInstructions, content)
}

// TestWatcherIgnoresUnrelatedSiblingFile 验证同目录无关文件变化不触发 reload。
func TestWatcherIgnoresUnrelatedSiblingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, minimalYAML(":9999", "src1"))
	cfg, _ := config.Load(path)
	holder := config.NewHolder(cfg)

	var reloads atomic.Int32
	w, err := New(path, holder, func() { reloads.Add(1) }, nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// 写无关文件
	writeFile(t, filepath.Join(dir, "notes.txt"), []byte("noise"))
	time.Sleep(600 * time.Millisecond)
	if reloads.Load() != 0 {
		t.Fatalf("无关文件不应触发 reload, reloads=%d", reloads.Load())
	}
	if holder.Current().Server.Listen != ":9999" {
		t.Fatalf("配置被意外改动")
	}
}
