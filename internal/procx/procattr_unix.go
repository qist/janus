//go:build !windows

// Package procx 提供跨平台的子进程控制原语（脱离、发信号、探活）。
package procx

import (
	"os/exec"
	"syscall"
)

// Detach 让 janus 拉起的子进程脱离当前会话/进程组：
// janus 退出或收到信号时，上游 OpenCode 不会被一起带走。
//
// Unix（Linux/macOS）用 setsid（新会话，完全脱离控制终端）。
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
