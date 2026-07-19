//go:build windows

package tray

import (
	"os/exec"
	"syscall"
)

const (
	// 见 Windows CreateProcess dwCreationFlags：
	//   DETACHED_PROCESS         = 0x00000008
	//   CREATE_NEW_PROCESS_GROUP = 0x00000200
	// syscall 包在 Windows 下没有为它们暴露常量，直接使用数值。
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

// detachProcess 让 rundll32 拉起的浏览器脱离网关的控制台与进程组，
// 避免网关退出时把浏览器一起带走。
func detachProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= detachedProcess | createNewProcessGroup
}
