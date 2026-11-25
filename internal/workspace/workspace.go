package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// Options 是 New 的參數。
type Options struct {
	Runner *proc.Runner // 必填
	// Env 每次操作取一次環境快照（如 Discovery.Env，快照未到就等，
	// 呼叫方 ctx 結束即停）；nil → proc.Inherited()。錯誤原樣回傳。
	Env  func(context.Context) (proc.Env, error)
	Git  string // git 可執行檔；"" → 每次呼叫在 Env 的 PATH 與使用者工具目錄中找第一個可用的 git
	Root string // 前綴到系統目錄上；"" 表示 /（測試用，與 proc 一致）
}

// Git 實作 hub 的 Git 連接埠：倉庫狀態（Inspect、Resolve、Exists）與
// worktree 的建立、合併、捨棄與差異（worktree.go）。
type Git struct {
	runner *proc.Runner
	env    func(context.Context) (proc.Env, error)
	git    string
	root   string

	mu    sync.Mutex
	cltOK bool // darwin：xcode-select -p 已確認過 CLT 已裝
}

func New(o Options) *Git {
	env := o.Env
	if env == nil {
		env = func(context.Context) (proc.Env, error) {
			return proc.Inherited(), nil
		}
	}
	return &Git{runner: o.Runner, env: env, git: o.Git, root: o.Root}
}

var gitEnv = []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "NO_COLOR=1"}

// gitRepoVars 是定位倉庫的環境變數：workspace 問的永遠是一個明確的
// 目錄，這些變數只會由外層環境（git 自己為 hook 設的）混進來，
// 留著會讓 git 答錯目錄，因此執行 git 前去掉。配置類變數保留。
var gitRepoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
}

// Inspect 以 git 命令回答 dir 的倉庫狀態（domain.md「隔離與 Fork」）。
// 找不到 git 時只有真的是倉庫才報 workspace.git_missing（P3）。
func (g *Git) Inspect(ctx context.Context, dir string) (task.Repo, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return task.Repo{}, fail(CodeDirMissing, err, "path", dir)
	}
	if err := ctx.Err(); err != nil {
		return task.Repo{}, err
	}
	env, err := g.env(ctx)
	if err != nil {
		return task.Repo{}, err
	}
	git, err := g.resolveGit(ctx, env)
	if err != nil {
		if errors.Is(err, errGitMissing) && !hasDotGit(dir) {
			return task.Repo{}, nil // 不是倉庫：按非 git 目錄處理
		}
		return task.Repo{}, err
	}

	res, err := g.run(ctx, env, git, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return task.Repo{}, err
	}
	if res.ExitCode != 0 {
		if strings.Contains(string(res.Stderr), "not a git repository") {
			return task.Repo{}, nil
		}
		return task.Repo{}, gitFailed(res)
	}
	if strings.TrimSpace(string(res.Stdout)) == "false" {
		return task.Repo{}, nil // bare 倉庫或 .git 目錄
	}
	repo := task.Repo{IsGit: true}

	res, err = g.run(ctx, env, git, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return task.Repo{}, err
	}
	if res.ExitCode != 0 {
		return task.Repo{}, gitFailed(res)
	}
	repo.Root = filepath.Clean(strings.TrimSpace(string(res.Stdout)))
	if real, err := filepath.EvalSymlinks(repo.Root); err == nil {
		repo.Root = real
	}

	res, err = g.run(ctx, env, git, dir, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return task.Repo{}, err
	}
	switch res.ExitCode {
	case 0:
		repo.Commit = strings.TrimSpace(string(res.Stdout))
	case 1:
		return repo, nil // 尚無 commit 的倉庫
	default:
		return task.Repo{}, gitFailed(res)
	}

	res, err = g.run(ctx, env, git, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return task.Repo{}, err
	}
	switch res.ExitCode {
	case 0:
		repo.Branch = strings.TrimSpace(string(res.Stdout))
	case 1:
		// detached HEAD
	default:
		return task.Repo{}, gitFailed(res)
	}

	res, err = g.run(ctx, env, git, dir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return task.Repo{}, err
	}
	if res.ExitCode != 0 {
		return task.Repo{}, gitFailed(res)
	}
	repo.Dirty = strings.TrimSpace(string(res.Stdout)) != ""
	return repo, nil
}

var errGitMissing = &Error{Code: CodeGitMissing}

// hasDotGit 由 dir 向上找 .git（目錄或檔案，直到檔案系統根，
// 不看 GIT_CEILING_DIRECTORIES）：找不到 git 時只有真的在倉庫
// 裡才擋（git_missing），一般目錄照常用（隔離 none）。
func hasDotGit(dir string) bool {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// Resolve 回傳 path 解開符號連結後的真實路徑（D72）；失敗是
// workspace.dir_missing。
func (g *Git) Resolve(dir string) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fail(CodeDirMissing, err, "path", dir)
	}
	return real, nil
}

// Exists 回報 path 是一個存在的目錄。
func (g *Git) Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// localTimeout 是檢查倉庫狀態的時限；opTimeout 是 worktree 操作的：大倉庫的 add -A 與 merge-tree、
// 簽名提交時等使用者輸入密碼、push 與 gh 都可能超過幾秒，時限只用來擋住卡死的命令。
const (
	localTimeout = 10 * time.Second
	opTimeout    = 2 * time.Minute
)

// run 以固定環境執行 git；取消、逾時與 Runner 關閉的錯誤原樣回傳，
// 呼叫者可用 errors.Is 辨認 context 錯誤。
func (g *Git) run(ctx context.Context, env proc.Env, git, dir string, args ...string) (proc.Result, error) {
	return g.exec(ctx, proc.Probe{Path: git, Args: args, Dir: dir,
		Env: env.Without(gitRepoVars...).With(gitEnv...), Timeout: localTimeout})
}

// exec 執行 p，錯誤的分法同 run。
func (g *Git) exec(ctx context.Context, p proc.Probe) (proc.Result, error) {
	res := g.runner.Run(ctx, p)
	if res.Err != nil && (res.TimedOut || ctx.Err() != nil || errors.Is(res.Err, proc.ErrClosed)) {
		return res, res.Err
	}
	return res, nil
}

// resolveGit 決定 git 路徑並確認它能跑（darwin 上 /usr/bin/git 需要 CLT）。
func (g *Git) resolveGit(ctx context.Context, env proc.Env) (string, error) {
	if g.git != "" {
		if proc.Executable(g.git) && g.usable(ctx, env, g.git) {
			return g.git, nil
		}
		return "", fail(CodeGitMissing, nil)
	}
	for _, p := range proc.Look("git", proc.SearchDirs(env, g.root)) {
		if g.usable(ctx, env, p) {
			return p, nil
		}
	}
	return "", fail(CodeGitMissing, nil)
}

// gitFailed 以 stderr 首行為 detail；沒有 stderr（如無法啟動）時用錯誤本身的說明。
func gitFailed(res proc.Result) *Error {
	detail := firstLine(string(res.Stderr))
	if detail == "" && res.Err != nil {
		detail = res.Err.Error()
	}
	return fail(CodeGitFailed, res.Err, "detail", detail)
}

// firstLine 取錯誤輸出的第一行，最多 200 字元。
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line[:min(len(line), 200)]
}
