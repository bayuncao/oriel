package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureIndex(t *testing.T) RegistryIndex {
	t.Helper()
	idx, err := ParseRegistry(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func entry(t *testing.T, id string) RegistryEntry {
	t.Helper()
	for _, e := range fixtureIndex(t).Agents {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no %s in the fixture", id)
	return RegistryEntry{}
}

// registryServer 以 ETag 提供測試資料，對它回應 304，
// 並在 down 被設定時失敗。
type registryServer struct {
	*httptest.Server
	hits, notModified atomic.Int32
	down              atomic.Bool
}

func newRegistryServer(t *testing.T, body []byte) *registryServer {
	s := &registryServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if s.down.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			s.notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestRegistryFetchCaches(t *testing.T) {
	srv := newRegistryServer(t, fixture(t))
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	c := &RegistryClient{HTTP: srv.Client(), URL: srv.URL, Dir: t.TempDir(), Now: func() time.Time { return now }}
	ctx := context.Background()

	idx, err := c.Fetch(ctx, false)
	if err != nil || len(idx.Agents) != 6 || idx.Stale.Code != "" || !idx.FetchedAt.Equal(now) {
		t.Fatalf("first fetch: %d agents, stale %v, %v", len(idx.Agents), idx.Stale, err)
	}
	// 新鮮：直接由快取提供，不發問。
	now = now.Add(23 * time.Hour)
	if idx, err = c.Fetch(ctx, false); err != nil || srv.hits.Load() != 1 || idx.Stale.Code != "" {
		t.Fatalf("fresh fetch: hits %d, stale %v, %v", srv.hits.Load(), idx.Stale, err)
	}
	// 到期：以 ETag 重新驗證。
	now = now.Add(2 * time.Hour)
	if idx, err = c.Fetch(ctx, false); err != nil || srv.notModified.Load() != 1 || len(idx.Agents) != 6 ||
		!idx.FetchedAt.Equal(now) || idx.Stale.Code != "" {
		t.Fatalf("revalidation: 304s %d, stale %v, %v", srv.notModified.Load(), idx.Stale, err)
	}
	// 強制：即使新鮮也會發問。
	if idx, err = c.Fetch(ctx, true); err != nil || srv.hits.Load() != 3 || idx.Stale.Code != "" {
		t.Fatalf("forced: hits %d, stale %v, %v", srv.hits.Load(), idx.Stale, err)
	}
}

// RA1：伺服器回錯誤時回傳快取，Stale 是這次確認失敗的原因。
func TestRegistryStaleOnBadStatus(t *testing.T) {
	srv := newRegistryServer(t, fixture(t))
	c := &RegistryClient{HTTP: srv.Client(), URL: srv.URL, Dir: t.TempDir()}
	ctx := context.Background()
	if _, err := c.Fetch(ctx, false); err != nil {
		t.Fatal(err)
	}
	srv.down.Store(true)
	idx, err := c.Fetch(ctx, true)
	if err != nil || len(idx.Agents) != 6 || idx.Stale.Code != CodeRegistryStatus ||
		!strings.Contains(idx.Stale.Params["status"], "502") {
		t.Fatalf("stale %v, %v", idx.Stale, err)
	}
}

// RA2：伺服器連不上時回傳快取，Stale 帶 unreachable 的原因。
func TestRegistryStaleOnUnreachable(t *testing.T) {
	srv := newRegistryServer(t, fixture(t))
	c := &RegistryClient{HTTP: srv.Client(), URL: srv.URL, Dir: t.TempDir()}
	ctx := context.Background()
	if _, err := c.Fetch(ctx, false); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	idx, err := c.Fetch(ctx, true)
	if err != nil || len(idx.Agents) != 6 || idx.Stale.Code != CodeRegistryUnreachable ||
		idx.Stale.Params["detail"] == "" {
		t.Fatalf("stale %v, %v", idx.Stale, err)
	}
}

func TestRegistryCacheLeavesNoTempFiles(t *testing.T) {
	srv := newRegistryServer(t, fixture(t))
	dir := t.TempDir()
	c := &RegistryClient{HTTP: srv.Client(), URL: srv.URL, Dir: dir}
	if _, err := c.Fetch(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*"))
	for i, n := range names {
		names[i] = filepath.Base(n)
	}
	if !slices.Equal(names, []string{"registry.json", "registry.meta.json"}) {
		t.Fatalf("cache dir holds %v", names)
	}
}

func TestRegistryCacheWriteFailureKeepsIndex(t *testing.T) {
	srv := newRegistryServer(t, fixture(t))
	dir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(dir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := &RegistryClient{HTTP: srv.Client(), URL: srv.URL, Dir: dir}
	idx, err := c.Fetch(context.Background(), false)
	if !errors.Is(err, ErrCacheWrite) || len(idx.Agents) != 6 {
		t.Fatalf("%d agents, %v", len(idx.Agents), err)
	}
}

func TestRegistryNeedsHTTPClient(t *testing.T) {
	c := &RegistryClient{URL: "http://127.0.0.1:0", Dir: t.TempDir()}
	if _, err := c.Fetch(context.Background(), false); err == nil {
		t.Fatal("want an error without an HTTP client")
	}
}

func TestRegistryFetchWithoutCacheFails(t *testing.T) {
	srv := newRegistryServer(t, fixture(t))
	srv.down.Store(true)
	c := &RegistryClient{HTTP: srv.Client(), URL: srv.URL, Dir: t.TempDir()}
	if _, err := c.Fetch(context.Background(), false); err == nil {
		t.Fatal("want an error with no cache")
	}
}

func TestRegistryEntryDefinition(t *testing.T) {
	for _, tc := range []struct {
		id      string
		command string
		args    []string
		env     []task.EnvVar
	}{
		{"grok-build", "npx", []string{"-y", "@xai-official/grok@1.0.68", "agent", "stdio"}, nil},
		{"pi-acp", "npx", []string{"-y", "pi-acp@0.0.34"}, nil},
		{"fast-agent", "uvx", []string{"fast-agent-acp==0.10.1", "-x"}, []task.EnvVar{{Name: "FAST_AGENT_MODEL", Value: "codexplan"}}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			a, err := AddRegistry(entry(t, tc.id), time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			d, err := a.Definition()
			if err != nil {
				t.Fatal(err)
			}
			if d.ID != task.AgentID("registry:"+tc.id) || d.Source != SourceRegistry || d.Protocol != task.ProtocolACP {
				t.Errorf("identity: %+v", d)
			}
			if d.Detect.Command != tc.command || d.Version != nil || len(d.Detect.NPM) != 0 {
				t.Errorf("detect: %+v, version %v", d.Detect, d.Version)
			}
			if !slices.Equal(d.Launch.Args, tc.args) || !slices.Equal(d.Launch.Env, tc.env) {
				t.Errorf("launch: %q %v", d.Launch.Args, d.Launch.Env)
			}
		})
	}
}

func TestRegistryBinaryEntry(t *testing.T) {
	e := entry(t, "cursor")
	p, _ := platform()
	_, ok := e.Distribution.Binary[p]
	d, err := entryDefinition(e)
	if !ok {
		if !errors.Is(err, ErrNotRunnable) {
			t.Fatalf("no binary for %s/%s: %v", runtime.GOOS, runtime.GOARCH, err)
		}
		return
	}
	if err != nil || d.Detect.Command != "cursor-agent" || !slices.Equal(d.Launch.Args, []string{"acp"}) {
		t.Fatalf("got %+v, %v", d, err)
	}
}

func TestAddRegistryKeepsRawEntry(t *testing.T) {
	idx, err := ParseRegistry([]byte(`{"version":"1.0.0","agents":[
		{"id":"x","name":"X","version":"1.0.0","future":{"a":1},"distribution":{"npx":{"package":"x@1.0.0"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := AddRegistry(idx.Agents[0], time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a.Entry), `"future":{"a":1}`) {
		t.Fatalf("saved %s", a.Entry)
	}
	if d, err := a.Definition(); err != nil || d.Launch.Args[1] != "x@1.0.0" {
		t.Fatalf("definition %+v, %v", d, err)
	}
}

func TestRegistryAddedIsPinned(t *testing.T) {
	old := entry(t, "gemini")
	a, err := AddRegistry(old, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// Registry 繼續前進；已加入的 agent 在使用者確認升級之前
	// 保留其版本。
	idx := fixtureIndex(t)
	for i := range idx.Agents {
		if idx.Agents[i].ID == "gemini" {
			idx.Agents[i].Version = "9.9.9"
			idx.Agents[i].Distribution.NPX.Package = "@google/gemini-cli@9.9.9"
		}
	}
	d, _ := a.Definition()
	if d.Launch.Args[1] != old.Distribution.NPX.Package {
		t.Fatalf("launch %q, want the pinned package", d.Launch.Args)
	}
	builtins := []Definition{{ID: "claude", Supersedes: []string{"claude-acp"}}, {ID: "grok", Supersedes: []string{"grok-build"}}}
	byID := map[string]RegistryListing{}
	for _, l := range ListRegistry(idx, builtins, []RegistryAgent{a}, nil) {
		byID[l.Entry.ID] = l
	}
	if l := byID["gemini"]; l.Added != old.Version || !l.Update || !l.Runnable {
		t.Errorf("gemini: %+v", l)
	}
	if byID["claude-acp"].Native != "claude" || byID["grok-build"].Native != "grok" || byID["pi-acp"].Native != "" {
		t.Errorf("native: %+v", byID)
	}
}

func TestCatalog(t *testing.T) {
	added, err := AddRegistry(entry(t, "gemini"), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	broken := RegistryAgent{ID: "bad", Entry: []byte("{")}
	defs, problems := Catalog(
		[]Definition{{ID: "claude"}, {ID: "claude"}},
		[]RegistryAgent{added, broken},
		[]CustomAgent{{Slug: "mine", Command: "my-agent"}, {Slug: "Bad Slug", Command: "x"}, {Slug: "rel", Command: "bin/x"}},
	)
	var ids []task.AgentID
	for _, d := range defs {
		ids = append(ids, d.ID)
	}
	if !slices.Equal(ids, []task.AgentID{"claude", "registry:gemini", "custom:mine"}) {
		t.Fatalf("ids %v", ids)
	}
	if defs[0].Source != SourceBuiltin || defs[2].Source != SourceCustom || defs[2].Name != "mine" || defs[2].Protocol != task.ProtocolACP {
		t.Errorf("defs %+v", defs)
	}
	type bad struct {
		agent task.AgentID
		code  ProblemCode
	}
	var got []bad
	for _, p := range problems {
		got = append(got, bad{p.Agent, p.Problem.Code})
	}
	want := []bad{
		{"claude", CodeDuplicateAgent},
		{"registry:bad", CodeRegistryEntryInvalid},
		{"custom:Bad Slug", CodeInvalidSlug},
		{"custom:rel", CodeRelativeCommand},
	}
	if !slices.Equal(got, want) {
		t.Errorf("problems %+v", problems)
	}
}

func TestPackageSpec(t *testing.T) {
	for _, tc := range []struct {
		r                   *packageRunner
		spec, name, version string
		pinned              string
	}{
		{&npx, "@google/gemini-cli@0.65.0", "@google/gemini-cli", "0.65.0", "@google/gemini-cli@0.65.0"},
		{&npx, "@google/gemini-cli", "@google/gemini-cli", "", "@google/gemini-cli@9.9.9"},
		{&npx, "pi-acp@0.0.34", "pi-acp", "0.0.34", "pi-acp@0.0.34"},
		{&npx, "cline", "cline", "", "cline@9.9.9"},
		{&uvx, "fast-agent-acp==0.10.1", "fast-agent-acp", "0.10.1", "fast-agent-acp==0.10.1"},
		{&uvx, "minion-code", "minion-code", "", "minion-code==9.9.9"},
	} {
		if n, v := tc.r.split(tc.spec); n != tc.name || v != tc.version {
			t.Errorf("split(%q) = %q, %q", tc.spec, n, v)
		}
		if got := tc.r.pin(tc.spec, "9.9.9"); got != tc.pinned {
			t.Errorf("pin(%q) = %q, want %q", tc.spec, got, tc.pinned)
		}
	}
	if got := npx.pin("pkg", ""); got != "pkg" {
		t.Errorf("pin without an entry version = %q", got)
	}
}
