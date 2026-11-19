//go:build unix

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/runtime/acp"
	"github.com/bayuncao/oriel/internal/task"
)

// fakeGrok 是說 ACP 的假 grok：--version 印出 version；啟動時在 log 記一行
// 「start 目前目錄」，探測（在 orielhub-probe- 暫存目錄裡）設了 GROK_GATE 時先
// 等那個具名管道有人寫；fail 時答 initialize 前就結束。建好會話後答完一輪。
func fakeGrok(version, log string, fail bool) string {
	s := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'grok " + version + " (0920c8169e27)'; exit 0; fi\n" +
		"echo \"start $PWD\" >> '" + log + "'\n" +
		"case \"$PWD\" in *orielhub-probe-*) [ -n \"$GROK_GATE\" ] && read g < \"$GROK_GATE\";; esac\n"
	if fail {
		return s + "echo broken >&2\nexit 1\n"
	}
	return s +
		"read l; echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{\"loadSession\":true}}}'\n" +
		"read l || exit 0\n" +
		"echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"s1\"}}'\n" +
		"read l; echo '{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"s1\",\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"hi\"}}}}'\n" +
		"echo '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"stopReason\":\"end_turn\"}}'\n" +
		"while read l; do :; done\n"
}

// probeDirs 是 log 裡探測用過的目錄（每次探測一個）。
func probeDirs(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if dir, ok := strings.CutPrefix(line, "start "); ok && strings.Contains(dir, "orielhub-probe-") {
			out = append(out, dir)
		}
	}
	return out
}

// fakeHome 是受控的家目錄：假 grok 放在它的廠商目錄（~/.grok/bin），假 pi
// 放在使用者工具目錄（~/.local/bin）；登入 shell 的快照碰不到開發者的設定與
// 裝好的 agent。回傳家目錄與 grok 的目錄。
func fakeHome(t *testing.T) (home, grokBin string) {
	t.Helper()
	home = t.TempDir()
	grokBin = filepath.Join(home, ".grok", "bin")
	for _, d := range []string{grokBin, filepath.Join(home, ".local", "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home, grokBin
}

// launchApp 在 home 下建 app（extra 加進環境），等 grok 就緒。
func launchApp(t *testing.T, home string, extra ...string) *App {
	t.Helper()
	env := append(proc.Env{"PATH=/usr/bin:/bin", "HOME=" + home, "SHELL=/bin/sh"}, extra...)
	a, _ := newTestApp(t, env, t.TempDir())
	waitReady(t, a, "grok")
	return a
}

func waitReady(t *testing.T, a *App, id task.AgentID) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		for _, i := range a.Agents().Installations() {
			if i.Agent == id && i.Availability == agent.Ready {
				return
			}
		}
		select {
		case <-a.Changes():
		case <-deadline:
			t.Fatalf("%s never ready", id)
		}
	}
}

// waitTurn 訂閱 Task，等它的 Session 完成第一輪；退出了就失敗。
func waitTurn(t *testing.T, a *App, id task.TaskID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctxb2(), 30*time.Second)
	defer cancel()
	done := errors.New("turn completed")
	err := a.Hub().Subscribe(ctx, hub.LocalOwner, id, 0, func(u hub.Update) error {
		switch {
		case u.Event == nil:
		case u.Event.Kind == task.TurnCompletedKind:
			return done
		case u.Event.Kind == task.ProcessExitedKind:
			return errors.New("session exited: " + u.Event.ProcessExited.Problem.Code)
		}
		return nil
	})
	if err != done {
		t.Fatalf("waiting for the first turn: %v", err)
	}
}

func createWithAgent(t *testing.T, a *App, id task.AgentID) task.TaskID {
	t.Helper()
	tid, _, err := a.Hub().CreateTaskWithAgent(ctxb2(), hub.LocalOwner, t.TempDir(),
		task.CreateTask{Title: "t", Brief: "say hi"}, id)
	if err != nil {
		t.Fatal(err)
	}
	return tid
}

// P01、P02、P05–P07、I02：帶 agent 建 Task，Launch Snapshot 用預設 Profile；
// 能否恢復按路徑與版本探測一次，探測的暫存目錄用完即刪，版本變了再探測。
func TestCreateTaskWithAgent(t *testing.T) {
	home, bin := fakeHome(t)
	log := filepath.Join(t.TempDir(), "grok.log")
	grok := filepath.Join(bin, "grok")
	writeExe(t, bin, "grok", fakeGrok("1.0.70", log, false))
	a := launchApp(t, home)

	id := createWithAgent(t, a, "grok")
	waitTurn(t, a, id)
	st, err := a.Hub().Snapshot(ctxb2(), hub.LocalOwner, id)
	if err != nil || st.Status != task.TaskInReview || len(st.Sessions) != 1 {
		t.Fatalf("status %s, %d sessions, %v", st.Status, len(st.Sessions), err)
	}
	for _, x := range st.Sessions {
		l := x.Launch
		if l.Agent != "grok" || l.AgentName != "Grok" || l.Protocol != task.ProtocolACP ||
			l.Profile != agent.DefaultProfileID || l.ProfileName != "Default" || l.Executable != grok ||
			strings.Join(l.Args, " ") != "agent stdio" || len(l.Env) != 0 ||
			l.Approval != task.ApprovalOrielHub || l.AskTimeout != 0 || l.Caps != (task.Capabilities{Resume: true}) {
			t.Fatalf("launch %+v", l)
		}
	}
	dirs := probeDirs(t, log)
	if len(dirs) != 1 {
		t.Fatalf("%d probes, want 1", len(dirs))
	}
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Fatalf("probe dir %s was not removed: %v", dirs[0], err)
	}

	waitTurn(t, a, createWithAgent(t, a, "grok")) // P06
	if n := len(probeDirs(t, log)); n != 1 {
		t.Fatalf("%d probes after the second task, want 1", n)
	}

	writeExe(t, bin, "grok", fakeGrok("1.0.71", log, false)+"# upgraded\n") // P07
	waitTurn(t, a, createWithAgent(t, a, "grok"))
	if n := len(probeDirs(t, log)); n != 2 {
		t.Fatalf("%d probes after the upgrade, want 2", n)
	}
}

// gatedLauncher 是探測停在具名管道上的 launcher：測試等到 waiters 個呼叫者
// 都在等這次探測，再放行。
func gatedLauncher(t *testing.T, fail bool) (a *App, l *launcher, log, gate string, waiting chan struct{}) {
	t.Helper()
	home, bin := fakeHome(t)
	log = filepath.Join(t.TempDir(), "grok.log")
	gate = filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	writeExe(t, bin, "grok", fakeGrok("1.0.70", log, fail))
	a = launchApp(t, home, "GROK_GATE="+gate)
	l = newLauncher(a.disc, catalog(a.agents.log), acp.New(acp.Options{Runner: a.runner}).Capabilities, a.agents.log)
	waiting = make(chan struct{}, 16)
	l.waiting = func() { waiting <- struct{}{} }
	return a, l, log, gate, waiting
}

// open 放行停在具名管道上的探測。
func open(t *testing.T, gate string) {
	t.Helper()
	if err := release(gate); err != nil {
		t.Fatal(err)
	}
}

// release 是 open 不碰 testing.T 的版本，給別的 goroutine 用。
func release(gate string) error {
	f, err := os.OpenFile(gate, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString("go\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func waitWaiters(t *testing.T, waiting chan struct{}, n int) {
	t.Helper()
	for range n {
		select {
		case <-waiting:
		case <-time.After(30 * time.Second):
			t.Fatal("callers never started waiting for the probe")
		}
	}
}

// P05、P08：同時來的解析共用一次探測，成功與失敗都是；失敗時大家都按不能恢復，
// 不記下來，下一次再探測。
func TestConcurrentLaunchesShareProbe(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := map[bool]string{false: "success", true: "failure"}[fail]
		t.Run(name, func(t *testing.T) {
			_, l, log, gate, waiting := gatedLauncher(t, fail)
			const n = 5
			results := make(chan task.LaunchSnapshot, n)
			var wg sync.WaitGroup
			for range n {
				wg.Go(func() {
					s, err := l.Launch(ctxb2(), "grok")
					if err != nil {
						t.Error(err)
					}
					results <- s
				})
			}
			waitWaiters(t, waiting, n-1)
			open(t, gate)
			wg.Wait()
			close(results)
			for s := range results {
				if s.Caps.Resume == fail {
					t.Fatalf("caps %+v after a probe that failed=%v", s.Caps, fail)
				}
			}
			if got := len(probeDirs(t, log)); got != 1 {
				t.Fatalf("%d probes, want 1", got)
			}
			if !fail {
				return
			}
			// 失敗沒有記下來：下一次再探測。
			released := make(chan error, 1)
			go func() { released <- release(gate) }()
			if _, err := l.Launch(ctxb2(), "grok"); err != nil {
				t.Fatal(err)
			}
			if err := <-released; err != nil {
				t.Fatal(err)
			}
			if got := len(probeDirs(t, log)); got != 2 {
				t.Fatalf("%d probes, want 2 (failures are not cached)", got)
			}
		})
	}
}

// 第一個呼叫者離開了：它拿到自己的 ctx 錯誤，探測照常做完，等著的人拿到結果。
func TestCanceledCallerDoesNotFailOthers(t *testing.T) {
	_, l, log, gate, waiting := gatedLauncher(t, false)
	ctx, cancel := context.WithCancel(ctxb2())
	first := make(chan error, 1)
	go func() {
		_, err := l.Launch(ctx, "grok")
		first <- err
	}()
	second := make(chan task.LaunchSnapshot, 1)
	go func() {
		s, err := l.Launch(ctxb2(), "grok")
		if err != nil {
			t.Error(err)
		}
		second <- s
	}()
	waitWaiters(t, waiting, 1)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller got %v, want context.Canceled", err)
	}
	open(t, gate)
	if s := <-second; !s.Caps.Resume {
		t.Fatalf("second caller got %+v", s.Caps)
	}
	if got := len(probeDirs(t, log)); got != 1 {
		t.Fatalf("%d probes, want 1", got)
	}
}

// P08：探測失敗時照樣建 Task，這次按不能恢復；每次建 Task 都再探測一次。
func TestProbeFailureDoesNotBlock(t *testing.T) {
	home, bin := fakeHome(t)
	log := filepath.Join(t.TempDir(), "grok.log")
	writeExe(t, bin, "grok", fakeGrok("1.0.70", log, true))
	a := launchApp(t, home)
	for range 2 {
		id := createWithAgent(t, a, "grok")
		st, _ := a.Hub().Snapshot(ctxb2(), hub.LocalOwner, id)
		for _, x := range st.Sessions {
			if x.Launch.Caps.Resume {
				t.Fatal("a failed probe must not claim resume")
			}
		}
	}
	if got := len(probeDirs(t, log)); got != 2 {
		t.Fatalf("%d probes, want 2: failures are not cached", got)
	}
}

// P03、P04、P09、I01：不能啟動時不建 Task、不登記 Workspace；沒選 agent 照舊。
func TestCreateTaskWithAgentRejects(t *testing.T) {
	home, bin := fakeHome(t)
	writeExe(t, bin, "grok", fakeGrok("1.0.70", filepath.Join(t.TempDir(), "log"), false))
	writeExe(t, filepath.Join(home, ".local", "bin"), "pi", "#!/bin/sh\necho 0.50.0\n")
	a := launchApp(t, home)
	waitReady(t, a, "pi")
	for _, tc := range []struct {
		agent task.AgentID
		code  string
	}{
		{"gemini", string(agent.CodeNotInstalled)},
		{"pi", string(hub.CodeAgentUnsupported)},
		{"nobody", string(agent.CodeUnknownAgent)},
	} {
		_, _, err := a.Hub().CreateTaskWithAgent(ctxb2(), hub.LocalOwner, t.TempDir(),
			task.CreateTask{Title: "t", Brief: "say hi"}, tc.agent)
		c, ok := errors.AsType[interface {
			error
			ErrorCode() string
		}](err)
		if !ok || c.ErrorCode() != tc.code {
			t.Fatalf("%s: got %v, want %s", tc.agent, err, tc.code)
		}
	}
	tasks, _ := a.Hub().Tasks(ctxb2(), hub.LocalOwner)
	ws, _ := a.Hub().Workspaces(ctxb2(), hub.LocalOwner)
	if len(tasks) != 0 || len(ws) != 0 {
		t.Fatalf("%d tasks, %d workspaces after rejected creations", len(tasks), len(ws))
	}
	id, _, err := a.Hub().CreateTaskWithAgent(ctxb2(), hub.LocalOwner, t.TempDir(),
		task.CreateTask{Title: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := a.Hub().Snapshot(ctxb2(), hub.LocalOwner, id); st.Status != task.TaskTodo || len(st.Sessions) != 0 {
		t.Fatalf("status %s, %d sessions", st.Status, len(st.Sessions))
	}
}
