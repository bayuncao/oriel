package workspace_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/workspace"
)

var _ hub.Git = (*workspace.Git)(nil)

var gitBin string

func requireGit(t *testing.T) {
	t.Helper()
	if gitBin != "" {
		return
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed; skipping workspace tests")
	}
	gitBin = git
}

// isolatedEnv 是測試用 git 環境：不讀使用者的 HOME 與全域 git config，
// PATH 取自測試行程好讓 git 能解析。
func isolatedEnv(t *testing.T) proc.Env {
	t.Helper()
	return proc.Env{
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		"PATH=" + os.Getenv("PATH"),
	}
}

// realDir 是 EvalSymlinks 後的路徑（t.TempDir() 的 /var 是連結）。
func realDir(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func gitRun(t *testing.T, e proc.Env, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	cmd.Env = e
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// gitInit 建一個 main 分支的空倉庫；舊 git 沒有 -b 時改用 symbolic-ref。
func gitInit(t *testing.T, e proc.Env, dir string) {
	t.Helper()
	cmd := exec.Command(gitBin, "init", "-q", "-b", "main")
	cmd.Dir = dir
	cmd.Env = e
	if err := cmd.Run(); err != nil {
		gitRun(t, e, dir, "init", "-q")
		gitRun(t, e, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	}
}

// repoWithCommit 建一個有一個 commit 的倉庫，回傳目錄。
func repoWithCommit(t *testing.T, e proc.Env) string {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, e, dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, e, dir, "add", "a.txt")
	gitRun(t, e, dir, "commit", "-q", "-m", "init")
	return dir
}

func staticEnv(e proc.Env) func(context.Context) (proc.Env, error) {
	return func(context.Context) (proc.Env, error) { return e, nil }
}

func newGit(t *testing.T, e proc.Env, opts func(*workspace.Options)) *workspace.Git {
	t.Helper()
	return newGitF(t, staticEnv(e), opts)
}

func newGitF(t *testing.T, env func(context.Context) (proc.Env, error), opts func(*workspace.Options)) *workspace.Git {
	t.Helper()
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	o := workspace.Options{Runner: runner, Env: env}
	if opts != nil {
		opts(&o)
	}
	return workspace.New(o)
}

// gitWrapper 在 dir 寫一個假 git：先記一行到 log，再 exec 真的 git。
// 回傳 log 的路徑。
func gitWrapper(t *testing.T, dir, log string) {
	t.Helper()
	fakeGit(t, dir, `echo ran >> '`+log+`'
exec '`+gitBin+`' "$@"`)
}

func TestInspect(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	t.Run("plain dir", func(t *testing.T) {
		e := isolatedEnv(t)
		repo, err := newGit(t, e, nil).Inspect(ctx, t.TempDir())
		if err != nil || repo != (task.Repo{}) {
			t.Fatalf("got %+v, %v", repo, err)
		}
	})

	commit := func(t *testing.T, e proc.Env, dir string) string {
		t.Helper()
		return strings.TrimSpace(gitRun(t, e, dir, "rev-parse", "HEAD"))
	}

	t.Run("one commit clean", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := repoWithCommit(t, e)
		repo, err := newGit(t, e, nil).Inspect(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		want := task.Repo{IsGit: true, Branch: "main", Commit: commit(t, e, dir),
			Root: realDir(t, dir)}
		if repo != want {
			t.Errorf("got %+v, want %+v", repo, want)
		}
	})

	t.Run("tracked modified is dirty", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := repoWithCommit(t, e)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		repo, err := newGit(t, e, nil).Inspect(ctx, dir)
		if err != nil || !repo.Dirty {
			t.Fatalf("got %+v, %v", repo, err)
		}
	})

	t.Run("staged is dirty", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := repoWithCommit(t, e)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, e, dir, "add", "a.txt")
		repo, err := newGit(t, e, nil).Inspect(ctx, dir)
		if err != nil || !repo.Dirty {
			t.Fatalf("got %+v, %v", repo, err)
		}
	})

	t.Run("untracked is not dirty", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := repoWithCommit(t, e)
		if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		repo, err := newGit(t, e, nil).Inspect(ctx, dir)
		if err != nil || repo.Dirty {
			t.Fatalf("got %+v, %v", repo, err)
		}
	})

	t.Run("detached head", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := repoWithCommit(t, e)
		gitRun(t, e, dir, "checkout", "-q", "--detach")
		repo, err := newGit(t, e, nil).Inspect(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		if repo.Branch != "" || repo.Commit != commit(t, e, dir) || repo.Root != realDir(t, dir) {
			t.Errorf("got %+v", repo)
		}
	})

	t.Run("unborn head", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := t.TempDir()
		gitInit(t, e, dir)
		repo, err := newGit(t, e, nil).Inspect(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		if repo != (task.Repo{IsGit: true, Root: realDir(t, dir)}) {
			t.Errorf("got %+v", repo)
		}
	})

	t.Run("subdirectory", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := repoWithCommit(t, e)
		sub := filepath.Join(dir, "sub", "deep")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		repo, err := newGit(t, e, nil).Inspect(ctx, sub)
		if err != nil {
			t.Fatal(err)
		}
		want := task.Repo{IsGit: true, Branch: "main", Commit: commit(t, e, dir), Dirty: true,
			Root: realDir(t, dir)}
		if repo != want {
			t.Errorf("got %+v, want %+v", repo, want)
		}
	})

	t.Run("bare repo", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := filepath.Join(t.TempDir(), "bare.git")
		gitRun(t, e, t.TempDir(), "init", "-q", "--bare", dir)
		repo, err := newGit(t, e, nil).Inspect(ctx, dir)
		if err != nil || repo != (task.Repo{}) {
			t.Fatalf("got %+v, %v", repo, err)
		}
	})

	t.Run("dot git dir", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := repoWithCommit(t, e)
		repo, err := newGit(t, e, nil).Inspect(ctx, filepath.Join(dir, ".git"))
		if err != nil || repo != (task.Repo{}) {
			t.Fatalf("got %+v, %v", repo, err)
		}
	})

	t.Run("missing and file", func(t *testing.T) {
		e := isolatedEnv(t)
		g := newGit(t, e, nil)
		f := filepath.Join(t.TempDir(), "f.txt")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(t.TempDir(), "nope"), f} {
			if _, err := g.Inspect(ctx, path); err == nil {
				t.Fatalf("%s: no error", path)
			} else {
				var werr *workspace.Error
				if !errors.As(err, &werr) || werr.Code != workspace.CodeDirMissing || werr.Params["path"] != path {
					t.Fatalf("%s: got %v", path, err)
				}
			}
		}
	})

	t.Run("git fails", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := t.TempDir()
		fake := fakeGit(t, dir, `printf 'fatal: boom\n' >&2\nexit 2`)
		g := newGit(t, e, func(o *workspace.Options) { o.Git = fake })
		_, err := g.Inspect(ctx, dir)
		var werr *workspace.Error
		if !errors.As(err, &werr) || werr.Code != workspace.CodeGitFailed || werr.Params["detail"] != "fatal: boom" {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("git env and dir", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := t.TempDir()
		out := filepath.Join(dir, "env.txt")
		fake := fakeGit(t, dir,
			`{ env; printf 'PWD=%s\n' "$PWD"; } > '`+out+`'\nprintf 'true\n'\nexit 0`)
		g := newGit(t, e, func(o *workspace.Options) { o.Git = fake })
		if _, err := g.Inspect(ctx, dir); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		s := string(data)
		for _, kv := range []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "NO_COLOR=1"} {
			if !strings.Contains(s, "\n"+kv+"\n") && !strings.HasPrefix(s, kv+"\n") {
				t.Errorf("env missing %s\n%s", kv, s)
			}
		}
		gotPwd, _ := filepath.EvalSymlinks(strings.TrimPrefix(strings.TrimSpace(s[strings.LastIndex(s, "PWD="):]), "PWD="))
		wantPwd, _ := filepath.EvalSymlinks(dir)
		if gotPwd != wantPwd {
			t.Errorf("pwd = %q, want %q", gotPwd, wantPwd)
		}
	})

	t.Run("cancelled ctx", func(t *testing.T) {
		e := isolatedEnv(t)
		dir := repoWithCommit(t, e)
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := newGit(t, e, nil).Inspect(ctx, dir); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}

// W：環境函式、候選迭代、倉庫變數剔除與無 git 的處理（G8、P3）。
func TestInspectEnv(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	// W1：Env 函式給的 PATH 裡的 git 包裝真的被用（繼承 PATH 沒有它）。
	t.Run("env func provides git", func(t *testing.T) {
		dir := repoWithCommit(t, isolatedEnv(t))
		wrap := t.TempDir()
		log := filepath.Join(t.TempDir(), "log")
		gitWrapper(t, wrap, log)
		g := newGit(t, proc.Env{"HOME=" + t.TempDir(), "PATH=" + wrap}, nil)
		if _, err := g.Inspect(ctx, dir); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(log); err != nil || !strings.Contains(string(b), "ran") {
			t.Fatalf("wrapper not used: %q, %v", b, err)
		}
	})

	// W2：Env 每次操作都會重取；第二次 Inspect 用第二份環境。
	t.Run("env taken per operation", func(t *testing.T) {
		dir := repoWithCommit(t, isolatedEnv(t))
		mk := func() (proc.Env, string) {
			wrap := t.TempDir()
			log := filepath.Join(t.TempDir(), "log")
			gitWrapper(t, wrap, log)
			return proc.Env{"HOME=" + t.TempDir(), "PATH=" + wrap}, log
		}
		e1, log1 := mk()
		e2, log2 := mk()
		var calls atomic.Int32
		g := newGitF(t, func(context.Context) (proc.Env, error) {
			if calls.Add(1) == 1 {
				return e1, nil
			}
			return e2, nil
		}, nil)
		if _, err := g.Inspect(ctx, dir); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Inspect(ctx, dir); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(log1); err != nil || !strings.Contains(string(b), "ran") {
			t.Fatalf("first env not used: %v", err)
		}
		if b, err := os.ReadFile(log2); err != nil || !strings.Contains(string(b), "ran") {
			t.Fatalf("second env not used: %v", err)
		}
	})

	// W4：上層環境的 GIT_DIR 不能讓 git 答錯目錄（G8）。
	t.Run("repo vars stripped", func(t *testing.T) {
		e := isolatedEnv(t)
		a := repoWithCommit(t, e)
		b := repoWithCommit(t, e)
		gitRun(t, e, a, "checkout", "-q", "-b", "a-branch")
		gitRun(t, e, b, "checkout", "-q", "-b", "b-branch")
		e2 := slices.Concat(e, proc.Env{"GIT_DIR=" + filepath.Join(b, ".git")})
		repo, err := newGit(t, e2, nil).Inspect(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		want := task.Repo{IsGit: true, Branch: "a-branch",
			Commit: commitOf(t, e, a), Root: realDir(t, a)}
		if repo != want {
			t.Fatalf("got %+v, want %+v", repo, want)
		}
	})

	// W5：沒有可用的 git，目錄與上級都沒有 .git → 非 git 目錄。
	t.Run("no git, not a repo", func(t *testing.T) {
		env := proc.Env{"HOME=" + t.TempDir(), "PATH=" + t.TempDir()}
		g := newGitF(t, staticEnv(env), func(o *workspace.Options) {
			o.Root = t.TempDir()
		})
		repo, err := g.Inspect(ctx, t.TempDir())
		if err != nil || repo != (task.Repo{}) {
			t.Fatalf("got %+v, %v", repo, err)
		}
	})

	// W6：沒有可用的 git，但目錄或上級有 .git → workspace.git_missing。
	t.Run("no git, but a repo", func(t *testing.T) {
		mk := func() *workspace.Git {
			env := proc.Env{"HOME=" + t.TempDir(), "PATH=" + t.TempDir()}
			return newGitF(t, staticEnv(env), func(o *workspace.Options) {
				o.Root = t.TempDir()
			})
		}
		base := t.TempDir()
		repo := filepath.Join(base, "repo")
		sub := filepath.Join(repo, "sub")
		// 上級的 .git 是目錄
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := mk().Inspect(ctx, sub); !errors.Is(err, missingGit()) {
			t.Fatalf("parent .git dir: %v", err)
		}
		// 上級的 .git 是檔案（worktree 的 gitdir 指標）
		if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".git"),
			[]byte("gitdir: /elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := mk().Inspect(ctx, sub); !errors.Is(err, missingGit()) {
			t.Fatalf("parent .git file: %v", err)
		}
		// 目錄自己的 .git
		d := t.TempDir()
		if err := os.Mkdir(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := mk().Inspect(ctx, d); !errors.Is(err, missingGit()) {
			t.Fatalf("own .git: %v", err)
		}
	})
}

// P5：Inspect 回傳工作樹根的真實路徑（D72）。
func TestInspectRoot(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	e := isolatedEnv(t)

	// 子目錄與倉庫根回同一個真實路徑。
	repo := repoWithCommit(t, e)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{repo, sub} {
		got, err := newGit(t, e, nil).Inspect(ctx, d)
		if err != nil || got.Root != realDir(t, repo) {
			t.Fatalf("%s: got %+v, %v", d, got, err)
		}
	}
	// 非 git 目錄沒有根。
	got, err := newGit(t, e, nil).Inspect(ctx, t.TempDir())
	if err != nil || got.Root != "" {
		t.Fatalf("plain dir: %+v, %v", got, err)
	}
	// 沒有 commit 的倉庫也有根。
	unborn := t.TempDir()
	gitInit(t, e, unborn)
	got, err = newGit(t, e, nil).Inspect(ctx, unborn)
	if err != nil || got.Root != realDir(t, unborn) {
		t.Fatalf("unborn: %+v, %v", got, err)
	}
	// 經符號連結的查詢也回真實路徑。
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	got, err = newGit(t, e, nil).Inspect(ctx, link)
	if err != nil || got.Root != realDir(t, repo) {
		t.Fatalf("link: %+v, %v", got, err)
	}
}

// Resolve 回傳真實路徑；不存在是 dir_missing。
func TestResolve(t *testing.T) {
	g := newGit(t, proc.Env{}, nil)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := g.Resolve(link)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := g.Resolve(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("no error")
	} else {
		var werr *workspace.Error
		if !errors.As(err, &werr) || werr.Code != workspace.CodeDirMissing {
			t.Fatalf("got %v", err)
		}
	}
}

func commitOf(t *testing.T, e proc.Env, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitRun(t, e, dir, "rev-parse", "HEAD"))
}

func TestInspectFeedsDecide(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	e := isolatedEnv(t)
	dir := repoWithCommit(t, e)
	repo, err := newGit(t, e, nil).Inspect(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	launch := task.LaunchSnapshot{Agent: "x", Executable: "x"}
	events, err := task.Decide(task.State{ID: "t-1"}, task.Inputs{Repo: repo, Policy: task.PolicyAuto},
		task.Command{Kind: task.CmdCreateTask, Origin: task.OriginDesktop,
			CreateTask: &task.CreateTask{Title: "t", Brief: "b", Workspace: "w1", Launches: []task.LaunchSnapshot{launch}}})
	if err != nil {
		t.Fatal(err)
	}
	var created *task.TaskCreated
	for i := range events {
		if events[i].TaskCreated != nil {
			created = events[i].TaskCreated
		}
	}
	if created == nil || created.Isolation != task.IsolationNone {
		t.Fatalf("got %+v", created)
	}

	// detached HEAD + 兩個 Launch → detached_head。
	gitRun(t, e, dir, "checkout", "-q", "--detach")
	repo, err = newGit(t, e, nil).Inspect(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = task.Decide(task.State{ID: "t-1"}, task.Inputs{Repo: repo, Policy: task.PolicyAuto},
		task.Command{Kind: task.CmdCreateTask, Origin: task.OriginDesktop,
			CreateTask: &task.CreateTask{Title: "t", Brief: "b", Workspace: "w1", Launches: []task.LaunchSnapshot{launch, launch}}})
	var rej *task.Rejection
	if !errors.As(err, &rej) || rej.Reason != task.ReasonDetachedHead {
		t.Fatalf("got %v", err)
	}
}

func TestExists(t *testing.T) {
	g := newGit(t, proc.Env{}, nil)
	dir := t.TempDir()
	if !g.Exists(dir) {
		t.Error("dir should exist")
	}
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if g.Exists(f) {
		t.Error("file should not exist")
	}
	if g.Exists(filepath.Join(dir, "nope")) {
		t.Error("missing should not exist")
	}
}

func TestUnsupported(t *testing.T) {
	g := newGit(t, proc.Env{}, nil)
	ctx := context.Background()
	for name, err := range map[string]error{
		"create":  g.Create(ctx, task.CreateWorktree{}),
		"discard": g.Discard(ctx, task.Worktree{}),
	} {
		checkUnsupported(t, name, err)
	}
	if _, _, err := g.Merge(ctx, task.MergeWorktree{}); err != nil {
		checkUnsupported(t, "merge", err)
	} else {
		t.Error("merge should fail")
	}
	if _, err := g.Diff(ctx, task.Worktree{}); err != nil {
		checkUnsupported(t, "diff", err)
	} else {
		t.Error("diff should fail")
	}
}

func checkUnsupported(t *testing.T, op string, err error) {
	t.Helper()
	var werr *workspace.Error
	if !errors.As(err, &werr) || werr.Code != workspace.CodeUnsupported || werr.Params["op"] != op {
		t.Fatalf("%s: got %v", op, err)
	}
}

func missingGit() error { return &workspace.Error{Code: workspace.CodeGitMissing} }

// fakeGit 在 dir 寫一個可執行的假 git script。
func fakeGit(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "git")
	body = strings.ReplaceAll(body, `\n`, "\n")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
