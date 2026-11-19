package proc

import (
	"bufio"
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ToolDirs 是存在的使用者工具目錄，依查找順序排列：
// npm、node 與各語言版本管理器、Homebrew 等放置命令的位置，
// 從 Finder 啟動的 app 的 PATH 上沒有它們。家目錄與
// 覆寫變數（NVM_DIR、MISE_DATA_DIR……）來自 env；root
// 會前綴到系統目錄上（"" 表示 /），讓測試可以替換
// 兩者。
func ToolDirs(env Env, root string) []string {
	home := env.Home()
	if home == "" {
		return nil
	}
	dirs := []string{filepath.Join(home, ".local", "bin")}
	if p := npmPrefix(env); p != "" {
		dirs = append(dirs, filepath.Join(p, "bin"))
	}
	dirs = append(dirs,
		nvmDefault(userDir(env, "NVM_DIR", ".nvm")),
		filepath.Join(userDir(env, "FNM_DIR", fnmDir), "aliases", "default", "bin"),
		miseShims(env),
		filepath.Join(userDir(env, "VOLTA_HOME", ".volta"), "bin"),
		asdfShims(env),
		userDir(env, "PNPM_HOME", pnpmDir),
		filepath.Join(userDir(env, "BUN_INSTALL", ".bun"), "bin"),
		filepath.Join(userDir(env, "CARGO_HOME", ".cargo"), "bin"),
		filepath.Join(userDir(env, "DENO_INSTALL", ".deno"), "bin"),
		filepath.Join(root, "/opt/homebrew/bin"),
		filepath.Join(root, "/usr/local/bin"),
	)
	return slices.DeleteFunc(dirs, func(d string) bool { return d == "" || !isDir(d) })
}

// SearchDirs 是 env 的 PATH 加上使用者工具目錄：
// OrielHub 以名稱查找命令的位置。
func SearchDirs(env Env, root string) []string {
	return append(env.PathDirs(), ToolDirs(env, root)...)
}

// userDir 是 env 中變數 v 指名的目錄，或家目錄下的 def。
func userDir(env Env, v, def string) string {
	return cmp.Or(env.Get(v), filepath.Join(env.Home(), def))
}

func miseShims(env Env) string {
	return filepath.Join(userDir(env, "MISE_DATA_DIR", ".local/share/mise"), "shims")
}
func asdfShims(env Env) string { return filepath.Join(userDir(env, "ASDF_DATA_DIR", ".asdf"), "shims") }

// npmPrefix 是 ~/.npmrc 中設定的 prefix，~ 與 ${VAR} 已展開。
func npmPrefix(env Env) string {
	f, err := os.Open(filepath.Join(env.Home(), ".npmrc"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && strings.TrimSpace(k) == "prefix" {
			return env.Expand(strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	return ""
}

// nvmDefault 是 nvm 的 default alias 的 bin 目錄，而非最新
// 安裝版本的：alias/default 保存版本前綴（"24"、"v24.1.0"）
// 或另一個 alias（"node"、"lts/*"、"lts/jod"）。
func nvmDefault(nvmDir string) string {
	alias := "default"
	for range 5 {
		b, err := os.ReadFile(filepath.Join(nvmDir, "alias", alias))
		if err != nil {
			break
		}
		alias = strings.TrimSpace(string(b))
	}
	versions, _ := filepath.Glob(filepath.Join(nvmDir, "versions", "node", "v*"))
	want := strings.TrimPrefix(alias, "v")
	if alias == "node" || alias == "stable" || strings.HasPrefix(alias, "lts/") {
		want = "" // 無法解析的 alias：取最新版本
	}
	var best string
	for _, v := range versions {
		name := strings.TrimPrefix(filepath.Base(v), "v")
		if want != "" && name != want && !strings.HasPrefix(name, want+".") {
			continue
		}
		if best == "" || CompareVersions(name, best) > 0 {
			best = name
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(nvmDir, "versions", "node", "v"+best, "bin")
}

// Look 依序回傳 dirs 中每個名為 name 的可執行檔，
// 不含重複項。
func Look(name string, dirs []string) []string {
	var out []string
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if Executable(p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// Executable 回報 path 是否為（解析符號連結後的）可執行
// 一般檔案。
func Executable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return executableMode(fi.Mode())
}

// CompareVersions 以數值比較點分版本（"1.10.0" >
// "1.9.2"）；缺失或非數值的部分視為 0，"-" 或 "+"
// 之後的內容忽略。
func CompareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if c := cmp.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, s := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}
