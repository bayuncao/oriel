package workspace

import (
	"context"

	"github.com/bayuncao/oriel/internal/task"
)

// worktree 操作在 M4 才會實作；目前一律回傳 workspace.unsupported，
// hub 把它當一般的 Effect 失敗掛起。

func (g *Git) Create(context.Context, task.CreateWorktree) error {
	return fail(CodeUnsupported, nil, "op", "create")
}

func (g *Git) Merge(context.Context, task.MergeWorktree) (conflicts []string, url string, err error) {
	return nil, "", fail(CodeUnsupported, nil, "op", "merge")
}

func (g *Git) Discard(context.Context, task.Worktree) error {
	return fail(CodeUnsupported, nil, "op", "discard")
}

func (g *Git) Diff(context.Context, task.Worktree) (string, error) {
	return "", fail(CodeUnsupported, nil, "op", "diff")
}
