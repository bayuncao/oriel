//go:build unix

package proc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRunErrors 確認 Run 的每種失敗都帶代碼，原因仍可用 errors.Is 取得。
func TestRunErrors(t *testing.T) {
	root := tempRoot(t)
	exit3 := filepath.Join(root, "exit3")
	script(t, exit3, "exit 3")
	killed := filepath.Join(root, "killed")
	script(t, killed, "kill -9 $$")
	slow := filepath.Join(root, "slow")
	script(t, slow, "sleep 60")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	var r Runner
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		probe  Probe
		code   Code
		params map[string]string
		cause  error
	}{
		{"start", context.Background(), Probe{Path: filepath.Join(root, "missing")}, CodeStartFailed, nil, nil},
		{"exit", context.Background(), Probe{Path: exit3}, CodeExited, map[string]string{"status": "3"}, nil},
		{"signal", context.Background(), Probe{Path: killed}, CodeSignaled, map[string]string{"signal": "9"}, nil},
		{"timeout", context.Background(), Probe{Path: slow, Timeout: time.Second}, CodeTimedOut, nil, context.DeadlineExceeded},
		{"cancel", cancelled, Probe{Path: slow}, CodeCanceled, nil, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := r.Run(tc.ctx, tc.probe)
			e, ok := errors.AsType[*Error](res.Err)
			if !ok || e.Code != tc.code {
				t.Fatalf("got %v", res.Err)
			}
			for k, v := range tc.params {
				if e.Params[k] != v {
					t.Errorf("param %s = %q, want %q", k, e.Params[k], v)
				}
			}
			if tc.cause != nil && !errors.Is(e, tc.cause) {
				t.Errorf("cause lost: %v", e)
			}
			if res.TimedOut != (tc.code == CodeTimedOut) {
				t.Errorf("TimedOut = %v", res.TimedOut)
			}
		})
	}
	r.Close()
	if err := r.Run(context.Background(), Probe{Path: exit3}).Err; !errors.Is(err, ErrClosed) {
		t.Errorf("after Close: %v", err)
	}
}

func TestRunnerOutput(t *testing.T) {
	root := tempRoot(t)
	sh := filepath.Join(root, "p")
	script(t, sh, `read line; echo "out:$line:$FOO"; echo err >&2; exit 3`)
	var r Runner
	res := r.Run(context.Background(), Probe{Path: sh, Env: Env{"FOO=bar"}})
	if res.ExitCode != 3 || res.Err == nil {
		t.Fatalf("exit %d err %v", res.ExitCode, res.Err)
	}
	if string(res.Stdout) != "out::bar\n" || string(res.Stderr) != "err\n" {
		t.Fatalf("stdout %q stderr %q", res.Stdout, res.Stderr)
	}
	if string(res.Output()) != "out::bar\nerr\n" {
		t.Fatalf("Output %q", res.Output())
	}
}

// 下面的腳本把具名管道開在 fd 3 上，子孫也繼承它：測試讀到 EOF
// 就表示群組裡每個行程都已結束，不必輪詢。

func TestRunnerCancelKillsGroup(t *testing.T) {
	root := tempRoot(t)
	f := fifo(t, root)
	sh := filepath.Join(root, "slow")
	// 只殺掉父行程的話，sleep 會留著 fd 3。
	script(t, sh, `exec 3>"`+f+`"; sleep 60 & echo ready >&3; wait`)
	var r Runner
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- r.Run(ctx, Probe{Path: sh}) }()
	events := watch(t, f)
	expect(t, events, "ready")
	cancel()
	expect(t, events, eof)
	if res := <-done; !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("want canceled, got %+v", res)
	}
}

// TestRunnerTermBeforeKill 確認群組先收到 SIGTERM：寬限期長到測試
// 不可能等到 SIGKILL。
func TestRunnerTermBeforeKill(t *testing.T) {
	root := tempRoot(t)
	f := fifo(t, root)
	sh := filepath.Join(root, "graceful")
	script(t, sh, `trap 'echo term >&3; exit 0' TERM; exec 3>"`+f+`"; echo ready >&3; `+idle)
	r := Runner{Grace: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- r.Run(ctx, Probe{Path: sh}) }()
	events := watch(t, f)
	expect(t, events, "ready")
	cancel()
	expect(t, events, "term")
	expect(t, events, eof)
	if res := <-done; !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("want canceled, got %+v", res)
	}
}

// TestRunnerKillsAfterGrace 確認忽略 SIGTERM 的群組在寬限期後被 SIGKILL。
func TestRunnerKillsAfterGrace(t *testing.T) {
	root := tempRoot(t)
	f := fifo(t, root)
	sh := filepath.Join(root, "stubborn")
	// 被忽略的訊號由 sleep 繼承。
	script(t, sh, `trap '' TERM; exec 3>"`+f+`"; echo ready >&3; sleep 60`)
	r := Runner{Grace: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- r.Run(ctx, Probe{Path: sh}) }()
	events := watch(t, f)
	expect(t, events, "ready")
	cancel()
	expect(t, events, eof)
	if res := <-done; !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("want canceled, got %+v", res)
	}
}

func TestRunnerCloseStopsRunning(t *testing.T) {
	root := tempRoot(t)
	f := fifo(t, root)
	sh := filepath.Join(root, "slow")
	script(t, sh, `exec 3>"`+f+`"; sleep 60 & echo ready >&3; wait`)
	var r Runner
	done := make(chan Result, 1)
	go func() { done <- r.Run(context.Background(), Probe{Path: sh}) }()
	events := watch(t, f)
	expect(t, events, "ready")
	r.Close()
	expect(t, events, eof)
	if res := <-done; !errors.Is(res.Err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %+v", res)
	}
	if res := r.Run(context.Background(), Probe{Path: sh}); !errors.Is(res.Err, ErrClosed) {
		t.Fatalf("after Close: %v, want ErrClosed", res.Err)
	}
	r.Close() // 再次呼叫無害
}

// TestRunnerEndsStragglers 確認程式自行結束後，群組裡留下（且仍開著
// 輸出）的子孫被結束，結果依程式自己的結束狀態回報。
func TestRunnerEndsStragglers(t *testing.T) {
	for _, tc := range []struct {
		name, exit string
		code       int
		err        error
	}{
		{"success", "", 0, nil},
		{"failure", "exit 3", 3, &Error{Code: CodeExited}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tempRoot(t)
			f := fifo(t, root)
			sh := filepath.Join(root, "leaves")
			script(t, sh, `exec 3>"`+f+`"; sleep 60 & echo hi; `+tc.exit)
			var r Runner
			done := make(chan Result, 1)
			go func() { done <- r.Run(context.Background(), Probe{Path: sh}) }()
			events := watch(t, f)
			expect(t, events, eof)
			res := <-done
			if res.ExitCode != tc.code || string(res.Stdout) != "hi\n" || (tc.err == nil) != (res.Err == nil) || (tc.err != nil && !errors.Is(res.Err, tc.err)) {
				t.Fatalf("got %+v", res)
			}
		})
	}
}

func TestSnapshotCanceled(t *testing.T) {
	root := tempRoot(t)
	sh := filepath.Join(root, "zsh")
	fakeShell(t, sh, `export X=1`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inherited := Env{"HOME=" + root, "SHELL=" + sh}
	s := Snapshotter{Runner: &Runner{}, Root: root}
	env, err := s.Snapshot(ctx, inherited)
	if !errors.Is(err, &Error{Code: CodeCanceled}) || !errors.Is(err, context.Canceled) || env.Get("X") != "" {
		t.Fatalf("got %v, %v", env, err)
	}
}

const eof = "<EOF>"

// idle 是要靠 TERM trap 收尾的腳本的空轉：不論 SIGTERM 何時到，trap
// 都在當下的前台命令結束後執行（POSIX），所以至多晚一秒。這類腳本
// 不能用 `sleep 60 & …; wait`：兩件事同時發生時 trap 要等 60 秒——
// SIGTERM 早於 wait 開始（wait 只在等待中收到訊號才提前返回），而且
// 背景的 sleep 在 fork 之後、exec 之前收到它（被 shell 的處理函式吞掉，
// sleep 活滿 60 秒，wait 就等它）。沒有 TERM trap 的腳本照用無妨：
// shell 收到 SIGTERM 就結束，留下的 sleep 由 reapGroup 收掉。
const idle = `while :; do sleep 1; done`

// fifo 建立具名管道。
func fifo(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "events")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// watch 讀 fifo 的每一行，最後送出 eof。開啟會阻塞到腳本開啟寫端。
func watch(t *testing.T, fifo string) <-chan string {
	t.Helper()
	events := make(chan string, 8)
	go func() {
		defer close(events)
		f, err := os.Open(fifo)
		if err != nil {
			return
		}
		defer f.Close()
		r := bufio.NewReader(f)
		for {
			line, err := r.ReadString('\n')
			if err == io.EOF {
				events <- eof
				return
			}
			if err != nil {
				return
			}
			events <- line[:len(line)-1]
		}
	}()
	return events
}

// expect 等下一個事件；30 秒只是把卡住變成失敗的保險。
func expect(t *testing.T, events <-chan string, want string) {
	t.Helper()
	select {
	case got, ok := <-events:
		if !ok || got != want {
			t.Fatalf("event %q (open %v), want %q", got, ok, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("no %q event", want)
	}
}
