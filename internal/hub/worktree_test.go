package hub

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/task"
)

// G4：真的 workspace.Git 走完一個 Task Worktree 的一生。agent 在 worktree 裡改檔案，
// 主目錄不受影響；完成 Task 時合併，改動以一個 squash commit 回到主目錄，
// worktree 被刪掉，Session 關閉（D16、D37）。
func TestWorktreeMergeThroughHub(t *testing.T) {
	env := newTestHub(t, nil)
	dir := gitRepo(t)
	env.store.PutWorkspace(ctxb(), Workspace{
		ID: "w-always", Path: realPath(t, dir), Policy: task.PolicyAlways})
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	id := env.create(t, dir, "write out.txt hello", testLaunch())
	w := watch(t, env.h, id)
	wt := w.next(task.WorktreeAttachedKind).WorktreeAttached
	w.next(task.TurnCompletedKind)
	if b, err := os.ReadFile(filepath.Join(wt.Path, "out.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("worktree out.txt = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Fatal("the agent's change reached the workspace before the merge")
	}

	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdSetStatus, SetStatus: &task.SetStatus{Status: task.TaskDone,
			Resolutions: []task.TargetResolution{{Target: task.Target{Level: task.TargetWorktree},
				Resolution: task.Resolution{Action: task.IntentMerge, Message: "Add out"}}}}}); err != nil {
		t.Fatal(err)
	}
	// 完成與合併意圖同一批記下（意圖在執行就能完成，domain.md）；合併成功後關掉 Session（D37）。
	w.until(func(e task.Event) bool {
		return e.Kind == task.TaskStatusChangedKind && e.TaskStatusChanged.To == task.TaskDone
	})
	w.next(task.WorktreeMergedKind)
	w.next(task.SessionClosedKind)

	if b, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("workspace out.txt = %q, %v", b, err)
	}
	cmd := exec.Command("git", "log", "-1", "--format=%s")
	cmd.Dir, cmd.Env = dir, gitTestEnv()
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "Add out" {
		t.Fatalf("last commit %q, %v", out, err)
	}
	if env.h.git.Exists(wt.Path) {
		t.Fatal("the worktree is still there")
	}
}

// Isolation 回答在一個目錄新建 Task 會用的隔離方式（D97），介面據此決定要不要提示共用目錄（T33）。
func TestIsolationQuery(t *testing.T) {
	env := newTestHub(t, nil)
	repo, plain := gitRepo(t), t.TempDir()
	ask := func(dir string) task.IsolationMode {
		t.Helper()
		mode, err := env.h.Isolation(ctxb(), dir)
		if err != nil {
			t.Fatal(err)
		}
		return mode
	}
	if got := ask(repo); got != task.IsolationNone {
		t.Fatalf("git repo, nothing running: %s", got)
	}
	for _, dir := range []string{repo, plain} {
		id := env.create(t, dir, "hang", testLaunch())
		w := watch(t, env.h, id)
		w.next(task.NativeSessionBoundKind)
		w.cancel()
	}
	if got := ask(repo); got != task.IsolationWorktree {
		t.Fatalf("git repo, a task running: %s", got)
	}
	if got := ask(plain); got != task.IsolationNone {
		t.Fatalf("plain folder, a task running: %s", got)
	}
	if _, err := env.h.Isolation(ctxb(), filepath.Join(plain, "nope")); errCode(err) != "workspace.dir_missing" {
		t.Fatalf("missing folder: %v", err)
	}
}
