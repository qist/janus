package opencode

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"janus/internal/config"
	"janus/internal/discover"
	"janus/internal/logx"
	"janus/internal/procx"
	"janus/internal/upstream"
)

// lifecycle.go —— janus 托管的 OpenCode 进程生命周期。
//
// 背景：线上常见 nohup/裸进程部署（无 systemd/容器 cgroup 兜底）。旧实现
// 用 Setsid 把 opencode 放到独立会话，`kill janus` 不会带上它 —— 于是每次
// 重启都会留下一个孤儿 opencode，越积越多。
//
// 现在的责任模型（BRIDGE_UPSTREAM_CLEANUP，默认 true）：
//   - janus 拉起的 opencode 打 JANUS_MANAGED_UPSTREAM 标记；
//   - 优雅退出（SIGTERM/SIGINT）时，收掉自己持有（Spawned/Managed）的实例；
//   - kill -9/崩溃留下的孤儿（标记还在、父进程已死），下次启动拉起前清扫。

// Supervisor 管理一个 OpenCode 渠道的端点探活与进程生命周期。
type Supervisor struct {
	log  *logx.Logger
	cfg  *config.Config
	up   *upstream.Upstream
	bus  *upstream.EventBus
	bin  string // MCP 桥脚本注册时用的 opencode 可执行文件提示（toolPrefix 同源）
	pref string // 工具桥 MCP server 名前缀，用于内联配置生成

	managedPID atomic.Int64
}

// NewSupervisor 构造渠道监督器。cfg 必须是调用方长期持有的同一份配置指针
// （热替换端点时会读取其最新字段）。
func NewSupervisor(log *logx.Logger, cfg *config.Config, up *upstream.Upstream, bus *upstream.EventBus, toolPrefix string) *Supervisor {
	return &Supervisor{log: log, cfg: cfg, up: up, bus: bus, pref: toolPrefix}
}

// SetManaged 记录「当前由本进程托管的上游 opencode」PID（0=无）。
func (s *Supervisor) SetManaged(pid int) {
	if s == nil {
		return
	}
	s.managedPID.Store(int64(pid))
}

// ManagedPID 返回当前托管的 PID（0=无）。
func (s *Supervisor) ManagedPID() int {
	if s == nil {
		return 0
	}
	return int(s.managedPID.Load())
}

// StopManaged 在退出时收掉本进程托管的上游 opencode。
// 由 main 的优雅停机路径调用（SIGTERM/SIGINT）。
func (s *Supervisor) StopManaged() {
	if s == nil || s.cfg == nil {
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
	Terminate(pid, s.log)
}

// Terminate 终止一个 opencode 进程：先 SIGTERM 优雅退出，
// 数秒后仍存活再 SIGKILL 兜底。对已死的 PID 静默（ESRCH 视为成功）。
func Terminate(pid int, log *logx.Logger) {
	if pid <= 0 {
		return
	}
	if err := procx.Signal(pid, syscall.SIGTERM); err != nil {
		if log != nil {
			log.Debugf("terminate upstream %d: SIGTERM: %v", pid, err)
		}
	}
	// 最多等 8 秒优雅退出；还在就强杀。
	deadline := time.Now().Add(8 * time.Second)
	for procx.Alive(pid) {
		if time.Now().After(deadline) {
			if log != nil {
				log.Warnf("upstream opencode (pid %d) ignored SIGTERM; sending SIGKILL", pid)
			}
			_ = procx.Signal(pid, syscall.SIGKILL)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// SweepOrphans 清扫「上一轮 janus 非正常退出（kill -9/崩溃）留下的、
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
func SweepOrphans(log *logx.Logger) {
	sweepOrphans(log)
}

func sweepOrphans(log *logx.Logger) {
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
		args := discover.ProcArgs(pid)
		if len(args) == 0 || !discover.IsOpenCodeServe(args) {
			continue
		}
		if !discover.IsManagedOpenCode(pid) {
			continue
		}
		if discover.PpidOf(pid) != 1 {
			continue // 父进程还活着（正常使用的实例），别动
		}
		if log != nil {
			log.Warnf("sweeping orphaned janus-managed opencode (pid %d, ppid=1)", pid)
		}
		Terminate(pid, log)
		n++
	}
	if n > 0 && log != nil {
		log.Infof("swept %d orphaned janus-managed opencode instance(s)", n)
	}
}

// Watch 持续探活；发现连不上就尝试重新发现并热替换端点。
// 这让 bridge 能扛住 OpenCode 桌面端的重启。配置里显式给了固定
// OPENCODE_URL 且非 auto 时，只做探活不覆盖（用户可能有意指向远程实例）。
func (s *Supervisor) Watch(ctx context.Context, allowDiscover bool) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()

	failStreak := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.up.Ping(pctx)
		cancel()

		if err == nil {
			if failStreak > 0 {
				s.log.Infof("upstream recovered")
			}
			failStreak = 0
			continue
		}

		failStreak++
		if !allowDiscover {
			if failStreak == 1 || failStreak%6 == 0 {
				s.log.Warnf("upstream unreachable (%d): %v", failStreak, err)
			}
			continue
		}
		// 每 3 次失败（约 30s）尝试一次重新发现，避免刷屏
		if failStreak%3 != 0 {
			continue
		}

		dctx, dcancel := context.WithTimeout(ctx, 25*time.Second)
		ep, derr := Ensure(dctx, s.log, s.cfg, s.cfg.AutostartUpstream, s.pref)
		dcancel()

		if derr != nil {
			s.log.Warnf("upstream unreachable (%d) and rediscovery failed: %v", failStreak, derr)
			continue
		}
		oldBase, _, _ := s.up.Endpoint()
		if ep.Base == oldBase {
			continue
		}
		s.up.SetEndpoint(ep.Base, ep.User, ep.Pass)
		s.log.Infof("upstream endpoint changed: %s -> %s (pid %d)", oldBase, ep.Base, ep.PID)

		// 换到本进程拉起的实例时，记下 PID 供退出回收（只认本进程拉起的，
		// 不认领别人的：多 janus 共存时误杀会打断对端）。
		if ep.Spawned {
			s.SetManaged(ep.PID)
		}

		// 事件流跟着换端点重连
		if s.bus != nil {
			s.bus.Reconnect()
		}
	}
}
