//go:build !windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
)

// signalProcess 向 pid 发送信号（Unix：kill(2)）。
func signalProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// processAlive 判断 pid 进程是否还活着（kill(pid, 0) 探测）。
// 无权限（EPERM）也视为活着；已不存在（ESRCH）才视为已退出。
// Linux 上僵尸进程（state=Z）虽仍能被 kill(pid,0) 命中，但对
// terminateUpstream 来说已算退出，因此单独排除。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil && err != syscall.EPERM {
		return false
	}
	if runtime.GOOS == "linux" {
		b, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if rerr == nil {
			s := string(b)
			if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
				// 格式：pid (comm) state ppid ...；state 在最后一个 ')' 后第二个字段开头。
				if s[i+2] == 'Z' {
					return false // 僵尸
				}
			}
		}
	}
	return true
}
