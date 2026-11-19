package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// TestStallStep 是沉默計時規則的純函式表格（m2.md ST02–ST10）。
func TestStallStep(t *testing.T) {
	out := &task.Command{Kind: task.FactOutput, Output: &task.Output{Channel: task.ChannelText, Text: "x"}}
	toolStart := func(id string) *task.Command {
		return &task.Command{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{ID: task.ToolCallID(id)}}
	}
	toolUpd := func(id string, st task.ToolStatus) *task.Command {
		return &task.Command{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{ID: task.ToolCallID(id), Status: st}}
	}
	asked := &task.Command{Kind: task.FactAsked, Asked: &task.Asked{ID: "p1"}}
	expired := &task.Command{Kind: task.FactAskExpired, AskExpired: &task.PermissionRef{Permission: "p1"}}
	failed := &task.Command{Kind: task.FactEffectFailed}

	// step 是一步：這一步之後的回合、有沒有待答的審批、agent 報來的事實，與期望的動作和時限。
	type step struct {
		turn    task.TurnID
		waiting bool
		c       *task.Command
		act     stallAction
		limit   time.Duration // act 為 stallArm 時
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"ST01 turn starts", []step{{turn: "t1", act: stallArm, limit: stallQuiet}}},
		{"ST02 output resets", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", c: out, act: stallArm, limit: stallQuiet},
		}},
		{"no change keeps the timer", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", act: stallKeep},
		}},
		{"hub facts are not activity", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", c: failed, act: stallKeep},
		}},
		{"ST03 running tool widens the limit", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", c: toolStart("a"), act: stallArm, limit: stallTool},
			{turn: "t1", c: toolUpd("a", task.ToolRunning), act: stallArm, limit: stallTool},
		}},
		{"ST04 back to 10 minutes when every tool ends", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", c: toolStart("a"), act: stallArm, limit: stallTool},
			{turn: "t1", c: toolStart("b"), act: stallArm, limit: stallTool},
			{turn: "t1", c: toolUpd("a", task.ToolCompleted), act: stallArm, limit: stallTool},
			{turn: "t1", c: toolUpd("b", task.ToolFailed), act: stallArm, limit: stallQuiet},
		}},
		{"a tool reported only by an update counts", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", c: toolUpd("a", task.ToolPending), act: stallArm, limit: stallTool},
		}},
		{"ST05 waiting ask pauses", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", c: asked, act: stallArm, limit: stallQuiet}, // 事實先記下動靜
			{turn: "t1", waiting: true, act: stallStop},              // 它的事件記下待答
			{turn: "t1", waiting: true, c: out, act: stallKeep},      // 等待時的動靜不排定
		}},
		{"ST06 decision restarts the count", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", waiting: true, act: stallStop},
			{turn: "t1", act: stallArm, limit: stallQuiet},
		}},
		{"ST06 expiry restarts the count, a tool still running keeps 30", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", c: toolStart("a"), act: stallArm, limit: stallTool},
			{turn: "t1", waiting: true, act: stallStop},
			{turn: "t1", waiting: true, c: expired, act: stallKeep},
			{turn: "t1", act: stallArm, limit: stallTool},
		}},
		{"ST09 turn ends", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "", act: stallStop},
			{turn: "", act: stallKeep},
		}},
		{"ST09 next turn starts fresh, earlier tools forgotten", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t1", c: toolStart("a"), act: stallArm, limit: stallTool},
			{turn: "t2", act: stallArm, limit: stallQuiet},
		}},
		{"a new turn that starts waiting stops the old timer", []step{
			{turn: "t1", act: stallArm, limit: stallQuiet},
			{turn: "t2", waiting: true, act: stallStop},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s stallState
			for i, st := range tt.steps {
				var act stallAction
				s, act = stallStep(s, st.turn, st.waiting, st.c)
				if act != st.act {
					t.Fatalf("step %d: action %d, want %d", i, act, st.act)
				}
				if act == stallArm && s.limit != st.limit {
					t.Fatalf("step %d: limit %v, want %v", i, s.limit, st.limit)
				}
			}
		})
	}
}

// TestAgentActivity：每個 Fact 種類都要明確歸類，新加的事實種類沒歸類時這裡失敗，不會默默被當成沒有動靜。
func TestAgentActivity(t *testing.T) {
	want := map[task.CommandKind]bool{
		task.FactOutput: true, task.FactToolStarted: true, task.FactToolUpdated: true, task.FactPlan: true,
		task.FactUsage: true, task.FactAsked: true, task.FactModelReported: true,
		// hub 自己產生的、或是回合與行程的結束與綁定，不算動靜。
		task.FactNativeBound: false, task.FactTurnEnded: false, task.FactExited: false, task.FactAskExpired: false,
		task.FactAnswered: false, task.FactConflicted: false, task.FactMerged: false, task.FactMergeConflict: false,
		task.FactDiscarded: false, task.FactNativeDeleted: false, task.FactRecommendation: false,
		task.FactReviewPrepared: false, task.FactEffectFailed: false,
	}
	f, err := parser.ParseFile(token.NewFileSet(), "../task/command.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			for i, name := range spec.(*ast.ValueSpec).Names {
				if !strings.HasPrefix(name.Name, "Fact") {
					continue
				}
				lit, ok := spec.(*ast.ValueSpec).Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a literal", name.Name)
				}
				k := task.CommandKind(strings.Trim(lit.Value, `"`))
				seen++
				w, ok := want[k]
				if !ok {
					t.Errorf("%s (%s) is not classified: decide whether it is agent activity (stall.go agentActivity)", name.Name, k)
					continue
				}
				if agentActivity(k) != w {
					t.Errorf("%s: agentActivity = %v, want %v", k, !w, w)
				}
			}
		}
	}
	if seen != len(want) {
		t.Errorf("command.go has %d Fact kinds, the table has %d", seen, len(want))
	}
	if agentActivity(task.CmdCancelTurn) {
		t.Error("a client command counted as agent activity")
	}
}

// TestStallStepPure：stallStep 不改傳入的狀態。
func TestStallStepPure(t *testing.T) {
	s, _ := stallStep(stallState{}, "t1", false, &task.Command{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{ID: "a"}})
	before := maps.Clone(s.tools)
	_, _ = stallStep(s, "t1", false, &task.Command{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{ID: "a", Status: task.ToolCompleted}})
	_, _ = stallStep(s, "t1", false, &task.Command{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{ID: "b"}})
	if !maps.Equal(s.tools, before) {
		t.Fatalf("input tools changed: %v, was %v", s.tools, before)
	}
}

// stallEnv 是帶排定通知的測試 hub。
func stallEnv(t *testing.T, s *Settings) *testEnv {
	t.Helper()
	env := newTestHub(t, nil)
	env.clock.mu.Lock()
	env.clock.armed = make(chan *fakeTimer, 256)
	env.clock.mu.Unlock()
	if s != nil {
		if err := env.h.PutSettings(ctxb(), LocalOwner, *s); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

// nextStallTimer 等下一個時限為 d 的計時器排定（其他計時器略過）。
func nextStallTimer(t *testing.T, env *testEnv, d time.Duration) *fakeTimer {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case x := <-env.clock.armed:
			if x.d == d {
				return x
			}
		case <-deadline:
			t.Fatalf("no %v timer", d)
			return nil
		}
	}
}

// startHang 建一個跑 hang 的 Task，等到回合開始且計時已排定。
func startHang(t *testing.T, env *testEnv) (task.TaskID, task.SessionID, *watcher, *fakeTimer) {
	t.Helper()
	id := env.create(t, t.TempDir(), "hang", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnStartedKind)
	timer := nextStallTimer(t, env, stallQuiet)
	w.next(task.NativeSessionBoundKind)
	return id, sid, w, timer
}

func cancelTurn(t *testing.T, env *testEnv, id task.TaskID, sid task.SessionID, w *watcher) {
	t.Helper()
	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{Kind: task.CmdCancelTurn, Session: sid}); err != nil {
		t.Fatal(err)
	}
	w.next(task.TurnCancelledKind)
}

// ST01、ST07：10 分鐘沒有動靜發一條通知，同一段沉默只發一次。
func TestStallNotify(t *testing.T) {
	env := stallEnv(t, nil)
	id, sid, w, _ := startHang(t, env)
	env.clock.Advance(stallQuiet)
	n := nextNotify(t, env)
	if n.Kind != NotifyStalled || n.Task != id || n.Session != sid || n.TaskTitle != "t" ||
		n.Params["agent"] != "Test Agent" || n.Params["minutes"] != "10" {
		t.Fatalf("notify %+v", n)
	}
	// ST07：繼續沉默不再通知。屏障：取消後再跑一輪 crash，它的 failed 通知之前不能有別的。
	env.clock.Advance(3 * stallTool)
	cancelTurn(t, env, id, sid, w)
	expectNoStallBefore(t, env, id, sid, w)
}

// expectNoStallBefore 送一輪 crash，等它的 failed 通知；通知是 FIFO，之前沒有 stalled 即可證明沒發。
func expectNoStallBefore(t *testing.T, env *testEnv, id task.TaskID, sid task.SessionID, w *watcher) {
	t.Helper()
	env.submit(t, id, sid, "crash")
	for {
		n := nextNotify(t, env)
		if n.Kind == NotifyStalled {
			t.Fatalf("stalled notified %+v", n)
		}
		if n.Kind == NotifyFailed {
			return
		}
	}
}

// ST09：回合結束時停掉計時器。
func TestStallStopsWhenTurnEnds(t *testing.T) {
	env := stallEnv(t, nil)
	id, sid, w, timer := startHang(t, env)
	cancelTurn(t, env, id, sid, w)
	if !timer.stopped {
		t.Fatal("stall timer still running after the turn ended")
	}
	env.clock.Advance(stallTool)
	expectNoStallBefore(t, env, id, sid, w)
}

// ST11：開關開著時發、關著時不發，走同一條路：滿時限後取消，再跑一輪 crash，看它的 failed 通知之前有沒有 stalled。
// 開著的那一行證明這條路上點火確實處理了，關著的那一行才不是白過。
func TestStallSetting(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			s := DefaultSettings()
			s.Notify.Stalled = on
			env := stallEnv(t, &s)
			id, sid, w, _ := startHang(t, env)
			env.clock.Advance(stallQuiet)
			cancelTurn(t, env, id, sid, w)
			env.submit(t, id, sid, "crash")
			stalled := false
			for {
				n := nextNotify(t, env)
				if n.Kind == NotifyStalled {
					stalled = true
				}
				if n.Kind == NotifyFailed {
					break
				}
			}
			if stalled != on {
				t.Fatalf("stalled notified = %v with the switch %v", stalled, on)
			}
		})
	}
}

// gatedHang 建一個 Task，它的 Prompt 在 gate 檔案出現前送不出去：回合已開始、agent 一直沒有動靜。
func gatedHang(t *testing.T, env *testEnv, script string) (task.TaskID, task.SessionID, *watcher, string) {
	t.Helper()
	gate := filepath.Join(env.dir, "prompt-gate")
	l := testLaunch()
	l.Env = []task.EnvVar{{Name: "ORIELHUB_TEST_PROMPT_GATE", Value: gate}}
	id := env.create(t, t.TempDir(), script, l)
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnStartedKind)
	nextStallTimer(t, env, stallQuiet)
	w.next(task.NativeSessionBoundKind)
	return id, sid, w, gate
}

// ST08：通知過之後 agent 又有了動靜，再沉默滿時限時再通知一次。
func TestStallNotifiesAgainAfterActivity(t *testing.T) {
	env := stallEnv(t, nil)
	_, _, _, gate := gatedHang(t, env, "say hi; hang")
	env.clock.Advance(stallQuiet)
	if n := nextNotify(t, env); n.Kind != NotifyStalled {
		t.Fatalf("first: %+v", n)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// 輸出是動靜：通知過、計時器已不在，所以排一個新的。
	nextStallTimer(t, env, stallQuiet)
	env.clock.Advance(stallQuiet)
	if n := nextNotify(t, env); n.Kind != NotifyStalled || n.Params["minutes"] != "10" {
		t.Fatalf("second: %+v", n)
	}
}

// ST10：已請求取消、agent 還沒確認時照常計時。
func TestStallWhileCancelling(t *testing.T) {
	env := stallEnv(t, nil)
	id, sid, w, gate := gatedHang(t, env, "hang")
	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{Kind: task.CmdCancelTurn, Session: sid}); err != nil {
		t.Fatal(err)
	}
	w.next(task.TurnCancelRequestedKind) // Prompt 還在送，回合沒有在本地結束
	env.clock.Advance(stallQuiet)
	if n := nextNotify(t, env); n.Kind != NotifyStalled || n.Session != sid {
		t.Fatalf("notify %+v", n)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	w.next(task.TurnCancelledKind)
}

// ST13：hub 關閉時停掉計時器。
func TestStallStopsOnClose(t *testing.T) {
	env := stallEnv(t, nil)
	_, _, _, timer := startHang(t, env)
	env.h.Close()
	if !timer.stopped {
		t.Fatal("stall timer still running after Close")
	}
}

// ST05、ST06：待答的審批停掉計時，決定之後從那一刻重新排定。
func TestStallPausedByAsk(t *testing.T) {
	env := stallEnv(t, nil)
	id := env.create(t, t.TempDir(), "ask a1 rm x", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnStartedKind)
	first := nextStallTimer(t, env, stallQuiet)
	ask := w.next(task.PermissionAskedKind)
	if !first.stopped {
		t.Fatal("stall timer kept running while an ask waits")
	}
	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{Permission: ask.PermissionAsked.ID, Verdict: task.Allow}}); err != nil {
		t.Fatal(err)
	}
	if again := nextStallTimer(t, env, stallQuiet); again == first {
		t.Fatal("no new timer after the decision")
	}
	w.next(task.TurnCompletedKind)
}
