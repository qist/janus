package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"janus/internal/agent"
	"janus/internal/upstream"
)

// agent_opencode.go —— opencode harness 的 AgentRuntime 适配器。
//
// 这是「薄适配」：会话/流式/工具/权限的重活仍由 OpenCode server 承担
// （internal/api/chat.go 里的会话桥逻辑不变），这里只是把 janus 侧已经用到的
// upstream + EventBus 原语收敛成 AgentRuntime 接口，让上层不再直接依赖 harness 细节。

type opencodeRuntime struct{ s *Server }

func (o *opencodeRuntime) Name() string { return agent.NameOpenCode }

// EnsureReady 确保上游 OpenCode 端点可达。
func (o *opencodeRuntime) EnsureReady(ctx context.Context) error {
	if o.s.up == nil {
		return errors.New("opencode upstream is not configured")
	}
	return o.s.up.Ping(ctx)
}

// CreateSession 在上游 OpenCode 建一个会话（可选带 agent / 目录 / 初始模型）。
func (o *opencodeRuntime) CreateSession(ctx context.Context, spec agent.SessionSpec) (string, error) {
	req := upstream.CreateSessionReq{}
	if spec.Key != "" {
		req.Title = spec.Key
	}
	if spec.Agent != "" {
		req.Agent = spec.Agent
	}
	if spec.Directory != "" {
		req.Location = &struct {
			Directory string `json:"directory"`
		}{Directory: spec.Directory}
	}
	if ref, ok := parseOCModelRef(spec.Model); ok {
		req.Model = &ref
	}
	sess, err := o.s.up.CreateSession(ctx, req)
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// Prompt 提交一轮输入并返回事件流；终止事件（succeeded/failed/interrupted）后关闭通道。
//
// 必须先订阅再发 prompt，否则会漏掉开头的增量（与 chat.go 的做法一致）。
func (o *opencodeRuntime) Prompt(ctx context.Context, sid string, req agent.PromptReq) (<-chan agent.Event, error) {
	if strings.TrimSpace(sid) == "" {
		return nil, errors.New("session id is required")
	}
	if ref, ok := parseOCModelRef(req.Model); ok {
		if err := o.s.up.SetModel(ctx, sid, ref); err != nil {
			return nil, err
		}
	}
	sub := o.s.bus.Subscribe(sid, 512)
	if _, err := o.s.up.Prompt(ctx, sid, upstream.OCPromptReq{Text: lastUserText(req.Messages)}); err != nil {
		sub.Cancel()
		return nil, err
	}

	out := make(chan agent.Event, 64)
	go func() {
		defer close(out)
		defer sub.Cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub.Ch:
				if !ok {
					return
				}
				e, terminal := ocEventToAgent(ev, sid)
				if e != nil {
					out <- *e
				}
				if terminal {
					return
				}
			}
		}
	}()
	return out, nil
}

func (o *opencodeRuntime) SetModel(ctx context.Context, sid, model string) error {
	ref, ok := parseOCModelRef(model)
	if !ok {
		return errors.New("model must be \"provider/model\"")
	}
	return o.s.up.SetModel(ctx, sid, ref)
}

func (o *opencodeRuntime) Interrupt(ctx context.Context, sid string) error {
	return o.s.up.Interrupt(ctx, sid)
}

// Close 是 no-op：进程生命周期由渠道监督器（channel/opencode）统一管理。
func (o *opencodeRuntime) Close(context.Context) error { return nil }

// parseOCModelRef 解析 "provider/model[:variant]" 为上游模型引用。
func parseOCModelRef(raw string) (upstream.OCModelRef, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return upstream.OCModelRef{}, false
	}
	variant := ""
	if i := strings.LastIndex(raw, ":"); i > 0 {
		// 仅当冒号后不是空且不含 "/" 时当变体（避免误切 URL 之类的形态）
		if v := raw[i+1:]; v != "" && !strings.Contains(v, "/") {
			variant, raw = v, raw[:i]
		}
	}
	i := strings.Index(raw, "/")
	if i <= 0 || i >= len(raw)-1 {
		return upstream.OCModelRef{}, false
	}
	return upstream.OCModelRef{
		ProviderID: raw[:i],
		ID:         raw[i+1:],
		Variant:    variant,
	}, true
}

// lastUserText 取历史里最后一条 user 消息的内容。
func lastUserText(msgs []agent.Message) string {
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

// ocEventToAgent 把一个上游事件映射成 Agent 事件；terminal 表示事件流可以收尾。
func ocEventToAgent(ev upstream.OCEvent, sid string) (*agent.Event, bool) {
	var sess upstream.EvtSession
	if json.Unmarshal(ev.Data, &sess) != nil || sess.SessionID != sid {
		return nil, false
	}
	switch ev.Type {
	case "session.text.delta":
		var d upstream.EvtTextDelta
		if json.Unmarshal(ev.Data, &d) == nil && d.Delta != "" {
			return &agent.Event{Type: agent.EventText, Text: d.Delta}, false
		}
	case "session.reasoning.delta":
		var d upstream.EvtTextDelta
		if json.Unmarshal(ev.Data, &d) == nil && d.Delta != "" {
			return &agent.Event{Type: agent.EventReasoning, Text: d.Delta}, false
		}
	case "session.tool.input.ended":
		var d upstream.EvtToolInput
		if json.Unmarshal(ev.Data, &d) == nil && d.Name != "" {
			var args json.RawMessage
			if strings.TrimSpace(d.Text) != "" {
				args = json.RawMessage(d.Text)
			}
			return &agent.Event{Type: agent.EventToolCall,
				ToolCall: &agent.ToolCall{ID: d.ID, Name: d.Name, Arguments: args}}, false
		}
	case "session.tool.success", "session.tool.failed":
		var d upstream.EvtToolResult
		if json.Unmarshal(ev.Data, &d) == nil {
			text := ev.Type
			for _, c := range d.Content {
				if c.Text != "" {
					text = c.Text
				}
			}
			if d.Error != "" {
				text = d.Error
			}
			return &agent.Event{Type: agent.EventToolResult, Text: text,
				ToolCall: &agent.ToolCall{ID: d.ID}}, false
		}
	case "session.step.ended":
		var d upstream.EvtStepEnded
		if json.Unmarshal(ev.Data, &d) == nil {
			u := &agent.Usage{CostUSD: d.Cost}
			if d.Tokens != nil {
				u.InputTokens = int64(d.Tokens.Input)
				u.OutputTokens = int64(d.Tokens.Output)
			}
			return &agent.Event{Type: agent.EventUsage, Usage: u, Finish: d.Finish}, false
		}
	case "session.execution.succeeded":
		return &agent.Event{Type: agent.EventDone, Finish: "stop"}, true
	case "session.execution.failed":
		return &agent.Event{Type: agent.EventError, Error: "upstream execution failed"}, true
	case "session.execution.interrupted":
		return &agent.Event{Type: agent.EventDone, Finish: "interrupted"}, true
	}
	return nil, false
}
