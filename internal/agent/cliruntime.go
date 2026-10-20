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
//
// 事件为什么不是「Prompt 返回的通道」独占：
//
//	工具调用回合里，子进程会停在 MCP tools/call 上等客户端回填结果；本轮 HTTP
//	请求必须先带着 tool_calls 返回，等下一次请求再把结果喂回去。若事件通道随
//	第一次请求结束关闭，续流时的增量就全丢了。因此每个会话有一个 eventHub：
//	子进程输出始终往 hub 广播，订阅者（每次 HTTP 请求一个）来去自由。

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
	harnessID string    // harness 侧会话 id，首轮之后才会有
	hub       *eventHub // 本会话的事件广播；进程结束即关闭
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

// Subscribe 订阅当前会话的事件流（可先于 Prompt 调用）。
//
// 返回的 cancel 只取消本次订阅，不影响子进程；进程结束或 hub 关闭时通道
// 自动关闭并排空已产生的事件。会话不存在时返回一个已关闭的通道。
func (r *CLIRuntime) Subscribe(sid string) (<-chan Event, func()) {
	r.mu.Lock()
	s := r.sessions[sid]
	if s == nil {
		r.mu.Unlock()
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	if s.hub == nil || s.hub.isClosed() {
		s.hub = newEventHub()
	}
	hub := s.hub
	r.mu.Unlock()
	return hub.subscribe()
}

// Start 启动一轮子进程，事件通过会话 hub 广播（调用前应已 Subscribe）。
func (r *CLIRuntime) Start(ctx context.Context, sid string, req PromptReq) error {
	r.mu.Lock()
	s := r.sessions[sid]
	if s == nil {
		r.mu.Unlock()
		return fmt.Errorf("cli: unknown session %q", sid)
	}
	if r.running[sid] != nil {
		r.mu.Unlock()
		return fmt.Errorf("cli: session %q already has a running turn", sid)
	}
	if s.hub == nil || s.hub.isClosed() {
		s.hub = newEventHub()
	}
	hub := s.hub
	cfg := s.cfg
	harnessID := s.harnessID
	r.mu.Unlock()

	if m := strings.TrimSpace(req.Model); m != "" {
		cfg.Model = m
	}
	prompt := LastUserText(req.Messages)
	if prompt == "" {
		prompt = strings.TrimSpace(req.System)
	}
	if prompt == "" {
		return errors.New("cli: empty prompt")
	}

	var args []string
	if harnessID != "" {
		args = r.spec.ArgsResume(cfg, harnessID, prompt)
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
		return err
	}
	r.mu.Lock()
	r.running[sid] = proc
	r.mu.Unlock()

	go r.pump(ctx, sid, proc, hub)
	return nil
}

// Prompt 起一个子进程跑这一轮，返回事件流。
//
// 兼容旧调用方：先订阅再启动，返回的通道就是本次订阅。需要「跨请求续流」的
// 调用方应改用 Subscribe + Start（见 api 层的 turnDriver）。
func (r *CLIRuntime) Prompt(ctx context.Context, sid string, req PromptReq) (<-chan Event, error) {
	ch, _ := r.Subscribe(sid)
	if err := r.Start(ctx, sid, req); err != nil {
		return nil, err
	}
	return ch, nil
}

// pump 消费子进程输出并广播到 hub，最后补一条 EventDone 并关闭 hub。
func (r *CLIRuntime) pump(ctx context.Context, sid string, proc *LineProc, hub *eventHub) {
	defer r.clearRunning(sid, proc)
	defer hub.close()

	for line := range proc.Lines() {
		evs, id := r.spec.Parse(line)
		if id != "" {
			// 当场记住：进程可能在 turn.completed 前被打断（工具调用等待、
			// 客户端断连），等到收尾才记会让下一轮丢掉可续接的 harness 会话。
			r.setHarnessID(sid, id)
		}
		for _, e := range evs {
			if e.At == 0 {
				e.At = time.Now().UnixMilli()
			}
			hub.publish(e)
		}
	}
	// 收尾。被打断时不等进程收尸：孙进程可能占着 stderr 管道，cmd.Wait
	// 会被 WaitDelay 拖住，而打断场景本来也不关心退出码。
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
			hub.publish(Event{Type: EventError, Error: msg, At: time.Now().UnixMilli()})
			finish = "error"
		}
	}
	hub.publish(Event{Type: EventDone, Finish: finish, At: time.Now().UnixMilli()})
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

// AbandonTurn 结束该会话上可能仍在跑的上一轮，并等它的事件流收尾。
//
// 用于「客户端放弃了上一轮」：断连/超时后子进程可能还停在旧回合上（工具
// 调用等待中尤其常见）。下一轮必须在 Subscribe 之前调用它 —— 旧进程的
// 收尾事件（EventDone interrupted）要留给旧订阅者，不能先进新订阅。
//
// 返回值只表示「清理已尽力」：ctx 到期时也返回，让调用方继续尝试开新轮，
// 真正的互斥仍由 Start 的 running 检查兜底。
func (r *CLIRuntime) AbandonTurn(ctx context.Context, sid string) {
	r.mu.Lock()
	proc := r.running[sid]
	r.mu.Unlock()
	if proc == nil {
		return
	}
	proc.Kill()
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		// pump 的 defer 顺序保证：clearRunning 晚于 hub.close，
		// 所以这里看不到 running 时，旧 hub 一定已经关闭。
		r.mu.Lock()
		still := r.running[sid] == proc
		r.mu.Unlock()
		if !still {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RemoveSession 丢弃会话状态（包括已学到的 harness 侧会话 id）。
//
// 用于「历史重置」：下一轮必须开一个真正的新会话，而不是 resume 旧线程。
func (r *CLIRuntime) RemoveSession(_ context.Context, sid string) error {
	r.mu.Lock()
	proc := r.running[sid]
	s := r.sessions[sid]
	delete(r.sessions, sid)
	delete(r.running, sid)
	r.mu.Unlock()
	if proc != nil {
		proc.Kill()
	}
	if s != nil && s.hub != nil {
		s.hub.close()
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

// ---------- 事件广播 ----------

// eventHub 把一条子进程事件流广播给任意数量的订阅者。
//
// 不做「最近事件缓存」：需要续流的场景（工具调用）里子进程正停在 MCP
// tools/call 上，订阅空档期不会产生事件；缓存历史反而会让下一次请求重放旧正文。
type eventHub struct {
	mu     sync.Mutex
	subs   map[*eventSub]struct{}
	closed bool
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[*eventSub]struct{}{}}
}

func (h *eventHub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func (h *eventHub) subscribe() (<-chan Event, func()) {
	sub := newEventSub(h)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		sub.finish()
		return sub.out, func() {}
	}
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	go sub.run()
	return sub.out, sub.cancel
}

func (h *eventHub) publish(e Event) {
	h.mu.Lock()
	subs := make([]*eventSub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.push(e)
	}
}

// close 表示「不会再有事件」：订阅者排空队列后关闭通道。
func (h *eventHub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := make([]*eventSub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.subs = nil
	h.mu.Unlock()
	for _, s := range subs {
		s.finish()
	}
}

func (h *eventHub) remove(s *eventSub) {
	h.mu.Lock()
	if h.subs != nil {
		delete(h.subs, s)
	}
	h.mu.Unlock()
}

// eventSub 是单个订阅者：无界队列 + 独立转发协程，避免慢消费者阻塞子进程。
type eventSub struct {
	hub  *eventHub
	out  chan Event
	done chan struct{}
	once sync.Once

	mu       sync.Mutex
	cond     *sync.Cond
	queue    []Event
	finished bool
	canceled bool
}

func newEventSub(h *eventHub) *eventSub {
	s := &eventSub{hub: h, out: make(chan Event, 64), done: make(chan struct{})}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *eventSub) push(e Event) {
	s.mu.Lock()
	if s.finished || s.canceled {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, e)
	s.cond.Signal()
	s.mu.Unlock()
}

func (s *eventSub) run() {
	defer close(s.out)
	defer s.hub.remove(s)
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.finished && !s.canceled {
			s.cond.Wait()
		}
		if s.canceled {
			s.mu.Unlock()
			return
		}
		if len(s.queue) == 0 && s.finished {
			s.mu.Unlock()
			return
		}
		e := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		select {
		case s.out <- e:
		case <-s.done:
			return
		}
	}
}

// finish 表示数据源结束：排空队列后退出。
func (s *eventSub) finish() {
	s.mu.Lock()
	s.finished = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// cancel 立即停止订阅（消费者不再需要后续事件）。
func (s *eventSub) cancel() {
	s.once.Do(func() {
		s.mu.Lock()
		s.canceled = true
		s.queue = nil
		s.cond.Broadcast()
		s.mu.Unlock()
		close(s.done)
	})
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
