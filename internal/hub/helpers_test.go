package hub

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/workspace"
)

// fakeClock 是可推進的時鐘：Advance 依到期順序點火計時器。
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	// armed 非 nil 時，每排定一個計時器就送出它（測試以此確認計時已排定）。
	armed chan *fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	due     time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{d: d, due: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	if c.armed != nil {
		// 不阻塞：在鎖裡等測試取走會讓 Advance 與 Now 跟著卡住；滿了直接讓測試失敗。
		select {
		case c.armed <- t:
		default:
			panic("fakeClock: armed channel full; the test is not draining it")
		}
	}
	return t
}

// Advance 推進時鐘並依到期順序點火；回呼在鎖外執行。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	changed := true
	for changed {
		changed = false
		sort.Slice(c.timers, func(i, j int) bool { return c.timers[i].due.Before(c.timers[j].due) })
		for _, t := range c.timers {
			if !t.stopped && !t.due.After(c.now) {
				t.stopped = true
				due = append(due, t.f)
				changed = true
				break
			}
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// testEnv 是測試的 hub 與它的真實現。
type testEnv struct {
	h      *Hub
	store  *MemStore
	clock  *fakeClock
	dir    string // ORIELHUB_TEST_DIR
	wt     string // WorktreeRoot
	notify chan Notification
}

// chanNotifier 是把通知送進 channel 的真實 Notifier。
type chanNotifier struct {
	ch  chan Notification
	err error
}

func (n chanNotifier) Notify(ctx context.Context, x Notification) error {
	if n.err != nil {
		return n.err
	}
	select {
	case n.ch <- x:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newTestHub(t *testing.T, tweak func(*Options)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	clock := &fakeClock{now: time.Now()}
	env := &testEnv{store: NewMemStore(), clock: clock,
		dir: dir, wt: filepath.Join(dir, "wt"),
		notify: make(chan Notification, 64)}
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	o := Options{
		Log:    env.store,
		Config: env.store,
		Index:  env.store,
		Runtimes: map[task.Protocol]Runtime{
			"test-line": lineRuntime{},
		},
		Git: workspace.New(workspace.Options{Runner: runner, Env: func(context.Context) (proc.Env, error) {
			return gitTestEnv(), nil
		}}),
		Env: func(context.Context) (proc.Env, error) {
			return proc.Inherited().With("ORIELHUB_TEST_DIR=" + dir), nil
		},
		Now:          clock.Now,
		Timer:        clock.Timer,
		WorktreeRoot: env.wt,
		Notifier:     chanNotifier{ch: env.notify},
	}
	if tweak != nil {
		tweak(&o)
	}
	env.h = New(o)
	t.Cleanup(env.h.Close)
	if err := env.h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return env
}

// testLaunch 是測試 agent 的 Launch Snapshot。
func testLaunch() task.LaunchSnapshot {
	return task.LaunchSnapshot{
		Agent: "test", AgentName: "Test Agent", Protocol: "test-line",
		Executable: os.Args[0], Profile: "default",
		Approval: task.ApprovalOrielHub,
		Caps:     task.Capabilities{Resume: true, DeleteNative: true},
	}
}

// create 建一個 Task；有 brief 才帶 launch（空 brief + launch 會被
// Decide 拒絕——brief 是第一個 prompt）。
func (env *testEnv) create(t *testing.T, dir, brief string, l task.LaunchSnapshot) task.TaskID {
	t.Helper()
	ct := task.CreateTask{Title: "t", Brief: brief}
	if brief != "" {
		ct.Launches = []task.LaunchSnapshot{l}
	}
	id, _, err := env.h.CreateTask(context.Background(), LocalOwner, dir, ct)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// errCode 是錯誤的穩定代碼；不是帶代碼的錯誤（含 nil）時為空。
func errCode(err error) string {
	if c, ok := err.(interface{ ErrorCode() string }); ok {
		return c.ErrorCode()
	}
	return ""
}

// submit 送一個 prompt。
func (env *testEnv) submit(t *testing.T, id task.TaskID, sid task.SessionID, text string) {
	t.Helper()
	_, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdSubmit, Session: sid,
		Submit: &task.Submit{Prompt: task.Prompt{Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
}

// watcher 是一條訂閱：回放 + 即時事件依序進 events。
type watcher struct {
	t      *testing.T
	events chan task.Event
	done   chan error
	cancel context.CancelFunc
}

func watch(t *testing.T, h *Hub, id task.TaskID) *watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{t: t, events: make(chan task.Event, 512), done: make(chan error, 1), cancel: cancel}
	t.Cleanup(cancel)
	go func() {
		w.done <- h.Subscribe(ctx, LocalOwner, id, 0, func(u Update) error {
			if u.Event == nil {
				return nil // Delta：watcher 只收集事件
			}
			select {
			case w.events <- *u.Event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	return w
}

const testWait = 30 * time.Second

// next 等下一個 kind 的事件；中間的事件被跳過。
func (w *watcher) next(kind task.EventKind) task.Event {
	w.t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case e := <-w.events:
			if e.Kind == kind {
				return e
			}
		case err := <-w.done:
			w.t.Fatalf("subscription ended while waiting for %s: %v", kind, err)
		case <-deadline:
			w.t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

// until 等 pred 成立的事件。
func (w *watcher) until(pred func(task.Event) bool) task.Event {
	w.t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case e := <-w.events:
			if pred(e) {
				return e
			}
		case err := <-w.done:
			w.t.Fatalf("subscription ended: %v", err)
		case <-deadline:
			w.t.Fatal("timed out in until")
		}
	}
}

// sessionOf 回傳 Task 的第一個 Session。
func (env *testEnv) sessionOf(t *testing.T, id task.TaskID) task.SessionID {
	t.Helper()
	w := watch(t, env.h, id)
	e := w.next(task.SessionCreatedKind)
	w.cancel()
	return e.Session
}

// gitRepo 在臨時目錄建一個有 main 分支與一個 commit 的真 repo；
// 找不到 git 就 Skip。
// realPath 是 EvalSymlinks 後的路徑：CreateTask 登記 Workspace
// 存的就是它（/var → /private/var）。
func realPath(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// gitTestEnv 是 hub 測試裡 workspace 跑 git 的環境：不讀使用者的 git 設定，
// 有固定的作者與提交者，合併的結果不隨開發機而變。
func gitTestEnv() proc.Env {
	return proc.Inherited().With("GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
}

// blockWorktrees 讓 env 建不出 worktree：WorktreeRoot 被一個檔案佔著，
// create_worktree 以 workspace.dir_missing 掛起。
func blockWorktrees(t *testing.T, env *testEnv) {
	t.Helper()
	if err := os.WriteFile(env.wt, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	return gitRepoIn(t, t.TempDir())
}

// gitRepoIn 在 dir 建一個有一個 commit、main 分支的倉庫。
func gitRepoIn(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

// loaded 是已載入的 actor 數。
func (h *Hub) loaded() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.actors)
}

// PutRule 直接加一條規則，不經 hub。
func (s *MemStore) PutRule(r task.Rule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, r)
}
