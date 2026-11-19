//go:build unix

package proc

import (
	"bufio"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startAgent 啟動 agent 行程；測試結束時 Stop，失敗也不留下行程。
func startAgent(t *testing.T, r *Runner, c Command) *Process {
	t.Helper()
	p, err := r.Start(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Stop()
		p.CloseStdout()
	})
	return p
}

// waitDone 等行程結束；30 秒只是把卡住變成失敗的保險。
func waitDone(t *testing.T, p *Process) {
	t.Helper()
	select {
	case <-p.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("agent process did not end")
	}
}

func wantExit(t *testing.T, p *Process, code Code, key, value string) {
	t.Helper()
	e := p.Exit()
	if e == nil || e.Code != code || e.Params[key] != value {
		t.Fatalf("exit %v, want %s{%s: %s}", e, code, key, value)
	}
}

// X01：獨立的 session 與群組，stdin / stdout 是協議管道，目錄與環境用傳入的。
func TestAgentStart(t *testing.T) {
	root := tempRoot(t)
	sh := filepath.Join(root, "agent")
	script(t, sh, `read line
pgid=$(ps -o pgid= -p $$ | tr -d ' ')
echo "got:$line pgid-is-pid:$([ "$pgid" = "$$" ] && echo yes) foo:$FOO home:$HOME pwd:$(pwd)"
ps -o tty= -p $$ | tr -d ' '
read rest`)
	var r Runner
	defer r.Close()
	p := startAgent(t, &r, Command{Path: sh, Env: Env{"PATH=/bin:/usr/bin", "FOO=bar"}, Dir: root})
	if _, err := fmt.Fprintln(p.Stdin(), "hello"); err != nil {
		t.Fatal(err)
	}
	out := bufio.NewReader(p.Stdout())
	line, err := out.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	want := "got:hello pgid-is-pid:yes foo:bar home: pwd:" + root + "\n"
	if line != want {
		t.Fatalf("got %q\nwant %q", line, want)
	}
	// 沒有控制終端：ps 在 macOS 印 "??"、在 Linux 印 "?"。
	if tty, err := out.ReadString('\n'); err != nil || !strings.HasPrefix(tty, "?") {
		t.Fatalf("controlling terminal %q (%v)", tty, err)
	}
	if p.Exit() != nil {
		t.Fatalf("Exit before Done: %v", p.Exit())
	}
}

// X02–X05：結束原因。
func TestAgentExit(t *testing.T) {
	root := tempRoot(t)
	for _, tc := range []struct {
		id, body   string
		code       Code
		key, value string
		stderr     string
	}{
		{"X03", "exit 0", CodeExited, "status", "0", ""},
		{"X04", "echo boom >&2; exit 3", CodeExited, "status", "3", "boom"},
		{"X05", "kill -9 $$", CodeSignaled, "signal", "9", ""},
	} {
		t.Run(tc.id, func(t *testing.T) {
			sh := filepath.Join(root, tc.id)
			script(t, sh, tc.body)
			var r Runner
			defer r.Close()
			p := startAgent(t, &r, Command{Path: sh})
			waitDone(t, p)
			wantExit(t, p, tc.code, tc.key, tc.value)
			if got := p.StderrTail(); got != tc.stderr {
				t.Fatalf("stderr tail %q, want %q", got, tc.stderr)
			}
		})
	}
	t.Run("X02", func(t *testing.T) {
		var r Runner
		defer r.Close()
		missing := filepath.Join(root, "missing")
		_, err := r.Start(Command{Path: missing})
		e, ok := errors.AsType[*Error](err)
		if !ok || e.Code != CodeStartFailed || e.Params["path"] != missing || e.Params["detail"] == "" {
			t.Fatalf("got %v", err)
		}
	})
}

// X06：stderr 只留最後 8 KiB，從一行的開頭算起，壞的 UTF-8 換掉。
func TestAgentStderrTail(t *testing.T) {
	root := tempRoot(t)
	sh := filepath.Join(root, "noisy")
	script(t, sh, `i=0
while [ $i -lt 20000 ]; do echo "line $i of the noise written to stderr" >&2; i=$((i+1)); done
printf 'last \377 end\n' >&2`)
	var r Runner
	defer r.Close()
	p := startAgent(t, &r, Command{Path: sh})
	waitDone(t, p)
	got := p.StderrTail()
	if len(got) > stderrTail || !strings.HasPrefix(got, "line ") || !strings.HasSuffix(got, "last \uFFFD end") {
		t.Fatalf("tail (%d bytes) %q…%q", len(got), got[:min(40, len(got))], got[max(0, len(got)-40):])
	}
}

// X07：行程自己結束後，群組裡的子孫一起結束。
func TestAgentEndsDescendants(t *testing.T) {
	root := tempRoot(t)
	f := fifo(t, root)
	sh := filepath.Join(root, "parent")
	script(t, sh, `exec 3>"`+f+`"; sleep 60 & echo ready >&3`)
	var r Runner
	defer r.Close()
	p := startAgent(t, &r, Command{Path: sh})
	events := watch(t, f)
	expect(t, events, "ready")
	expect(t, events, eof)
	waitDone(t, p)
	wantExit(t, p, CodeExited, "status", "0")
}

// X08：Stop 關 stdin、送 SIGTERM 給整個群組；寬限期一小時也立刻收尾。
func TestAgentStop(t *testing.T) {
	root := tempRoot(t)
	f := fifo(t, root)
	sh := filepath.Join(root, "graceful")
	// 讀 stdin 的子行程忽略 SIGTERM，只在 stdin 關閉時結束；領頭收到
	// SIGTERM 後等它，所以兩件事都會記下來，與先後無關。
	script(t, sh, `exec 3>"`+f+`"
exec 4<&0 # 背景工作的 stdin 預設是 /dev/null，明確轉向才讀得到管道
(trap '' TERM; while read l <&4; do :; done; echo stdin-closed >&3) &
reader=$!
trap 'echo term >&3; wait $reader; exit 0' TERM
sleep 60 & echo ready >&3; wait`)
	r := Runner{Grace: time.Hour}
	defer r.Close()
	p := startAgent(t, &r, Command{Path: sh})
	events := watch(t, f)
	expect(t, events, "ready")
	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case e, ok := <-events:
			if !ok || e == eof {
				t.Fatalf("events ended early: %v", got)
			}
			got[e] = true
		case <-time.After(30 * time.Second):
			t.Fatalf("got %v, want term and stdin-closed", got)
		}
	}
	if !got["term"] || !got["stdin-closed"] {
		t.Fatalf("got %v", got)
	}
	expect(t, events, eof)
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("Stop did not return")
	}
	if p.Exit() == nil {
		t.Fatal("no exit reason after Stop")
	}
	p.Stop() // 再次呼叫無害
}

// X09：忽略 SIGTERM 的群組在寬限期後被 SIGKILL。
func TestAgentStopKillsAfterGrace(t *testing.T) {
	root := tempRoot(t)
	f := fifo(t, root)
	sh := filepath.Join(root, "stubborn")
	script(t, sh, `trap '' TERM; exec 3>"`+f+`"; echo ready >&3; sleep 60`)
	r := Runner{Grace: time.Millisecond}
	defer r.Close()
	p := startAgent(t, &r, Command{Path: sh})
	events := watch(t, f)
	expect(t, events, "ready")
	p.Stop()
	expect(t, events, eof)
	wantExit(t, p, CodeSignaled, "signal", "9")
}

// X10、X11：Runner.Close 結束執行中的 agent 行程、等它們結束，之後拒絕啟動。
func TestAgentRunnerClose(t *testing.T) {
	root := tempRoot(t)
	f := fifo(t, root)
	sh := filepath.Join(root, "slow")
	script(t, sh, `exec 3>"`+f+`"; sleep 60 & echo ready >&3; wait`)
	var r Runner
	p := startAgent(t, &r, Command{Path: sh})
	events := watch(t, f)
	expect(t, events, "ready")
	r.Close()
	select {
	case <-p.Done():
	default:
		t.Fatal("Close returned before the agent process ended")
	}
	expect(t, events, eof)
	if _, err := r.Start(Command{Path: sh}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close: %v", err)
	}
}
