package agent

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// testBuiltins 是測試用的內建：claude 原生支援 Registry 的 claude-acp（AC16）。
var testBuiltins = []Definition{{ID: "claude", Supersedes: []string{"claude-acp"}}, {ID: "grok"}}

func code(err error) ProblemCode {
	if p, ok := errors.AsType[*Problem](err); ok {
		return p.Code
	}
	return ""
}

// m4.md AC10、AC14–AC16：加入 Registry 條目。
func TestConfigAddRegistry(t *testing.T) {
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	added, err := Config{}.AddRegistry(testBuiltins, entry(t, "pi-acp"), at)
	if err != nil {
		t.Fatal(err)
	}
	if len(added.Registry) != 1 || added.Registry[0].ID != "pi-acp" || added.Registry[0].Version != "0.0.34" ||
		!added.Registry[0].AddedAt.Equal(at) || len(added.Registry[0].Entry) == 0 {
		t.Fatalf("added %+v", added.Registry)
	}
	for _, tc := range []struct {
		name string
		cfg  Config
		e    RegistryEntry
		want ProblemCode
	}{
		{"AC15 already added", added, entry(t, "pi-acp"), CodeAlreadyAdded},
		{"AC16 superseded by a builtin", Config{}, entry(t, "claude-acp"), CodeDuplicateAgent},
		{"AC14 no distribution", Config{}, RegistryEntry{ID: "nothing", Name: "Nothing", Version: "1.0.0"}, CodeNotRunnable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.AddRegistry(testBuiltins, tc.e, at)
			if code(err) != tc.want {
				t.Fatalf("err %v, want %s", err, tc.want)
			}
			if len(got.Registry) != len(tc.cfg.Registry) {
				t.Fatalf("config changed on error: %+v", got.Registry)
			}
		})
	}
}

// m4.md AC21：更新保留 ID、加入時間與位置，換成新的條目。
func TestConfigUpdateRegistry(t *testing.T) {
	first := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	c, err := Config{}.AddRegistry(testBuiltins, entry(t, "pi-acp"), first)
	if err != nil {
		t.Fatal(err)
	}
	if c, err = c.AddRegistry(testBuiltins, entry(t, "fast-agent"), first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	newer := entry(t, "pi-acp")
	newer.Version, newer.raw = "0.0.35", nil
	newer.Distribution.NPX = &PackageTarget{Package: "pi-acp@0.0.35"}
	next, err := c.UpdateRegistry(newer)
	if err != nil {
		t.Fatal(err)
	}
	if c.Registry[0].Version != "0.0.34" {
		t.Fatalf("UpdateRegistry changed its receiver: %+v", c.Registry)
	}
	got := next.Registry[0]
	if got.ID != "pi-acp" || got.Version != "0.0.35" || !got.AddedAt.Equal(first) || next.Registry[1].ID != "fast-agent" {
		t.Fatalf("updated %+v", next.Registry)
	}
	d, err := got.Definition()
	if err != nil || !slices.Contains(d.Launch.Args, "pi-acp@0.0.35") {
		t.Fatalf("definition %+v, %v", d.Launch, err)
	}
	if _, err := (Config{}).UpdateRegistry(newer); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("update of an entry that is not added: %v", err)
	}
}

// m4.md AC22、AC25、AC03：移除。
func TestConfigRemove(t *testing.T) {
	c, err := Config{}.AddRegistry(testBuiltins, entry(t, "pi-acp"), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	secret := task.EnvVar{Name: "TOKEN", Secret: SecretKey("mine", "TOKEN", "n1")}
	if c, err = c.PutCustom(CustomAgent{Slug: "mine", Command: "mine-acp", Env: []task.EnvVar{secret}}, true); err != nil {
		t.Fatal(err)
	}
	// AC03：解析不了的條目也移除得了。
	c.Registry = append(c.Registry, RegistryAgent{ID: "broken", Entry: []byte("{")})
	for _, id := range []task.AgentID{"registry:pi-acp", "custom:mine", "grok"} {
		if c, err = c.SetOverride(id, "/opt/"+string(id)); err != nil {
			t.Fatal(err)
		}
	}

	next, err := c.Remove(testBuiltins, "custom:mine")
	if err != nil || len(next.Custom) != 0 {
		t.Fatalf("remove custom: %+v %v", next.Custom, err)
	}
	// AC35、AC37：移除的自訂 agent 的密鑰記成待清理，不在這裡刪。
	if !slices.Equal(next.Retired, []RetiredSecret{{Agent: "custom:mine", Key: secret.Secret}}) {
		t.Fatalf("retired %+v", next.Retired)
	}
	if next.Override("custom:mine") != "" || next.Override("grok") == "" {
		t.Fatalf("overrides after removing custom:mine: %+v", next.Overrides)
	}
	next, err = next.Remove(testBuiltins, "registry:pi-acp")
	if err != nil || len(next.Retired) != 1 || len(next.Registry) != 1 || next.Override("registry:pi-acp") != "" {
		t.Fatalf("remove registry: %+v %v", next, err)
	}
	if next, err = next.Remove(testBuiltins, "registry:broken"); err != nil || len(next.Registry) != 0 {
		t.Fatalf("remove an unparsable entry: %+v %v", next.Registry, err)
	}
	if len(c.Custom) != 1 || len(c.Registry) != 2 || len(c.Retired) != 0 {
		t.Fatalf("Remove changed its receiver: %+v", c)
	}
	for _, tc := range []struct {
		id   task.AgentID
		want ProblemCode
	}{
		{"grok", CodeBuiltin},
		{"registry:nope", CodeUnknownAgent},
		{"custom:nope", CodeUnknownAgent},
		{"nope", CodeUnknownAgent},
	} {
		if _, err := c.Remove(testBuiltins, tc.id); code(err) != tc.want {
			t.Errorf("remove %s: %v, want %s", tc.id, err, tc.want)
		}
	}
}

// m4.md AC30–AC34、AC36：自訂 agent。
func TestConfigPutCustom(t *testing.T) {
	c, err := Config{}.PutCustom(CustomAgent{Slug: "zed", Name: "  ", Command: "zed-acp"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if c, err = c.PutCustom(CustomAgent{Slug: "alpha", Name: "Alpha", Command: "/opt/alpha", Args: []string{"--acp"}}, true); err != nil {
		t.Fatal(err)
	}
	if c.Custom[0].Slug != "alpha" || c.Custom[1].Slug != "zed" || c.Custom[1].Name != "zed" {
		t.Fatalf("custom agents %+v: want sorted by slug, an empty name replaced by the slug (AC34)", c.Custom)
	}
	edited, err := c.PutCustom(CustomAgent{Slug: "alpha", Name: "Alpha 2", Command: "alpha-acp"}, false)
	if err != nil || edited.Custom[0].Name != "Alpha 2" || edited.Custom[0].Command != "alpha-acp" || len(edited.Custom) != 2 {
		t.Fatalf("edit: %+v %v", edited.Custom, err)
	}
	if c.Custom[0].Name != "Alpha" {
		t.Fatalf("PutCustom changed its receiver: %+v", c.Custom)
	}
	for _, tc := range []struct {
		name   string
		a      CustomAgent
		create bool
		want   ProblemCode
	}{
		{"AC31 bad slug", CustomAgent{Slug: "Bad Slug", Command: "x"}, true, CodeInvalidSlug},
		{"AC32 no command", CustomAgent{Slug: "x"}, true, CodeNoCommand},
		{"AC32 relative command", CustomAgent{Slug: "x", Command: "bin/x"}, true, CodeRelativeCommand},
		{"AC33 slug taken", CustomAgent{Slug: "zed", Command: "x"}, true, CodeDuplicateAgent},
		{"AC36 edit a missing agent", CustomAgent{Slug: "missing", Command: "x"}, false, CodeUnknownAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.PutCustom(tc.a, tc.create)
			if code(err) != tc.want {
				t.Fatalf("err %v, want %s", err, tc.want)
			}
			if len(got.Custom) != 2 {
				t.Fatalf("config changed on error: %+v", got.Custom)
			}
		})
	}
}

// m4.md AC40–AC42：覆蓋路徑。
func TestConfigSetOverride(t *testing.T) {
	c, err := Config{}.SetOverride("grok", " /opt/grok ")
	if err != nil || c.Override("grok") != "/opt/grok" {
		t.Fatalf("set: %+v %v", c.Overrides, err)
	}
	if c, err = c.SetOverride("claude", "/opt/claude"); err != nil || c.Overrides[0].Agent != "claude" {
		t.Fatalf("overrides %+v: want sorted by agent", c.Overrides)
	}
	if _, err := c.SetOverride("grok", "bin/grok"); code(err) != CodeRelativeOverride {
		t.Fatalf("relative path: %v", err)
	}
	cleared, err := c.SetOverride("grok", "")
	if err != nil || cleared.Override("grok") != "" || cleared.Override("claude") == "" {
		t.Fatalf("clear: %+v %v", cleared.Overrides, err)
	}
}

// m4.md AC20、AC36、AC40：Agent 頁的列接上設定；密鑰只有鍵。
func TestConfigAnnotate(t *testing.T) {
	c, err := Config{}.AddRegistry(testBuiltins, entry(t, "pi-acp"), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	env := []task.EnvVar{{Name: "TOKEN", Secret: SecretKey("mine", "TOKEN", "n1")}}
	if c, err = c.PutCustom(CustomAgent{Slug: "mine", Command: "mine-acp", Env: env}, true); err != nil {
		t.Fatal(err)
	}
	if c, err = c.SetOverride("grok", "/opt/grok"); err != nil {
		t.Fatal(err)
	}
	defs, problems := Catalog(testBuiltins, c.Registry, c.Custom)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	byID := map[task.AgentID]AgentRow{}
	for _, r := range c.Annotate(Rows(defs, nil)) {
		byID[r.ID] = r
	}
	if byID["grok"].Override != "/opt/grok" || byID["claude"].Override != "" {
		t.Errorf("overrides: %+v", byID)
	}
	if byID["registry:pi-acp"].Pinned != "0.0.34" || byID["grok"].Pinned != "" {
		t.Errorf("pinned: %+v", byID["registry:pi-acp"])
	}
	cu := byID["custom:mine"].Custom
	if cu == nil || cu.Command != "mine-acp" || cu.Env[0].Value != "" || cu.Env[0].Secret == "" || byID["grok"].Custom != nil {
		t.Errorf("custom: %+v", cu)
	}
	if cu != nil {
		cu.Env[0].Name = "CHANGED"
		if c.Custom[0].Env[0].Name != "TOKEN" {
			t.Error("a row's custom env shares its slice with the config")
		}
	}
}

// m4.md AC35、AC37：改寫自訂 agent 時，不再用的密鑰記成待清理，沿用的不記；Swept 移出刪掉的。
func TestConfigRetiresSecrets(t *testing.T) {
	keep := task.EnvVar{Name: "KEEP", Secret: SecretKey("mine", "KEEP", "n1")}
	swap := task.EnvVar{Name: "SWAP", Secret: SecretKey("mine", "SWAP", "n1")}
	drop := task.EnvVar{Name: "DROP", Secret: SecretKey("mine", "DROP", "n1")}
	c, err := Config{}.PutCustom(CustomAgent{Slug: "mine", Command: "mine-acp", Env: []task.EnvVar{keep, swap, drop}}, true)
	if err != nil || len(c.Retired) != 0 {
		t.Fatalf("create: %+v %v", c.Retired, err)
	}
	swapped := task.EnvVar{Name: "SWAP", Secret: SecretKey("mine", "SWAP", "n2")}
	plain := task.EnvVar{Name: "DROP", Value: "now plain"}
	next, err := c.PutCustom(CustomAgent{Slug: "mine", Command: "mine-acp", Env: []task.EnvVar{keep, swapped, plain}}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []RetiredSecret{{Agent: "custom:mine", Key: swap.Secret}, {Agent: "custom:mine", Key: drop.Secret}}
	if !slices.Equal(next.Retired, want) {
		t.Fatalf("retired %+v, want %+v", next.Retired, want)
	}
	if len(c.Retired) != 0 {
		t.Fatalf("PutCustom changed its receiver: %+v", c.Retired)
	}
	if _, err := next.PutCustom(CustomAgent{Slug: "missing", Command: "x"}, false); err == nil {
		t.Fatal("edit of a missing agent succeeded")
	}
	swept := next.Swept([]string{swap.Secret})
	if !slices.Equal(swept.Retired, want[1:]) || len(next.Retired) != 2 {
		t.Fatalf("swept %+v (receiver %+v)", swept.Retired, next.Retired)
	}
}
