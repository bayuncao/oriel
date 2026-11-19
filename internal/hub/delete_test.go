package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// archivedTask 建一個跑完一輪的 Task 並歸檔；連線可能還在關，DeleteTask 會等它（DL06）。
func archivedTask(t *testing.T, env *testEnv) (task.TaskID, *watcher) {
	t.Helper()
	id, _, _, w := archiveIdle(t, env, testLaunch())
	env.command(t, id, task.CmdArchive)
	return id, w
}

func deleteErr(h *Hub, p Principal, id task.TaskID) error {
	return h.DeleteTask(context.Background(), p, id)
}

// DL01、DL02、DL04、DL16：刪掉已歸檔的 Task：事件與投影都沒了，WatchTasks 推一列 Removed，
// 開著的訂閱以 unknown_task 結束，Actor 收掉，之後的操作都是 unknown_task。
func TestDeleteTask(t *testing.T) {
	env := newTestHub(t, nil)
	keep := env.create(t, t.TempDir(), "", testLaunch())
	id, w := archivedTask(t, env)

	rows := make(chan TaskView, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watching := make(chan error, 1)
	go func() {
		watching <- env.h.WatchTasks(ctx, LocalOwner, func(v TaskView) error {
			rows <- v
			return nil
		})
	}()
	// 初值：兩個 Task 都在。
	for seen := 0; seen < 2; seen++ {
		select {
		case <-rows:
		case <-time.After(testWait):
			t.Fatal("no initial rows")
		}
	}

	if err := deleteErr(env.h, LocalOwner, id); err != nil {
		t.Fatal(err)
	}
	if recs, _ := env.store.Read(context.Background(), id, 0, 0); len(recs) != 0 {
		t.Fatalf("%d events left", len(recs))
	}
	views, err := env.h.Tasks(context.Background(), LocalOwner)
	if err != nil || len(views) != 1 || views[0].ID != keep {
		t.Fatalf("tasks %+v %v", views, err)
	}
	select {
	case v := <-rows:
		if v.ID != id || !v.Removed {
			t.Fatalf("pushed %+v", v)
		}
	case <-time.After(testWait):
		t.Fatal("no removed row")
	}
	select {
	case err := <-w.done:
		if errCode(err) != string(CodeUnknownTask) {
			t.Fatalf("subscription ended with %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("subscription still open")
	}
	env.h.mu.Lock()
	_, loaded := env.h.actors[id]
	env.h.mu.Unlock()
	if loaded {
		t.Fatal("actor still loaded")
	}
	if err := deleteErr(env.h, LocalOwner, id); errCode(err) != string(CodeUnknownTask) {
		t.Fatalf("second delete %v", err)
	}
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{Kind: task.CmdUnarchive}); errCode(err) != string(CodeUnknownTask) {
		t.Fatalf("command after delete %v", err)
	}
	if _, err := env.h.Snapshot(context.Background(), LocalOwner, id); errCode(err) != string(CodeUnknownTask) {
		t.Fatalf("snapshot after delete %v", err)
	}
	// 留下的 Task 不受影響。
	if _, err := env.h.Snapshot(context.Background(), LocalOwner, keep); err != nil {
		t.Fatal(err)
	}
}

// DL03、DL04、DL05：沒歸檔、不存在、遠程都不刪。
func TestDeleteTaskRefused(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	if err := deleteErr(env.h, LocalOwner, id); errCode(err) != string(CodeNotArchived) {
		t.Fatalf("unarchived: %v", err)
	}
	if recs, _ := env.store.Read(context.Background(), id, 0, 0); len(recs) == 0 {
		t.Fatal("events deleted")
	}
	if err := deleteErr(env.h, LocalOwner, "t-missing"); errCode(err) != string(CodeUnknownTask) {
		t.Fatalf("missing: %v", err)
	}
	env.command(t, id, task.CmdArchive)
	if err := deleteErr(env.h, Principal{Device: "d9"}, id); errCode(err) != string(CodeNeedsDesktop) {
		t.Fatalf("remote: %v", err)
	}
	if recs, _ := env.store.Read(context.Background(), id, 0, 0); len(recs) == 0 {
		t.Fatal("events deleted")
	}
}

// closingTask 建一個跑完一輪的 Task，歸檔，但 agent 行程停在 close gate 上關不完；回傳打開 gate 的函式。
func closingTask(t *testing.T, env *testEnv) (task.TaskID, func()) {
	t.Helper()
	gate := filepath.Join(env.dir, "close-gate")
	l := testLaunch()
	l.Env = []task.EnvVar{{Name: "ORIELHUB_TEST_CLOSE_GATE", Value: gate}}
	id, _, _, w := archiveIdle(t, env, l)
	w.cancel()
	env.command(t, id, task.CmdArchive)
	return id, func() {
		if err := os.WriteFile(gate, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// queueDelete 在背景送出刪除，等它在 Actor 裡排上（等著連線關完）才回來。
func queueDelete(t *testing.T, env *testEnv, id task.TaskID) <-chan error {
	t.Helper()
	queued := make(chan struct{}, 1)
	env.h.hooks.deleteQueued = func(task.TaskID) { queued <- struct{}{} }
	done := make(chan error, 1)
	go func() { done <- deleteErr(env.h, LocalOwner, id) }()
	select {
	case <-queued:
	case err := <-done:
		t.Fatalf("delete returned before the conn closed: %v", err)
	case <-time.After(testWait):
		t.Fatal("delete never queued")
	}
	return done
}

// DL06：歸檔時的連線還沒關完：刪除等它關完才刪。
func TestDeleteTaskWaitsForClose(t *testing.T) {
	env := newTestHub(t, nil)
	id, open := closingTask(t, env)
	done := queueDelete(t, env, id)
	if recs, _ := env.store.Read(context.Background(), id, 0, 0); len(recs) == 0 {
		t.Fatal("deleted before the conn closed")
	}
	open()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testWait):
		t.Fatal("delete never finished")
	}
	if recs, _ := env.store.Read(context.Background(), id, 0, 0); len(recs) != 0 {
		t.Fatalf("%d events left", len(recs))
	}
}

// DL07：刪除排著時先到的取消歸檔讓刪除失敗，Task 留著。
func TestDeleteTaskLosesToUnarchive(t *testing.T) {
	env := newTestHub(t, nil)
	id, open := closingTask(t, env)
	done := queueDelete(t, env, id)
	env.command(t, id, task.CmdUnarchive)
	open()
	select {
	case err := <-done:
		if errCode(err) != string(CodeNotArchived) {
			t.Fatalf("delete %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("delete never answered")
	}
	if s, err := env.h.Snapshot(context.Background(), LocalOwner, id); err != nil || s.Archived {
		t.Fatalf("state %+v %v", s.Archived, err)
	}
}

// DL08：刪除後重啟，Task 不會回來。
func TestDeleteTaskSurvivesRestart(t *testing.T) {
	env := newTestHub(t, nil)
	id, w := archivedTask(t, env)
	w.cancel()
	if err := deleteErr(env.h, LocalOwner, id); err != nil {
		t.Fatal(err)
	}
	env.h.Close()
	h := env.restart(t)
	views, err := h.Tasks(context.Background(), LocalOwner)
	if err != nil || len(views) != 0 {
		t.Fatalf("tasks %+v %v", views, err)
	}
	if _, err := h.Snapshot(context.Background(), LocalOwner, id); errCode(err) != string(CodeUnknownTask) {
		t.Fatalf("snapshot %v", err)
	}
}

// DL10：還有開著的 worktree（Task Worktree 或 Fork）不刪；合併或丟棄了（Outcome 不是 open）可以刪。
// M2 建不出 worktree，以 State 直接核對。
func TestDeletableWorktrees(t *testing.T) {
	base := task.State{Archived: true}
	wt := base
	wt.Worktree = &task.WorktreeState{Outcome: task.OutcomeOpen}
	merged := base
	merged.Worktree = &task.WorktreeState{Outcome: task.OutcomeMerged}
	fork := base
	fork.Sessions = map[task.SessionID]task.Session{"s-1": {Fork: &task.WorktreeState{Outcome: task.OutcomeOpen}}}
	for _, c := range []struct {
		name  string
		state task.State
		want  string
	}{
		{"archived", base, ""},
		{"open task worktree", wt, task.ReasonUnresolvedChanges.Code()},
		{"merged task worktree", merged, ""},
		{"open fork", fork, task.ReasonUnresolvedChanges.Code()},
		{"not archived", task.State{}, string(CodeNotArchived)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := errCode((&actor{state: c.state}).deletable()); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// 呼叫者在等連線關完時走了：DeleteTask 回 ctx 的錯誤，不再卡著；排上的刪除照樣在連線關完後做。
func TestDeleteTaskCallerGone(t *testing.T) {
	env := newTestHub(t, nil)
	id, open := closingTask(t, env)
	queued := make(chan struct{}, 1)
	env.h.hooks.deleteQueued = func(task.TaskID) { queued <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.h.DeleteTask(ctx, LocalOwner, id) }()
	select {
	case <-queued:
	case <-time.After(testWait):
		t.Fatal("delete never queued")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("delete %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("DeleteTask still waiting after its ctx ended")
	}
	rows := make(chan TaskView, 64)
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	go env.h.WatchTasks(wctx, LocalOwner, func(v TaskView) error { rows <- v; return nil })
	deadline := time.After(testWait)
	// 先等到初值裡的這一列：觀察者已登記，之後的 Removed 不會漏掉。
	for seen := false; !seen; {
		select {
		case v := <-rows:
			seen = v.ID == id
		case <-deadline:
			t.Fatal("no initial row")
		}
	}
	open()
	for {
		select {
		case v := <-rows:
			if v.ID == id && v.Removed {
				if recs, _ := env.store.Read(context.Background(), id, 0, 0); len(recs) != 0 {
					t.Fatalf("%d events left", len(recs))
				}
				return
			}
		case <-deadline:
			t.Fatal("the queued delete never ran")
		}
	}
}
