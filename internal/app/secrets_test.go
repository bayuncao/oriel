package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/task"
)

// recordingKeychain 是記下寫過哪些鍵的記憶體鑰匙串（讀寫照常由 MemKeychain 做）。
type recordingKeychain struct {
	*provider.MemKeychain
	mu  sync.Mutex
	set []string
}

func (k *recordingKeychain) Set(name string, value []byte) error {
	k.mu.Lock()
	k.set = append(k.set, name)
	k.mu.Unlock()
	return k.MemKeychain.Set(name, value)
}

func (k *recordingKeychain) written() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.set)
}

// secretsApp 是帶記憶體鑰匙串的 app：hub 經同一個鑰匙串解析密鑰。受控家目錄裡有假 grok，
// dir 裡有說 ACP 的假程式 mine-acp（自訂 agent 用）。
type secretsApp struct {
	a    *App
	keys *recordingKeychain
	data string
	env  proc.Env
	dir  string
	// swept 收到每次清理結束（Agents.swept）。
	swept chan struct{}
}

func newSecretsApp(t *testing.T, keys *recordingKeychain, data string) *secretsApp {
	t.Helper()
	home, bin := fakeHome(t)
	writeExe(t, bin, "grok", fakeGrok("1.0.70", filepath.Join(t.TempDir(), "grok.log"), false))
	dir := t.TempDir()
	writeExe(t, dir, "mine-acp", fakeGrok("0.1.0", filepath.Join(t.TempDir(), "mine.log"), false))
	s := &secretsApp{keys: keys, data: data, dir: dir,
		env: proc.Env{"PATH=/usr/bin:/bin", "HOME=" + home, "SHELL=/bin/sh"}}
	s.open(t)
	return s
}

func (s *secretsApp) open(t *testing.T) {
	t.Helper()
	a, err := New(ctxb2(), Options{DataDir: s.data, Inherited: s.env, Root: t.TempDir(), Keychain: s.keys,
		Secret: func(key string) (string, error) {
			b, err := s.keys.Get(key)
			return string(b), err
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	s.a = a
	s.swept = make(chan struct{}, 64)
	a.agents.sweepMu.Lock()
	a.agents.swept = func() { s.swept <- struct{}{} }
	a.agents.sweepMu.Unlock()
	s.settle(t)
}

// settle 等到一次在這之後才開始的清理結束：啟動時的清理在背景跑（AC39），等它做完，之後等到的清理不會是它。
// 鉤子在 sweepMu 下裝好，所以再排的這一次在啟動那次結束之後才開始，結束時鉤子一定在。
func (s *secretsApp) settle(t *testing.T) {
	t.Helper()
	s.a.sweeper.trigger()
	select {
	case <-s.swept:
	case <-time.After(30 * time.Second):
		t.Fatal("no sweep")
	}
	for len(s.swept) > 0 {
		<-s.swept
	}
}

func (s *secretsApp) value(key string) string {
	b, err := s.keys.Get(key)
	if err != nil {
		return ""
	}
	return string(b)
}

// secret 是自訂 agent slug 的 TOKEN 在設定裡的鍵。
func (s *secretsApp) secret(t *testing.T, slug string) string {
	t.Helper()
	r, ok := row(s.a, task.AgentID("custom:"+slug))
	if !ok || r.Custom == nil {
		t.Fatalf("custom:%s not in the catalog", slug)
	}
	for _, v := range r.Custom.Env {
		if v.Name == "TOKEN" {
			return v.Secret
		}
	}
	t.Fatalf("no TOKEN in %+v", r.Custom.Env)
	return ""
}

func (s *secretsApp) retired() []string {
	s.a.agents.mu.Lock()
	defer s.a.agents.mu.Unlock()
	var out []string
	for _, r := range s.a.agents.cfg.Retired {
		out = append(out, r.Key)
	}
	return out
}

func (s *secretsApp) put(t *testing.T, slug, token string, create bool, extra ...agent.EnvInput) {
	t.Helper()
	in := agent.CustomAgentInput{Slug: slug, Command: filepath.Join(s.dir, "mine-acp"), Args: []string{"agent", "stdio"},
		Env: append([]agent.EnvInput{{Name: "TOKEN", Value: token, Secret: true}}, extra...)}
	put := s.a.Agents().EditCustom
	if create {
		put = s.a.Agents().AddCustom
	}
	if err := put(ctxb2(), in); err != nil {
		t.Fatal(err)
	}
	waitReady(t, s.a, task.AgentID("custom:"+slug))
}

// archive 歸檔 Task（還沒歸檔時）。
func (s *secretsApp) archive(t *testing.T, id task.TaskID) {
	t.Helper()
	st, err := s.a.Hub().Snapshot(ctxb2(), hub.LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Archived {
		return
	}
	if _, err := s.a.Hub().Command(ctxb2(), hub.LocalOwner, id, task.Command{Kind: task.CmdArchive}); err != nil {
		t.Fatal(err)
	}
}

// deleteTask 歸檔並刪除 Task，等到刪除之後的那次清理結束（AC39）。
func (s *secretsApp) deleteTask(t *testing.T, id task.TaskID) {
	t.Helper()
	s.archive(t, id)
	for len(s.swept) > 0 { // 之前的清理不算
		<-s.swept
	}
	if err := s.a.Hub().DeleteTask(ctxb2(), hub.LocalOwner, id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.swept:
	case <-time.After(30 * time.Second):
		t.Fatal("no sweep after deleting the task")
	}
}

func snapshotSecret(t *testing.T, a *App, id task.TaskID) string {
	t.Helper()
	st, err := a.Hub().Snapshot(ctxb2(), hub.LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range st.Sessions {
		for _, e := range x.Launch.Env {
			if e.Name == "TOKEN" {
				return e.Secret
			}
		}
	}
	t.Fatalf("no TOKEN in the launch of %s", id)
	return ""
}

// m4.md 規則 7、AC35、AC37–AC39b：換下來的密鑰等沒有 Task 引用了才從鑰匙串刪。
func TestRetiredSecrets(t *testing.T) {
	keys := &recordingKeychain{MemKeychain: provider.NewMemKeychain()}
	data := t.TempDir()
	s := newSecretsApp(t, keys, data)

	// AC37：沒有 Task 用過，換下來的鍵立刻刪掉。
	s.put(t, "fresh", "a1", true)
	unused := s.secret(t, "fresh")
	s.put(t, "fresh", "a2", false)
	if s.value(unused) != "" || slices.Contains(s.retired(), unused) {
		t.Fatalf("an unused retired secret was kept: value %q, retired %v", s.value(unused), s.retired())
	}

	// AC37、AC38：Task 的 Session 快照引用的鍵留著；之後新建的 Session 用新鍵。
	s.put(t, "mine", "v1", true)
	k1 := s.secret(t, "mine")
	t1 := createWithAgent(t, s.a, "custom:mine")
	waitTurn(t, s.a, t1)
	if snapshotSecret(t, s.a, t1) != k1 {
		t.Fatal("the first task does not use the first key")
	}
	s.put(t, "mine", "v2", false)
	k2 := s.secret(t, "mine")
	if k2 == k1 || s.value(k1) != "v1" || !slices.Contains(s.retired(), k1) {
		t.Fatalf("a secret a task uses: value %q, retired %v", s.value(k1), s.retired())
	}
	t2 := createWithAgent(t, s.a, "custom:mine")
	waitTurn(t, s.a, t2)
	if snapshotSecret(t, s.a, t2) != k2 {
		t.Fatal("a new session does not use the new key")
	}

	// 移除：密鑰都記成待清理，被 t2 引用的留著。
	if err := s.a.Agents().Remove(ctxb2(), "custom:mine"); err != nil {
		t.Fatal(err)
	}
	if s.value(k2) != "v2" || !slices.Contains(s.retired(), k2) {
		t.Fatalf("after removal: value %q, retired %v", s.value(k2), s.retired())
	}

	// AC39：歸檔的 Task 仍算引用；刪了它之後的清理刪掉只有它引用的鍵。
	s.archive(t, t2)
	s.deleteTask(t, t1)
	if s.value(k1) != "" || slices.Contains(s.retired(), k1) {
		t.Fatalf("k1 after deleting its only task: value %q, retired %v", s.value(k1), s.retired())
	}
	if s.value(k2) != "v2" {
		t.Fatal("k2 is still used by an archived task but was deleted")
	}

	// AC39a：鑰匙串刪不掉時留在待清理裡；AC39：下次啟動時在背景清理（open 等它做完）。
	keys.Err = errors.New("locked")
	s.deleteTask(t, t2)
	keys.Err = nil
	if s.value(k2) != "v2" || !slices.Contains(s.retired(), k2) {
		t.Fatalf("a failed delete: value %q, retired %v", s.value(k2), s.retired())
	}
	s.a.Close()
	s.open(t)
	if s.value(k2) != "" || len(s.retired()) != 0 {
		t.Fatalf("after reopening: value %q, retired %v", s.value(k2), s.retired())
	}

	// AC39b：設定存不進去時回錯，設定不變，剛寫的新鍵刪掉。
	s.put(t, "late", "b1", true)
	before := s.secret(t, "late")
	s.a.db.Close()
	in := agent.CustomAgentInput{Slug: "late", Command: filepath.Join(s.dir, "mine-acp"),
		Env: []agent.EnvInput{{Name: "TOKEN", Value: "b2", Secret: true}}}
	if err := s.a.Agents().EditCustom(ctxb2(), in); err == nil {
		t.Fatal("an edit with the store closed succeeded")
	}
	if s.secret(t, "late") != before || s.value(before) != "b1" || len(s.retired()) != 0 {
		t.Fatalf("a failed edit changed the config: key %s, retired %v", s.secret(t, "late"), s.retired())
	}
	live := []string{before, s.secret(t, "fresh")} // 設定裡還在用的
	for _, k := range keys.written() {
		if !slices.Contains(live, k) && s.value(k) != "" {
			t.Fatalf("the key %s was left in the keychain", k)
		}
	}
}

// m4.md AC37：解析出、還沒寫進事件流的快照引用的密鑰（launcher 的 lease）在清理時留著。
// 建 Task 停在探測上（具名管道）時換掉密鑰：舊鍵不刪；放行後 Task 的快照引用它，仍不刪。
func TestRetiredSecretLeased(t *testing.T) {
	keys := &recordingKeychain{MemKeychain: provider.NewMemKeychain()}
	s := newSecretsApp(t, keys, t.TempDir())
	gate := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	s.put(t, "mine", "v1", true, agent.EnvInput{Name: "GROK_GATE", Value: gate})
	k1 := s.secret(t, "mine")

	created := make(chan task.TaskID, 1)
	go func() {
		id, _, err := s.a.Hub().CreateTaskWithAgent(ctxb2(), hub.LocalOwner, t.TempDir(), task.CreateTask{Title: "t", Brief: "say hi"}, "custom:mine", "")
		if err != nil {
			t.Error(err)
		}
		created <- id
	}()
	// 打開管道的寫端會等到探測打開讀端：那時解析已登記了它的密鑰。
	w, err := os.OpenFile(gate, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.put(t, "mine", "v2", false, agent.EnvInput{Name: "GROK_GATE", Value: gate})
	if s.value(k1) != "v1" || !slices.Contains(s.retired(), k1) {
		t.Fatalf("a secret of a launch in flight: value %q, retired %v", s.value(k1), s.retired())
	}
	if _, err := w.WriteString("go\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	var id task.TaskID
	select {
	case id = <-created:
	case <-time.After(30 * time.Second):
		t.Fatal("the task was never created")
	}
	if snapshotSecret(t, s.a, id) != k1 {
		t.Fatal("the task does not use the key it resolved")
	}
	// 再清理一次（改寫觸發）：已寫進事件流，Task 引用它，仍留著。
	s.put(t, "mine", "v3", false, agent.EnvInput{Name: "GROK_GATE", Value: gate})
	if s.value(k1) != "v1" {
		t.Fatal("a secret a task uses was deleted")
	}
}

// pausingKeychain 在 Set 時先通知 entered、等 resume 再寫（只停 pause 次），測試據此在寫鑰匙串的途中改設定。
type pausingKeychain struct {
	*provider.MemKeychain
	pause   int
	entered chan struct{}
	resume  chan struct{}
	set     []string // 只在測試的 goroutine 之間依序寫讀（entered / resume 是同步點）
}

func (k *pausingKeychain) Set(name string, value []byte) error {
	k.set = append(k.set, name)
	if k.pause > 0 {
		k.pause--
		k.entered <- struct{}{}
		<-k.resume
	}
	return k.MemKeychain.Set(name, value)
}

// m4.md AC35：密鑰在 write 之外寫進鑰匙串；那時智能體被移除了，存設定前的再檢查回 agent.unknown_agent，
// 剛寫的新鍵刪掉。寫鑰匙串時別的設定改動不必等它。
func TestCustomAgentEditRacesRemoval(t *testing.T) {
	keys := &pausingKeychain{MemKeychain: provider.NewMemKeychain(), entered: make(chan struct{}), resume: make(chan struct{})}
	a, err := New(ctxb2(), Options{DataDir: t.TempDir(), Inherited: proc.Env{"PATH=/usr/bin"}, Root: t.TempDir(), Keychain: keys})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	g := a.Agents()
	in := agent.CustomAgentInput{Slug: "mine", Command: "mine-acp", Env: []agent.EnvInput{{Name: "TOKEN", Value: "v1", Secret: true}}}
	if err := g.AddCustom(ctxb2(), in); err != nil {
		t.Fatal(err)
	}
	keys.pause = 1
	in.Env[0].Value = "v2"
	edited := make(chan error, 1)
	go func() { edited <- g.EditCustom(ctxb2(), in) }()
	<-keys.entered
	if err := g.Remove(ctxb2(), "custom:mine"); err != nil { // 不等寫鑰匙串
		t.Fatal(err)
	}
	close(keys.resume)
	if err := <-edited; problemCode(err) != agent.CodeUnknownAgent {
		t.Fatalf("edit after the agent was removed: %v", err)
	}
	if _, ok := row(a, "custom:mine"); ok {
		t.Fatal("the removed agent came back")
	}
	if len(keys.set) != 2 {
		t.Fatalf("keys written: %v", keys.set)
	}
	for _, k := range keys.set { // v1 的鍵隨移除清理掉；v2 的新鍵在編輯失敗時刪掉
		if _, err := keys.Get(k); !errors.Is(err, provider.ErrNoSecret) {
			t.Errorf("%s is still in the keychain: %v", k, err)
		}
	}
}

// m4.md AC37：清理時被解析登記而跳過的密鑰，解析放掉之後（這裡是建 Task 被取消，快照沒寫進事件流）再清理一次，
// 不必等別的刪除或改寫。
func TestReleasedLeaseSweeps(t *testing.T) {
	keys := &recordingKeychain{MemKeychain: provider.NewMemKeychain()}
	s := newSecretsApp(t, keys, t.TempDir())
	gate := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	s.put(t, "mine", "v1", true, agent.EnvInput{Name: "GROK_GATE", Value: gate})
	k1 := s.secret(t, "mine")

	ctx, cancel := context.WithCancel(ctxb2())
	defer cancel()
	failed := make(chan error, 1)
	go func() {
		_, _, err := s.a.Hub().CreateTaskWithAgent(ctx, hub.LocalOwner, t.TempDir(), task.CreateTask{Title: "t", Brief: "say hi"}, "custom:mine", "")
		failed <- err
	}()
	w, err := os.OpenFile(gate, os.O_WRONLY, 0) // 探測打開讀端時，解析已登記了 k1
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	s.put(t, "mine", "v2", false, agent.EnvInput{Name: "GROK_GATE", Value: gate})
	if s.value(k1) != "v1" || !slices.Contains(s.retired(), k1) {
		t.Fatalf("a leased secret: value %q, retired %v", s.value(k1), s.retired())
	}
	for len(s.swept) > 0 {
		<-s.swept
	}
	cancel()
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("a cancelled create succeeded")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the create never returned")
	}
	select {
	case <-s.swept:
	case <-time.After(30 * time.Second):
		t.Fatal("releasing the lease did not sweep")
	}
	if s.value(k1) != "" || slices.Contains(s.retired(), k1) {
		t.Fatalf("after the release: value %q, retired %v", s.value(k1), s.retired())
	}
}

// m4.md AC22：移除時刪記下的模型仍在 write 下，同一個 ID 不會在這之間被加回、設了預設模型又被刪掉。
func TestRemoveForgetsModelsUnderWrite(t *testing.T) {
	s := newSecretsApp(t, &recordingKeychain{MemKeychain: provider.NewMemKeychain()}, t.TempDir())
	s.put(t, "mine", "v1", true)
	g := s.a.agents
	forget := g.forget
	var held, called bool
	g.forget = func(ctx context.Context, id task.AgentID) error {
		called = true
		if held = !g.write.TryLock(); !held {
			g.write.Unlock()
		}
		return forget(ctx, id)
	}
	if err := g.Remove(ctxb2(), "custom:mine"); err != nil {
		t.Fatal(err)
	}
	if !called || !held {
		t.Fatalf("forget called %v, under write %v", called, held)
	}
}
