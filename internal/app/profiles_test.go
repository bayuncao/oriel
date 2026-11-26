package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/task"
)

// profiles 是 agent 在 Agent 頁那一列的 Profile（PO30）。
func (s *secretsApp) profiles(t *testing.T, id task.AgentID) []agent.Profile {
	t.Helper()
	r, ok := row(s.a, id)
	if !ok {
		t.Fatalf("%s not in the catalog", id)
	}
	return r.Profiles
}

func profileSecret(p agent.Profile, name string) string {
	for _, v := range p.Env {
		if v.Name == name {
			return v.Secret
		}
	}
	return ""
}

// m4.md PO01–PO05、PO13、PO15–PO17：Profile 存進設定、解析進 Session 的快照；它的密鑰存鑰匙串，刪掉後等
// 沒有 Task 引用了才清理。
func TestProfiles(t *testing.T) {
	keys := &recordingKeychain{MemKeychain: provider.NewMemKeychain()}
	s := newSecretsApp(t, keys, t.TempDir())
	waitReady(t, s.a, "grok")
	g := s.a.Agents()

	// PO01：只有預設 Profile；grok 有自己的寬鬆模式。
	r, _ := row(s.a, "grok")
	if len(r.Profiles) != 1 || r.Profiles[0].ID != agent.DefaultProfileID || !r.Native {
		t.Fatalf("PO01 row %+v", r)
	}
	if c, _ := row(s.a, "codex"); c.Native {
		t.Fatal("codex has no permissive mode")
	}

	// 輸入錯了不碰鑰匙串（PO07、PO10、unknown_agent）。
	for _, tc := range []struct {
		name string
		in   agent.ProfileInput
		want agent.ProblemCode
	}{
		{"PO07", agent.ProfileInput{Agent: "codex", Name: "Y", Approval: task.ApprovalNative}, agent.CodeNativeUnsupported},
		{"PO10", agent.ProfileInput{Agent: "grok", Name: "Y", AskTimeout: -time.Second}, agent.CodeInvalidTimeout},
		{"unknown agent", agent.ProfileInput{Agent: "nobody", Name: "Y"}, agent.CodeUnknownAgent},
	} {
		tc.in.Env = []agent.EnvInput{{Name: "TOKEN", Value: "x", Secret: true}}
		if _, err := g.AddProfile(ctxb2(), tc.in); problemCode(err) != tc.want {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	if w := keys.written(); len(w) != 0 {
		t.Fatalf("bad input wrote %v", w)
	}

	// PO03、PO13：新的 ID；密鑰存鑰匙串，設定只存鍵。
	in := agent.ProfileInput{Agent: "grok", Name: "Fast", Approval: task.ApprovalNative, ExtraArgs: []string{"--trace"},
		Env:        []agent.EnvInput{{Name: "TOKEN", Value: "t1", Secret: true}, {Name: "MODE", Value: "x"}},
		AskTimeout: 5 * time.Minute, TimeoutVerdict: task.Deny}
	id, err := g.AddProfile(ctxb2(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(id), "p_") {
		t.Fatalf("PO03 id %q", id)
	}
	ps := s.profiles(t, "grok")
	if len(ps) != 2 || ps[1].ID != id || ps[1].Name != "Fast" {
		t.Fatalf("after adding %+v", ps)
	}
	k1 := profileSecret(ps[1], "TOKEN")
	if !strings.HasPrefix(k1, "profile:grok:"+string(id)+":TOKEN:") || s.value(k1) != "t1" {
		t.Fatalf("PO13 key %q value %q", k1, s.value(k1))
	}
	if _, err := g.AddProfile(ctxb2(), agent.ProfileInput{Agent: "grok", Name: "fast"}); problemCode(err) != agent.CodeProfileNameTaken {
		t.Fatalf("PO03 name taken: %v", err)
	}

	// PO17：沒有的 Profile，不建 Task。
	if _, _, err := s.a.Hub().CreateTaskWithAgent(ctxb2(), hub.LocalOwner, t.TempDir(),
		task.CreateTask{Title: "t", Brief: "say hi"}, "grok", "p_none"); problemCode(err) != agent.CodeUnknownProfile {
		t.Fatalf("PO17: %v", err)
	}

	// PO16：Session 的快照用選的 Profile。
	tid, _, err := s.a.Hub().CreateTaskWithAgent(ctxb2(), hub.LocalOwner, t.TempDir(),
		task.CreateTask{Title: "t", Brief: "say hi"}, "grok", id)
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, s.a, tid)
	launch := func() task.LaunchSnapshot {
		st, err := s.a.Hub().Snapshot(ctxb2(), hub.LocalOwner, tid)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range st.Sessions {
			return x.Launch
		}
		t.Fatal("no session")
		return task.LaunchSnapshot{}
	}
	l := launch()
	if l.Profile != id || l.ProfileName != "Fast" || l.Approval != task.ApprovalNative ||
		!slices.Equal(l.Args, []string{"agent", "--always-approve", "stdio", "--trace"}) ||
		l.AskTimeout != 5*time.Minute || l.TimeoutVerdict != task.Deny || snapshotSecret(t, s.a, tid) != k1 {
		t.Fatalf("PO16 launch %+v", l)
	}

	// PO15：改了 Profile，已有 Session 的快照不變；換下來的密鑰被它引用，留著。
	in.ID, in.Name, in.Env = id, "Faster", []agent.EnvInput{{Name: "TOKEN", Value: "t2", Secret: true}}
	if err := g.EditProfile(ctxb2(), in); err != nil {
		t.Fatal(err)
	}
	if l2 := launch(); l2.ProfileName != "Fast" || snapshotSecret(t, s.a, tid) != k1 {
		t.Fatalf("PO15 launch changed: %+v", l2)
	}
	k2 := profileSecret(s.profiles(t, "grok")[1], "TOKEN")
	if k2 == k1 || s.value(k1) != "t1" || !slices.Contains(s.retired(), k1) {
		t.Fatalf("PO15 secrets: k2 %q, k1 value %q, retired %v", k2, s.value(k1), s.retired())
	}

	// PO02：改了預設的，重開還在；名稱照舊（PO32）。
	if err := g.EditProfile(ctxb2(), agent.ProfileInput{Agent: "grok", ID: agent.DefaultProfileID, Name: "X",
		ExtraArgs: []string{"--quiet"}}); err != nil {
		t.Fatal(err)
	}
	s.a.Close()
	s.open(t)
	g = s.a.Agents()
	ps = s.profiles(t, "grok")
	if len(ps) != 2 || ps[0].Name != "Default" || !slices.Equal(ps[0].ExtraArgs, []string{"--quiet"}) || ps[1].Name != "Faster" {
		t.Fatalf("PO02 after reopening %+v", ps)
	}

	// PO04、PO05：預設的刪不掉；刪別的，它的密鑰清理掉，被 Task 引用的等 Task 刪了才清理。
	if err := g.RemoveProfile(ctxb2(), "grok", agent.DefaultProfileID); problemCode(err) != agent.CodeDefaultProfile {
		t.Fatalf("PO04: %v", err)
	}
	if err := g.RemoveProfile(ctxb2(), "grok", id); err != nil {
		t.Fatal(err)
	}
	if ps := s.profiles(t, "grok"); len(ps) != 1 {
		t.Fatalf("PO05 left %+v", ps)
	}
	if s.value(k2) != "" || s.value(k1) != "t1" {
		t.Fatalf("PO05: unused %q kept %q", s.value(k2), s.value(k1))
	}
	s.deleteTask(t, tid)
	if s.value(k1) != "" || len(s.retired()) != 0 {
		t.Fatalf("after deleting the task: value %q, retired %v", s.value(k1), s.retired())
	}
}

// m4.md PO19：移除智能體時它的 Profile 與密鑰一起刪。
func TestRemovedAgentDropsProfiles(t *testing.T) {
	keys := &recordingKeychain{MemKeychain: provider.NewMemKeychain()}
	s := newSecretsApp(t, keys, t.TempDir())
	s.put(t, "mine", "v1", true)
	id, err := s.a.Agents().AddProfile(ctxb2(), agent.ProfileInput{Agent: "custom:mine", Name: "P",
		Env: []agent.EnvInput{{Name: "TOKEN", Value: "p1", Secret: true}}})
	if err != nil {
		t.Fatal(err)
	}
	key := profileSecret(s.profiles(t, "custom:mine")[1], "TOKEN")
	if err := s.a.Agents().Remove(ctxb2(), "custom:mine"); err != nil {
		t.Fatal(err)
	}
	s.a.agents.mu.Lock()
	left := slices.ContainsFunc(s.a.agents.cfg.Profiles, func(p agent.Profile) bool { return p.ID == id })
	s.a.agents.mu.Unlock()
	if left || s.value(key) != "" || len(s.retired()) != 0 {
		t.Fatalf("PO19: profile left %v, secret %q, retired %v", left, s.value(key), s.retired())
	}
}

// m4.md AC35、PO13：沿用的密鑰按原來那一行找，改了名的行帶著自己原來的值（兩行互換名稱也不串）。
func TestRenamedSecretsKeepTheirValues(t *testing.T) {
	keys := &recordingKeychain{MemKeychain: provider.NewMemKeychain()}
	s := newSecretsApp(t, keys, t.TempDir())
	g := s.a.Agents()
	in := agent.ProfileInput{Agent: "grok", Name: "P",
		Env: []agent.EnvInput{{Name: "A", Value: "valA", Secret: true}, {Name: "B", Value: "valB", Secret: true}}}
	id, err := g.AddProfile(ctxb2(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.ID, in.Env = id, []agent.EnvInput{{Name: "B", Secret: true, Kept: "A"}, {Name: "A", Secret: true, Kept: "B"}}
	if err := g.EditProfile(ctxb2(), in); err != nil {
		t.Fatal(err)
	}
	p := s.profiles(t, "grok")[1]
	if got := [2]string{s.value(profileSecret(p, "A")), s.value(profileSecret(p, "B"))}; got != [2]string{"valB", "valA"} {
		t.Fatalf("after swapping the names A, B hold %q", got)
	}
	if len(s.retired()) != 0 {
		t.Fatalf("kept secrets were retired: %v", s.retired())
	}
}
