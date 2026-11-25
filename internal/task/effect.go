package task

import (
	"strings"
	"time"
)

// Effect 是 Plan 要求 hub 執行的副作用。Effect 具有冪等性；
// hub 在 actor 之外執行它們，並將結果以執行期 Command 回報。
//
// 與 Command、Event 同為扁平結構：Kind 選出被設定的那個 payload。
type Effect struct {
	Key     EffectKey  `json:"key"`
	Kind    EffectKind `json:"kind"`
	Session SessionID  `json:"session,omitzero"`

	StartConn        *StartConn        `json:"startConn,omitzero"`
	SendPrompt       *Prompt           `json:"sendPrompt,omitzero"`
	AnswerPermission *AnswerPermission `json:"answerPermission,omitzero"`
	ScheduleTimer    *ScheduleTimer    `json:"scheduleTimer,omitzero"`
	CreateWorktree   *CreateWorktree   `json:"createWorktree,omitzero"`
	MergeWorktree    *MergeWorktree    `json:"mergeWorktree,omitzero"`
	DiscardWorktree  *DiscardWorktree  `json:"discardWorktree,omitzero"`
	DeleteNative     *DeleteNative     `json:"deleteNative,omitzero"`
	PrepareReview    *PrepareReview    `json:"prepareReview,omitzero"`
	SetModel         *ModelSwitch      `json:"setModel,omitzero"`
}

// EffectKey 以確定性方式識別一個 Effect："<EffectKind>:<對象>"（例如
// "start_conn:s3"，格式見 docs/architecture/domain.md），在 Task 內唯一，
// 讓 hub 能判斷它是否已在執行中、Plan 能跳過已失敗而掛起的 Effect。
type EffectKey string

// Kind 取出 EffectKey 的 EffectKind（":" 之前的部分；prepare_review 沒有對象）。
func (k EffectKey) Kind() EffectKind {
	kind, _, _ := strings.Cut(string(k), ":")
	return EffectKind(kind)
}

// newEffectKey 組出 EffectKey：kind 加對象；沒有對象時只有 kind。
func newEffectKey(kind EffectKind, object string) EffectKey {
	if object == "" {
		return EffectKey(kind)
	}
	return EffectKey(string(kind) + ":" + object)
}

// effectKey 是這個意圖對應的 Effect 的 Key（merge_worktree:i21 等）。
func (in Intent) effectKey() EffectKey {
	var kind EffectKind
	switch in.Kind {
	case IntentMerge:
		kind = EffMergeWorktree
	case IntentDiscard:
		kind = EffDiscardWorktree
	case IntentDeleteNative:
		kind = EffDeleteNative
	}
	return newEffectKey(kind, string(in.ID))
}

type EffectKind string

const (
	EffStartConn        EffectKind = "start_conn"
	EffSendPrompt       EffectKind = "send_prompt"
	EffAnswerPermission EffectKind = "answer_permission"
	EffCancelTurn       EffectKind = "cancel_turn"
	EffCloseConn        EffectKind = "close_conn"
	EffReportExited     EffectKind = "report_exited" // 綁定的行程已不在：回合是交給它的，或原生會話接不回來
	EffScheduleTimer    EffectKind = "schedule_timer"
	EffCreateWorktree   EffectKind = "create_worktree"
	EffMergeWorktree    EffectKind = "merge_worktree"
	EffDiscardWorktree  EffectKind = "discard_worktree"
	EffDeleteNative     EffectKind = "delete_native"
	EffSetModel         EffectKind = "set_model"      // 把模型設成 Session.ModelWanted（D118），結果為 model_reported
	EffPrepareReview    EffectKind = "prepare_review" // 取各 Fork 的 diff 存成 blob，結果為 review_prepared
)

type StartConn struct {
	Launch  LaunchSnapshot  `json:"launch"`
	Workdir string          `json:"workdir"`
	Resume  NativeSessionID `json:"resume,omitzero"`
}

// ModelSwitch 是要 agent 換成的模型：Option 是它報告的設置 ID。
type ModelSwitch struct {
	Option string `json:"option"`
	Value  string `json:"value"`
}

type AnswerPermission struct {
	Permission PermissionID `json:"permission"`
	Verdict    Verdict      `json:"verdict"`
}

type ScheduleTimer struct {
	Permission PermissionID `json:"permission"`
	At         time.Time    `json:"at"`
}

// CreateWorktree 建立 Task Worktree（Session 為空）或 Fork。
type CreateWorktree struct {
	Repo     string   `json:"repo"` // 分支出來的目錄
	Worktree Worktree `json:"worktree"`
}

type MergeWorktree struct {
	Intent   IntentID      `json:"intent"`
	From     Worktree      `json:"from"`
	Into     string        `json:"into"` // 其 BaseBranch 接收合併的目錄
	Strategy MergeStrategy `json:"strategy"`
	Message  string        `json:"message"`
}

type DiscardWorktree struct {
	Intent   IntentID `json:"intent"`
	Worktree Worktree `json:"worktree"`
	Repo     string   `json:"repo"` // worktree 所屬倉庫的目錄，同 MergeWorktree.Into：worktree 已不在時仍能刪它的分支
}

type DeleteNative struct {
	Intent   IntentID        `json:"intent"`
	Protocol Protocol        `json:"protocol"`
	NativeID NativeSessionID `json:"nativeId"`
	Workdir  string          `json:"workdir"`
}

// PrepareReview 要 hub 取各候選 Fork 相對其 BaseCommit 的 diff。
type PrepareReview struct {
	Candidates []ReviewCandidate `json:"candidates"`
}

type ReviewCandidate struct {
	Session  SessionID `json:"session"`
	Worktree Worktree  `json:"worktree"`
}

// Live 是 hub 所觀察到實際執行中的狀態；Plan 將其與
// State 所要求的相比較。
type Live struct {
	WorkspaceDir string // Workspace 的目錄：none 隔離的工作目錄、建 Task Worktree 的 repo、Task Worktree 合回的目標
	Conns        map[SessionID]ConnInfo
	Worktrees    map[string]bool // 現有 Task Worktree 與 Fork 的路徑
	Timers       map[AskKey]bool
	InFlight     map[EffectKey]bool // 已發出但尚未回報的 effect
	// Lost 是日誌記著綁定（Session.Bound）、行程卻已不在的 Session（D117）：hub 重啟後 actor 載入時就記著的，
	// 或 hub 自己關掉了連線（歸檔）。值是那時的回合（沒有回合時為空）：那一輪交給了不在的行程，按 D2 失敗；
	// 之後才開始的回合照“還沒啟動”以 Resume 啟動。新的連線登記或 ProcessExited 時移除。
	Lost map[SessionID]TurnID
}

type ConnInfo struct {
	NativeID NativeSessionID
	// Turn 是此連線最後送出 Prompt 的回合，由 hub 在 Conn.Prompt 成功後
	// 設定；Plan 只在它不是目前的回合時送出 send_prompt。它與連線同生
	// 同滅，所以不落入日誌：連線消失後那一輪走 report_exited，不會重送。
	Turn TurnID
	// Cancelled 是已對此連線送出取消的回合，由 hub 在 Conn.Cancel 成功後
	// 設定；Plan 不再為它送 cancel_turn，等 agent 回報這一輪的結局。
	Cancelled TurnID
}
