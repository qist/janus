package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"janus/internal/agent"
	"janus/internal/gateway"
	"janus/internal/upstream"
)

// cli_turn.go —— 把 CLI 型 harness（codex / claude-code）接进 IDE 请求路径。
//
// 与 opencode 的差别决定了这里的实现形态：
//   - 事件来自子进程 stdout，而不是 upstream.EventBus；
//   - 没有 /message 接口可对账，也没有上游 idle 状态可查询；
//   - 一轮子进程可能跨多个 HTTP 请求（工具调用回填后继续跑同一进程）。
//
// 因此 CLI 的 turnSession 只做三件事：订阅会话级事件 hub、把 prompt 变成
// 子进程启动、把打断转发给 runtime。Cancel 只解绑订阅，绝不杀子进程 ——
// 工具调用回合里第一次请求返回 tool_calls 后，进程必须继续停在 MCP
// tools/call 上等下一次请求回填结果（见 agent.CLIRuntime 的 eventHub）。

// cliMCPName 是注册到 CLI harness 的 MCP server 名。
//
// 固定值而不是按会话命名：CLI 的 MCP 配置写在 argv/env 里，一进程一配置，
// 不存在 opencode 那种「同 location 下所有 server 互相可见」的串会话问题；
// 固定名字也让日志/排查更直观。
const cliMCPName = "janus"

// cliTurnDriver 是 turnDriver 的 CLI 实现。
type cliTurnDriver struct {
	s     *Server
	rt    *agent.CLIRuntime
	scene *cliScene
}

func (d cliTurnDriver) Subscribe(_ context.Context, sid string) turnSession {
	ch, cancel := d.rt.Subscribe(sid)
	return &cliTurnSession{rt: d.rt, sid: sid, ch: ch, cancel: cancel}
}

// PrepareTurn 收掉被放弃的上一轮：客户端断连/超时后子进程可能还停在旧回合
// 上（尤其是工具调用等待中）。留在那里会让下一次 Prompt 撞上
// "session already has a running turn"，且旧进程的收尾事件会先进新订阅。
func (d cliTurnDriver) PrepareTurn(ctx context.Context, sid string) {
	if sid == "" {
		return
	}
	d.rt.AbandonTurn(ctx, sid)
}

func (d cliTurnDriver) Prompt(_ context.Context, sid string, req upstream.OCPromptReq) (int64, error) {
	// 子进程不随 handler 返回而取消：工具调用回合里，第一次请求带着
	// tool_calls 返回后，进程要一直停在 MCP tools/call 上等下一次请求。
	// 生命周期由 runtime 的 running 表和 Interrupt/RemoveSession 管理。
	if err := d.rt.Start(context.Background(), sid, d.scene.request(req.Text)); err != nil {
		return 0, err
	}
	return time.Now().UnixMilli(), nil
}

// cliTurnSession 把 runtime 的事件订阅适配成 turnSession。
type cliTurnSession struct {
	rt     *agent.CLIRuntime
	sid    string
	ch     <-chan agent.Event
	cancel func()
}

func (t *cliTurnSession) Events() <-chan agent.Event { return t.ch }

// Reconcile 恒为 false：CLI 的事件流就是唯一真相，没有独立对账数据源。
func (t *cliTurnSession) Reconcile(context.Context, int64) (turnSnapshot, bool) {
	return turnSnapshot{}, false
}

func (t *cliTurnSession) ReconcileSupported() bool { return false }

// State 恒为 false：CLI 没有上游 idle 时间戳，收尾只由 EventDone 驱动。
func (t *cliTurnSession) State(context.Context, int64, bool) (bool, string) { return false, "" }

// WaitIdle 必须阻塞到 ctx 结束且永不在事件消费完之前返回 nil。
//
// executor.runEvents 把「WaitIdle 返回 nil」当作 agent loop 已空闲，会立刻
// 收尾本轮；CLI 的子进程在 EventDone 之前随时可能继续输出，提前返回会截断
// 正文。因此这里只等 ctx（请求超时/取消），由 EventDone 负责正常收尾。
func (t *cliTurnSession) WaitIdle(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (t *cliTurnSession) Interrupt(ctx context.Context) error {
	return t.rt.Interrupt(ctx, t.sid)
}

// Cancel 只解绑本次订阅；子进程继续跑（工具续流依赖这一点）。
func (t *cliTurnSession) Cancel() {
	if t.cancel != nil {
		t.cancel()
	}
}

// cliScene 是本次请求内的 CLI 上下文：模型面地址、临时 key 的签发依据、
// 以及归一后的模型名。它不持有请求级资源，因此不需要 release。
type cliScene struct {
	baseURL   string
	principal Principal

	ref   upstream.OCModelRef
	model string // 传给 CLI 的模型名（provider/id[:variant]）
}

// requestRuntime 汇总本次请求选中的 harness 与其 CLI scene（非 CLI 时 scene=nil）。
type requestRuntime struct {
	rt    agent.AgentRuntime
	cli   *agent.CLIRuntime
	scene *cliScene
}

// runtimeForRequest 选本次请求的 runtime；CLI 型顺带构造 scene。
//
// CLI 的模型面协议由 harness 自己决定（codex 走 Responses，claude-code 走
// Anthropic），路由解析必须按该协议判断「这个模型能不能直连」，所以这里覆盖
// 调用方传入的协议。
func (s *Server) runtimeForRequest(r *http.Request, p Principal, protocol, model string) requestRuntime {
	rt := s.agentRuntimeFor(r, p)
	rr := requestRuntime{rt: rt}
	if crt, ok := rt.(*agent.CLIRuntime); ok {
		rr.cli = crt
		rr.scene = s.newCLIScene(r, p, gatewayProtocolFor(crt.Name()), model)
	}
	return rr
}

// alignRuntime 让会话绑定到本次选中的 harness。
//
// 会话已有绑定且换了 harness 时必须重置：OpenCode 的 sessionID 和 CLI 的
// harness 会话 id 是两套体系，混用会让模型看到断裂甚至别人的上下文。
// 空值表示「尚未确定」（旧库行），直接沿用本请求的 runtime，不触发重置。
func (s *Server) alignRuntime(conv *Conversation, rt agent.AgentRuntime) {
	if conv == nil || rt == nil {
		return
	}
	name := rt.Name()
	if conv.runtime != "" && conv.runtime != name {
		s.log.Infof("runtime changed %s -> %s; resetting conversation (key=%s)", conv.runtime, name, conv.Key)
		s.resetSession(conv)
	}
	conv.runtime = name
}

// prepareTools 按 runtime 注册客户端工具：OpenCode 注册到上游，CLI 写进子进程配置。
func (s *Server) prepareTools(ctx context.Context, conv *Conversation, rt agent.AgentRuntime,
	dir string, tools []ToolSpec, binding string) error {
	if crt, ok := rt.(*agent.CLIRuntime); ok {
		s.ensureCLITools(conv, crt, tools, binding)
		return nil
	}
	return s.ensureTools(ctx, conv, dir, tools, binding)
}

// ensureRuntimeSession 保证底层会话存在：OpenCode 建/复用上游会话，CLI 刷新
// 子进程配置（模型/key/MCP 地址）。created 表示本轮新建了会话。
func (s *Server) ensureRuntimeSession(ctx context.Context, conv *Conversation, rr requestRuntime,
	ref upstream.OCModelRef, agentName, dir string, msgs []ChatMessage) (created bool, err error) {
	if rr.cli != nil {
		return s.ensureCLISession(ctx, conv, rr.cli, rr.scene, dir)
	}
	return s.ensureSession(ctx, conv, ref, agentName, dir, msgs)
}

func (s *Server) newCLIScene(r *http.Request, p Principal, protocol, model string) *cliScene {
	ref := s.cliModelRef(p, protocol, model)
	return &cliScene{
		baseURL:   s.publicBaseURL(r),
		principal: p,
		ref:       ref,
		model:     cliModelName(ref, model),
	}
}

// request 把一轮 prompt 转成 runtime 的输入。
//
// CLI 的工具来自 MCP（不需要 PromptReq.Tools），历史由 harness 侧会话维护，
// 因此这里只送本轮文本。
func (c *cliScene) request(text string) agent.PromptReq {
	return agent.PromptReq{
		Model:    c.model,
		Stream:   true,
		Messages: []agent.Message{{Role: "user", Content: text}},
	}
}

// cliModelRef 把客户端模型名归一成真实模型。
//
// 优先走 routeDirect（与模型面透传同一套路由/默认模型别名），这样 CLI 拿到
// 的是 "provider/upstream" 形态，回调 janus 时能直接命中同一条落点；路由失败
// 时按 "provider/model" 原样拆分，最后退回裸名（由内层请求的路由层兜底）。
func (s *Server) cliModelRef(p Principal, protocol, raw string) upstream.OCModelRef {
	name := strings.TrimSpace(raw)
	if isDefaultAlias(name) || isJanusAlias(name) {
		switch {
		case strings.TrimSpace(s.runtimeDefaultModel()) != "":
			name = strings.TrimSpace(s.runtimeDefaultModel())
		case strings.TrimSpace(s.cfg.DefaultModel) != "":
			name = strings.TrimSpace(s.cfg.DefaultModel)
		default:
			name = "default"
		}
	}
	if name == "" {
		name = "default"
	}
	if s.router != nil && s.router.Store() != nil {
		if res, err := s.routeDirect(p, protocol, name, ""); err == nil && res.UpstreamID != "" {
			return upstream.OCModelRef{
				ProviderID: res.ProviderID,
				ID:         res.UpstreamID,
				Variant:    res.Variant,
			}
		}
	}
	if ref, ok := parseOCModelRef(name); ok {
		return ref
	}
	return upstream.OCModelRef{ID: name}
}

// cliModelName 返回传给 CLI 的 --model 值。
func cliModelName(ref upstream.OCModelRef, raw string) string {
	if ref.ProviderID != "" && ref.ID != "" {
		return ref.String()
	}
	if ref.ID != "" {
		return ref.ID
	}
	if raw = strings.TrimSpace(raw); raw != "" {
		return raw
	}
	return "default"
}

// cliGatewayKey 返回本会话给 CLI 用的临时 agent key（按需签发/续期）。
//
// key 是会话级的，而不是「一请求一签」：工具调用回合里子进程会跨请求存活，
// 续流后仍要用启动时拿到的 key 调模型面；请求级 key 会在第一次 HTTP 返回时
// 被撤销，导致续流后的模型请求 401。TTL 覆盖一次工具等待 + 请求超时，
// 过期后下一次请求自动续签；reset 时主动撤销。
func (s *Server) cliGatewayKey(conv *Conversation, scene *cliScene) (string, error) {
	now := time.Now()
	// 预留 10 分钟余量：避免子进程续流时 key 恰好过期。
	if conv.cliKey != "" && conv.cliBaseURL == scene.baseURL &&
		now.Add(10*time.Minute).Before(conv.cliKeyExpires) {
		return conv.cliKey, nil
	}

	ttl := s.cfg.ToolCallWait + s.cfg.RequestTimeout + 10*time.Minute
	switch {
	case ttl < time.Hour:
		ttl = time.Hour
	case ttl > 24*time.Hour:
		ttl = 24 * time.Hour
	}
	key, release := s.mintInternalAgentKey(ttl, scene.principal)
	if key == "" {
		return "", errors.New("cannot mint an internal agent key for the CLI harness")
	}
	// 旧 key 不立即撤销：上一轮的子进程可能仍停在 MCP 调用上，续流后还要用它。
	// TTL 会兜底回收，这里只放弃引用。
	conv.cliKey = key
	conv.cliKeyExpires = now.Add(ttl)
	conv.cliBaseURL = scene.baseURL
	conv.cliKeyRelease = release
	return key, nil
}

// ensureCLISession 确保 CLI 会话存在，并刷新本轮的 gateway/MCP 配置。
//
// 复用同一 session key 时 runtime 只更新配置、保留已学到的 harness 会话 id，
// 因此这里每轮都调用 CreateSession（模型/key/MCP 工具集可能变化）。
func (s *Server) ensureCLISession(ctx context.Context, conv *Conversation, rt *agent.CLIRuntime,
	scene *cliScene, dir string) (created bool, err error) {

	if err := rt.EnsureReady(ctx); err != nil {
		return false, err
	}
	sid := conv.snapshotSessionID()
	created = sid == ""
	if created {
		// 同一 conversationKey 的桶里可能有多个会话（不同话题/子代理），直接用
		// conv.Key 会让它们共用同一个 CLI 会话，上下文互相串味。加随机后缀保证
		// 一个 Conversation 一个 sid；sid 会随会话落库，跨重启仍能对上。
		sid = conv.Key + "#" + randomToken()[:8]
	}
	key, err := s.cliGatewayKey(conv, scene)
	if err != nil {
		return false, err
	}
	mcpURL := ""
	if conv.toolSess != nil {
		mcpURL = s.mcpEndpoint(conv.toolSess.token)
	}
	if _, err := rt.CreateSession(ctx, agent.SessionSpec{
		Key:            sid,
		Directory:      dir,
		Model:          scene.model,
		GatewayBaseURL: scene.baseURL,
		GatewayAPIKey:  key,
		MCPURL:         mcpURL,
		MCPName:        cliMCPName,
	}); err != nil {
		return false, err
	}

	conv.setSessionID(sid)
	conv.runtime = rt.Name()
	conv.model = scene.ref
	conv.directory = dir
	// 同步工具桥的会话绑定：换绑会让旧会话遗留的挂起调用立刻以错误收尾。
	if conv.toolSess != nil {
		conv.toolSess.setSessionID(sid)
	}
	s.persistConv(conv)
	if created {
		s.log.Infof("cli session created %s runtime=%s dir=%s model=%s", sid, rt.Name(), dir, scene.model)
	}
	return created, nil
}

// ensureCLITools 为 CLI 会话准备工具桥。
//
// 与 opencode 的 ensureTools 共用「登记工具 + 稳定绑定 + 路径改写」逻辑，
// 但不调用 s.up.AddMCP：MCP 地址通过 CreateSession 写进 CLI 的 argv/env，
// 由子进程自己连接。
func (s *Server) ensureCLITools(conv *Conversation, rt *agent.CLIRuntime, tools []ToolSpec, binding string) {
	// 工具结果超时时中断本轮 agent：CLI 型是杀子进程，而不是调 OpenCode。
	sess, name, fp := s.prepareToolSession(conv, tools, binding, rt.Interrupt)

	conv.mcpName = name
	conv.toolsFP = fp
	conv.toolSess = sess
	conv.toolsRegAt = time.Now()
	s.metrics.IncToolReg()
	s.log.Infof("cli tool bridge ready: server=%s tools=%d dir=%s", name, len(sess.snapshotTools()), conv.directory)
}

// cliRestoredSessionLost 报告「从库恢复的 CLI 会话」在 runtime 里已不存在
// （进程重启后 harness 侧线程随子进程状态一起丢了）。此时必须重置会话、
// 全量重放历史，否则模型只能看到增量、上下文断裂。
func (s *Server) cliRestoredSessionLost(conv *Conversation, rt *agent.CLIRuntime) bool {
	if rt == nil || !conv.restoredFromDB.Load() {
		return false
	}
	sid := conv.snapshotSessionID()
	return sid != "" && rt.HarnessID(sid) == ""
}

// cliRuntimeByName 按 runtime 名取 CLI runtime（未注册/非 CLI 返回 nil）。
func (s *Server) cliRuntimeByName(name string) *agent.CLIRuntime {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	rt, ok := s.agentRegistry().Get(name)
	if !ok {
		return nil
	}
	crt, _ := rt.(*agent.CLIRuntime)
	return crt
}

// isCLIRuntime 判断某个 runtime 名是否属于 CLI 型（用于 reset/removeTools 分流）。
func (s *Server) isCLIRuntime(name string) bool {
	return s.cliRuntimeByName(name) != nil
}

// turnDriverFor 按 runtime 选择本轮驱动；CLI 型需要 scene 提供模型/地址。
func (s *Server) turnDriverFor(rt agent.AgentRuntime, scene *cliScene) turnDriver {
	if crt, ok := rt.(*agent.CLIRuntime); ok && scene != nil {
		return cliTurnDriver{s: s, rt: crt, scene: scene}
	}
	return s.turnDriver()
}

// gatewayProtocolFor 由 CLI harness 的回调协议决定路由时的协议语义。
func gatewayProtocolFor(runtimeName string) string {
	switch runtimeName {
	case "claude-code":
		return gateway.ProtocolAnthropic
	default:
		return gateway.ProtocolOpenAIResponses
	}
}
