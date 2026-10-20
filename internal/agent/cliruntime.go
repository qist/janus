package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cliruntime.go —— CLI 型 harness 的通用 AgentRuntime 实现。
//
// 一轮 Prompt = 起一个一次性子进程（exec / -p 模式），把它的 JSONL 事件流
// 翻译成 agent.Event。多轮会话靠 harness 侧会话 id（codex 的 thread_id /
// claude 的 session_id）+ resume 子命令实现 —— 进程本身无状态。
//
// harness 特有的部分（argv、env、事件解析）由 CLISpec 注入，放在各自的
// channel 包里；这里只做通用的生命周期与事件管线。
//
// 与 opencode 型 runtime 的差别：opencode 是一个常驻 server（janus 通过 HTTP
// 驱动、事件走 EventBus 广播）；CLI 型是「一 Prompt 一进程」，事件直接从
// 子进程 stdout 来。

// SessionConfig 是 CLI harness 跑一轮所需的配置。
type SessionConfig struct {
	Key            string // janus 侧逻辑会话键
	Directory      string // 子进程工作目录（harness 侧项目路径）
	Model          string // 模型名（harness 视角）
	GatewayBaseURL string // janus 基址，CLI 拿它当模型 provider
	GatewayAPIKey  string // 给 CLI 用的 agent key
	MCPURL         string // janus 的 MCP 端点（工具桥；空=不注册工具）
	MCPName        string // 注册到 harness 的 MCP server 名
}

// WantsMCP 表示是否要把 janus 的 MCP server 注册给子进程。
func (c SessionConfig) WantsMCP() bool {
	return strings.TrimSpace(c.MCPURL) != "" && strings.TrimSpace(c.MCPName) != ""
}

// CLISpec 描述一个 CLI harness 怎么被驱动。
type CLISpec struct {
	Name string // runtime 名（codex / claude-code）
	Bin  string // 可执行文件名或绝对路径

	// ArgsNew 构造「新会话第一轮」的 argv（不含 bin 本身）。
	ArgsNew func(cfg SessionConfig, prompt string) []string
	// ArgsResume 构造「续接已有会话」的 argv；harnessID 是 harness 侧会话 id。
	// 为 nil 时退化为 ArgsNew（即每轮都开新会话）。
	ArgsResume func(cfg SessionConfig, harnessID, prompt string) []string
	// Env 返回额外环境变量（"K=V"），可为 nil。
	Env func(cfg SessionConfig) []string
	// Parse 把一行 stdout 翻译成若干事件；harnessID 非空表示本轮学到了
	// harness 侧会话 id（供下一轮 resume）。
	Parse func(line []byte) (events []Event, harnessID string)
}

// CLIRuntime 是 CLISpec 的通用实现，满足 AgentRuntime。
type CLIRuntime struct {
	spec CLISpec

	mu       sync.Mutex
	sessions map[string]*cliSession
	running  map[string]*LineProc
}

type cliSession struct {
	cfg       SessionConfig
	harnessID string // harness 侧会话 id，首轮之后才会有
}

// NewCLIRuntime 用 spec 建一个 CLI runtime。
func NewCLIRuntime(spec CLISpec) *CLIRuntime {
	if spec.ArgsResume == nil {
		newArgs := spec.ArgsNew
		spec.ArgsResume = func(cfg SessionConfig, _, prompt string) []string {
			return newArgs(cfg, prompt)
		}
	}
	return &CLIRuntime{
		spec:     spec,
		sessions: map[string]*cliSession{},
		running:  map[string]*LineProc{},
	}
}

func (r *CLIRuntime) Name() string { return r.spec.Name }

// EnsureReady 检查可执行文件在 PATH 里。
func (r *CLIRuntime) EnsureReady(context.Context) error {
	bin := strings.TrimSpace(r.spec.Bin)
	if bin == "" {
		return errors.New("cli: binary is not configured")
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("cli: %s not found in PATH: %w", bin, err)
	}
	return nil
}

// CreateSession 只登记会话配置；子进程在第一轮 Prompt 时才起。
func (r *CLIRuntime) CreateSession(_ context.Context, spec SessionSpec) (string, error) {
	id := strings.TrimSpace(spec.Key)
	if id == "" {
		id = r.spec.Name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	name := strings.TrimSpace(spec.MCPName)
	if name == "" {
		name = "janus"
	}
	cfg := SessionConfig{
		Key:            id,
		Directory:      spec.Directory,
		Model:          spec.Model,
		GatewayBaseURL: spec.GatewayBaseURL,
		GatewayAPIKey:  spec.GatewayAPIKey,
		MCPURL:         spec.MCPURL,
		MCPName:        name,
	}
	r.mu.Lock()
	if s := r.sessions[id]; s != nil {
		// 复用同一会话键 = 续接：保留已学到的 harness 侧会话 id，只刷新配置。
		s.cfg = cfg
	} else {
		r.sessions[id] = &cliSession{cfg: cfg}
	}
	r.mu.Unlock()
	return id, nil
}

// Prompt 起一个子进程跑这一轮，返回事件流。
//
// req.Tools 被忽略：CLI harness 的工具经 janus 的 MCP 端点下发（哪里声明 tools
// 由调用方决定），不走 argv。
func (r *CLIRuntime) Prompt(ctx context.Context, sid string, req PromptReq) (<-chan Event, error) {
	r.mu.Lock()
	s := r.sessions[sid]
	r.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("cli: unknown session %q", sid)
	}
	cfg := s.cfg
	if m := strings.TrimSpace(req.Model); m != "" {
		cfg.Model = m
	}
	prompt := LastUserText(req.Messages)
	if prompt == "" {
		prompt = strings.TrimSpace(req.System)
	}
	if prompt == "" {
		return nil, errors.New("cli: empty prompt")
	}

	var args []string
	if s.harnessID != "" {
		args = r.spec.ArgsResume(cfg, s.harnessID, prompt)
	} else {
		args = r.spec.ArgsNew(cfg, prompt)
	}
	var env []string
	if r.spec.Env != nil {
		env = r.spec.Env(cfg)
	}

	proc, err := StartLineProc(ctx, ProcSpec{
		Bin:  r.spec.Bin,
		Args: args,
		Dir:  cfg.Directory,
		Env:  env,
	})
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.running[sid] = proc
	r.mu.Unlock()

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		defer r.clearRunning(sid, proc)

		var harnessID string
		for line := range proc.Lines() {
			evs, id := r.spec.Parse(line)
			if id != "" {
				harnessID = id
			}
			for _, e := range evs {
				if !sendEvent(ctx, out, e) {
					go drain(proc) // 消费方不读了：排空 stdout 让扫描协程能退出
					return
				}
			}
		}
		// 收尾。被打断时不等进程收尸：孙进程可能占着 stderr 管道，cmd.Wait
		// 会被 WaitDelay 拖住，而打断场景本来也不关心退出码。
		if harnessID != "" {
			r.setHarnessID(sid, harnessID)
		}
		finish := "stop"
		switch {
		case ctx.Err() != nil, proc.Killed():
			finish = "interrupted"
		default:
			if werr := proc.Wait(); werr != nil {
				msg := strings.TrimSpace(proc.Stderr())
				if msg == "" {
					msg = werr.Error()
				}
				sendEvent(ctx, out, Event{Type: EventError, Error: msg})
				finish = "error"
			}
		}
		sendEvent(ctx, out, Event{Type: EventDone, Finish: finish})
	}()
	return out, nil
}

// SetModel 只影响后续轮次（进程一轮一换，无持久状态）。
func (r *CLIRuntime) SetModel(_ context.Context, sid, model string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sid]
	if s == nil {
		return fmt.Errorf("cli: unknown session %q", sid)
	}
	s.cfg.Model = strings.TrimSpace(model)
	return nil
}

// Interrupt 杀掉该会话正在跑的子进程。
func (r *CLIRuntime) Interrupt(_ context.Context, sid string) error {
	r.mu.Lock()
	proc := r.running[sid]
	r.mu.Unlock()
	if proc != nil {
		proc.Kill()
	}
	return nil
}

// Close 杀掉所有在跑的子进程。
func (r *CLIRuntime) Close(context.Context) error {
	r.mu.Lock()
	procs := make([]*LineProc, 0, len(r.running))
	for _, p := range r.running {
		procs = append(procs, p)
	}
	r.running = map[string]*LineProc{}
	r.mu.Unlock()
	for _, p := range procs {
		p.Kill()
	}
	return nil
}

// HarnessID 返回该会话已知的 harness 侧会话 id（测试/诊断用）。
func (r *CLIRuntime) HarnessID(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[sid]; s != nil {
		return s.harnessID
	}
	return ""
}

func (r *CLIRuntime) setHarnessID(sid, id string) {
	r.mu.Lock()
	if s := r.sessions[sid]; s != nil {
		s.harnessID = id
	}
	r.mu.Unlock()
}

func (r *CLIRuntime) clearRunning(sid string, proc *LineProc) {
	r.mu.Lock()
	if r.running[sid] == proc {
		delete(r.running, sid)
	}
	r.mu.Unlock()
}

// sendEvent 送一个事件；ctx 已取消时返回 false。
func sendEvent(ctx context.Context, out chan<- Event, e Event) bool {
	select {
	case out <- e:
		return true
	default:
	}
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

// drain 排空一个 LineProc 的输出（消费方提前退出时用）。
func drain(p *LineProc) {
	for range p.Lines() {
	}
}

// LastUserText 取历史里最后一条 user 消息的内容；没有则退到末条。
func LastUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1].Content
	}
	return ""
}
