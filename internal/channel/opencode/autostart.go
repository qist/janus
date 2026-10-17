package opencode

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
	"strings"
	"time"

	"janus/internal/config"
	"janus/internal/discover"
	"janus/internal/logx"
	"janus/internal/procx"
	"janus/internal/upstream"
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

// FindBinary 定位 opencode 可执行文件。
func FindBinary(override string) (string, error) {
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

// Env 组装拉起 OpenCode 用的子进程环境。
//
// 注意：janus.env 是当**配置文件**读的（JANUS_CONFIG），内容只在 janus 进程内
// 解析成 Config，不会进 os.Environ()。因此凡是需要让子进程看到的项，都必须在这里
// 显式追加——否则子进程只拿到 systemd 给的那几个变量。
func Env(cfg *config.Config, serverPassword, toolPrefix string) []string {
	// 先剔除继承来的同名项：这几个键由本函数权威决定。同名两份时子进程取值
	// 未定义（Go 的 exec 保留最后一个，但不该依赖），显式去重更稳。
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "OPENCODE_SERVER_PASSWORD="),
			strings.HasPrefix(kv, discover.ManagedEnv+"="),
			strings.HasPrefix(kv, discover.OpencodeConfigContentEnv+"="),
			strings.HasPrefix(kv, "OPENCODE_CLIENT="):
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "OPENCODE_SERVER_PASSWORD="+serverPassword)
	// 托管标记：退出清理、孤儿识别都靠它（详见 internal/discover）。
	env = append(env, discover.ManagedEnv+"="+discover.ManagedEnvValue)
	// 自动注入 janus 需要的 agent 定义（内联配置，优先级高于全局/项目配置），
	// 不再依赖手写 ~/.config/opencode/opencode.jsonc。
	if content := InlineConfig(cfg, toolPrefix); content != "" {
		env = append(env, discover.OpencodeConfigContentEnv+"="+content)
	}
	// 客户端身份：OpenCode 用它填上游请求头 x-opencode-client 与 User-Agent
	// 尾段（opencode/latest/<ver>/<client>）。空 = 交给 OpenCode 自己的默认（cli）。
	if v := strings.TrimSpace(cfg.OpencodeClient); v != "" {
		env = append(env, "OPENCODE_CLIENT="+v)
	}
	return env
}

// Spawn 以随机端口拉起一个 `opencode serve`，等它就绪后返回端点。
//
// 进程用 Setsid 独立会话启动，stdout/stderr 丢弃；但 systemd 下它仍在
// janus.service 的 cgroup 内，服务重启时会一起被回收（随后本桥会再拉起）。
// 本桥拉起的实例带 JANUS_MANAGED_UPSTREAM 标记，退出时由 janus 负责收掉。
func Spawn(ctx context.Context, log *logx.Logger, cfg *config.Config, toolPrefix string) (*discover.Endpoint, error) {
	path, err := FindBinary(cfg.OpencodeBin)
	if err != nil {
		return nil, err
	}

	// 先清扫上一轮 kill -9/崩溃留下的孤儿（免积压），再拉新的。
	if cfg.UpstreamCleanup {
		sweepOrphans(log)
	}

	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("no free port: %w", err)
	}
	pass := randomPassword()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := exec.Command(path, "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))
	cmd.Env = Env(cfg, pass, toolPrefix)
	cmd.Stdout = nil // /dev/null
	cmd.Stderr = nil
	procx.Detach(cmd)

	log.Infof("starting opencode: %s serve --hostname 127.0.0.1 --port %d", filepath.Base(path), port)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start opencode: %w", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // 回收，避免僵尸

	deadline := time.Now().Add(20 * time.Second)
	for {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		perr := upstream.ProbeEndpoint(pctx, base, "opencode", pass)
		cancel()
		if perr == nil {
			return &discover.Endpoint{Base: base, User: "opencode", Pass: pass, PID: pid, Spawned: true, Managed: true}, nil
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

// Ensure 返回可用上游：默认优先复用已在跑的 opencode；没有且允许时自己拉一个。
// cfg.ReuseExternal=false 时跳过复用，总是自己拉一个（这样才能注入生成的 agent 配置）。
func Ensure(ctx context.Context, log *logx.Logger, cfg *config.Config, allowSpawn bool, toolPrefix string) (*discover.Endpoint, error) {
	var derr error
	if cfg.ReuseExternal {
		var ep *discover.Endpoint
		ep, derr = discover.Discover(ctx, upstream.ProbeEndpoint)
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
	return Spawn(ctx, log, cfg, toolPrefix)
}
