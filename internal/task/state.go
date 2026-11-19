package task

import "time"

// State 是 Task——唯一的聚合——由 Evolve 從其 Stream 摺疊而成。
//
// Sessions 存值而非指針，所以 s.Sessions[id].Status = x 無法編譯，
// 修改一律是取出、改、放回。map 與 slice 仍與舊 State 共用底層，
// 所以 Evolve 修改前先複製被改的 map 與 slice（copy-on-write），
// 指針欄位（Worktree、Review、Session.Turn、Session.Fork、Session.Exit）整個替換、不就地修改。
type State struct {
	ID            TaskID                          `json:"id"`
	Version       Seq                             `json:"version"`   // 最後一個被摺疊事件的 Seq
	UpdatedAt     time.Time                       `json:"updatedAt"` // 最後一個事件的 At；置頂與取消置頂不算（只改側欄，D109）
	Title         string                          `json:"title"`
	Brief         string                          `json:"brief"`
	Workspace     WorkspaceID                     `json:"workspace"`
	Isolation     IsolationMode                   `json:"isolation"` // 建立時固定
	Worktree      *WorktreeState                  `json:"worktree"`  // Task Worktree；Isolation 為 none 時為 nil
	Status        TaskStatus                      `json:"status"`
	Archived      bool                            `json:"archived"`          // 與 Status 正交，因此取消封存會還原它
	PinnedAt      time.Time                       `json:"pinnedAt,omitzero"` // 置頂的時間（TaskPinned 的 At）；零值 = 未置頂（D109）。與 Archived 正交
	Priority      TaskPriority                    `json:"priority"`          // 建立時為 none（D135）
	Labels        []LabelID                       `json:"labels"`            // 按 ID 排序、不重複；目錄裡刪掉的標籤可能還留著，由讀端略過（D135）
	Sessions      map[SessionID]Session           `json:"sessions"`
	Order         []SessionID                     `json:"order"` // 建立順序
	Rules         map[RuleID]Rule                 `json:"rules"` // session 與 task 範圍的規則
	Confirmations map[ConfirmationID]Confirmation `json:"confirmations"`
	Intents       map[IntentID]Intent             `json:"intents"`
	Review        *Review                         `json:"review"` // 目前進行中的 fan-out 審查（若有）
	Failed        map[EffectKey]Suspension        `json:"failed"` // 失敗而掛起的 Effect，直到 EffectRetried
}

// Suspension 是 State.Failed 裡一個掛起的 Effect：失敗的原因，與失敗的時間
// （EffectFailed 事件的 At，重放得到同一個值）；“需要處理”按它排序（D78）。
type Suspension struct {
	Problem Problem   `json:"problem"`
	At      time.Time `json:"at"`
}

// TaskStatus 會隨 session 活動自動推進；使用者可以
// 覆寫它，之後自動規則仍會繼續套用。
type TaskStatus string

const (
	TaskTodo       TaskStatus = "todo"
	TaskInProgress TaskStatus = "in_progress"
	TaskInReview   TaskStatus = "in_review"
	TaskDone       TaskStatus = "done" // 要求每個 Fork 與 Task Worktree 都已解決
)

// TaskPriority 是使用者給 Task 的優先級（D135），照 Arc 看板的五級；只用來整理，不影響任何規則。
type TaskPriority string

const (
	PriorityNone   TaskPriority = "none"
	PriorityLow    TaskPriority = "low"
	PriorityMedium TaskPriority = "medium"
	PriorityHigh   TaskPriority = "high"
	PriorityUrgent TaskPriority = "urgent"
)

// StatusCause 記錄狀態變更是自動的還是使用者的。
type StatusCause string

const (
	CauseAuto StatusCause = "auto"
	CauseUser StatusCause = "user"
)

// Confirmation 是來自遠端客戶端、等待 desktop 核准的
// 高風險命令。它永不過期。
type Confirmation struct {
	ID          ConfirmationID `json:"id"`
	Command     Command        `json:"command"` // 核准後視同來自 desktop 執行
	RequestedBy DeviceID       `json:"requestedBy"`
	RequestedAt time.Time      `json:"requestedAt"`
}

// Intent 是已決定但其副作用尚未完成的操作。Plan 將每個
// Intent 轉為 Effect，直到結果事件將其移除，
// 因此這項工作能撐過當機。
type Intent struct {
	ID        IntentID      `json:"id"`
	Kind      IntentKind    `json:"kind"`
	Target    Target        `json:"target"`
	Strategy  MergeStrategy `json:"strategy,omitzero"` // 僅限合併
	Message   string        `json:"message,omitzero"`  // 提交訊息或 PR 標題，僅限合併
	Conflicts []string      `json:"conflicts"`         // 上一次衝突嘗試涉及的檔案
}

type IntentKind string

const (
	IntentMerge        IntentKind = "merge"
	IntentDiscard      IntentKind = "discard"
	IntentDeleteNative IntentKind = "delete_native" // Target.Session 的原生 session 檔案
)

// Review 是在 fan-out 之後推薦 Fork 的審查者 session。
type Review struct {
	Launch      *LaunchSnapshot `json:"launch"`            // 審查者的啟動設定；nil = 手動選擇
	Reviewer    SessionID       `json:"reviewer,omitzero"` // 審查者 session；review_prepared 之後才建立
	Candidates  []SessionID     `json:"candidates"`
	Recommended SessionID       `json:"recommended,omitzero"`
	Rationale   string          `json:"rationale,omitzero"`
}

// OpenWorktrees 回報 Task 還有沒有開著的 worktree（Task Worktree 或 Fork 的 Outcome 為 open），
// 不管有沒有合併 / 丟棄意圖在執行：刪除 Task 時它們的目錄會成為沒人管的孤兒（m2.md DL10）。
func (s State) OpenWorktrees() bool {
	if s.Worktree != nil && s.Worktree.Outcome == OutcomeOpen {
		return true
	}
	for _, x := range s.Sessions {
		if x.openFork() {
			return true
		}
	}
	return false
}
