package hub

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

func TestFirstPromptStartsAgent(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.PromptQueuedKind)
	w.next(task.TurnStartedKind)
	if e := w.next(task.TaskStatusChangedKind); e.TaskStatusChanged.To != task.TaskInProgress {
		t.Fatalf("status %+v", e.TaskStatusChanged)
	}
	w.next(task.NativeSessionBoundKind)
	e := w.until(func(e task.Event) bool {
		return e.Kind == task.MessageAppendedKind && e.MessageAppended.Text == "hi"
	})
	if e.MessageAppended.Partial {
		t.Fatal("first message should be final")
	}
	w.next(task.TurnCompletedKind)
	if e := w.next(task.TaskStatusChangedKind); e.TaskStatusChanged.To != task.TaskInReview {
		t.Fatalf("status %+v", e.TaskStatusChanged)
	}
	snap := snapshotOf(t, env.h, id)
	if _, ok := snap.Live.Conns[sid]; !ok {
		t.Fatal("conn missing")
	}
}

func TestIdleConnectionReused(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say a", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.NativeSessionBoundKind)
	w.next(task.TurnCompletedKind)
	env.submit(t, id, sid, "say b")
	w.next(task.TurnCompletedKind)
	// 只有一條 NativeSessionBound。
	recs, _ := env.store.Read(context.Background(), id, 0, 0)
	n := 0
	for _, r := range recs {
		if r.Kind == task.NativeSessionBoundKind {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("bound count %d", n)
	}
}

func TestPromptsQueueFIFO(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "slow 1", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	env.submit(t, id, sid, "say q2")
	env.submit(t, id, sid, "say q3")
	for range 2 {
		w.next(task.PromptQueuedKind)
	}
	// slow 1 等 go-1 才會結束第一個回合。
	if err := os.WriteFile(filepath.Join(env.dir, "go-1"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// FIFO：三個回合依提交順序完成，訊息依序是 c1、q2、q3。
	var texts []string
	w.until(func(e task.Event) bool {
		if e.Kind == task.MessageAppendedKind {
			texts = append(texts, e.MessageAppended.Text)
		}
		return len(texts) == 3
	})
	if texts[0] != "c1" || texts[1] != "q2" || texts[2] != "q3" {
		t.Fatalf("out of order: %v", texts)
	}
}

func TestChannelSwitchClosesMessage(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "think a; say b", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.SessionCreatedKind)
	e1 := w.until(func(e task.Event) bool {
		return e.Kind == task.MessageAppendedKind && e.MessageAppended.Channel == task.ChannelThinking
	})
	if e1.MessageAppended.Partial {
		t.Fatal("thinking message should be final on switch")
	}
	e2 := w.until(func(e task.Event) bool {
		return e.Kind == task.MessageAppendedKind && e.MessageAppended.Channel == task.ChannelText
	})
	if e2.MessageAppended.Text != "b" {
		t.Fatalf("text %q", e2.MessageAppended.Text)
	}
	w.next(task.TurnCompletedKind)
}

func TestToolCallFlushesText(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say a; tool c1; say b", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.SessionCreatedKind)
	e := w.next(task.MessageAppendedKind)
	if e.MessageAppended.Text != "a" || e.MessageAppended.Partial {
		t.Fatalf("msg %+v", e.MessageAppended)
	}
	if e := w.next(task.ToolCallStartedKind); e.ToolCallStarted.ID != "c1" {
		t.Fatalf("tool %+v", e.ToolCallStarted)
	}
	if e := w.next(task.MessageAppendedKind); e.MessageAppended.Text != "b" {
		t.Fatalf("msg %+v", e.MessageAppended)
	}
	w.next(task.TurnCompletedKind)
}

func TestPeriodicFlush(t *testing.T) {
	env := newTestHub(t, nil)
	// 等 chunk 抵達的信號。
	arrived := make(chan struct{}, 8)
	env.h.hooks.fact = func(_ task.TaskID, c task.Command) {
		if c.Kind == task.FactOutput {
			arrived <- struct{}{}
		}
	}
	id := env.create(t, t.TempDir(), "slow 3", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.SessionCreatedKind)
	<-arrived                // c1
	snapshotOf(t, env.h, id) // 等事實處理完（計時器已排）
	env.clock.Advance(5 * time.Second)
	e := w.until(func(e task.Event) bool {
		return e.Kind == task.MessageAppendedKind && e.MessageAppended.Partial
	})
	if e.MessageAppended.Text != "c1" {
		t.Fatalf("msg %+v", e.MessageAppended)
	}
	mid := e.MessageAppended.Message
	os.WriteFile(filepath.Join(env.dir, "go-1"), []byte("x"), 0o644)
	<-arrived // c2
	snapshotOf(t, env.h, id)
	env.clock.Advance(5 * time.Second)
	e = w.until(func(e task.Event) bool {
		return e.Kind == task.MessageAppendedKind && e.MessageAppended.Partial
	})
	if e.MessageAppended.Text != "c2" || e.MessageAppended.Message != mid {
		t.Fatalf("msg %+v", e.MessageAppended)
	}
	os.WriteFile(filepath.Join(env.dir, "go-2"), []byte("x"), 0o644)
	<-arrived // c3
	os.WriteFile(filepath.Join(env.dir, "go-3"), []byte("x"), 0o644)
	e = w.until(func(e task.Event) bool {
		return e.Kind == task.MessageAppendedKind && !e.MessageAppended.Partial &&
			e.MessageAppended.Channel == task.ChannelText
	})
	if e.MessageAppended.Text != "c3" || e.MessageAppended.Message != mid {
		t.Fatalf("msg %+v", e.MessageAppended)
	}
	w.next(task.TurnCompletedKind)
}

func TestAskAllowedByUser(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "ask a1 rm -rf x", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	e := w.next(task.PermissionAskedKind)
	pid := e.PermissionAsked.ID
	if pid == "" {
		t.Fatal("no permission id")
	}
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{Permission: pid, Verdict: task.Allow}}); err != nil {
		t.Fatal(err)
	}
	if e := w.next(task.PermissionDecidedKind); e.PermissionDecided.Decision.Source != task.DecidedByUser {
		t.Fatalf("decision %+v", e.PermissionDecided)
	}
	w.next(task.PermissionAnsweredKind)
	w.next(task.TurnCompletedKind)
}

func TestAskDenied(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "ask a1 rm -rf x", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	e := w.next(task.PermissionAskedKind)
	env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{Permission: e.PermissionAsked.ID, Verdict: task.Deny}})
	w.next(task.TurnFailedKind)
}

func TestAskMatchedByRule(t *testing.T) {
	env := newTestHub(t, func(o *Options) {})
	env.store.PutRule(task.Rule{ID: "g1", Scope: task.ScopeGlobal,
		Match: task.MatchPrefix, Pattern: "rm", Verdict: task.Allow})
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	id := env.create(t, t.TempDir(), "ask a1 rm x", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.PermissionAskedKind)
	e := w.next(task.PermissionDecidedKind)
	if e.PermissionDecided.Decision.Source != task.DecidedByRule {
		t.Fatalf("decision %+v", e.PermissionDecided)
	}
	w.next(task.PermissionAnsweredKind)
	w.next(task.TurnCompletedKind)
}

func TestAskTimesOut(t *testing.T) {
	l := testLaunch()
	l.AskTimeout = 30 * time.Second
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "ask a1 rm x", l)
	w := watch(t, env.h, id)
	w.next(task.PermissionAskedKind)
	snapshotOf(t, env.h, id) // 等 schedule_timer 排定
	env.clock.Advance(30 * time.Second)
	e := w.next(task.PermissionDecidedKind)
	if e.PermissionDecided.Decision.Source != task.DecidedByTimeout ||
		e.PermissionDecided.Decision.Verdict != task.Deny {
		t.Fatalf("decision %+v", e.PermissionDecided)
	}
	w.next(task.PermissionAnsweredKind)
	w.next(task.TurnFailedKind)
}

func TestCancelRunningTurn(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "hang", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnStartedKind)
	w.next(task.NativeSessionBoundKind)
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdCancelTurn, Session: sid}); err != nil {
		t.Fatal(err)
	}
	w.next(task.TurnCancelRequestedKind)
	w.next(task.TurnCancelledKind)
}

// Prompt 正在送出時要求取消：等它送完再經 agent 取消，不在本地結束這一輪；
// 否則 agent 還在做這個 Prompt，下一個就送不進去。
func TestCancelWhilePromptSending(t *testing.T) {
	env := newTestHub(t, nil)
	gate := filepath.Join(env.dir, "prompt-gate")
	l := testLaunch()
	l.Env = []task.EnvVar{{Name: "ORIELHUB_TEST_PROMPT_GATE", Value: gate}}
	id := env.create(t, t.TempDir(), "hang", l)
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.NativeSessionBoundKind)
	if snap := snapshotOf(t, env.h, id); !snap.Live.InFlight[task.EffectKey("send_prompt:"+string(snap.State.Sessions[sid].Turn.ID))] {
		t.Fatalf("send_prompt not in flight: %v", snap.Live.InFlight)
	}
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdCancelTurn, Session: sid}); err != nil {
		t.Fatal(err)
	}
	w.next(task.TurnCancelRequestedKind)
	if snap := snapshotOf(t, env.h, id); snap.State.Sessions[sid].Turn == nil {
		t.Fatal("the turn ended locally while its prompt was being sent")
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	w.next(task.TurnCancelledKind) // agent 收到 Prompt 後收到取消，回 cancelled
	env.submit(t, id, sid, "say next")
	if e := w.next(task.MessageAppendedKind); e.MessageAppended.Text != "next" {
		t.Fatalf("message %+v", e.MessageAppended)
	}
	w.next(task.TurnCompletedKind)
}

func TestCancelBeforePromptSent(t *testing.T) {
	env := newTestHub(t, nil)
	l := testLaunch()
	l.Env = []task.EnvVar{{Name: "ORIELHUB_TEST_START_GATE", Value: filepath.Join(env.dir, "gate")}}
	id := env.create(t, t.TempDir(), "hang", l)
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnStartedKind)
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdCancelTurn, Session: sid}); err != nil {
		t.Fatal(err)
	}
	w.next(task.TurnCancelRequestedKind)
	if err := os.WriteFile(filepath.Join(env.dir, "gate"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.next(task.NativeSessionBoundKind)
	w.next(task.TurnCancelledKind)
	if data, err := os.ReadFile(filepath.Join(env.dir, "prompts.log")); err == nil && len(data) > 0 {
		t.Fatalf("prompt was sent: %q", data)
	}
}

func TestCrash(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "crash", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	e := w.next(task.TurnFailedKind)
	if e.TurnFailed.Problem.Code != "proc.exited" ||
		e.TurnFailed.Problem.Params["status"] != "3" {
		t.Fatalf("problem %+v", e.TurnFailed.Problem)
	}
	e2 := w.next(task.ProcessExitedKind)
	if e2.ProcessExited.StderrTail == "" {
		t.Fatal("no stderr tail")
	}
	snap := snapshotOf(t, env.h, id)
	if _, ok := snap.Live.Conns[sid]; ok {
		t.Fatal("conn should be gone")
	}
}

// D102：agent 綁定前就結束（多半是沒登入），Session 記為退出，原因與
// stderr 尾部都留下，不經不帶尾部的 effect_failed。
func TestExitBeforeBound(t *testing.T) {
	env := newTestHub(t, nil)
	l := testLaunch()
	l.Env = []task.EnvVar{{Name: "ORIELHUB_TEST_EARLY_EXIT", Value: "1"}}
	id := env.create(t, t.TempDir(), "say never", l)
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	e := w.next(task.TurnFailedKind)
	if e.TurnFailed.Problem.Code != "proc.exited" || e.TurnFailed.Problem.Params["status"] != "2" {
		t.Fatalf("problem %+v", e.TurnFailed.Problem)
	}
	e2 := w.next(task.ProcessExitedKind)
	if e2.ProcessExited.StderrTail != "not logged in" {
		t.Fatalf("stderr tail %q", e2.ProcessExited.StderrTail)
	}
	snap := snapshotOf(t, env.h, id)
	if _, ok := snap.Live.Conns[sid]; ok || len(snap.State.Failed) != 0 {
		t.Fatalf("conns %v failed %v", snap.Live.Conns, snap.State.Failed)
	}
}

func TestResumeAfterCrash(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "crash", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	e := w.next(task.NativeSessionBoundKind)
	nid := e.NativeSessionBound.NativeID
	w.next(task.ProcessExitedKind)
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdResume, Session: sid}); err != nil {
		t.Fatal(err)
	}
	w.next(task.SessionResumedKind)
	env.submit(t, id, sid, "say hi")
	if e := w.next(task.NativeSessionBoundKind); e.NativeSessionBound.NativeID != nid {
		t.Fatalf("resumed id %+v", e.NativeSessionBound)
	}
	w.next(task.TurnCompletedKind)
}

func TestNoRuntime(t *testing.T) {
	env := newTestHub(t, nil)
	l := testLaunch()
	l.Protocol = "nope"
	id := env.create(t, t.TempDir(), "say x", l)
	w := watch(t, env.h, id)
	w.next(task.SessionCreatedKind)
	e := w.next(task.TurnFailedKind)
	if e.TurnFailed.Problem.Code != string(CodeNoRuntime) {
		t.Fatalf("problem %+v", e.TurnFailed.Problem)
	}
	w.next(task.ProcessExitedKind)
}

func TestSecretMissing(t *testing.T) {
	env := newTestHub(t, func(o *Options) {
		o.Secret = func(string) (string, error) {
			return "", errors.New("no keychain")
		}
	})
	l := testLaunch()
	l.Env = []task.EnvVar{{Name: "K", Secret: "k1"}}
	id := env.create(t, t.TempDir(), "say x", l)
	w := watch(t, env.h, id)
	w.next(task.SessionCreatedKind)
	e := w.next(task.TurnFailedKind)
	if e.TurnFailed.Problem.Code != string(CodeSecretMissing) ||
		e.TurnFailed.Problem.Params["name"] != "k1" {
		t.Fatalf("problem %+v", e.TurnFailed.Problem)
	}
}

func TestCloseIdleSession(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.NativeSessionBoundKind)
	w.next(task.TurnCompletedKind)
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdCloseSession, Session: sid, CloseSession: &task.CloseSession{}}); err != nil {
		t.Fatal(err)
	}
	w.next(task.SessionClosedKind)
	waitFor(t, func() bool {
		snap := snapshotOf(t, env.h, id)
		_, ok := snap.Live.Conns[sid]
		return !ok
	})
}

func TestCloseRunningSession(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "hang", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.NativeSessionBoundKind)
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdCloseSession, Session: sid, CloseSession: &task.CloseSession{}}); err != nil {
		t.Fatal(err)
	}
	w.next(task.TurnCancelledKind)
	w.next(task.SessionClosedKind)
}

func TestGitEffectSuspended(t *testing.T) {
	env := newTestHub(t, nil)
	dir := gitRepo(t)
	env.store.PutWorkspace(context.Background(), Workspace{
		ID: "w-always", Path: realPath(t, dir), Policy: task.PolicyAlways})
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	id := env.create(t, dir, "say hi", testLaunch())
	w := watch(t, env.h, id)
	e := w.next(task.EffectFailedKind)
	if e.EffectFailed.Problem.Code != "workspace.unsupported" {
		t.Fatalf("problem %+v", e.EffectFailed.Problem)
	}
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdRetryEffect, RetryEffect: &task.RetryEffect{Effect: e.EffectFailed.Effect}}); err != nil {
		t.Fatal(err)
	}
	w.next(task.EffectRetriedKind)
	w.next(task.EffectFailedKind)
}

func TestArchiveClosesConnections(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.NativeSessionBoundKind)
	w.next(task.TurnCompletedKind)
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdArchive}); err != nil {
		t.Fatal(err)
	}
	w.next(task.TaskArchivedKind)
	waitFor(t, func() bool {
		snap := snapshotOf(t, env.h, id)
		return len(snap.Live.Conns) == 0
	})
	// 歸檔不把 Session 記成退出（D117）：取消歸檔後不必恢復，直接送就接回原生會話。
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdUnarchive}); err != nil {
		t.Fatal(err)
	}
	env.submit(t, id, sid, "say hi")
	w.until(func(e task.Event) bool {
		if e.Kind == task.TurnFailedKind || e.Kind == task.ProcessExitedKind {
			t.Fatalf("unexpected %s: %+v", e.Kind, e)
		}
		return e.Kind == task.TurnCompletedKind
	})
}

// snapshot 經 snap 訊息取 State + Live。
func snapshotOf(t *testing.T, h *Hub, id task.TaskID) snapshot {
	t.Helper()
	h.mu.Lock()
	a := h.actors[id]
	h.mu.Unlock()
	ch := make(chan snapshot, 1)
	a.send(h.ctx, msg{snap: ch})
	select {
	case s := <-ch:
		return s
	case <-time.After(testWait):
		t.Fatal("snapshot timed out")
		return snapshot{}
	}
}

// FanOut 會 Inspect Task 的 workdir（乾淨 repo 通過，髒 repo 拒絕）。
func TestFanOutInspectsWorkdir(t *testing.T) {
	dir := gitRepo(t)
	env := newTestHub(t, nil)
	id := env.create(t, dir, "say hi", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnCompletedKind)
	_, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdFanOut,
		FanOut: &task.FanOut{Prompt: task.Prompt{Text: "say x"},
			Launches: []task.LaunchSnapshot{testLaunch(), testLaunch()}}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w.next(task.ForkCreatedKind)
	}
	// 髒工作目錄 → dirty_workdir 拒絕。
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("add", "f")
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdFanOut,
		FanOut: &task.FanOut{Prompt: task.Prompt{Text: "say y"},
			Launches: []task.LaunchSnapshot{testLaunch(), testLaunch()}}})
	var rej *task.Rejection
	if !errors.As(err, &rej) || rej.Reason != task.ReasonDirtyWorkdir {
		t.Fatalf("got %v", err)
	}
	_ = sid
}

// slow subscriber can leave：訂閱者卡住、buffer 滿時取消，
// actor 不能被它擋住。
func TestSlowSubscriberCanLeave(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.h.Subscribe(ctx, LocalOwner, id, 0, func(Update) error {
			<-release // 卡住：ch 很快滿
			return nil
		})
	}()
	for i := range 300 {
		if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
			Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "t" + strconv.Itoa(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	cancel()
	<-done
	// actor 還在服務。
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "after"}}); err != nil {
		t.Fatal(err)
	}
}
