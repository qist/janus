package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// mcp.go —— 桥内置的 MCP server（Streamable HTTP）。
//
// 协议要点（实测 OpenCode desktop 2.0.22 的行为）：
//   - 客户端 POST JSON-RPC，Accept: application/json, text/event-stream
//   - initialize 必须回 protocolVersion / capabilities / serverInfo，并带 Mcp-Session-Id
//   - 通知类消息（无 id）回 202 无 body
//   - 客户端会额外 GET 一次 Accept: text/event-stream 开服务端推送通道；
//     我们不需要服务端主动推送，回 405 即可（实测客户端会正常回退到 POST）
//
// 一个 token 对应一个会话（URL 里的 /mcp/{token} 就是凭据）。

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.tools == nil {
		http.Error(w, "tool bridge disabled", http.StatusServiceUnavailable)
		return
	}
	// 来源白名单（BRIDGE_MCP_ALLOW）。默认空=不限制；外网暴露时建议设为
	// 上游 OpenCode 的来源网段。
	if !s.mcpSourceAllowed(r) {
		s.log.Warnf("mcp request from disallowed source: %s", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	token := r.PathValue("token")
	sess := s.tools.Get(token)

	switch r.Method {
	case http.MethodGet:
		// 不需要服务端推送；405 让客户端回退到 POST（实测可行）
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
		// 继续
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json-rpc", http.StatusBadRequest)
		return
	}

	// 通知类：无 id → 202
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// 未知 token：会话重置/回收后，上游可能还拿着旧的 MCP 配置在调。
	// 不再返回 404（上游会据此判定整条 MCP 连接失败、agent 报"服务挂了"），
	// 而是给一个"空会话"式的可恢复响应，让上游平滑收敛。
	if sess == nil {
		s.log.Debugf("mcp request for unknown session (method=%s); serving empty session", req.Method)
		s.handleUnknownMCPSession(w, r, token, &req)
		return
	}

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		// 回客户端请求的版本，保证协商成功
		ver := p.ProtocolVersion
		if ver == "" {
			ver = "2025-06-18"
		}
		w.Header().Set("Mcp-Session-Id", sess.token)
		mcpReply(w, req.ID, map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "janus", "version": "1.0"},
		})

	case "ping":
		mcpReply(w, req.ID, map[string]any{})

	case "tools/list":
		tools := sess.snapshotTools()
		out := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			schema := t.Function.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out = append(out, map[string]any{
				"name":        sess.mcpToolName(t.Function.Name),
				"description": t.Function.Description,
				"inputSchema": schema,
			})
		}
		mcpReply(w, req.ID, map[string]any{"tools": out})

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			mcpErr(w, req.ID, -32602, "invalid tools/call params")
			return
		}
		original, ok := sess.originalName(p.Name)
		if !ok {
			mcpErr(w, req.ID, -32601, "unknown tool "+p.Name)
			return
		}
		args := string(p.Arguments)
		if args == "" || args == "null" {
			args = "{}"
		}
		s.mcpToolCall(w, r, req.ID, sess, p.Name, original, args)

	default:
		s.log.Debugf("mcp unhandled method %q", req.Method)
		mcpErr(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// mcpToolCall 处理一次 tools/call 的公共流程：参数改写 →（可选）桥内执行 →
// 防孤儿守卫 → 挂起等客户端回填。已知 token 与 stale-token 重路由两条路径共用。
func (s *Server) mcpToolCall(w http.ResponseWriter, r *http.Request, id json.RawMessage,
	sess *toolSession, mcpName, original, args string) {

	// 远程 mode B：参数里的工作区路径改写成客户端项目路径（见 toolbridge.rewriteArgs）。
	args = sess.rewriteArgs(args)

	// 桥自己执行的工具（如 Claude Code 的 web_search 服务端工具）：
	// 直接执行并把结果回给 agent，不甩给客户端。
	if fn := sess.serverTool(original); fn != nil {
		content, isErr := fn(r.Context(), args)
		s.log.Debugf("mcp server-tool executed: %s", original)
		mcpReply(w, id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": content}},
			"isError": isErr,
		})
		return
	}

	callID := "call_" + randomToken()[:24]

	// 防串会话：OpenCode 会把同 location 下所有 MCP server 暴露给每个
	// session，模型可能引用到别的会话的 server。目标会话没有在飞请求时
	// 立即报错，别挂到 ToolCallWait 超时。
	if !sess.waitForWaiter(s.cfg.ToolOrphanWait) {
		// 目标会话没有在飞请求：等够 ToolOrphanWait 还没人接手 → 判孤儿
		// （客户端在两轮之间离开时就是这样：agent 还在调工具，但没人会回结果）。
		// 只拒绝，不中断：MCP 请求不携带调用方 session id，无法确定调用出自哪个
		// 上游会话，按「当前绑定」中断会误杀活会话——实测 3 次：垂死 agent 的尾期
		// 调用被算到正在服务的会话头上；且回合以 tool_calls 交给客户端后 agent
		// 停在挂起调用上等回填，本来就没有 waiter，同样会被误判成孤儿。孤儿
		// agent 由其他路径兜底终止：幽灵调用超时中断其宿主会话、工具桥换绑清理、
		// terminated 重建时的 DeleteSession，以及模型收到连续报错后自行放弃。
		if ok, suppressed := sess.shouldLogReject(60 * time.Second); ok {
			if suppressed > 0 {
				s.log.Warnf("mcp tool call for idle conversation: %s (%s); rejecting (suppressed %d)",
					original, sess.key, suppressed)
			} else {
				s.log.Warnf("mcp tool call for idle conversation: %s (%s); rejecting", original, sess.key)
			}
		}
		// 拒绝消息按状态分流：旧文案结尾"call a tool exposed for your current
		// conversation"被模型当成重试暗示，实测 agent 连续换着花样调了 5 轮工具、
		// 每轮白等 30s 后才放弃。明确劝停，让它尽快收尾。
		msg := "bridge: no active client request on this conversation (client disconnected, between turns, or this tool belongs to another conversation). " +
			"Do not retry tools on this server; call a tool exposed for your current conversation, or end your turn and briefly tell the user that tool execution is temporarily unavailable."
		if sess.hasPending() {
			msg = "bridge: a tool call was already delivered to this conversation's client and is still awaiting its result; " +
				"new tool calls cannot be accepted until the client responds. Stop calling tools and end your turn — briefly tell the user that tool execution is temporarily unavailable."
		}
		mcpReply(w, id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": msg}},
			"isError": true,
		})
		return
	}

	// 挂起：先通知执行器有新调用，然后等客户端在后续请求里回填结果。
	pend := sess.park(callID, original, mcpName, args)
	s.log.Debugf("mcp tool call parked: %s (%s) args=%s", original, callID, truncate(args, 200))
	res := s.tools.waitResult(r.Context(), pend)

	// 调用已终结（客户端在 ToolCallWait 内没回结果 / 上游自己掐断了 tools/call
	// 连接）：标记 stale —— 之后客户端若晚到回填，不得再触发 resume（见
	// livePending），否则会在已放弃的回合上空转，客户端再多收一次"模型请求失败"。
	// 实测：上游 abort 路径此前不标 stale，挂起调用残留 live，agent 的后续
	// 重试全部撞孤儿守卫，最终认定"工具通道断了"。
	if res.TimedOut || res.Aborted {
		pend.markStale()
		s.metrics.incToolTimeouts()
		// 楔死取证：找回本轮 tool_calls 响应的落盘文件，与成功轮逐字节对比
		s.forensics.trace(pend.CallID)
	}
	if res.TimedOut {
		// 调用属于当前绑定：标记 terminated（下一轮重放保护：同内容 → 499，
		// 新内容 → 重建），并中断当前上游，避免孤儿 agent 继续烧 token。
		if sess.ownsPendingCall(pend) {
			if sess.onTimeout != nil {
				sess.onTimeout()
			}
			if sid := sess.getSessionID(); sid != "" {
				s.log.Warnf("tool result timeout: interrupting upstream %s (%s) to stop orphaned agent", sid, sess.key)
				go func() {
					iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
					defer c()
					_ = s.up.Interrupt(iCtx, sid)
				}()
			}
		} else if sid := sess.ghostInterruptSid(pend); sid != "" {
			// 幽灵调用：中断它自己所属的旧会话（孤儿 agent 的宿主）——否则
			// agent 收到超时错误后继续跑，无人观看地做完整个任务，还跟当前
			// 会话抢工具桥（实测：静默跑完 42 条消息 + idle 守卫误杀活会话）。
			// 中断目标是旧会话本身，绝不碰当前绑定的新会话。
			s.log.Warnf("stale tool call timed out: tool=%s callSid=%s current=%s (conv=%s); interrupting abandoned session %s",
				pend.ToolName, pend.CallSid, sess.getSessionID(), sess.key, sid)
			go func() {
				iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
				defer c()
				_ = s.up.Interrupt(iCtx, sid)
			}()
		} else {
			// 有 waiters：可能是并发 bucket-mate 的活跃调用，保守不中断。
			s.log.Warnf("stale tool call timed out: tool=%s callSid=%s current=%s (conv=%s); dropping (waiter active)",
				pend.ToolName, pend.CallSid, sess.getSessionID(), sess.key)
		}
	}

	// 配套状态查询工具失败时，给 agent 补一句"换名重试"提示：它列表里就有
	// 另一个名字（CheckCommandStatus/check_command_status），不用再反复搜索卡住。
	if res.IsError {
		if h := companionErrorHint(pend.ToolName); h != "" {
			res.Content += h
		}
	}

	mcpReply(w, id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": res.Content}},
		"isError": res.IsError,
	})
}

// handleUnknownMCPSession 给"未知 token"的 MCP 请求一个可恢复响应，而不是 404：
// initialize / ping / tools/list 正常返回（工具列表为空），tools/call 先尝试按
// 工具名重路由，兜底才明确报错。
// 这样上游（OpenCode 的 MCP 客户端）不会把整条连接判死，agent 也不会报"服务挂了"。
func (s *Server) handleUnknownMCPSession(w http.ResponseWriter, r *http.Request, token string, req *mcpRequest) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		ver := p.ProtocolVersion
		if ver == "" {
			ver = "2025-06-18"
		}
		w.Header().Set("Mcp-Session-Id", token)
		mcpReply(w, req.ID, map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "janus", "version": "1.0"},
		})
	case "ping":
		mcpReply(w, req.ID, map[string]any{})
	case "tools/list":
		mcpReply(w, req.ID, map[string]any{"tools": []any{}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			mcpErr(w, req.ID, -32602, "invalid tools/call params")
			return
		}
		// token 失效（会话重置/换指纹后的残留配置）但工具名内嵌的会话命名空间
		// 仍指向活会话：按名字路由到活会话正常处理。全量重放型客户端（Trae）
		// 的历史里带着旧纪元的工具名，agent 依样调用——此前一律报 stale，
		// agent 全部失败后认定"工具通道断了"并放弃任务（实测）。
		if sess, original, ok := s.tools.ResolveToolName(p.Name); ok {
			s.log.Infof("mcp stale-token call rerouted by tool name: tool=%s conv=%s", original, sess.key)
			args := string(p.Arguments)
			if args == "" || args == "null" {
				args = "{}"
			}
			s.mcpToolCall(w, r, req.ID, sess, p.Name, original, args)
			return
		}
		mcpReply(w, req.ID, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "bridge: this tool server is stale (session was reset); no tools available",
			}},
			"isError": true,
		})
	default:
		mcpErr(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func mcpReply(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
}

func mcpErr(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// mcpEndpoint 返回给 OpenCode 注册用的 URL。
func (s *Server) mcpEndpoint(token string) string {
	base := strings.TrimRight(s.cfg.MCPPublicURL, "/")
	if base == "" {
		// 默认用本机回环：MCP 是 OpenCode server 来调我们，同机即可。
		// listenAddr() 返回的是 host:port，直接拼在 "http://127.0.0.1" 后面
		// 会得到 "http://127.0.0.1127.0.0.1:2810" 这种废 URL（OpenCode 会 500）。
		base = "http://" + s.listenAddr()
	}
	return base + "/mcp/" + token
}

// listenAddr 从 BRIDGE_ADDR 推导出可直连的 host:port。
func (s *Server) listenAddr() string {
	a := s.cfg.Addr
	// 0.0.0.0 / [::] 换成回环，避免注册一个连不上的地址
	if strings.HasPrefix(a, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(a, "0.0.0.0:")
	}
	if strings.HasPrefix(a, "[::]:") {
		return "127.0.0.1:" + strings.TrimPrefix(a, "[::]:")
	}
	return a
}
