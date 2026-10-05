package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// OpenCode 桌面端/Hub 每次重启都会换随机端口和随机密码，
// 硬编码的 OPENCODE_URL / OPENCODE_PASSWORD 撑不过一次重启。
// 这里在 Linux 上直接读 /proc 自动发现正在跑的 opencode serve 进程。
//
// 发现顺序：
//  1. 扫描 /proc/<pid>/cmdline，找 basename 为 opencode 且带 serve 的进程
//  2. 从 --port / --hostname 拼出候选地址
//  3. 从 /proc/<pid>/environ 读 OPENCODE_SERVER_PASSWORD
//  4. 逐个探活 /api/info，第一个 2xx 即采用

type Endpoint struct {
	Base string
	User string
	Pass string
	PID  int
}

// Discover 返回当前可用的 OpenCode 端点；找不到返回错误。
func Discover(ctx context.Context, probe func(ctx context.Context, base, user, pass string) error) (*Endpoint, error) {
	cands, err := scanProc()
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("no running opencode serve process found")
	}

	var lastErr error
	for _, c := range cands {
		if err := probe(ctx, c.Base, c.User, c.Pass); err != nil {
			lastErr = err
			continue
		}
		return c, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all %d candidate endpoint(s) failed probe", len(cands))
	}
	return nil, lastErr
}

func scanProc() ([]*Endpoint, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	var out []*Endpoint
	seen := map[string]bool{}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		args := procArgs(pid)
		if len(args) == 0 {
			continue
		}
		if !isOpenCodeServe(args) {
			continue
		}

		host := flagValue(args, "--hostname")
		if host == "" {
			host = flagValue(args, "--host")
		}
		port := flagValue(args, "--port")
		if port == "" {
			continue
		}
		// 监听 0.0.0.0 / :: 时本地回环一定通；具体 IP 直接用
		dialHost := host
		switch host {
		case "", "0.0.0.0", "::", "[::]":
			dialHost = "127.0.0.1"
		}
		base := fmt.Sprintf("http://%s:%s", dialHost, port)
		if seen[base] {
			continue
		}
		seen[base] = true

		pass := procEnv(pid, "OPENCODE_SERVER_PASSWORD")
		out = append(out, &Endpoint{
			Base: base,
			User: "opencode",
			Pass: pass,
			PID:  pid,
		})
	}
	return out, nil
}

func procArgs(pid int) []string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	raw := strings.Split(string(b), "\x00")
	out := raw[:0]
	for _, a := range raw {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

func procEnv(pid int, key string) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, kv := range strings.Split(string(b), "\x00") {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

func isOpenCodeServe(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if filepath.Base(args[0]) != "opencode" {
		return false
	}
	for _, a := range args[1:] {
		if a == "serve" {
			return true
		}
	}
	return false
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"=")
		}
	}
	return ""
}

// WatchUpstream 持续探活；发现连不上就尝试重新发现并热替换端点。
// 这让 bridge 能扛住 OpenCode 桌面端的重启。配置里显式给了固定
// OPENCODE_URL 且非 auto 时，只做探活不覆盖（用户可能有意指向远程实例）。
func (s *Server) WatchUpstream(ctx context.Context, allowDiscover bool) {
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
		ep, derr := EnsureUpstream(dctx, s.log, &s.cfg, s.cfg.AutostartUpstream)
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

		// 事件流跟着换端点重连
		s.bus.Reconnect()
	}
}

// probeEndpoint 用给定凭据探一次 /api/info，只做连通性判断。
func probeEndpoint(ctx context.Context, base, user, pass string) error {
	u := NewUpstream(Config{Upstream: base, Username: user, Password: pass}, NewLogger("error"))
	u.client = &http.Client{Timeout: 4 * time.Second}
	return u.do(ctx, http.MethodGet, "/api/info", nil, nil, nil)
}
