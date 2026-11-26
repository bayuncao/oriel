package hub

import (
	"os"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/task"
)

// relayed 接力給 test agent，回傳新 Session 第一個 Prompt。
func relayed(t *testing.T, env *testEnv, w *watcher, id task.TaskID, from task.SessionID) task.Prompt {
	t.Helper()
	if _, err := env.h.CreateSessionWithAgent(ctxb(), LocalOwner, id, "test", "", task.Prompt{Text: "say reviewed"}, from); err != nil {
		t.Fatal(err)
	}
	return *w.until(func(e task.Event) bool { return e.Kind == task.PromptQueuedKind && e.Session != from }).PromptQueued
}

// m4.md HO01、HO05：Task Worktree 的接力帶上前一 Session 的回覆與 worktree 的改動；前一個 Session 不關。
func TestRelayWorktree(t *testing.T) {
	env := newTestHub(t, func(o *Options) { o.Launcher = newDiscoveryLauncher(t) })
	dir := gitRepo(t)
	env.store.PutWorkspace(ctxb(), Workspace{ID: "w-always", Path: realPath(t, dir), Policy: task.PolicyAlways})
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	id := env.create(t, dir, "write out.txt hello; say Implemented it", testLaunch())
	w := watch(t, env.h, id)
	wt := w.next(task.WorktreeAttachedKind).WorktreeAttached
	from := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnCompletedKind)

	p := relayed(t, env, w, id, from)
	if p.Text != "say reviewed" {
		t.Fatalf("text %q", p.Text)
	}
	for _, want := range []string{"Implemented it", "- added out.txt (+1 -0)\n", "git diff " + wt.BaseCommit} {
		if !strings.Contains(p.Context, want) {
			t.Errorf("missing %q in\n%s", want, p.Context)
		}
	}
	w.next(task.TurnCompletedKind)
	s, err := env.h.Snapshot(ctxb(), LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Sessions) != 2 || s.Sessions[from].Status != task.SessionIdle {
		t.Fatalf("sessions %+v", s.Sessions)
	}
}

// HO08：直接在資料夾裡的 Task 列出它改過的檔案；不是 git 倉庫時不附 git 命令。
func TestRelayFolder(t *testing.T) {
	env := newTestHub(t, func(o *Options) { o.Launcher = newDiscoveryLauncher(t) })
	id := env.create(t, t.TempDir(), "use u1 edit b.txt; use u2 read c.txt; use u3 edit a.txt; say ok", testLaunch())
	w := watch(t, env.h, id)
	from := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnCompletedKind)

	p := relayed(t, env, w, id, from)
	_, changes, _ := strings.Cut(p.Context, "Changes so far:")
	if !strings.Contains(changes, "- a.txt\n- b.txt\n") || strings.Contains(changes, "c.txt") ||
		strings.Contains(changes, "git") {
		t.Fatalf("context\n%s", p.Context)
	}
}

// HO11：讀不到改動時照常接力，寫明沒能讀到。
func TestRelayUnreadChanges(t *testing.T) {
	env := newTestHub(t, func(o *Options) { o.Launcher = newDiscoveryLauncher(t) })
	dir := gitRepo(t)
	env.store.PutWorkspace(ctxb(), Workspace{ID: "w-always", Path: realPath(t, dir), Policy: task.PolicyAlways})
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	id := env.create(t, dir, "say done", testLaunch())
	w := watch(t, env.h, id)
	wt := w.next(task.WorktreeAttachedKind).WorktreeAttached
	from := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnCompletedKind)
	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatal(err)
	}
	if p := relayed(t, env, w, id, from); !strings.Contains(p.Context, "could not read the changes") {
		t.Fatalf("context\n%s", p.Context)
	}
}

// HO04：來源不是這個 Task 的 Session 時不解析 agent、不建 Session。
func TestRelayUnknownSource(t *testing.T) {
	cl := &countingLauncher{inner: newDiscoveryLauncher(t)}
	env := newTestHub(t, func(o *Options) { o.Launcher = cl })
	id := env.create(t, t.TempDir(), "say done", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.TurnCompletedKind)
	_, err := env.h.CreateSessionWithAgent(ctxb(), LocalOwner, id, "test", "", task.Prompt{Text: "x"}, "s-nope")
	if errCode(err) != task.ReasonUnknownSession.Code() || cl.calls() != 0 {
		t.Fatalf("%v, %d resolutions", err, cl.calls())
	}
}
