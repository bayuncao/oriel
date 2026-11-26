//go:build unix

package hub

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// discoveryLauncher 是 hub 測試用的 Launcher 真實現（規則 5）：真的
// agent.Discovery 在受控的目錄裡找測試 agent，用 agent.Launch 與預設 Profile
// 解析。能否恢復不探測：測試 agent 的協議（test-line）本來就能恢復。
type discoveryLauncher struct {
	disc *agent.Discovery
	defs []agent.Definition
}

func (l discoveryLauncher) Launch(ctx context.Context, id task.AgentID, profile task.ProfileID) (task.LaunchSnapshot, func(), error) {
	for _, def := range l.defs {
		if def.ID != id {
			continue
		}
		if profile != "" && profile != agent.DefaultProfileID { // 只有預設 Profile（m4.md PO17、PO18）
			return task.LaunchSnapshot{}, nil, agent.ErrUnknownProfile
		}
		inst, err := l.disc.Verify(ctx, id)
		if err != nil {
			return task.LaunchSnapshot{}, nil, err
		}
		if err := inst.Unready(); err != nil {
			return task.LaunchSnapshot{}, nil, err
		}
		return agent.Launch(def, inst, agent.DefaultProfile(def), task.Capabilities{Resume: true}), func() {}, nil
	}
	return task.LaunchSnapshot{}, nil, agent.ErrUnknownAgent
}

func (l discoveryLauncher) CheckAgent(id task.AgentID) error {
	if slices.ContainsFunc(l.defs, func(d agent.Definition) bool { return d.ID == id }) {
		return nil
	}
	return agent.ErrUnknownAgent
}

// newDiscoveryLauncher：測試二進位以 orielhub-test-agent 之名放在 bin 裡
// （Definition 的廠商目錄，不靠 PATH，登入 shell 改了 PATH 也找得到）。
func newDiscoveryLauncher(t *testing.T) discoveryLauncher {
	t.Helper()
	bin := t.TempDir()
	if err := os.Symlink(os.Args[0], filepath.Join(bin, "orielhub-test-agent")); err != nil {
		t.Fatal(err)
	}
	def := func(id, name string, proto task.Protocol, cmd string) agent.Definition {
		return agent.Definition{ID: task.AgentID(id), Name: name, Protocol: proto,
			Detect: agent.DetectSpec{Command: cmd, Dirs: []string{bin}}}
	}
	// models 報告模型設置（settings.md MD05、MD07）。
	models := def("models", "Model Agent", "test-line", "orielhub-test-agent")
	models.Launch.Env = []task.EnvVar{{Name: "ORIELHUB_TEST_MODELS", Value: "fast,deep"}}
	defs := []agent.Definition{
		def("test", "Test Agent", "test-line", "orielhub-test-agent"),
		models,
		def("noruntime", "No Runtime", "nope", "orielhub-test-agent"),
		def("gone", "Gone", "test-line", "not-installed-anywhere"),
	}
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	disc := agent.NewDiscovery(agent.DiscoveryOptions{Runner: runner, Root: t.TempDir(),
		Inherited: proc.Env{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "SHELL=/bin/sh"}})
	disc.SetAgents(defs, nil)
	disc.Start()
	t.Cleanup(disc.Close)
	return discoveryLauncher{disc: disc, defs: defs}
}

// CreateTaskWithAgent：先解析 agent，失敗時不建 Task、不登記 Workspace（P03、P04、
// P09、I03）；成功時 Task 帶一個用預設 Profile 的 Session，跑完第一輪（I02）。
func TestCreateTaskWithAgent(t *testing.T) {
	l := newDiscoveryLauncher(t)
	remote := Principal{Device: "phone"}
	for _, tc := range []struct {
		name     string
		launcher Launcher
		p        Principal
		agent    task.AgentID
		brief    string
		code     string // "" 表示成功
	}{
		{name: "I02 ready agent", launcher: l, p: LocalOwner, agent: "test", brief: "say hi"},
		{name: "P09 unknown agent", launcher: l, p: LocalOwner, agent: "nobody", brief: "say hi", code: string(agent.CodeUnknownAgent)},
		{name: "P03 not installed", launcher: l, p: LocalOwner, agent: "gone", brief: "say hi", code: string(agent.CodeNotInstalled)},
		{name: "P04 no runtime for the protocol", launcher: l, p: LocalOwner, agent: "noruntime", brief: "say hi", code: string(CodeAgentUnsupported)},
		{name: "no launcher", launcher: nil, p: LocalOwner, agent: "test", brief: "say hi", code: string(CodeAgentUnsupported)},
		{name: "I03 empty brief", launcher: l, p: LocalOwner, agent: "test", brief: "  ", code: task.ReasonInvalid.Code()},
		{name: "remote, unregistered folder", launcher: l, p: remote, agent: "test", brief: "say hi", code: string(CodeUnknownWorkspace)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestHub(t, func(o *Options) {
				if tc.launcher != nil {
					o.Launcher = tc.launcher
				}
			})
			id, _, err := env.h.CreateTaskWithAgent(ctxb(), tc.p, t.TempDir(), task.CreateTask{Title: "t", Brief: tc.brief}, tc.agent, "")

			if tc.code != "" {
				if errCode(err) != tc.code {
					t.Fatalf("got %v, want %s", err, tc.code)
				}
				tasks, _ := env.h.Tasks(ctxb(), LocalOwner)
				ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
				if len(tasks) != 0 || len(ws) != 0 {
					t.Fatalf("%d tasks, %d workspaces after a rejected creation", len(tasks), len(ws))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			w := watch(t, env.h, id)
			s := w.next(task.SessionCreatedKind)
			lc := s.SessionCreated.Launch
			if lc.Agent != "test" || lc.Profile != agent.DefaultProfileID || lc.Approval != task.ApprovalOrielHub ||
				filepath.Base(lc.Executable) != "orielhub-test-agent" {
				t.Fatalf("launch %+v", lc)
			}
			w.next(task.TurnCompletedKind)
		})
	}
	t.Run("closed hub", func(t *testing.T) {
		env := newTestHub(t, func(o *Options) { o.Launcher = l })
		env.h.Close()
		if _, _, err := env.h.CreateTaskWithAgent(ctxb(), LocalOwner, t.TempDir(), task.CreateTask{Title: "t", Brief: "say hi"}, "test", ""); err != ErrClosed {
			t.Fatalf("got %v", err)
		}
	})
}

// countingLauncher 把解析交給真的 launcher，並數它被呼叫幾次；enter 非 nil 時
// 每次呼叫先經過它（參數是第幾次），測試據此在解析途中插入別的操作。它也數還沒
// release 的解析（held）：每次成功的解析都要正好 release 一次（m4.md AC37）；
// onRelease 非 nil 時在 release 裡呼叫（測試據此核對 release 時快照已寫進事件流）。
type countingLauncher struct {
	inner     Launcher
	enter     func(n int)
	onRelease func()
	mu        sync.Mutex
	n         int
	held      int
}

func (l *countingLauncher) Launch(ctx context.Context, id task.AgentID, profile task.ProfileID) (task.LaunchSnapshot, func(), error) {
	l.mu.Lock()
	l.n++
	n := l.n
	l.mu.Unlock()
	if l.enter != nil {
		l.enter(n)
	}
	snap, release, err := l.inner.Launch(ctx, id, profile)
	if err != nil {
		return snap, nil, err
	}
	l.mu.Lock()
	l.held++
	l.mu.Unlock()
	return snap, func() {
		if l.onRelease != nil {
			l.onRelease()
		}
		l.mu.Lock()
		l.held-- // 多 release 一次會變成負數
		l.mu.Unlock()
		release()
	}, nil
}

// unreleased 是成功解析、還沒 release 的次數；多 release 時是負數。
func (l *countingLauncher) unreleased() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

func (l *countingLauncher) CheckAgent(id task.AgentID) error { return l.inner.CheckAgent(id) }

func (l *countingLauncher) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// CreateSessionWithAgent（m2.md I05–I11，m4.md HO03、HO13、HO17）：給 Task 啟動 agent。
// 拒絕時不建 Session；不必啟動 agent 就能判斷的（agent 或消息是空的、沒有這個
// Task、資料夾裡有 Session 在運行、已歸檔）不解析 agent。
func TestCreateSessionWithAgent(t *testing.T) {
	status := func(s task.TaskStatus) func(task.Event) bool {
		return func(e task.Event) bool {
			return e.Kind == task.TaskStatusChangedKind && e.TaskStatusChanged.To == s
		}
	}
	l := newDiscoveryLauncher(t)
	// bare 建一個沒有 Session 的 Task。
	bare := func(t *testing.T, env *testEnv) task.TaskID {
		t.Helper()
		id, _, err := env.h.CreateTask(ctxb(), LocalOwner, t.TempDir(), task.CreateTask{Title: "t"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// busy 建一個 Session 在運行（等著取消）的 Task。
	busy := func(t *testing.T, env *testEnv) task.TaskID {
		t.Helper()
		id := env.create(t, t.TempDir(), "hang", testLaunch())
		w := watch(t, env.h, id)
		w.next(task.TurnStartedKind)
		w.cancel()
		return id
	}
	archived := func(t *testing.T, env *testEnv) task.TaskID {
		t.Helper()
		id := bare(t, env)
		if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{Kind: task.CmdArchive}); err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("I05 the message is the first prompt", func(t *testing.T) {
		env := newTestHub(t, func(o *Options) { o.Launcher = l })
		id := bare(t, env)
		if _, err := env.h.CreateSessionWithAgent(ctxb(), LocalOwner, id, "test", "", task.Prompt{Text: "say hi"}, ""); err != nil {
			t.Fatal(err)
		}
		w := watch(t, env.h, id)
		lc := w.next(task.SessionCreatedKind).SessionCreated.Launch
		if lc.Agent != "test" || lc.Profile != agent.DefaultProfileID || lc.Approval != task.ApprovalOrielHub ||
			filepath.Base(lc.Executable) != "orielhub-test-agent" {
			t.Fatalf("launch %+v", lc)
		}
		if q := w.next(task.PromptQueuedKind).PromptQueued; q.Text != "say hi" {
			t.Fatalf("first prompt %+v", q)
		}
		w.until(status(task.TaskInProgress))
		w.next(task.TurnCompletedKind)
		w.until(status(task.TaskInReview))
	})

	unknown := func(*testing.T, *testEnv) task.TaskID { return "t-nope" }
	for _, tc := range []struct {
		name    string
		setup   func(*testing.T, *testEnv) task.TaskID
		agent   task.AgentID
		prompt  string
		code    string
		resolve bool // 會解析 agent
	}{
		{name: "I06 blank message", setup: bare, agent: "test", prompt: " \n ", code: task.ReasonInvalid.Code()},
		{name: "I07 no agent", setup: bare, agent: "", prompt: "say hi", code: task.ReasonInvalid.Code()},
		{name: "I08 unknown task", setup: unknown, agent: "test", prompt: "say hi", code: string(CodeUnknownTask)},
		{name: "I09 not installed", setup: bare, agent: "gone", prompt: "say hi", code: string(agent.CodeNotInstalled), resolve: true},
		{name: "I09 no runtime for the protocol", setup: bare, agent: "noruntime", prompt: "say hi", code: string(CodeAgentUnsupported), resolve: true},
		{name: "I09 unknown agent", setup: bare, agent: "nobody", prompt: "say hi", code: string(agent.CodeUnknownAgent), resolve: true},
		{name: "HO03 a session is running", setup: busy, agent: "test", prompt: "say hi", code: task.ReasonWorkdirBusy.Code()},
		{name: "I11 archived", setup: archived, agent: "test", prompt: "say hi", code: task.ReasonArchived.Code()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := &countingLauncher{inner: l}
			env := newTestHub(t, func(o *Options) { o.Launcher = cl })
			id := tc.setup(t, env)
			before := -1
			if s, err := env.h.Snapshot(ctxb(), LocalOwner, id); err == nil {
				before = len(s.Sessions)
			}
			_, err := env.h.CreateSessionWithAgent(ctxb(), LocalOwner, id, tc.agent, "", task.Prompt{Text: tc.prompt}, "")
			if got := errCode(err); got != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			if n := cl.calls(); (n > 0) != tc.resolve {
				t.Fatalf("%d resolutions, want resolving %v", n, tc.resolve)
			}
			if s, err := env.h.Snapshot(ctxb(), LocalOwner, id); err == nil && len(s.Sessions) != before {
				t.Fatalf("%d sessions after a rejection, had %d", len(s.Sessions), before)
			}
			if n := cl.unreleased(); n != 0 {
				t.Fatalf("%d resolutions not released exactly once", n)
			}
		})
	}

	// HO17：同時來的兩次只有一次建得成：第一次停在解析途中時第二次建好了、它的一輪在跑，
	// 第一次放行後在 Actor 裡再核對一次，回 task.workdir_busy。
	t.Run("HO17 two at once", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		open := func() { once.Do(func() { close(release) }) }
		t.Cleanup(open)
		cl := &countingLauncher{inner: l, enter: func(n int) {
			if n == 1 {
				close(entered)
				<-release
			}
		}}
		env := newTestHub(t, func(o *Options) { o.Launcher = cl })
		id := bare(t, env)
		first := make(chan error, 1)
		go func() {
			_, err := env.h.CreateSessionWithAgent(ctxb(), LocalOwner, id, "test", "", task.Prompt{Text: "say hi"}, "")
			first <- err
		}()
		<-entered
		if _, err := env.h.CreateSessionWithAgent(ctxb(), LocalOwner, id, "test", "", task.Prompt{Text: "hang"}, ""); err != nil {
			t.Fatal(err)
		}
		open()
		if err := <-first; errCode(err) != task.ReasonWorkdirBusy.Code() {
			t.Fatalf("first call: %v", err)
		}
		if s, err := env.h.Snapshot(ctxb(), LocalOwner, id); err != nil || len(s.Sessions) != 1 {
			t.Fatalf("%d sessions, %v", len(s.Sessions), err)
		}
		if n := cl.unreleased(); n != 0 {
			t.Fatalf("%d resolutions not released exactly once (the rejected one too)", n)
		}
	})

	t.Run("closed hub", func(t *testing.T) {
		env := newTestHub(t, func(o *Options) { o.Launcher = l })
		id := bare(t, env)
		env.h.Close()
		if _, err := env.h.CreateSessionWithAgent(ctxb(), LocalOwner, id, "test", "", task.Prompt{Text: "say hi"}, ""); err != ErrClosed {
			t.Fatalf("got %v", err)
		}
	})
}

// m4.md AC37：Launcher 的 release 在快照寫進事件流之後才呼叫（建 Task 與給 Task 啟動 agent 都是），
// 解析成功但建不成時也呼叫；每次正好一次。
func TestLaunchReleasedAfterPersisting(t *testing.T) {
	l := newDiscoveryLauncher(t)
	var env *testEnv
	var sessions []int // 每次 release 時，事件流裡帶 Session 的 Task 數
	cl := &countingLauncher{inner: l, onRelease: func() {
		views, err := env.h.Tasks(ctxb(), LocalOwner)
		if err != nil {
			t.Error(err)
		}
		n := 0
		for _, v := range views {
			if v.Sessions > 0 {
				n++
			}
		}
		sessions = append(sessions, n)
	}}
	env = newTestHub(t, func(o *Options) { o.Launcher = cl })
	if _, _, err := env.h.CreateTaskWithAgent(ctxb(), LocalOwner, t.TempDir(), task.CreateTask{Title: "t", Brief: "say hi"}, "test", ""); err != nil {
		t.Fatal(err)
	}
	id, _, err := env.h.CreateTask(ctxb(), LocalOwner, t.TempDir(), task.CreateTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.h.CreateSessionWithAgent(ctxb(), LocalOwner, id, "test", "", task.Prompt{Text: "say hi"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.h.CreateTaskWithAgent(ctxb(), LocalOwner, t.TempDir(), task.CreateTask{Title: "t", Brief: "say hi"}, "noruntime", ""); errCode(err) != string(CodeAgentUnsupported) {
		t.Fatalf("no runtime: %v", err)
	}
	if !slices.Equal(sessions, []int{1, 2, 2}) || cl.unreleased() != 0 {
		t.Fatalf("tasks with a session at each release %v, %d unreleased", sessions, cl.unreleased())
	}
}

// m4.md AC37–AC39：SecretsInUse 收集 agent 的快照（含已歸檔的 Task）引用的密鑰，只看那個 agent 的；
// 讀的是隨事件寫入的投影，重開 hub 後（Task 沒載入）也一樣。
func TestSecretsInUse(t *testing.T) {
	env := newTestHub(t, func(o *Options) {
		o.Secret = func(string) (string, error) { return "v", nil }
	})
	launch := func(agent task.AgentID, key string) task.LaunchSnapshot {
		l := testLaunch()
		l.Agent, l.Env = agent, []task.EnvVar{{Name: "K", Secret: key}}
		return l
	}
	settle := func(id task.TaskID) {
		w := watch(t, env.h, id)
		w.next(task.TurnCompletedKind)
		w.cancel()
	}
	open := env.create(t, t.TempDir(), "say hi", launch("test", "k1"))
	settle(open)
	archived := env.create(t, t.TempDir(), "say hi", launch("test", "k2"))
	settle(archived)
	if _, err := env.h.Command(ctxb(), LocalOwner, archived, task.Command{Kind: task.CmdArchive}); err != nil {
		t.Fatal(err)
	}
	other := env.create(t, t.TempDir(), "say hi", launch("other", "k3"))
	settle(other)
	env.h.Close()

	h := New(Options{Log: env.store, Config: env.store, Index: env.store, Now: env.clock.Now, Timer: env.clock.Timer,
		Runtimes: map[task.Protocol]Runtime{"test-line": lineRuntime{}}})
	t.Cleanup(h.Close)
	if err := h.Start(ctxb()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		agent task.AgentID
		want  []string
	}{{"test", []string{"k1", "k2"}}, {"other", []string{"k3"}}, {"nobody", nil}} {
		used, err := h.SecretsInUse(ctxb(), tc.agent)
		if err != nil || used == nil {
			t.Fatalf("%s: %v, %v", tc.agent, used, err)
		}
		got := slices.Sorted(maps.Keys(used))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.agent, got, tc.want)
		}
	}
	h.mu.Lock()
	_, loaded := h.actors[archived]
	h.mu.Unlock()
	if loaded {
		t.Error("reading an archived task's secrets started its actor")
	}
	h.Close()
	if _, err := h.SecretsInUse(ctxb(), "test"); err != ErrClosed {
		t.Fatalf("closed hub: %v", err)
	}
}
