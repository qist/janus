// Package discover 在 Linux 上通过 /proc 自动发现正在运行的 OpenCode serve 进程。
//
// OpenCode 桌面端/Hub 每次重启都会换随机端口和随机密码，
// 硬编码的 OPENCODE_URL / OPENCODE_PASSWORD 撑不过一次重启。
//
// 发现顺序：
//  1. 扫描 /proc/<pid>/cmdline，找 basename 为 opencode 且带 serve 的进程
//  2. 从 --port / --hostname 拼出候选地址
//  3. 从 /proc/<pid>/environ 读 OPENCODE_SERVER_PASSWORD
//  4. 逐个探活 /api/info，第一个 2xx 即采用
package discover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Endpoint struct {
	Base string
	User string
	Pass string
	PID  int

	// Spawned 表示该端点由「本次进程」拉起（EnsureUpstream 的拉起分支）。
	// 退出时由 janus 负责收掉。
	Spawned bool

	// Managed 表示该 opencode 是 janus 托管的（进程环境带托管标记，
	// 包括上一次 janus 非正常退出留下的孤儿）。复用这样的实例后，
	// 当前 janus 也在退出时负责收掉，避免孤儿长期残留。
	Managed bool
}

// ManagedEnv 是 janus 拉起的 opencode 进程环境里的托管标记。
// 用于：① 退出清理判定；② 开局清扫 kill -9/崩溃留下的孤儿（按 PPID 判定）。
const ManagedEnv = "JANUS_MANAGED_UPSTREAM"

const ManagedEnvValue = "1"

// OpencodeConfigContentEnv 是注入 OpenCode 内联配置的环境变量名。
// 同时也是「janus 托管 opencode」的识别标记之一（见 IsManagedOpenCode）：
// 旧版本拉起的实例没有 ManagedEnv，但都会带上这个 env。
const OpencodeConfigContentEnv = "OPENCODE_CONFIG_CONTENT"

// IsManagedOpenCode 判断 pid 是否是一个「janus 托管」的 opencode：
// 有新标记 ManagedEnv，或带了 janus 注入的内联 agent 配置
// OpencodeConfigContentEnv（兼容旧版本拉起的实例）。
func IsManagedOpenCode(pid int) bool {
	if procEnv(pid, ManagedEnv) != "" {
		return true
	}
	return procEnv(pid, OpencodeConfigContentEnv) != ""
}

// PpidOf 读取 /proc/<pid>/stat 的父进程号。非 Linux（无 /proc）返回 -1。
func PpidOf(pid int) int {
	if runtime.GOOS != "linux" {
		return -1
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	s := string(b)
	// 格式：pid (comm) state ppid ...；comm 可能带空格/括号，从最后一个 ')' 后面解析。
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return -1
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 2 {
		return -1
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return -1
	}
	return ppid
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

		args := ProcArgs(pid)
		if len(args) == 0 {
			continue
		}
		if !IsOpenCodeServe(args) {
			continue
		}

		host := FlagValue(args, "--hostname")
		if host == "" {
			host = FlagValue(args, "--host")
		}
		port := FlagValue(args, "--port")
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
			Base:    base,
			User:    "opencode",
			Pass:    pass,
			PID:     pid,
			Managed: IsManagedOpenCode(pid),
		})
	}
	return out, nil
}

// ProcArgs 读取 /proc/<pid>/cmdline 并拆成去空后的 argv。
func ProcArgs(pid int) []string {
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

// procEnv 读取 /proc/<pid>/environ 里某个环境变量的值。
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

// IsOpenCodeServe 判断 argv 是否是一个 `opencode ... serve` 进程。
func IsOpenCodeServe(args []string) bool {
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

// FlagValue 从 argv 里取 `--name value` 或 `--name=value` 形式的值。
func FlagValue(args []string, name string) string {
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
