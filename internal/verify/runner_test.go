package verify

import (
	"context"
	"strings"
	"testing"
	"time"

	"janus/internal/work"
)

func TestRunCapturesExitAndOutput(t *testing.T) {
	ctx := context.Background()
	r := NewRunner(t.TempDir())

	results, err := r.Run(ctx, []work.Check{
		{Name: "ok", Command: "echo hello", TimeoutSeconds: 5},
		{Name: "fail", Command: "exit 3", TimeoutSeconds: 5},
		{Name: "stderr", Command: "echo oops >&2; exit 1", TimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len = %d, want 3", len(results))
	}

	if results[0].ExitCode != 0 || !strings.Contains(results[0].OutputTail, "hello") {
		t.Fatalf("ok result = %+v", results[0])
	}
	if results[0].OutputSHA == "" {
		t.Fatal("missing output hash")
	}
	if results[1].ExitCode != 3 {
		t.Fatalf("fail exit = %d, want 3", results[1].ExitCode)
	}
	if results[1].Passed(3) != true || results[1].Passed(0) != false {
		t.Fatalf("Passed mismatch: %+v", results[1])
	}
	if !strings.Contains(results[2].OutputTail, "oops") {
		t.Fatalf("stderr not captured: %+v", results[2])
	}
}

func TestRunTimeout(t *testing.T) {
	ctx := context.Background()
	r := NewRunner(t.TempDir())

	started := time.Now()
	results, err := r.Run(ctx, []work.Check{
		{Name: "sleep", Command: "sleep 5", TimeoutSeconds: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("timeout did not fire, elapsed %v", elapsed)
	}
	if results[0].ExitCode != -1 {
		t.Fatalf("exit = %d, want -1", results[0].ExitCode)
	}
	if !strings.Contains(results[0].OutputTail, "timeout") {
		t.Fatalf("tail = %q, want timeout note", results[0].OutputTail)
	}
}

func TestRunRunsInWorkspace(t *testing.T) {
	root := t.TempDir()
	r := NewRunner(root)

	results, err := r.Run(context.Background(), []work.Check{
		{Name: "pwd", Command: "pwd", TimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(results[0].OutputTail, root) {
		t.Fatalf("cwd = %q, want inside %q", results[0].OutputTail, root)
	}
}

func TestRunKeepsOnlyTailButHashesEverything(t *testing.T) {
	root := t.TempDir()
	r := NewRunner(root)

	first, err := r.Run(context.Background(), []work.Check{
		{Name: "big", Command: "seq 1 5000", TimeoutSeconds: 10},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(first[0].OutputTail) > TailBytes {
		t.Fatalf("tail = %d bytes, want <= %d", len(first[0].OutputTail), TailBytes)
	}
	if !strings.HasSuffix(strings.TrimSpace(first[0].OutputTail), "5000") {
		t.Fatalf("tail lost the end of output: %q", first[0].OutputTail)
	}
	if first[0].OutputSHA == "" {
		t.Fatal("missing hash for full output")
	}

	second, err := r.Run(context.Background(), []work.Check{
		{Name: "big", Command: "seq 1 5000", TimeoutSeconds: 10},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first[0].OutputSHA != second[0].OutputSHA {
		t.Fatal("same output produced different hashes")
	}
}

func TestRunRejectsMissingRoot(t *testing.T) {
	r := NewRunner("")
	if _, err := r.Run(context.Background(), nil); err == nil {
		t.Fatal("expected error for empty root")
	}
	r = NewRunner(t.TempDir() + "/does-not-exist")
	if _, err := r.Run(context.Background(), nil); err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestRunEmptyCommand(t *testing.T) {
	r := NewRunner(t.TempDir())
	results, err := r.Run(context.Background(), []work.Check{{Name: "blank"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].ExitCode != -1 {
		t.Fatalf("exit = %d, want -1", results[0].ExitCode)
	}
}
