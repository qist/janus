package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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

// aggregateMonths 把按天数据按自然月聚合（月份升序）：本月/上月分开算的依据。
func TestAggregateMonths(t *testing.T) {
	days := []DayUsage{
		{Date: "2026-11-02", Requests: 10, Tokens: 2000, CostUSD: 1.5},
		{Date: "2026-11-01", Requests: 5, Tokens: 1000, CostUSD: 0.5},
		{Date: "2026-10-31", Requests: 7, Tokens: 700, CostUSD: 0.7},
		{Date: "2026-10-03", Requests: 3, Tokens: 300, CostUSD: 0.3},
		{Date: "bad", Requests: 99, Tokens: 99, CostUSD: 9}, // 非法日期应跳过
	}
	got := aggregateMonths(days)
	want := []MonthUsage{
		{Month: "2026-10", Requests: 10, Tokens: 1000, CostUSD: 1.0},
		{Month: "2026-11", Requests: 15, Tokens: 3000, CostUSD: 2.0},
	}
	if len(got) != len(want) {
		t.Fatalf("months = %+v, want %d entries", got, len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("months[%d] = %+v, want %+v", i, got[i], w)
		}
	}
	if len(aggregateMonths(nil)) != 0 {
		t.Error("空输入应返回空切片")
	}
}

// 计费周期锚定：Go 套餐 monthly 重置时间不是自然月 1 号时，本期/上期按
// 重置日对齐（从重置额度开始算），周期模型用量按 from/to 区间拉取。
func TestUsageReportPeriodAnchor(t *testing.T) {
	db := usageMockDB(t)
	var mu sync.Mutex
	var modelQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/billing/status":
			w.Write([]byte(`{"mode":"pay-as-you-go","balanceMicroCents":"100000000"}`))
		case "/usage/summary":
			w.Write([]byte(`{"totalRequests":"1","totalCostMicroCents":"1"}`))
		case "/usage/models":
			mu.Lock()
			modelQueries = append(modelQueries, r.URL.RawQuery)
			mu.Unlock()
			w.Write([]byte(`{"items":[{"model":"m","provider":"opencode-go","totalRequests":"7","totalCostMicroCents":"100"}]}`))
		case "/usage/cost-by-day":
			w.Write([]byte(`[]`))
		case "/go/usage":
			next := time.Now().Add(10 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
			w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":1,"resetsAt":"` + next + `"},` +
				`"monthly":{"status":"ok","percent":2,"resetsAt":"` + next + `"}}}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewUsageClient(Config{
		ConsoleURL: srv.URL,
		GoUsageURL: srv.URL + "/go/usage",
		UsageTTL:   time.Hour,
		OpencodeDB: db,
	}, NewLogger("error"))
	rep, err := c.Report(context.Background())
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Period == nil || !rep.Period.Anchored {
		t.Fatalf("period = %+v, want anchored", rep.Period)
	}
	if rep.Period.End != rep.Period.ResetsAt[:10] {
		t.Errorf("period.End = %s, want %s", rep.Period.End, rep.Period.ResetsAt[:10])
	}
	if rep.Period.PrevEnd == "" || rep.Period.PrevEnd >= rep.Period.Start {
		t.Errorf("prev_end = %q, want 早于 start %q（边界日不重复计数）", rep.Period.PrevEnd, rep.Period.Start)
	}
	if rep.PeriodModels == nil || len(rep.PeriodModels.Current) != 1 || rep.PeriodModels.Current[0].Requests != 7 {
		t.Fatalf("period_models.current = %+v", rep.PeriodModels)
	}
	mu.Lock()
	defer mu.Unlock()
	// /usage/models 共 3 次：全历史 1 次（无 query）+ 周期区间 2 次（并发，顺序不保证）
	var periodQs []string
	for _, q := range modelQueries {
		if q != "" {
			periodQs = append(periodQs, q)
		}
	}
	if len(periodQs) != 2 {
		t.Fatalf("period model fetches = %d (%v), want 2", len(periodQs), modelQueries)
	}
	wantCur := "from=" + rep.Period.Start + "&to=" + rep.Period.End
	wantPrev := "from=" + rep.Period.PrevStart + "&to=" + rep.Period.PrevEnd
	if (periodQs[0] != wantCur || periodQs[1] != wantPrev) &&
		(periodQs[0] != wantPrev || periodQs[1] != wantCur) {
		t.Errorf("queries = %v, want {%s, %s}", periodQs, wantCur, wantPrev)
	}
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
