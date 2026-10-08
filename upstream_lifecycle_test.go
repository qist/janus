package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// upstream_lifecycle_test.go —— 托管 upstream 生命周期（退出清理 + 孤儿清扫）的
// 单元测试。依赖 /proc，仅 Linux 下生效；其它平台直接跳过。

func linuxOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("需要 /proc（Linux）")
	}
}

// spawnBlockingChild 拉起一个挂着不退出（cat 从管道读、无 EOF）的子进程。
// 等 /proc/<pid>/environ 就绪后再返回：fork→exec 的窗口期里读 environ
// 可能拿到 0 字节（进程还没 exec），生产环境读的都是已运行一段时间的进程，
// 不受影响；这里显式等 exec 完成。
func spawnBlockingChild(t *testing.T, extraEnv []string) (pid int, stop func()) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "cat")
	cmd.Env = append(os.Environ(), extraEnv...)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = pr
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	_ = pr.Close() // 子进程持有读端；写端 pw 保持打开，cat 不会 EOF

	pid = cmd.Process.Pid
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); rerr == nil && len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return pid, func() {
		_ = pw.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

func TestPPIDOfCurrentProcess(t *testing.T) {
	linuxOnly(t)
	if got := ppidOf(os.Getpid()); got != os.Getppid() {
		t.Fatalf("ppidOf(self)=%d, want %d", got, os.Getppid())
	}
	if ppidOf(99999999) != -1 {
		t.Fatal("不存在的 pid 应返回 -1")
	}
}

// 托管识别：新标记 JANUS_MANAGED_UPSTREAM 与旧的 OPENCODE_CONFIG_CONTENT
// 注入标记都要能认出来；普通进程不能误判。
func TestIsManagedOpenCodeMarkers(t *testing.T) {
	linuxOnly(t)

	pid, stop := spawnBlockingChild(t, []string{managedUpstreamEnv + "=" + managedUpstreamEnvValue})
	if !isManagedOpenCode(pid) {
		t.Fatalf("带 JANUS_MANAGED_UPSTREAM 的进程应判托管 (pid=%d)", pid)
	}
	stop()

	pid2, stop2 := spawnBlockingChild(t, []string{opencodeConfigContentEnv + `={"agents":{}}`})
	if !isManagedOpenCode(pid2) {
		t.Fatalf("带 OPENCODE_CONFIG_CONTENT 的进程应判托管 (pid=%d)", pid2)
	}
	stop2()

	pid3, stop3 := spawnBlockingChild(t, nil)
	if isManagedOpenCode(pid3) {
		t.Fatalf("无标记的进程不应判托管 (pid=%d)", pid3)
	}
	stop3()
}

// terminateUpstream 应先 SIGTERM 优雅终止，进程应快速退出（僵尸也算退出）。
func TestTerminateUpstreamKillsChild(t *testing.T) {
	linuxOnly(t)
	pid, stop := spawnBlockingChild(t, nil)
	defer stop()

	start := time.Now()
	terminateUpstream(pid, NewLogger("error"))
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("terminateUpstream 应快速返回，实际 %s", elapsed)
	}
	if processAlive(pid) {
		t.Fatalf("terminateUpstream 后进程仍存活 (pid=%d)", pid)
	}
}

// stopManagedUpstream 只收「托管中」的 PID；无托管时是空操作。
func TestStopManagedUpstreamNoopWhenUnset(t *testing.T) {
	s := &Server{cfg: Config{UpstreamCleanup: true}, log: NewLogger("error")}
	s.stopManagedUpstream() // 不应 panic
	if s.managedPID.Load() != 0 {
		t.Fatal("无托管 PID 时不应有副作用")
	}
}

// 关闭清理开关后，stopManagedUpstream 不碰任何进程。
func TestStopManagedUpstreamRespectsConfig(t *testing.T) {
	linuxOnly(t)
	pid, stop := spawnBlockingChild(t, nil)
	defer stop()

	s := &Server{cfg: Config{UpstreamCleanup: false}, log: NewLogger("error")}
	s.setManagedUpstream(pid)
	s.stopManagedUpstream()
	if !processAlive(pid) {
		t.Fatal("BRIDGE_UPSTREAM_CLEANUP=false 时不应终止子进程")
	}
}
