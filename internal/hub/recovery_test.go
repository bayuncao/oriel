package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/workspace"
)

// newHubOn 在同一個 Log/Config/時鐘上開第二個 hub（模擬重啟）。
func (env *testEnv) restart(t *testing.T) *Hub {
	t.Helper()
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	h := New(Options{
		Log:      env.store,
		Config:   env.store,
		Index:    env.store,
		Notifier: chanNotifier{ch: env.notify},
		Runtimes: map[task.Protocol]Runtime{
			"test-line": lineRuntime{},
		},
		Git: workspace.New(workspace.Options{Runner: runner, Env: func(context.Context) (proc.Env, error) {
			return proc.Inherited(), nil
		}}),
		Env: func(context.Context) (proc.Env, error) {
			return proc.Inherited().With("ORIELHUB_TEST_DIR=" + env.dir), nil
		},
		Now:          env.clock.Now,
		Timer:        env.clock.Timer,
		WorktreeRoot: env.wt,
	})
	t.Cleanup(h.Close)
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRestartAfterIdle(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnCompletedKind)
	w.cancel()
	n1, _ := env.store.Read(context.Background(), id, 0, 0)
	env.h.Close()
	h2 := env.restart(t)
	// 子行程已死、Log 沒有新記錄；Session 照舊閒著，行程不在只記在 Live.Lost（D117，m2.md H07）。
	snap := snapshotOf(t, h2, id)
	if len(snap.Live.Conns) != 0 {
		t.Fatalf("conns %+v", snap.Live.Conns)
	}
	if lost, ok := snap.Live.Lost[sid]; !ok || lost != "" {
		t.Fatalf("lost %+v", snap.Live.Lost)
	}
	if x := snap.State.Sessions[sid]; x.Status != task.SessionIdle || x.Exit != nil {
		t.Fatalf("session %+v", x)
	}
	n2, _ := env.store.Read(context.Background(), id, 0, 0)
	if len(n2) != len(n1) {
		t.Fatalf("log grew: %d → %d", len(n1), len(n2))
	}
}

// 重啟後向閒著的 Session 送訊息（不等任何東西）：agent 重新啟動並接回同一個原生會話，這一輪照常完成（m2.md H08）。
func TestPromptAfterRestart(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	nid := w.next(task.NativeSessionBoundKind).NativeSessionBound.NativeID
	w.next(task.TurnCompletedKind)
	w.cancel()
	n1, _ := env.store.Read(context.Background(), id, 0, 0)
	env.h.Close()
	env.h = env.restart(t)
	env.submit(t, id, sid, "say hi")
	w2 := watch(t, env.h, id)
	w2.until(func(e task.Event) bool {
		if e.Seq <= task.Seq(len(n1)) {
			return false // 重啟前的回放
		}
		switch e.Kind {
		case task.TurnFailedKind, task.ProcessExitedKind:
			t.Fatalf("unexpected %s: %+v", e.Kind, e)
		case task.NativeSessionBoundKind:
			t.Fatalf("rebound %+v; resuming the same native session records nothing", e.NativeSessionBound)
		}
		return e.Kind == task.TurnCompletedKind
	})
	snap := snapshotOf(t, env.h, id)
	if c, ok := snap.Live.Conns[sid]; !ok || c.NativeID != nid {
		t.Fatalf("conn %+v, want native %q", snap.Live.Conns, nid)
	}
	if _, ok := snap.Live.Lost[sid]; ok {
		t.Fatalf("still lost %+v", snap.Live.Lost)
	}
}

// 重啟後閒著的 Session，agent 接不回原生會話：actor 讀任何命令之前就記為退出，緊接著送的訊息被拒絕，
// 不會先收下再失敗（m2.md H09）。
func TestRestartIdleNotResumable(t *testing.T) {
	l := testLaunch()
	l.Caps.Resume = false
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", l)
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnCompletedKind)
	w.cancel()
	env.h.Close()
	h2 := env.restart(t)
	_, err := h2.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdSubmit, Session: sid, Submit: &task.Submit{Prompt: task.Prompt{Text: "say hi"}}})
	if code := errCode(err); code != "task.session_exited" {
		t.Fatalf("submit err %v", err)
	}
	x := snapshotOf(t, h2, id).State.Sessions[sid]
	if x.Status != task.SessionExited || x.Exit == nil || x.Exit.Problem.Code != string(CodeConnectionLost) {
		t.Fatalf("session %+v", x)
	}
}

func TestRestartDuringTurn(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "hang", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnStartedKind)
	w.next(task.NativeSessionBoundKind)
	w.cancel()
	env.h.Close()
	h2 := env.restart(t)
	w2 := watch(t, h2, id)
	e := w2.next(task.TurnFailedKind)
	if e.TurnFailed.Problem.Code != string(CodeConnectionLost) {
		t.Fatalf("problem %+v", e.TurnFailed.Problem)
	}
	w2.next(task.ProcessExitedKind)
	if _, err := h2.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdResume, Session: sid}); err != nil {
		t.Fatal(err)
	}
	env.h = h2
	env.submit(t, id, sid, "say hi")
	w2.next(task.TurnCompletedKind)
}

func TestTerminalTaskNotLoaded(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnCompletedKind)
	env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdCloseSession, Session: sid, CloseSession: &task.CloseSession{}})
	w.next(task.SessionClosedKind)
	w.cancel()
	env.h.Close()
	h2 := env.restart(t)
	if n := h2.loaded(); n != 0 {
		t.Fatalf("loaded %d actors, want 0", n)
	}
}

func TestWorktreeExistsAfterRestart(t *testing.T) {
	env := newTestHub(t, nil)
	// 手造一段有 WorktreeAttached 的 stream，路徑真的存在。
	id := task.TaskID("t-wt")
	dir := t.TempDir()
	mkEv := func(seq task.Seq, kind task.EventKind, build func(*task.Event)) Record {
		e := task.Event{Seq: seq, Kind: kind, At: env.clock.Now()}
		build(&e)
		b, _ := task.EncodeEvent(e)
		return Record{Seq: seq, Kind: kind, At: e.At, Payload: b}
	}
	ws := Workspace{ID: "w-wt", Path: dir, Policy: task.PolicyAlways}
	env.store.PutWorkspace(context.Background(), ws)
	wt := filepath.Join(env.wt, "t-wt")
	os.MkdirAll(wt, 0o755)
	err := env.store.Append(context.Background(), id, 0, Batch{Records: []Record{
		mkEv(1, task.TaskCreatedKind, func(e *task.Event) {
			e.TaskCreated = &task.TaskCreated{Title: "t", Workspace: "w-wt", Isolation: task.IsolationWorktree}
		}),
		mkEv(2, task.WorktreeAttachedKind, func(e *task.Event) {
			e.WorktreeAttached = &task.Worktree{Path: wt, Branch: "orielhub/t-wt"}
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	h2 := env.restart(t)
	a, err2 := h2.loadActor(context.Background(), id)
	if err2 != nil {
		t.Fatal(err2)
	}
	_ = a
	snap := snapshotOf(t, h2, id)
	if !snap.Live.Worktrees[wt] {
		t.Fatalf("worktrees %+v", snap.Live.Worktrees)
	}
}

// pending ask after restart is cancelled：重啟後連線不在，
// report_exited → 回合失敗、ask 一併取消，不會有 PermissionDecided。
func TestPendingAskAfterRestart(t *testing.T) {
	l := testLaunch()
	l.AskTimeout = 30 * time.Second
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "ask a1 rm x", l)
	w := watch(t, env.h, id)
	w.next(task.PermissionAskedKind)
	w.cancel()
	env.h.Close()
	env.clock.Advance(60 * time.Second) // 重啟時期限已過
	h2 := env.restart(t)
	w2 := watch(t, h2, id)
	seen := map[task.EventKind]task.Event{}
	deadline := time.Now().Add(testWait)
	for seen[task.ProcessExitedKind].Kind == "" && time.Now().Before(deadline) {
		select {
		case e := <-w2.events:
			seen[e.Kind] = e
		case <-time.After(time.Second):
		}
	}
	if _, ok := seen[task.ProcessExitedKind]; !ok {
		t.Fatal("no ProcessExited")
	}
	if _, ok := seen[task.PermissionCancelledKind]; !ok {
		t.Fatalf("no PermissionCancelled, got %v", seen)
	}
	if _, ok := seen[task.PermissionDecidedKind]; ok {
		t.Fatal("PermissionDecided should not arrive")
	}
	if e := seen[task.TurnFailedKind]; e.TurnFailed == nil ||
		e.TurnFailed.Problem.Code != string(CodeConnectionLost) {
		t.Fatalf("turn failed %+v", e.TurnFailed)
	}
	env.clock.Advance(time.Second) // 點火已過期的計時器，讓它自我清理
	snap := snapshotOf(t, h2, id)
	if len(snap.Live.Timers) != 0 {
		t.Fatalf("timers %+v", snap.Live.Timers)
	}
}

func TestCloseRacesWithFacts(t *testing.T) {
	env := newTestHub(t, nil)
	base := runtime.NumGoroutine()
	// 10 個 Task 各一個 session 在 slow 50 串流中；各用獨立目錄，
	// 否則同 Workspace 的第二個會被強制 worktree（目前不支援）。
	for range 10 {
		id := env.create(t, t.TempDir(), "slow 50", testLaunch())
		watch(t, env.h, id).next(task.NativeSessionBoundKind)
	}
	env.h.Close()
	deadline := time.Now().Add(testWait)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if runtime.NumGoroutine() > base+2 {
		t.Fatalf("goroutines %d > %d", runtime.NumGoroutine(), base)
	}
}

// D97：同倉庫已有 Task 在跑時新 Task 也用主目錄，它的 Session 照常啟動
// （M4 做出 worktree 之前）。
func TestOtherTaskRunningSharesDir(t *testing.T) {
	env := newTestHub(t, nil)
	dir := gitRepo(t)
	id1 := env.create(t, dir, "hang", testLaunch())
	w := watch(t, env.h, id1)
	w.next(task.NativeSessionBoundKind)
	w.cancel()
	id2, _, err := env.h.CreateTask(context.Background(), LocalOwner, dir,
		task.CreateTask{Title: "t2", Brief: "say hi", Launches: []task.LaunchSnapshot{testLaunch()}})
	if err != nil {
		t.Fatal(err)
	}
	w2 := watch(t, env.h, id2)
	if e := w2.next(task.TaskCreatedKind); e.TaskCreated.Isolation != task.IsolationNone {
		t.Fatalf("isolation %s", e.TaskCreated.Isolation)
	}
	w2.next(task.TurnCompletedKind)
}

func TestNonGitDirIsNone(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	id, _, err := env.h.CreateTask(context.Background(), LocalOwner, dir, task.CreateTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := env.store.Read(context.Background(), id, 0, 1)
	e, _ := task.DecodeEvent(recs[0].Seq, recs[0].Kind, recs[0].At, recs[0].Payload)
	if e.TaskCreated.Isolation != task.IsolationNone {
		t.Fatalf("isolation %s", e.TaskCreated.Isolation)
	}
	// 不存在的目錄 → workspace.dir_missing 原樣回傳。
	_, _, err = env.h.CreateTask(context.Background(), LocalOwner,
		filepath.Join(t.TempDir(), "nope"), task.CreateTask{Title: "t"})
	var werr *workspace.Error
	if !errors.As(err, &werr) || werr.Code != workspace.CodeDirMissing {
		t.Fatalf("got %v", err)
	}
}

// archiveIdle 建一個跑完一輪、閒著的 Task，回傳它、它的 Session 與原生會話。
func archiveIdle(t *testing.T, env *testEnv, l task.LaunchSnapshot) (task.TaskID, task.SessionID, task.NativeSessionID, *watcher) {
	t.Helper()
	id := env.create(t, t.TempDir(), "say hi", l)
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	nid := w.next(task.NativeSessionBoundKind).NativeSessionBound.NativeID
	w.next(task.TurnCompletedKind)
	return id, sid, nid, w
}

func (env *testEnv) command(t *testing.T, id task.TaskID, kind task.CommandKind) {
	t.Helper()
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{Kind: kind}); err != nil {
		t.Fatal(err)
	}
}

// 歸檔閒著的 Task：agent 行程被停掉，它回報的退出被丟掉，Session 照舊閒著（m2.md H11）。
func TestArchiveKeepsIdleSession(t *testing.T) {
	env := newTestHub(t, nil)
	exited := make(chan struct{}, 1)
	env.h.hooks.fact = func(_ task.TaskID, c task.Command) {
		if c.Kind == task.FactExited {
			select {
			case exited <- struct{}{}:
			default:
			}
		}
	}
	id, sid, _, w := archiveIdle(t, env, testLaunch())
	n1, _ := env.store.Read(context.Background(), id, 0, 0)
	env.command(t, id, task.CmdArchive)
	select {
	case <-exited: // 被停掉的行程回報了退出，已入隊
	case <-time.After(testWait):
		t.Fatal("the stopped agent never reported its exit")
	}
	// 快照排在那條事實之後：它已被處理（丟掉）。
	snap := snapshotOf(t, env.h, id)
	x := snap.State.Sessions[sid]
	if x.Status != task.SessionIdle || x.Exit != nil || !x.Bound {
		t.Fatalf("session %+v", x)
	}
	if _, ok := snap.Live.Lost[sid]; !ok || len(snap.Live.Conns) != 0 {
		t.Fatalf("live %+v", snap.Live)
	}
	n2, _ := env.store.Read(context.Background(), id, 0, 0)
	if len(n2) != len(n1)+1 || n2[len(n2)-1].Kind != task.TaskArchivedKind {
		t.Fatalf("log %d → %d, last %s", len(n1), len(n2), n2[len(n2)-1].Kind)
	}
	w.cancel()
}

// 取消歸檔後馬上送訊息，舊行程還在關：新行程等它結束才啟動，接回同一個原生會話，這一輪照常完成（m2.md H12、H13）。
func TestUnarchiveWhileClosing(t *testing.T) {
	env := newTestHub(t, nil)
	gate := filepath.Join(env.dir, "close-gate")
	l := testLaunch()
	l.Env = []task.EnvVar{{Name: "ORIELHUB_TEST_CLOSE_GATE", Value: gate}}
	id, sid, nid, w := archiveIdle(t, env, l)
	env.command(t, id, task.CmdArchive)
	env.command(t, id, task.CmdUnarchive)
	env.submit(t, id, sid, "say hi")
	snap := snapshotOf(t, env.h, id)
	turn := snap.State.Sessions[sid].Turn
	if turn == nil || !snap.Live.InFlight[closeKey(sid)] || snap.Live.InFlight[task.EffectKey("start_conn:"+string(sid))] {
		t.Fatalf("turn %+v in flight %v: start_conn must wait for close_conn", turn, snap.Live.InFlight)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	w.until(func(e task.Event) bool {
		switch e.Kind {
		case task.TurnFailedKind, task.ProcessExitedKind:
			t.Fatalf("unexpected %s: %+v", e.Kind, e)
		case task.NativeSessionBoundKind:
			t.Fatalf("rebound %+v; resuming the same native session records nothing", e.NativeSessionBound)
		}
		return e.Kind == task.TurnCompletedKind && e.Seq > snap.State.Version
	})
	if c := snapshotOf(t, env.h, id).Live.Conns[sid]; c.NativeID != nid {
		t.Fatalf("conn native %q, want %q", c.NativeID, nid)
	}
}

// agent 接不回以前的會話：取消歸檔時就記為退出，緊接著送的訊息被拒絕（m2.md H14）。
func TestUnarchiveNotResumable(t *testing.T) {
	l := testLaunch()
	l.Caps.Resume = false
	env := newTestHub(t, nil)
	id, sid, _, w := archiveIdle(t, env, l)
	w.cancel()
	env.command(t, id, task.CmdArchive)
	env.command(t, id, task.CmdUnarchive)
	_, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdSubmit, Session: sid, Submit: &task.Submit{Prompt: task.Prompt{Text: "say hi"}}})
	if code := errCode(err); code != "task.session_exited" {
		t.Fatalf("submit err %v", err)
	}
	x := snapshotOf(t, env.h, id).State.Sessions[sid]
	if x.Exit == nil || x.Exit.Problem.Code != string(CodeConnectionLost) {
		t.Fatalf("session %+v", x)
	}
}
