package task

// IsolationMode 是 Task 在其 Workspace 中的工作方式，建立時決定。
type IsolationMode string

const (
	IsolationNone     IsolationMode = "none"     // 直接在 Workspace 目錄中
	IsolationWorktree IsolationMode = "worktree" // 在 Task Worktree 中
)

// IsolationPolicy 是 Workspace 對自動選擇的覆寫。
type IsolationPolicy string

const (
	PolicyAuto   IsolationPolicy = "auto"
	PolicyAlways IsolationPolicy = "always"
	PolicyNever  IsolationPolicy = "never"
)

// Worktree 定位一個 git worktree：Task Worktree，或從
// Task 工作目錄分支出來的 Fork。
type Worktree struct {
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"baseBranch"` // 合併回去的分支
	BaseCommit string `json:"baseCommit"`
}

// WorktreeState 是 Worktree 加上它的結局。
type WorktreeState struct {
	Worktree
	Outcome Outcome `json:"outcome"`
}

type Outcome string

const (
	OutcomeOpen      Outcome = "open"
	OutcomeMerged    Outcome = "merged"
	OutcomeDiscarded Outcome = "discarded"
)

// Target 指明合併或捨棄所套用的 worktree：Session 的
// Fork，或 Task Worktree。
type Target struct {
	Level   TargetLevel `json:"level"`
	Session SessionID   `json:"session,omitzero"` // 僅限 Fork
}

type TargetLevel string

const (
	TargetFork     TargetLevel = "fork"     // 合併進 Task 的工作目錄
	TargetWorktree TargetLevel = "worktree" // 合併進 Workspace 的基礎分支
)

type MergeStrategy string

const (
	MergeSquash      MergeStrategy = "squash" // 預設
	MergePullRequest MergeStrategy = "pull_request"
)

// Resolution 是 desktop 在關閉 Session 或完成 Task 時，
// 對未合併變更所選擇的處置方式。
type Resolution struct {
	Action   IntentKind    `json:"action"` // 合併或捨棄
	Strategy MergeStrategy `json:"strategy,omitzero"`
	Message  string        `json:"message,omitzero"`
}

// TargetResolution 是 SetStatus 完成 Task 時對某個未解決 worktree 的處置。
type TargetResolution struct {
	Target Target `json:"target"`
	Resolution
}

// Repo 描述 hub 檢查到的 Workspace 目錄狀態。
type Repo struct {
	IsGit  bool   `json:"isGit"`
	Branch string `json:"branch,omitzero"`
	Commit string `json:"commit,omitzero"`
	Dirty  bool   `json:"dirty"`
	Root   string `json:"root,omitzero"` // git 工作樹的根（真實路徑）；非 git 為空
}
