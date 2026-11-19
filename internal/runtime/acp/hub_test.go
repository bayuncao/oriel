//go:build unix

package acp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/runtime/contract"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/workspace"
)

// H：hub 註冊 ACP 轉接器，agent 由測試二進位依夾具扮演。

// newHub 建 hub；env 是 agent 拿到的環境（含要回放的夾具）。
func newHub(t *testing.T, store *hub.MemStore, env []string) *hub.Hub {
	t.Helper()
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	h := hub.New(hub.Options{
		Log: store, Config: store, Index: store,
		Runtimes: map[task.Protocol]hub.Runtime{task.ProtocolACP: New(Options{Runner: runner, Version: "test"})},
		Git: workspace.New(workspace.Options{Runner: runner, Env: func(context.Context) (proc.Env, error) {
			return proc.Inherited(), nil
		}}),
		Env:          func(context.Context) (proc.Env, error) { return proc.Env(env), nil },
		WorktreeRoot: filepath.Join(t.TempDir(), "wt"),
	})
	t.Cleanup(h.Close)
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

// stream 是一條訂閱：事件與增量。
type stream struct {
	t      *testing.T
	events chan task.Event
	deltas chan hub.Delta
}

func subscribe(t *testing.T, h *hub.Hub, id task.TaskID) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &stream{t: t, events: make(chan task.Event, 1024), deltas: make(chan hub.Delta, 1024)}
	go h.Subscribe(ctx, hub.LocalOwner, id, 0, func(u hub.Update) error {
		switch {
		case u.Event != nil:
			s.events <- *u.Event
		case u.Delta != nil:
			s.deltas <- *u.Delta
		}
		return nil
	})
	return s
}

// next 等下一個這種事件，略過中間的。
func (s *stream) next(kind task.EventKind) task.Event {
	s.t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case e := <-s.events:
			if e.Kind == kind {
				return e
			}
		case <-deadline:
			s.t.Fatalf("no %s event", kind)
		}
	}
}

// delta 等下一個帶這段文字的增量。
func (s *stream) delta(text string) {
	s.t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case d := <-s.deltas:
			if d.Text == text {
				return
			}
		case <-deadline:
			s.t.Fatalf("no delta %q", text)
		}
	}
}

func send(t *testing.T, h *hub.Hub, id task.TaskID, c task.Command) {
	t.Helper()
	if _, err := h.Command(context.Background(), hub.LocalOwner, id, c); err != nil {
		t.Fatalf("%s: %v", c.Kind, err)
	}
}

func createTask(t *testing.T, h *hub.Hub, brief string) task.TaskID {
	t.Helper()
	id, _, err := h.CreateTask(context.Background(), hub.LocalOwner, t.TempDir(), task.CreateTask{
		Title: "t", Brief: brief, Launches: []task.LaunchSnapshot{testLaunch()}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// H01：建 Task 帶一個 Session，一輪對話完成，Task 進入評審中。
func TestHubTurn(t *testing.T) {
	f := fixture(t, append(started, lPrompt(3, "say pong"), lChunk("pong"), lResult(3, `{"stopReason":"end_turn"}`))...)
	h := newHub(t, hub.NewMemStore(), replayEnv(t, f))
	id := createTask(t, h, "say pong")
	s := subscribe(t, h, id)
	s.next(task.TurnStartedKind)
	if e := s.next(task.NativeSessionBoundKind); e.NativeSessionBound.NativeID != "s1" {
		t.Fatalf("bound %+v", e.NativeSessionBound)
	}
	s.delta("pong")
	if e := s.next(task.MessageAppendedKind); e.MessageAppended.Text != "pong" || e.MessageAppended.Partial {
		t.Fatalf("message %+v", e.MessageAppended)
	}
	s.next(task.TurnCompletedKind)
	st, err := h.Snapshot(context.Background(), hub.LocalOwner, id)
	if err != nil || st.Status != task.TaskInReview {
		t.Fatalf("status %s, %v", st.Status, err)
	}
}

// H02、H03：使用者允許並選「總是允許」，同一輪再請求同一個命令時由規則自動回答。
func TestHubAlwaysAllow(t *testing.T) {
	opts := "[" + optAllowOnce + "," + optAllowAlways + "," + optRejectOnce + "]"
	f := fixture(t, append(started, lPrompt(3, "touch twice"),
		lUpdate(`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"touch","kind":"execute","rawInput":{"command":"touch a.txt"}}`),
		lAsk("r1", "t1", opts), lAnswer("r1", "once"),
		lUpdate(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}`),
		lUpdate(`{"sessionUpdate":"tool_call","toolCallId":"t2","title":"touch","kind":"execute","rawInput":{"command":"touch b.txt"}}`),
		lAsk("r2", "t2", opts), lAnswer("r2", "once"),
		lUpdate(`{"sessionUpdate":"tool_call_update","toolCallId":"t2","status":"completed"}`),
		lResult(3, `{"stopReason":"end_turn"}`))...)
	h := newHub(t, hub.NewMemStore(), replayEnv(t, f))
	id := createTask(t, h, "touch twice")
	s := subscribe(t, h, id)
	a := s.next(task.PermissionAskedKind)
	if a.PermissionAsked.Request.Command != "touch a.txt" || a.PermissionAsked.Request.Tool != "execute" {
		t.Fatalf("asked %+v", a.PermissionAsked.Request)
	}
	send(t, h, id, task.Command{Kind: task.CmdDecidePermission, Session: a.Session,
		DecidePermission: &task.DecidePermission{Permission: a.PermissionAsked.ID, Verdict: task.Allow,
			Always: &task.Rule{Scope: task.ScopeSession, Match: task.MatchPrefix, Pattern: "touch"}}})
	s.next(task.RuleAddedKind)
	s.next(task.PermissionAnsweredKind)
	a2 := s.next(task.PermissionAskedKind)
	if d := s.next(task.PermissionDecidedKind); d.PermissionDecided.Permission != a2.PermissionAsked.ID ||
		d.PermissionDecided.Decision.Source != task.DecidedByRule {
		t.Fatalf("decision %+v", d.PermissionDecided)
	}
	s.next(task.TurnCompletedKind)
}

// H04：取消這一輪後，佇列裡的下一個 Prompt 開新的一輪（D1）。
func TestHubCancelThenQueue(t *testing.T) {
	f := fixture(t, append(started, lPrompt(3, "one"), lChunk("1"),
		`{"client":{"method":"session/cancel","params":{"sessionId":"s1"}}}`,
		lResult(3, `{"stopReason":"cancelled"}`),
		lPrompt(4, "two"), lChunk("2"), lResult(4, `{"stopReason":"end_turn"}`))...)
	h := newHub(t, hub.NewMemStore(), replayEnv(t, f))
	id := createTask(t, h, "one")
	s := subscribe(t, h, id)
	sid := s.next(task.SessionCreatedKind).Session
	s.delta("1") // agent 已收到 Prompt
	send(t, h, id, task.Command{Kind: task.CmdSubmit, Session: sid,
		Submit: &task.Submit{Prompt: task.Prompt{Text: "two"}}})
	s.next(task.PromptQueuedKind)
	send(t, h, id, task.Command{Kind: task.CmdCancelTurn, Session: sid})
	s.next(task.TurnCancelledKind)
	s.next(task.TurnStartedKind)
	s.delta("2")
	s.next(task.TurnCompletedKind)
}

// H05：一輪中途崩潰：TurnFailed 帶結束原因，ProcessExited 帶 stderr 尾部；
// 恢復後以 session/resume 接回原會話，繼續下一個 Prompt。
func TestHubCrashThenResume(t *testing.T) {
	exit3 := 3
	boom, _ := json.Marshal(contract.Line{Stderr: "boom"})
	quit, _ := json.Marshal(contract.Line{Exit: &exit3})
	crash := fixture(t, append(started, lPrompt(3, "one"), lChunk("po"), string(boom), string(quit))...)
	h := newHub(t, hub.NewMemStore(), replayEnv(t, crash, resumed(t, "two")))
	id := createTask(t, h, "one")
	s := subscribe(t, h, id)
	sid := s.next(task.SessionCreatedKind).Session
	if e := s.next(task.TurnFailedKind); e.TurnFailed.Problem.Code != "proc.exited" || e.TurnFailed.Problem.Params["status"] != "3" {
		t.Fatalf("turn failed %+v", e.TurnFailed)
	}
	if e := s.next(task.ProcessExitedKind); e.ProcessExited.StderrTail != "boom" {
		t.Fatalf("exited %+v", e.ProcessExited)
	}
	send(t, h, id, task.Command{Kind: task.CmdResume, Session: sid})
	send(t, h, id, task.Command{Kind: task.CmdSubmit, Session: sid,
		Submit: &task.Submit{Prompt: task.Prompt{Text: "two"}}})
	if e := s.next(task.NativeSessionBoundKind); e.NativeSessionBound.NativeID != "s1" {
		t.Fatalf("bound %+v", e.NativeSessionBound)
	}
	s.delta("again")
	s.next(task.TurnCompletedKind)
}

// resumed 是接回 s1 後答一輪的夾具（第二次啟動用）。
func resumed(t *testing.T, prompt string) string {
	return fixture(t, lInit,
		`{"agent":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}}}`,
		`{"client":{"id":2,"method":"session/resume","params":{"sessionId":"s1","cwd":"<cwd>"}}}`, lResult(2, `{}`),
		lPrompt(3, prompt), lChunk("again"), lResult(3, `{"stopReason":"end_turn"}`))
}

// H06：一輪進行中 hub 關閉；重開後這一輪以 hub.connection_lost 失敗，恢復後接回原會話。
func TestHubRestart(t *testing.T) {
	store := hub.NewMemStore()
	hang := fixture(t, append(started, lPrompt(3, "one"), lChunk("working"))...)
	env := replayEnv(t, hang, resumed(t, "two"))
	h := newHub(t, store, env)
	id := createTask(t, h, "one")
	s := subscribe(t, h, id)
	sid := s.next(task.SessionCreatedKind).Session
	s.delta("working")
	h.Close()

	h2 := newHub(t, store, env)
	s2 := subscribe(t, h2, id)
	if e := s2.next(task.TurnFailedKind); e.TurnFailed.Problem.Code != "hub.connection_lost" {
		t.Fatalf("turn failed %+v", e.TurnFailed)
	}
	s2.next(task.ProcessExitedKind)
	send(t, h2, id, task.Command{Kind: task.CmdResume, Session: sid})
	send(t, h2, id, task.Command{Kind: task.CmdSubmit, Session: sid,
		Submit: &task.Submit{Prompt: task.Prompt{Text: "two"}}})
	s2.delta("again")
	s2.next(task.TurnCompletedKind)
}
