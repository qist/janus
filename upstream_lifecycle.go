package main

import (
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// upstream_lifecycle.go —— janus 托管的 OpenCode 进程生命周期。
//
// 背景：线上常见 nohup/裸进程部署（无 systemd/容器 cgroup 兜底）。旧实现
// 用 Setsid 把 opencode 放到独立会话，`kill janus` 不会带上它 —— 于是每次
// 重启都会留下一个孤儿 opencode，越积越多。
//
// 现在的责任模型（BRIDGE_UPSTREAM_CLEANUP，默认 true）：
//   - janus 拉起的 opencode 打 JANUS_MANAGED_UPSTREAM 标记；
//   - 优雅退出（SIGTERM/SIGINT）时，收掉自己持有（Spawned/Managed）的实例；
//   - kill -9/崩溃留下的孤儿（标记还在、父进程已死），下次启动拉起前清扫。

// setManagedUpstream 记录「当前由本进程托管的上游 opencode」PID（0=无）。
func (s *Server) setManagedUpstream(pid int) {
	if s == nil {
		return
	}
	s.managedPID.Store(int64(pid))
}

// stopManagedUpstream 在退出时收掉本进程托管的上游 opencode。
// 由 main 的优雅停机路径调用（SIGTERM/SIGINT）。
func (s *Server) stopManagedUpstream() {
	if s == nil {
		return
	}
	if !s.cfg.UpstreamCleanup {
		return
	}
	pid := int(s.managedPID.Swap(0))
	if pid <= 0 {
		return
	}
	s.log.Infof("stopping janus-managed upstream opencode (pid %d)", pid)
	terminateUpstream(pid, s.log)
}

// terminateUpstream 终止一个 opencode 进程：先 SIGTERM 优雅退出，
// 数秒后仍存活再 SIGKILL 兜底。对已死的 PID 静默（ESRCH 视为成功）。
func terminateUpstream(pid int, log *Logger) {
	if pid <= 0 {
		return
	}
	if err := signalProcess(pid, syscall.SIGTERM); err != nil {
		if log != nil {
			log.Debugf("terminate upstream %d: SIGTERM: %v", pid, err)
		}
	}
	// 最多等 8 秒优雅退出；还在就强杀。
	deadline := time.Now().Add(8 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			if log != nil {
				log.Warnf("upstream opencode (pid %d) ignored SIGTERM; sending SIGKILL", pid)
			}
			_ = signalProcess(pid, syscall.SIGKILL)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// sweepManagedOrphans 清扫「上一轮 janus 非正常退出（kill -9/崩溃）留下的、
// 已成孤儿（父进程已死）的 janus 托管 opencode」，避免进程越积越多。
//
// 只在 Linux（/proc 可用）生效；判定条件三者缺一不可：
//  1. 是 `opencode serve` 进程；
//  2. 带 janus 托管标记（JANUS_MANAGED_UPSTREAM 或 OPENCODE_CONFIG_CONTENT）；
//  3. 父进程已是 1（init/reaper）——即原 owner janus 已死。
//
// 外部桌面端实例没有托管标记，不受影响；被活着自己的 janus 直接托管的实例
// PPID 不是 1，不会被误杀。（被其它 janus「复用」的孤儿 PPID 也是 1，
// 属多实例共存的边界情形——正常单 janus 部署不存在。）
func sweepManagedOrphans(log *Logger) {
	if runtime.GOOS != "linux" {
		return // /proc 方案仅 Linux；其它平台靠退出清理兜底（无孤儿积累源）
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		args := procArgs(pid)
		if len(args) == 0 || !isOpenCodeServe(args) {
			continue
		}
		if !isManagedOpenCode(pid) {
			continue
		}
		if ppidOf(pid) != 1 {
			continue // 父进程还活着（正常使用的实例），别动
		}
		if log != nil {
			log.Warnf("sweeping orphaned janus-managed opencode (pid %d, ppid=1)", pid)
		}
		terminateUpstream(pid, log)
		n++
	}
	if n > 0 && log != nil {
		log.Infof("swept %d orphaned janus-managed opencode instance(s)", n)
	}
}
