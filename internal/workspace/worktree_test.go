package workspace_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/workspace"
)

// wtFixture 是一個有 main 分支與一個 commit 的倉庫，加上放 worktree 的目錄。
type wtFixture struct {
	e    proc.Env
	repo string // 主目錄
	root string // WorktreeRoot
	base string // main 的 commit
	g    *workspace.Git
}

func newWT(t *testing.T) *wtFixture {
	t.Helper()
	requireGit(t)
	e := isolatedEnv(t)
	repo := realDir(t, repoWithCommit(t, e))
	return &wtFixture{e: e, repo: repo, root: filepath.Join(realDir(t, t.TempDir()), "worktrees"),
		base: commitOf(t, e, repo), g: newGit(t, e, nil)}
}

// wt 是 Task 名為 name 的 worktree，從 main 的 base 切出。
func (f *wtFixture) wt(name string) task.Worktree {
	return task.Worktree{Path: filepath.Join(f.root, name), Branch: "orielhub/" + name,
		BaseBranch: "main", BaseCommit: f.base}
}

func (f *wtFixture) create(t *testing.T, w task.Worktree) {
	t.Helper()
	if err := f.g.Create(context.Background(), task.CreateWorktree{Repo: f.repo, Worktree: w}); err != nil {
		t.Fatal(err)
	}
}

func (f *wtFixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitRun(t, f.e, dir, args...))
}

// commit 在 dir 寫入 name 並提交。
func (f *wtFixture) commit(t *testing.T, dir, name, content string) string {
	t.Helper()
	write(t, dir, name, content)
	f.git(t, dir, "add", name)
	f.git(t, dir, "commit", "-q", "-m", "edit "+name)
	return commitOf(t, f.e, dir)
}

func (f *wtFixture) squash(m task.MergeWorktree) task.MergeWorktree {
	m.Into, m.Strategy = f.repo, task.MergeSquash
	return m
}

func (f *wtFixture) branchGone(t *testing.T, branch string) bool {
	t.Helper()
	return !strings.Contains(f.git(t, f.repo, "branch", "--list", branch), branch)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wantCode 確認 err 是帶著 params 的 workspace 錯誤 code。
func wantCode(t *testing.T, err error, code workspace.Code, params map[string]string) {
	t.Helper()
	var werr *workspace.Error
	if !errors.As(err, &werr) || werr.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
	if params != nil && !maps.Equal(werr.Params, params) {
		t.Fatalf("params = %v, want %v", werr.Params, params)
	}
}

func TestCreate(t *testing.T) {
	ctx := context.Background()
	t.Run("W1 new worktree from the base commit", func(t *testing.T) {
		f := newWT(t)
		f.commit(t, f.repo, "later.txt", "later") // main 已往前走：worktree 仍從 BaseCommit 切出
		w := f.wt("t1")
		f.create(t, w)
		if got := commitOf(t, f.e, w.Path); got != f.base {
			t.Fatalf("HEAD = %s, want %s", got, f.base)
		}
		if got := f.git(t, w.Path, "symbolic-ref", "--short", "HEAD"); got != w.Branch {
			t.Fatalf("branch = %s, want %s", got, w.Branch)
		}
	})
	t.Run("W2 already created", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		f.create(t, w)
	})
	t.Run("W3 branch left after the directory went away", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		c := f.commit(t, w.Path, "b.txt", "b")
		if err := os.RemoveAll(w.Path); err != nil {
			t.Fatal(err)
		}
		f.create(t, w)
		if got := commitOf(t, f.e, w.Path); got != c {
			t.Fatalf("HEAD = %s, want the branch's commit %s", got, c)
		}
	})
	t.Run("W4 path taken by something else", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		if err := os.MkdirAll(w.Path, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, w.Path, "mine.txt", "keep")
		err := f.g.Create(ctx, task.CreateWorktree{Repo: f.repo, Worktree: w})
		wantCode(t, err, workspace.CodePathTaken, map[string]string{"path": w.Path})
		if read(t, w.Path, "mine.txt") != "keep" {
			t.Fatal("the directory was touched")
		}
	})
	t.Run("W4 path is another branch's worktree", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		other := w
		other.Branch = "orielhub/other"
		err := f.g.Create(ctx, task.CreateWorktree{Repo: f.repo, Worktree: other})
		wantCode(t, err, workspace.CodePathTaken, map[string]string{"path": w.Path})
	})
	t.Run("W6 worktree root is not a directory", func(t *testing.T) {
		f := newWT(t)
		if err := os.MkdirAll(filepath.Dir(f.root), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Dir(f.root), filepath.Base(f.root), "")
		err := f.g.Create(ctx, task.CreateWorktree{Repo: f.repo, Worktree: f.wt("t1")})
		wantCode(t, err, workspace.CodeDirMissing, map[string]string{"path": f.root})
	})
	t.Run("W5 fork from the task worktree", func(t *testing.T) {
		f := newWT(t)
		tw := f.wt("t1")
		f.create(t, tw)
		c := f.commit(t, tw.Path, "b.txt", "b")
		fork := task.Worktree{Path: filepath.Join(f.root, "t1-s2"), Branch: "orielhub/t1-s2",
			BaseBranch: tw.Branch, BaseCommit: c}
		if err := f.g.Create(ctx, task.CreateWorktree{Repo: tw.Path, Worktree: fork}); err != nil {
			t.Fatal(err)
		}
		if got := commitOf(t, f.e, fork.Path); got != c {
			t.Fatalf("HEAD = %s, want %s", got, c)
		}
	})
}

func TestDiff(t *testing.T) {
	ctx := context.Background()
	f := newWT(t)
	w := f.wt("t1")
	f.create(t, w)
	t.Run("D4 no changes", func(t *testing.T) {
		d, err := f.g.Diff(ctx, w)
		if err != nil || d != "" {
			t.Fatalf("Diff = %q, %v; want empty", d, err)
		}
	})
	f.commit(t, w.Path, ".gitignore", "*.log\n")
	f.commit(t, w.Path, "committed.txt", "c\n")
	write(t, w.Path, "a.txt", "changed\n")  // 未提交
	write(t, w.Path, "new.txt", "n\n")      // 未追蹤
	write(t, w.Path, "skip.log", "ignored") // 被忽略
	f.git(t, w.Path, "add", "new.txt")
	f.git(t, w.Path, "reset", "-q", "new.txt") // index 記過又撤掉：仍是未追蹤
	status := f.git(t, w.Path, "status", "--porcelain")
	staged := f.git(t, w.Path, "diff", "--cached", "--name-only")
	d, err := f.g.Diff(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("D1 committed, uncommitted and untracked", func(t *testing.T) {
		for _, want := range []string{"b/committed.txt", "+changed", "b/new.txt"} {
			if !strings.Contains(d, want) {
				t.Errorf("diff lacks %q:\n%s", want, d)
			}
		}
	})
	t.Run("D2 worktree untouched", func(t *testing.T) {
		if got := f.git(t, w.Path, "status", "--porcelain"); got != status {
			t.Errorf("status = %q, want %q", got, status)
		}
		if got := f.git(t, w.Path, "diff", "--cached", "--name-only"); got != staged {
			t.Errorf("staged = %q, want %q", got, staged)
		}
	})
	t.Run("D3 ignored files left out", func(t *testing.T) {
		if strings.Contains(d, "skip.log") {
			t.Errorf("diff has the ignored file:\n%s", d)
		}
	})
	t.Run("D6 big files as binary", func(t *testing.T) {
		big := f.wt("big")
		f.create(t, big)
		write(t, big.Path, "big.txt", strings.Repeat(strings.Repeat("a", 99)+"\n", 20000)) // 2 MB
		d, err := f.g.Diff(ctx, big)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(d, "Binary files /dev/null and b/big.txt differ") || len(d) > 1000 {
			t.Fatalf("diff of a big file (%d bytes):\n%.300s", len(d), d)
		}
	})
	t.Run("D5 worktree not there", func(t *testing.T) {
		missing := f.wt("absent")
		_, err := f.g.Diff(ctx, missing)
		wantCode(t, err, workspace.CodeDirMissing, map[string]string{"path": missing.Path})
	})
}

func TestMerge(t *testing.T) {
	ctx := context.Background()
	t.Run("M1 squash into the checked-out base branch", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		f.commit(t, w.Path, "b.txt", "b")
		write(t, w.Path, "c.txt", "c") // 未提交：先自動提交
		conflicts, url, err := f.g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "Title\n\nBody"}))
		if err != nil || conflicts != nil || url != "" {
			t.Fatalf("Merge = %v, %q, %v", conflicts, url, err)
		}
		if got := f.git(t, f.repo, "rev-parse", "HEAD^"); got != f.base {
			t.Fatalf("squash commit's parent = %s, want %s", got, f.base)
		}
		if got := f.git(t, f.repo, "log", "-1", "--format=%B"); got != "Title\n\nBody" {
			t.Fatalf("message = %q", got)
		}
		if read(t, f.repo, "b.txt") != "b" || read(t, f.repo, "c.txt") != "c" {
			t.Fatal("the merged files are not in the base directory")
		}
		if f.git(t, f.repo, "status", "--porcelain") != "" {
			t.Fatal("the base directory is not clean")
		}
		if f.g.Exists(w.Path) || !f.branchGone(t, w.Branch) {
			t.Fatal("the worktree or its branch is still there")
		}
	})
	t.Run("M2' base branch not checked out", func(t *testing.T) {
		f := newWT(t)
		f.git(t, f.repo, "switch", "-q", "-c", "dev")
		w := f.wt("t1")
		f.create(t, w)
		f.commit(t, w.Path, "b.txt", "b")
		if _, _, err := f.g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"})); err != nil {
			t.Fatal(err)
		}
		if got := f.git(t, f.repo, "rev-parse", "main^"); got != f.base {
			t.Fatalf("main^ = %s, want %s", got, f.base)
		}
		if got := f.git(t, f.repo, "symbolic-ref", "--short", "HEAD"); got != "dev" {
			t.Fatalf("checked out %s, want dev", got)
		}
		if _, err := os.Stat(filepath.Join(f.repo, "b.txt")); err == nil {
			t.Fatal("dev's directory got main's file")
		}
	})
	t.Run("M3' unrelated uncommitted change kept", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		f.commit(t, w.Path, "b.txt", "b")
		write(t, f.repo, "a.txt", "mine\n")
		if _, _, err := f.g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"})); err != nil {
			t.Fatal(err)
		}
		if read(t, f.repo, "a.txt") != "mine\n" || read(t, f.repo, "b.txt") != "b" {
			t.Fatal("the change was lost or the merge is missing")
		}
	})
	t.Run("M3'' overlapping uncommitted change", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		f.commit(t, w.Path, "a.txt", "theirs\n")
		write(t, f.repo, "a.txt", "mine\n")
		_, _, err := f.g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"}))
		wantCode(t, err, workspace.CodeDirtyTarget, map[string]string{"path": f.repo, "files": "a.txt"})
		if got := commitOf(t, f.e, f.repo); got != f.base {
			t.Fatalf("main moved to %s", got)
		}
		if read(t, f.repo, "a.txt") != "mine\n" || !f.g.Exists(w.Path) {
			t.Fatal("the change or the worktree was lost")
		}
	})
	t.Run("M4 conflict changes nothing", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		wtHead := f.commit(t, w.Path, "a.txt", "theirs\n")
		write(t, w.Path, "draft.txt", "d") // 未提交：衝突時也不替它提交
		head := f.commit(t, f.repo, "a.txt", "ours\n")
		conflicts, _, err := f.g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"}))
		if err != nil || !slices.Equal(conflicts, []string{"a.txt"}) {
			t.Fatalf("Merge = %v, %v; want the conflict on a.txt", conflicts, err)
		}
		if commitOf(t, f.e, f.repo) != head || f.git(t, f.repo, "status", "--porcelain") != "" {
			t.Fatal("the base directory changed")
		}
		if !f.g.Exists(w.Path) || f.branchGone(t, w.Branch) {
			t.Fatal("the worktree or its branch went away")
		}
		if commitOf(t, f.e, w.Path) != wtHead || f.git(t, w.Path, "status", "--porcelain") != "?? draft.txt" {
			t.Fatal("the worktree's uncommitted change was committed or lost")
		}
	})
	t.Run("M5 nothing to merge", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		if _, _, err := f.g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"})); err != nil {
			t.Fatal(err)
		}
		if commitOf(t, f.e, f.repo) != f.base || f.g.Exists(w.Path) || !f.branchGone(t, w.Branch) {
			t.Fatal("want main unchanged and the worktree removed")
		}
	})
	t.Run("M6 already squashed before a crash", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		f.commit(t, w.Path, "b.txt", "b")
		f.git(t, f.repo, "merge", "-q", "--squash", w.Branch)
		f.git(t, f.repo, "commit", "-q", "-m", "m")
		head := commitOf(t, f.e, f.repo)
		if _, _, err := f.g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"})); err != nil {
			t.Fatal(err)
		}
		if commitOf(t, f.e, f.repo) != head || f.g.Exists(w.Path) {
			t.Fatal("want no second commit and the worktree removed")
		}
	})
	t.Run("M7 worktree and branch already gone", func(t *testing.T) {
		f := newWT(t)
		conflicts, _, err := f.g.Merge(ctx, f.squash(task.MergeWorktree{From: f.wt("t1"), Message: "m"}))
		if err != nil || conflicts != nil || commitOf(t, f.e, f.repo) != f.base {
			t.Fatalf("Merge = %v, %v", conflicts, err)
		}
	})
	t.Run("M8 no git identity", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		g := newGit(t, f.e.Without("GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"), nil)
		_, _, err := g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"}))
		wantCode(t, err, workspace.CodeNoIdentity, map[string]string{"path": f.repo})
	})
	t.Run("M9 base branch moved meanwhile", func(t *testing.T) {
		f := newWT(t)
		f.git(t, f.repo, "switch", "-q", "-c", "dev")
		f.git(t, f.repo, "switch", "-q", "main")
		other := f.commit(t, f.repo, "other.txt", "o") // 另一個 Task 剛合進去的
		f.git(t, f.repo, "reset", "-q", "--hard", f.base)
		f.git(t, f.repo, "switch", "-q", "dev") // main 不檢出：以 update-ref 推進
		w := f.wt("t1")
		f.create(t, w)
		f.commit(t, w.Path, "b.txt", "b")
		// 第一次算出 squash commit 時把 main 推到 other：update-ref 的舊值不符，不覆蓋，以新的 main 重算。
		bin, mark := t.TempDir(), filepath.Join(t.TempDir(), "moved")
		fakeGit(t, bin, `case " $* " in *" commit-tree "*" -p "*) [ -f '`+mark+`' ] || { : > '`+mark+`'; '`+gitBin+`' -C '`+f.repo+`' update-ref refs/heads/main `+other+`; };; esac
exec '`+gitBin+`' "$@"`)
		g := newGit(t, f.e, func(o *workspace.Options) { o.Git = filepath.Join(bin, "git") })
		if _, _, err := g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"})); err != nil {
			t.Fatal(err)
		}
		if got := f.git(t, f.repo, "rev-parse", "main^"); got != other {
			t.Fatalf("main^ = %s, want the squash on top of %s", got, other)
		}
		if f.git(t, f.repo, "show", "main:b.txt") != "b" || f.git(t, f.repo, "show", "main:other.txt") != "o" {
			t.Fatal("a change was lost")
		}
	})
	t.Run("M10 base branch keeps moving", func(t *testing.T) {
		f := newWT(t)
		f.git(t, f.repo, "switch", "-q", "-c", "dev")
		w := f.wt("t1")
		f.create(t, w)
		f.commit(t, w.Path, "b.txt", "b")
		// 每次算出 squash commit 時都有人往 main 上加一個 commit。
		bin := t.TempDir()
		fakeGit(t, bin, `case " $* " in *" commit-tree "*" -p "*) r='`+f.repo+`'; g='`+gitBin+`'
c=$("$g" -C "$r" commit-tree "$("$g" -C "$r" rev-parse 'main^{tree}')" -p main -m bump) && "$g" -C "$r" update-ref refs/heads/main "$c";; esac
exec '`+gitBin+`' "$@"`)
		g := newGit(t, f.e, func(o *workspace.Options) { o.Git = filepath.Join(bin, "git") })
		_, _, err := g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"}))
		wantCode(t, err, workspace.CodeBranchMoved, map[string]string{"branch": "main"})
		if !f.g.Exists(w.Path) {
			t.Fatal("the worktree went away")
		}
	})
	t.Run("G5 git too old", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		bin := t.TempDir()
		fakeGit(t, bin, `[ "$1" = version ] && { echo "git version 2.30.1"; exit 0; }
exec '`+gitBin+`' "$@"`)
		g := newGit(t, f.e, func(o *workspace.Options) { o.Git = filepath.Join(bin, "git") })
		_, _, err := g.Merge(ctx, f.squash(task.MergeWorktree{From: w, Message: "m"}))
		wantCode(t, err, workspace.CodeGitTooOld, map[string]string{"version": "2.30.1", "want": "2.38"})
	})
}

// prFixture 加上 origin（本機的 bare 倉庫）與一個假 gh：它記下參數，
// pr list 印出 existing 檔案的內容，pr create 印出新 PR 的地址。
type prFixture struct {
	*wtFixture
	bin, log, existing string
}

const newPR = "https://github.com/o/r/pull/7"

func newPRFixture(t *testing.T, gh string) *prFixture {
	t.Helper()
	f := &prFixture{wtFixture: newWT(t), bin: t.TempDir()}
	f.log, f.existing = filepath.Join(f.bin, "log"), filepath.Join(f.bin, "existing")
	origin := t.TempDir()
	f.git(t, origin, "init", "-q", "--bare")
	f.git(t, f.repo, "remote", "add", "origin", origin)
	f.git(t, f.repo, "push", "-q", "origin", "main")
	if gh == "" {
		gh = `echo "$@" >> '` + f.log + `'
case "$1 $2" in
"--version ") exit 0;;
"pr list") [ -f '` + f.existing + `' ] && read -r u < '` + f.existing + `' && echo "$u"; exit 0;;
"pr create") echo "Creating pull request"; echo ` + newPR + `; exit 0;;
esac
exit 1`
	}
	path := filepath.Join(f.bin, "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+gh+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.g = f.ghGit(t, f.bin)
	return f
}

// ghGit 只在 dir 與系統目錄裡找 gh（Root 讓使用者工具目錄落空，找不到本機裝的 gh）。
func (f *prFixture) ghGit(t *testing.T, dir string) *workspace.Git {
	e := f.e.Without("PATH").With("PATH=" + dir + ":/usr/bin:/bin")
	return newGit(t, e, func(o *workspace.Options) { o.Git, o.Root = gitBin, t.TempDir() })
}

func (f *prFixture) pr(w task.Worktree) task.MergeWorktree {
	return task.MergeWorktree{From: w, Into: f.repo, Strategy: task.MergePullRequest, Message: "Title\n\nBody"}
}

func (f *prFixture) logged(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(f.log)
	return string(b)
}

func TestMergePullRequest(t *testing.T) {
	ctx := context.Background()
	t.Run("P1 push and open a PR", func(t *testing.T) {
		f := newPRFixture(t, "")
		w := f.wt("t1")
		f.create(t, w)
		c := f.commit(t, w.Path, "b.txt", "b")
		_, url, err := f.g.Merge(ctx, f.pr(w))
		if err != nil || url != newPR {
			t.Fatalf("Merge = %q, %v", url, err)
		}
		origin := f.git(t, f.repo, "remote", "get-url", "origin")
		if got := f.git(t, origin, "rev-parse", "refs/heads/"+w.Branch); got != c {
			t.Fatalf("origin has %s, want %s", got, c)
		}
		if want := "pr create --base main --head orielhub/t1 --title Title --body Body"; !strings.Contains(f.logged(t), want) {
			t.Fatalf("gh calls:\n%s\nwant %q", f.logged(t), want)
		}
		if f.g.Exists(w.Path) || !f.branchGone(t, w.Branch) {
			t.Fatal("the local worktree or branch is still there")
		}
	})
	t.Run("P2 PR already open", func(t *testing.T) {
		f := newPRFixture(t, "")
		w := f.wt("t1")
		f.create(t, w)
		f.commit(t, w.Path, "b.txt", "b")
		write(t, f.bin, "existing", "https://github.com/o/r/pull/3\n")
		_, url, err := f.g.Merge(ctx, f.pr(w))
		if err != nil || url != "https://github.com/o/r/pull/3" {
			t.Fatalf("Merge = %q, %v", url, err)
		}
		if strings.Contains(f.logged(t), "pr create") {
			t.Fatal("opened a second PR")
		}
	})
	t.Run("P3 no gh", func(t *testing.T) {
		f := newPRFixture(t, "")
		w := f.wt("t1")
		f.create(t, w)
		_, _, err := f.ghGit(t, t.TempDir()).Merge(ctx, f.pr(w))
		wantCode(t, err, workspace.CodeGhMissing, nil)
	})
	t.Run("P3 gh that cannot run", func(t *testing.T) {
		f := newPRFixture(t, `exit 1`)
		w := f.wt("t1")
		f.create(t, w)
		_, _, err := f.g.Merge(ctx, f.pr(w))
		wantCode(t, err, workspace.CodeGhMissing, nil)
	})
	t.Run("P6 base branch not on origin", func(t *testing.T) {
		f := newPRFixture(t, "")
		tw := f.wt("t1")
		f.create(t, tw)
		c := f.commit(t, tw.Path, "b.txt", "b")
		fork := task.Worktree{Path: filepath.Join(f.root, "t1-s2"), Branch: "orielhub/t1-s2",
			BaseBranch: tw.Branch, BaseCommit: c}
		if err := f.g.Create(ctx, task.CreateWorktree{Repo: tw.Path, Worktree: fork}); err != nil {
			t.Fatal(err)
		}
		m := f.pr(fork)
		m.Into = tw.Path
		_, _, err := f.g.Merge(ctx, m)
		wantCode(t, err, workspace.CodeNoBaseOnRemote, map[string]string{"branch": tw.Branch})
		origin := f.git(t, f.repo, "remote", "get-url", "origin")
		if out := f.git(t, origin, "branch", "--list", fork.Branch); out != "" {
			t.Fatalf("pushed %s anyway", out)
		}
	})
	t.Run("P4 no origin", func(t *testing.T) {
		f := newPRFixture(t, "")
		f.git(t, f.repo, "remote", "remove", "origin")
		w := f.wt("t1")
		f.create(t, w)
		_, _, err := f.g.Merge(ctx, f.pr(w))
		wantCode(t, err, workspace.CodeNoRemote, map[string]string{"path": f.repo})
	})
	t.Run("P5 push fails", func(t *testing.T) {
		f := newPRFixture(t, "")
		f.git(t, f.repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))
		w := f.wt("t1")
		f.create(t, w)
		_, _, err := f.g.Merge(ctx, f.pr(w))
		wantCode(t, err, workspace.CodeGitFailed, nil)
		if !f.g.Exists(w.Path) {
			t.Fatal("the worktree went away")
		}
	})
	t.Run("P5 gh fails", func(t *testing.T) {
		f := newPRFixture(t, `[ "$1" = --version ] && exit 0; echo "not logged in" >&2; exit 1`)
		w := f.wt("t1")
		f.create(t, w)
		_, _, err := f.g.Merge(ctx, f.pr(w))
		wantCode(t, err, workspace.CodeGhFailed, map[string]string{"detail": "not logged in"})
	})
}

func TestDiscard(t *testing.T) {
	ctx := context.Background()
	t.Run("X1 uncommitted changes dropped", func(t *testing.T) {
		f := newWT(t)
		w := f.wt("t1")
		f.create(t, w)
		f.commit(t, w.Path, "b.txt", "b")
		write(t, w.Path, "c.txt", "c")
		if err := f.g.Discard(ctx, task.DiscardWorktree{Worktree: w, Repo: f.repo}); err != nil {
			t.Fatal(err)
		}
		if f.g.Exists(w.Path) || !f.branchGone(t, w.Branch) || commitOf(t, f.e, f.repo) != f.base {
			t.Fatal("want the worktree and branch removed and main unchanged")
		}
	})
	t.Run("X2 already gone", func(t *testing.T) {
		f := newWT(t)
		if err := f.g.Discard(ctx, task.DiscardWorktree{Worktree: f.wt("t1"), Repo: f.repo}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWorktreeOpsCommon(t *testing.T) {
	f := newWT(t)
	w := f.wt("t1")
	t.Run("G1 no git", func(t *testing.T) {
		g := newGit(t, f.e, func(o *workspace.Options) { o.Git = filepath.Join(t.TempDir(), "git") })
		err := g.Create(context.Background(), task.CreateWorktree{Repo: f.repo, Worktree: w})
		wantCode(t, err, workspace.CodeGitMissing, nil)
	})
	t.Run("G2 cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.g.Create(ctx, task.CreateWorktree{Repo: f.repo, Worktree: w}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}
