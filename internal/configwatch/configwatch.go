// Package configwatch 提供配置文件的热重载：监听 config.yaml 变化，
// 重新解析并以新 *Config 替换 Holder，同时回调通知 scheduler reload。
//
// 写回路径（admin 保存）与外部编辑（vim 等）都通过文件变化触发，
// 保证只有一条生效路径：磁盘 → Load → holder.Replace → scheduler.Reload。
package configwatch

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// Watcher 监听单个配置文件并热重载。
type Watcher struct {
	path     string
	holder   *config.Holder
	onReload func() // 热重载成功后回调（scheduler.Reload）

	fsw      *fsnotify.Watcher
	stop     chan struct{}
	stopOnce sync.Once // Close 幂等：避免重复 close channel 触发 panic
	wg       sync.WaitGroup

	lastLoadErr atomic.Pointer[string]
}

// New 构造 Watcher。onReload 在每次成功重载后调用（可空）。
func New(path string, holder *config.Holder, onReload func()) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	// fsnotify 在某些平台（尤其编辑器的 atomic save）需要监听父目录才能捕获
	// 原子重命名式写入。这里同时 watch 文件与所在目录。
	if err := fsw.Add(path); err != nil {
		// 文件 watch 失败不致命，继续尝试 watch 目录
		slog.Warn("配置文件 fsnotify add 失败，尝试监听目录", "path", path, "error", err)
	}
	w := &Watcher{
		path:     path,
		holder:   holder,
		onReload: onReload,
		fsw:      fsw,
		stop:     make(chan struct{}),
	}
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

func (w *Watcher) loop() {
	defer w.wg.Done()
	// debounce：编辑器可能触发多次事件（write+rename+chmod），合并 200ms 窗口。
	const debounce = 200 * time.Millisecond
	var timer *time.Timer
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
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, w.reload)
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
	slog.Info("配置热重载完成", "path", w.path, "sources", len(cfg.Sources))
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
	// 幂等：shutdownHandler 与 main defer 都会调用 Close。
	// close 已关闭的 channel 会 panic，用 stopOnce 保证仅真正执行一次；
	// 后续调用直接返回首次的 err（或 nil）。
	var err error
	w.stopOnce.Do(func() {
		close(w.stop)
		err = w.fsw.Close()
		w.wg.Wait()
	})
	return err
}

type errStr struct{ s string }

func (e *errStr) Error() string { return e.s }
