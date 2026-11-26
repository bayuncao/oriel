package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/task"
)

func problemCode(err error) agent.ProblemCode {
	if p, ok := errors.AsType[*agent.Problem](err); ok {
		return p.Code
	}
	return ""
}

func row(a *App, id task.AgentID) (agent.AgentRow, bool) {
	for _, r := range a.Agents().Rows() {
		if r.ID == id {
			return r, true
		}
	}
	return agent.AgentRow{}, false
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// waitFor 等到 agent 的發現結果滿足 ok。
func waitFor(t *testing.T, a *App, id task.AgentID, ok func(agent.Installation) bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		for _, i := range a.Agents().Installations() {
			if i.Agent == id && ok(i) {
				return
			}
		}
		select {
		case <-a.Changes():
		case <-deadline:
			t.Fatalf("%s never reached the expected state: %+v", id, a.Agents().Installations())
		}
	}
}

// m4.md AC30、AC50、AC53、AC02、AC05：說 ACP 的假程式當作自訂 agent，加入後不用重啟就能建 Task 對話；
// 重開後還在，上一次的發現結果存下了。
func TestCustomAgentChats(t *testing.T) {
	home, bin := fakeHome(t)
	writeExe(t, bin, "grok", fakeGrok("1.0.70", filepath.Join(t.TempDir(), "grok.log"), false))
	dir := t.TempDir()
	writeExe(t, dir, "mine-acp", fakeGrok("0.1.0", filepath.Join(t.TempDir(), "mine.log"), false))
	env := proc.Env{"PATH=/usr/bin:/bin", "HOME=" + home, "SHELL=/bin/sh"}
	data := t.TempDir()
	a, err := New(ctxb2(), Options{DataDir: data, Inherited: env, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)

	in := agent.CustomAgentInput{Slug: "mine", Name: "Mine", Command: filepath.Join(dir, "mine-acp"), Args: []string{"agent", "stdio"}}
	if err := a.Agents().AddCustom(ctxb2(), in); err != nil {
		t.Fatal(err)
	}
	waitReady(t, a, "custom:mine")
	r, _ := row(a, "custom:mine")
	if r.Source != agent.SourceCustom || !r.Launchable || r.Custom == nil || r.Custom.Name != "Mine" {
		t.Fatalf("row %+v", r)
	}
	id := createWithAgent(t, a, "custom:mine")
	waitTurn(t, a, id)
	st, err := a.Hub().Snapshot(ctxb2(), hub.LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range st.Sessions {
		if x.Launch.Agent != "custom:mine" || x.Launch.AgentName != "Mine" || x.Launch.Executable != in.Command ||
			strings.Join(x.Launch.Args, " ") != "agent stdio" {
			t.Fatalf("launch %+v", x.Launch)
		}
	}
	// waitReady 從 Changes 收到的表已先存好（saveInstallations）。
	last, err := a.db.LastInstallations(ctxb2())
	if err != nil || !slices.ContainsFunc(last, func(i agent.Installation) bool { return i.Agent == "custom:mine" && i.Availability == agent.Ready }) {
		t.Fatalf("saved installations %+v, %v", last, err)
	}
	a.Close()

	b, err := New(ctxb2(), Options{DataDir: data, Inherited: env, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	if !slices.ContainsFunc(b.Agents().Definitions(), func(d agent.Definition) bool { return d.ID == "custom:mine" }) {
		t.Fatal("custom:mine is gone after reopening (AC02)")
	}
	waitReady(t, b, "custom:mine")
}

// mutableRegistry 是索引可換的 Registry 伺服器（AC20：Registry 出了新版本）。
func mutableRegistry(t *testing.T, body string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var cur atomic.Value
	cur.Store(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(cur.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	return srv, &cur
}

// m4.md AC10、AC15、AC17、AC20–AC22、AC24、AC25：加入、更新、移除 Registry 條目。
func TestRegistryAgentLifecycle(t *testing.T) {
	srv, body := mutableRegistry(t, registryBody)
	a, _ := registryApp(t, srv, nil)
	g := a.Agents()

	if err := g.AddRegistry(ctxb2(), "one", "0.9.0"); problemCode(err) != agent.CodeRegistryEntryChanged {
		t.Fatalf("adding a version the index does not have: %v (AC10)", err)
	}
	if err := g.AddRegistry(ctxb2(), "one", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	r, ok := row(a, "registry:one")
	if !ok || r.Pinned != "1.0.0" || r.Source != agent.SourceRegistry || !r.Launchable {
		t.Fatalf("row %+v (in catalog %v)", r, ok)
	}
	if err := g.AddRegistry(ctxb2(), "one", "1.0.0"); problemCode(err) != agent.CodeAlreadyAdded {
		t.Fatalf("adding twice: %v (AC15)", err)
	}
	if err := g.AddRegistry(ctxb2(), "nope", "1.0.0"); problemCode(err) != agent.CodeRegistryEntryMissing {
		t.Fatalf("adding an entry not in the index: %v (AC17)", err)
	}
	if err := a.Hub().SetDefaultModel(ctxb2(), hub.LocalOwner, "registry:one", "m1"); err != nil {
		t.Fatal(err)
	}

	body.Store(strings.Replace(registryBody, `"version":"1.0.0","distribution":{"npx":{"package":"one@1.0.0"}}`,
		`"version":"1.1.0","distribution":{"npx":{"package":"one@1.1.0"}}`, 1))
	list, err := g.Registry(ctxb2(), true)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list.Listings, func(l agent.RegistryListing) bool { return l.Entry.ID == "one" })
	if i < 0 || list.Listings[i].Added != "1.0.0" || !list.Listings[i].Update {
		t.Fatalf("listing %+v: want added 1.0.0 with an update (AC20)", list.Listings)
	}
	if got := strings.Join(list.Listings[i].Command, " "); got != "npx -y one@1.1.0" {
		t.Fatalf("command %q: the add and update confirmation shows the pinned package (AC10, AC21)", got)
	}
	if r, _ := row(a, "registry:one"); r.Pinned != "1.0.0" {
		t.Fatalf("pinned %s before the update", r.Pinned)
	}
	// 確認時看到的是舊版：索引已經換了，不按新的跑（AC10、AC21）。
	if err := g.UpdateRegistry(ctxb2(), "one", "1.0.0"); problemCode(err) != agent.CodeRegistryEntryChanged {
		t.Fatalf("updating to a version the index no longer has: %v", err)
	}
	if r, _ := row(a, "registry:one"); r.Pinned != "1.0.0" {
		t.Fatalf("pinned %s after a refused update", r.Pinned)
	}
	if err := g.UpdateRegistry(ctxb2(), "one", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if r, _ := row(a, "registry:one"); r.Pinned != "1.1.0" {
		t.Fatalf("pinned %s after the update (AC21)", r.Pinned)
	}

	if err := g.Remove(ctxb2(), "registry:one"); err != nil {
		t.Fatal(err)
	}
	if _, ok := row(a, "registry:one"); ok {
		t.Fatal("registry:one still in the catalog (AC22)")
	}
	models, err := a.Hub().Models(ctxb2(), hub.LocalOwner)
	if err != nil || slices.ContainsFunc(models, func(m hub.AgentModels) bool { return m.Agent == "registry:one" }) {
		t.Fatalf("models %+v, %v: the removed agent's default model is kept (AC22)", models, err)
	}
	_, _, err = a.Hub().CreateTaskWithAgent(ctxb2(), hub.LocalOwner, t.TempDir(), task.CreateTask{Title: "t", Brief: "b"}, "registry:one", "")
	if problemCode(err) != agent.CodeUnknownAgent {
		t.Fatalf("creating a task with a removed agent: %v (AC24)", err)
	}
	if err := g.Remove(ctxb2(), "grok"); problemCode(err) != agent.CodeBuiltin {
		t.Fatalf("removing a builtin: %v (AC25)", err)
	}
}

// m4.md AC35、AC36：密鑰的值存鑰匙串，設定裡只有鍵；改寫時沒給值沿用，給了新值換新鍵、刪舊鍵；移除時刪掉。
func TestCustomAgentSecrets(t *testing.T) {
	keys := provider.NewMemKeychain()
	a, err := New(ctxb2(), Options{DataDir: t.TempDir(), Inherited: proc.Env{"PATH=/usr/bin"}, Root: t.TempDir(), Keychain: keys})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	g := a.Agents()
	secretOf := func() task.EnvVar {
		t.Helper()
		r, ok := row(a, "custom:mine")
		if !ok || r.Custom == nil {
			t.Fatal("custom:mine not in the catalog")
		}
		for _, v := range r.Custom.Env {
			if v.Name == "TOKEN" {
				return v
			}
		}
		t.Fatalf("no TOKEN in %+v", r.Custom.Env)
		return task.EnvVar{}
	}
	value := func(key string) string {
		b, err := keys.Get(key)
		if err != nil {
			return ""
		}
		return string(b)
	}
	in := agent.CustomAgentInput{Slug: "mine", Command: "mine-acp", Env: []agent.EnvInput{
		{Name: "TOKEN", Value: "s3cret", Secret: true}, {Name: "MODE", Value: "fast"}}}
	if err := g.AddCustom(ctxb2(), in); err != nil {
		t.Fatal(err)
	}
	first := secretOf()
	if first.Value != "" || first.Secret == "" || value(first.Secret) != "s3cret" {
		t.Fatalf("secret %+v -> %q", first, value(first.Secret))
	}
	cfg, err := a.db.AgentConfig(ctxb2())
	if err != nil || strings.Contains(string(mustJSON(t, cfg)), "s3cret") {
		t.Fatalf("the secret's value is in the stored config: %v", err)
	}

	in.Env[0].Value = "" // 沒說沿用：刪掉這一行又加回同名的密鑰而沒填值，不會悄悄沿用原來的
	if err := g.EditCustom(ctxb2(), in); problemCode(err) != agent.CodeSecretEmpty {
		t.Fatalf("an empty secret without keep: %v", err)
	}
	in.Env[0].Kept = "TOKEN" // 沿用
	if err := g.EditCustom(ctxb2(), in); err != nil {
		t.Fatal(err)
	}
	in.Env[0].Kept = ""
	if got := secretOf(); got.Secret != first.Secret || value(first.Secret) != "s3cret" {
		t.Fatalf("an edit without a value changed the secret: %+v", got)
	}
	in.Env[0].Value = "n3w"
	if err := g.EditCustom(ctxb2(), in); err != nil {
		t.Fatal(err)
	}
	second := secretOf()
	if second.Secret == first.Secret || value(second.Secret) != "n3w" || value(first.Secret) != "" {
		t.Fatalf("new value: key %s -> %q, old key -> %q", second.Secret, value(second.Secret), value(first.Secret))
	}

	bad := in
	bad.Env = []agent.EnvInput{{Name: "TOKEN", Value: "x", Secret: true}, {Name: "A=B", Value: "y"}}
	if err := g.EditCustom(ctxb2(), bad); problemCode(err) != agent.CodeInvalidEnv {
		t.Fatalf("bad env name: %v", err)
	}
	if got := secretOf(); got.Secret != second.Secret || value(second.Secret) != "n3w" {
		t.Fatalf("a rejected edit changed the secret: %+v", got)
	}
	fresh := agent.CustomAgentInput{Slug: "other", Command: "x", Env: []agent.EnvInput{{Name: "T", Secret: true}}}
	if err := g.AddCustom(ctxb2(), fresh); problemCode(err) != agent.CodeSecretEmpty {
		t.Fatalf("a new secret without a value: %v", err)
	}

	// 輸入錯了不碰鑰匙串：鑰匙串壞了也回輸入的錯，而不是 agent.keychain_unavailable。
	keys.Err = errors.New("locked")
	for _, tc := range []struct {
		name   string
		in     agent.CustomAgentInput
		create bool
		want   agent.ProblemCode
	}{
		{"slug taken", agent.CustomAgentInput{Slug: "mine", Command: "x", Env: []agent.EnvInput{{Name: "T", Value: "v", Secret: true}}}, true, agent.CodeDuplicateAgent},
		{"bad slug", agent.CustomAgentInput{Slug: "Bad", Command: "x", Env: []agent.EnvInput{{Name: "T", Value: "v", Secret: true}}}, true, agent.CodeInvalidSlug},
		{"bad env after a secret", agent.CustomAgentInput{Slug: "mine", Command: "x", Env: []agent.EnvInput{{Name: "T", Value: "v", Secret: true}, {Name: "A=B"}}}, false, agent.CodeInvalidEnv},
		{"edit a missing agent keeping a secret", agent.CustomAgentInput{Slug: "ghost", Command: "x", Env: []agent.EnvInput{{Name: "TOKEN", Secret: true, Kept: "TOKEN"}}}, false, agent.CodeUnknownAgent},
	} {
		put := g.EditCustom
		if tc.create {
			put = g.AddCustom
		}
		if err := put(ctxb2(), tc.in); problemCode(err) != tc.want {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.want)
		}
	}
	keys.Err = nil

	if err := g.Remove(ctxb2(), "custom:mine"); err != nil {
		t.Fatal(err)
	}
	if value(second.Secret) != "" {
		t.Fatal("the removed agent's secret is still in the keychain")
	}
}

// m4.md AC40–AC43、AC07：覆蓋路徑只用那一個候選；存不進去時目錄不變。
func TestOverridePath(t *testing.T) {
	home, bin := fakeHome(t)
	writeExe(t, bin, "grok", fakeGrok("1.0.70", filepath.Join(t.TempDir(), "grok.log"), false))
	a := launchApp(t, home)
	g := a.Agents()
	missing := filepath.Join(t.TempDir(), "grok")
	if err := g.SetOverride(ctxb2(), "grok", missing); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "grok", func(i agent.Installation) bool {
		// 壞了的沒有選定的路徑，覆蓋路徑是唯一的候選（AC40：不回退到廠商目錄裡的 grok）。
		return i.Availability == agent.Broken && i.Problem.Code == agent.CodeNotExecutable &&
			len(i.Candidates) == 1 && i.Candidates[0].Source == agent.FromOverride
	})
	if r, _ := row(a, "grok"); r.Override != missing {
		t.Fatalf("override %q", r.Override)
	}
	if err := g.SetOverride(ctxb2(), "grok", "grok"); problemCode(err) != agent.CodeRelativeOverride {
		t.Fatalf("relative override: %v", err)
	}
	if err := g.SetOverride(ctxb2(), "nope", "/x"); problemCode(err) != agent.CodeUnknownAgent {
		t.Fatalf("override of an unknown agent: %v", err)
	}
	if err := g.SetOverride(ctxb2(), "grok", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a, "grok", func(i agent.Installation) bool {
		return i.Availability == agent.Ready && i.PathSource != agent.FromOverride
	})

	a.db.Close() // AC07
	if err := g.AddCustom(ctxb2(), agent.CustomAgentInput{Slug: "mine", Command: "mine-acp"}); err == nil {
		t.Fatal("AddCustom succeeded with the store closed")
	}
	if _, ok := row(a, "custom:mine"); ok {
		t.Fatal("the catalog changed although the config was not saved")
	}
}
