package hub

// rebuild_test.go：Task 列的掛起操作數（D71）與啟動時重建投影。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

var update = flag.Bool("update", false,
	"write testdata/views-v<viewsVersion>.json when it does not exist yet")

// P1–P7：Failed 等於 State.Failed 的條數，隨掛起、重試、撤銷意圖與
// 略過評審增減；歸檔不影響。
func TestProjectionFailedCount(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	created := task.Event{Kind: task.TaskCreatedKind,
		TaskCreated: &task.TaskCreated{Title: "t", Workspace: "w-1", Isolation: task.IsolationWorktree}}
	failed := func(key task.EffectKey) task.Event {
		return task.Event{Kind: task.EffectFailedKind, EffectFailed: &task.EffectFailure{
			Effect: key, Problem: task.Problem{Code: "workspace.unsupported"}}}
	}
	merge := task.Event{Kind: task.MergeRequestedKind, MergeRequested: &task.Intent{
		ID: "i3", Kind: task.IntentMerge, Target: task.Target{Level: task.TargetWorktree}}}
	conflicted := task.Event{Kind: task.MergeConflictedKind,
		MergeConflicted: &task.MergeConflict{Intent: "i3", Files: []string{"a.go"}}}
	cases := []struct {
		name   string
		events []task.Event
		want   int
	}{
		{"P1 no failures", []task.Event{created}, 0},
		{"P2 one failure", []task.Event{created, failed("create_worktree:task")}, 1},
		{"P3 two keys", []task.Event{created, failed("create_worktree:task"), failed("prepare_review")}, 2},
		{"P4 retried", []task.Event{created, failed("create_worktree:task"),
			{Kind: task.EffectRetriedKind, EffectRetried: &task.RetryEffect{Effect: "create_worktree:task"}}}, 0},
		{"P5 intent cancelled", []task.Event{created, merge, conflicted, failed("merge_worktree:i3"),
			{Kind: task.IntentCancelledKind, IntentCancelled: &task.IntentRef{Intent: "i3"}}}, 0},
		{"P6 review skipped", []task.Event{created, failed("prepare_review"),
			{Kind: task.ReviewSkippedKind}}, 0},
		{"P7 archived keeps count", []task.Event{created, failed("create_worktree:task"),
			{Kind: task.TaskArchivedKind}}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for i := range c.events {
				c.events[i].Seq, c.events[i].At = task.Seq(i+1), at
			}
			_, _, v := projections(task.State{ID: "t-1"}, nil, c.events, "")
			if got := v.Task.Failed; got != c.want {
				t.Fatalf("failed %d, want %d", got, c.want)
			}
		})
	}
}

// alwaysTask 在策略 always 的 git 倉庫建一個沒有 agent 的 Task：
// 建 worktree 的 Effect 以 workspace.unsupported 掛起（M1 沒有 worktree）。
func alwaysTask(t *testing.T, env *testEnv) task.TaskID {
	t.Helper()
	dir := gitRepo(t)
	env.store.PutWorkspace(ctxb(), Workspace{
		ID: "w-always", Path: realPath(t, dir), Policy: task.PolicyAlways})
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	return env.create(t, dir, "", testLaunch())
}

// watchRows 收 WatchTasks 推來的列。
func watchRows(t *testing.T, h *Hub) chan TaskView {
	t.Helper()
	ctx, cancel := context.WithCancel(ctxb())
	t.Cleanup(cancel)
	rows := make(chan TaskView, 256)
	go h.WatchTasks(ctx, LocalOwner, func(v TaskView) error {
		select {
		case rows <- v:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	return rows
}

// nextRow 等 id 的下一筆 Failed 為 want 的列。
func nextRow(t *testing.T, rows chan TaskView, id task.TaskID, want int) TaskView {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case v := <-rows:
			if v.ID == id && v.Failed == want {
				return v
			}
		case <-deadline:
			t.Fatalf("no row for %s with failed=%d", id, want)
		}
	}
}

// taskRow 回傳 Tasks() 裡 id 那一列。
func taskRow(t *testing.T, h *Hub, id task.TaskID) TaskView {
	t.Helper()
	rows, err := h.Tasks(ctxb(), LocalOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range rows {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no row for %s", id)
	return TaskView{}
}

// H1–H3：掛起與重試經 WatchTasks 推出；重啟後不需新事件仍是 1。
func TestFailedCountThroughHub(t *testing.T) {
	env := newTestHub(t, nil)
	rows := watchRows(t, env.h)
	id := alwaysTask(t, env)
	w := watch(t, env.h, id)
	e := w.next(task.EffectFailedKind)
	nextRow(t, rows, id, 1)
	if got := taskRow(t, env.h, id).Failed; got != 1 {
		t.Fatalf("H1 tasks failed %d", got)
	}

	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdRetryEffect, RetryEffect: &task.RetryEffect{Effect: e.EffectFailed.Effect}}); err != nil {
		t.Fatal(err)
	}
	nextRow(t, rows, id, 0)
	w.next(task.EffectFailedKind)
	nextRow(t, rows, id, 1)
	w.cancel()

	before, _ := env.store.Read(ctxb(), id, 0, 0)
	env.h.Close()
	env.h = env.restart(t)
	if got := taskRow(t, env.h, id).Failed; got != 1 {
		t.Fatalf("H3 after restart failed %d", got)
	}
	if after, _ := env.store.Read(ctxb(), id, 0, 0); len(after) != len(before) {
		t.Fatalf("H3 restart wrote events: %d → %d", len(before), len(after))
	}
}

// tamper 改寫 MemStore 裡已存的 Task 列，模擬舊版本留下的投影。
func (s *MemStore) tamper(id task.TaskID, f func(*TaskView)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.views[id]
	row := *v.Task
	f(&row)
	v.Task = &row
	s.views[id] = v
}

func (s *MemStore) setViewsVersion(v int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.viewsVersion = v
}

// R1、R5：舊版本的投影在 Start 時重建；UpdatedAt 是最後事件的時間；
// 已結束的 Task 只重寫投影、不建 actor。
func TestRebuildOldViews(t *testing.T) {
	env := newTestHub(t, nil)
	id := alwaysTask(t, env)
	w := watch(t, env.h, id)
	w.next(task.EffectFailedKind)
	w.cancel()
	env.h.Close()
	recs, _ := env.store.Read(ctxb(), id, 0, 0)
	last := recs[len(recs)-1].At
	env.store.tamper(id, func(v *TaskView) { v.Failed, v.UpdatedAt = 0, time.Time{} })
	env.store.setViewsVersion(0)

	env.h = env.restart(t)
	row := taskRow(t, env.h, id)
	if row.Failed != 1 || !row.UpdatedAt.Equal(last) {
		t.Fatalf("R1 row %+v, want failed 1 at %v", row, last)
	}
	if v, _ := env.store.ViewsVersion(ctxb()); v != viewsVersion {
		t.Fatalf("R1 version %d", v)
	}
	if n := env.h.loaded(); n != 0 {
		t.Fatalf("R5 terminal task loaded: %d actors", n)
	}
}

// R2：版本相同時不重寫；R6：存的版本比 hub 新也重建，並記成 hub 的版本。
func TestRebuildOnlyWhenVersionDiffers(t *testing.T) {
	cases := []struct {
		name    string
		stored  int
		rebuilt bool
	}{
		{"R2 same version", viewsVersion, false},
		{"R6 newer version", viewsVersion + 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newTestHub(t, nil)
			id := env.create(t, t.TempDir(), "", testLaunch())
			env.h.Close()
			env.store.tamper(id, func(v *TaskView) { v.Title = "tampered" })
			env.store.setViewsVersion(c.stored)
			env.h = env.restart(t)
			title := taskRow(t, env.h, id).Title
			if rebuilt := title != "tampered"; rebuilt != c.rebuilt {
				t.Fatalf("title %q, rebuilt %v", title, rebuilt)
			}
			if v, _ := env.store.ViewsVersion(ctxb()); v != viewsVersion {
				t.Fatalf("version %d", v)
			}
		})
	}
}

// viewsJSON 是比較用的投影：JSON 不帶 time 的單調時鐘讀數。
func viewsJSON(t *testing.T, s *MemStore, id task.TaskID) string {
	t.Helper()
	v, err := s.ViewsOf(ctxb(), id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// R3、R4：重建出的投影與增量寫出的完全一致（Session、確認、掛起、
// 編輯過的標籤、遮蔽的訊息；待答提問由 TestViewsGolden 覆蓋）；重建
// 做兩次（第一次的版本沒記下）結果仍相同，檢索列不重複。
func TestRebuildMatchesIncremental(t *testing.T) {
	env := newTestHub(t, nil)

	// A：兩則訊息，遮蔽第一則，再改標題（標籤列被取代）。
	a := env.create(t, t.TempDir(), "say hi", testLaunch())
	wa := watch(t, env.h, a)
	e := wa.next(task.MessageAppendedKind)
	sid := e.Session
	wa.next(task.TurnCompletedKind)
	env.submit(t, a, sid, "say yo")
	wa.next(task.TurnCompletedKind)
	if _, err := env.h.Command(ctxb(), LocalOwner, a, task.Command{
		Kind: task.CmdRedactMessage, Session: sid,
		RedactMessage: &task.RedactMessage{Message: e.MessageAppended.Message}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.h.Command(ctxb(), LocalOwner, a, task.Command{
		Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "Beta", Brief: "b2"}}); err != nil {
		t.Fatal(err)
	}
	wa.cancel()

	// B：掛起的 worktree 加一個遠端合併的待確認。
	b := alwaysTask(t, env)
	wb := watch(t, env.h, b)
	wb.next(task.EffectFailedKind)
	if _, err := env.h.Command(ctxb(), Principal{Device: "d9"}, b, task.Command{
		Kind:  task.CmdMerge,
		Merge: &task.Merge{Target: task.Target{Level: task.TargetWorktree}}}); err != nil {
		t.Fatal(err)
	}
	wb.cancel()

	// 兩個 Task 重啟時都不產生新事件：A 的 Session 閒著，B 的
	// 建 worktree 掛起、Plan 跳過它；所以重建後的投影可以直接比對。
	ids := []task.TaskID{a, b}
	env.h.Close()
	want := map[task.TaskID]string{}
	for _, id := range ids {
		want[id] = viewsJSON(t, env.store, id)
	}
	for round := range 2 {
		env.store.mu.Lock()
		if round == 0 { // 第一輪從空的投影重建
			for _, id := range ids {
				delete(env.store.views, id)
				delete(env.store.search, id)
			}
		}
		// 第二輪：投影留著，只當版本「沒記下」，在既有的列上再重建一次。
		env.store.viewsVersion = 0
		env.store.mu.Unlock()
		env.h = env.restart(t)
		env.h.Close()
		for _, id := range ids {
			if got := viewsJSON(t, env.store, id); got != want[id] {
				t.Fatalf("round %d task %s:\nrebuilt %s\nwant %s", round, id, got, want[id])
			}
		}
	}
}

// goldenEvents 是固定的一條 Stream：三個 Session（一個已關閉）、一則
// 訊息與一則遮蔽的訊息、未決的提問、待確認、掛起的 Effect、改過的標籤、置頂、結束的回合。
func goldenEvents() []task.Event {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	launch := task.LaunchSnapshot{Agent: "test", AgentName: "Test Agent",
		Protocol: "test-line", Profile: "default", Approval: task.ApprovalOrielHub}
	events := []task.Event{
		{Kind: task.TaskCreatedKind, TaskCreated: &task.TaskCreated{
			Title: "Alpha", Brief: "first", Workspace: "w-1", Isolation: task.IsolationWorktree}},
		{Kind: task.SessionCreatedKind, Session: "s2",
			SessionCreated: &task.SessionCreated{Launch: launch, Role: task.RoleWorker}},
		{Kind: task.PromptQueuedKind, Session: "s2", PromptQueued: &task.Prompt{ID: "p3", Text: "first"}},
		{Kind: task.TurnStartedKind, Session: "s2", TurnStarted: &task.TurnStarted{Turn: "t4", Prompt: "p3"}},
		{Kind: task.TaskStatusChangedKind, TaskStatusChanged: &task.TaskStatusChanged{
			From: task.TaskTodo, To: task.TaskInProgress, Cause: task.CauseAuto}},
		{Kind: task.MessageAppendedKind, Session: "s2", MessageAppended: &task.MessageAppended{
			Message: "m6", Turn: "t4", Channel: task.ChannelText, Text: "hello"}},
		{Kind: task.MessageAppendedKind, Session: "s2", MessageAppended: &task.MessageAppended{
			Message: "m7", Turn: "t4", Channel: task.ChannelText, Redacted: true}},
		{Kind: task.MessageRedactedKind, Session: "s2", MessageRedacted: &task.RedactMessage{Message: "m7"}},
		{Kind: task.PermissionAskedKind, Session: "s2", PermissionAsked: &task.Ask{ID: "a1", Turn: "t4",
			Request: task.PermissionRequest{Tool: "Bash", Kind: task.ToolExecute, Command: "rm x"}, AskedAt: at}},
		{Kind: task.ConfirmationRequestedKind, ConfirmationRequested: &task.Confirmation{ID: "c10",
			Command: task.Command{Kind: task.CmdMerge, Origin: task.OriginRemote, Device: "d9",
				Merge: &task.Merge{Target: task.Target{Level: task.TargetWorktree}}},
			RequestedBy: "d9", RequestedAt: at}},
		{Kind: task.EffectFailedKind, EffectFailed: &task.EffectFailure{
			Effect: "create_worktree:task", Problem: task.Problem{Code: "workspace.unsupported"}}},
		{Kind: task.TaskEditedKind, TaskEdited: &task.EditTask{Title: "Beta", Brief: "second"}},
		{Kind: task.SessionCreatedKind, Session: "s13",
			SessionCreated: &task.SessionCreated{Launch: launch, Role: task.RoleWorker}},
		{Kind: task.SessionClosedKind, Session: "s13"},
		// 用過的檔案（D107）：Workspace 未登記，路徑保留絕對路徑；搜尋不算；完成了才算。
		{Kind: task.ToolCallStartedKind, Session: "s2", ToolCallStarted: &task.ToolCall{
			ID: "c16", Kind: task.ToolEdit, Paths: []string{"/repo/a.go", "/repo/b.go"}}},
		{Kind: task.ToolCallStartedKind, Session: "s2", ToolCallStarted: &task.ToolCall{
			ID: "c17", Kind: task.ToolSearch, Paths: []string{"/repo"}}},
		{Kind: task.ToolCallUpdatedKind, Session: "s2", ToolCallUpdated: &task.ToolUpdate{ID: "c16", Status: task.ToolCompleted}},
		{Kind: task.ToolCallUpdatedKind, Session: "s2", ToolCallUpdated: &task.ToolUpdate{ID: "c17", Status: task.ToolCompleted}},
		{Kind: task.PromptQueuedKind, Session: "s2", PromptQueued: &task.Prompt{ID: "p20", Text: "see",
			Attachments: []task.Attachment{{Name: "a.go", Path: "/repo/a.go"}}}},
		// 置頂（D109）：Task 列帶置頂的時間。
		{Kind: task.TaskPinnedKind},
		// 結束的回合（D115）：失敗的一輪帶原因與消息的第一個非空行；只有附件的一輪帶附件名；取消的不算。
		{Kind: task.SessionCreatedKind, Session: "s22",
			SessionCreated: &task.SessionCreated{Launch: launch, Role: task.RoleWorker}},
		{Kind: task.PromptQueuedKind, Session: "s22", PromptQueued: &task.Prompt{ID: "p23", Text: "\n  fix it  \nmore"}},
		{Kind: task.TurnStartedKind, Session: "s22", TurnStarted: &task.TurnStarted{Turn: "t24", Prompt: "p23"}},
		{Kind: task.TurnFailedKind, Session: "s22", TurnFailed: &task.TurnFailed{Turn: "t24", Problem: task.Problem{Code: "proc.exited"}}},
		{Kind: task.PromptQueuedKind, Session: "s22", PromptQueued: &task.Prompt{ID: "p26",
			Attachments: []task.Attachment{{Name: "b.go", Path: "/repo/b.go"}}}},
		{Kind: task.TurnStartedKind, Session: "s22", TurnStarted: &task.TurnStarted{Turn: "t27", Prompt: "p26"}},
		{Kind: task.TurnCompletedKind, Session: "s22"},
		{Kind: task.PromptQueuedKind, Session: "s22", PromptQueued: &task.Prompt{ID: "p29", Text: "stop"}},
		{Kind: task.TurnStartedKind, Session: "s22", TurnStarted: &task.TurnStarted{Turn: "t30", Prompt: "p29"}},
		{Kind: task.TurnCancelledKind, Session: "s22"},
	}
	for i := range events {
		events[i].Seq, events[i].At = task.Seq(i+1), at.Add(time.Duration(i)*time.Second)
	}
	return events
}

// G1：固定事件重建出的投影與 testdata/views-v<viewsVersion>.json 相同。
// 投影的算法或形狀改了，這裡就失敗：升 viewsVersion，再以 -update
// 寫出新版本的檔案（-update 不覆寫既有版本的檔案，並刪掉舊版本的）。
func TestViewsGolden(t *testing.T) {
	env := newTestHub(t, nil)
	var b Batch
	for _, e := range goldenEvents() {
		payload, err := task.EncodeEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Records = append(b.Records, Record{Seq: e.Seq, Kind: e.Kind, At: e.At, Payload: payload})
	}
	if err := env.store.Append(ctxb(), "t-golden", 0, b); err != nil {
		t.Fatal(err)
	}
	l, err := env.h.replay(ctxb(), "t-golden", true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(rebuildViews(l), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", fmt.Sprintf("views-v%d.json", viewsVersion))
	want, err := os.ReadFile(path)
	if os.IsNotExist(err) && *update {
		old, _ := filepath.Glob(filepath.Join("testdata", "views-v*.json"))
		for _, p := range old {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatalf("%v: bump viewsVersion if the projection changed, then run with -update", err)
	}
	if string(got) != string(want) {
		t.Fatalf("projection changed but viewsVersion is still %d: bump it and run with -update\ngot:\n%s", viewsVersion, got)
	}
}
