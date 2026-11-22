package task

import "time"

// Event 是 Task Stream 中不可變的事實。已儲存的事件永不改變，
// 唯一的例外是 RedactMessage 會將一個 MessageAppended payload
// 替換為遮蔽標記。
//
// 採用扁平結構以便 mygo 為其產生 TypeScript：Kind 選出
// 被設定的那一個 payload。Session 在每個 session 層級事件上都會設定。
//
// 演進規則：欄位只能新增且必須為可選；重新命名或
// 移除欄位需要在編解碼器中加入 upcaster，
// 列於領域架構文件中。
type Event struct {
	Seq     Seq       `json:"seq"`
	At      time.Time `json:"at"`
	Kind    EventKind `json:"kind"`
	Session SessionID `json:"session,omitzero"`

	// Task。
	TaskCreated       *TaskCreated       `json:"taskCreated,omitzero"`
	TaskEdited        *EditTask          `json:"taskEdited,omitzero"`
	TaskStatusChanged *TaskStatusChanged `json:"taskStatusChanged,omitzero"`
	TaskPrioritySet   *SetPriority       `json:"taskPrioritySet,omitzero"`
	TaskLabelsSet     *SetLabels         `json:"taskLabelsSet,omitzero"` // 已排序、不重複

	// 刪除。
	MessageRedacted      *RedactMessage `json:"messageRedacted,omitzero"`
	NativeSessionDeleted *Intent        `json:"nativeSessionDeleted,omitzero"`
	NativeFilesRemoved   *IntentRef     `json:"nativeFilesRemoved,omitzero"`

	// 確認。
	ConfirmationRequested *Confirmation    `json:"confirmationRequested,omitzero"`
	ConfirmationGranted   *ConfirmationRef `json:"confirmationGranted,omitzero"`
	ConfirmationCancelled *ConfirmationRef `json:"confirmationCancelled,omitzero"`

	// 審查。
	ReviewRequested *ReviewRequested `json:"reviewRequested,omitzero"`
	ForkRecommended *Recommendation  `json:"forkRecommended,omitzero"`

	// Session 生命週期。
	SessionCreated     *SessionCreated `json:"sessionCreated,omitzero"`
	NativeSessionBound *NativeBound    `json:"nativeSessionBound,omitzero"`
	SessionResumed     *SessionResumed `json:"sessionResumed,omitzero"`
	ProcessExited      *Exited         `json:"processExited,omitzero"`

	// 模型（D118）。
	ModelChosen       *SetModel     `json:"modelChosen,omitzero"`
	ModelReported     *ModelReport  `json:"modelReported,omitzero"`
	ModelSwitchFailed *ModelProblem `json:"modelSwitchFailed,omitzero"`

	// 回合。
	PromptQueued *Prompt      `json:"promptQueued,omitzero"`
	TurnStarted  *TurnStarted `json:"turnStarted,omitzero"`
	TurnFailed   *TurnFailed  `json:"turnFailed,omitzero"`

	// 內容。
	MessageAppended *MessageAppended `json:"messageAppended,omitzero"`
	ToolCallStarted *ToolCall        `json:"toolCallStarted,omitzero"`
	ToolCallUpdated *ToolUpdate      `json:"toolCallUpdated,omitzero"`
	PlanUpdated     *PlanUpdate      `json:"planUpdated,omitzero"`
	UsageReported   *Usage           `json:"usageReported,omitzero"`

	// 權限。
	PermissionAsked     *Ask               `json:"permissionAsked,omitzero"`
	PermissionDecided   *PermissionDecided `json:"permissionDecided,omitzero"`
	PermissionAnswered  *PermissionRef     `json:"permissionAnswered,omitzero"`
	PermissionCancelled *PermissionRef     `json:"permissionCancelled,omitzero"`
	RuleAdded           *Rule              `json:"ruleAdded,omitzero"`

	// 隔離。
	WorktreeAttached *Worktree      `json:"worktreeAttached,omitzero"` // Task Worktree
	ForkCreated      *Worktree      `json:"forkCreated,omitzero"`
	MergeRequested   *Intent        `json:"mergeRequested,omitzero"`
	DiscardRequested *Intent        `json:"discardRequested,omitzero"`
	IntentCancelled  *IntentRef     `json:"intentCancelled,omitzero"`
	Merged           *IntentResult  `json:"merged,omitzero"` // ForkMerged、WorktreeMerged
	Discarded        *IntentRef     `json:"discarded,omitzero"`
	MergeConflicted  *MergeConflict `json:"mergeConflicted,omitzero"`
	WorktreeKept     *Target        `json:"worktreeKept,omitzero"`

	// Effect。
	EffectFailed  *EffectFailure `json:"effectFailed,omitzero"`
	EffectRetried *RetryEffect   `json:"effectRetried,omitzero"`
}

type EventKind string

const (
	// Task。
	TaskCreatedKind       EventKind = "TaskCreated"
	TaskEditedKind        EventKind = "TaskEdited"
	TaskStatusChangedKind EventKind = "TaskStatusChanged"
	TaskArchivedKind      EventKind = "TaskArchived"
	TaskUnarchivedKind    EventKind = "TaskUnarchived"
	TaskPinnedKind        EventKind = "TaskPinned" // 置頂的時間是事件的 At（D109）
	TaskUnpinnedKind      EventKind = "TaskUnpinned"
	TaskPrioritySetKind   EventKind = "TaskPrioritySet" // D135
	TaskLabelsSetKind     EventKind = "TaskLabelsSet"   // D135

	// 刪除。刪除整個 Task 不會產生事件：其 Stream 會被移除。
	MessageRedactedKind      EventKind = "MessageRedacted"
	NativeSessionDeletedKind EventKind = "NativeSessionDeleted" // 設定 NativeGone 並記錄 intent
	NativeFilesRemovedKind   EventKind = "NativeFilesRemoved"   // intent 已完成

	// 確認。
	ConfirmationRequestedKind EventKind = "ConfirmationRequested"
	ConfirmationGrantedKind   EventKind = "ConfirmationGranted"
	ConfirmationCancelledKind EventKind = "ConfirmationCancelled"

	// 審查。
	ReviewRequestedKind EventKind = "ReviewRequested"
	ForkRecommendedKind EventKind = "ForkRecommended"
	ReviewSkippedKind   EventKind = "ReviewSkipped" // 改為手動選擇：Launch 置空、清掉 prepare_review 的掛起
	ReviewEndedKind     EventKind = "ReviewEnded"   // ChooseFork 之後清掉 Review

	// Session 生命週期。
	SessionCreatedKind     EventKind = "SessionCreated"
	NativeSessionBoundKind EventKind = "NativeSessionBound"
	SessionResumedKind     EventKind = "SessionResumed"
	SessionClosedKind      EventKind = "SessionClosed"
	ProcessExitedKind      EventKind = "ProcessExited"

	// 模型（D118）。
	ModelChosenKind       EventKind = "ModelChosen"       // 使用者選的模型：設 ModelWanted、清掉 ModelProblem
	ModelReportedKind     EventKind = "ModelReported"     // agent 報告的模型設置：設 Model；不是 Start 的報告與 ModelWanted 不同時跟著 agent
	ModelSwitchFailedKind EventKind = "ModelSwitchFailed" // 沒能換成：清掉 ModelWanted，設 ModelProblem

	// 回合。
	PromptQueuedKind        EventKind = "PromptQueued"
	TurnStartedKind         EventKind = "TurnStarted"
	TurnCancelRequestedKind EventKind = "TurnCancelRequested"
	TurnCompletedKind       EventKind = "TurnCompleted"
	TurnCancelledKind       EventKind = "TurnCancelled"
	TurnFailedKind          EventKind = "TurnFailed"

	// 內容。
	MessageAppendedKind EventKind = "MessageAppended"
	ToolCallStartedKind EventKind = "ToolCallStarted"
	ToolCallUpdatedKind EventKind = "ToolCallUpdated"
	PlanUpdatedKind     EventKind = "PlanUpdated"
	UsageReportedKind   EventKind = "UsageReported"

	// 權限。
	PermissionAskedKind     EventKind = "PermissionAsked"
	PermissionDecidedKind   EventKind = "PermissionDecided"
	PermissionAnsweredKind  EventKind = "PermissionAnswered"
	PermissionCancelledKind EventKind = "PermissionCancelled"
	RuleAddedKind           EventKind = "RuleAdded"

	// 隔離。
	WorktreeAttachedKind  EventKind = "WorktreeAttached"
	ForkCreatedKind       EventKind = "ForkCreated"
	MergeRequestedKind    EventKind = "MergeRequested"
	DiscardRequestedKind  EventKind = "DiscardRequested"
	IntentCancelledKind   EventKind = "IntentCancelled"
	ForkMergedKind        EventKind = "ForkMerged"
	ForkDiscardedKind     EventKind = "ForkDiscarded"
	WorktreeMergedKind    EventKind = "WorktreeMerged"
	WorktreeDiscardedKind EventKind = "WorktreeDiscarded"
	MergeConflictedKind   EventKind = "MergeConflicted"
	WorktreeKeptKind      EventKind = "WorktreeKept"

	// 衝突。
	SessionConflictedKind EventKind = "SessionConflicted"
	SessionTakenOverKind  EventKind = "SessionTakenOver"

	// Effect。
	EffectFailedKind  EventKind = "EffectFailed" // 掛起：Plan 跳過它，直到 EffectRetried
	EffectRetriedKind EventKind = "EffectRetried"
)

// 未與命令共用的 Event payload。

type TaskCreated struct {
	Title     string        `json:"title"`
	Brief     string        `json:"brief"`
	Workspace WorkspaceID   `json:"workspace"`
	Isolation IsolationMode `json:"isolation"` // 由 Inputs 決定；若為 worktree 則接著發出 WorktreeAttached
}

type TaskStatusChanged struct {
	From  TaskStatus  `json:"from"`
	To    TaskStatus  `json:"to"`
	Cause StatusCause `json:"cause"`
}

type ReviewRequested struct {
	Candidates []SessionID     `json:"candidates"` // Fork session
	Launch     *LaunchSnapshot `json:"launch"`     // 當時的 Inputs.Reviewer；nil = 沒有可用的審查者，由使用者手動選擇
}

type SessionCreated struct {
	Launch LaunchSnapshot `json:"launch"`
	Role   Role           `json:"role"`
}

// SessionResumed 是 Resume 的結果（結構版本 2：版本 1 沒有載荷，upcaster 補成空的）。Requeued 是重新排在隊首的
// Prompt：行程還沒綁定原生會話（agent 沒起來，或接回時起不來）就失敗的那一輪的 Prompt，它從沒送到 agent
// （m2.md L56）；ID 是新的。
type SessionResumed struct {
	Requeued *Prompt `json:"requeued,omitzero"`
}

type TurnStarted struct {
	Turn   TurnID   `json:"turn"`
	Prompt PromptID `json:"prompt"`
}

type TurnFailed struct {
	Turn    TurnID  `json:"turn"`
	Problem Problem `json:"problem"`
}

// MessageAppended 是一則訊息的一個片段；具有相同 Message ID 的
// 連續片段構成一則訊息，最後一個片段的 Partial 為 false。
type MessageAppended struct {
	Message  MessageID   `json:"message"`
	Turn     TurnID      `json:"turn"`
	Channel  ChannelKind `json:"channel"`
	Text     string      `json:"text"`
	Partial  bool        `json:"partial"`
	Redacted bool        `json:"redacted"` // 遮蔽標記：Text 為空
}

type PermissionDecided struct {
	Permission PermissionID `json:"permission"`
	Decision   Decision     `json:"decision"`
}

type IntentRef struct {
	Intent IntentID `json:"intent"`
}
