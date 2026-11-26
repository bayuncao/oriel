//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bayuncao/oriel/internal/proc"
)

// machine 是假機器：一個系統目錄的根、一個 home，以及
// 一個不從開發者環境繼承任何東西的環境快照。
type machine struct {
	t    *testing.T
	root string
	home string
	path string // 快照的 PATH 目錄
	env  proc.Env
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &machine{t: t, root: root, home: filepath.Join(root, "home"), path: filepath.Join(root, "path")}
	m.mkdir(m.home)
	m.mkdir(m.path)
	m.env = proc.Env{"HOME=" + m.home, "PATH=" + m.path + ":/usr/bin:/bin"}
	return m
}

func (m *machine) mkdir(d string) {
	if err := os.MkdirAll(d, 0o755); err != nil {
		m.t.Fatal(err)
	}
}

// script 寫入一個可執行的 shell 腳本並回傳其路徑。每次
// 執行會在 path+".runs" 附加一行。
func (m *machine) script(path, body string) string {
	m.t.Helper()
	m.mkdir(filepath.Dir(path))
	_ = os.Remove(path + ".runs")
	content := "#!/bin/sh\necho run >> '" + path + ".runs'\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		m.t.Fatal(err)
	}
	return path
}

func (m *machine) symlink(target, link string) string {
	m.t.Helper()
	m.mkdir(filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		m.t.Fatal(err)
	}
	return link
}

func (m *machine) write(path, content string) {
	m.t.Helper()
	m.mkdir(filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func runs(path string) int {
	b, _ := os.ReadFile(path + ".runs")
	return strings.Count(string(b), "run")
}

func (m *machine) detector() *detector {
	runner := &proc.Runner{}
	m.t.Cleanup(runner.Close)
	return &detector{runner: runner, root: m.root, run: withoutDeadline(runner)}
}

// withoutDeadline 以 runner 真的執行假腳本，但不設逾時：探測在腳本
// 結束時結束，而非在計時器到期時。負載下 macOS 第一次執行每個新
// 腳本前的檢查可能超過產品的逾時，候選便被當成逾時而改選後面的候選。
// 逾時的行為由經 detector.run 回傳 timedOutResult 的測試涵蓋。
func withoutDeadline(runner *proc.Runner) func(context.Context, proc.Probe) proc.Result {
	return func(ctx context.Context, p proc.Probe) proc.Result {
		p.Timeout = 0
		return runner.Run(ctx, p)
	}
}

var fooVersion = &VersionProbe{Args: []string{"--version"}, Pattern: `^foo (\d+\.\d+\.\d+)`}

func fooDef() Definition {
	return Definition{ID: "foo", Name: "Foo", Detect: DetectSpec{Command: "foo", Dirs: []string{"~/.foo/bin"}}, Version: fooVersion}
}

func TestDetectLookupOrder(t *testing.T) {
	m := newMachine(t)
	onPath := m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	tool := m.script(filepath.Join(m.home, ".local/bin/foo"), "echo foo 2.0.0")
	vendor := m.script(filepath.Join(m.home, ".foo/bin/foo"), "echo foo 3.0.0")

	in := m.detector().detect(context.Background(), m.env, fooDef(), Override{}).inst
	if in.Availability != Ready || in.Path != onPath || in.PathSource != FromPath || in.Version != "1.0.0" {
		t.Fatalf("got %+v", in)
	}
	want := []struct {
		path string
		src  PathSource
	}{{onPath, FromPath}, {tool, FromToolDir}, {vendor, FromVendorDir}}
	if len(in.Candidates) != len(want) {
		t.Fatalf("candidates %+v", in.Candidates)
	}
	for i, w := range want {
		if c := in.Candidates[i]; c.Path != w.path || c.Source != w.src {
			t.Errorf("candidate %d = %+v, want %s from %s", i, c, w.path, w.src)
		}
	}
	if runs(tool) != 0 || runs(vendor) != 0 {
		t.Error("candidates after the chosen one were run")
	}
}

// TestDetectVersionTimeoutHoldsPlace 是 D137 的情境 S1–S6：只是逾時的
// 版本探測不代表候選錯了，所以它保住自己的順位，不回退到後面的候選；
// 無法執行與別的程式照舊試下一個。
func TestDetectVersionTimeoutHoldsPlace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		first    string // PATH 上的 foo 的腳本內容
		override bool
		timeouts int // 前幾次對 PATH 上的 foo 的探測逾時
		rounds   int // 檢測次數（第二次即背景重試）
		avail    Availability
		problem  ProblemCode
		chosen   PathSource // avail 為 Ready 時
		vendor   int        // 廠商目錄的 foo 執行的次數
	}{
		{"S1 timeout holds its place", "echo foo 1.0.0", false, 1, 1, Broken, CodeVersionTimeout, "", 0},
		{"S2 retry answers", "echo foo 1.0.0", false, 1, 2, Ready, "", FromPath, 0},
		{"S3 retry times out again", "echo foo 1.0.0", false, 2, 2, Broken, CodeVersionTimeout, "", 0},
		{"S4 failure falls through", "exit 3", false, 0, 1, Ready, "", FromVendorDir, 1},
		{"S5 other program falls through", "echo something else", false, 0, 1, Ready, "", FromVendorDir, 1},
		{"S6 override timeout is broken", "echo foo 1.0.0", true, 1, 1, Broken, CodeVersionTimeout, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t)
			first := m.script(filepath.Join(m.path, "foo"), tc.first)
			vendor := m.script(filepath.Join(m.home, ".foo/bin/foo"), "echo foo 3.0.0")
			d := m.detector()
			real := d.run
			var timeouts atomic.Int32
			d.run = func(ctx context.Context, p proc.Probe) proc.Result {
				if p.Path == first && timeouts.Add(1) <= int32(tc.timeouts) {
					return timedOutResult
				}
				return real(ctx, p)
			}
			ov := Override{}
			if tc.override {
				ov = Override{Agent: "foo", Executable: first}
			}
			var r detection
			for range tc.rounds {
				r = d.detect(context.Background(), m.env, fooDef(), ov)
			}
			in := r.inst
			if in.Availability != tc.avail || in.Problem.Code != tc.problem {
				t.Fatalf("got %s %+v, want %s %s", in.Availability, in.Problem, tc.avail, tc.problem)
			}
			if tc.avail == Ready && in.PathSource != tc.chosen {
				t.Errorf("chose %s from %s, want %s", in.Path, in.PathSource, tc.chosen)
			}
			if r.timedOut != (tc.problem == CodeVersionTimeout) {
				t.Errorf("timedOut = %v", r.timedOut)
			}
			if tc.problem == CodeVersionTimeout && !tc.override && len(in.Candidates) != 2 {
				t.Errorf("later candidates not listed: %+v", in.Candidates)
			}
			if n := runs(vendor); n != tc.vendor {
				t.Errorf("vendor foo ran %d times, want %d", n, tc.vendor)
			}
		})
	}
}

func TestDetectSkipsOtherProgramWithSameName(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo something else")
	vendor := m.script(filepath.Join(m.home, ".foo/bin/foo"), "echo foo 3.0.0")
	in := m.detector().detect(context.Background(), m.env, fooDef(), Override{}).inst
	if in.Availability != Ready || in.Path != vendor {
		t.Fatalf("got %+v", in)
	}
	if in.Candidates[0].Rejected.Code != CodeVersionMismatch {
		t.Errorf("first candidate: %+v", in.Candidates[0])
	}
}

func TestDetectOnlyOtherProgramsIsMissing(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo something else")
	in := m.detector().detect(context.Background(), m.env, fooDef(), Override{}).inst
	if in.Availability != Missing || in.Problem.Code != CodeVersionMismatch || len(in.Candidates) != 1 {
		t.Fatalf("got %+v", in)
	}
}

func TestDetectMissing(t *testing.T) {
	m := newMachine(t)
	in := m.detector().detect(context.Background(), m.env, fooDef(), Override{}).inst
	if in.Availability != Missing || len(in.Candidates) != 0 || in.Auth != AuthUnknown {
		t.Fatalf("got %+v", in)
	}
}

func TestDetectOverride(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	good := m.script(filepath.Join(m.root, "opt/foo"), "echo foo 9.0.0")
	wrong := m.script(filepath.Join(m.root, "opt/bar"), "echo bar 1.0.0")
	plain := filepath.Join(m.root, "opt/plain")
	m.write(plain, "not executable")

	for _, tc := range []struct {
		name  string
		path  string
		avail Availability
	}{
		{"accepted", good, Ready},
		{"rejected does not fall back", wrong, Broken},
		{"not executable", plain, Broken},
		{"does not exist", filepath.Join(m.root, "nope"), Broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := m.detector().detect(context.Background(), m.env, fooDef(), Override{Agent: "foo", Executable: tc.path}).inst
			if in.Availability != tc.avail || len(in.Candidates) != 1 || in.Candidates[0].Source != FromOverride {
				t.Fatalf("got %+v", in)
			}
			if tc.avail == Broken && (in.Problem.Code == "" || in.Path != "") {
				t.Fatalf("broken override: %+v", in)
			}
		})
	}
}

// 自訂 agent 的命令是絕對路徑時，它是唯一的候選，來源是 command 而不是覆寫（使用者沒設覆寫）；
// 被拒時同樣不退回 PATH 上的同名程式。
func TestDetectAbsoluteCommand(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	good := m.script(filepath.Join(m.root, "opt/foo"), "echo foo 9.0.0")
	wrong := m.script(filepath.Join(m.root, "opt/bar"), "echo bar 1.0.0")
	for _, tc := range []struct {
		name  string
		cmd   string
		avail Availability
	}{
		{"accepted", good, Ready},
		{"rejected does not fall back", wrong, Broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := fooDef()
			def.Detect.Command = tc.cmd
			in := m.detector().detect(context.Background(), m.env, def, Override{}).inst
			if in.Availability != tc.avail || len(in.Candidates) != 1 || in.Candidates[0].Source != FromCommand {
				t.Fatalf("got %+v", in)
			}
			if tc.avail == Ready && in.PathSource != FromCommand {
				t.Fatalf("path source %s, want %s", in.PathSource, FromCommand)
			}
		})
	}
}

func TestDetectNameSharedNeedsBrew(t *testing.T) {
	m := newMachine(t)
	def := Definition{
		ID: "grok", Detect: DetectSpec{Command: "grok", Dirs: []string{"~/.grok/bin"}, Brew: []string{"grok-build"}, NameShared: true},
		Version: &VersionProbe{Args: []string{"--version"}, Pattern: `^grok (\d+\.\d+\.\d+) \([0-9a-f]+\)`},
	}
	// Homebrew 無關的 grok formula，甚至會印出
	// 符合的版本。
	regex := m.script(filepath.Join(m.root, "opt/homebrew/Cellar/grok/1.0/bin/grok"), "echo grok 1.0.0 (abc)")
	m.symlink(regex, filepath.Join(m.path, "grok"))

	in := m.detector().detect(context.Background(), m.env, def, Override{}).inst
	if in.Availability != Missing || runs(regex) != 0 {
		t.Fatalf("regex grok: %+v, runs %d", in, runs(regex))
	}

	// grok-build cask 從 PATH 被接受。
	cask := m.script(filepath.Join(m.root, "opt/homebrew/Caskroom/grok-build/1.0.62/grok-1.0.62-macos-aarch64"), "echo grok 1.0.62 '(0920c81)'")
	m.symlink(cask, filepath.Join(m.root, "opt/homebrew/bin/grok"))
	in = m.detector().detect(context.Background(), m.env, def, Override{}).inst
	if in.Availability != Ready || in.PathSource != FromToolDir || in.Version != "1.0.62" {
		t.Fatalf("cask: %+v", in)
	}

	// 廠商目錄只需要版本檢查。
	vendor := m.script(filepath.Join(m.home, ".grok/downloads/grok-1.0.52"), "echo grok 1.0.52 '(0920c81)'")
	m.symlink(vendor, filepath.Join(m.home, ".grok/bin/grok"))
	if err := os.Remove(filepath.Join(m.root, "opt/homebrew/bin/grok")); err != nil {
		t.Fatal(err)
	}
	in = m.detector().detect(context.Background(), m.env, def, Override{}).inst
	if in.Availability != Ready || in.PathSource != FromVendorDir || in.Version != "1.0.52" {
		t.Fatalf("vendor: %+v", in)
	}
	// 啟動路徑是找到的那個，而非帶版本的真實檔案。
	if in.Path != filepath.Join(m.home, ".grok/bin/grok") || in.Candidates[len(in.Candidates)-1].Real != vendor {
		t.Fatalf("paths: %+v", in)
	}

	// 在 PATH 上的廠商目錄，以及安裝程式放在
	// ~/.local/bin 的符號連結，也算是廠商的安裝。
	link := m.symlink(vendor, filepath.Join(m.home, ".local/bin/grok"))
	in = m.detector().detect(context.Background(), m.env, def, Override{}).inst
	if in.Availability != Ready || in.Path != link || in.PathSource != FromToolDir {
		t.Fatalf("installer symlink: %+v", in)
	}
	in = m.detector().detect(context.Background(), m.env.With("PATH="+filepath.Join(m.home, ".grok/bin")), def, Override{}).inst
	if in.Availability != Ready || in.Path != filepath.Join(m.home, ".grok/bin/grok") || in.PathSource != FromPath {
		t.Fatalf("vendor dir on PATH: %+v", in)
	}
}

func TestDetectNPMReadsPackageJSON(t *testing.T) {
	m := newMachine(t)
	pkg := filepath.Join(m.home, ".npm-global/lib/node_modules/@anthropic-ai/claude-code")
	m.write(filepath.Join(pkg, "package.json"), `{"name":"@anthropic-ai/claude-code","version":"2.1.287"}`)
	cli := m.script(filepath.Join(pkg, "cli.js"), "exit 1")
	m.symlink(cli, filepath.Join(m.path, "claude"))
	def := Definition{ID: "claude", Detect: DetectSpec{Command: "claude", NPM: []string{"@anthropic-ai/claude-code"}},
		Version: &VersionProbe{Args: []string{"--version"}, Pattern: `^(\d+\.\d+\.\d+) \(Claude Code\)`}}

	in := m.detector().detect(context.Background(), m.env, def, Override{}).inst
	if in.Availability != Ready || in.Version != "2.1.287" || runs(cli) != 0 {
		t.Fatalf("got %+v, runs %d", in, runs(cli))
	}

	// 有 npm 套件但沒有版本探測時，套件之外的程式
	// 不是該 Agent。
	m.script(filepath.Join(m.home, ".local/bin/pi"), "echo 1.0.0")
	piDef := Definition{ID: "pi", Detect: DetectSpec{Command: "pi", NPM: []string{"@earendil-works/pi-coding-agent"}}}
	if in := m.detector().detect(context.Background(), m.env, piDef, Override{}).inst; in.Availability != Missing || in.Problem.Code != CodeNotFromNPM {
		t.Fatalf("pi outside npm: %+v", in)
	}
}

func TestDetectWithoutIdentityAcceptsFirst(t *testing.T) {
	m := newMachine(t)
	npx := m.script(filepath.Join(m.path, "npx"), "exit 1")
	def := Definition{ID: "registry:gemini", Detect: DetectSpec{Command: "npx"}}
	in := m.detector().detect(context.Background(), m.env, def, Override{}).inst
	if in.Availability != Ready || in.Path != npx || in.Version != "" || runs(npx) != 0 {
		t.Fatalf("got %+v", in)
	}
}

func TestDetectCustomAbsoluteCommand(t *testing.T) {
	m := newMachine(t)
	bin := m.script(filepath.Join(m.root, "opt/agent"), "")
	def := Definition{ID: "custom:x", Detect: DetectSpec{Command: bin}}
	in := m.detector().detect(context.Background(), m.env, def, Override{}).inst
	if in.Availability != Ready || in.Path != bin {
		t.Fatalf("got %+v", in)
	}
}

func TestDetectUnwrapsMise(t *testing.T) {
	m := newMachine(t)
	real := m.script(filepath.Join(m.home, ".local/share/mise/installs/foo/1/bin/foo"), "echo foo 1.2.3")
	m.script(filepath.Join(m.path, "mise"), `[ "$*" = "which foo" ] && echo `+real)
	wrapper := m.script(filepath.Join(m.path, "foo"), `exec mise x -- foo "$@"`)
	in := m.detector().detect(context.Background(), m.env, fooDef(), Override{}).inst
	if in.Availability != Ready || in.Path != real || in.Candidates[0].Path != wrapper || in.Version != "1.2.3" {
		t.Fatalf("got %+v", in)
	}
}

func TestDetectRemembersFailures(t *testing.T) {
	m := newMachine(t)
	foo := m.script(filepath.Join(m.path, "foo"), "echo crashed >&2; exit 2")
	d := m.detector()
	for range 2 {
		if in := d.detect(context.Background(), m.env, fooDef(), Override{}).inst; in.Availability != Broken || in.Problem.Code != CodeVersionFailed || in.Problem.Params["output"] != "crashed" {
			t.Fatalf("got %+v", in)
		}
	}
	if runs(foo) != 1 {
		t.Fatalf("failing binary ran %d times, want 1", runs(foo))
	}
	// 不同的 PATH 可能補上它缺的東西。
	d.detect(context.Background(), m.env.With("PATH="+m.path), fooDef(), Override{})
	if runs(foo) != 2 {
		t.Fatalf("runs with another PATH = %d, want 2", runs(foo))
	}
	// 改變過的二進位檔會被再次執行。
	m.script(foo, "echo foo 1.0.0 # now fixed")
	if in := d.detect(context.Background(), m.env, fooDef(), Override{}).inst; in.Availability != Ready {
		t.Fatalf("after change: %+v", in)
	}
	// Forget 清除記憶。
	m.script(foo, "exit 3")
	d.detect(context.Background(), m.env, fooDef(), Override{})
	d.detect(context.Background(), m.env, fooDef(), Override{})
	d.forget()
	d.detect(context.Background(), m.env, fooDef(), Override{})
	if n := runs(foo); n != 2 {
		t.Fatalf("runs after Forget = %d, want 2", n)
	}
}

func TestDetectVersionOnFailingExit(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0; exit 1")
	if in := m.detector().detect(context.Background(), m.env, fooDef(), Override{}).inst; in.Availability != Ready || in.Version != "1.0.0" {
		t.Fatalf("got %+v", in)
	}
}

func TestDetectTimeoutIsNotRemembered(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d := m.detector()
	var probes atomic.Int32
	d.run = func(context.Context, proc.Probe) proc.Result { probes.Add(1); return timedOutResult }
	for range 2 {
		r := d.detect(context.Background(), m.env, fooDef(), Override{})
		if r.inst.Availability != Broken || r.inst.Candidates[0].Rejected.Code != CodeVersionTimeout || !r.timedOut {
			t.Fatalf("got %+v", r)
		}
	}
	if n := probes.Load(); n != 2 {
		t.Fatalf("timed-out binary probed %d times, want 2", n)
	}
}

// timedOutResult 是逾時的探測；測試經 detector.run 回傳它，不必真的等逾時
// （腳本在逾時前是否來得及開始取決於機器的負載）。
var timedOutResult = proc.Result{ExitCode: -1, TimedOut: true, Err: &proc.Error{Code: proc.CodeTimedOut}}

func TestDetectOutdated(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.4.0")
	for _, tc := range []struct {
		name string
		vp   VersionProbe
		want Availability
	}{
		{"new enough", VersionProbe{Min: "1.2.0"}, Ready},
		{"too old", VersionProbe{Min: "1.10.0"}, Outdated},
		{"excluded", VersionProbe{Exclude: []string{"1.4.0"}}, Outdated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := fooDef()
			vp := tc.vp
			vp.Args, vp.Pattern = fooVersion.Args, fooVersion.Pattern
			def.Version = &vp
			in := m.detector().detect(context.Background(), m.env, def, Override{}).inst
			if in.Availability != tc.want || in.Path == "" || (tc.want == Outdated) != (in.Problem.Code != "") {
				t.Fatalf("got %+v", in)
			}
		})
	}
}

func TestDetectAuth(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), `case "$1" in
  --version) echo foo 1.0.0;;
  status) echo "$STATUS"; exit "$CODE";;
esac`)
	for _, tc := range []struct {
		name, status, code, re string
		want                   AuthState
	}{
		{"pattern matches", `{"loggedIn": true}`, "0", `"loggedIn":\s*true`, Authenticated},
		{"pattern fails despite exit 0", `{"loggedIn": false}`, "0", `"loggedIn":\s*true`, Unauthenticated},
		{"exit 0", "Logged in", "0", "", Authenticated},
		{"exit 1", "Not logged in", "1", "", Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := fooDef()
			def.Auth = &AuthProbe{Args: []string{"status"}, AuthenticatedRE: tc.re}
			env := m.env.With("STATUS="+tc.status, "CODE="+tc.code)
			if in := m.detector().detect(context.Background(), env, def, Override{}).inst; in.Auth != tc.want {
				t.Fatalf("auth = %s, want %s", in.Auth, tc.want)
			}
		})
	}
}

func TestDetectWithoutHomeDirectory(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	env := m.env.With("HOME=" + filepath.Join(m.root, "gone"))
	if in := m.detector().detect(context.Background(), env, fooDef(), Override{}).inst; in.Availability != Ready {
		t.Fatalf("got %+v", in)
	}
}

func TestDetectInvalidPatternIsNotRemembered(t *testing.T) {
	m := newMachine(t)
	foo := m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	def := fooDef()
	def.Version = &VersionProbe{Args: []string{"--version"}, Pattern: "("}
	det := m.detector()
	in := det.detect(context.Background(), m.env, def, Override{}).inst
	if in.Availability != Broken || in.Problem.Code != CodeInvalidVersionPattern {
		t.Fatalf("got %+v", in)
	}
	if len(det.failed) != 0 || runs(foo) != 0 {
		t.Fatalf("remembered %v, ran %d times", det.failed, runs(foo))
	}
	// 修正 Definition 後不必等手動刷新。
	if in := det.detect(context.Background(), m.env, fooDef(), Override{}).inst; in.Availability != Ready {
		t.Fatalf("after fixing the pattern: %+v", in)
	}
}

func TestDetectForgetDuringProbe(t *testing.T) {
	m := newMachine(t)
	started, gate := m.fifo("started"), m.fifo("gate")
	m.script(filepath.Join(m.path, "foo"), "echo > '"+started+"'\nread _ < '"+gate+"'\nexit 1")
	det := m.detector()
	done := make(chan Installation, 1)
	go func() { done <- det.detect(context.Background(), m.env, fooDef(), Override{}).inst }()
	await(t, started)
	det.forget() // 使用者按了重新整理
	release(t, gate)
	if in := recv(t, done); in.Availability != Broken {
		t.Fatalf("got %+v", in)
	}
	// 開始於 forget 之前的失敗不得留下紀錄。
	if len(det.failed) != 0 {
		t.Fatalf("remembered %v", det.failed)
	}
}
