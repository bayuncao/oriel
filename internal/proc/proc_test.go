//go:build unix

package proc

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// tempRoot 是解析過符號連結的暫存目錄，讓路徑在 macOS 上
// 與 EvalSymlinks 的結果相等（/var -> /private/var）。
func tempRoot(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func script(t *testing.T, path, body string) {
	writeFile(t, path, "#!/bin/sh\n"+body+"\n", 0o755)
}

func TestEnvExpand(t *testing.T) {
	env := Env{"HOME=/h", "X=/x", "EMPTY="}
	for _, tc := range []struct{ in, want string }{
		{"~/.grok/bin", "/h/.grok/bin"},
		{"~", "/h"},
		{"$X/bin", "/x/bin"},
		{"${X}/bin", "/x/bin"},
		{"$UNSET", ""},
		{"$UNSET/bin", ""},
		{"$EMPTY/bin", ""},
		{"/abs", "/abs"},
	} {
		if got := env.Expand(tc.in); got != tc.want {
			t.Errorf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := (Env{}).Expand("~/x"); got != "" {
		t.Errorf("Expand without HOME = %q, want empty", got)
	}
}

func TestEnvWithWithout(t *testing.T) {
	env := Env{"A=1", "B=2"}
	got := env.With("A=3", "C=4").Without("B")
	if !slices.Equal(got, Env{"A=3", "C=4"}) {
		t.Fatalf("got %v", got)
	}
	if !slices.Equal(env, Env{"A=1", "B=2"}) {
		t.Fatalf("receiver changed: %v", env)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.2", 1},
		{"v24.1.0", "24.1.0", 0},
		{"2.1", "2.1.0", 0},
		{"0.153.4", "0.153.10", -1},
		{"1.0.0-beta.1", "1.0.0", 0},
	} {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestToolDirs(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "home")
	for _, d := range []string{
		".local/bin", "npm-prefix/bin", ".nvm/versions/node/v24.1.0/bin", ".nvm/versions/node/v25.0.0/bin",
		".local/share/mise/shims", "custom-volta/bin", ".asdf/shims", ".bun/bin", ".cargo/bin",
	} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "opt/homebrew/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".npmrc"), "color=false\nprefix = ~/npm-prefix\n", 0o644)
	writeFile(t, filepath.Join(home, ".nvm/alias/default"), "24\n", 0o644)
	env := Env{"HOME=" + home, "VOLTA_HOME=" + filepath.Join(home, "custom-volta")}

	got := ToolDirs(env, root)
	want := []string{
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, "npm-prefix/bin"),
		filepath.Join(home, ".nvm/versions/node/v24.1.0/bin"), // default alias，而非最新版本
		filepath.Join(home, ".local/share/mise/shims"),
		filepath.Join(home, "custom-volta/bin"),
		filepath.Join(home, ".asdf/shims"),
		filepath.Join(home, ".bun/bin"),
		filepath.Join(home, ".cargo/bin"),
		filepath.Join(root, "opt/homebrew/bin"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ToolDirs:\n got %q\nwant %q", got, want)
	}
}

func TestNvmDefaultAliases(t *testing.T) {
	root := tempRoot(t)
	for _, v := range []string{"v20.1.0", "v22.3.0", "v22.10.0", "v24.0.0"} {
		if err := os.MkdirAll(filepath.Join(root, "versions/node", v, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := func(v string) string { return filepath.Join(root, "versions/node", v, "bin") }
	for _, tc := range []struct{ name, alias, want string }{
		{"major", "22", bin("v22.10.0")},
		{"exact", "v20.1.0", bin("v20.1.0")},
		{"node", "node", bin("v24.0.0")},
		{"via lts alias", "lts/*", bin("v22.3.0")},
		{"no match", "18", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.RemoveAll(filepath.Join(root, "alias"))
			writeFile(t, filepath.Join(root, "alias/default"), tc.alias, 0o644)
			writeFile(t, filepath.Join(root, "alias/lts/*"), "lts/jod", 0o644)
			writeFile(t, filepath.Join(root, "alias/lts/jod"), "v22.3.0", 0o644)
			if got := nvmDefault(root); got != tc.want {
				t.Fatalf("nvmDefault = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLook(t *testing.T) {
	root := tempRoot(t)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	script(t, filepath.Join(a, "tool"), "")
	writeFile(t, filepath.Join(b, "tool"), "not executable", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "c", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Look("tool", []string{a, b, filepath.Join(root, "c"), a})
	if !slices.Equal(got, []string{filepath.Join(a, "tool")}) {
		t.Fatalf("Look = %q", got)
	}
}
