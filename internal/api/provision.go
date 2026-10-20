package api

import (
	"janus/internal/agent"
	"janus/internal/channel/claudecode"
	"janus/internal/channel/codex"
	"janus/internal/channel/opencode"
)

// provision.go —— harness 无关的「接入配置」分发。
//
// 每个 harness 在各自的 channel 包里实现 agent.Harness；这里只做注册与选择。
// handler 不再对 kind 做 if/switch 拼配置（旧代码只认识 opencode）。

// harnessRegistry 惰性构建 provisioner 注册表。
func (s *Server) harnessRegistry() *agent.HarnessRegistry {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.harnesses == nil {
		reg := agent.NewHarnessRegistry()
		reg.Register(opencode.NewHarness(MCPNamePrefix))
		reg.Register(codex.Harness{})
		reg.Register(claudecode.Harness{})
		s.harnesses = reg
	}
	return s.harnesses
}

// harnessFor 按 agents.kind 取 provisioner。
func (s *Server) harnessFor(kind string) (agent.Harness, bool) {
	return s.harnessRegistry().Get(kind)
}

// toolMode 把旧的 (agent, tool_calling) 两个开关映射成工具执行模式（README 的 A/B/C）。
//
//	agent=build 且 tool_calling=false        → native（旧 A：内置工具在 janus 主机跑）
//	tool_calling=false                       → none（旧 C：无工具）
//	其余（含 orchestrator + true）            → bridge（旧 B：客户端工具桥）
func (s *Server) toolMode() agent.ToolMode {
	switch {
	case s.cfg.Agent == "build" && !s.cfg.ToolCalling:
		return agent.ToolModeNative
	case !s.cfg.ToolCalling:
		return agent.ToolModeNone
	default:
		return agent.ToolModeBridge
	}
}
