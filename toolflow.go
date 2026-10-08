package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// toolflow.go —— 工具调用的编排：注册 / 回填 / 恢复 / 清理。

// ensureTools 确保当前会话在 OpenCode 里注册了 MCP server，且工具集是最新的。
//
// 需要（重新）注册的三种情况：
//  1. 首次：会话还没注册过
//  2. 工具集变化：客户端换了 tools
//  3. 注册过期：上游（OpenCode）重启会丢掉所有 MCP 注册，而桥不知道；
//     所以超过 ToolReregister 就主动续注册一次，代价只是一次 PUT。
func (s *Server) ensureTools(ctx context.Context, conv *Conversation, dir string, tools []ToolSpec) error {
	// 客户端声明了 RunCommand 这类「异步命令」工具时，配套的状态查询工具
	//（check_command_status）只在客户端本地存在、不会写进 tools[] 声明，
	// agent 既看不到也调不到 → 长命令（build）跑起来后没法得知结果、卡住。
	// 这里按需补上配套工具，走同一透传链路给客户端执行。
	if s.cfg.ToolCompanions {
		tools = augmentCompanionTools(tools)
	}
	sess := s.tools.Register(conv.Key, tools)
	// 记录当前上游 session id：parked 工具调用超时时用它中断上游，避免孤儿 agent。
	sess.setSessionID(conv.snapshotSessionID())
	fp := toolsFingerprint(tools)

	// 注册名带工具集指纹：OpenCode 对"已存在的 MCP server 重新 PUT"不会重拉
	// tools/list，模型会一直看到旧工具。换个名字 = 新 server，必然重新连接、重列。
	name := mcpRegName(sess.mcpName, fp)

	fresh := conv.mcpName == name && conv.toolsFP == fp && conv.toolSess != nil
	if fresh && s.cfg.ToolReregister > 0 && time.Since(conv.toolsRegAt) < s.cfg.ToolReregister {
		return nil
	}

	url := s.mcpEndpoint(sess.token)
	if err := s.up.AddMCP(ctx, dir, name, url, nil, s.cfg.ToolCallWait); err != nil {
		return err
	}
	// 工具集变化时清掉上一个名字（删不掉也没关系，janitor 会扫残留）。
	if conv.mcpName != "" && conv.mcpName != name {
		if err := s.up.RemoveMCP(ctx, dir, conv.mcpName); err != nil {
			s.log.Debugf("remove superseded mcp %s: %v", conv.mcpName, err)
		}
	}
	s.tools.Alias(sess, name)
	conv.mcpName = name
	conv.toolsFP = fp
	conv.toolSess = sess
	conv.toolsRegAt = time.Now()
	verb := "registered"
	if fresh {
		verb = "re-registered"
	}
	s.metrics.incToolReg()
	s.log.Infof("tool bridge %s: server=%s tools=%d dir=%s", verb, name, len(sess.snapshotTools()), dir)
	return nil
}

// removeTools 注销当前会话的 MCP server（客户端不再声明 tools 时调用）。
func (s *Server) removeTools(ctx context.Context, conv *Conversation, dir string) {
	name := conv.mcpName
	conv.mcpName = ""
	conv.toolsFP = ""
	conv.toolSess = nil
	conv.pendingTools = nil
	conv.toolsRegAt = time.Time{}
	s.tools.Unregister(conv.Key)
	if name == "" {
		return
	}
	if err := s.up.RemoveMCP(ctx, dir, name); err != nil {
		s.log.Debugf("remove mcp %s: %v", name, err)
		return
	}
	s.log.Infof("tool bridge unregistered: server=%s", name)
}

// sweepTools 清理"会话已经不在 store 里"的 MCP 注册，避免上游越积越多。
func (s *Server) sweepTools(ctx context.Context) {
	names, err := s.up.ListMCP(ctx, s.cfg.Directory)
	if err != nil {
		return
	}
	for _, n := range names {
		if !strings.HasPrefix(n, mcpNamePrefix) {
			continue
		}
		if s.tools.sessionByName(n) != nil {
			continue // 还活着
		}
		if err := s.up.RemoveMCP(ctx, s.cfg.Directory, n); err == nil {
			s.log.Infof("janitor removed stale mcp server %s", n)
		}
	}
}

// sessionByName 供 sweep 判断某个 MCP server 是否属于活跃会话。
func (b *ToolBridge) sessionByName(name string) *toolSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.byName[name]
}

// ---------- 配套工具补全 ----------

// companionTriggerTools 是「异步命令」类工具：命中即视为客户端支持配套的状态查询。
var companionTriggerTools = map[string]bool{
	"runcommand":      true, // Trae/CodeBuddy 等 PascalCase 命名
	"run_command":     true,
	"execute_command": true,
	"executecommand":  true,
}

// companionToolID 是配套状态查询工具的入参（RunCommand 返回的异步命令 id）。
const companionToolID = "command_id"

// companionToolSchema 描述配套状态查询工具的入参：command_id 取 RunCommand 返回的 id。
var companionToolSchema = json.RawMessage(`{"type":"object","properties":{"` +
	companionToolID +
	`":{"type":"string","description":"RunCommand 返回的异步命令 id，用于查询该命令的最新状态/输出"}},"required":["` +
	companionToolID +
	`"]}`)

// augmentCompanionTools 给「异步命令」类工具补配套的状态查询工具。
//
// 实测（Trae）：RunCommand 对长任务立即返回 command_id、异步执行，但配套的状态
// 查询工具只在客户端本地有、不会写进发给桥的 tools[] 声明（"在工具列表外"）。
// 于是 agent 只看到 RunCommand：build 跑起来后既不知道结束、也拿不到输出，
// 只能猜/干等 → 卡住。补上配套工具后，agent 正常闭环：
//
//	RunCommand → command_id → 状态查询工具 → 结果
//
// 配套工具叫什么，**优先从触发工具的描述里自动识别**（不同客户端文档里写的名字
// 不同：CheckCommandStatus / check_command_status / GetCommandStatus… 描述里
// 写了哪个就用哪个，不靠猜）；描述里没写才退回默认两个名字都试
// （CheckCommandStatus 与 check_command_status，哪个跟客户端本地名字一致哪个成功）。
// 名字原样透传给客户端本地执行；客户端已声明过同名工具则不重复。
func augmentCompanionTools(tools []ToolSpec) []ToolSpec {
	var trigger *ToolSpec
	for i := range tools {
		if companionTriggerTools[strings.ToLower(tools[i].Function.Name)] {
			trigger = &tools[i]
			break
		}
	}
	if trigger == nil {
		return tools
	}
	names := companionNamesFromDescription(trigger.Function.Description)
	if len(names) == 0 {
		names = []string{"CheckCommandStatus", "check_command_status"}
	}
	out := append([]ToolSpec(nil), tools...)
	for _, name := range names {
		if hasTool(out, name) {
			continue // 客户端自己声明了，别重复
		}
		out = append(out, ToolSpec{
			Type: "function",
			Function: ToolFunction{
				Name: name,
				Description: "查询 RunCommand 启动的异步命令的最新状态与输出。" +
					"command_id 必须取 RunCommand 返回的 id；构建/编译等长任务仍在运行时返回" +
					"继续等待，结束后返回退出码与最终输出。",
				Parameters: companionToolSchema,
			},
		})
	}
	return normalizeTools(out)
}

// companionToolTokens 是从触发工具（RunCommand…）描述里识别配套状态查询工具名的
// 候选标志（按出现顺序、原文大小写照抄，避免猜错客户端本地名字）。
var companionToolTokens = []string{
	"CheckCommandStatus",
	"check_command_status",
	"GetCommandStatus",
	"get_command_status",
	"CheckTerminalStatus",
	"check_terminal_status",
}

// companionNamesFromDescription 从描述文本里提取配套状态查询工具名（出现即用）。
func companionNamesFromDescription(desc string) []string {
	var out []string
	for _, tok := range companionToolTokens {
		if !strings.Contains(desc, tok) {
			continue
		}
		dup := false
		for _, n := range out {
			if n == tok {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, tok)
		}
	}
	return out
}

// companionStatusFamily 是配套状态查询工具的已知名字集合（小写）。用于调它失败时
// 给 agent 补一句"换名重试"提示，避免它在工具列表里反复搜索、卡住。
var companionStatusFamily = map[string]bool{
	"checkcommandstatus":    true,
	"check_command_status":  true,
	"getcommandstatus":      true,
	"get_command_status":    true,
	"checkterminalstatus":   true,
	"check_terminal_status": true,
}

// companionErrorHint 返回配套状态查询工具失败时应附加给 agent 的提示；非配套工具返回空。
func companionErrorHint(toolName string) string {
	if !companionStatusFamily[strings.ToLower(toolName)] {
		return ""
	}
	return "\n\nbridge hint: 若错误是\"工具名不存在/名称不对\"，说明客户端没认这个名字：" +
		"请改用另一个状态查询工具名重试（CheckCommandStatus / check_command_status，" +
		"command_id 不变）。两个都不行的话，需把状态查询工具加进客户端的工具声明，" +
		"或改用上游原生命令工具执行。"
}

// hasTool 判断工具集里是否已存在指定名字（大小写敏感，与客户端声明一致）。
func hasTool(tools []ToolSpec, name string) bool {
	for _, t := range tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

// ---------- 客户端回填工具结果 ----------

// toolResultsFromMessages 从请求的 messages[] 里提取 role:"tool" 的结果，按 tool_call_id 索引。
func toolResultsFromMessages(msgs []ChatMessage) map[string]ToolResult {
	out := map[string]ToolResult{}
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		id := m.ToolCallID
		if id == "" {
			continue
		}
		out[id] = ToolResult{Content: m.Content.Text}
	}
	return out
}

// hasToolResults 判断请求里是否带了工具执行结果。
func hasToolResults(msgs []ChatMessage) bool {
	for _, m := range msgs {
		if m.Role == "tool" {
			return true
		}
	}
	return false
}

// stripStaleToolResults 在「全部工具调用均已超时、客户端才把结果补回来」的场景下，
// 从消息里去掉这些过期结果与纯 tool_calls 的中间轮。
//
// 为什么必须剥：客户端可能把积压的一整批旧结果一起回填（实测一次 18 条，含早已
// 消费过的），若照旧平铺进新 prompt，agent 会把过期数据当新上下文用——"数据回放"。
// 剥离后只留真正的对话内容；若一条 user 都没有（客户端只回填了结果、没带新指令），
// 退回原列表的最后一条 user，让 agent 至少有内容可回应。
func stripStaleToolResults(msgs []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			continue // 过期结果：不进 prompt
		case "assistant":
			// 纯"调用工具"的中间轮没有内容，整条丢弃；带文字（推理/结论）的保留但清掉
			// tool_calls —— 它们的对应结果已被丢弃，留着只会引诱模型去"消费"旧结果。
			if m.Content.Text == "" {
				continue
			}
			m.ToolCalls = nil
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return msgs // 极端退化：全部剥光，回退旧行为
	}
	hasUser := false
	for _, m := range out {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		if u := lastUserTurn(msgs); len(u) > 0 {
			out = append(out, u...)
		}
	}
	return out
}

// livePending 过滤掉已超时（stale）的挂起调用，保留仍在等客户端回填的。
//
// 客户端在等待期内没回结果时，桥会按超时释放并中断上游 agent（见 mcp.go）。
// 它若之后才把结果发回来，绝不能走 resume：agent 已不在等，续跑只是空转
// （无 prompt → stalled → 客户端再收到一次"模型请求失败"）。过滤后走普通
// 新轮次，把结果平铺进提示词（FlattenDelta 输出为 [Tool: name]）继续干活。
func livePending(list []*pendingCall) []*pendingCall {
	out := list[:0]
	for _, p := range list {
		if p.isStale() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// resultIDs 列出客户端回填的所有 tool_call_id（诊断用，排序后稳定输出）。
func resultIDs(m map[string]ToolResult) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// toolResultItem 是客户端回填的一条工具结果（带它对应的工具名）。
type toolResultItem struct {
	ID     string
	Name   string // 来自请求里 assistant.tool_calls 的 id→name（可能为空）
	Result ToolResult
}

// orderedToolResults 列出「当前这一轮」客户端回填的工具结果（带工具名）。
//
// 只取最后一条带 tool_calls 的 assistant 之后的结果 —— 否则整段历史里的旧结果
// 会被同名匹配命中，返回上一轮的缓存内容。用于客户端 tool_call_id 与 janus 对不上
// 时按「工具名 + 顺序」兜底。
func orderedToolResults(msgs []ChatMessage) []toolResultItem {
	last := -1
	for i, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	nameOf := map[string]string{}
	for _, tc := range msgs[last].ToolCalls {
		if tc.ID != "" && tc.Function != nil {
			nameOf[tc.ID] = tc.Function.Name
		}
	}
	var out []toolResultItem
	for _, m := range msgs[last+1:] {
		if m.Role != "tool" || m.ToolCallID == "" {
			continue
		}
		out = append(out, toolResultItem{ID: m.ToolCallID, Name: nameOf[m.ToolCallID], Result: ToolResult{Content: m.Content.Text}})
	}
	return out
}

// assignToolResults 按「① 精确 id → ② 工具名 → ③ 顺序」三级对齐客户端回填的
// 工具结果，返回 bridgeID（call_xxx）→ 客户端结果的映射。
//
// 部分客户端（实测 Trae / Claude Code）会用它自己生成的 id 回填（toolu_… /
// 自定义 hex），与 janus 下发到下游的 call_xxx 对不上；Chat / Anthropic /
// Responses 三条路径统一走这个对齐，避免 answered=0/N、agent 拿到
// "did not supply a result"。
func assignToolResults(pending []*pendingCall, results map[string]ToolResult,
	items []toolResultItem, log *Logger) map[string]ToolResult {

	assigned := map[string]ToolResult{}
	taken := make([]bool, len(items))

	// ① 精确 id
	for _, p := range pending {
		if r, ok := results[p.CallID]; ok {
			assigned[p.CallID] = r
			for i := range items {
				if !taken[i] && items[i].ID == p.CallID {
					taken[i] = true
					break
				}
			}
		}
	}
	// ② 工具名
	for _, p := range pending {
		if _, ok := assigned[p.CallID]; ok {
			continue
		}
		for i := range items {
			if taken[i] || items[i].Name != p.ToolName {
				continue
			}
			assigned[p.CallID] = items[i].Result
			taken[i] = true
			if log != nil {
				log.Infof("tool result matched by name (id mismatch): pending=%s tool=%s client_id=%s",
					p.CallID, p.ToolName, items[i].ID)
			}
			break
		}
	}
	// ③ 顺序兜底（名字也拿不到时）
	for _, p := range pending {
		if _, ok := assigned[p.CallID]; ok {
			continue
		}
		for i := range items {
			if taken[i] {
				continue
			}
			assigned[p.CallID] = items[i].Result
			taken[i] = true
			if log != nil {
				log.Infof("tool result matched by order (id+name mismatch): pending=%s tool=%s client_id=%s",
					p.CallID, p.ToolName, items[i].ID)
			}
			break
		}
	}
	return assigned
}

// resumeToolCalls 处理「客户端回填工具结果」的后续请求。
//
// 关键点：
//   - 不发送新 prompt。上游 agent 还停在 MCP tools/call 上，等我们把结果喂回去。
//   - 必须先订阅事件再回填，否则会漏掉 agent 继续执行时产生的开头增量。
//   - 回填后 agent 可能给出最终答案，也可能再次调用工具（循环），
//     所以后续流程与普通一轮完全一致。
func (s *Server) resumeToolCalls(ctx context.Context, w http.ResponseWriter, r *http.Request,
	req ChatRequest, ref OCModelRef, conv *Conversation,
	pending []*pendingCall, results map[string]ToolResult,
	dir string, sub *subscription, promptAt int64) {

	// 先订阅，再唤醒 agent。
	// 客户端回填的结果按 tool_call_id 索引；但有些客户端（实测）会用它自己生成的 id，
	// 而不是 janus 下发的 call_xxx —— 按「① 精确 id → ② 工具名 → ③ 顺序」三级对齐，
	// 建立 pending ↔ 客户端结果的对应关系。
	assigned := assignToolResults(pending, results, orderedToolResults(req.Messages), s.log)

	answered := 0
	for _, p := range pending {
		res, ok := assigned[p.CallID]
		if !ok {
			// 客户端没给这一条的结果：以错误收尾，避免 agent 永久挂住。
			s.log.Warnf("tool result missing: pending=%s tool=%s; client supplied=%v",
				p.CallID, p.ToolName, resultIDs(results))
			res = ToolResult{
				Content: "bridge: client did not supply a result for tool call " + p.CallID,
				IsError: true,
			}
		} else {
			answered++
		}
		p.complete(res)
	}
	s.log.Infof("tool results delivered: conv=%s answered=%d/%d", conv.Key, answered, len(pending))

	// 让 agent 跑起来，复用流式/非流式收尾
	if req.Stream {
		s.streamCompletion(w, r, req, ref, conv, sub, promptAt, "")
		return
	}
	s.blockingCompletion(ctx, w, req, ref, conv, sub, promptAt)
}

// pendingToolCalls 取出本会话等待回填的调用。
func (c *Conversation) pendingToolCalls() []*pendingCall {
	return c.pendingTools
}

func (c *Conversation) setPendingToolCalls(p []*pendingCall) {
	c.pendingTools = p
}
