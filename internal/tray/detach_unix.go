//go:build unix

package tray

import (
	"os/exec"
	"syscall"
)

// detachProcess 让子进程与网关脱离会话与进程组。
//
// 背景：不做 detach 时，网关是通过 systemd user scope 启动的会话进程，
// xdg-open 拉起的浏览器（尤其是尚未运行的 Chrome/Chromium）会被 GNOME/
// systemd 认为是本 scope 的临时子进程，被绑定到网关的生命周期与 stdio
// 上，导致新窗口打不开、只有浏览器已在运行时能通过 D-Bus 转发到既有实例
// 才成功打开新标签页。Setsid 创建新会话，浏览器脱离网关的 controlling
// terminal 与进程组，即可独立启动。
func detachProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
