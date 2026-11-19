package proc

import (
	"bytes"
	"cmp"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Unwrapper 找出版本管理器包裝腳本或 shim（mise、asdf）
// 背後的程式。這類包裝器會重新進入管理器，當管理器自己的
// 環境缺失時可能遞迴回到包裝器本身，因此 OrielHub
// 改為執行真正的程式。
type Unwrapper struct {
	Run     func(context.Context, Probe) Result // 執行管理器的 which，通常是 (*Runner).Run
	Root    string                              // 前綴加到系統目錄上；"" 表示 /
	Env     Env                                 // 環境快照
	Dirs    []string                            // 尋找 mise 與 asdf 的位置；nil 表示 Env 的 PATH 與 ToolDirs
	Timeout time.Duration                       // 零值表示 5 秒
}

// maxWrapper 是作為可能的 mise 包裝腳本讀取的最大檔案。
const maxWrapper = 8 << 10

// miseExec 符合如 `exec mise x node@22 -- claude "$@"` 的包裝行。
var miseExec = regexp.MustCompile(`^\s*(?:exec\s+)?(?:\S*/)?(mise|rtx)\s+(?:x|exec)\b(.*?)\s--\s+(\S+)`)

// Unwrap 回傳 path 實際執行的程式；當它不是包裝器、或
// 管理器無法指出程式時回傳 path 本身。
func (u *Unwrapper) Unwrap(ctx context.Context, path, name string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	var argv [][]string
	switch {
	case isMise(real):
		// mise shim 是指向 mise 本身的符號連結。
		argv = [][]string{{real, "which", name}}
	case u.isAsdfShim(path, real):
		if asdf := u.find("asdf"); asdf != "" {
			argv = [][]string{{asdf, "which", name}}
		}
	default:
		if tool, ok := miseWrapper(real, name); ok {
			if mise := u.find("mise"); mise != "" {
				argv = [][]string{{mise, "which", name}}
				if tool != "" {
					argv = append(argv, []string{mise, "which", "--tool", tool, name})
				}
			}
		}
	}
	for _, a := range argv {
		if p := u.which(ctx, a); p != "" && u.acceptable(p, path, real, name) {
			return p
		}
	}
	return path
}

// isMise 回報解析後的路徑是否為 mise 本身（rtx 是它的舊
// 名稱）；mise shim 是指向它的符號連結。
func isMise(real string) bool {
	b := filepath.Base(real)
	return b == "mise" || b == "rtx"
}

func (u *Unwrapper) isAsdfShim(path, real string) bool {
	if filepath.Dir(path) == asdfShims(u.Env) {
		return true
	}
	b, ok := readSmall(real)
	return ok && bytes.Contains(b, []byte("asdf exec"))
}

// miseWrapper 回報 file 是否為 name 的 mise 包裝腳本，
// 以及它在 "--" 之前指名的工具（若有）。
func miseWrapper(file, name string) (tool string, ok bool) {
	b, ok := readSmall(file)
	if !ok || !bytes.HasPrefix(b, []byte("#!")) {
		return "", false
	}
	for line := range strings.Lines(string(b)) {
		m := miseExec.FindStringSubmatch(line)
		if m == nil || strings.Trim(m[3], `"'`) != name {
			continue
		}
		for _, f := range strings.Fields(m[2]) {
			if !strings.HasPrefix(f, "-") {
				return f, true
			}
		}
		return "", true
	}
	return "", false
}

func readSmall(file string) ([]byte, bool) {
	fi, err := os.Stat(file)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxWrapper {
		return nil, false
	}
	b, err := os.ReadFile(file)
	return b, err == nil
}

func (u *Unwrapper) find(name string) string {
	dirs := u.Dirs
	if dirs == nil {
		dirs = SearchDirs(u.Env, u.Root)
	}
	for _, p := range Look(name, dirs) {
		// 在 shim 目錄中找到的 mise 或 asdf 本身就是 shim。
		if !strings.HasSuffix(filepath.Dir(p), "shims") {
			return p
		}
	}
	return ""
}

func (u *Unwrapper) which(ctx context.Context, argv []string) string {
	res := u.Run(ctx, Probe{Path: argv[0], Args: argv[1:], Env: u.Env, Dir: u.Env.WorkDir(), Timeout: cmp.Or(u.Timeout, 5*time.Second)})
	if res.Err != nil {
		return ""
	}
	return strings.TrimSpace(string(res.Stdout))
}

// acceptable 拒絕管理器的回答：不是絕對路徑的可執行檔、
// 又是同一個包裝器、或是另一個包裝器。
func (u *Unwrapper) acceptable(p, path, real, name string) bool {
	if !filepath.IsAbs(p) || strings.ContainsRune(p, '\n') || !Executable(p) {
		return false
	}
	pr, err := filepath.EvalSymlinks(p)
	if err != nil || pr == real || p == path {
		return false
	}
	if isMise(pr) || u.isAsdfShim(p, pr) {
		return false
	}
	_, wrapper := miseWrapper(pr, name)
	return !wrapper
}
