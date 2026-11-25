// Package workspace 實作 hub 的 Git 連接埠：Inspect 以 git 命令
// 回答目錄的倉庫狀態，Exists 回答目錄存在；Create、Merge、Discard、Diff
// 建立、合併、捨棄 worktree 並計算它的改動（M4-1，D159）。
// 規則見 docs/architecture/hub.md（Git 連接埠）與
// docs/architecture/domain.md（隔離與 Fork、Inputs.Repo）。
package workspace
