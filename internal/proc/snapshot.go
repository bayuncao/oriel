package proc

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// probeVars 在詢問 shell 期間設定，讓設定檔能辨識，
// 也讓 oh-my-zsh 既不自我更新也不啟動 tmux。它們會從
// 快照中移除，連同只描述探測 shell 本身的變數。
var (
	probeVars = []string{
		"ORIELHUB_SHELL_PROBE=1",
		"DISABLE_AUTO_UPDATE=true",
		"DISABLE_UPDATE_PROMPT=true",
		"ZSH_TMUX_AUTOSTART=false",
	}
	shellOnlyVars = []string{"_", "PWD", "OLDPWD", "SHLVL"}
)

// Snapshotter 取得使用者終端機會有的環境：從 Finder 或
// Dock 啟動的 app 只會得到 launchd 的精簡環境。
// 規則見 docs/architecture/agent.md。
type Snapshotter struct {
	Runner *Runner
	// Root 前綴加到系統路徑上（fish 與 nu 在其下查找）；
	// "" 表示 /。
	Root string
	// Timeout 限制每個 shell；零值表示 5 秒。
	Timeout time.Duration
}

// Snapshot 以 -ilc 詢問登入 shell，若無回應則改用
// -lc；已設定但非登入 shell 的 fish 與 nushell 會同時
// 被詢問，且只補充登入 shell 的回答。沒有 shell 回答時回傳
// inherited 與 ErrNoShell；ctx 結束時回傳 inherited 與 CodeCanceled
// （errors.Is 可得到 ctx.Err()）。
func (s *Snapshotter) Snapshot(ctx context.Context, inherited Env) (Env, error) {
	if !shellSnapshot {
		return inherited, nil
	}
	login := cmp.Or(inherited.Get("SHELL"), defaultShell)
	extra := s.extraShells(inherited, filepath.Base(login))

	answers := make([]Env, 1+len(extra))
	var wg sync.WaitGroup
	wg.Go(func() {
		for _, flags := range []string{"-ilc", "-lc"} {
			if env, ok := s.ask(ctx, inherited, login, flags, ""); ok {
				answers[0] = env
				return
			}
		}
	})
	for i, sh := range extra {
		wg.Go(func() {
			prefix := ""
			if filepath.Base(sh) == "nu" {
				prefix = "^" // 執行外部的 /bin/sh，而非 nushell 內建命令
			}
			if env, ok := s.ask(ctx, inherited, sh, "-lc", prefix); ok {
				answers[1+i] = env
			}
		})
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return inherited, fail(CodeCanceled, err)
	}
	if !slices.ContainsFunc(answers, func(e Env) bool { return e != nil }) {
		return inherited, ErrNoShell
	}
	base := answers[0]
	if base == nil {
		base = inherited
	}
	for _, e := range answers[1:] {
		if e != nil {
			base = merge(base, e)
		}
	}
	return base, nil
}

// merge 將 other 的 PATH 項目附加在 base 之後，並加入
// base 沒有的變數。
func merge(base, other Env) Env {
	out := slices.Clone(base)
	dirs := base.PathDirs()
	for _, d := range other.PathDirs() {
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	out = out.With("PATH=" + strings.Join(dirs, string(filepath.ListSeparator)))
	for _, kv := range other {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := out.Lookup(k); !ok {
			out = append(out, kv)
		}
	}
	return out
}

// extraShells 是已安裝且已設定的 fish 與 nushell，
// 除非其中之一是登入 shell。
func (s *Snapshotter) extraShells(env Env, login string) []string {
	home := env.Home()
	config := env.Get("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	configured := map[string][]string{
		"fish": {filepath.Join(config, "fish")},
		"nu":   {filepath.Join(config, "nushell")},
	}
	for _, d := range nuConfigDirs {
		configured["nu"] = append(configured["nu"], filepath.Join(home, d))
	}
	dirs := append(SearchDirs(env, s.Root), filepath.Join(s.Root, "/usr/bin"))
	var out []string
	for _, name := range []string{"fish", "nu"} {
		if name == login || home == "" {
			continue
		}
		found := Look(name, dirs)
		if len(found) == 0 || !slices.ContainsFunc(configured[name], isDir) {
			continue
		}
		out = append(out, found[0])
	}
	return out
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// ask 在 shell 內執行 /bin/sh，並在兩個標記之間讀取其匯出的
// 環境，因此設定檔印出的任何內容都會被忽略。只讀取 stdout。
func (s *Snapshotter) ask(ctx context.Context, inherited Env, shell, flags, prefix string) (Env, bool) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	marker := "__ORIELHUB_ENV_" + hex.EncodeToString(b[:]) + "__"
	script := prefix + "/bin/sh -c 'printf %s " + marker + "; /usr/bin/env -0; printf %s " + marker + "'"
	res := s.Runner.Run(ctx, Probe{
		Path:    shell,
		Args:    []string{flags, script},
		Env:     inherited.With(probeVars...),
		Dir:     inherited.WorkDir(),
		Timeout: cmp.Or(s.Timeout, 5*time.Second),
	})
	if res.TimedOut || ctx.Err() != nil {
		return nil, false
	}
	return parseEnv(res.Stdout, marker)
}

func parseEnv(out []byte, marker string) (Env, bool) {
	m := []byte(marker)
	start := bytes.Index(out, m)
	if start < 0 {
		return nil, false
	}
	rest := out[start+len(m):]
	end := bytes.Index(rest, m)
	if end < 0 {
		return nil, false
	}
	var env Env
	for _, kv := range bytes.Split(rest[:end], []byte{0}) {
		if k, _, ok := strings.Cut(string(kv), "="); ok && k != "" {
			env = append(env, string(kv))
		}
	}
	names := slices.Clone(shellOnlyVars)
	for _, kv := range probeVars {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	return env.Without(names...), len(env) > 0
}
