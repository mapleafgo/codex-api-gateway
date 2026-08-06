// Package main starts the CodexApiGateway HTTP server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/admin"
	"github.com/mapleafgo/codex-api-gateway/internal/autostart"
	"github.com/mapleafgo/codex-api-gateway/internal/codexconfig"
	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/configwatch"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/server"
	"github.com/mapleafgo/codex-api-gateway/internal/tray"
)

// version 由构建注入：-ldflags "-X main.version=<tag>"（见 Taskfile LDFLAGS）；
// 未注入时为空串（startup 日志与管理页均不展示）。
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
	chdirHome := flag.Bool("chdir-home", false, "switch to user home directory before startup (autostart uses this)")
	flag.Parse()
	maybeDaemonize(*daemon || *daemonLong)
	if *chdirHome {
		if err := chdirUserHome(); err != nil {
			slog.Error("切换到用户目录失败", "error", err)
			os.Exit(1)
		}
	}

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
	// （最长等满 Shutdown 超时），在 systray 事件循环线程同步执行会阻塞菜单响应，表现为
	// "点退出无响应、需多次点击才退出"。改为 main 在 <-t.Done() 后执行。
	//
	// headless 环境（无 D-Bus / DISPLAY）systray 初始化失败时，tray 包内部
	// 自动降级为信号模式，保证服务仍可在纯服务器场景运行。
	var (
		urlMu    sync.RWMutex
		adminURL string
	)
	// 开机自启 Spec：用当前可执行文件 + 绝对 config 路径。
	// 工作目录取用户目录，与直接从 $HOME 启动一致，避免自启时落到二进制目录
	// 导致相对路径（gateway.log/gateway.pid 等）与直接打开不同。
	autoSpec := autostartSpec(absConfigPath)
	if autoSpec == nil {
		slog.Debug("无法解析可执行文件路径，隐藏开机自启菜单")
	}

	// Codex base URL 在 config.Load 前未知：托盘先创建，配置就绪后
	// 写入 codexBase 并刷新菜单，勾选态按真实监听地址判定。
	var (
		codexMu   sync.RWMutex
		codexBase string
	)
	codexMgr := codexconfig.New(func() string {
		codexMu.RLock()
		defer codexMu.RUnlock()
		return codexBase
	})

	t := tray.New(tray.Config{
		Tooltip: "codex-api-gateway",
		// -d 后台模式同样启用托盘：systray 异常或提前返回时 tray 包内部
		// 降级为信号模式继续运行，不会带退守护进程。
		// GATEWAY_NO_TRAY=1 可显式禁用托盘（纯服务器部署跳过图形初始化）。
		ForceSignal: os.Getenv("GATEWAY_NO_TRAY") == "1",
		OpenURLFunc: func() string {
			urlMu.RLock()
			defer urlMu.RUnlock()
			return adminURL
		},
		Autostart: autoSpec,
		Codex:     codexMgr,
	})
	go t.Run()

	// config.Load 失败会 os.Exit(1)，整个进程（含托盘 goroutine）一起退出，
	// 不会留下后台运行的残留进程。
	cfg, err := config.Load(absConfigPath)
	if err != nil {
		// 仅文件缺失时生成默认配置（打包为单文件后首次运行的场景）。
		// 解析/校验失败必须保留原文件退出：坏文件里仍是用户的全部
		// sources 配置，覆盖等于毁档；这与热重载"Load 失败保留旧配置"一致。
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Error("加载配置失败，保留原文件退出（修复 config.yaml 后重启）", "config_path", absConfigPath, "error", err)
			os.Exit(1)
		}
		slog.Warn("config.yaml 不存在，生成默认配置", "config_path", absConfigPath)
		if werr := config.WriteDefault(absConfigPath); werr != nil {
			slog.Error("生成默认配置失败", "config_path", absConfigPath, "error", werr)
			os.Exit(1)
		}
		cfg, err = config.Load(absConfigPath)
		if err != nil {
			slog.Error("默认配置加载仍失败", "config_path", absConfigPath, "error", err)
			os.Exit(1)
		}
	}

	srv := server.New(cfg)
	defer srv.Close()
	// 后台恢复线程：保证无请求流量时，degrade_interval 已超时的降级源也能
	// 恢复到原始优先级位置（状态保持 degraded，等真实成功或累计机会失败熔断）。
	srv.Scheduler().StartRecovery()
	defer srv.Scheduler().StopRecovery()

	// 配置热重载：fsnotify 监听 config.yaml 与同级 base_instructions.md，自动 Load 并替换 holder；
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
	// /v1/responses 长流都能在 Shutdown 前收到取消，避免等满 Shutdown 超时。
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()
	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeout),
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
	adminURL = adminURLFromListen(cfg.Server.Listen)
	urlMu.Unlock()
	codexMu.Lock()
	codexBase = codexBaseURL(cfg.Server.Listen)
	codexMu.Unlock()
	t.RefreshMenu()

	// 阻塞直到托盘退出（tray.Quit / 信号 / tray 内部降级退出），
	// 或 HTTP server 运行期自行退出（listener 被外部关闭等）——后者若只等
	// 托盘，进程会带着死掉的 HTTP server 静默挂着，错误也无人上报。
	var serveErr error
	serveErrReceived := false
	select {
	case <-t.Done():
		slog.Debug("退出流程：托盘已 Done")
	case serveErr = <-serverErrCh:
		serveErrReceived = true
		slog.Warn("HTTP 服务先于托盘退出，触发关闭流程", "error", serveErr)
		t.Quit()
		<-t.Done()
	}

	// 走一遍统一关闭流程（幂等）。
	select {
	case <-shutdownCh:
		slog.Debug("退出流程：shutdownCh 已关闭，跳过 shutdownHandler")
	default:
		shutdownHandler(httpSrv, watcher, shutdownCh, appCancel)
	}

	// 检查 HTTP server 是否以非预期原因退出。
	t4 := time.Now()
	if !serveErrReceived {
		serveErr = <-serverErrCh
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		slog.Error("HTTP 服务异常退出", "listen", cfg.Server.Listen, "error", serveErr)
		os.Exit(1)
	}
	slog.Debug("退出流程：serverErrCh 接收完成", "elapsed", time.Since(t4).String())
	slog.Info("codex-api-gateway 已退出")
}

// adminURLFromListen 把 server.listen 转成浏览器可打开的管理页 URL。
// listen 可以是 ":8383" 也可以带 host（"127.0.0.1:8383"）；直接字符串拼接
// 只对无 host 形态成立。host 为空或通配地址时用 localhost。
func adminURLFromListen(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://localhost" + listen + "/"
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// codexBaseURL 把 server.listen 转成 Codex provider 的 base_url（含 /v1）。
func codexBaseURL(listen string) string {
	return strings.TrimSuffix(adminURLFromListen(listen), "/") + "/v1"
}

// autostartSpec 用当前可执行文件与 config 路径构建自启 Spec。
// WorkDir 取用户目录，使自启与直接从 $HOME 启动的工作目录一致。
// os.Executable 失败时返回 nil（托盘菜单隐藏该项）。
func autostartSpec(configPath string) *autostart.Spec {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	exe, _ = filepath.EvalSymlinks(exe)
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		absConfig = configPath
	}
	workDir, err := os.UserHomeDir()
	if err != nil {
		workDir = filepath.Dir(absConfig)
	}
	return &autostart.Spec{
		AppID:       "codex-api-gateway",
		DisplayName: "Codex API Gateway",
		Exec:        exe,
		Args:        []string{"-config", absConfig, "-chdir-home"},
		WorkDir:     workDir,
	}
}

// chdirUserHome 把进程工作目录切到用户目录，供自启路径使用。
func chdirUserHome() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return os.Chdir(home)
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
	// 避免 Shutdown 干等超时。
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
	// 关闭日志 writer，确保退出前缓冲队列中的日志落盘。
	if err := logging.Close(); err != nil {
		slog.Warn("关闭日志 writer 失败", "error", err)
	}
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
func adminMount(mux *http.ServeMux, srv *server.Server, cfgPath string, w *configwatch.Watcher, applyLogging func(config.LoggingCfg) error) {
	reload := func() {
		if w != nil {
			w.Reload()
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				// 降级 reload 的 panic 不能静默吞：管理页保存看似成功
				// 实际未生效，必须留下可观测痕迹。
				slog.Error("管理页保存后重载 panic", "recover", rec)
			}
		}()
		if newCfg, err := config.Load(cfgPath); err == nil {
			srv.Holder().Replace(newCfg)
			if err := srv.ReloadScheduler(); err != nil {
				slog.Error("管理页保存后应用 scheduler 配置失败", "error", err)
			}
			if err := applyLogging(newCfg.Logging); err != nil {
				slog.Error("管理页保存后应用日志配置失败", "error", err)
			}
		}
	}
	admin.Mount(mux, admin.Deps{
		Holder:         srv.Holder(),
		Metrics:        srv.Metrics(),
		CfgPath:        cfgPath,
		Version:        version,
		ReloadFromDisk: reload,
		ModelsFetcher:  srv.Scheduler().ListUpstreamModels,
		SourceHealth: func() []admin.SourceHealthView {
			hs := srv.Scheduler().SourceHealth()
			out := make([]admin.SourceHealthView, 0, len(hs))
			for _, h := range hs {
				out = append(out, admin.SourceHealthView{
					Name: h.Name, State: h.State,
					DegradeCount: h.DegradeCount, Priority: h.Priority,
					Disabled: h.Disabled,
				})
			}
			return out
		},
		PromoteSource: srv.Scheduler().PromoteSource,
	})
}

// applyLogging 把 logging 配置应用到运行中的进程日志系统（重配置 slog handler）。
// 供热重载（configwatch 与 admin 手动保存两条路径）复用，确保管理页修改日志配置即时生效。
// 失败时返回错误，由 configwatch 或管理接口记录对应应用阶段。
func applyLogging(cfg config.LoggingCfg) error {
	if err := logging.Configure(cfg); err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	return nil
}
