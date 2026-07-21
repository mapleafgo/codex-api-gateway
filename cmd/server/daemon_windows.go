//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// setDaemonSysProcAttr 在 Windows 下创建新进程组，尽量脱离父控制台。
func setDaemonSysProcAttr(cmd *exec.Cmd) {
	const (
		createNewProcessGroup = 0x00000200
		detachedProcess       = 0x00000008
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
}
