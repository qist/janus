package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDBStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.db")

	d, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	d.putResponse("resp_1", "resp:k", time.Now().Add(time.Minute).Unix(), []byte(`{"id":"resp_1"}`))
	d.saveConv(dbConversation{Key: "resp:k", SessionID: "ses_1", ProviderID: "p", ModelID: "m"})
	d.close()

	// 重新打开（模拟进程重启）
	d2, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.close()

	payload, ck, _, ok := d2.getResponse("resp_1")
	if !ok || ck != "resp:k" || string(payload) != `{"id":"resp_1"}` {
		t.Fatalf("getResponse: payload=%s convKey=%s ok=%v", payload, ck, ok)
	}
	row, ok := d2.loadConv("resp:k")
	if !ok || row.SessionID != "ses_1" || row.ProviderID != "p" {
		t.Fatalf("loadConv: %+v ok=%v", row, ok)
	}
	if !d2.deleteResponse("resp_1") {
		t.Error("deleteResponse 应返回 true")
	}
	d2.deleteConv("resp:k")
	if _, _, _, ok := d2.getResponse("resp_1"); ok {
		t.Error("删除后不应再查到")
	}
}

// 持久化治理：历史超过上限时只落会话映射，不落历史，避免库无界增长。
func TestPersistConvCapsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.db")
	stub, _, _ := stubChainUpstream(t)
	cfg := Config{
		Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		DefaultModel: "p/m1", ResponsesEnabled: true, ResponseTTL: time.Minute,
		MaxBodyBytes: 1 << 20, DBPath: path, HistoryMaxBytes: 64,
	}
	srv := NewServer(cfg, NewLogger("error"))

	big := srv.store.AcquireKey("x:big")
	big.setSessionID("ses_big")
	big.setLast([]ChatMessage{{Role: "user", Content: MessageContent{Text: strings.Repeat("x", 500)}}})
	srv.persistConv(big)
	srv.store.Release(big)

	row, ok := srv.db.loadConv("x:big")
	if !ok || row.SessionID != "ses_big" {
		t.Fatalf("会话映射应保留: %+v ok=%v", row, ok)
	}
	if len(row.History) != 0 {
		t.Fatalf("超大历史不该落库，实际 %dB", len(row.History))
	}

	small := srv.store.AcquireKey("x:small")
	small.setSessionID("ses_small")
	small.setLast([]ChatMessage{{Role: "user", Content: MessageContent{Text: "hi"}}})
	srv.persistConv(small)
	srv.store.Release(small)
	row2, _ := srv.db.loadConv("x:small")
	if len(row2.History) == 0 {
		t.Fatal("小历史应落库")
	}
}

func chainServerDB(t *testing.T, stub *httptest.Server, dbPath string) *Server {
	t.Helper()
	cfg := Config{
		Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		DefaultModel: "p/m1", ResponsesEnabled: true, ResponseTTL: time.Minute,
		MaxBodyBytes: 1 << 20, ToolAnnotations: false, AnthropicEnabled: true,
		IdlePollInterval: 200 * time.Millisecond, ReconcileInterval: 300 * time.Millisecond,
		DBPath: dbPath,
	}
	srv := NewServer(cfg, NewLogger("error"))
	if srv.db == nil {
		t.Fatal("DBPath 已配置，db 不应为 nil")
	}
	return srv
}

// Chat 也让跨重启续接闭环：持久化"会话键 → sessionID + 历史快照"，
// 重启后带完整历史的下一轮仍前缀命中、复用同一个 OpenCode session。
func TestChatHistorySurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖空闲判定")
	}
	path := filepath.Join(t.TempDir(), "janus.db")
	stub, sessions, _ := stubChainUpstream(t)

	srv1 := chainServerDB(t, stub, path)
	rec1 := postChatWithHeader(t, srv1, `{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"}]}`, "X-Session-ID", "S")
	if rec1.Code != 200 {
		t.Fatalf("r1 status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	if n := atomic.LoadInt32(sessions); n != 1 {
		t.Fatalf("第一轮应建 1 个 session，实际 %d", n)
	}

	// 重启进程
	srv2 := chainServerDB(t, stub, path)
	// 第二轮带完整历史（含上一轮 assistant），应命中恢复出的会话
	body2 := `{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`
	rec2 := postChatWithHeader(t, srv2, body2, "X-Session-ID", "S")
	if rec2.Code != 200 {
		t.Fatalf("r2 status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if n := atomic.LoadInt32(sessions); n != 1 {
		t.Fatalf("重启后应复用同一 session，实际创建了 %d 个", n)
	}
}

func postChatWithHeader(t *testing.T, srv *Server, body, hk, hv string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hk, hv)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// 关键：#3 的目标——进程重启后，用上一轮的 previous_response_id 仍能续上同一个
// OpenCode session（响应与会话映射都从 SQLite 恢复）。
func TestResponsesChainSurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖空闲判定")
	}
	path := filepath.Join(t.TempDir(), "janus.db")
	stub, sessions, _ := stubChainUpstream(t)

	srv1 := chainServerDB(t, stub, path)
	rec1, r1 := postResponse(t, srv1, `{"model":"default","input":"第一句","store":true}`)
	if rec1.Code != 200 {
		t.Fatalf("r1 status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	if n := atomic.LoadInt32(sessions); n != 1 {
		t.Fatalf("第一轮应建 1 个 session，实际 %d", n)
	}

	// 模拟进程重启：全新 Server，同一个 DB 文件
	srv2 := chainServerDB(t, stub, path)
	rec2, r2 := postResponse(t, srv2, fmt.Sprintf(`{"model":"default","input":"第二句","store":true,"previous_response_id":%q}`, r1.ID))
	if rec2.Code != 200 {
		t.Fatalf("r2 status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if n := atomic.LoadInt32(sessions); n != 1 {
		t.Fatalf("重启后应复用同一 session，实际创建了 %d 个", n)
	}
	if r2.PreviousResponseID == nil || *r2.PreviousResponseID != r1.ID {
		t.Fatalf("链断了: %v", r2.PreviousResponseID)
	}
}
