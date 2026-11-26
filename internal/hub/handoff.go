package hub

import (
	"context"

	"github.com/bayuncao/oriel/internal/task"
)

// changes 是接力時交接內容裡 Task 的改動（m4.md HO05、HO08–HO11）：Task Worktree 開著時是它相對
// BaseCommit 的檔案與行數（同 Diff，不要 patch）；Task 直接在資料夾裡時是它的 Session 改過、在資料夾裡的
// 檔案（Index.EditedFiles），與資料夾是不是 git 倉庫。讀不到時照常接力，Unread 為真並記下原因（HO11）。
func (h *Hub) changes(ctx context.Context, s task.State) task.Changes {
	if w := s.Worktree; w != nil {
		c := task.Changes{Worktree: true}
		text, err := h.git.Diff(ctx, w.Worktree)
		if err != nil {
			h.logger.Warn("hub: handoff could not read the worktree diff", "task", s.ID, "err", err)
			c.Unread = true
			return c
		}
		for _, f := range parseDiff(text, 0).Files {
			c.Files = append(c.Files, task.ChangedFile{Path: f.Path, OldPath: f.OldPath, Change: string(f.Change),
				Added: f.Added, Removed: f.Removed, Binary: f.Binary})
		}
		return capFiles(c)
	}
	var c task.Changes
	if repo, err := h.git.Inspect(ctx, h.workspaceDir(s.Workspace)); err == nil {
		c.Git = repo.IsGit
	}
	paths, err := h.index.EditedFiles(ctx, s.ID)
	if err != nil {
		h.logger.Warn("hub: handoff could not read the files the task edited", "task", s.ID, "err", err)
		c.Unread = true
		return c
	}
	for _, p := range paths {
		c.Files = append(c.Files, task.ChangedFile{Path: p})
	}
	return capFiles(c)
}

// capFiles 只留前 MaxHandoffFiles 個檔案，其餘記在 More（HO09）。
func capFiles(c task.Changes) task.Changes {
	if n := len(c.Files); n > task.MaxHandoffFiles {
		c.More = n - task.MaxHandoffFiles
		c.Files = c.Files[:task.MaxHandoffFiles]
	}
	return c
}
