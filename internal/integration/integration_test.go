package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// fixture 是一台假的 Mac：bin 裡可能有假的 gh，Env 回傳以 bin 為 PATH 的快照
// （OrielHub 自己的 PATH 裡沒有它，IG13）。
type fixture struct {
	root  string
	bin   string
	store *MemStore
	r     *proc.Runner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{root: root, bin: filepath.Join(root, "bin"), store: &MemStore{}, r: &proc.Runner{}}
	if err := os.MkdirAll(f.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.r.Close)
	return f
}

func (f *fixture) env(context.Context) (proc.Env, error) {
	return proc.Env{"PATH=" + f.bin + ":/usr/bin:/bin", "HOME=" + f.root}, nil
}

// gh 寫一個假的 gh：先把參數一行一個附加到 args 檔、記一次呼叫，再跑 body。
func (f *fixture) gh(t *testing.T, body string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"" + filepath.Join(f.root, "args") + "\"\necho call >> \"" +
		filepath.Join(f.root, "calls") + "\"\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(f.bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// account 是假 gh 回報的 github.com 帳號（JSON 的形狀同 gh 2.105.0）。
func account(state string, active bool, login, scopes string) string {
	return `echo '{"hosts":{"github.com":[{"state":"` + state + `","active":` + strconv.FormatBool(active) +
		`,"host":"github.com","login":"` + login + `","tokenSource":"keyring","scopes":"` + scopes + `","gitProtocol":"https"}]}}'`
}

var ready = account("success", true, "may", "gist, read:org, repo, workflow")

func (f *fixture) service(t *testing.T, timeout time.Duration) *Service {
	t.Helper()
	s, err := New(context.Background(), Options{Runner: f.r, Env: f.env, Store: f.store, Root: f.root, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func (f *fixture) stored(t *testing.T) []IntegrationID {
	t.Helper()
	ids, err := f.store.EnabledIntegrations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func (f *fixture) calls(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, "calls"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "call\n")
}

func refresh(t *testing.T, s *Service) Integration {
	t.Helper()
	cat, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Items) != 1 {
		t.Fatalf("items %+v, want only github", cat.Items)
	}
	return cat.Items[0]
}

func problem(code Code, kv ...string) *task.Problem { return fail(code, nil, kv...).problem() }

func wantItem(t *testing.T, got Integration, status IntegrationStatus, enabled bool, login string, p *task.Problem) {
	t.Helper()
	if got.ID != GitHub || got.Name != "GitHub" || got.Group != GroupCode {
		t.Errorf("identity %+v", got)
	}
	if got.Status != status || got.Enabled != enabled || got.Account != login {
		t.Errorf("got %s enabled %v account %q, want %s %v %q", got.Status, got.Enabled, got.Account, status, enabled, login)
	}
	if (got.Problem == nil) != (p == nil) || got.Problem != nil && (got.Problem.Code != p.Code || !sameParams(got.Problem.Params, p.Params)) {
		t.Errorf("problem %+v, want %+v", got.Problem, p)
	}
}

func sameParams(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestDetect 是 IG01–IG04、IG11：gh 的各種回答變成什麼狀態。
func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gh      string // 空：沒有 gh
		status  IntegrationStatus
		login   string
		problem *task.Problem
	}{
		{name: "IG01 no gh", status: StatusUnavailable, problem: problem(CodeGhMissing)},
		{name: "IG02 no account", gh: `echo '{"hosts":{}}'`, status: StatusUnavailable, problem: problem(CodeGhLoggedOut)},
		{name: "IG02 not active", gh: account("success", false, "may", "repo"), status: StatusUnavailable, problem: problem(CodeGhLoggedOut)},
		{name: "IG16 bad token", gh: `echo '{"hosts":{"github.com":[{"state":"error","error":"The token in keyring is invalid.","active":true,"login":"may","scopes":""}]}}'`,
			status: StatusUnavailable, problem: problem(CodeGhUnreachable, "detail", "The token in keyring is invalid.")},
		{name: "IG16 error without text", gh: account("error", true, "may", ""), status: StatusUnavailable, problem: problem(CodeGhUnreachable, "detail", "error")},
		{name: "IG03 no repo scope", gh: account("success", true, "may", "gist, read:org, repository"),
			status: StatusUnavailable, problem: problem(CodeGhScopeMissing, "scope", "repo")},
		{name: "IG04 ready", gh: ready, status: StatusAvailable, login: "may"},
		{name: "IG16 timeout", gh: `echo '{"hosts":{"github.com":[{"state":"timeout","error":"","active":true,"login":"may","scopes":"repo"}]}}'`,
			status: StatusUnavailable, problem: problem(CodeGhUnreachable, "detail", "timeout")},
		{name: "IG16 network error", gh: `echo '{"hosts":{"github.com":[{"state":"error","error":"dial tcp: lookup api.github.com: no such host","active":true,"login":"may","scopes":"repo"}]}}'`,
			status: StatusUnavailable, problem: problem(CodeGhUnreachable, "detail", "dial tcp: lookup api.github.com: no such host")},
		{name: "IG17 fine-grained token", gh: account("success", true, "may", ""), status: StatusAvailable, login: "may"},
		{name: "IG11 not json", gh: `echo 'Logged in to github.com'`, status: StatusUnavailable,
			problem: problem(CodeGhFailed, "detail", "invalid character 'L' looking for beginning of value")},
		{name: "IG11 exit 1", gh: `echo first >&2; echo 'unknown flag: --json' >&2; exit 1`, status: StatusUnavailable,
			problem: problem(CodeGhFailed, "detail", "unknown flag: --json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.gh != "" {
				f.gh(t, tc.gh)
			}
			wantItem(t, refresh(t, f.service(t, 0)), tc.status, false, tc.login, tc.problem)
		})
	}
}

// TestBeforeFirstCheck：還沒檢測時是 unknown；New 不跑 gh（IG15）。
func TestBeforeFirstCheck(t *testing.T) {
	f := newFixture(t)
	f.gh(t, ready)
	s := f.service(t, 0)
	wantItem(t, s.List().Items[0], StatusUnknown, false, "", nil)
	if n := f.calls(t); n != 0 {
		t.Fatalf("gh ran %d times before Start or Refresh", n)
	}
}

// TestStart：Start 檢測一次，Watch 收到結果（IG15）。
func TestStart(t *testing.T) {
	f := newFixture(t)
	f.gh(t, ready)
	s := f.service(t, 0)
	got := make(chan IntegrationCatalog, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Watch(ctx, func(c IntegrationCatalog) error { got <- c; return nil }) }()
	<-got // 現在的內容
	s.Start()
	select {
	case c := <-got:
		wantItem(t, c.Items[0], StatusAvailable, false, "may", nil)
	case <-time.After(30 * time.Second):
		t.Fatal("no push after Start")
	}
}

// TestConnect 是 IG05：連接先檢測，通過才記下（只有標記，沒有 token）；重開後仍是已連接。
func TestConnect(t *testing.T) {
	f := newFixture(t)
	f.gh(t, ready)
	s := f.service(t, 0)
	cat, err := s.Connect(context.Background(), GitHub)
	if err != nil {
		t.Fatal(err)
	}
	wantItem(t, cat.Items[0], StatusConnected, true, "may", nil)
	if ids := f.stored(t); !slices.Equal(ids, []IntegrationID{GitHub}) {
		t.Fatalf("stored %v", ids)
	}
	if n := f.calls(t); n != 1 {
		t.Fatalf("gh ran %d times, want 1 (the check before connecting)", n)
	}
	s.Close()

	again := f.service(t, 0)
	wantItem(t, again.List().Items[0], StatusUnknown, true, "", nil)
	wantItem(t, refresh(t, again), StatusConnected, true, "may", nil)
}

// TestConnectUnavailable 是 IG06：接不上時回報與狀態相同的原因，不記下。
func TestConnectUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		gh   string
		want *task.Problem
	}{
		{"missing", "", problem(CodeGhMissing)},
		{"logged out", `echo '{"hosts":{}}'`, problem(CodeGhLoggedOut)},
		{"scope", account("success", true, "may", "gist"), problem(CodeGhScopeMissing, "scope", "repo")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.gh != "" {
				f.gh(t, tc.gh)
			}
			s := f.service(t, 0)
			_, err := s.Connect(context.Background(), GitHub)
			var e *Error
			if !errors.As(err, &e) || string(e.Code) != tc.want.Code || !sameParams(e.Params, tc.want.Params) {
				t.Fatalf("err %v, want %+v", err, tc.want)
			}
			if ids := f.stored(t); len(ids) != 0 {
				t.Fatalf("stored %v", ids)
			}
			wantItem(t, s.List().Items[0], StatusUnavailable, false, "", tc.want)
		})
	}
}

// TestLoggedOutAfterConnect 是 IG07：已連接後 gh 退出登入 → 不可用但保留連接；
// 重新登入後檢測回到已連接，不用再點連接。
func TestLoggedOutAfterConnect(t *testing.T) {
	f := newFixture(t)
	f.gh(t, ready)
	s := f.service(t, 0)
	if _, err := s.Connect(context.Background(), GitHub); err != nil {
		t.Fatal(err)
	}
	f.gh(t, `echo '{"hosts":{}}'`)
	wantItem(t, refresh(t, s), StatusUnavailable, true, "", problem(CodeGhLoggedOut))
	if ids := f.stored(t); !slices.Equal(ids, []IntegrationID{GitHub}) {
		t.Fatalf("stored %v", ids)
	}
	f.gh(t, ready)
	wantItem(t, refresh(t, s), StatusConnected, true, "may", nil)
}

// TestDisconnect 是 IG08：斷開清掉標記並存下；再斷開一次無操作、不寫。
func TestDisconnect(t *testing.T) {
	f := newFixture(t)
	f.gh(t, ready)
	s := f.service(t, 0)
	if _, err := s.Connect(context.Background(), GitHub); err != nil {
		t.Fatal(err)
	}
	cat, err := s.Disconnect(context.Background(), GitHub)
	if err != nil {
		t.Fatal(err)
	}
	wantItem(t, cat.Items[0], StatusAvailable, false, "may", nil)
	if ids := f.stored(t); len(ids) != 0 {
		t.Fatalf("stored %v", ids)
	}
	f.store.Err = errors.New("must not write")
	again, err := s.Disconnect(context.Background(), GitHub)
	if err != nil || again.Rev != cat.Rev {
		t.Fatalf("second disconnect: %v, rev %d → %d", err, cat.Rev, again.Rev)
	}
}

// TestStoreFailure：寫不進 store 時回報錯誤，狀態不變。
func TestStoreFailure(t *testing.T) {
	f := newFixture(t)
	f.gh(t, ready)
	s := f.service(t, 0)
	boom := errors.New("disk full")
	f.store.Err = boom
	if _, err := s.Connect(context.Background(), GitHub); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	wantItem(t, s.List().Items[0], StatusAvailable, false, "may", nil)
}

// TestUnknown 是 IG09：不認得的 ID 回 integration.unknown{id}，不跑 gh。
func TestUnknown(t *testing.T) {
	f := newFixture(t)
	s := f.service(t, 0)
	for name, call := range map[string]func(context.Context, IntegrationID) (IntegrationCatalog, error){"connect": s.Connect, "disconnect": s.Disconnect} {
		_, err := call(context.Background(), "gitlab")
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeUnknown || e.Params["id"] != "gitlab" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := f.calls(t); n != 0 {
		t.Fatalf("gh ran %d times", n)
	}
}

// TestHang 是 IG10：gh 卡住時在超時後結束，回報 gh_failed{detail}。超時之後收掉整個行程群組
// 是 proc.Runner 的事，由它的測試確定地驗收（TestRunnerTermBeforeKill、TestRunnerKillsAfterGrace）；
// 這裡不去找 gh 的 pid：負載高時 gh 可能來不及寫出它就被結束。
func TestHang(t *testing.T) {
	f := newFixture(t)
	f.gh(t, `sleep 60 & wait`)
	got := refresh(t, f.service(t, time.Second))
	if got.Status != StatusUnavailable || got.Problem == nil || got.Problem.Code != string(CodeGhFailed) || got.Problem.Params["detail"] == "" {
		t.Fatalf("got %+v problem %+v", got, got.Problem)
	}
}

// TestWatch 是 IG12：每次變化推一份，Rev 遞增；檢測結果相同時不推。
func TestWatch(t *testing.T) {
	f := newFixture(t)
	f.gh(t, ready)
	s := f.service(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan IntegrationCatalog, 16)
	done := make(chan error, 1)
	go func() { done <- s.Watch(ctx, func(c IntegrationCatalog) error { got <- c; return nil }) }()
	next := func(want IntegrationStatus) IntegrationCatalog {
		t.Helper()
		select {
		case c := <-got:
			if c.Items[0].Status != want {
				t.Fatalf("pushed %s, want %s", c.Items[0].Status, want)
			}
			return c
		case <-time.After(30 * time.Second):
			t.Fatalf("no push of %s", want)
		}
		return IntegrationCatalog{}
	}
	first := next(StatusUnknown)
	refresh(t, s)
	second := next(StatusAvailable)
	refresh(t, s) // 結果相同：不推
	if _, err := s.Connect(context.Background(), GitHub); err != nil {
		t.Fatal(err)
	}
	third := next(StatusConnected) // 相同的那次若推了，這裡會先讀到 available
	if _, err := s.Disconnect(context.Background(), GitHub); err != nil {
		t.Fatal(err)
	}
	fourth := next(StatusAvailable)
	if !(first.Rev < second.Rev && second.Rev < third.Rev && third.Rev < fourth.Rev) {
		t.Fatalf("revs %d %d %d %d", first.Rev, second.Rev, third.Rev, fourth.Rev)
	}
	s.Close()
	if err := <-done; err != nil {
		t.Fatalf("Watch after Close: %v", err)
	}
	select {
	case c := <-got:
		t.Fatalf("extra push %+v", c)
	default:
	}
}

// TestArgs 是 IG13、IG14：gh 從 Env 的 PATH 找到；只跑一次
// `gh auth status --json hosts --hostname github.com --active`，帶不互動的環境。
func TestArgs(t *testing.T) {
	f := newFixture(t)
	envFile := filepath.Join(f.root, "env")
	f.gh(t, `env > "`+envFile+`"; echo '{"hosts":{}}'`)
	refresh(t, f.service(t, 0))
	b, err := os.ReadFile(filepath.Join(f.root, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(b)); !slices.Equal(got, ghStatusArgs) {
		t.Fatalf("args %q, want %q", got, ghStatusArgs)
	}
	e, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range ghEnv {
		if !strings.Contains(string(e), want+"\n") {
			t.Errorf("env lacks %s", want)
		}
	}
}

// TestRefreshJoins：Refresh 等到進行中的那次結束，再起一次，讀到呼叫之後的狀態。
func TestRefreshJoins(t *testing.T) {
	f := newFixture(t)
	gate := filepath.Join(f.root, "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	passed := filepath.Join(f.root, "passed")
	// 第一次卡在讀 gate，直到測試寫入，回報沒登入；之後的都回報接得上。
	f.gh(t, `if [ ! -e "`+passed+`" ]; then read x < "`+gate+`"; touch "`+passed+`"; echo '{"hosts":{}}'; else `+ready+`; fi`)
	s := f.service(t, 0)
	s.Start()
	res := make(chan IntegrationCatalog, 1)
	errs := make(chan error, 1)
	go func() {
		c, err := s.Refresh(context.Background())
		res <- c
		errs <- err
	}()
	g, err := os.OpenFile(gate, os.O_WRONLY, 0) // 等第一次的 gh 打開 gate
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.WriteString("go\n"); err != nil {
		t.Fatal(err)
	}
	g.Close()
	select {
	case c := <-res:
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		wantItem(t, c.Items[0], StatusAvailable, false, "may", nil)
	case <-time.After(30 * time.Second):
		t.Fatal("Refresh did not return")
	}
	if n := f.calls(t); n != 2 {
		t.Fatalf("gh ran %d times, want 2", n)
	}
}

// TestClosed：關閉後的呼叫回 integration.closed；Close 可以重複呼叫。
func TestClosed(t *testing.T) {
	f := newFixture(t)
	s := f.service(t, 0)
	s.Close()
	s.Close()
	if _, err := s.Refresh(context.Background()); !errors.Is(err, errClosed) {
		t.Errorf("Refresh: %v", err)
	}
	if _, err := s.Disconnect(context.Background(), GitHub); !errors.Is(err, errClosed) {
		t.Errorf("Disconnect: %v", err)
	}
}

// TestCodes：參數就是說明裡的佔位符。
func TestCodes(t *testing.T) {
	for code, want := range map[Code][]string{
		CodeGhMissing: nil, CodeGhLoggedOut: nil, CodeGhScopeMissing: {"scope"},
		CodeGhUnreachable: {"detail"}, CodeGhFailed: {"detail"}, CodeUnknown: {"id"}, CodeClosed: nil,
	} {
		if got := code.Params(); !slices.Equal(got, want) {
			t.Errorf("%s params %v, want %v", code, got, want)
		}
	}
	if len(Codes()) != 7 {
		t.Errorf("codes %v", Codes())
	}
}

// TestDetail：逾時就說逾時（stderr 裡的提示不蓋過它，IG10）；說明截到 200 個字元，不切開中文。
func TestDetail(t *testing.T) {
	if got := ghDetail(proc.Result{TimedOut: true, Stderr: []byte("A new release of gh is available\n"), Err: errors.New("x")}); got != "timed out" {
		t.Errorf("timeout detail %q", got)
	}
	long := strings.Repeat("代理", 150)
	got := clip(long)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 200 {
		t.Errorf("clip: valid %v, %d runes", utf8.ValidString(got), utf8.RuneCountInString(got))
	}
	if clip("short") != "short" {
		t.Error("clip changed a short string")
	}
}

// TestConnectWithoutCheck：檢測被中途結束（Runner 關了）時沒有結果，連接不能當作通過。
func TestConnectWithoutCheck(t *testing.T) {
	f := newFixture(t)
	f.gh(t, ready)
	s := f.service(t, 0)
	f.r.Close()
	if _, err := s.Connect(context.Background(), GitHub); !errors.Is(err, errClosed) {
		t.Fatalf("err %v, want closed", err)
	}
	if ids := f.stored(t); len(ids) != 0 {
		t.Fatalf("stored %v without a passing check", ids)
	}
}

// gatedStore 是 Store 的真實現（MemStore）外加一道門：寫入停在門前，直到測試放行。
type gatedStore struct {
	*MemStore
	entered chan struct{} // 寫入到了門前時關閉
	release chan struct{} // 測試關閉後放行
	done    atomic.Bool   // 寫入已完成
}

func (g *gatedStore) PutEnabledIntegrations(ctx context.Context, ids []IntegrationID) error {
	close(g.entered)
	<-g.release
	err := g.MemStore.PutEnabledIntegrations(ctx, ids)
	g.done.Store(true)
	return err
}

// TestCloseWaitsForWrite：連接正在寫標記時 Close，Close 等它寫完才返回（之後 app 才關 store），
// 寫入沒有遺失；關閉之後不再寫。
func TestCloseWaitsForWrite(t *testing.T) {
	f := newFixture(t)
	f.gh(t, ready)
	g := &gatedStore{MemStore: f.store, entered: make(chan struct{}), release: make(chan struct{})}
	s, err := New(context.Background(), Options{Runner: f.r, Env: f.env, Store: g, Root: f.root})
	if err != nil {
		t.Fatal(err)
	}
	connected := make(chan error, 1)
	go func() { _, err := s.Connect(context.Background(), GitHub); connected <- err }()
	<-g.entered
	wroteBeforeReturn := make(chan bool, 1)
	go func() { s.Close(); wroteBeforeReturn <- g.done.Load() }()
	close(g.release)
	if err := <-connected; err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !<-wroteBeforeReturn {
		t.Fatal("Close returned before the write finished")
	}
	if ids := f.stored(t); !slices.Equal(ids, []IntegrationID{GitHub}) {
		t.Fatalf("stored %v", ids)
	}
	if _, err := s.Disconnect(context.Background(), GitHub); !errors.Is(err, errClosed) {
		t.Fatalf("Disconnect after Close: %v", err)
	}
}
