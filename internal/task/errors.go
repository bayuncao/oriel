package task

// Rejection 是當 Command 違反規則時 Decide 回傳的錯誤。
type Rejection struct {
	Reason Reason `json:"reason"`
	Detail string `json:"detail,omitzero"`
}

func (r *Rejection) Error() string {
	if r.Detail == "" {
		return string(r.Reason)
	}
	return string(r.Reason) + ": " + r.Detail
}

// ErrorCode 與 ErrorParams 是邊界（transport）辨認錯誤所用的介面：
// 代碼為 "task." 加上 Reason；Detail 只是說明，不作為翻譯參數。
func (r *Rejection) ErrorCode() string              { return r.Reason.Code() }
func (r *Rejection) ErrorParams() map[string]string { return nil }

// Reason 是客戶端可以據以行動的穩定代碼。
type Reason string

// Code 是 Reason 對邊界的代碼："task." 加上 Reason。
func (r Reason) Code() string { return "task." + string(r) }

const (
	ReasonForgedOrigin      Reason = "forged_origin" // Origin 與 Kind 不符：客戶端命令帶 runtime，或執行期事實不帶 runtime
	ReasonArchived          Reason = "archived"      // Task 已封存
	ReasonUnknownSession    Reason = "unknown_session"
	ReasonSessionClosed     Reason = "session_closed"
	ReasonSessionExited     Reason = "session_exited"     // Session 已退出，請先 Resume
	ReasonSessionOpen       Reason = "session_open"       // Session 仍連著 agent（idle / running）：刪原生檔案前請先關閉
	ReasonSessionRunning    Reason = "session_running"    // 有 Session 正在運行（歸檔前請先等它結束或取消）
	ReasonConflicted        Reason = "conflicted"         // 請先 TakeOver
	ReasonNotResumable      Reason = "not_resumable"      // 未退出、沒有原生 ID，或原生檔案已不存在
	ReasonUnresolvedChanges Reason = "unresolved_changes" // 請先合併或捨棄 worktree
	ReasonNeedsDesktop      Reason = "needs_desktop"      // 只有 desktop 可以執行此操作
	ReasonInFlight          Reason = "in_flight"          // 操作正在執行中，不能撤銷；掛起後才可以
	ReasonDirtyWorkdir      Reason = "dirty_workdir"      // fan-out 前請先提交或捨棄
	ReasonNotGit            Reason = "not_git"
	ReasonDetachedHead      Reason = "detached_head"     // 沒有檢出分支，worktree 無處合回
	ReasonModelUnavailable  Reason = "model_unavailable" // agent 的模型列表裡沒有這個模型（列表變了）
	ReasonModelSwitching    Reason = "model_switching"   // 上一次選的模型還沒換好
	ReasonUnknownLabel      Reason = "unknown_label"     // 標籤目錄裡沒有這個標籤（剛被刪掉）
	ReasonInvalid           Reason = "invalid"           // 格式不對的命令（程式或協定錯誤）；使用者能處理的拒絕都有具體原因
)

// CodeMergeConflict 是合併衝突掛起 merge_worktree 時的 Problem 代碼（README 規則 9）；
// 參數 files 是衝突檔案，以換行分隔。
const CodeMergeConflict = "task.merge_conflict"

// Problem 是事件與執行期事實中記錄的失敗原因：穩定的代碼加上參數，
// 前端依代碼翻譯（docs/architecture/README.md 規則 9）。代碼屬於產生
// 失敗的套件（如 proc.signaled），由 hub 從錯誤的 ErrorCode 與
// ErrorParams 取得；agent 自己說出的文字，放在回報它的執行期轉接器
// 所定義代碼的 detail 參數裡。
type Problem struct {
	Code   string            `json:"code"`
	Params map[string]string `json:"params,omitzero"`
}

// Reasons 是所有拒絕原因，供邊界核對翻譯表（TestReasons 確認沒有遺漏）。
func Reasons() []Reason {
	return []Reason{
		ReasonForgedOrigin, ReasonArchived, ReasonUnknownSession, ReasonSessionClosed,
		ReasonSessionExited, ReasonSessionOpen, ReasonSessionRunning, ReasonConflicted,
		ReasonNotResumable,
		ReasonUnresolvedChanges, ReasonNeedsDesktop, ReasonDirtyWorkdir, ReasonNotGit,
		ReasonDetachedHead, ReasonInvalid, ReasonInFlight, ReasonModelUnavailable, ReasonModelSwitching,
		ReasonUnknownLabel,
	}
}
