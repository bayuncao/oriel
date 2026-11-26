package hub

import (
	"cmp"
	"context"
	"strconv"
	"strings"

	"github.com/bayuncao/oriel/internal/task"
)

// maxDiffPatch 是 Hub.Diff 放進結果的 patch 合計上限（位元組）：再多介面也看不完，
// 傳給前端的資料也不該無限大（m4.md WT04）。
const maxDiffPatch = 1 << 20

// Diff 是 Task Worktree 相對它的 BaseCommit 的改動，逐檔拆開（m4.md WT01–WT07）：
// 已提交、未提交與未追蹤的都在，worktree 不被改動。Task 沒有開著的 Task Worktree 時
// 是 hub.no_worktree；worktree 還沒建好時是 Git 回的錯誤（workspace.dir_missing）。
func (h *Hub) Diff(ctx context.Context, p Principal, id task.TaskID) (DiffView, error) {
	s, err := h.Snapshot(ctx, p, id)
	if err != nil {
		return DiffView{}, err
	}
	if s.Worktree == nil || s.Worktree.Outcome != task.OutcomeOpen {
		return DiffView{}, fail(CodeNoWorktree, nil)
	}
	text, err := h.git.Diff(ctx, s.Worktree.Worktree)
	if err != nil {
		// 算的同時合併或丟棄可能正在刪這個 worktree（Diff 不經 Actor，與 Effect 並行）：那時的錯誤不是故障，
		// worktree 已處置或有處置意圖就回 no_worktree。
		if now, serr := h.Snapshot(ctx, p, id); serr == nil && (now.Worktree == nil || now.WorktreeClosing()) {
			return DiffView{}, fail(CodeNoWorktree, err)
		}
		return DiffView{}, err
	}
	return parseDiff(text, maxDiffPatch), nil
}

// parseDiff 把 git diff（a/、b/ 前綴，Git 連接埠的格式）拆成檔案，保持 git 的順序；
// patch 依序放到合計會超過 limit 的第一個檔案為止，之後的只留路徑與行數。
func parseDiff(text string, limit int) DiffView {
	files := []DiffFile{}
	for _, chunk := range splitDiff(text) {
		files = append(files, parseFile(chunk))
	}
	v := DiffView{Files: files}
	used := 0
	for i := range v.Files {
		f := &v.Files[i]
		if f.Patch == "" {
			continue
		}
		if v.Omitted || used+len(f.Patch) > limit {
			f.Patch, f.Omitted, v.Omitted = "", true, true
			continue
		}
		used += len(f.Patch)
	}
	return v
}

// splitDiff 依 "diff --git " 開頭的行把輸出切成每個檔案一段。
func splitDiff(text string) []string {
	var out []string
	start := -1
	at := 0
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, "diff --git ") {
			if start >= 0 {
				out = append(out, text[start:at])
			}
			start = at
		}
		at += len(line)
	}
	if start >= 0 {
		out = append(out, text[start:])
	}
	return out
}

// parseFile 解析一個檔案的一段：標頭決定路徑與種類，第一個 @@ 起是 patch。
func parseFile(chunk string) DiffFile {
	f := DiffFile{Change: ChangeModified}
	header, patch := chunk, ""
	if i := hunkStart(chunk); i >= 0 {
		header, patch = chunk[:i], chunk[i:]
	}
	var from, to string
	for line := range strings.Lines(header) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			from, to = gitPaths(line[len("diff --git "):])
		case strings.HasPrefix(line, "new file mode "):
			f.Change = ChangeAdded
		case strings.HasPrefix(line, "deleted file mode "):
			f.Change = ChangeDeleted
		case strings.HasPrefix(line, "rename from "):
			f.Change, from = ChangeRenamed, unquote(line[len("rename from "):])
		case strings.HasPrefix(line, "rename to "):
			to = unquote(line[len("rename to "):])
		case strings.HasPrefix(line, "--- "):
			if p, ok := strings.CutPrefix(unquote(trimTab(line[4:])), "a/"); ok {
				from = p
			}
		case strings.HasPrefix(line, "+++ "):
			if p, ok := strings.CutPrefix(unquote(trimTab(line[4:])), "b/"); ok {
				to = p
			}
		case strings.HasPrefix(line, "Binary files "), line == "GIT binary patch":
			f.Binary = true
		}
	}
	f.Path = cmp.Or(to, from)
	switch f.Change {
	case ChangeDeleted:
		f.Path = cmp.Or(from, to)
	case ChangeRenamed:
		f.OldPath = from
	}
	if f.Binary {
		return f
	}
	f.Patch = patch
	for line := range strings.Lines(patch) {
		switch {
		case strings.HasPrefix(line, "+"):
			f.Added++
		case strings.HasPrefix(line, "-"):
			f.Removed++
		}
	}
	return f
}

// hunkStart 是第一個以 @@ 開頭的行的位置；沒有時為 -1。
func hunkStart(chunk string) int {
	if strings.HasPrefix(chunk, "@@") {
		return 0
	}
	if i := strings.Index(chunk, "\n@@"); i >= 0 {
		return i + 1
	}
	return -1
}

// gitPaths 解析 "diff --git" 之後的兩個路徑。路徑有特殊字元時 git 以 C 的寫法加引號；
// 沒加引號時兩邊同名（改名另有 rename 行），各佔一半。
func gitPaths(s string) (from, to string) {
	if strings.HasPrefix(s, `"`) {
		if q, err := strconv.QuotedPrefix(s); err == nil {
			return strings.TrimPrefix(unquote(q), "a/"),
				strings.TrimPrefix(unquote(strings.TrimSpace(s[len(q):])), "b/")
		}
	}
	if n := len(s); n >= 5 && n%2 == 1 {
		a, b := s[:n/2], s[n/2+1:]
		if strings.HasPrefix(a, "a/") && strings.HasPrefix(b, "b/") && a[2:] == b[2:] {
			return a[2:], b[2:]
		}
	}
	if i := strings.Index(s, " b/"); i >= 0 {
		return strings.TrimPrefix(s[:i], "a/"), unquote(s[i+3:])
	}
	return "", ""
}

// unquote 去掉 git 給路徑加的 C 寫法的引號；沒加引號的原樣回傳。
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

// trimTab 去掉 git 在含空白的路徑後面加的定位字元。
func trimTab(s string) string {
	s, _, _ = strings.Cut(s, "\t")
	return s
}
