// Package tray 封装系统托盘的启动、菜单与生命周期管理。
//
// 设计要点：
//   - Run 在独立 goroutine 中调用，通过 Show 立即显示图标，实现"在任何处理前启动托盘"。
//   - 菜单："打开"、可选勾选"开机自启"、"退出"。
//   - headless 降级：systray 在无图形桌面（无 D-Bus / DISPLAY）下初始化失败时，
//     自动转为等待 SIGINT/SIGTERM 的信号模式，保证服务可在纯服务器环境运行。
//   - logo 通过 go:embed 内嵌 assets/logo.png，零外部文件依赖。
//   - 退出流程不阻塞等待 tray.Run 返回：Quit 先关闭 Done channel 通知等待方，
//     tr.Remove（含 D-Bus 连接关闭）异步执行。因为 Remove 若在菜单 OnClick
//     的调用栈里同步执行，会关闭当前正处理该 Event 方法的 D-Bus 连接导致
//     自死锁，表现为"点退出卡住、需多次点击"。
//
// 平台注意：macOS 的 NSStatusBar 要求事件循环运行在主线程，若未来需要在 macOS
// 原生运行，应将 Run 调用移到主 goroutine；当前面向 Linux 桌面/服务器场景。
package tray

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/gogpu/systray"
	"github.com/mapleafgo/codex-api-gateway/assets"
	"github.com/mapleafgo/codex-api-gateway/internal/autostart"
)

// logoBytes 从共享 assets 包获取，托盘与管理页 favicon 共用同一份 logo。
var logoBytes = assets.Logo

// Config 描述托盘行为。
type Config struct {
	// Tooltip 托盘悬停提示，为空则用默认文案。
	Tooltip string
	// OpenURLFunc "打开"菜单点击时调用，返回要打开的 http(s) URL；
	// 返回空串或非 http(s) URL 时记 DEBUG 并跳过。为 nil 时隐藏"打开"项。
	OpenURLFunc func() string
	// Autostart 非 nil 时显示「开机自启」勾选菜单；为 OS 自启注册的真相源。
	Autostart *autostart.Spec
	// OnQuit "退出"菜单点击或收到 SIGINT/SIGTERM 时调用（同步），
	// 用于触发优雅关闭（关闭 HTTP server、释放资源）。可为 nil。
	OnQuit func()
}

// Tray 封装托盘实例与生命周期。
type Tray struct {
	cfg      Config
	mu       sync.Mutex
	tray     *systray.SystemTray
	quitOnce sync.Once
	quitCh   chan struct{}
}

// New 创建托盘（尚未启动）。
func New(cfg Config) *Tray {
	if cfg.Tooltip == "" {
		cfg.Tooltip = "codex-api-gateway"
	}
	return &Tray{cfg: cfg, quitCh: make(chan struct{})}
}

// Run 启动托盘并阻塞，直到 Quit 被调用或收到 SIGINT/SIGTERM。
// systray 初始化失败时自动降级为信号模式。应在独立 goroutine 中调用。
// 返回时 OnQuit 已执行（若有）。Done channel 在 Quit 完成时就关闭
// （早于 Run 返回），不依赖 tray.Run goroutine 是否被 Remove 唤醒。
func (t *Tray) Run() {
	// 注册信号：即使在托盘模式下，也允许 Ctrl+C 退出。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := t.runTray(sigCh); err != nil {
		slog.Warn("系统托盘初始化失败，降级为信号模式（headless）", "error", err)
		t.runSignal(sigCh)
	}
	// 兜底：确保 OnQuit 已触发。quitCh 在 Quit 内已关闭，这里的 closeDone 幂等。
	t.Quit()
	t.closeDone()
}

// runTray 启动 systray，返回 nil 表示正常退出，返回 error 表示初始化失败。
// 退出时机：菜单"退出"被点击（Quit 关闭 quitCh）、tray.Run 因 Remove 返回
// （异步清理的副作用）或收到信号。
func (t *Tray) runTray(sigCh chan os.Signal) error {
	tray := systray.New()
	tray.SetIcon(logoBytes).
		SetTooltip(t.cfg.Tooltip).
		SetMenu(t.buildMenu())
	tray.Show()

	t.mu.Lock()
	t.tray = tray
	t.mu.Unlock()

	slog.Info("系统托盘已启动", "tooltip", t.cfg.Tooltip)

	// tray.Run 阻塞 pump 事件循环，直到 Remove 被调用（由 Quit 触发）。
	runErr := make(chan error, 1)
	go func() { runErr <- tray.Run() }()

	select {
	case err := <-runErr:
		// tray.Run 自己返回（菜单"退出"触发 Remove，或平台错误）。
		return err
	case sig := <-sigCh:
		slog.Info("收到信号，准备退出", "signal", sig.String())
		return nil
	case <-t.quitCh:
		// Quit 已被调用（菜单"退出"或信号），Done 已关闭，立即返回。
		// 不等 tray.Run：Remove 异步执行，进程退出自然回收其 goroutine。
		return nil
	}
}

// runSignal 在 systray 不可用时仅监听信号。
func (t *Tray) runSignal(sigCh chan os.Signal) {
	sig := <-sigCh
	slog.Info("收到信号，准备退出", "signal", sig.String())
}

// buildMenu 按当前配置与自启状态组装菜单。
// systray 的 Checkbox 不会在点击后自动翻转 Checked，切换成功后需重建菜单。
func (t *Tray) buildMenu() *systray.Menu {
	menu := systray.NewMenu()
	if t.cfg.OpenURLFunc != nil {
		menu.Add("打开", t.onOpen)
		menu.AddSeparator()
	}
	if t.cfg.Autostart != nil {
		enabled := false
		if on, err := t.cfg.Autostart.IsEnabled(); err != nil {
			slog.Debug("查询开机自启状态失败", "error", err)
		} else {
			enabled = on
		}
		menu.AddCheckbox("开机自启", enabled, t.onAutostartToggle)
		menu.AddSeparator()
	}
	menu.Add("退出", t.onQuit)
	return menu
}

// refreshMenu 在托盘已 Show 后替换菜单（用于自启勾选刷新）。
func (t *Tray) refreshMenu() {
	t.mu.Lock()
	tr := t.tray
	t.mu.Unlock()
	if tr == nil {
		return
	}
	tr.SetMenu(t.buildMenu())
}

// onAutostartToggle 切换开机自启；失败时保持原勾选并记 WARN。
func (t *Tray) onAutostartToggle() {
	if t.cfg.Autostart == nil {
		return
	}
	on, err := t.cfg.Autostart.IsEnabled()
	if err != nil {
		slog.Warn("查询开机自启状态失败", "error", err)
		return
	}
	if on {
		if err := t.cfg.Autostart.Disable(); err != nil {
			slog.Warn("关闭开机自启失败", "error", err)
			return
		}
		slog.Info("已关闭开机自启")
	} else {
		if err := t.cfg.Autostart.Enable(); err != nil {
			slog.Warn("开启开机自启失败", "error", err)
			return
		}
		slog.Info("已开启开机自启")
	}
	t.refreshMenu()
}

// onOpen 菜单"打开"回调：打开管理页浏览器。
func (t *Tray) onOpen() {
	if t.cfg.OpenURLFunc == nil {
		return
	}
	rawURL := t.cfg.OpenURLFunc()
	if rawURL == "" {
		slog.Debug("打开菜单被点击，但 OpenURL 返回空，跳过")
		return
	}
	if err := openBrowser(rawURL); err != nil {
		slog.Warn("打开浏览器失败", "url", rawURL, "error", err)
	}
}

// onQuit 菜单"退出"回调。
func (t *Tray) onQuit() { t.Quit() }

// Quit 触发退出。可重复调用，仅首次生效：执行 OnQuit 回调并销毁托盘。
// 首次调用先关闭 Done channel（通知 main 与 runTray 立即解除阻塞），
// 再异步执行 tr.Remove。同步 Remove 会关闭正在处理菜单 OnClick 的 D-Bus
// 连接导致自死锁，异步化后 OnClick 立即返回，点击即时响应。
func (t *Tray) Quit() {
	t.quitOnce.Do(func() {
		t0 := time.Now()
		slog.Debug("tray.Quit: 开始退出流程")
		if t.cfg.OnQuit != nil {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("OnQuit 回调 panic", "recover", rec)
				}
			}()
			t1 := time.Now()
			t.cfg.OnQuit()
			slog.Debug("tray.Quit: OnQuit 回调完成", "elapsed", time.Since(t1).String())
		}
		// 先关 Done：让等待 <-t.Done() 的 main 立即继续关闭流程，
		// 让 runTray 的 select 立即走 quitCh 分支返回。
		t2 := time.Now()
		t.closeDone()
		slog.Debug("tray.Quit: closeDone 完成", "elapsed", time.Since(t2).String())
		t.mu.Lock()
		tr := t.tray
		t.mu.Unlock()
		if tr != nil {
			// 异步销毁：避免在 D-Bus 方法处理栈里同步关闭连接自死锁。
			// 进程退出时 goroutine 自然回收，D-Bus 资源随连接关闭释放。
			t3 := time.Now()
			go func() {
				tr.Remove()
				slog.Debug("tray.Quit: 异步 Remove 完成", "elapsed", time.Since(t3).String())
			}()
		}
		slog.Debug("tray.Quit: 退出流程结束", "elapsed", time.Since(t0).String())
	})
}

// closeDone 关闭 Done channel。幂等：Quit 内与 Run 末尾都会调用。
func (t *Tray) closeDone() {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.quitCh:
	default:
		close(t.quitCh)
	}
}

// Done 返回一个 channel，在托盘退出（Quit 或信号）后关闭。
// 关闭时机：Quit 执行完 OnQuit 与 Remove 后（由菜单/信号/Run 末尾触发）。
func (t *Tray) Done() <-chan struct{} { return t.quitCh }

// openBrowser 跨平台打开默认浏览器；拒绝非 http(s) 的 URL 防止命令注入。
//
// 关键实现要点（回归修复）：
//   - 让浏览器进程脱离网关的会话/进程组：在 Linux/GNOME 下，当默认浏览器
//     尚未启动时，xdg-open → gio open 拉起的 Chrome 会继承网关会话，被
//     GNOME/systemd 认为是网关子进程而无法完成启动，表现为"托盘点开无
//     反应，只有浏览器已运行时才能唤起新标签页"。detachProcess 通过平台
//     特定的 SysProcAttr（Unix/mac 下 Setsid；Windows 下 DETACHED_PROCESS
//     | CREATE_NEW_PROCESS_GROUP）解决。
//   - Linux 上若网关进程自身没有 DISPLAY/WAYLAND_DISPLAY（典型：systemd
//     --user service），浏览器冷启动同样会静默失败。此时从 user manager
//     的 show-environment 注入桌面会话变量（见 withDesktopSessionEnv）。
//     推荐用 .desktop 自启（packaging/install-autostart.sh），进程会天然
//     带着图形会话环境。
//   - 立刻在 goroutine 中 Wait 回收包装器进程（xdg-open / open / rundll32
//     都是短命脚本），避免僵尸；真正的浏览器进程已在 detach 后独立运行。
//   - stdin/stdout/stderr 显式指向 /dev/null，避免包装器把提示信息污染
//     到网关的 slog handler。
func openBrowser(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("拒绝打开非 http(s) URL: %s", rawURL)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		return fmt.Errorf("不支持的平台: %s", runtime.GOOS)
	}
	devnull, derr := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if derr == nil {
		cmd.Stdin = devnull
		cmd.Stdout = devnull
		cmd.Stderr = devnull
	}
	if env := withDesktopSessionEnv(os.Environ()); env != nil {
		cmd.Env = env
	}
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		if devnull != nil {
			_ = devnull.Close()
		}
		return err
	}
	// 异步 Wait 回收包装器进程，防止僵尸；真正的浏览器进程已脱离本会话。
	go func() {
		_ = cmd.Wait()
		if devnull != nil {
			_ = devnull.Close()
		}
	}()
	return nil
}
