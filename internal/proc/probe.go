package proc

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// maxOutput 限制探測的 stdout 與 stderr 各保留的量。
const maxOutput = 1 << 20

const (
	// defaultGrace 是 Runner.Grace 的零值所代表的寬限期。
	defaultGrace = 2 * time.Second
	// waitDelay 是程式結束後等子孫關閉輸出的時間，之後 Wait 不再等。
	waitDelay = time.Second
	// reapTries 與 reapInterval 是 reapGroup 送 SIGKILL 的次數上限與間隔：
	// 最多約 100 毫秒。
	reapTries    = 20
	reapInterval = 5 * time.Millisecond
)

// Probe 是為得知某事而執行的短命程式：shell 的
// 環境、版本、登入狀態。
type Probe struct {
	Path    string
	Args    []string
	Env     Env
	Dir     string
	Timeout time.Duration
}

// Result 是探測印出的內容與其結束方式。Stdout 是開頭的 1 MiB
// （答案在開頭），Stderr 是結尾的 1 MiB（錯誤在結尾）。
// 無法啟動、非零結束、被訊號終止、逾時、被取消或被 Close
// 停止時設定 Err，一律是 *Error（CodeStartFailed、CodeExited、
// CodeSignaled、CodeTimedOut、CodeCanceled 或 CodeClosed）。
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int // 沒有啟動或被訊號終止時為 -1
	TimedOut bool
	Err      error
}

// Output 是 stdout 接著 stderr。
func (r Result) Output() []byte { return append(bytes.Clone(r.Stdout), r.Stderr...) }

// Runner 執行探測，每個都在獨立的 session 與行程群組中、
// 以空的 stdin 且無控制終端執行。逾時、取消或 Close 時終止
// 整個群組：先送 SIGTERM，寬限期內沒有結束再送 SIGKILL。
// 零值即可使用。
type Runner struct {
	// Grace 是 SIGTERM 之後等群組自行結束的時間；零值表示 2 秒。
	// 須在第一次 Run 之前設定。
	Grace time.Duration

	mu     sync.Mutex
	closed bool
	stop   chan struct{} // Close 時關閉；第一次使用時建立
	wg     sync.WaitGroup
}

// Run 啟動 p 並等待其結束。
func (r *Runner) Run(ctx context.Context, p Probe) Result {
	stop, ok := r.enter()
	if !ok {
		return Result{ExitCode: -1, Err: ErrClosed}
	}
	defer r.wg.Done()

	cmd := exec.Command(p.Path, p.Args...)
	cmd.Env = p.Env
	cmd.Dir = p.Dir
	stdout, stderr := &head{max: maxOutput}, &tail{max: maxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	isolate(cmd)
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return Result{ExitCode: -1, Err: fail(CodeStartFailed, err, "path", p.Path, "detail", err.Error())}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var timeout <-chan time.Time
	if p.Timeout > 0 {
		t := time.NewTimer(p.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	var why *Error
	select {
	case err := <-done:
		return exited(cmd, stdout, stderr, err)
	case <-timeout:
		why = fail(CodeTimedOut, context.DeadlineExceeded)
	case <-ctx.Done():
		why = fail(CodeCanceled, ctx.Err())
	case <-stop:
		why = ErrClosed
	}
	// 同時就緒時 select 任選其一：程式已經結束就以它的結束為準。
	select {
	case err := <-done:
		return exited(cmd, stdout, stderr, err)
	default:
	}
	terminate(cmd, done, cmp.Or(r.Grace, defaultGrace))
	res := result(cmd, stdout, stderr, why)
	res.TimedOut = why.Code == CodeTimedOut
	return res
}

// enter 登記一個探測；Close 之後回報 false。
func (r *Runner) enter() (<-chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	r.wg.Add(1)
	return r.stopLocked(), true
}

func (r *Runner) stopLocked() chan struct{} {
	if r.stop == nil {
		r.stop = make(chan struct{})
	}
	return r.stop
}

// Close 終止所有仍在執行的探測（它們回報 ErrClosed）、拒絕新的
// 探測，並等每個 Run 都回傳後才回傳：此時每個群組都已收到 SIGKILL。
// 最久約為 Grace 加 1.1 秒（等子孫關閉輸出，再收掉群組）。
func (r *Runner) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.stopLocked())
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// exited 是程式自行結束後的結果。群組裡留下的子孫是已結束探測的
// 孤兒，一併結束。
func exited(cmd *exec.Cmd, stdout *head, stderr *tail, err error) Result {
	reapGroup(cmd)
	return result(cmd, stdout, stderr, exitErr(cmd, err))
}

func result(cmd *exec.Cmd, stdout *head, stderr *tail, err error) Result {
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: cmd.ProcessState.ExitCode(), Err: err}
}

// exitErr 依結束狀態說明程式自行結束的方式；正常結束為 nil。
func exitErr(cmd *exec.Cmd, err error) error {
	if err == nil {
		return nil
	}
	if sig, ok := signaled(cmd.ProcessState); ok {
		return fail(CodeSignaled, err, "signal", strconv.Itoa(sig))
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		return fail(CodeExited, err, "status", strconv.Itoa(code))
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil // 正常結束，只是留下的子孫仍開著輸出
	}
	return fail(CodeExited, err, "status", "0")
}

// head 只保留寫入的前 max 位元組。
type head struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (h *head) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room := h.max - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (h *head) Bytes() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return bytes.Clone(h.buf)
}

// tail 只保留寫入的最後 max 位元組。緩衝區最多長到 2*max，
// 才把較舊的部分移掉，所以每個位元組平均只搬移一次。
type tail struct {
	mu    sync.Mutex
	max   int
	buf   []byte
	total int // 寫入的總量，用來判斷是否截斷過
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	t.total += n
	p = p[max(0, len(p)-t.max):]
	if len(t.buf)+len(p) > 2*t.max {
		keep := min(len(t.buf), t.max-len(p))
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-keep:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tail) Bytes() []byte {
	b, _ := t.tailBytes()
	return b
}

// tailBytes 是保留的結尾，以及前面是否有被丟掉的部分。
func (t *tail) tailBytes() ([]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.buf[max(0, len(t.buf)-t.max):]), t.total > t.max
}
