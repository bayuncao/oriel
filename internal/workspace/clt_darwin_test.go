//go:build darwin

package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/workspace"
)

// fakeUsrBin 在 Root 下建假的 /usr/bin/git 與 /usr/bin/xcode-select。
func fakeUsrBin(t *testing.T, xcodeScript string) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "/usr/bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"git":          "#!/bin/sh\nprintf 'true\n'\nexit 0\n",
		"xcode-select": "#!/bin/sh\n" + xcodeScript + "\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestUsrBinGitNeedsCLT(t *testing.T) {
	ctx := context.Background()
	e := isolatedEnv(t)
	dir := t.TempDir()
	// P3：目錄得是倉庫（有 .git），沒有 git 才會報 git_missing。
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// xcode-select 失敗 → git 不可用。
	root := fakeUsrBin(t, "exit 2")
	g := newGit(t, e, func(o *workspace.Options) {
		o.Root = root
		o.Git = filepath.Join(root, "/usr/bin/git")
	})
	if _, err := g.Inspect(ctx, dir); !errors.Is(err, missingGit()) {
		t.Fatalf("got %v, want git_missing", err)
	}

	// xcode-select 成功 → git 可用（假 git 一律印 true，Inspect 往下走）。
	root = fakeUsrBin(t, "exit 0")
	g = newGit(t, e, func(o *workspace.Options) {
		o.Root = root
		o.Git = filepath.Join(root, "/usr/bin/git")
	})
	if _, err := g.Inspect(ctx, dir); errors.Is(err, missingGit()) {
		t.Fatalf("got %v, want no git_missing", err)
	}
}

// W3：/usr/bin/git 因沒裝 CLT 不可用時跳過它、用後面的候選（D45）。
func TestSkipsUnusableUsrBinGit(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	dir := repoWithCommit(t, isolatedEnv(t))

	root := fakeUsrBin(t, "exit 2")
	hb := filepath.Join(root, "/opt/homebrew/bin")
	if err := os.MkdirAll(hb, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "log")
	gitWrapper(t, hb, log)
	env := proc.Env{"HOME=" + t.TempDir(),
		"PATH=" + filepath.Join(root, "/usr/bin")}
	g := newGitF(t, staticEnv(env), func(o *workspace.Options) {
		o.Root = root
	})
	repo, err := g.Inspect(ctx, dir)
	if err != nil || !repo.IsGit {
		t.Fatalf("got %+v, %v", repo, err)
	}
	if b, err := os.ReadFile(log); err != nil || !strings.Contains(string(b), "ran") {
		t.Fatalf("homebrew candidate not used: %v", err)
	}
}
