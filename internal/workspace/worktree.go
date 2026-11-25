package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// worktree 的建立、合併、捨棄與差異（domain.md「隔離與 Fork」、D16、D36、D37）。
// 每個操作都可重試：崩潰或逾時後再做一次，從 git 的現狀接著做完，結果相同。

// mergeTreeMin 是合併需要的最低 git 版本：merge-tree --write-tree 在記憶體裡合併，
// 不碰任何工作目錄與 index。
var mergeTreeMin = [2]int{2, 38}

// op 是一次操作：環境快照與 git 路徑只取一次。
type op struct {
	g   *Git
	ctx context.Context
	env proc.Env
	git string
}

func (g *Git) begin(ctx context.Context) (*op, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env, err := g.env(ctx)
	if err != nil {
		return nil, err
	}
	git, err := g.resolveGit(ctx, env)
	if err != nil {
		return nil, err
	}
	return &op{g: g, ctx: ctx, env: env, git: git}, nil
}

// try 在 dir 執行 git，結束碼交給呼叫者判斷；只有 context、逾時與 Runner 關閉回 error。
// extra 加在固定環境之後（例如臨時的 GIT_INDEX_FILE）。
func (o *op) try(dir string, extra []string, timeout time.Duration, args ...string) (proc.Result, error) {
	return o.g.exec(o.ctx, proc.Probe{Path: o.git, Args: args, Dir: dir,
		Env: o.env.Without(gitRepoVars...).With(gitEnv...).With(extra...), Timeout: timeout})
}

// out 在 dir 執行 git，非零結束是 git_failed；回傳原樣的 stdout。
func (o *op) out(dir string, args ...string) (string, error) {
	res, err := o.try(dir, nil, opTimeout, args...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", gitFailed(res)
	}
	return string(res.Stdout), nil
}

// do 同 out，去掉頭尾空白。
func (o *op) do(dir string, args ...string) (string, error) {
	s, err := o.out(dir, args...)
	return strings.TrimSpace(s), err
}

// linked 是 git worktree list 的一筆：路徑與檢出的分支（detached 時為空）。
type linked struct{ path, branch string }

func (o *op) worktrees(repo string) ([]linked, error) {
	s, err := o.out(repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var ws []linked
	for f := range strings.SplitSeq(s, "\x00") {
		switch {
		case strings.HasPrefix(f, "worktree "):
			ws = append(ws, linked{path: strings.TrimPrefix(f, "worktree ")})
		case strings.HasPrefix(f, "branch ") && len(ws) > 0:
			ws[len(ws)-1].branch = strings.TrimPrefix(f, "branch refs/heads/")
		}
	}
	return ws, nil
}

func (o *op) branchExists(repo, branch string) (bool, error) {
	res, err := o.try(repo, nil, opTimeout, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, gitFailed(res)
}

// samePath 比較兩個路徑，能解開符號連結時比較解開後的（t.TempDir 與資料目錄可能經過連結）。
func samePath(a, b string) bool {
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return real(a) == real(b)
}

// Create 建立 worktree：從 BaseCommit 切出 Branch 檢出到 Path（W1、W5）。已建好的什麼都不做（W2）；
// 分支還在而目錄不見了，把現有的分支檢出回去，分支上的 commit 不丟（W3）；
// Path 被別的東西佔著時不碰它（W4）。
func (g *Git) Create(ctx context.Context, c task.CreateWorktree) error {
	o, err := g.begin(ctx)
	if err != nil {
		return err
	}
	w := c.Worktree
	ws, err := o.worktrees(c.Repo)
	if err != nil {
		return err
	}
	for _, l := range ws {
		if samePath(l.path, w.Path) && g.Exists(w.Path) {
			if l.branch == w.Branch {
				return nil
			}
			return fail(CodePathTaken, nil, "path", w.Path)
		}
	}
	// WorktreeRoot 建不出來（例如被一個檔案佔著）是 dir_missing（W6）。
	if root := filepath.Dir(w.Path); os.MkdirAll(root, 0o755) != nil {
		return fail(CodeDirMissing, nil, "path", root)
	}
	if _, err := os.Lstat(w.Path); err == nil {
		return fail(CodePathTaken, nil, "path", w.Path)
	}
	// 目錄已不在的登記先清掉，否則 git 認為分支仍檢出在那裡。
	if _, err := o.do(c.Repo, "worktree", "prune"); err != nil {
		return err
	}
	has, err := o.branchExists(c.Repo, w.Branch)
	if err != nil {
		return err
	}
	if has {
		_, err = o.do(c.Repo, "worktree", "add", "--quiet", w.Path, w.Branch)
	} else {
		_, err = o.do(c.Repo, "worktree", "add", "--quiet", "-b", w.Branch, w.Path, w.BaseCommit)
	}
	return err
}

// Diff 是 worktree 相對 BaseCommit 的全部改動：已提交、未提交與未追蹤的檔案，.gitignore 忽略的不算（D1–D4）。
// 在臨時的 index 上算，worktree 的 index 與檔案都不變；diff 寫進檔案，不受探測輸出的上限截斷。
func (g *Git) Diff(ctx context.Context, w task.Worktree) (string, error) {
	o, err := g.begin(ctx)
	if err != nil {
		return "", err
	}
	tmp, extra, err := o.tempIndex(w.Path)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "diff")
	for _, args := range [][]string{
		{"diff", "--cached", "--no-color", "--no-ext-diff", "--no-textconv",
			"--src-prefix=a/", "--dst-prefix=b/", "--output=" + out, w.BaseCommit, "--"},
	} {
		res, err := o.try(w.Path, extra, opTimeout, args...)
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			return "", gitFailed(res)
		}
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// tempIndex 在臨時目錄做一份 dir 的 index，加進 dir 現在的全部改動（含未追蹤的檔案），
// 回傳臨時目錄（用完由呼叫者刪）與指向它的環境。dir 的 index 與檔案都不變。
func (o *op) tempIndex(dir string) (string, []string, error) {
	tmp, err := os.MkdirTemp("", "orielhub-index-")
	if err != nil {
		return "", nil, err
	}
	index := filepath.Join(tmp, "index")
	// 從 worktree 的 index 複製一份：帶著檔案的 stat 資訊，add -A 不必重算每個檔案。
	real, err := o.do(dir, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err == nil {
		if b, rerr := os.ReadFile(real); rerr == nil {
			err = os.WriteFile(index, b, 0o600)
		}
	}
	extra := []string{"GIT_INDEX_FILE=" + index}
	if err == nil {
		var res proc.Result
		if res, err = o.try(dir, extra, opTimeout, "add", "-A"); err == nil && res.ExitCode != 0 {
			err = gitFailed(res)
		}
	}
	if err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	return tmp, extra, nil
}

// snapshot 把 dir 現在的內容（含未提交與未追蹤的改動）做成一個 commit 物件，父 commit 是 HEAD，
// 不移動任何分支、不跑 hook、不簽名：它只是合併的輸入，不會留在任何分支上。沒有改動時就是 HEAD。
func (o *op) snapshot(dir, msg string) (string, error) {
	head, err := o.do(dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	st, err := o.do(dir, "status", "--porcelain")
	if err != nil || st == "" {
		return head, err
	}
	tmp, extra, err := o.tempIndex(dir)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	res, err := o.try(dir, extra, opTimeout, "write-tree")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", gitFailed(res)
	}
	return o.do(dir, "-c", "user.useConfigOnly=true", "commit-tree", "--no-gpg-sign",
		strings.TrimSpace(string(res.Stdout)), "-p", head, "-m", msg)
}

// Merge 把 From 合進它的 BaseBranch，成功後刪掉 From 的 worktree 與分支（D37）。
// squash：把 From 的現狀（含未提交的改動）壓成一個 commit 合進去，見 squash（D159）；
// 有衝突時回傳衝突的檔案，任何目錄與分支都不改。pull_request：推送分支並開 PR（P1–P6）。
func (g *Git) Merge(ctx context.Context, m task.MergeWorktree) ([]string, string, error) {
	o, err := g.begin(ctx)
	if err != nil {
		return nil, "", err
	}
	from := m.From
	there := g.Exists(from.Path)
	has, err := o.branchExists(m.Into, from.Branch)
	if err != nil {
		return nil, "", err
	}
	if m.Strategy == task.MergePullRequest {
		url, err := o.pullRequest(m, there, has)
		return nil, url, err
	}
	if !there && !has {
		return nil, "", nil // 上次已合進去並刪掉了（M7）
	}
	if err := o.atLeast(mergeTreeMin); err != nil {
		return nil, "", err
	}
	if err := o.identity(m.Into); err != nil {
		return nil, "", err
	}
	head := "refs/heads/" + from.Branch
	if there {
		// 合併 worktree 的現狀，但不在它的分支上提交：有衝突時 From 也原封不動（M4）。
		if head, err = o.snapshot(from.Path, m.Message); err != nil {
			return nil, "", err
		}
	}
	for range mergeAttempts {
		conflicts, moved, err := o.squash(m, head)
		if err != nil || conflicts != nil {
			return conflicts, "", err
		}
		if !moved {
			return nil, "", o.remove(m.Into, from, there, has)
		}
	}
	return nil, "", fail(CodeBranchMoved, nil, "branch", from.BaseBranch)
}

// mergeAttempts 是基礎分支在合併途中被推進（例如另一個 Task 剛合進去）時重算的次數（M9）。
const mergeAttempts = 3

// squash 把 head 壓成一個 commit 合進 From 的 BaseBranch：merge-tree 在記憶體裡算出合併結果，
// 有衝突時回傳衝突的檔案、什麼都不改（D36）；沒有新內容時不提交（M5、M6）。
// moved 是基礎分支在算的途中被推進了：什麼都沒改，以新的基礎分支重算即可。
func (o *op) squash(m task.MergeWorktree, head string) (conflicts []string, moved bool, err error) {
	from := m.From
	base, err := o.do(m.Into, "rev-parse", "--verify", "refs/heads/"+from.BaseBranch+"^{commit}")
	if err != nil {
		return nil, false, err
	}
	res, err := o.try(m.Into, nil, opTimeout,
		"merge-tree", "--write-tree", "--name-only", "--no-messages", base, head)
	if err != nil {
		return nil, false, err
	}
	if res.ExitCode != 0 && res.ExitCode != 1 {
		return nil, false, gitFailed(res)
	}
	lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	if res.ExitCode == 1 {
		return conflictFiles(lines[1:]), false, nil
	}
	baseTree, err := o.do(m.Into, "rev-parse", base+"^{tree}")
	if err != nil || lines[0] == baseTree {
		return nil, false, err
	}
	// 照使用者的 commit.gpgSign 簽名：這個 commit 會留在基礎分支上。
	c, err := o.do(m.Into, "-c", "user.useConfigOnly=true", "commit-tree", lines[0], "-p", base, "-m", m.Message)
	if err != nil {
		return nil, false, err
	}
	moved, err = o.advance(m.Into, from.BaseBranch, base, c)
	return nil, moved, err
}

// conflictFiles 是 merge-tree --name-only 列出的衝突檔案，去掉重複與空行，依出現順序。
func conflictFiles(lines []string) []string {
	var files []string
	for _, l := range lines {
		if l != "" && !slices.Contains(files, l) {
			files = append(files, l)
		}
	}
	return files
}

// advance 把 branch 從 old 推進到 new：檢出著它的目錄以 merge --ff-only 推進，
// 未提交的改動與合併結果重疊時 git 拒絕，回報 dirty_target（M3′、M3″）；
// 沒有檢出時以 update-ref 帶舊值推進（M2′）。branch 已不是 old 時不覆蓋，moved 為真（M9）。
func (o *op) advance(repo, branch, old, new string) (moved bool, err error) {
	ws, err := o.worktrees(repo)
	if err != nil {
		return false, err
	}
	for _, l := range ws {
		if l.branch != branch {
			continue
		}
		res, err := o.try(l.path, nil, opTimeout, "merge", "--ff-only", "--quiet", new)
		if err != nil || res.ExitCode == 0 {
			return false, err
		}
		if moved, err := o.movedFrom(repo, branch, old); err != nil || moved {
			return moved, err
		}
		if files := overwritten(string(res.Stderr)); len(files) > 0 {
			return false, fail(CodeDirtyTarget, nil, "path", l.path, "files", strings.Join(files, "\n"))
		}
		return false, gitFailed(res)
	}
	res, err := o.try(repo, nil, opTimeout, "update-ref", "refs/heads/"+branch, new, old)
	if err != nil || res.ExitCode == 0 {
		return false, err
	}
	if moved, err := o.movedFrom(repo, branch, old); err != nil || moved {
		return moved, err
	}
	return false, gitFailed(res)
}

// movedFrom 回報 branch 已不在 old。
func (o *op) movedFrom(repo, branch, old string) (bool, error) {
	now, err := o.do(repo, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	return err == nil && now != old, err
}

// overwritten 是 git 因未提交的改動（或未追蹤的檔案）會被覆蓋而拒絕時列出的檔案。
func overwritten(stderr string) []string {
	var files []string
	listing := false
	for l := range strings.SplitSeq(stderr, "\n") {
		switch {
		case strings.Contains(l, "would be overwritten by merge"):
			listing = true
		case listing && strings.HasPrefix(l, "\t"):
			files = append(files, strings.TrimSpace(l))
		default:
			listing = false
		}
	}
	return files
}

// commitAll 以 msg 提交 dir 裡未提交的改動（含新檔案），給 PR 推送用；沒有改動時什麼都不做。
// 這是替 agent 的工作做的自動提交，不跑使用者的 hook（PR 的檢查交給 CI）；簽名照使用者的設定。
func (o *op) commitAll(dir, msg string) error {
	st, err := o.do(dir, "status", "--porcelain")
	if err != nil || st == "" {
		return err
	}
	if _, err := o.do(dir, "add", "-A"); err != nil {
		return err
	}
	_, err = o.do(dir, "-c", "user.useConfigOnly=true", "commit", "--quiet", "--no-verify", "-m", msg)
	return err
}

// identity 確認 git 有使用者設定的作者與提交者（M8）：useConfigOnly 不讓 git 拿主機名猜一個。
func (o *op) identity(dir string) error {
	for _, v := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
		res, err := o.try(dir, nil, opTimeout, "-c", "user.useConfigOnly=true", "var", v)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fail(CodeNoIdentity, nil, "path", dir)
		}
	}
	return nil
}

// atLeast 確認 git 的版本不低於 want（G5）。
func (o *op) atLeast(want [2]int) error {
	s, err := o.do("", "version")
	if err != nil {
		return err
	}
	version := s
	if f := strings.Fields(s); len(f) >= 3 {
		version = f[2]
	}
	parts := strings.SplitN(version, ".", 3)
	if len(parts) >= 2 {
		major, err1 := strconv.Atoi(parts[0])
		minor, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && (major > want[0] || major == want[0] && minor >= want[1]) {
			return nil
		}
	}
	return fail(CodeGitTooOld, nil, "version", version,
		"want", strconv.Itoa(want[0])+"."+strconv.Itoa(want[1]))
}

// remove 刪掉 worktree 與它的分支，未提交的改動一併丟掉；已不在的部分略過。
func (o *op) remove(repo string, w task.Worktree, there, has bool) error {
	if there {
		if _, err := o.do(repo, "worktree", "remove", "--force", "--force", w.Path); err != nil {
			return err
		}
	} else if _, err := o.do(repo, "worktree", "prune"); err != nil {
		return err
	}
	if has {
		if _, err := o.do(repo, "branch", "--quiet", "-D", w.Branch); err != nil {
			return err
		}
	}
	return nil
}

// Discard 刪掉 worktree 與它的分支（X1）；已不在時什麼都不做（X2）。
func (g *Git) Discard(ctx context.Context, d task.DiscardWorktree) error {
	o, err := g.begin(ctx)
	if err != nil {
		return err
	}
	has, err := o.branchExists(d.Repo, d.Worktree.Branch)
	if err != nil {
		return err
	}
	return o.remove(d.Repo, d.Worktree, g.Exists(d.Worktree.Path), has)
}

// ghEnv 讓 gh 不互動、不檢查更新、不上色。
var ghEnv = []string{"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1"}

// pullRequest 推送 From 的分支並開 PR，回傳它的地址（P1）；分支已有開著的 PR 時沿用（P2）。
// 之後照 D37 刪掉本地的 worktree 與分支，遠端分支留著。推送之前先確認 gh 能用、
// 基礎分支在 origin 上（P6）：PR 開不出來時不留下推送了一半的東西。
func (o *op) pullRequest(m task.MergeWorktree, there, has bool) (string, error) {
	from := m.From
	gh := o.findGh()
	if gh == "" {
		return "", fail(CodeGhMissing, nil)
	}
	if !there && !has {
		// 上次已開好 PR 並刪掉了本地的：回開著的 PR 的地址。
		return o.openPR(gh, m.Into, from.Branch)
	}
	if res, err := o.try(m.Into, nil, opTimeout, "remote", "get-url", "origin"); err != nil {
		return "", err
	} else if res.ExitCode != 0 {
		return "", fail(CodeNoRemote, nil, "path", m.Into)
	}
	res, err := o.try(m.Into, nil, opTimeout, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+from.BaseBranch)
	if err != nil {
		return "", err
	}
	switch res.ExitCode {
	case 0:
	case 2:
		return "", fail(CodeNoBaseOnRemote, nil, "branch", from.BaseBranch)
	default:
		return "", gitFailed(res)
	}
	if err := o.identity(m.Into); err != nil {
		return "", err
	}
	if there {
		if err := o.commitAll(from.Path, m.Message); err != nil {
			return "", err
		}
	}
	ref := "refs/heads/" + from.Branch
	if res, err = o.try(m.Into, nil, opTimeout, "push", "--quiet", "origin", ref+":"+ref); err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", gitFailed(res)
	}
	url, err := o.openPR(gh, m.Into, from.Branch)
	if err != nil {
		return "", err
	}
	if url == "" {
		title, body := splitMessage(m.Message, from.Branch)
		out, err := o.gh(gh, m.Into, "pr", "create", "--base", from.BaseBranch, "--head", from.Branch,
			"--title", title, "--body", body)
		if err != nil {
			return "", err
		}
		lines := strings.Split(out, "\n")
		url = strings.TrimSpace(lines[len(lines)-1])
	}
	return url, o.remove(m.Into, from, there, has)
}

// splitMessage 把合併訊息拆成 PR 的標題（第一個非空行）與正文（其餘）；訊息全空時標題用分支名。
func splitMessage(msg, branch string) (title, body string) {
	rest := strings.TrimSpace(msg)
	title, body, _ = strings.Cut(rest, "\n")
	if title = strings.TrimSpace(title); title == "" {
		title = branch
	}
	return title, strings.TrimSpace(body)
}

// findGh 是第一個能執行的 gh（gh --version 成功）；找不到時為空。
func (o *op) findGh() string {
	for _, p := range proc.Look("gh", proc.SearchDirs(o.env, o.g.root)) {
		if _, err := o.gh(p, "", "--version"); err == nil {
			return p
		}
	}
	return ""
}

// openPR 是 branch 上開著的 PR 的地址，沒有時為空。
func (o *op) openPR(gh, dir, branch string) (string, error) {
	return o.gh(gh, dir, "pr", "list", "--head", branch, "--state", "open",
		"--json", "url", "--jq", `.[0].url // ""`)
}

// gh 在 dir 執行 gh，非零結束是 gh_failed；回傳去掉頭尾空白的 stdout。
func (o *op) gh(gh, dir string, args ...string) (string, error) {
	res, err := o.g.exec(o.ctx, proc.Probe{Path: gh, Args: args, Dir: dir,
		Env: o.env.Without(gitRepoVars...).With(ghEnv...), Timeout: opTimeout})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		detail := firstLine(strings.TrimSpace(string(res.Stderr)))
		if detail == "" && res.Err != nil {
			detail = res.Err.Error()
		}
		return "", fail(CodeGhFailed, res.Err, "detail", detail)
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}
