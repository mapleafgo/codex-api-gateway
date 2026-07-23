package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maybeDaemonize 实现类似 docker compose -d 的后台启动：
// 父进程 re-exec 自身（去掉 -d），脱离终端会话后等待子进程就绪；
// 子进程成功监听并写入 gateway.pid 后，父进程打印成功并退出。
//
// 必须在 flag.Parse 之后、任何会占用终端的初始化之前调用。
func maybeDaemonize(enabled bool) {
	if !enabled {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: 无法解析可执行文件: %v\n", err)
		os.Exit(1)
	}
	exe, _ = filepath.EvalSymlinks(exe)

	// go run 运行时，exe 是 /tmp/go-build*/... 临时文件，父进程退出后会被删除
	// 这种情况下跳过 daemonize，直接前台运行，留给调用者处理后台
	exePath := filepath.Dir(exe)
	if strings.Contains(exePath, "/go-build") || strings.Contains(exePath, "/tmp/go") {
		fmt.Fprintf(os.Stderr, "daemon: warning: detected go run environment, skipping daemonize\n"+
			"daemon mode only works with pre-built binary (./codex-api-gateway -d).\n"+
			"Starting in foreground...\n")
		return
	}

	args := stripDaemonFlags(os.Args[1:])
	cmd := exec.Command(exe, args...)
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	cmd.Env = append(os.Environ(), "GATEWAY_DAEMON=1")
	cmd.Stdin = nil

	logPath := resolveDaemonPath(os.Getenv("GATEWAY_LOG"), "gateway.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: 无法打开日志 %s: %v\n", logPath, err)
		os.Exit(1)
	}
	// 子进程继承 fd；父进程退出时关闭自己的副本即可
	cmd.Stdout = f
	cmd.Stderr = f
	setDaemonSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "daemon: 后台启动失败: %v\n", err)
		os.Exit(1)
	}
	_ = f.Close()

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	pidPath := resolveDaemonPath(os.Getenv("GATEWAY_PID_FILE"), "gateway.pid")
	if err := waitDaemonReady(cmd.Process.Pid, pidPath, waitDone, 15*time.Second); err != nil {
		// 尽力清理半启动子进程，避免残留占端口
		_ = cmd.Process.Kill()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
		}
		fmt.Fprintf(os.Stderr, "daemon: %v\nlog: %s\n", err, logPath)
		os.Exit(1)
	}
	// 子进程已独立会话运行；不再阻塞 Wait，后台 goroutine 会回收状态。
	// 父进程退出后子进程由 init 收养，不会成僵尸。
	// 此时 logging 尚未初始化（父进程即将退出），用 fmt 写 stdout 前台提示用户。
	fmt.Printf("codex-api-gateway 已后台启动 pid=%d log=%s\n", cmd.Process.Pid, logPath)
	os.Exit(0)
}

// resolveDaemonPath 把相对路径落到当前工作目录。
func resolveDaemonPath(v, def string) string {
	if v == "" {
		v = def
	}
	if filepath.IsAbs(v) {
		return v
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, v)
	}
	return v
}

// waitDaemonReady 等待子进程成功监听并写入 pid 文件，或确认子进程已失败退出。
// waitDone 来自 cmd.Wait()，可区分“真退出”与僵尸误判。
func waitDaemonReady(pid int, pidPath string, waitDone <-chan error, timeout time.Duration) error {
	deadline := time.After(timeout)
	want := strconv.Itoa(pid)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-waitDone:
			if err != nil {
				return fmt.Errorf("子进程启动后退出（pid=%d）: %w", pid, err)
			}
			return fmt.Errorf("子进程启动后退出（pid=%d）", pid)
		case <-deadline:
			return fmt.Errorf("等待就绪超时（pid=%d 未写入 %s）", pid, pidPath)
		case <-ticker.C:
			if b, err := os.ReadFile(pidPath); err == nil {
				if strings.TrimSpace(string(b)) == want {
					return nil
				}
			}
		}
	}
}

// stripDaemonFlags 去掉 -d / --daemon，避免子进程再次 daemonize。
func stripDaemonFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "-d", "--daemon":
			continue
		default:
			// 兼容 -d=true 这类形式
			if strings.HasPrefix(a, "-d=") || strings.HasPrefix(a, "--daemon=") {
				continue
			}
			out = append(out, a)
		}
	}
	return out
}
