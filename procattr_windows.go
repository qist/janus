//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess 让 janus 拉起的子进程独立于 janus 的控制台信号：
// 用户按 Ctrl+C 时不会连带杀掉上游 OpenCode。
//
// Windows 没有 setsid，用 CREATE_NEW_PROCESS_GROUP 达到同样的"脱离"效果。
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
