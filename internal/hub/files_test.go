package hub

// files_test.go：用過的檔案（D107，m2.md 第 16 節 F）。

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// F22：一批事件裡每個事件的檔案按折疊了它的 State 算，與重放逐事件收集的相同——之後才建的 Fork
// 不改變它之前用的檔案的工作目錄。
func TestProjectionsFilesPerEvent(t *testing.T) {
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	ws := filepath.FromSlash("/repo")
	prev := task.State{ID: "t-1", Sessions: map[task.SessionID]task.Session{"s1": {ID: "s1", Turn: &task.Turn{ID: "t1"}}},
		Order: []task.SessionID{"s1"}}
	done := func(seq task.Seq, id task.ToolCallID) task.Event {
		return task.Event{Seq: seq, At: at, Kind: task.ToolCallUpdatedKind, Session: "s1",
			ToolCallUpdated: &task.ToolUpdate{ID: id, Status: task.ToolCompleted}}
	}
	events := []task.Event{
		{Seq: 1, At: at, Kind: task.ToolCallStartedKind, Session: "s1",
			ToolCallStarted: &task.ToolCall{ID: "c1", Kind: task.ToolRead, Paths: []string{"/repo/a.go"}}},
		done(2, "c1"),
		{Seq: 3, At: at, Kind: task.ForkCreatedKind, Session: "s1", ForkCreated: &task.Worktree{Path: filepath.FromSlash("/wt/fork-1")}},
		{Seq: 4, At: at, Kind: task.ToolCallStartedKind, Session: "s1",
			ToolCallStarted: &task.ToolCall{ID: "c2", Kind: task.ToolEdit, Paths: []string{"/wt/fork-1/b.go"}}},
		done(5, "c2"),
	}
	next, _, v := projections(prev, nil, events, ws)
	if next.Version != 5 || next.Sessions["s1"].Fork == nil {
		t.Fatalf("state not folded: version %d fork %v", next.Version, next.Sessions["s1"].Fork)
	}
	want := []FileUse{
		{Path: "a.go", Via: FileRead, At: at},
		{Path: "b.go", Via: FileEdit, At: at, Edited: true},
	}
	if !slices.Equal(v.Files, want) {
		t.Fatalf("files %+v\nwant %+v", v.Files, want)
	}
	// 與重放（replay 逐事件折疊再收集）相同。
	s, replayed := prev, fileSet{}
	for _, e := range events {
		s = task.Evolve(s, e)
		replayed.add(fileUses(s, e, ws))
	}
	if !slices.Equal(replayed.list(), v.Files) {
		t.Fatalf("replay %+v, batch %+v", replayed.list(), v.Files)
	}
	if v.Task.Version != 5 {
		t.Fatalf("task row version %d, want 5", v.Task.Version)
	}
}

// F10、F19、F20、F22：一個事件用到哪些檔案、記成什麼。工具呼叫完成時才算，分類與路徑取更新改過的。
func TestFileUses(t *testing.T) {
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	ws := filepath.FromSlash("/repo")
	wt := filepath.FromSlash("/wt/t-1")
	fork := filepath.FromSlash("/wt/fork-1")
	turn := &task.Turn{ID: "t1"}
	plain := task.State{ID: "t-1", Sessions: map[task.SessionID]task.Session{"s1": {ID: "s1", Turn: turn}}}
	isolated := task.State{ID: "t-1", Worktree: &task.WorktreeState{Worktree: task.Worktree{Path: wt}},
		Sessions: map[task.SessionID]task.Session{
			"s1": {ID: "s1", Turn: turn},
			"s2": {ID: "s2", Turn: turn, Fork: &task.WorktreeState{Worktree: task.Worktree{Path: fork}}},
		}}
	// started 是開始時就給了分類與路徑的工具呼叫（還沒完成）。
	started := task.State{ID: "t-1", Sessions: map[task.SessionID]task.Session{"s1": {ID: "s1",
		Turn: &task.Turn{ID: "t1", Tools: map[task.ToolCallID]task.ToolRef{"c1": {Kind: task.ToolEdit, Paths: []string{"/repo/a.go"}}}}}}}
	update := func(status task.ToolStatus, kind task.ToolKind, paths ...string) task.Event {
		return task.Event{Kind: task.ToolCallUpdatedKind, Session: "s1", At: at,
			ToolCallUpdated: &task.ToolUpdate{ID: "c1", Status: status, Kind: kind, Paths: paths}}
	}
	tool := func(kind task.ToolKind, paths ...string) task.Event {
		return update(task.ToolCompleted, kind, paths...)
	}
	use := func(path string, via FileVia, edited bool) FileUse {
		return FileUse{Path: path, Via: via, Edited: edited, At: at}
	}
	cases := []struct {
		name  string
		state task.State
		event task.Event
		dir   string
		want  []FileUse
	}{
		{"F19 read", plain, tool(task.ToolRead, "/repo/a.go"), ws, []FileUse{use("a.go", FileRead, false)}},
		{"F19 edit", plain, tool(task.ToolEdit, "/repo/src/b.ts"), ws, []FileUse{use("src/b.ts", FileEdit, true)}},
		{"F19 delete", plain, tool(task.ToolDelete, "/repo/c.md"), ws, []FileUse{use("c.md", FileDelete, true)}},
		{"F19 move records both paths", plain, tool(task.ToolMove, "/repo/old.go", "/repo/new.go"), ws,
			[]FileUse{use("old.go", FileMove, true), use("new.go", FileMove, true)}},
		{"F19 search not recorded", plain, tool(task.ToolSearch, "/repo"), ws, nil},
		{"F19 execute not recorded", plain, tool(task.ToolExecute, "/repo/a.go"), ws, nil},
		{"F19 fetch not recorded", plain, tool(task.ToolFetch, "/repo/a.go"), ws, nil},
		{"F19 think not recorded", plain, tool(task.ToolThink, "/repo/a.go"), ws, nil},
		{"F19 other not recorded", plain, tool(task.ToolOther, "/repo/a.go"), ws, nil},
		{"F19 relative path is under the workdir", plain, tool(task.ToolRead, "lib/x.go"), ws, []FileUse{use("lib/x.go", FileRead, false)}},
		{"F19 workdir itself and empty paths skipped", plain, tool(task.ToolRead, "/repo", "", " "), ws, nil},
		{"F10 outside the workdir keeps the absolute path", plain, tool(task.ToolRead, "/etc/hosts"), ws,
			[]FileUse{{Path: filepath.FromSlash("/etc/hosts"), Outside: true, Via: FileRead, At: at}}},
		{"F10 sibling with a shared prefix is outside", plain, tool(task.ToolRead, "/repository/a.go"), ws,
			[]FileUse{{Path: filepath.FromSlash("/repository/a.go"), Outside: true, Via: FileRead, At: at}}},
		{"F10 unknown workspace keeps the absolute path", plain, tool(task.ToolRead, "/repo/a.go"), "",
			[]FileUse{{Path: filepath.FromSlash("/repo/a.go"), Outside: true, Via: FileRead, At: at}}},
		{"F20 attachments", plain, task.Event{Kind: task.PromptQueuedKind, Session: "s1", At: at,
			PromptQueued: &task.Prompt{Text: "see", Attachments: []task.Attachment{
				{Name: "notes.md", Path: "/repo/docs/notes.md"}, {Name: "blob", MIME: "image/png"}}}}, ws,
			[]FileUse{use("docs/notes.md", FileAttach, false)}},
		{"F22 task worktree", isolated, tool(task.ToolEdit, "/wt/t-1/a.go"), ws, []FileUse{use("a.go", FileEdit, true)}},
		{"F22 fork", isolated, task.Event{Kind: task.ToolCallUpdatedKind, Session: "s2", At: at,
			ToolCallUpdated: &task.ToolUpdate{ID: "c2", Status: task.ToolCompleted, Kind: task.ToolEdit, Paths: []string{"/wt/fork-1/a.go"}}}, ws,
			[]FileUse{use("a.go", FileEdit, true)}},
		{"F22 workspace path from a worktree session is outside", isolated, tool(task.ToolRead, "/repo/a.go"), ws,
			[]FileUse{{Path: filepath.FromSlash("/repo/a.go"), Outside: true, Via: FileRead, At: at}}},
		{"F19 a started call is not a use yet", plain, task.Event{Kind: task.ToolCallStartedKind, Session: "s1", At: at,
			ToolCallStarted: &task.ToolCall{ID: "c1", Kind: task.ToolEdit, Paths: []string{"/repo/a.go"}}}, ws, nil},
		{"F19 kind and paths from the start", started, update(task.ToolCompleted, ""), ws, []FileUse{use("a.go", FileEdit, true)}},
		{"F19 kind and paths changed by an update", started, tool(task.ToolRead, "/repo/b.go"), ws, []FileUse{use("b.go", FileRead, false)}},
		{"F19 running is not a use yet", started, update(task.ToolRunning, ""), ws, nil},
		{"F19 failed or denied is not a use", started, update(task.ToolFailed, ""), ws, nil},
		{"F19 unknown call", plain, update(task.ToolCompleted, ""), ws, nil},
		{"other events", plain, task.Event{Kind: task.MessageAppendedKind, At: at,
			MessageAppended: &task.MessageAppended{Text: "/repo/a.go"}}, ws, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fileUses(task.Evolve(c.state, c.event), c.event, c.dir)
			if !slices.Equal(got, c.want) {
				t.Fatalf("got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// F11、F12、F16、F19、F20、F23：Hub.Files 跨 Task 合併同一個 Workspace 裡的同一路徑，
// 不算已歸檔的 Task；重建投影後結果相同。
func TestHubFiles(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	hosts := filepath.FromSlash("/etc/hosts")
	id1 := env.create(t, dir, "use c1 read a.go; use c2 edit b.go; use c3 read "+hosts+"; use c4 search .; fail c5 edit d.go", testLaunch())
	watch(t, env.h, id1).next(task.TurnCompletedKind)
	t0 := env.clock.Now()
	env.clock.Advance(time.Minute)
	id2 := env.create(t, dir, "use c1 edit a.go", testLaunch())
	watch(t, env.h, id2).next(task.TurnCompletedKind)
	t1 := env.clock.Now()

	tasks, err := env.h.Tasks(ctxb(), LocalOwner)
	if err != nil {
		t.Fatal(err)
	}
	ws := tasks[0].Workspace
	if tasks[1].Workspace != ws {
		t.Fatalf("workspaces %v", tasks)
	}
	files := func(h *Hub) []FileView {
		t.Helper()
		got, err := h.Files(ctxb(), LocalOwner)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := []FileView{
		{Workspace: ws, Path: "a.go", Via: FileEdit, At: t1, Edited: true, Task: id2, Tasks: 2},
		{Workspace: ws, Path: hosts, Outside: true, Via: FileRead, At: t0, Task: id1, Tasks: 1},
		{Workspace: ws, Path: "b.go", Via: FileEdit, At: t0, Edited: true, Task: id1, Tasks: 1},
	}
	if got := files(env.h); !slices.Equal(got, want) {
		t.Fatalf("F11 got %+v\nwant %+v", got, want)
	}

	// F20：附在消息上的檔案。
	snap, err := env.h.Snapshot(ctxb(), LocalOwner, id1)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := env.h.workspaceOf(ws)
	env.clock.Advance(time.Minute)
	t2 := env.clock.Now()
	w1 := watch(t, env.h, id1)
	if _, err := env.h.Command(ctxb(), LocalOwner, id1, task.Command{
		Kind: task.CmdSubmit, Session: snap.Order[0],
		Submit: &task.Submit{Prompt: task.Prompt{Text: "say ok", Attachments: []task.Attachment{
			{Name: "notes.md", Path: filepath.Join(w.Path, "docs", "notes.md")}}}}}); err != nil {
		t.Fatal(err)
	}
	w1.next(task.PromptQueuedKind)
	attach := FileView{Workspace: ws, Path: "docs/notes.md", Via: FileAttach, At: t2, Task: id1, Tasks: 1}
	if got := files(env.h); len(got) != 4 || got[0] != attach {
		t.Fatalf("F20 got %+v", got)
	}

	// F23：投影從日誌重建後相同。
	before := files(env.h)
	env.h.Close()
	env.store.mu.Lock()
	env.store.files = map[task.TaskID]fileSet{}
	env.store.viewsVersion = 1
	env.store.mu.Unlock()
	h2 := env.restart(t)
	if got := files(h2); !slices.Equal(got, before) {
		t.Fatalf("F23 got %+v\nwant %+v", got, before)
	}

	// F16：已歸檔的 Task 不算；a.go 回到 Task 1 的那一次（只讀過）。
	if _, err := h2.Command(ctxb(), LocalOwner, id2, task.Command{Kind: task.CmdArchive}); err != nil {
		t.Fatal(err)
	}
	got := files(h2)
	i := slices.IndexFunc(got, func(f FileView) bool { return f.Path == "a.go" })
	if i < 0 || got[i] != (FileView{Workspace: ws, Path: "a.go", Via: FileRead, At: t0, Task: id1, Tasks: 1}) {
		t.Fatalf("F16 got %+v", got)
	}
}

// F11：同一路徑的後一次取代方式與時間，改過則一直算改過。
func TestMergeUse(t *testing.T) {
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	f := fileSet{}
	f.add([]FileUse{{Path: "a", Via: FileEdit, Edited: true, At: at}})
	f.add([]FileUse{{Path: "a", Via: FileRead, At: at.Add(time.Second)}, {Path: "b", Via: FileRead, At: at}})
	want := []FileUse{{Path: "a", Via: FileRead, Edited: true, At: at.Add(time.Second)}, {Path: "b", Via: FileRead, At: at}}
	if got := f.list(); !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
	if got := (fileSet{}).list(); got != nil {
		t.Fatalf("empty %+v", got)
	}
}
