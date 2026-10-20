package api

import (
	"net/http"
	"strings"

	"janus/internal/agent"
	"janus/internal/gateway"
	"janus/internal/httpx"
)

// agent_registry.go —— Agent 面的 runtime 注册表与选择。
//
// handler 不再直接判断「走会话桥还是透传」，而是问注册表要一个 AgentRuntime。
// 注册四个 runtime：opencode（会话桥）、direct（模型面透传）、
// codex / claude-code（CLI 子进程，见 agent_cli.go）。

// agentRegistry 惰性构建并缓存 runtime 注册表。
func (s *Server) agentRegistry() *agent.Registry {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.agents == nil {
		reg := agent.NewRegistry()
		reg.Register(&opencodeRuntime{s: s})
		reg.Register(&directRuntime{s: s})
		// CLI 型 harness：一 Prompt 一子进程（见 agent_cli.go）。
		s.registerCLIRuntimes(reg)
		s.agents = reg
	}
	return s.agents
}

// agentRuntimeFor 选本次请求该走哪个 runtime：
//
//	agent key / 显式 agent=direct → direct（不建会话，直接透传）
//	其余                          → opencode（会话桥）
func (s *Server) agentRuntimeFor(r *http.Request, p Principal) agent.AgentRuntime {
	name := agent.NameOpenCode
	if s.directMode(r, p) {
		name = agent.NameDirect
	}
	if rt, ok := s.agentRegistry().Get(name); ok {
		return rt
	}
	return nil
}

// handleListRuntimes：GET /api/v1/runtimes[/resolve?model=&credential=&protocol=]
//
// 列出已注册的 harness runtime；带 model 时额外给出该模型的解析结果与
// 「该请求会走哪个 runtime」的判定，便于排查「为什么这个模型没直连/没走会话」。
func (s *Server) handleListRuntimes(w http.ResponseWriter, r *http.Request) {
	reg := s.agentRegistry()
	list := make([]map[string]any, 0, len(reg.List()))
	for _, rt := range reg.List() {
		list = append(list, map[string]any{"name": rt.Name()})
	}
	resp := map[string]any{"runtimes": list}

	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}
	protocol := strings.TrimSpace(r.URL.Query().Get("protocol"))
	if protocol == "" {
		protocol = gateway.ProtocolOpenAIChat
	}
	// 复用后端的解析逻辑：能落到目录里的模型可直连（direct），否则交会话桥。
	if s.router != nil && s.router.Store() != nil {
		res, err := s.routeDirect(Principal{}, protocol, model, strings.TrimSpace(r.URL.Query().Get("credential")))
		if err != nil {
			resp["resolve_error"] = err.Error()
			resp["runtime"] = agent.NameOpenCode
		} else {
			resp["route"] = map[string]any{
				"provider_id":   res.ProviderID,
				"credential_id": res.CredentialID,
				"upstream_id":   res.UpstreamID,
				"variant":       res.Variant,
				"rule":          res.Rule,
				"passthrough":   res.Passthrough,
			}
			resp["runtime"] = agent.NameDirect
		}
	} else {
		resp["runtime"] = agent.NameOpenCode
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
