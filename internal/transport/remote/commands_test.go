//go:build unix

package remote

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// 遠程能做的事（docs/acceptance/remote.md 第 4 節 RP）在傳輸這一層的部分：命令、建 Task、
// agent 列表與用量經同一條加密通道，以這台設備的身份交給 hub。遠程來源的規則本身
// （審批、確認、隔離）在 task 與 hub 的測試裡。

// result 把成功的回覆解成 T。
func result[T any](t *testing.T, r Reply) T {
	t.Helper()
	if r.Error != nil || r.Result == nil {
		t.Fatalf("got %+v (error %+v), want a result", r, r.Error)
	}
	var v T
	if err := json.Unmarshal(r.Result, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *env) snapshot(id task.TaskID) task.State {
	e.t.Helper()
	s, err := e.hub.Snapshot(ctx(e.t), hub.LocalOwner, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func TestRP15LaunchingCommandsNotAllowed(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	id := e.newTask("t")
	before := jsonOf(t, e.snapshot(id))
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	launch := task.LaunchSnapshot{Agent: "evil", Protocol: "acp"}
	cases := []struct {
		name string
		cmd  task.Command
	}{
		{"create session", task.Command{Kind: task.CmdCreateSession, CreateSession: &task.CreateSession{Launch: launch}}},
		{"fan out", task.Command{Kind: task.CmdFanOut, FanOut: &task.FanOut{Launches: []task.LaunchSnapshot{launch}}}},
	}
	for _, tc := range cases {
		r := c.call("Command", CommandParams{Task: id, Command: tc.cmd})
		if r.Error == nil || r.Error.Code != string(CodeNotAllowed) {
			t.Errorf("%s: got %+v (error %+v), want %s", tc.name, r, r.Error, CodeNotAllowed)
		}
	}
	if jsonOf(t, e.snapshot(id)) != before {
		t.Fatal("task changed")
	}
}

func TestRP16CommandsRunAsTheDevice(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	id := e.newTask("t")
	phone, _ := e.device("iPhone")
	c := e.join(phone)

	seq := result[task.Seq](t, c.call("Command", CommandParams{Task: id, Command: task.Command{Kind: task.CmdPin}}))
	if s := e.snapshot(id); s.PinnedAt.IsZero() || s.Version != seq {
		t.Fatalf("after pin: pinned at %v, version %d; reply %d", s.PinnedAt, s.Version, seq)
	}

	cases := []struct {
		name   string
		params any
		code   string
	}{
		// 桌面確認一個不存在的確認是無操作；遠程一律 needs_desktop（RP09、D34）。即使 App
		// 自稱來自桌面，Origin 也由 Mac 按連線填入。
		{"RP09 confirm, origin claimed as desktop", CommandParams{Task: id, Command: task.Command{
			Kind: task.CmdConfirm, Origin: task.OriginDesktop, Device: "someone-else",
			Confirm: &task.ConfirmationRef{Confirmation: "c-none"}}}, task.ReasonNeedsDesktop.Code()},
		{"unknown task", CommandParams{Task: "t-none", Command: task.Command{Kind: task.CmdPin}}, string(hub.CodeUnknownTask)},
		{"create task goes through CreateTask", CommandParams{Task: id, Command: task.Command{Kind: task.CmdCreateTask}},
			task.ReasonInvalid.Code()},
		{"unknown field in the command", json.RawMessage(`{"task":"` + string(id) + `","command":{"kind":"pin","x":1}}`),
			string(CodeBadRequest)},
	}
	for _, tc := range cases {
		r := c.call("Command", tc.params)
		if r.Error == nil || r.Error.Code != tc.code {
			t.Errorf("%s: got %+v (error %+v), want %s", tc.name, r, r.Error, tc.code)
		}
	}
}

func TestRP10CreateTask(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	if _, _, err := e.hub.CreateTask(ctx(t), hub.LocalOwner, t.TempDir(), task.CreateTask{Title: "first"}); err != nil {
		t.Fatal(err)
	}
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	// App 從 Workspaces 選資料夾：登記的是解開符號連結後的真實路徑，遠程送來的
	// 路徑 hub 不解析（不碰檔案系統），只與登記的比對。
	ws := result[[]hub.Workspace](t, c.call("Workspaces", nil))
	if len(ws) != 1 {
		t.Fatalf("workspaces %+v", ws)
	}
	registered := ws[0].Path

	id := result[task.TaskID](t, c.call("CreateTask", CreateTaskParams{Dir: registered, Title: "from the phone", Brief: "b"}))
	if s := e.snapshot(id); s.Title != "from the phone" || s.Brief != "b" {
		t.Fatalf("created %+v", s)
	}

	cases := []struct {
		name   string
		params CreateTaskParams
		code   string
	}{
		{"RP11 unregistered folder", CreateTaskParams{Dir: t.TempDir(), Title: "t"}, string(hub.CodeUnknownWorkspace)},
		// 帶 agent 時經 hub 的 Launcher 解析；這個 hub 沒有 Launcher，錯誤原樣回來。
		{"agent resolved by the hub", CreateTaskParams{Dir: registered, Title: "t", Brief: "b", Agent: "test"},
			string(hub.CodeAgentUnsupported)},
	}
	for _, tc := range cases {
		r := c.call("CreateTask", tc.params)
		if r.Error == nil || r.Error.Code != tc.code {
			t.Errorf("%s: got %+v (error %+v), want %s", tc.name, r, r.Error, tc.code)
		}
	}
}

// catalog 是 Agents.List 的 agent 目錄真實現：固定的定義接上真的 Discovery，
// 同 app 的 *Agents。
type catalog struct {
	defs []agent.Definition
	disc *agent.Discovery
}

func (c catalog) Definitions() []agent.Definition     { return c.defs }
func (c catalog) Installations() []agent.Installation { return c.disc.Installations() }

func newCatalog(t *testing.T) catalog {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "orielhub-test-agent"), []byte("#!/bin/sh\necho 1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	def := func(id, cmd string) agent.Definition {
		return agent.Definition{ID: task.AgentID(id), Name: id, Protocol: "acp",
			Detect: agent.DetectSpec{Command: cmd, Dirs: []string{bin}}}
	}
	defs := []agent.Definition{def("installed", "orielhub-test-agent"), def("gone", "not-installed-anywhere")}
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	disc := agent.NewDiscovery(agent.DiscoveryOptions{Runner: runner, Root: t.TempDir(),
		Inherited: proc.Env{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "SHELL=/bin/sh"}})
	disc.SetAgents(defs, nil)
	disc.Start()
	t.Cleanup(disc.Close)
	if err := disc.Refresh(ctx(t)); err != nil {
		t.Fatal(err)
	}
	return catalog{defs: defs, disc: disc}
}

func TestRP17AgentsList(t *testing.T) {
	e := newEnv(t)
	cat := newCatalog(t)
	e.agents = cat
	e.serve(Options{})
	phone, _ := e.device("iPhone")
	c := e.join(phone)

	r := c.call("Agents.List", nil)
	want := agent.Rows(cat.Definitions(), cat.Installations())
	if got := compact(t, r.Result); r.Error != nil || got != jsonOf(t, want) {
		t.Fatalf("Agents.List\n got %s (error %+v)\nwant %s", got, r.Error, jsonOf(t, want))
	}
	if len(want) != 2 || want[0].Installation.Agent != "installed" {
		t.Fatalf("rows %+v", want)
	}
}

func TestRP12Usage(t *testing.T) {
	e := newEnv(t)
	at := e.clock.Now().Add(-time.Hour)
	if err := e.usageStore.ApplyUsage(ctx(t), usage.Batch{Records: []usage.Record{{
		Agent: "claude", Key: "k1", At: at, Model: "m", Tokens: usage.Tokens{Input: 10, Output: 5}}}}); err != nil {
		t.Fatal(err)
	}
	e.serve(Options{})
	phone, _ := e.device("iPhone")
	c := e.join(phone)

	want, err := e.usage.Summary(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if r := c.call("Usage.Summary", nil); r.Error != nil || compact(t, r.Result) != jsonOf(t, want) {
		t.Fatalf("Usage.Summary\n got %s (error %+v)\nwant %s", r.Result, r.Error, jsonOf(t, want))
	}

	watch := c.request("Usage.Watch", nil)
	first := pushUntil(c, watch, any2[usage.ReadState])
	refreshed := result[usage.ReadState](t, c.call("Usage.Refresh", nil))
	if refreshed.Rev <= first.Rev {
		t.Fatalf("refresh rev %d, first pushed rev %d", refreshed.Rev, first.Rev)
	}
	pushUntil(c, watch, func(s usage.ReadState) bool { return s.Rev >= refreshed.Rev })
	// 用量的 Watch 在 ctx 結束時回 ctx 的錯誤；App 取消仍以 end 結束（RQ04）。
	c.cancel(watch)
	if r := endOf(c, watch); !r.End || r.Error != nil {
		t.Fatalf("got %+v (error %+v), want end", r, r.Error)
	}
}
