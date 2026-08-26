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
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
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
	validator  config.SourceValidator // 插件级校验器；可为 nil
	onReload   ReloadCallback         // 热重载成功后回调（scheduler.Reload）
	onLog      LoggingCallback        // 热重载成功后回调（重新配置日志系统，可空）

	fsw      *fsnotify.Watcher
	stop     chan struct{}
	stopOnce sync.Once // Close 幂等：避免重复 close channel 触发 panic
	wg       sync.WaitGroup

	mu          sync.Mutex
	reloadTimer *time.Timer // debounce timer；Close 时 Stop，避免测试清理后仍 reload

	lastLoadErr     atomic.Pointer[string]
	lastContentHash atomic.Uint64 // 最近一次成功加载的 config 原始字节 hash，用于去重双触发
}

// ReloadCallback 把最新 holder 配置应用到运行时组件。
type ReloadCallback func() error

// LoggingCallback 把最新日志配置应用到进程默认 logger。
type LoggingCallback func(config.LoggingCfg) error

// New 构造 Watcher。onReload 在每次成功重载后调用（可空）；
// onLog 在每次成功重载后调用，用于把新的 logging 配置应用到运行中的日志系统
// （否则管理页修改日志等级/格式/文件不会即时生效），可空。
//
// 监听范围：config.yaml、同级 base_instructions.md，以及配置所在目录
// （覆盖编辑器原子保存 rename，以及 md 文件稍后才创建的情况）。
func New(path string, holder *config.Holder, validator config.SourceValidator, onReload ReloadCallback, onLog LoggingCallback) (*Watcher, error) {
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
		validator:  validator,
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
			// fsnotify 事件触发的时间去重：与手动 Reload() 形成双触发时，
			// 内容相同的那一次直接跳过，避免重复 Replace + scheduler.Reload。
			w.reloadTimer = time.AfterFunc(debounce, func() { w.reload(true) })
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
// dedupe 为 true 时（来自 fsnotify 事件），若文件内容与上次成功加载一致则
// 整体跳过；手动 Reload() 传入 false，始终真正加载（调用方明确要求重新生效，
// 即使文件未变化也需重放 callback，例如 callback 失败后的重试）。
func (w *Watcher) reload(dedupe bool) {
	raw, err := os.ReadFile(w.path)
	if err != nil {
		s := err.Error()
		w.lastLoadErr.Store(&s)
		slog.Error("热重载读取配置文件失败，保留旧配置", "path", w.path, "error", err)
		return
	}
	// base_instructions 也在 reload 内被 config.Load 一并读取；若它变化而
	// config 不变，同样需要刷新。把二者原始字节一起纳入 hash，才能避开
	// "base_instructions 变化被误判为相同" 的去重缺陷。
	biPath := filepath.Join(filepath.Dir(w.path), config.BaseInstructionsFileName)
	var biRaw []byte
	if _, statErr := os.Stat(biPath); statErr == nil {
		biRaw, err = os.ReadFile(biPath)
		if err != nil {
			s := err.Error()
			w.lastLoadErr.Store(&s)
			slog.Error("热重载读取基线指令文件失败，保留旧配置", "path", biPath, "error", err)
			return
		}
	}
	content := make([]byte, 0, len(raw)+len(biRaw))
	content = append(content, raw...)
	content = append(content, biRaw...)
	// 内容去重：admin 保存显式 Reload() 与 fsnotify 事件会各触发一次；
	// 若文件内容与上次成功加载一致，说明配置没有实质变化，跳过以避免
	// 重复的 holder.Replace + scheduler.Reload 造成的冗余热重载。
	h := fnvHash(content)
	if dedupe && w.lastContentHash.Load() == h {
		return
	}
	cfg, err := config.LoadWithValidator(w.path, w.validator)
	if err != nil {
		s := err.Error()
		w.lastLoadErr.Store(&s)
		slog.Error("热重载配置失败，保留旧配置", "path", w.path, "error", err)
		return
	}
	s := ""
	w.lastLoadErr.Store(&s)
	w.lastContentHash.Store(h)
	w.holder.Replace(cfg)
	var applyErrors []error
	if w.onReload != nil {
		if err := callReloadCallback(w.onReload); err != nil {
			applyErrors = append(applyErrors, fmt.Errorf("scheduler reload: %w", err))
			slog.Error("热重载应用 scheduler 配置失败", "path", w.path, "error", err)
		}
	}
	if w.onLog != nil {
		if err := callLoggingCallback(w.onLog, cfg.Logging); err != nil {
			applyErrors = append(applyErrors, fmt.Errorf("logging reload: %w", err))
			slog.Error("热重载应用日志配置失败", "path", w.path, "error", err)
		}
	}
	if err := errors.Join(applyErrors...); err != nil {
		message := err.Error()
		w.lastLoadErr.Store(&message)
		slog.Warn("配置已加载但运行时应用不完整", "path", w.path, "error", err)
		return
	}
	slog.Info("配置热重载完成",
		"path", w.path,
		"sources", len(cfg.Sources),
		"base_instructions_bytes", len(cfg.BaseInstructions))
}

// fnvHash 返回数据的 FNV-1a 64 位散列，用于配置内容去重。
func fnvHash(data []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(data)
	return h.Sum64()
}

func callReloadCallback(callback ReloadCallback) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return callback()
}

func callLoggingCallback(callback LoggingCallback, cfg config.LoggingCfg) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return callback(cfg)
}

// LastLoadErr 返回最近一次加载错误（nil 表示成功或无错误）。
func (w *Watcher) LastLoadErr() error {
	p := w.lastLoadErr.Load()
	if p == nil || *p == "" {
		return nil
	}
	return errors.New(*p)
}

// Reload 手动触发一次重载（admin 写回后调用）。
func (w *Watcher) Reload() { w.reload(false) }

// Close 停止监听。
func (w *Watcher) Close() error {
	// 幂等：shutdownHandler 与 main 的 defer 都会调用 Close。
	// close 已关闭的 channel 会 panic，用 stopOnce 保证仅真正执行一次；
	// 后续调用不再执行关闭流程，并安全返回 nil。
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
