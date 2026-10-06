package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// ---------- 上游 OpenCode 自动拉起 ----------
//
// janus 自己负责 OpenCode 的生命周期，不需要单独的 systemd/启动服务：
//
//	启动时先看有没有已在跑的 `opencode serve`（/proc 自动发现）；
//	没有就检查 opencode 是否安装——装了就以随机端口拉一个，没装就给出安装提示；
//	已在跑就直接复用（现有发现逻辑，密码从进程环境读取）。
//
// 模型授权（OAuth/token）仍由 OpenCode 自己存在它的库里，janus 不参与；
// /v1/usage 时只读读同一个库。
//
// 注意：本桥拉起的 opencode 用随机端口 + 随机密码，密码只在本次进程的
// 环境变量里，janus 自己知道并直接使用；已在跑的那个则按其进程环境里的密码连。

// findOpenCodeBinary 定位 opencode 可执行文件。
func findOpenCodeBinary(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if p, err := exec.LookPath("opencode"); err == nil && p != "" {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cands := []string{
			filepath.Join(home, ".opencode", "bin", "opencode"),
			"/usr/local/bin/opencode",
			"/usr/bin/opencode",
		}
		for _, p := range cands {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("未找到 opencode 可执行文件；请先安装：curl -fsSL https://opencode.ai/v2/install | bash")
}

// freePort 让内核分配一个空闲的回环端口。
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomPassword() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下退化为时间戳，至少不是空
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// SpawnOpenCode 以随机端口拉起一个 `opencode serve`，等它就绪后返回端点。
//
// 进程用 Setsid 独立会话启动，stdout/stderr 丢弃；但 systemd 下它仍在
// janus.service 的 cgroup 内，服务重启时会一起被回收（随后本桥会再拉起）。
func SpawnOpenCode(ctx context.Context, log *Logger, cfg *Config) (*Endpoint, error) {
	path, err := findOpenCodeBinary(cfg.OpencodeBin)
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("no free port: %w", err)
	}
	pass := randomPassword()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := exec.Command(path, "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))
	env := append(os.Environ(), "OPENCODE_SERVER_PASSWORD="+pass)
	// 自动注入 janus 需要的 agent 定义（内联配置，优先级高于全局/项目配置），
	// 不再依赖手写 ~/.config/opencode/opencode.jsonc。
	if content := opencodeInlineConfig(cfg); content != "" {
		env = append(env, "OPENCODE_CONFIG_CONTENT="+content)
	}
	cmd.Env = env
	cmd.Stdout = nil // /dev/null
	cmd.Stderr = nil
	detachProcess(cmd)

	log.Infof("starting opencode: %s serve --hostname 127.0.0.1 --port %d", filepath.Base(path), port)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start opencode: %w", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // 回收，避免僵尸

	deadline := time.Now().Add(20 * time.Second)
	for {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		perr := probeEndpoint(pctx, base, "opencode", pass)
		cancel()
		if perr == nil {
			return &Endpoint{Base: base, User: "opencode", Pass: pass, PID: pid}, nil
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("opencode (pid %d) 已启动但 %s 未就绪: %w", pid, base, perr)
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// EnsureUpstream 返回可用上游：默认优先复用已在跑的 opencode；没有且允许时自己拉一个。
// cfg.ReuseExternal=false 时跳过复用，总是自己拉一个（这样才能注入生成的 agent 配置）。
func EnsureUpstream(ctx context.Context, log *Logger, cfg *Config, allowSpawn bool) (*Endpoint, error) {
	var derr error
	if cfg.ReuseExternal {
		var ep *Endpoint
		ep, derr = Discover(ctx, probeEndpoint)
		if derr == nil {
			return ep, nil
		}
	}
	if !allowSpawn {
		if derr == nil {
			derr = fmt.Errorf("no running OpenCode and autostart disabled")
		}
		return nil, derr
	}
	if cfg.ReuseExternal {
		log.Infof("no running OpenCode found (%v); starting one…", derr)
	} else {
		log.Infof("reuse disabled (OPENCODE_REUSE_EXTERNAL=false); starting janus-managed OpenCode…")
	}
	return SpawnOpenCode(ctx, log, cfg)
}
