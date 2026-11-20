package proc

import (
	"bytes"
	"cmp"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// stderrTail 是 agent 行程保留的 stderr 結尾：崩潰時附在 exited 事實裡，
// 會寫進事件，所以只留最後 8 KiB。
const stderrTail = 8 << 10

// Command 是要常駐執行的 agent 程式。stdin 與 stdout 只給協議用。
type Command struct {
	Path string
	Args []string
	Env  Env
	Dir  string
}

// Process 是執行中的 agent 行程，在獨立的 session 與行程群組中、無控制
// 終端。Stdin 寫給它，Stdout 讀它的協議輸出；Done 關閉後 Exit 是結束
// 原因、StderrTail 是 stderr 的結尾。行程自行結束時，群組裡留下的子孫
// 一併結束。
type Process struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *tail

	stop     chan struct{} // Stop 時關閉
	stopOnce sync.Once
	done     chan struct{}
	exit     *Error // done 關閉前寫入
}

// Start 啟動 agent 行程；Runner Close 之後回 ErrClosed。Runner.Close
// 會結束仍在執行的 agent 行程並等它們結束。
func (r *Runner) Start(c Command) (*Process, error) {
	stop, ok := r.enter()
	if !ok {
		return nil, ErrClosed
	}
	p, err := start(c)
	if err != nil {
		r.wg.Done()
		return nil, err
	}
	go func() {
		defer r.wg.Done()
		p.watch(stop, cmp.Or(r.Grace, defaultGrace))
	}()
	return p, nil
}

func start(c Command) (*Process, error) {
	failed := func(err error) error {
		return fail(CodeStartFailed, err, "path", c.Path, "detail", err.Error())
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, failed(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, failed(err)
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	errTail := &tail{max: stderrTail}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errTail
	isolate(cmd)
	cmd.WaitDelay = waitDelay
	err = cmd.Start()
	// 子行程有自己的一份；這邊只留 stdin 的寫端與 stdout 的讀端，
	// 行程與它的子孫都結束時才讀得到 EOF。
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, failed(err)
	}
	return &Process{cmd: cmd, stdin: inW, stdout: outR, stderr: errTail,
		stop: make(chan struct{}), done: make(chan struct{})}, nil
}

// watch 等行程結束，或在 Stop、Runner.Close 時結束它的群組。
func (p *Process) watch(runnerStop <-chan struct{}, grace time.Duration) {
	exited := make(chan error) // 只關閉，terminate 讀到關閉即知已結束
	var waitErr error
	go func() {
		waitErr = p.cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-p.stop:
		p.terminate(exited, grace)
	case <-runnerStop:
		p.terminate(exited, grace)
	}
	<-exited
	reapGroup(p.cmd) // 群組裡留下的子孫
	p.stdin.Close()
	p.exit = agentExit(p.cmd, waitErr)
	close(p.done)
}

func (p *Process) terminate(exited chan error, grace time.Duration) {
	p.stdin.Close()
	terminate(p.cmd, exited, grace)
}

// agentExit 說明行程如何結束；agent 行程正常結束也是一個原因（status 0）。
func agentExit(cmd *exec.Cmd, err error) *Error {
	ps := cmd.ProcessState
	if ps == nil {
		return fail(CodeExited, err, "status", "-1")
	}
	if sig, ok := signaled(ps); ok {
		return fail(CodeSignaled, err, "signal", strconv.Itoa(sig))
	}
	return fail(CodeExited, err, "status", strconv.Itoa(ps.ExitCode()))
}

// Stdin 是行程的 stdin；行程結束或 Stop 後寫入會失敗。
func (p *Process) Stdin() io.Writer { return p.stdin }

// Stdout 是行程的協議輸出；行程與它的子孫都結束後讀到 EOF。
// 讀完的一方以 CloseStdout 釋放。
func (p *Process) Stdout() io.Reader { return p.stdout }

// CloseStdout 釋放 stdout 的讀端；之後的讀取失敗。
func (p *Process) CloseStdout() error { return p.stdout.Close() }

// Done 在行程結束（群組也已結束）後關閉。
func (p *Process) Done() <-chan struct{} { return p.done }

// Exit 是結束原因（CodeExited 帶 status、CodeSignaled 帶 signal）；
// Done 關閉之前是 nil。
func (p *Process) Exit() *Error {
	select {
	case <-p.done:
		return p.exit
	default:
		return nil
	}
}

// StderrTail 是 stderr 的最後 8 KiB：截斷時從一行的開頭算起，不是
// UTF-8 的位元組換成 U+FFFD，去掉結尾的空白。
func (p *Process) StderrTail() string {
	b, cut := p.stderr.tailBytes()
	if cut {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return strings.TrimRight(strings.ToValidUTF8(string(b), "\uFFFD"), " \t\r\n")
}

// Stop 結束行程：關 stdin、向整個群組送 SIGTERM，寬限期內沒結束就
// SIGKILL；行程結束後才返回。可以重複呼叫，也可以在行程自行結束後呼叫。
func (p *Process) Stop() {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
}
