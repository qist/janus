package main

import (
	"context"
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
	sess := s.tools.Register(conv.Key, tools)
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

// releaseToolsIfIdle 在本轮不再需要等客户端回填工具结果时，主动注销本会话的
// MCP server。
//
// 原因：OpenCode 会把同一 location 下注册的所有 MCP server 暴露给每个 session。
// 空闲会话的 server 若长期挂着（默认到 SessionTTL 30m），它的工具会被别的会话的
// 模型看到甚至调用（串会话）。所以一轮结束且没有 pending 调用时立即注销，下一轮
// 用到时再注册（指纹名不变时是同一名字，不会让模型看到过期工具）。
func (s *Server) releaseToolsIfIdle(conv *Conversation) {
	if conv == nil || !s.cfg.ToolCalling {
		return
	}
	if len(conv.pendingToolCalls()) > 0 || conv.mcpName == "" {
		return
	}
	dir := conv.directory
	if dir == "" {
		dir = s.cfg.Directory
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.removeTools(ctx, conv, dir)
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
	items := orderedToolResults(req.Messages)
	taken := make([]bool, len(items))
	assigned := map[string]ToolResult{} // bridgeID -> 结果

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
			s.log.Infof("tool result matched by name (id mismatch): pending=%s tool=%s client_id=%s",
				p.CallID, p.ToolName, items[i].ID)
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
			s.log.Infof("tool result matched by order (id+name mismatch): pending=%s tool=%s client_id=%s",
				p.CallID, p.ToolName, items[i].ID)
			break
		}
	}

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
