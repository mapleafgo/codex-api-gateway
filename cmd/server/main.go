// Package main starts the CodexApiGateway HTTP server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/admin"
	"github.com/mapleafgo/codex-api-gateway/internal/autostart"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/configwatch"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/server"
	"github.com/mapleafgo/codex-api-gateway/internal/tray"
)

// version 由 CI 通过 -ldflags "-X ...cmd/server.version=<tag>" 注入；
// 本地构建或未注入时为空串（startup 日志中展示）。
var version string

// pidFilePath 默认 gateway.pid（工作目录），可用 GATEWAY_PID_FILE 覆盖。
// task stop 优先读此文件定位进程，避免端口解析误杀。
func pidFilePath() string {
	if v := os.Getenv("GATEWAY_PID_FILE"); v != "" {
		return v
	}
	return "gateway.pid"
}

func writePIDFile(path string) error {
	pid := os.Getpid()
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

func removePIDFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Debug("清理 pid 文件失败", "path", path, "error", err)
	}
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	daemon := flag.Bool("d", false, "run in background (detach, like docker compose -d)")
	daemonLong := flag.Bool("daemon", false, "alias of -d")
	flag.Parse()
	maybeDaemonize(*daemon || *daemonLong)

	absConfigPath, err := filepath.Abs(*configPath)
	if err != nil {
		absConfigPath = *configPath
	}

	// 两阶段初始化：先只解析 logging 段并配置日志系统，确保后续 config.Load
	// 的日志（含 base_instructions 加载、配置加载完成等）走配置好的 handler，
	// 而不是以 Go 默认格式打到终端。
	loggingCfg := config.LoadLogging(absConfigPath)
	if err := logging.Configure(loggingCfg); err != nil {
		slog.Error("配置日志失败", "log_file", loggingCfg.File, "error", err)
		os.Exit(1)
	}

	// 系统托盘在所有处理最开始就启动：logging 配置好后立即创建并 Show，
	// 确保即使后续 config.Load / server.New / HTTP 监听卡住或失败，托盘图标
	// 也已经可见，用户随时能通过"退出"菜单终止进程，不会出现"后台运行但
	// 找不到应用"的情况。
	//
	// 初始化完成前 OpenURLFunc 返回空（"打开"菜单记 DEBUG 跳过）。config.Load
	// 完成后 main 写入 adminURL，"打开"菜单指向管理页。urlMu 保护跨 goroutine
	// 读写。
	//
	// 关闭逻辑（shutdownHandler）不放 tray.OnQuit 回调里：它含 HTTP Shutdown
	// （最长 10 秒），在 systray 事件循环线程同步执行会阻塞菜单响应，表现为
	// "点退出无响应、需多次点击才退出"。改为 main 在 <-t.Done() 后执行。
	//
	// headless 环境（无 D-Bus / DISPLAY）systray 初始化失败时，tray 包内部
	// 自动降级为信号模式，保证服务仍可在纯服务器场景运行。
	var (
		urlMu    sync.RWMutex
		adminURL string
	)
	// 开机自启 Spec：用当前可执行文件 + 绝对 config 路径，工作目录为二进制所在目录。
	// Executable 失败时不展示菜单项（托盘仍可用）。
	var autoSpec *autostart.Spec
	if exe, err := os.Executable(); err != nil {
		slog.Debug("无法解析可执行文件路径，隐藏开机自启菜单", "error", err)
	} else {
		exe, _ = filepath.EvalSymlinks(exe)
		autoSpec = &autostart.Spec{
			AppID:       "codex-api-gateway",
			DisplayName: "Codex API Gateway",
			Exec:        exe,
			Args:        []string{"-config", absConfigPath},
			WorkDir:     filepath.Dir(exe),
		}
	}

	t := tray.New(tray.Config{
		Tooltip: "codex-api-gateway",
		// -d 子进程无交互会话：systray 可能秒退，导致 main 立刻退出、pid 文件被 defer 删掉。
		ForceSignal: os.Getenv("GATEWAY_DAEMON") == "1",
		OpenURLFunc: func() string {
			urlMu.RLock()
			defer urlMu.RUnlock()
			return adminURL
		},
		Autostart: autoSpec,
	})
	go t.Run()

	// config.Load 失败会 os.Exit(1)，整个进程（含托盘 goroutine）一起退出，
	// 不会留下后台运行的残留进程。
	cfg, err := config.Load(absConfigPath)
	if err != nil {
		slog.Warn("加载配置失败，尝试生成默认配置", "config_path", absConfigPath, "error", err)
		// 打包为单文件后，首次运行可能没有 config.yaml。缺失或解析失败时
		// 自动生成最小默认配置并重试一次，保证进程可启动（管理页可用，
		// 转发请求在用户添加 source 前返回 503）。
		if werr := config.WriteDefault(absConfigPath); werr != nil {
			slog.Error("生成默认配置失败", "config_path", absConfigPath, "error", werr)
			os.Exit(1)
		}
		slog.Info("已生成默认配置", "config_path", absConfigPath)
		cfg, err = config.Load(absConfigPath)
		if err != nil {
			slog.Error("默认配置加载仍失败", "config_path", absConfigPath, "error", err)
			os.Exit(1)
		}
	}

	srv := server.New(cfg)
	defer srv.Close()

	// 配置热重载：fsnotify 监听 config.yaml 变化，自动 Load 并替换 holder；
	// scheduler.Reload 由 srv.ReloadScheduler 触发，重建运行时优先级；
	// 日志系统（logging.level/format/file）通过 applyLogging 同步重配置，
	// 使管理页修改日志配置即时生效。
	// watcher 不可用不阻断启动，管理页保存改为退化为手动 Load+Replace。
	watcher, werr := configwatch.New(absConfigPath, srv.Holder(), srv.ReloadScheduler, applyLogging)
	if werr != nil {
		slog.Warn("配置热重载不可用（fsnotify 初始化失败），管理页保存需重启生效", "error", werr)
	} else {
		defer watcher.Close()
		slog.Info("配置热重载已启用", "path", absConfigPath)
	}

	mux := srv.Mux()
	adminMount(mux, srv, absConfigPath, watcher, applyLogging)

	// HTTP server 用 *http.Server 以支持 Shutdown；由 tray/shutdown 协调关闭。
	// appCtx 在退出时 cancel，通过 BaseContext 注入每个请求：管理页 SSE、
	// /v1/responses 长流都能在 Shutdown 前收到取消，避免等满 10s 超时。
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()
	httpSrv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: mux,
		BaseContext: func(net.Listener) context.Context {
			return appCtx
		},
	}
	// 先绑定监听地址，再写 pid 文件。
	// 避免端口占用时仍写 pid/挂起托盘，导致 -d 父进程误报成功、task stop 指向僵尸进程。
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		slog.Error("监听失败", "listen", cfg.Server.Listen, "error", err)
		os.Exit(1)
	}
	// pid 文件：仅在监听成功后写入，退出时删除；task stop 靠它精准定位。
	// -d 父进程也以 pid 文件作为“已就绪”信号。
	pidPath := pidFilePath()
	if err := writePIDFile(pidPath); err != nil {
		// pid 文件是 -d 就绪信号与 task stop 定位依据，写失败则直接退出，避免假启动。
		slog.Error("写入 pid 文件失败", "path", pidPath, "error", err)
		_ = ln.Close()
		os.Exit(1)
	}
	slog.Info("已写入 pid 文件", "path", pidPath, "pid", os.Getpid())
	defer removePIDFile(pidPath)
	// shutdownCh：收到"退出"信号（托盘退出菜单或 SIGINT/SIGTERM）时关闭，
	// 由 shutdownHandler 统一触发 HTTP Shutdown + watcher.Close + srv.Close。
	shutdownCh := make(chan struct{})
	serverErrCh := make(chan error, 1)
	go func() {
		slog.Info("codex-api-gateway 开始监听", "listen", cfg.Server.Listen, "log_level", cfg.Logging.Level, "log_format", cfg.Logging.Format, "version", version)
		err := httpSrv.Serve(ln)
		// Shutdown 会使 Serve 返回 ErrServerClosed，属正常退出。
		serverErrCh <- err
		slog.Debug("退出流程：HTTP goroutine 即将等待 shutdownCh")
		<-shutdownCh
		slog.Debug("退出流程：HTTP goroutine 收到 shutdownCh，返回")
	}()

	// 初始化完成：写入 adminURL，"打开"菜单此后指向管理页。
	// 关闭逻辑由 <-t.Done() 后的兜底 select 执行，不在 tray 回调里做。
	urlMu.Lock()
	adminURL = "http://localhost" + cfg.Server.Listen + "/"
	urlMu.Unlock()

	// 阻塞直到托盘退出（tray.Quit / 信号 / tray 内部降级退出）。
	<-t.Done()
	slog.Debug("退出流程：托盘已 Done")

	// 兜底：若 HTTP server 因自身原因先退出（如端口冲突），也走一遍关闭流程。
	select {
	case <-shutdownCh:
		slog.Debug("退出流程：shutdownCh 已关闭，跳过 shutdownHandler")
	default:
		shutdownHandler(httpSrv, watcher, shutdownCh, appCancel)
	}

	// 检查 HTTP server 是否以非预期原因退出。
	t4 := time.Now()
	if err := <-serverErrCh; err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP 服务异常退出", "listen", cfg.Server.Listen, "error", err)
		os.Exit(1)
	}
	slog.Debug("退出流程：serverErrCh 接收完成", "elapsed", time.Since(t4).String())
	slog.Info("codex-api-gateway 已退出")
}

// shutdownHandler 统一执行优雅关闭：
//  1. 先 cancel appCtx，打断长连接（管理 SSE / 上游流式转发）；
//  2. 再 Shutdown HTTP server（等待在途请求，最长 2s，因长流已取消应很快返回）；
//  3. 关闭 watcher（停止 fsnotify）；
//  4. 通过 shutdownCh 通知 HTTP goroutine 可以返回。
//
// 多次调用安全（内部已由各组件的 Close/Shutdown 语义或 defer 保证幂等）。
func shutdownHandler(httpSrv *http.Server, watcher *configwatch.Watcher, shutdownCh chan struct{}, appCancel context.CancelFunc) {
	slog.Debug("退出流程：shutdownHandler 开始")
	t0 := time.Now()
	// 先 cancel 再 Shutdown：让 r.Context() 立刻 Done，SSE/流式 handler 退出，
	// 避免 Shutdown 干等 10s 超时。
	if appCancel != nil {
		appCancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Warn("HTTP Shutdown 超时或出错", "error", err)
	}
	slog.Debug("退出流程：HTTP Shutdown 完成", "elapsed", time.Since(t0).String())
	t1 := time.Now()
	if watcher != nil {
		_ = watcher.Close()
	}
	slog.Debug("退出流程：watcher.Close 完成", "elapsed", time.Since(t1).String(), "watcher_nil", watcher == nil)
	t2 := time.Now()
	select {
	case <-shutdownCh:
	default:
		close(shutdownCh)
	}
	slog.Debug("退出流程：shutdownCh 关闭完成", "elapsed", time.Since(t2).String())
}

// adminMount 挂载管理页到 mux，reload 回调统一从磁盘重载。
// watcher 为 nil 时退化为手动 Load+Replace+Reload+重配置日志。
// applyLogging 在每次成功重载后把新的 logging 配置应用到运行中的日志系统。
func adminMount(mux *http.ServeMux, srv *server.Server, cfgPath string, w *configwatch.Watcher, applyLogging func(config.LoggingCfg)) {
	reload := func() {
		if w != nil {
			w.Reload()
			return
		}
		defer func() { _ = recover() }()
		if newCfg, err := config.Load(cfgPath); err == nil {
			srv.Holder().Replace(newCfg)
			srv.ReloadScheduler()
			applyLogging(newCfg.Logging)
		}
	}
	admin.Mount(mux, admin.Deps{
		Holder:         srv.Holder(),
		Metrics:        srv.Metrics(),
		CfgPath:        cfgPath,
		ReloadFromDisk: reload,
		ModelsFetcher:  srv.Scheduler().ListUpstreamModels,
	})
}

// applyLogging 把 logging 配置应用到运行中的进程日志系统（重配置 slog handler）。
// 供热重载（configwatch 与 admin 手动保存两条路径）复用，确保管理页修改日志配置即时生效。
// 异常不向上抛出，避免影响调用方（configwatch goroutine / 管理接口）。
func applyLogging(cfg config.LoggingCfg) {
	if err := logging.Configure(cfg); err != nil {
		slog.Error("热重载应用日志配置失败，沿用旧日志配置", "log_file", cfg.File, "error", err)
	}
}
