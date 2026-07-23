// Package configwatch 提供配置文件的热重载：监听 config.yaml 与同级
// base_instructions.md 的变化，重新解析并以新 *Config 替换 Holder，
// 同时回调通知 scheduler reload。
//
// 写回路径（admin 保存）与外部编辑（vim 等）都通过文件变化触发，
// 保证只有一条生效路径：磁盘 → Load → holder.Replace → scheduler.Reload。
// Load 固定顺带读取 config 同级 base_instructions.md，因此任一被监听文件
// 变化都会刷新基线指令内容。
package configwatch

import (
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// Watcher 监听配置相关文件并热重载。
type Watcher struct {
	path       string // config.yaml 路径
	configBase string // config 文件名，用于过滤目录事件
	holder     *config.Holder
	onReload   func()                  // 热重载成功后回调（scheduler.Reload）
	onLog      func(config.LoggingCfg) // 热重载成功后回调（重新配置日志系统，可空）

	fsw      *fsnotify.Watcher
	stop     chan struct{}
	stopOnce sync.Once // Close 幂等：避免重复 close channel 触发 panic
	wg       sync.WaitGroup

	mu          sync.Mutex
	reloadTimer *time.Timer // debounce timer；Close 时 Stop，避免测试清理后仍 reload

	lastLoadErr atomic.Pointer[string]
}

// New 构造 Watcher。onReload 在每次成功重载后调用（可空）；
// onLog 在每次成功重载后调用，用于把新的 logging 配置应用到运行中的日志系统
// （否则管理页修改日志等级/格式/文件不会即时生效），可空。
//
// 监听范围：config.yaml、同级 base_instructions.md，以及配置所在目录
// （覆盖编辑器原子保存 rename，以及 md 文件稍后才创建的情况）。
func New(path string, holder *config.Holder, onReload func(), onLog func(config.LoggingCfg)) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	// fsnotify 在某些平台（尤其编辑器的 atomic save）需要监听父目录才能捕获
	// 原子重命名式写入；也覆盖 base_instructions.md 从无到有的 Create。
	if err := fsw.Add(dir); err != nil {
		slog.Warn("配置目录 fsnotify add 失败", "dir", dir, "error", err)
	}
	// 再 watch 文件本身（部分平台仅目录事件不够稳定时双保险）
	if err := fsw.Add(path); err != nil {
		slog.Warn("配置文件 fsnotify add 失败", "path", path, "error", err)
	}
	biPath := filepath.Join(dir, config.BaseInstructionsFileName)
	if err := fsw.Add(biPath); err != nil {
		// 文件可能尚不存在：依赖目录 watch 捕获后续 Create。
		slog.Debug("基线指令文件暂未监听（可能尚未创建）", "path", biPath, "error", err)
	}
	w := &Watcher{
		path:       path,
		configBase: filepath.Base(path),
		holder:     holder,
		onReload:   onReload,
		onLog:      onLog,
		fsw:        fsw,
		stop:       make(chan struct{}),
	}
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

func (w *Watcher) loop() {
	defer w.wg.Done()
	// debounce：编辑器可能触发多次事件（write+rename+chmod），合并 200ms 窗口。
	const debounce = 200 * time.Millisecond
	for {
		select {
		case <-w.stop:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Rename) {
				continue
			}
			if !w.isWatchedPath(ev.Name) {
				continue
			}
			// base_instructions.md 可能首次 Create 后才存在：补注册文件 watch。
			if filepath.Base(ev.Name) == config.BaseInstructionsFileName {
				_ = w.fsw.Add(ev.Name)
			}
			w.mu.Lock()
			if w.reloadTimer != nil {
				w.reloadTimer.Stop()
			}
			w.reloadTimer = time.AfterFunc(debounce, w.reload)
			w.mu.Unlock()
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			if err != nil {
				slog.Warn("configwatch fsnotify 错误", "error", err)
			}
		}
	}
}

// isWatchedPath 判断事件是否来自 config 或同级基线指令文件。
// 目录 watch 会收到同目录任意文件事件，必须过滤。
func (w *Watcher) isWatchedPath(name string) bool {
	if name == "" {
		return false
	}
	base := filepath.Base(name)
	return base == w.configBase || base == config.BaseInstructionsFileName
}

// reload 重新加载配置并替换 holder。失败时保留旧配置，记录错误。
func (w *Watcher) reload() {
	cfg, err := config.Load(w.path)
	if err != nil {
		s := err.Error()
		w.lastLoadErr.Store(&s)
		slog.Error("热重载配置失败，保留旧配置", "path", w.path, "error", err)
		return
	}
	s := ""
	w.lastLoadErr.Store(&s)
	w.holder.Replace(cfg)
	if w.onReload != nil {
		// 隔离回调 panic：管理侧逻辑异常不能影响 watcher goroutine
		func() {
			defer func() { _ = recover() }()
			w.onReload()
		}()
	}
	if w.onLog != nil {
		// 重新配置日志系统：管理页修改 logging.level/format/file 需即时生效。
		// 隔离 panic：日志重配置异常不能影响 watcher goroutine 与转发路径。
		func() {
			defer func() { _ = recover() }()
			w.onLog(cfg.Logging)
		}()
	}
	slog.Info("配置热重载完成",
		"path", w.path,
		"sources", len(cfg.Sources),
		"base_instructions_bytes", len(cfg.BaseInstructions))
}

// LastLoadErr 返回最近一次加载错误（nil 表示成功或无错误）。
func (w *Watcher) LastLoadErr() error {
	p := w.lastLoadErr.Load()
	if p == nil || *p == "" {
		return nil
	}
	return &errStr{*p}
}

// Reload 手动触发一次重载（admin 写回后调用）。
func (w *Watcher) Reload() { w.reload() }

// Close 停止监听。
func (w *Watcher) Close() error {
	// 幂等：shutdownHandler 与 main 的 defer 都会调用 Close。
	// close 已关闭的 channel 会 panic，用 stopOnce 保证仅真正执行一次；
	// 后续调用直接返回首次的 err（或 nil）。
	var err error
	w.stopOnce.Do(func() {
		close(w.stop)
		w.mu.Lock()
		if w.reloadTimer != nil {
			w.reloadTimer.Stop()
			w.reloadTimer = nil
		}
		w.mu.Unlock()
		err = w.fsw.Close()
		w.wg.Wait()
	})
	return err
}

type errStr struct{ s string }

func (e *errStr) Error() string { return e.s }
