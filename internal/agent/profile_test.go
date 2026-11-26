package agent

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// 有寬鬆模式參數的與沒有的 agent（PO06、PO07）。
var (
	permissive = Definition{ID: "grok", Name: "Grok", Launch: LaunchSpec{
		Args: []string{"agent", "stdio"}, Native: []string{"agent", "--always-approve", "stdio"},
		Env: []task.EnvVar{{Name: "MODE", Value: "agent"}, {Name: "KEEP", Value: "1"}}}}
	strict = Definition{ID: "codex", Name: "Codex", Launch: LaunchSpec{Args: []string{"app-server"}}}
)

// m4.md PO01、PO18：沒存過也有預設 Profile；空的 ID 是它。
func TestConfigDefaultProfile(t *testing.T) {
	got := Config{}.ProfilesOf("grok")
	want := Profile{ID: DefaultProfileID, Name: "Default", Agent: "grok", Default: true, Approval: task.ApprovalOrielHub}
	if len(got) != 1 || !profileEqual(got[0], want) {
		t.Fatalf("profiles %+v, want only %+v", got, want)
	}
	p, err := Config{}.Profile("grok", "")
	if err != nil || p.ID != DefaultProfileID {
		t.Fatalf("Profile(\"\") = %+v, %v", p, err)
	}
}

// m4.md PO03–PO11、PO14、PO17：新建、改寫與檢查。
func TestConfigPutProfile(t *testing.T) {
	five := 5 * time.Minute
	fast := Profile{ID: "p_fast", Name: "Fast", Approval: task.ApprovalNative, ExtraArgs: []string{"--x", "a b", ""}}
	c, err := Config{}.PutProfile(permissive, fast, true)
	if err != nil {
		t.Fatal(err)
	}
	got := c.ProfilesOf("grok")
	if len(got) != 2 || got[0].ID != DefaultProfileID || got[1].ID != "p_fast" || got[1].Agent != "grok" ||
		got[1].Default || !slices.Equal(got[1].ExtraArgs, []string{"--x", "a b", ""}) {
		t.Fatalf("after create %+v", got)
	}
	if other := c.ProfilesOf("codex"); len(other) != 1 {
		t.Fatalf("another agent sees %+v", other)
	}

	// 改預設的：名稱照舊（PO32），超時與決定存下（PO08）。
	c, err = c.PutProfile(permissive, Profile{ID: DefaultProfileID, Name: "Renamed", AskTimeout: five, TimeoutVerdict: task.Deny}, false)
	if err != nil {
		t.Fatal(err)
	}
	def := c.ProfilesOf("grok")[0]
	if def.Name != "Default" || !def.Default || def.AskTimeout != five || def.TimeoutVerdict != task.Deny ||
		def.Approval != task.ApprovalOrielHub {
		t.Fatalf("edited default %+v", def)
	}
	// 超時為 0 時清掉決定。
	c2, err := c.PutProfile(permissive, Profile{ID: DefaultProfileID, TimeoutVerdict: task.Allow}, false)
	if err != nil || c2.ProfilesOf("grok")[0].TimeoutVerdict != "" {
		t.Fatalf("zero timeout kept the verdict: %+v, %v", c2.ProfilesOf("grok")[0], err)
	}

	for _, tc := range []struct {
		name   string
		def    Definition
		p      Profile
		create bool
		want   ProblemCode
	}{
		{"PO03 empty name", permissive, Profile{ID: "p_b", Name: "  "}, true, CodeProfileNameEmpty},
		{"PO03 name taken", permissive, Profile{ID: "p_b", Name: " fast "}, true, CodeProfileNameTaken},
		{"PO07 no permissive mode", strict, Profile{ID: "p_b", Name: "B", Approval: task.ApprovalNative}, true, CodeNativeUnsupported},
		{"PO09 timeout without a verdict", permissive, Profile{ID: "p_b", Name: "B", AskTimeout: five}, true, CodeTimeoutVerdictMissing},
		{"PO10 negative timeout", permissive, Profile{ID: "p_b", Name: "B", AskTimeout: -time.Second, TimeoutVerdict: task.Deny}, true, CodeInvalidTimeout},
		{"PO10 over a day", permissive, Profile{ID: "p_b", Name: "B", AskTimeout: MaxAskTimeout + time.Second, TimeoutVerdict: task.Deny}, true, CodeInvalidTimeout},
		{"PO17 edit a missing profile", permissive, Profile{ID: "p_none", Name: "B"}, false, CodeUnknownProfile},
		{"PO17 edit another agent's profile", strict, Profile{ID: "p_fast", Name: "B"}, false, CodeUnknownProfile},
		{"create over an existing ID", permissive, Profile{ID: "p_fast", Name: "B"}, true, CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.PutProfile(tc.def, tc.p, tc.create)
			if code(err) != tc.want {
				t.Fatalf("err %v, want %s", err, tc.want)
			}
			if len(got.Profiles) != len(c.Profiles) {
				t.Fatalf("config changed on error: %+v", got.Profiles)
			}
		})
	}
	// 一天整是上限，接受（PO10）；改自己的名稱不算重複。
	if _, err := c.PutProfile(permissive, Profile{ID: "p_fast", Name: "FAST", AskTimeout: MaxAskTimeout, TimeoutVerdict: task.Allow}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Profile("codex", "p_fast"); code(err) != CodeUnknownProfile {
		t.Fatalf("another agent's profile: %v", err)
	}
}

// m4.md PO04、PO05、PO19：刪 Profile、移除 agent；它們的密鑰記成待清理。
func TestConfigRemoveProfile(t *testing.T) {
	c := Config{Custom: []CustomAgent{{Slug: "mine", Command: "mine-acp"}}}
	mine, _ := c.Custom[0].Definition()
	var err error
	for _, p := range []Profile{
		{ID: "p_a", Name: "A", Env: []task.EnvVar{{Name: "T", Secret: "profile:custom:mine:p_a:T:1"}}},
		{ID: DefaultProfileID, Env: []task.EnvVar{{Name: "T", Secret: "profile:custom:mine:default:T:1"}}},
	} {
		if c, err = c.PutProfile(mine, p, p.ID != DefaultProfileID); err != nil {
			t.Fatal(err)
		}
	}
	if c, err = c.PutProfile(permissive, Profile{ID: "p_g", Name: "G", Env: []task.EnvVar{{Name: "T", Secret: "grok-key"}}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RemoveProfile("custom:mine", DefaultProfileID); code(err) != CodeDefaultProfile {
		t.Fatalf("PO04: %v", err)
	}
	if _, err := c.RemoveProfile("grok", "p_a"); code(err) != CodeUnknownProfile {
		t.Fatalf("another agent's profile: %v", err)
	}
	removed, err := c.RemoveProfile("custom:mine", "p_a")
	if err != nil {
		t.Fatal(err)
	}
	if ids := profileIDs(removed.ProfilesOf("custom:mine")); !slices.Equal(ids, []task.ProfileID{DefaultProfileID}) {
		t.Fatalf("PO05 left %v", ids)
	}
	if !slices.Equal(removed.Retired, []RetiredSecret{{Agent: "custom:mine", Key: "profile:custom:mine:p_a:T:1"}}) {
		t.Fatalf("PO05 retired %+v", removed.Retired)
	}
	if len(c.ProfilesOf("custom:mine")) != 2 {
		t.Fatal("RemoveProfile changed the original config")
	}

	gone, err := c.Remove(nil, "custom:mine")
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(gone.Profiles, func(p Profile) bool { return p.Agent == "custom:mine" }) ||
		len(gone.ProfilesOf("grok")) != 2 {
		t.Fatalf("PO19 profiles %+v", gone.Profiles)
	}
	keys := retiredKeys(gone)
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"profile:custom:mine:default:T:1", "profile:custom:mine:p_a:T:1"}) {
		t.Fatalf("PO19 retired %v", keys)
	}

	// 改寫時不再用的密鑰也記成待清理，留著的不記。
	edited, err := c.PutProfile(permissive, Profile{ID: "p_g", Name: "G"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(retiredKeys(edited), []string{"grok-key"}) {
		t.Fatalf("edit retired %v", edited.Retired)
	}
}

// m4.md PO11、PO13：環境變數的檢查。
func TestCheckEnv(t *testing.T) {
	old := []task.EnvVar{{Name: "TOKEN", Secret: "k"}, {Name: "PLAIN", Value: "v"}}
	for _, tc := range []struct {
		name string
		in   []EnvInput
		want ProblemCode
	}{
		{"ok", []EnvInput{{Name: " A ", Value: "1"}, {Name: "TOKEN", Secret: true, Kept: "TOKEN"}}, ""},
		{"renamed keeps its own secret", []EnvInput{{Name: "RENAMED", Secret: true, Kept: "TOKEN"}}, ""},
		{"empty name", []EnvInput{{Name: " "}}, CodeInvalidEnv},
		{"name with =", []EnvInput{{Name: "A=B"}}, CodeInvalidEnv},
		{"repeated", []EnvInput{{Name: "A"}, {Name: " A"}}, CodeInvalidEnv},
		{"secret without a value", []EnvInput{{Name: "NEW", Secret: true}}, CodeSecretEmpty},
		{"keep without an old secret", []EnvInput{{Name: "PLAIN", Secret: true, Kept: "PLAIN"}}, CodeSecretEmpty},
		{"keep a name that was not there", []EnvInput{{Name: "TOKEN", Secret: true, Kept: "GONE"}}, CodeSecretEmpty},
		{"empty value without keep", []EnvInput{{Name: "TOKEN", Secret: true}}, CodeSecretEmpty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := code(CheckEnv(tc.in, old)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// m4.md PO06、PO12、PO14、PO16：解析。
func TestLaunchWithProfile(t *testing.T) {
	inst := Installation{Path: "/bin/grok"}
	p := Profile{ID: "p_fast", Name: "Fast", Approval: task.ApprovalNative, ExtraArgs: []string{"--model", "a b"},
		Env:        []task.EnvVar{{Name: "MODE", Value: "profile"}, {Name: "TOKEN", Secret: "k"}},
		AskTimeout: time.Minute, TimeoutVerdict: task.Allow}
	s := Launch(permissive, inst, p, task.Capabilities{})
	if !slices.Equal(s.Args, []string{"agent", "--always-approve", "stdio", "--model", "a b"}) {
		t.Fatalf("PO06/PO14 args %q", s.Args)
	}
	if s.Approval != task.ApprovalNative || s.Profile != "p_fast" || s.ProfileName != "Fast" ||
		s.AskTimeout != time.Minute || s.TimeoutVerdict != task.Allow {
		t.Fatalf("PO16 snapshot %+v", s)
	}
	// 智能體的在前，Profile 的在後：同名時進程取後一個（PO12）。
	want := []task.EnvVar{{Name: "MODE", Value: "agent"}, {Name: "KEEP", Value: "1"}, {Name: "MODE", Value: "profile"}, {Name: "TOKEN", Secret: "k"}}
	if !slices.Equal(s.Env, want) {
		t.Fatalf("PO12 env %+v", s.Env)
	}
	if d := Launch(permissive, inst, DefaultProfile(permissive), task.Capabilities{}); !slices.Equal(d.Args, []string{"agent", "stdio"}) {
		t.Fatalf("default args %q", d.Args)
	}
	if a, b := NewProfileID(), NewProfileID(); !strings.HasPrefix(string(a), "p_") || a == b {
		t.Fatal("NewProfileID")
	}
}

func profileEqual(a, b Profile) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Agent == b.Agent && a.Default == b.Default && a.Approval == b.Approval &&
		slices.Equal(a.ExtraArgs, b.ExtraArgs) && slices.Equal(a.Env, b.Env) && a.AskTimeout == b.AskTimeout &&
		a.TimeoutVerdict == b.TimeoutVerdict
}

func profileIDs(ps []Profile) []task.ProfileID {
	var out []task.ProfileID
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func retiredKeys(c Config) []string {
	var out []string
	for _, r := range c.Retired {
		out = append(out, r.Key)
	}
	return out
}
