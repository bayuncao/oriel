package hub

// export_test.go：給同目錄的外部測試套件（package hub_test，
// 例如 sqlite_test.go）用的接縫——接進真實的 Log/Config/Index。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/workspace"
)

// NewTestEnv 建一個用給定 Store 的測試環境（其餘同內部測試）。
func NewTestEnv(t *testing.T, store interface {
	Log
	Config
	Index
}) *testEnv {
	t.Helper()
	dir := t.TempDir()
	clock := &fakeClock{now: time.Now()}
	env := &testEnv{store: nil, clock: clock,
		dir: dir, wt: filepath.Join(dir, "wt"),
		notify: make(chan Notification, 64)}
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	env.h = New(Options{
		Log: store, Config: store, Index: store,
		Runtimes: map[task.Protocol]Runtime{
			"test-line": lineRuntime{},
		},
		Git: workspace.New(workspace.Options{Runner: runner, Env: func(context.Context) (proc.Env, error) {
			return proc.Inherited(), nil
		}}),
		Env: func(context.Context) (proc.Env, error) {
			return proc.Inherited().With("ORIELHUB_TEST_DIR=" + dir), nil
		},
		Now:          clock.Now,
		Timer:        clock.Timer,
		WorktreeRoot: env.wt,
		Notifier:     chanNotifier{ch: env.notify},
	})
	t.Cleanup(env.h.Close)
	if err := env.h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return env
}

// Create 建帶測試 agent launch 的 Task。
func (env *testEnv) Create(t *testing.T, dir, brief string) task.TaskID {
	return env.create(t, dir, brief, testLaunch())
}

// Hub 回傳底層的 Hub。
func (env *testEnv) Hub() *Hub { return env.h }

// Watch 訂閱一個 Task 的事件。
func (env *testEnv) Watch(t *testing.T, id task.TaskID) *watcher {
	return watch(t, env.h, id)
}

// Next 等下一個 kind 的事件。
func (w *watcher) Next(kind task.EventKind) task.Event { return w.next(kind) }

// Until 等 pred 成立的事件。
func (w *watcher) Until(pred func(task.Event) bool) task.Event { return w.until(pred) }

// Stop 結束訂閱。
func (w *watcher) Stop() { w.cancel() }

// SessionOf 回傳 Task 的第一個 SessionID。
func (env *testEnv) SessionOf(t *testing.T, id task.TaskID) task.SessionID {
	return env.sessionOf(t, id)
}

// Submit 送一個 prompt。
func (env *testEnv) Submit(t *testing.T, id task.TaskID, sid task.SessionID, text string) {
	env.submit(t, id, sid, text)
}

// GitRepo 建一個有 main 分支與一個 commit 的臨時倉庫。
func GitRepo(t *testing.T) string { return gitRepo(t) }

// RealPath 是 EvalSymlinks 後的路徑（Workspace 登記的路徑）。
func RealPath(t *testing.T, dir string) string { return realPath(t, dir) }
