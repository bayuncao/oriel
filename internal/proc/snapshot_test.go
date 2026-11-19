//go:build unix

package proc

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// fakeShell 寫入一個 shell，先執行 body——可能依 $1 中的旗標
// 印出雜訊或卡住——再用 /bin/sh 執行其最後一個參數。
func fakeShell(t *testing.T, path, body string) {
	script(t, path, body+"\nshift\nexec /bin/sh -c \"$1\"")
}

func TestSnapshotReadsBetweenMarkers(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "home")
	sh := filepath.Join(root, "zsh")
	fakeShell(t, sh, `echo "profile noise"; export PATH=/login/bin:$PATH FROM_PROFILE='a b'; export LINES_VAR="x
y"`)
	s := Snapshotter{Runner: &Runner{}, Root: root}
	env, err := s.Snapshot(context.Background(), Env{"HOME=" + home, "SHELL=" + sh, "PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if got := env.Get("FROM_PROFILE"); got != "a b" {
		t.Errorf("FROM_PROFILE = %q", got)
	}
	if got := env.Get("LINES_VAR"); got != "x\ny" {
		t.Errorf("LINES_VAR = %q", got)
	}
	if got := env.PathDirs(); len(got) == 0 || got[0] != "/login/bin" {
		t.Errorf("PATH = %q", got)
	}
	for _, name := range []string{"ORIELHUB_SHELL_PROBE", "DISABLE_AUTO_UPDATE", "ZSH_TMUX_AUTOSTART", "PWD", "SHLVL", "_"} {
		if _, ok := env.Lookup(name); ok {
			t.Errorf("%s leaked into the snapshot", name)
		}
	}
}

func TestSnapshotSetsAntiHangVars(t *testing.T) {
	root := tempRoot(t)
	sh := filepath.Join(root, "zsh")
	// 未設定探測變數就會卡住的設定檔。
	fakeShell(t, sh, `[ "$ORIELHUB_SHELL_PROBE" = 1 ] && [ "$DISABLE_AUTO_UPDATE" = true ] && [ "$ZSH_TMUX_AUTOSTART" = false ] || sleep 60`)
	s := Snapshotter{Runner: &Runner{}, Root: root, Timeout: 5 * time.Second}
	if _, err := s.Snapshot(context.Background(), Env{"HOME=" + root, "SHELL=" + sh}); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotFallsBackToLoginOnly(t *testing.T) {
	root := tempRoot(t)
	sh := filepath.Join(root, "zsh")
	// -ilc 會卡住（.zshrc 中 exec tmux）；-lc 會回應。
	fakeShell(t, sh, `[ "$1" = -ilc ] && sleep 60; export ONLY_LOGIN=1`)
	s := Snapshotter{Runner: &Runner{}, Root: root, Timeout: time.Second}
	env, err := s.Snapshot(context.Background(), Env{"HOME=" + root, "SHELL=" + sh})
	if err != nil {
		t.Fatal(err)
	}
	if env.Get("ONLY_LOGIN") != "1" {
		t.Fatalf("snapshot did not come from -lc: %v", env)
	}
}

func TestSnapshotNoAnswerKeepsInherited(t *testing.T) {
	root := tempRoot(t)
	sh := filepath.Join(root, "zsh")
	script(t, sh, "echo no markers here")
	inherited := Env{"HOME=" + root, "SHELL=" + sh, "PATH=/usr/bin"}
	s := Snapshotter{Runner: &Runner{}, Root: root}
	env, err := s.Snapshot(context.Background(), inherited)
	if err != ErrNoShell || !slices.Equal(env, inherited) {
		t.Fatalf("got %v, %v", env, err)
	}
}

func TestSnapshotMergesFish(t *testing.T) {
	root := tempRoot(t)
	home := filepath.Join(root, "home")
	zsh := filepath.Join(root, "zsh")
	fakeShell(t, zsh, `export PATH=/zsh/bin:/shared/bin SHARED=zsh`)
	fakeShell(t, filepath.Join(root, "opt/homebrew/bin/fish"), `export PATH=/fish/bin:/shared/bin SHARED=fish FISH_ONLY=1`)
	if err := os.MkdirAll(filepath.Join(home, ".config/fish"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := Snapshotter{Runner: &Runner{}, Root: root}
	env, err := s.Snapshot(context.Background(), Env{"HOME=" + home, "SHELL=" + zsh, "PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if got := env.PathDirs(); !slices.Equal(got, []string{"/zsh/bin", "/shared/bin", "/fish/bin"}) {
		t.Errorf("PATH = %q", got)
	}
	if env.Get("SHARED") != "zsh" || env.Get("FISH_ONLY") != "1" {
		t.Errorf("SHARED=%q FISH_ONLY=%q", env.Get("SHARED"), env.Get("FISH_ONLY"))
	}
}

func TestSnapshotSkipsUnconfiguredFish(t *testing.T) {
	root := tempRoot(t)
	zsh := filepath.Join(root, "zsh")
	fakeShell(t, zsh, `export PATH=/zsh/bin`)
	fakeShell(t, filepath.Join(root, "opt/homebrew/bin/fish"), `export PATH=/fish/bin`)
	s := Snapshotter{Runner: &Runner{}, Root: root}
	env, err := s.Snapshot(context.Background(), Env{"HOME=" + root, "SHELL=" + zsh})
	if err != nil {
		t.Fatal(err)
	}
	if got := env.PathDirs(); !slices.Equal(got, []string{"/zsh/bin"}) {
		t.Errorf("PATH = %q", got)
	}
}
