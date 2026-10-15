package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// usageMockDB 建一个带有效 opencode 凭据的最小 SQLite 库（readCredential 只读它）。
func usageMockDB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI 不可用")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "opencode.db")
	sql := `create table credential (integration_id text, active int, value text);
insert into credential values ('opencode', 1, '{"type":"oauth","access":"tok","expires":9999999999999,"metadata":{"orgID":"org_x"}}');`
	if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("seed db: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = os.Remove(db) })
	return db
}

// 失败负缓存：console 抖动时窗口内不重试、有旧数据就给旧的（宁旧不卡）；
// 窗口过后恢复重试，成功即清错误状态。
func TestUsageReportNegativeCache(t *testing.T) {
	oldTTL := usageErrTTL
	usageErrTTL = 80 * time.Millisecond
	t.Cleanup(func() { usageErrTTL = oldTTL })

	db := usageMockDB(t)
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			// 模拟 console 挂起：不回包等客户端超时太慢，用 500 等价验证失败路径
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/billing/status":
			w.Write([]byte(`{"mode":"pay-as-you-go","balanceMicroCents":"100000000"}`))
		case "/usage/summary":
			w.Write([]byte(`{"totalRequests":"42","totalCostMicroCents":"700"}`))
		case "/usage/models":
			w.Write([]byte(`{"items":[]}`))
		case "/usage/cost-by-day":
			w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewUsageClient(Config{
		ConsoleURL: srv.URL,
		UsageTTL:   1 * time.Millisecond, // 立刻过期，强制每次走 fetch
		OpencodeDB: db,
	}, NewLogger("error"))

	ctx := context.Background()

	// 第一次：成功并进缓存
	r1, err := c.Report(ctx)
	if err != nil {
		t.Fatalf("first report: %v", err)
	}
	if r1.Totals.Requests != 42 {
		t.Fatalf("first report totals = %+v", r1.Totals)
	}

	// console 开始失败：失败时返回旧数据（宁旧不卡），不报错
	healthy.Store(false)
	r2, err := c.Report(ctx)
	if err != nil {
		t.Fatalf("失败时已有旧数据，应返回旧数据而非报错: %v", err)
	}
	if r2.Totals.Requests != 42 {
		t.Fatalf("stale report = %+v", r2.Totals)
	}

	// 负缓存窗口内：不重试（失败计数不再增长），继续给旧数据
	r3, err := c.Report(ctx)
	if err != nil || r3.Totals.Requests != 42 {
		t.Fatalf("窗口内应给旧数据: %v %+v", err, r3)
	}

	// 窗口过后恢复重试：仍然失败 → 有旧数据继续给旧的
	time.Sleep(120 * time.Millisecond)
	r4, err := c.Report(ctx)
	if err != nil || r4 == nil {
		t.Fatalf("窗口过后重试失败时也应给旧数据: %v", err)
	}

	// 恢复成功：错误状态清空，数据更新
	healthy.Store(true)
	r5, err := c.Report(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if r5.FetchedAt.Before(r1.FetchedAt) {
		t.Error("恢复后应拿到新数据")
	}

	// 从未成功过且失败：报错（无旧数据可给）
	c2 := NewUsageClient(Config{
		ConsoleURL: "http://127.0.0.1:1", // 连接必失败
		UsageTTL:   1 * time.Millisecond,
		OpencodeDB: db,
	}, NewLogger("error"))
	if _, err := c2.Report(ctx); err == nil {
		t.Fatal("无旧数据时失败应报错")
	}
}
