package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"janus/internal/agent"
	"janus/internal/upstream"
)

// turn.go —— 一轮会话的「事件来源」抽象。
//
// chat.go 的 executor 原先直接依赖 upstream.Upstream + EventBus：订阅、发
// prompt、对账（ListMessages）、判空闲（GetSession）、打断都写死在 OpenCode
// 的语义里。CLI 型 harness（codex / claude-code）的事件来自子进程 stdout，
// 没有这些上游接口，因此无法接进 /v1/chat/completions。
//
// 这里把「事件来源 + 会话状态」收敛成 turnDriver / turnSession：
//
//	turnDriver  —— 建一轮（必须先订阅再 prompt，否则丢开头增量）
//	turnSession —— 事件流、对账、状态、等待空闲、打断
//
// opencodeTurnSession 是当前唯一实现，行为与原代码逐一对应；CLI 实现只需
// 把 agent.Event 流接上，对账返回 false、空闲由事件流收尾驱动即可。

// turnSnapshot 是一次对账的结果（补齐事件流丢失的尾巴）。
type turnSnapshot struct {
	text      string
	reasoning string
	finish    string
	tokens    *upstream.OCTokens
	failure   *upstreamFailure
}

// turnSession 是一轮会话的事件来源与会话状态的组合。
type turnSession interface {
	Events() <-chan agent.Event
	Reconcile(ctx context.Context, since int64) (turnSnapshot, bool)
	State(ctx context.Context, promptAt int64, seenStarted bool) (idle bool, outcome string)
	WaitIdle(ctx context.Context) error
	Interrupt(ctx context.Context) error
	Cancel()
}

// turnDriver 负责建立一轮。
type turnDriver interface {
	Subscribe(ctx context.Context, sid string) turnSession
	Prompt(ctx context.Context, sid string, req upstream.OCPromptReq) (created int64, err error)
}

// turnDriver 返回当前会话桥使用的驱动（OpenCode）。
func (s *Server) turnDriver() turnDriver { return opencodeTurnDriver{s: s} }

// opencodeTurnDriver 把 OpenCode 的 up + EventBus 适配成 turnDriver。
type opencodeTurnDriver struct{ s *Server }

func (d opencodeTurnDriver) Subscribe(ctx context.Context, sid string) turnSession {
	ts := &opencodeTurnSession{s: d.s, sid: sid, sub: d.s.bus.Subscribe(sid, 512)}
	ts.cancelSub = ts.sub.Cancel
	ts.startForward(ctx)
	return ts
}

// adoptTurnSession 把调用方已持有的 EventBus 订阅包装成 turnSession。
// Anthropic / Responses 两条协议各自管理订阅生命周期（handler 里 defer Cancel），
// 因此这里不接管取消，只做事件转换。
func (s *Server) adoptTurnSession(ctx context.Context, sid string, sub *upstream.Subscription) turnSession {
	ts := &opencodeTurnSession{s: s, sid: sid, sub: sub}
	ts.startForward(ctx)
	return ts
}

func (d opencodeTurnDriver) Prompt(ctx context.Context, sid string, req upstream.OCPromptReq) (int64, error) {
	resp, err := d.s.up.Prompt(ctx, sid, req)
	if err != nil {
		return 0, err
	}
	if resp != nil && resp.Data.Time.Created > 0 {
		return resp.Data.Time.Created, nil
	}
	return 0, nil
}

type opencodeTurnSession struct {
	s   *Server
	sid string
	sub *upstream.Subscription
	ch  chan agent.Event

	cancelSub  func()
	cancelOnce sync.Once
}

// startForward 把上游 OCEvent 流转换成 agent.Event 流（只转一次）。
func (t *opencodeTurnSession) startForward(ctx context.Context) {
	t.ch = make(chan agent.Event, 64)
	go func() {
		defer close(t.ch)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-t.sub.Ch:
				if !ok {
					return
				}
				e, _ := ocEventToAgent(ev, t.sid)
				if e == nil {
					continue
				}
				select {
				case t.ch <- *e:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
}

func (t *opencodeTurnSession) Events() <-chan agent.Event { return t.ch }

func (t *opencodeTurnSession) Cancel() {
	t.cancelOnce.Do(func() {
		if t.cancelSub != nil {
			t.cancelSub()
		}
	})
}

func (t *opencodeTurnSession) Interrupt(ctx context.Context) error {
	return t.s.up.Interrupt(ctx, t.sid)
}

func (t *opencodeTurnSession) WaitIdle(ctx context.Context) error {
	return t.s.up.WaitUntilIdle(ctx, t.sid)
}

// State 判断本轮是否已结束，并带回上游给出的 outcome。
//
// 只看 idle 时间戳不够：上游可能以 failed 收尾（例如余额不足），此时若只报
// succeeded，客户端会以为"模型就是这么答的"，拿不到真正原因。
func (t *opencodeTurnSession) State(ctx context.Context, promptAt int64, seenStarted bool) (bool, string) {
	sess, err := t.s.up.GetSession(ctx, t.sid)
	if err != nil {
		return false, ""
	}
	outcome := sess.Outcome
	if !seenStarted {
		// 还没观测到执行开始时，仅凭 idle 判断容易误判（上一轮遗留的 idle）。
		// 此时要求 idle 晚于 prompt 至少一个 grace。
		return sess.Time.Idle >= promptAt && time.Now().UnixMilli()-promptAt > 3000, outcome
	}
	return sess.Time.Idle >= promptAt, outcome
}

// Reconcile 用消息接口补齐事件流丢失的尾巴。
func (t *opencodeTurnSession) Reconcile(ctx context.Context, since int64) (turnSnapshot, bool) {
	msgs, err := t.s.up.ListMessages(ctx, t.sid, "asc", 200)
	if err != nil {
		t.s.log.Debugf("reconcile list messages failed: %v", err)
		return turnSnapshot{}, false
	}

	var snap turnSnapshot
	var full, reasoning strings.Builder
	var tokens *upstream.OCTokens
	sawAssistant := false

	for i := range msgs {
		m := &msgs[i]
		if m.Type != "assistant" {
			continue
		}
		if m.Time.Created > 0 && m.Time.Created < since {
			continue
		}
		sawAssistant = true
		if len(m.Error) > 0 && string(m.Error) != "null" && snap.failure == nil {
			var f upstreamFailure
			if json.Unmarshal(m.Error, &f) == nil && (f.Type != "" || f.Message != "") {
				snap.failure = &f
			}
		}
		full.WriteString(m.PlainText())
		reasoning.WriteString(m.Reasoning())
		if m.Finish != "" {
			snap.finish = m.Finish
		}
		if m.Tokens != nil {
			if tokens == nil {
				tokens = &upstream.OCTokens{}
			}
			tokens.Input += m.Tokens.Input
			tokens.Output += m.Tokens.Output
			tokens.Reason += m.Tokens.Reason
			tokens.Cache.Read += m.Tokens.Cache.Read
			tokens.Cache.Write += m.Tokens.Cache.Write
		}
	}
	if !sawAssistant {
		return turnSnapshot{}, false
	}
	snap.text = full.String()
	snap.reasoning = reasoning.String()
	snap.tokens = tokens
	return snap, true
}
