//go:build unix

package proc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUnwrap(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "home")
	real := filepath.Join(home, ".local/share/mise/installs/node/22/bin/claude")
	script(t, real, "echo real")
	bin := filepath.Join(root, "bin")

	// mise 對 claude 印出真正的程式，對其他命令
	// 印出無用的東西。
	mise := filepath.Join(bin, "mise")
	script(t, mise, `case "$*" in
  "which claude") echo `+real+`;;
  "which --tool node@22 tool") echo `+real+`;;
  "which loop") echo `+filepath.Join(home, ".local/share/mise/shims/loop")+`;;
  *) exit 1;;
esac`)
	asdf := filepath.Join(bin, "asdf")
	script(t, asdf, `[ "$*" = "which claude" ] && echo `+real)

	shims := filepath.Join(home, ".local/share/mise/shims")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mise, filepath.Join(shims, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mise, filepath.Join(shims, "loop")); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "wrap/claude")
	script(t, wrapper, `exec mise x -- claude "$@"`)
	toolWrapper := filepath.Join(root, "wrap/tool")
	script(t, toolWrapper, `exec mise x node@22 -- tool "$@"`)
	asdfShim := filepath.Join(home, ".asdf/shims/claude")
	script(t, asdfShim, `exec asdf exec "claude" "$@"`)
	plain := filepath.Join(root, "plain/claude")
	script(t, plain, `echo plain`)
	other := filepath.Join(root, "wrap/codex")
	script(t, other, `exec mise x -- claude "$@"`) // 包裝的是另一個命令

	u := Unwrapper{Run: (&Runner{}).Run, Root: root, Env: Env{"HOME=" + home}, Dirs: []string{bin}}
	for _, tc := range []struct {
		name, path, cmd, want string
	}{
		{"mise wrapper script", wrapper, "claude", real},
		{"mise wrapper naming a tool", toolWrapper, "tool", real},
		{"mise shim", filepath.Join(shims, "claude"), "claude", real},
		{"asdf shim", asdfShim, "claude", real},
		{"plain program", plain, "claude", plain},
		{"wrapper for another command", other, "codex", other},
		{"answer is another shim", filepath.Join(shims, "loop"), "loop", filepath.Join(shims, "loop")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := u.Unwrap(context.Background(), tc.path, tc.cmd); got != tc.want {
				t.Fatalf("Unwrap = %q, want %q", got, tc.want)
			}
		})
	}
}
