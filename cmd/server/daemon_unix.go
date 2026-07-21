//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setDaemonSysProcAttr 让子进程创建新会话，脱离控制终端。
func setDaemonSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
