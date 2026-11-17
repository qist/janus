// Package verify executes the machine-verifiable checks declared in a work
// contract. It is deliberately dumb: run the command, capture the exit code
// and output, and let the independent verifier judge. It is not a sandbox;
// production runs must wrap it in an isolated workspace (see REFACTOR §11).
package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"janus/internal/work"
)

const (
	DefaultTimeout = 300 * time.Second
	// TailBytes is how much trailing output is stored with the result. The
	// full stream is hashed, so the stored tail can still be verified against
	// OutputSHA without keeping megabytes in the database.
	TailBytes = 2000
)

// Runner executes contract checks inside one workspace.
type Runner struct {
	Root  string
	Shell string
	Env   []string
}

// NewRunner returns a runner rooted at the workspace path.
func NewRunner(root string) *Runner {
	return &Runner{Root: root, Shell: "sh"}
}

var _ work.CheckRunner = (*Runner)(nil)

// Run executes every check in order and returns one result per check. It only
// fails when the runner itself is misconfigured; a failing command is data,
// not an error.
func (r *Runner) Run(ctx context.Context, checks []work.Check) ([]work.CheckResult, error) {
	if strings.TrimSpace(r.Root) == "" {
		return nil, errors.New("workspace root required")
	}
	info, err := os.Stat(r.Root)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", r.Root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %q is not a directory", r.Root)
	}

	results := make([]work.CheckResult, 0, len(checks))
	for _, check := range checks {
		results = append(results, r.runOne(ctx, check))
	}
	return results, nil
}

func (r *Runner) runOne(ctx context.Context, check work.Check) work.CheckResult {
	result := work.CheckResult{
		Name:     strings.TrimSpace(check.Name),
		Command:  check.Command,
		ExitCode: -1,
	}
	if strings.TrimSpace(check.Command) == "" {
		result.OutputTail = "empty command"
		return result
	}

	timeout := time.Duration(check.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := r.Shell
	if shell == "" {
		shell = "sh"
	}

	var output cappedBuffer
	output.reset()

	cmd := exec.CommandContext(runCtx, shell, "-c", check.Command)
	cmd.Dir = r.Root
	cmd.Env = r.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// A shell may leave grandchildren (sleep, test runners) holding the output
	// pipes after the shell itself is killed. WaitDelay bounds how long Run
	// blocks on them once the deadline fires.
	cmd.WaitDelay = time.Second
	cmd.Stdout = &output
	cmd.Stderr = &output

	start := time.Now()
	err := cmd.Run()
	result.DurationMS = time.Since(start).Milliseconds()
	result.OutputSHA = output.sum()
	result.OutputTail = output.tailText()

	switch {
	case err == nil:
		result.ExitCode = 0
	case runCtx.Err() == context.DeadlineExceeded:
		result.ExitCode = -1
		result.OutputTail = truncate("[timeout after "+timeout.String()+"] "+result.OutputTail, TailBytes)
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			break
		}
		result.ExitCode = -1
		result.OutputTail = truncate(err.Error()+": "+result.OutputTail, TailBytes)
	}
	return result
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[len(s)-limit:]
}

// cappedBuffer hashes the full stream and keeps only the trailing TailBytes.
// stdout and stderr are copied by separate goroutines, so writes need a lock.
type cappedBuffer struct {
	mu      sync.Mutex
	hasher  hash.Hash
	tailBuf []byte
}

func (b *cappedBuffer) reset() {
	b.hasher = sha256.New()
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	_, _ = b.hasher.Write(p)
	b.tailBuf = append(b.tailBuf, p...)
	if len(b.tailBuf) > TailBytes {
		b.tailBuf = append([]byte(nil), b.tailBuf[len(b.tailBuf)-TailBytes:]...)
	}
	return len(p), nil
}

func (b *cappedBuffer) sum() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return hex.EncodeToString(b.hasher.Sum(nil))
}

func (b *cappedBuffer) tailText() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(string(bytes.TrimRight(b.tailBuf, "\x00")), "")
}
