package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 权限自动应答：headless 桥必须替 agent 把 external_directory 等 ask 请求答掉，
// 否则工具会一直挂到客户端超时（见 /srv/data 复现）。

func permissionBus(t *testing.T, mode string, hits *permissionHits) *EventBus {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits.record(r.URL.Path, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	up := NewUpstream(Config{Upstream: srv.URL, Username: "opencode", Password: "x"}, NewLogger("error"))
	return NewEventBus(up, NewLogger("error"), mode)
}

type permissionHits struct {
	mu     sync.Mutex
	list   []string
	bodies []string
	done   chan struct{}
}

func (h *permissionHits) record(path string, body []byte) {
	h.mu.Lock()
	h.list = append(h.list, path)
	h.bodies = append(h.bodies, string(body))
	h.mu.Unlock()
	select {
	case h.done <- struct{}{}:
	default:
	}
}

func (h *permissionHits) snapshot() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.list...), append([]string(nil), h.bodies...)
}

func permissionEvent() OCEvent {
	return OCEvent{
		Type: "permission.asked",
		Data: json.RawMessage(`{"id":"per_1","sessionID":"ses_1","action":"external_directory","resources":["/srv/data/*"]}`),
	}
}

func TestPermissionAnsweredForOwnedSession(t *testing.T) {
	hits := &permissionHits{done: make(chan struct{}, 1)}
	bus := permissionBus(t, "once", hits)

	sub := bus.Subscribe("ses_1", 8)
	defer sub.cancel()

	bus.dispatch(permissionEvent())

	select {
	case <-hits.done:
	case <-time.After(5 * time.Second):
		t.Fatal("没有发出权限应答")
	}
	paths, bodies := hits.snapshot()
	wantPath := "/api/session/ses_1/permission/per_1/reply"
	if len(paths) != 1 || paths[0] != wantPath {
		t.Fatalf("reply path = %v, want [%s]", paths, wantPath)
	}
	if bodies[0] != `{"decision":"once"}` {
		t.Fatalf("reply body = %s", bodies[0])
	}
}

func TestPermissionSkippedWhenOff(t *testing.T) {
	hits := &permissionHits{done: make(chan struct{}, 1)}
	bus := permissionBus(t, "off", hits)

	sub := bus.Subscribe("ses_1", 8)
	defer sub.cancel()

	bus.dispatch(permissionEvent())

	select {
	case <-hits.done:
		t.Fatal("off 模式不应答，却发出了请求")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestPermissionSkippedForForeignSession(t *testing.T) {
	hits := &permissionHits{done: make(chan struct{}, 1)}
	bus := permissionBus(t, "once", hits)

	// 没有订阅者：不是本桥的会话
	bus.dispatch(permissionEvent())

	select {
	case <-hits.done:
		t.Fatal("非本桥会话不应答，却发出了请求")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestPermissionReplyAlways(t *testing.T) {
	hits := &permissionHits{done: make(chan struct{}, 1)}
	bus := permissionBus(t, "always", hits)
	sub := bus.Subscribe("ses_1", 8)
	defer sub.cancel()

	bus.dispatch(permissionEvent())

	select {
	case <-hits.done:
	case <-time.After(5 * time.Second):
		t.Fatal("没有发出权限应答")
	}
	_, bodies := hits.snapshot()
	if bodies[0] != `{"decision":"always"}` {
		t.Fatalf("reply body = %s", bodies[0])
	}
}

func TestPermissionAnsweredWithoutSubscriberWhenOwned(t *testing.T) {
	hits := &permissionHits{done: make(chan struct{}, 1)}
	bus := permissionBus(t, "once", hits)
	// 没有订阅者，但 Server 判定这个 session 属于本桥
	bus.setOwnedFunc(func(id string) bool { return id == "ses_1" })

	bus.dispatch(permissionEvent())

	select {
	case <-hits.done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned session 即使无订阅者也应自动应答")
	}
}

func TestNormalizePermissionReply(t *testing.T) {
	cases := map[string]string{
		"":        "once",
		"once":    "once",
		"ALWAYS":  "always",
		" reject": "reject",
		"off":     "off",
		"bogus":   "once",
	}
	for in, want := range cases {
		if got := normalizePermissionReply(in); got != want {
			t.Errorf("normalizePermissionReply(%q) = %q, want %q", in, got, want)
		}
	}
}
