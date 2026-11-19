// Package workspace 實作 hub 的 Git 連接埠：Inspect 以 git 命令
// 回答目錄的倉庫狀態，Exists 回答目錄存在。worktree 的建、合併、
// 捨棄與 diff 在 M4 之前一律回傳 workspace.unsupported。
// 規則見 docs/architecture/hub.md（Git 連接埠）與
// docs/architecture/domain.md（隔離與 Fork、Inputs.Repo）。
package workspace
