package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cliproc.go —— 「一行一个 JSON 事件」的 CLI 子进程驱动。
//
// CLI 型 harness（codex / claude-code）在非交互模式下不是 HTTP server，而是
// 一次性子进程：把事件以 JSONL（每行一个 JSON 对象）写到 stdout，跑完就退出。
// 多轮靠「记住 harness 侧会话 id，然后 resume」实现，进程本身无状态。
//
// 这里只做与 harness 无关的部分：启动、按行读 stdout、结束/杀掉。每行怎么
// 翻译成 agent.Event 由各 harness 的解析器负责（见 CLISpec.Parse）。
//
// 读与等分成两个协程：cmd.Wait() 必须在读取结束前并发跑。
//   - Wait 是唯一会关闭 stdout 管道的人；若等读循环结束再 Wait，一旦有孙进程
//     继承着管道写端（codex/claude 常起子进程），读端就永远等不到 EOF，双方互等。
//   - 反过来，Wait 返回即关管道，读循环随之结束 —— 即使孙进程还活着也不会卡住。

// ProcSpec 描述要启动的一个 CLI 子进程。
type ProcSpec struct {
	Bin   string
	Args  []string
	Dir   string   // 工作目录；空=继承
	Env   []string // 追加到 os.Environ() 之后（"K=V"）
	Stdin string   // 非空则一次性写入 stdin（多数 harness 用位置参数传 prompt）
}

// LineProc 是一个正在运行的子进程，stdout 被按行切分后送到 Lines()。
//
// 生命周期：StartLineProc → 消费 Lines() 直到关闭 → Wait() 取退出结果。
// Lines() 关闭即代表「不会再有事件」。ctx 取消会杀掉子进程。
type LineProc struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	lines  chan []byte
	stop   chan struct{} // 消费方弃读时关闭，让读协程退出
	stderr *boundedBuffer

	once    sync.Once
	killed  atomic.Bool
	done    chan struct{} // cmd.Wait 已返回
	waitErr error
}

// boundedBuffer 是有上界的字节缓冲：只保留前 N 字节，防止子进程刷爆内存。
type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
	n   int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rem := b.n - len(b.buf); rem > 0 {
		take := len(p)
		if take > rem {
			take = rem
		}
		b.buf = append(b.buf, p[:take]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// StartLineProc 启动子进程并开始按行读取 stdout。
func StartLineProc(ctx context.Context, spec ProcSpec) (*LineProc, error) {
	if strings.TrimSpace(spec.Bin) == "" {
		return nil, errors.New("cli: empty binary")
	}
	cmd := exec.CommandContext(ctx, spec.Bin, spec.Args...)
	cmd.Dir = spec.Dir
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errBuf := &boundedBuffer{n: 8 << 10}
	cmd.Stderr = errBuf
	// ctx 取消 / 子进程先退出后管道仍被孙进程占着时，宽限一段时间就强制收口。
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &LineProc{
		cmd:    cmd,
		stdout: stdout,
		lines:  make(chan []byte, 256),
		stop:   make(chan struct{}),
		stderr: errBuf,
		done:   make(chan struct{}),
	}
	go p.read()
	go p.wait()
	return p, nil
}

// read 按行读 stdout，EOF 后关闭 Lines()。
func (p *LineProc) read() {
	defer close(p.lines)
	sc := bufio.NewScanner(p.stdout)
	// 事件行可能很长（整段代码/补丁），把上限放宽到 8MB。
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		select {
		case p.lines <- cp:
		case <-p.stop:
			return
		}
	}
}

// wait 等子进程退出；返回后 stdout 管道被关闭，read 随之收尾。
func (p *LineProc) wait() {
	defer close(p.done)
	p.waitErr = p.cmd.Wait()
}

// Lines 返回按行切分的 stdout；不会再有事时通道关闭。
func (p *LineProc) Lines() <-chan []byte { return p.lines }

// Stderr 返回子进程 stderr 的前若干字节（有界）。
func (p *LineProc) Stderr() string { return p.stderr.String() }

// Kill 杀掉子进程并立刻收口 stdout。可重复调用。
func (p *LineProc) Kill() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.killed.Store(true)
		close(p.stop)
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		// 主动关读端：孙进程可能仍持有写端，不关的话读协程等不到 EOF，
		// 而 cmd.Wait 又要等 WaitDelay 才肯关管道，打断就会慢一拍。
		_ = p.stdout.Close()
	})
}

// Killed 表示该进程是被 Kill 主动杀掉的（而非自己跑完/报错退出）。
// 用于把「被打断」和「执行失败」区分开。
func (p *LineProc) Killed() bool { return p != nil && p.killed.Load() }

// Wait 等子进程退出，返回退出错误（成功为 nil）。
func (p *LineProc) Wait() error {
	<-p.done
	return p.waitErr
}
