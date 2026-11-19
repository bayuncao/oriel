package hub

import (
	"context"
	"errors"
	"testing"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/workspace"
)

// noGit 是找不到任何 git 的 workspace.Git（PATH 與系統目錄都是空的）。
func noGit(t *testing.T) *workspace.Git {
	t.Helper()
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	return workspace.New(workspace.Options{Runner: runner,
		Env: func(context.Context) (proc.Env, error) {
			return proc.Env{"HOME=" + t.TempDir(), "PATH=" + t.TempDir()}, nil
		},
		Root: t.TempDir()})
}

// W5-hub：沒有 git 時普通資料夾照常建 Task（隔離 none），Workspace 也登記。
func TestCreateTaskNoGit(t *testing.T) {
	env := newTestHub(t, func(o *Options) { o.Git = noGit(t) })
	dir := t.TempDir()
	id, _, err := env.h.CreateTask(ctxb(), LocalOwner, dir,
		task.CreateTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	e := watch(t, env.h, id).next(task.TaskCreatedKind)
	if e.TaskCreated == nil || e.TaskCreated.Isolation != task.IsolationNone {
		t.Fatalf("isolation %+v", e.TaskCreated)
	}
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	var found bool
	for _, x := range ws {
		if x.Path == realPath(t, dir) {
			found = true
		}
	}
	if !found {
		t.Fatal("workspace not registered")
	}
}

// gated 是 Env 先打信號、再等放行或 ctx 結束的 workspace.Git。
func gated(t *testing.T) (*workspace.Git, chan<- struct{}, chan struct{}) {
	t.Helper()
	entered := make(chan struct{}, 1) // 有緩衝：Env 先到也不會丟掉信號
	release := make(chan struct{})
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	g := workspace.New(workspace.Options{Runner: runner,
		Env: func(ctx context.Context) (proc.Env, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
				return proc.Inherited(), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}})
	return g, release, entered
}

// H1：workspace 的環境要等快照；放行後建 Task 成功，
// ctx 取消則回 ctx 錯誤、不建 Task 也不登記 Workspace。
func TestCreateTaskWaitsForEnv(t *testing.T) {
	mk := func(t *testing.T) (*testEnv, chan<- struct{}, chan struct{}) {
		git, release, entered := gated(t)
		return newTestHub(t, func(o *Options) { o.Git = git }), release, entered
	}
	ct := task.CreateTask{Title: "t"}

	t.Run("released", func(t *testing.T) {
		env, release, entered := mk(t)
		done := make(chan error, 1)
		go func() {
			_, _, err := env.h.CreateTask(ctxb(), LocalOwner, t.TempDir(), ct)
			done <- err
		}()
		<-entered
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		env, _, entered := mk(t)
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(ctxb())
		done := make(chan error, 1)
		go func() {
			_, _, err := env.h.CreateTask(ctx, LocalOwner, dir, ct)
			done <- err
		}()
		<-entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
		for _, x := range ws {
			if x.Path == realPath(t, dir) {
				t.Fatal("workspace registered after cancel")
			}
		}
		tasks, _ := env.h.Tasks(ctxb(), LocalOwner)
		if len(tasks) != 0 {
			t.Fatalf("tasks %v", tasks)
		}
	})
}
