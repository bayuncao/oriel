package task

// Command 是 Decide 的所有輸入：客戶端的意圖（Origin 為 desktop 或
// remote）或執行期事實（Origin 為 runtime）：agent 輸出、權限
// 請求、計時器到期、行程結束與 effect 結果。
//
// 採用扁平結構以便 mygo 為其產生 TypeScript：Kind 選出
// 被設定的那一個 payload；其他 payload 皆為 nil。沒有
// payload 的 Kind 只使用 Session。
type Command struct {
	Kind    CommandKind `json:"kind"`
	Origin  Origin      `json:"origin"`           // 由 hub 填入，絕不由客戶端填入
	Device  DeviceID    `json:"device,omitzero"`  // 配對的裝置，用於 OriginRemote
	Session SessionID   `json:"session,omitzero"` // 用於 session 層級的 kind

	// 客戶端命令。
	CreateTask         *CreateTask       `json:"createTask,omitzero"`
	EditTask           *EditTask         `json:"editTask,omitzero"`
	SetStatus          *SetStatus        `json:"setStatus,omitzero"`
	SetPriority        *SetPriority      `json:"setPriority,omitzero"`
	SetLabels          *SetLabels        `json:"setLabels,omitzero"`
	CreateSession      *CreateSession    `json:"createSession,omitzero"`
	Submit             *Submit           `json:"submit,omitzero"`
	CloseSession       *CloseSession     `json:"closeSession,omitzero"`
	DecidePermission   *DecidePermission `json:"decidePermission,omitzero"`
	FanOut             *FanOut           `json:"fanOut,omitzero"`
	ChooseFork         *ChooseFork       `json:"chooseFork,omitzero"`
	Merge              *Merge            `json:"merge,omitzero"`
	Discard            *Discard          `json:"discard,omitzero"`
	RedactMessage      *RedactMessage    `json:"redactMessage,omitzero"`
	Confirm            *ConfirmationRef  `json:"confirm,omitzero"`
	CancelConfirmation *ConfirmationRef  `json:"cancelConfirmation,omitzero"`
	CancelIntent       *IntentRef        `json:"cancelIntent,omitzero"` // 撤銷已掛起的意圖
	RetryEffect        *RetryEffect      `json:"retryEffect,omitzero"`
	SetModel           *SetModel         `json:"setModel,omitzero"`

	// 執行期事實。
	Bound          *NativeBound    `json:"bound,omitzero"`
	Output         *Output         `json:"output,omitzero"`
	ToolStarted    *ToolCall       `json:"toolStarted,omitzero"`
	ToolUpdated    *ToolUpdate     `json:"toolUpdated,omitzero"`
	Plan           *PlanUpdate     `json:"plan,omitzero"`
	Usage          *Usage          `json:"usage,omitzero"`
	Asked          *Asked          `json:"asked,omitzero"`
	TurnEnded      *TurnEnded      `json:"turnEnded,omitzero"`
	Exited         *Exited         `json:"exited,omitzero"`
	AskExpired     *PermissionRef  `json:"askExpired,omitzero"`
	Answered       *PermissionRef  `json:"answered,omitzero"`
	Merged         *IntentResult   `json:"merged,omitzero"`
	MergeConflict  *MergeConflict  `json:"mergeConflict,omitzero"`
	Discarded      *IntentResult   `json:"discarded,omitzero"`
	NativeDeleted  *IntentResult   `json:"nativeDeleted,omitzero"`
	Recommendation *Recommendation `json:"recommendation,omitzero"`
	ReviewPrepared *ReviewPrepared `json:"reviewPrepared,omitzero"`
	EffectFailed   *EffectFailure  `json:"effectFailed,omitzero"`
	ModelReported  *ModelReport    `json:"modelReported,omitzero"`
}

type CommandKind string

// 客戶端命令種類。
const (
	CmdCreateTask          CommandKind = "create_task"
	CmdEditTask            CommandKind = "edit_task"
	CmdSetStatus           CommandKind = "set_status"
	CmdArchive             CommandKind = "archive"
	CmdUnarchive           CommandKind = "unarchive"
	CmdPin                 CommandKind = "pin"          // 置頂（D109）；已置頂時不產生事件
	CmdUnpin               CommandKind = "unpin"        // 取消置頂；未置頂時不產生事件
	CmdSetPriority         CommandKind = "set_priority" // 優先級（D135）；相同時不產生事件
	CmdSetLabels           CommandKind = "set_labels"   // 標籤的完整集合（D135）；相同時不產生事件
	CmdCreateSession       CommandKind = "create_session"
	CmdSubmit              CommandKind = "submit"
	CmdCancelTurn          CommandKind = "cancel_turn"
	CmdResume              CommandKind = "resume"
	CmdCloseSession        CommandKind = "close_session"
	CmdTakeOver            CommandKind = "take_over"
	CmdDecidePermission    CommandKind = "decide_permission"
	CmdFanOut              CommandKind = "fan_out"
	CmdChooseFork          CommandKind = "choose_fork"
	CmdSkipReview          CommandKind = "skip_review"
	CmdMerge               CommandKind = "merge"
	CmdDiscard             CommandKind = "discard"
	CmdRedactMessage       CommandKind = "redact_message"
	CmdDeleteNativeSession CommandKind = "delete_native_session"
	CmdConfirm             CommandKind = "confirm"
	CmdCancelConfirmation  CommandKind = "cancel_confirmation"
	CmdCancelIntent        CommandKind = "cancel_intent" // 撤銷已掛起的意圖
	CmdRetryEffect         CommandKind = "retry_effect"  // 重試失敗而掛起的 Effect
	CmdSetModel            CommandKind = "set_model"     // 換 Session 的模型（D118）
)

// 執行期事實種類；除非 Origin 為 runtime，否則 Decide 會拒絕它們。
const (
	FactNativeBound    CommandKind = "native_bound"
	FactOutput         CommandKind = "output"
	FactToolStarted    CommandKind = "tool_started"
	FactToolUpdated    CommandKind = "tool_updated"
	FactPlan           CommandKind = "plan"
	FactUsage          CommandKind = "usage"
	FactAsked          CommandKind = "asked"
	FactTurnEnded      CommandKind = "turn_ended"
	FactExited         CommandKind = "exited"
	FactAskExpired     CommandKind = "ask_expired"
	FactAnswered       CommandKind = "answered"
	FactConflicted     CommandKind = "conflicted" // 原生 session 在 OrielHub 之外被使用
	FactMerged         CommandKind = "merged"
	FactMergeConflict  CommandKind = "merge_conflict"
	FactDiscarded      CommandKind = "discarded"
	FactNativeDeleted  CommandKind = "native_deleted"
	FactRecommendation CommandKind = "recommendation"
	FactReviewPrepared CommandKind = "review_prepared"
	FactEffectFailed   CommandKind = "effect_failed"
	FactModelReported  CommandKind = "model_reported" // agent 報告的模型設置（D118）
)

// 客戶端命令的 payload。

type CreateTask struct {
	Title     string      `json:"title"`
	Brief     string      `json:"brief"`
	Workspace WorkspaceID `json:"workspace"`
	// Launches 立即以 Brief 作為第一個 prompt 啟動 session；
	// 多於一個則會 fan-out。留空則 Task 維持在 todo。
	Launches []LaunchSnapshot `json:"launches"`
}

type EditTask struct {
	Title string `json:"title"`
	Brief string `json:"brief"`
}

type SetStatus struct {
	Status      TaskStatus         `json:"status"`
	Resolutions []TargetResolution `json:"resolutions"` // 用於 done：每個未解決的 worktree 一筆，僅限 desktop
}

// SetPriority 設定 Task 的優先級。
type SetPriority struct {
	Priority TaskPriority `json:"priority"`
}

// SetLabels 以完整的集合取代 Task 的標籤；每個都要在標籤目錄裡（Inputs.Labels）。
type SetLabels struct {
	Labels []LabelID `json:"labels"`
}

type CreateSession struct {
	Launch LaunchSnapshot `json:"launch"`
	Prompt *Prompt        `json:"prompt"` // 可選的第一個 prompt
}

type Submit struct {
	Prompt Prompt `json:"prompt"`
}

type CloseSession struct {
	Resolution *Resolution `json:"resolution"` // 當其 Fork 有變更時，desktop 必須提供
}

type DecidePermission struct {
	Permission PermissionID `json:"permission"`
	Verdict    Verdict      `json:"verdict"`
	Always     *Rule        `json:"always"` // 同時儲存此規則；ID 由 Decide 或 hub 指派
}

type FanOut struct {
	Prompt   Prompt           `json:"prompt"`
	Launches []LaunchSnapshot `json:"launches"` // 各建立一個 Fork 與 Session
}

type ChooseFork struct {
	Winner   SessionID     `json:"winner"`
	Strategy MergeStrategy `json:"strategy,omitzero"`
	Message  string        `json:"message,omitzero"`
}

type Merge struct {
	Target   Target        `json:"target"`
	Strategy MergeStrategy `json:"strategy"`
	Message  string        `json:"message"`
}

type Discard struct {
	Target Target `json:"target"`
}

type RedactMessage struct {
	Message MessageID `json:"message"`
}

type ConfirmationRef struct {
	Confirmation ConfirmationID `json:"confirmation"`
}

type RetryEffect struct {
	Effect EffectKey `json:"effect"`
}

// SetModel 選一個 agent 報告過的模型（Session.Model 的 Choices 之一，D118）。
type SetModel struct {
	Value string `json:"value"`
}

// 執行期事實的 payload。

type NativeBound struct {
	NativeID NativeSessionID `json:"nativeId"`
}

// Output 是由 hub 沖寫的一段 agent 文字（在工具呼叫之前、
// 約每五秒一次，以及在回合結束時）。
type Output struct {
	Channel ChannelKind `json:"channel"`
	Text    string      `json:"text"`
	Partial bool        `json:"partial"` // 此訊息後續還有更多片段
}

type ToolUpdate struct {
	ID     ToolCallID `json:"id"`
	Status ToolStatus `json:"status"`
	// Kind、Title、Paths 是工具呼叫目前的分類、標題與路徑：agent 可能在開始之後才給出或改掉它們
	// （ACP 的 tool_call_update）。空的（Paths 為 nil）沿用之前的。
	Kind   ToolKind `json:"kind,omitzero"`
	Title  string   `json:"title,omitzero"`
	Paths  []string `json:"paths,omitzero"`
	Output string   `json:"output,omitzero"`
	Blob   BlobRef  `json:"blob,omitzero"` // 過長的輸出或 diff
}

type PlanUpdate struct {
	Entries []PlanEntry `json:"entries"`
}

type Asked struct {
	ID      PermissionID      `json:"id"`
	Request PermissionRequest `json:"request"`
}

type TurnEnded struct {
	Result  TurnResult `json:"result"`
	Problem *Problem   `json:"problem,omitzero"` // 僅限 TurnError
}

type TurnResult string

const (
	TurnOK        TurnResult = "completed"
	TurnCancelled TurnResult = "cancelled"
	TurnError     TurnResult = "failed"
)

// Exited 是 agent 行程的結束：Problem 是 proc.exited {status} 或
// proc.signaled {signal}；StderrTail 是 stderr 的結尾，原樣顯示。
type Exited struct {
	Problem    Problem `json:"problem"`
	StderrTail string  `json:"stderrTail"`
}

type PermissionRef struct {
	Permission PermissionID `json:"permission"`
}

type IntentResult struct {
	Intent IntentID `json:"intent"`
	URL    string   `json:"url,omitzero"` // pull request，用於 MergePullRequest
}

type MergeConflict struct {
	Intent IntentID `json:"intent"`
	Files  []string `json:"files"`
}

// Recommendation 是審查者的判定，由 hub 從
// 審查 session 的最後一則訊息中擷取。
type Recommendation struct {
	Winner    SessionID `json:"winner"`
	Rationale string    `json:"rationale"`
}

// ReviewPrepared 是 prepare_review 的結果：每個候選 Fork 的 diff，
// 一律以 blob 附件給出，成為審查者 Prompt 的附件。
type ReviewPrepared struct {
	Diffs []ForkDiff `json:"diffs"`
}

type ForkDiff struct {
	Session SessionID  `json:"session"`
	Diff    Attachment `json:"diff"`
}

// ModelReport 是 agent 報告的模型設置：Start 是建立或接回會話時的回應，否則是 agent 之後改了（或切換的結果）。
// Setting 為 nil 表示 agent 沒有模型設置。
type ModelReport struct {
	Setting *ModelSetting `json:"setting"`
	Start   bool          `json:"start"`
}

// EffectFailure 是 Effect 的失敗：作為事實回報，也是 EffectFailed
// 事件的 payload（把非連線類的 Effect 掛起，等待 RetryEffect）。
type EffectFailure struct {
	Effect  EffectKey `json:"effect"`
	Problem Problem   `json:"problem"`
}
