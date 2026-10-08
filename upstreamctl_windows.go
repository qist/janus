//go:build windows

package main

import (
	"os"
	"syscall"
)

// signalProcess 向 pid 发送信号。Windows 没有 POSIX 信号语义：
// graceful 阶段发出即按终止处理（os.Process.Kill），
// SIGTERM/SIGKILL 都会终止进程；terminateUpstream 的等待循环
// 随后会确认进程真正退出。
func signalProcess(pid int, _ syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// processAlive 判断 pid 进程是否还活着：打开带查询权限的句柄，
// 成功即存活。权限不足也视为存活（保守，避免误判已退出）。
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == syscall.ERROR_ACCESS_DENIED
	}
	syscall.CloseHandle(h)
	return true
}
