package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EventBus：单条 /api/event 长连 + 按 sessionID fan-out。
//
// OpenCode 的事件流是全站广播、无法过滤，所以必须只开一条连接，
// 在进程内按 data.sessionID 分发给各在飞请求。

type OCEvent struct {
	ID      string          `json:"id"`
	Created int64           `json:"created"`
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
}

// 常见事件的 data 载荷。字段不全时按需解码，忽略未知字段。
type evtSession struct {
	SessionID string `json:"sessionID"`
}

type evtTextDelta struct {
	SessionID          string `json:"sessionID"`
	AssistantMessageID string `json:"assistantMessageID"`
	Ordinal            int    `json:"ordinal"`
	Delta              string `json:"delta"`
	Text               string `json:"text"`
}

type evtStepEnded struct {
	SessionID          string    `json:"sessionID"`
	AssistantMessageID string    `json:"assistantMessageID"`
	Finish             string    `json:"finish"`
	RawFinish          string    `json:"rawFinish"`
	Cost               float64   `json:"cost"`
	Tokens             *OCTokens `json:"tokens"`
}

type evtToolInput struct {
	SessionID          string `json:"sessionID"`
	AssistantMessageID string `json:"assistantMessageID"`
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Text               string `json:"text"`
}

type evtUsage struct {
	SessionID string    `json:"sessionID"`
	Cost      float64   `json:"cost"`
	Tokens    *OCTokens `json:"tokens"`
}

// evtPermission 是 OpenCode 权限请求（permission.asked）的载荷。
type evtPermission struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
}

type subscription struct {
	ch     chan OCEvent
	cancel func()
}

type EventBus struct {
	up  *Upstream
	log *Logger

	// permissionReply 非空时自动应答本桥所属会话的权限请求（once/always/reject）。
	// 空或 off 表示不干预。
	permissionReply string

	// owned 判断某上游 session 是否属于本桥（由 Server 注入）。
	// 不能只依赖当前订阅者：agent 会在两次请求之间异步发起权限请求。
	owner func(string) bool

	mu           sync.Mutex
	subs         map[string][]*subscription
	activeCancel context.CancelFunc

	// generation 每次成功(重)连自增，供调用方判断是否需要强制对账。
	generation atomic.Int64
	closed     atomic.Bool
	failures   atomic.Int64

	// consumeReconnect 标记本次断开是主动切换端点，不该计入失败/退避。
	consumeReconnect atomic.Bool
}

func (b *EventBus) cancelActive() {
	b.mu.Lock()
	c := b.activeCancel
	b.mu.Unlock()
	if c != nil {
		c()
	}
}

func NewEventBus(up *Upstream, log *Logger, permissionReply string) *EventBus {
	return &EventBus{
		up:              up,
		log:             log,
		permissionReply: permissionReply,
		subs:            map[string][]*subscription{},
	}
}

func (b *EventBus) Generation() int64 { return b.generation.Load() }

// Subscribe 必须在发送 prompt 之前调用，否则会漏掉开头的 delta。
func (b *EventBus) Subscribe(sessionID string, buf int) *subscription {
	if buf <= 0 {
		buf = 256
	}
	s := &subscription{ch: make(chan OCEvent, buf)}
	b.mu.Lock()
	b.subs[sessionID] = append(b.subs[sessionID], s)
	b.mu.Unlock()

	s.cancel = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		list := b.subs[sessionID]
		for i, x := range list {
			if x == s {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		// 空桶要删掉，否则每个用过的 sessionID 都会在 map 里留一个空切片，
		// 长期运行下 map 无界增长（内存泄漏）。
		if len(list) == 0 {
			delete(b.subs, sessionID)
		} else {
			b.subs[sessionID] = list
		}
		// 关闭通道只在持锁时做一次，dispatch 同样持锁，避免 close 冲突。
		close(s.ch)
	}
	return s
}

func (b *EventBus) dispatch(ev OCEvent) {
	var d evtSession
	if json.Unmarshal(ev.Data, &d) != nil || d.SessionID == "" {
		return
	}
	// 权限请求：只处理本桥正在跟踪的会话（有订阅者），避免误答 desktop 端会话。
	if ev.Type == "permission.asked" {
		b.answerPermission(ev, d.SessionID)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs[d.SessionID] {
		select {
		case s.ch <- ev:
		default:
			// 缓冲满：丢最旧的一条再塞新的，保证拿到最新进度。
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- ev:
			default:
			}
		}
	}
}

// setOwnedFunc 注入"某个 session 是否属于本桥"的判定。
func (b *EventBus) setOwnedFunc(f func(string) bool) {
	b.mu.Lock()
	b.owner = f
	b.mu.Unlock()
}

// answerPermission 异步应答一个权限请求。
//
// headless 桥没人点“允许”，不应答的话 agent 的工具会一直挂到客户端超时。
// 只对本桥拥有的 session 生效（desktop 端的会话一律不碰）。
func (b *EventBus) answerPermission(ev OCEvent, sessionID string) {
	if b.permissionReply == "" || b.permissionReply == "off" {
		return
	}
	b.mu.Lock()
	owner := b.owner
	owned := len(b.subs[sessionID]) > 0
	b.mu.Unlock()
	if owner != nil {
		owned = owner(sessionID)
	}
	if !owned {
		return
	}
	var p evtPermission
	if json.Unmarshal(ev.Data, &p) != nil || p.ID == "" || p.SessionID == "" {
		return
	}
	decision := b.permissionReply
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := b.up.ReplyPermission(ctx, p.SessionID, p.ID, decision); err != nil {
			b.log.Warnf("permission reply failed: session=%s request=%s decision=%s: %v",
				p.SessionID, p.ID, decision, err)
			return
		}
		b.log.Infof("permission answered: session=%s request=%s decision=%s action=%s resources=%v",
			p.SessionID, p.ID, decision, p.Action, p.Resources)
	}()
}

// Run 阻塞直到 ctx 取消，内部自行断线重连。
func (b *EventBus) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := b.consume(ctx)
		if b.closed.Load() || ctx.Err() != nil {
			return
		}

		// Reconnect 请求的重连不当作失败，立即重试且不推进退避
		if b.consumeReconnect.Swap(false) {
			b.log.Infof("event stream reconnecting after endpoint change")
			continue
		}

		n := b.failures.Add(1)
		b.log.Warnf("event stream dropped: %v (failure #%d), retrying in %s", err, n, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// Reconnect 让 Run 主动断开当前 SSE 连接并立刻重连（切端点后调用）。
func (b *EventBus) Reconnect() {
	b.consumeReconnect.Store(true)
	b.cancelActive()
}

// setActiveCancel 由 consume 注册当前连接的取消函数。
func (b *EventBus) setActiveCancel(c context.CancelFunc) {
	b.mu.Lock()
	b.activeCancel = c
	b.mu.Unlock()
}

func (b *EventBus) consume(ctx context.Context) error {
	// 单独派生一个可取消的 ctx：Reconnect 时关掉它就能立刻断开 SSE
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.setActiveCancel(cancel)

	req, err := http.NewRequestWithContext(connCtx, http.MethodGet, b.up.endpoint()+"/api/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", b.up.authHeader())
	req.Header.Set("Accept", "text/event-stream")

	resp, err := b.up.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return &APIError{Status: resp.StatusCode, Msg: "event subscribe failed"}
	}

	b.generation.Add(1)
	b.failures.Store(0)
	b.log.Infof("event stream connected (%s)", resp.Header.Get("Content-Type"))

	return b.readSSE(connCtx, resp.Body)
}

func (b *EventBus) readSSE(ctx context.Context, body io.Reader) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	var data strings.Builder
	var eventType string
	flush := func() error {
		defer func() {
			data.Reset()
			eventType = ""
		}()
		raw := strings.TrimSpace(data.String())
		if raw == "" {
			return nil
		}
		var ev OCEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			b.log.Debugf("skip malformed event: %.120s", raw)
			return nil
		}
		if ev.Type == "" {
			return nil
		}
		b.dispatch(ev)
		return nil
	}

	for {
		// 心跳期间也要能响应取消
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return err
			}
			return io.EOF
		}
		line := sc.Text()

		switch {
		case strings.HasPrefix(line, ":"):
			// heartbeat 注释
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line, "data:"))
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		default:
			_ = eventType
		}
	}
}

func (b *EventBus) Close() {
	b.closed.Store(true)
}
