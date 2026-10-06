//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess 让 janus 拉起的子进程脱离当前会话/进程组：
// janus 退出或收到信号时，上游 OpenCode 不会被一起带走。
//
// Unix（Linux/macOS）用 setsid（新会话，完全脱离控制终端）。
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
