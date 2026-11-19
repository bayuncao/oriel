package proc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Env 是以 KEY=VALUE 項目表示的環境，明確傳給每個
// 子行程；os.Environ 絕不改變。方法皆回傳副本。
type Env []string

// Inherited 是 OrielHub 自身啟動時的環境。
func Inherited() Env { return Env(os.Environ()) }

// Get 回傳 name 的值，未設定時回傳 ""。
func (e Env) Get(name string) string {
	v, _ := e.Lookup(name)
	return v
}

func (e Env) Lookup(name string) (string, bool) {
	for i := len(e) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(e[i], "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// With 回傳設定 kv 中每個 KEY=VALUE 的副本，取代較早的
// 值。
func (e Env) With(kv ...string) Env {
	out := slices.Clone(e)
	for _, entry := range kv {
		k, _, _ := strings.Cut(entry, "=")
		out = slices.DeleteFunc(out, func(s string) bool { return strings.HasPrefix(s, k+"=") })
		out = append(out, entry)
	}
	return out
}

// Without 回傳移除指定變數後的副本。
func (e Env) Without(names ...string) Env {
	return slices.DeleteFunc(slices.Clone(e), func(s string) bool {
		k, _, _ := strings.Cut(s, "=")
		return slices.Contains(names, k)
	})
}

// Home 是依據 e 的使用者家目錄。
func (e Env) Home() string { return e.Get("HOME") }

// WorkDir 是探測執行的位置：家目錄，或不存在時為 ""
// （OrielHub 自己的工作目錄）。
func (e Env) WorkDir() string {
	if h := e.Home(); h != "" && isDir(h) {
		return h
	}
	return ""
}

// PathDirs 是 e 的 PATH 項目，依序排列，不含空值。
func (e Env) PathDirs() []string {
	return slices.DeleteFunc(filepath.SplitList(e.Get("PATH")), func(s string) bool { return s == "" })
}

// Expand 將開頭的 ~ 替換為家目錄，將 $VAR 或 ${VAR}
// 替換為它們在 e 中的值。任一變數未設定或為空時回傳 ""，
// 因此 "$X/bin" 絕不會變成 "/bin"。
func (e Env) Expand(s string) string {
	missing := false
	get := func(name string) string {
		v := e.Get(name)
		missing = missing || v == ""
		return v
	}
	if s == "~" || strings.HasPrefix(s, "~/") {
		s = get("HOME") + s[1:]
	}
	if s = os.Expand(s, get); missing {
		return ""
	}
	return s
}
