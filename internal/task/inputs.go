package task

import "time"

// Inputs 是 hub 傳給 Decide 的 Task 外部事實唯讀快照。
// 每個影響決策的輸入都會記錄在產生的事件中，
// 因此重放只需要 Evolve。
type Inputs struct {
	Now time.Time `json:"now"`

	// Rules 是套用於此 Task 的 workspace 與全域規則。
	Rules []Rule `json:"rules"`

	// Repo 是 Task 的工作目錄（Task 建立前即 Workspace）。
	// 它與 Policy、OtherTasksRunning 一起決定隔離方式的選擇；
	// Fork 從其 commit 分支出去；WorktreeRoot 是 worktree
	// 的建立位置。
	Repo              Repo            `json:"repo"`
	Policy            IsolationPolicy `json:"policy"`
	OtherTasksRunning bool            `json:"otherTasksRunning"` // 在同一儲存庫中
	WorktreeRoot      string          `json:"worktreeRoot"`

	// Reviewer 是 fan-out 審查的啟動設定，由 hub 從
	// 預設的審查者 Profile 解析而來；無可用者時為 nil。
	Reviewer *LaunchSnapshot `json:"reviewer"`

	// Labels 是標籤目錄裡現有的標籤（D135）；hub 只在 set_labels 時填。
	Labels []LabelID `json:"labels"`
}
