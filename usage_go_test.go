package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGoUsageParse(t *testing.T) {
	raw := goUsageRaw{}
	raw.Usage.Rolling.Status, raw.Usage.Rolling.Percent = "ok", 4
	raw.Usage.Rolling.ResetsAt = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	raw.Usage.Weekly.Status, raw.Usage.Weekly.Percent = "ok", 50
	raw.Usage.Weekly.ResetsAt = time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	raw.Usage.Monthly.Status, raw.Usage.Monthly.Percent = "ok", 25
	raw.Usage.Monthly.ResetsAt = time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)

	g := raw.toUsage()
	if g == nil {
		t.Fatal("expected go usage, got nil")
	}
	if g.Rolling.Percent != 4 || g.Weekly.Percent != 50 || g.Monthly.Percent != 25 {
		t.Fatalf("percents = %+v", g)
	}
	if g.Rolling.ResetsInSec < 7100 || g.Rolling.ResetsInSec > 7200 {
		t.Fatalf("rolling resets_in_sec = %d, want ~7200", g.Rolling.ResetsInSec)
	}
}

// 非 Go 账号：端点返回空对象时不应产生一个全零的 Go 区块。
func TestGoUsageEmptyIsNil(t *testing.T) {
	if g := (goUsageRaw{}).toUsage(); g != nil {
		t.Fatalf("expected nil for empty usage, got %+v", g)
	}
}

// 真实响应里的 resetsAt 带毫秒（2026-10-04T15:07:18.000Z），必须能解析。
func TestGoUsageParsesFractionalResetsAt(t *testing.T) {
	raw := goUsageRaw{}
	raw.Usage.Rolling.Status = "ok"
	raw.Usage.Rolling.ResetsAt = "2030-10-04T15:07:18.000Z"
	g := raw.toUsage()
	if g == nil || g.Rolling.ResetsAt.IsZero() {
		t.Fatalf("fractional RFC3339 not parsed: %+v", g)
	}
}

func TestUsageClientGetURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-opencode-org-id"); got != "org_x" {
			t.Errorf("x-opencode-org-id = %q", got)
		}
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":4,"resetsAt":"2030-01-01T00:00:00.000Z"}}}`))
	}))
	defer srv.Close()

	c := &UsageClient{hc: srv.Client()}
	var raw goUsageRaw
	err := c.getURL(context.Background(), &consoleCredential{Access: "tok", OrgID: "org_x"}, srv.URL, &raw)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Usage.Rolling.Percent != 4 {
		t.Fatalf("percent = %v", raw.Usage.Rolling.Percent)
	}
}
