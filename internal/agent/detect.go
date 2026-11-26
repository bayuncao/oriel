package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
)

// detector 尋找一個 Agent 的可執行檔、確認它確實是該 Agent，
// 並讀取其版本與登入狀態。規則見
// docs/architecture/agent.md（「發現」）。它會記住失敗的探測，
// 讓同一個二進位檔在改變之前不會被再次執行。
type detector struct {
	runner *proc.Runner
	run    func(context.Context, proc.Probe) proc.Result // 測試的接縫，涵蓋 detector 啟動的每個探測；nil 表示 runner.Run
	root   string                                        // 前綴加到系統目錄上；"" 表示 /
	// 逾時；零值表示使用文件中的預設值。
	versionTimeout time.Duration
	authTimeout    time.Duration
	unwrapTimeout  time.Duration

	mu     sync.Mutex
	failed map[failKey]Problem
	epoch  int // forget 時遞增；開始於舊 epoch 的探測不再記下失敗
}

// fileID 識別磁碟上的一個可執行檔：其路徑以及其
// 真實檔案的大小與修改時間。
type fileID struct {
	exec string
	size int64
	mod  time.Time
}

// failKey 是探測失敗的二進位檔，連同執行時的 PATH（直譯器
// 不在 PATH 上的腳本，改用快照環境也許就能運作）。
type failKey struct {
	fileID
	path string
}

// fingerprint 讓 Verify 能判斷選定的可執行檔未曾改變。
type fingerprint struct {
	fileID
	real string
}

func (f fingerprint) current() bool {
	if f.exec == "" || !proc.Executable(f.exec) {
		return false
	}
	fi, err := os.Stat(f.real)
	return err == nil && fi.Size() == f.size && fi.ModTime().Equal(f.mod)
}

// detection 是 Installation 加上 Discovery 所需的其他資訊。
type detection struct {
	inst     Installation
	fp       fingerprint
	timedOut bool // 某個探測逾時；值得重試一次
}

// verdict 是檢查一個候選的結果，超出其 Rejected 原因的部分。
type verdict struct {
	failed   bool // 無法執行，而非它是別的程式
	timedOut bool
	fp       fingerprint
}

// forget 清除記住的探測失敗紀錄。
func (d *detector) forget() {
	d.mu.Lock()
	d.failed = nil
	d.epoch++
	d.mu.Unlock()
}

// detect 回傳 def 在 env 中的 Installation，並尊重 ov。
func (d *detector) detect(ctx context.Context, env proc.Env, def Definition, ov Override) detection {
	res := detection{inst: Installation{Agent: def.ID, Availability: Missing, Auth: AuthUnknown, ProbedAt: time.Now()}}
	cands := d.candidates(env, def, ov)
	res.inst.Candidates = cands
	if len(cands) == 0 {
		return res
	}
	unwrap := proc.Unwrapper{Run: d.runProbe, Root: d.root, Env: env, Timeout: d.unwrapTimeout}
	var broken Problem
	for i := range cands {
		c := &cands[i]
		v := d.check(ctx, env, def, &unwrap, c)
		res.timedOut = res.timedOut || v.timedOut
		if c.Rejected.Code == "" {
			res.inst.Availability = Ready
			res.inst.Path, res.inst.PathSource, res.inst.Version = c.Exec, c.Source, c.Version
			res.fp = v.fp
			break
		}
		// 指定的路徑（覆寫或自訂 agent 的絕對路徑命令）被拒即為 broken，不回退；
		// 只是逾時不代表候選錯了，它保住順位，等背景重試（D137）。
		if c.Source.Pinned() || v.timedOut {
			broken = c.Rejected
			break
		}
		if v.failed && broken.Code == "" {
			broken = c.Rejected
		}
	}
	switch {
	case res.inst.Availability != Ready && broken.Code != "":
		res.inst.Availability, res.inst.Problem = Broken, broken
	case res.inst.Availability != Ready:
		res.inst.Problem = cands[0].Rejected
	default:
		if p := outdated(def.Version, res.inst.Version); p.Code != "" {
			res.inst.Availability, res.inst.Problem = Outdated, p
		}
		res.inst.Auth = d.auth(ctx, env, def.Auth, res.inst.Path)
	}
	return res
}

// candidates 依查找順序列出為 def 找到的每個可執行檔。
// 設定了使用者的覆寫、或自訂 agent 的絕對路徑命令時，
// 它是唯一的候選。
func (d *detector) candidates(env proc.Env, def Definition, ov Override) []Candidate {
	if p := ov.Executable; p != "" {
		return []Candidate{{Path: p, Source: FromOverride}}
	}
	cmd := def.Detect.Command
	if filepath.IsAbs(cmd) {
		return []Candidate{{Path: cmd, Source: FromCommand}}
	}
	var out []Candidate
	for _, src := range []struct {
		source PathSource
		dirs   []string
	}{
		{FromPath, env.PathDirs()},
		{FromToolDir, proc.ToolDirs(env, d.root)},
		{FromVendorDir, vendorDirs(env, def.Detect)},
	} {
		for _, p := range proc.Look(cmd, src.dirs) {
			if !slices.ContainsFunc(out, func(c Candidate) bool { return c.Path == p }) {
				out = append(out, Candidate{Path: p, Source: src.source})
			}
		}
	}
	return out
}

// vendorDirs 是 spec.Dirs 在 env 中展開後的結果，不含空值。
func vendorDirs(env proc.Env, spec DetectSpec) []string {
	var out []string
	for _, dir := range spec.Dirs {
		if dir = env.Expand(dir); dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

// vendorInstalled 回報在別處找到的候選是否為廠商的安裝：
// 它位於其中一個廠商目錄（也可能在 PATH 上），或與該處的
// 命令是同一個檔案（安裝程式放在 ~/.local/bin 的符號連結）。
func vendorInstalled(env proc.Env, spec DetectSpec, path, real string) bool {
	for _, dir := range vendorDirs(env, spec) {
		if filepath.Dir(path) == filepath.Clean(dir) {
			return true
		}
		if r, err := filepath.EvalSymlinks(filepath.Join(dir, spec.Command)); err == nil && r == real {
			return true
		}
	}
	return false
}

// check 解開 c 的包裝、確認它是 def 的 Agent 並讀取其版本。
// 不是時設定 c.Rejected。
func (d *detector) check(ctx context.Context, env proc.Env, def Definition, unwrap *proc.Unwrapper, c *Candidate) verdict {
	if !proc.Executable(c.Path) {
		c.Rejected = problem(CodeNotExecutable, "path", c.Path)
		return verdict{failed: true}
	}
	c.Exec = unwrap.Unwrap(ctx, c.Path, filepath.Base(c.Path))
	real, err := filepath.EvalSymlinks(c.Exec)
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Stat(real)
	}
	if err != nil {
		c.Rejected = problem(CodeUnreadable, "path", c.Exec, "detail", err.Error())
		return verdict{failed: true}
	}
	c.Real = real
	id := fileID{exec: c.Exec, size: fi.Size(), mod: fi.ModTime()}
	v := verdict{fp: fingerprint{fileID: id, real: real}}

	spec := def.Detect
	if spec.NameShared && (c.Source == FromPath || c.Source == FromToolDir) && !inBrew(real, spec.Brew) && !vendorInstalled(env, spec, c.Path, real) {
		c.Rejected = problem(CodeOtherProgram, "command", spec.Command, "brew", strings.Join(spec.Brew, ", "))
		return v
	}
	if ver, ok := npmVersion(real, spec.NPM); ok {
		c.Version = ver
		return v
	}
	if def.Version == nil {
		if len(spec.NPM) > 0 {
			c.Rejected = problem(CodeNotFromNPM, "packages", strings.Join(spec.NPM, ", "))
		}
		return v
	}

	// Definition 本身寫錯不是這個程式的失敗，不記住。
	re, err := regexp.Compile(def.Version.Pattern)
	if err != nil {
		c.Rejected, v.failed = problem(CodeInvalidVersionPattern, "detail", err.Error()), true
		return v
	}
	key := failKey{fileID: id, path: env.Get("PATH")}
	d.mu.Lock()
	reason, known := d.failed[key]
	epoch := d.epoch
	d.mu.Unlock()
	if known {
		c.Rejected, v.failed = reason, true
		return v
	}
	v.failed, v.timedOut = d.version(ctx, env, def.Version.Args, re, c)
	if v.failed && !v.timedOut && ctx.Err() == nil {
		d.remember(key, epoch, c.Rejected)
	}
	return v
}

// remember 記下 key 的失敗原因，除非探測開始後失敗紀錄已被 forget。
func (d *detector) remember(key failKey, epoch int, reason Problem) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.epoch != epoch {
		return
	}
	if d.failed == nil {
		d.failed = map[failKey]Problem{}
	}
	d.failed[key] = reason
}

// version 對 c 執行版本探測，設定其 Version 或 Rejected。
// 輸出不符合 pattern 表示是另一個程式；沒有版本的非零
// 結束、或沒有結束，表示它無法執行。
func (d *detector) version(ctx context.Context, env proc.Env, args []string, re *regexp.Regexp, c *Candidate) (failed, timedOut bool) {
	res := d.probe(ctx, env, c.Exec, args, cmp.Or(d.versionTimeout, 10*time.Second))
	out := strings.TrimSpace(string(res.Output()))
	switch m := re.FindStringSubmatch(out); {
	case res.TimedOut:
		c.Rejected = problem(CodeVersionTimeout)
		return true, true
	case len(m) > 1:
		c.Version = m[1]
		return false, false
	case res.Err != nil:
		c.Rejected = problem(CodeVersionFailed, "detail", res.Err.Error(), "output", firstLine(out))
		return true, false
	}
	c.Rejected = problem(CodeVersionMismatch, "output", firstLine(out))
	return false, false
}

// auth 執行官方的狀態命令；任何使它無法回答的狀況
// 都讓狀態維持未知。
func (d *detector) auth(ctx context.Context, env proc.Env, ap *AuthProbe, exe string) AuthState {
	if ap == nil {
		return AuthUnknown
	}
	res := d.probe(ctx, env, exe, ap.Args, cmp.Or(d.authTimeout, 5*time.Second))
	if res.TimedOut || res.ExitCode < 0 {
		return AuthUnknown
	}
	ok := res.ExitCode == 0
	if ap.AuthenticatedRE != "" {
		re, err := regexp.Compile(ap.AuthenticatedRE)
		if err != nil {
			return AuthUnknown
		}
		ok = re.Match(res.Output())
	}
	if ok {
		return Authenticated
	}
	return Unauthenticated
}

// probe 以空的 stdin 且無顏色的方式執行 agent 自己的可執行檔。
func (d *detector) probe(ctx context.Context, env proc.Env, exe string, args []string, timeout time.Duration) proc.Result {
	return d.runProbe(ctx, proc.Probe{Path: exe, Args: args, Env: env.With("NO_COLOR=1"), Dir: env.WorkDir(), Timeout: timeout})
}

// runProbe 經測試接縫 run（若有）執行 p，否則交給 runner。
func (d *detector) runProbe(ctx context.Context, p proc.Probe) proc.Result {
	if d.run != nil {
		return d.run(ctx, p)
	}
	return d.runner.Run(ctx, p)
}

// outdated 說明 version 在 vp 下為何過時；不過時回傳零值。
func outdated(vp *VersionProbe, version string) Problem {
	switch {
	case vp == nil || version == "":
		return Problem{}
	case slices.Contains(vp.Exclude, version):
		return problem(CodeVersionExcluded, "version", version)
	case vp.Min != "" && proc.CompareVersions(version, vp.Min) < 0:
		return problem(CodeVersionTooOld, "version", version, "min", vp.Min)
	}
	return Problem{}
}

// npmVersion 在 real 位於其中一個 npm 套件內時，從
// package.json 讀取版本，不需執行任何東西。
func npmVersion(real string, pkgs []string) (string, bool) {
	slash := filepath.ToSlash(real)
	for _, pkg := range pkgs {
		marker := "/node_modules/" + pkg + "/"
		i := strings.LastIndex(slash, marker)
		if i < 0 {
			continue
		}
		b, err := os.ReadFile(filepath.FromSlash(slash[:i+len(marker)] + "package.json"))
		if err != nil {
			continue
		}
		var p struct{ Name, Version string }
		if json.Unmarshal(b, &p) == nil && p.Name == pkg && p.Version != "" {
			return p.Version, true
		}
	}
	return "", false
}

// inBrew 回報 real 是否位於其中一個 Homebrew formula 或
// cask 內。
func inBrew(real string, pkgs []string) bool {
	slash := filepath.ToSlash(real)
	return slices.ContainsFunc(pkgs, func(p string) bool {
		return strings.Contains(slash, "/Cellar/"+p+"/") || strings.Contains(slash, "/Caskroom/"+p+"/")
	})
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
