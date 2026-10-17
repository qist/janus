package discover

import (
	"context"
	"testing"
	"time"

	"janus/internal/upstream"
)

func TestIsOpenCodeServe(t *testing.T) {
	yes := [][]string{
		{"/root/.opencode/bin/opencode", "--log-level", "warn", "serve", "--hostname", "0.0.0.0", "--port", "2809"},
		{"opencode", "serve"},
	}
	no := [][]string{
		{"/usr/bin/bash", "-c", "opencode serve"},        // 不是进程本体
		{"/root/.opencode/bin/opencode", "run", "hello"}, // 没有 serve
		{},
	}
	for _, a := range yes {
		if !IsOpenCodeServe(a) {
			t.Errorf("should detect serve: %v", a)
		}
	}
	for _, a := range no {
		if IsOpenCodeServe(a) {
			t.Errorf("should NOT detect serve: %v", a)
		}
	}
}

func TestFlagValue(t *testing.T) {
	args := []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "2809"}
	if got := FlagValue(args, "--port"); got != "2809" {
		t.Errorf("port = %q", got)
	}
	if got := FlagValue(args, "--hostname"); got != "0.0.0.0" {
		t.Errorf("hostname = %q", got)
	}
	if got := FlagValue(args, "--missing"); got != "" {
		t.Errorf("missing flag = %q", got)
	}
	// --key=value 形式
	if got := FlagValue([]string{"opencode", "serve", "--port=1234"}, "--port"); got != "1234" {
		t.Errorf("inline form = %q", got)
	}
}

// 真实环境里至少应能看到当前跑着的 opencode serve（本机自测）。
func TestScanProcFindsRunningOpenCode(t *testing.T) {
	cands, err := scanProc()
	if err != nil {
		t.Skipf("cannot scan /proc: %v", err)
	}
	if len(cands) == 0 {
		t.Skip("no opencode serve process running; skipping")
	}
	for _, c := range cands {
		if c.Base == "" {
			t.Errorf("bad candidate: %+v", c)
		}
	}
}

func TestDiscoverAgainstLiveServer(t *testing.T) {
	cands, err := scanProc()
	if err != nil || len(cands) == 0 {
		t.Skip("no opencode serve process running; skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ep, err := Discover(ctx, upstream.ProbeEndpoint)
	if err != nil {
		t.Fatalf("discover failed: %v", err)
	}
	if ep.Pass == "" {
		t.Error("discovered endpoint has empty password")
	}
	t.Logf("discovered %s (pid %d)", ep.Base, ep.PID)
}
