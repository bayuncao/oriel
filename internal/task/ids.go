package task

import "strconv"

// 識別碼。在 Task 內部建立的實體（session、turn、prompt、
// message、intent、confirmation、stream rule），其 ID 由建立它們的
// 事件之 Seq 衍生而來，因此 Decide 不需要 ID 產生器。
// TaskID 由 hub 產生；PermissionID 與 ToolCallID 由回報它們的
// 執行期轉接器產生，在其 Session 內唯一。
type (
	TaskID          string
	SessionID       string
	TurnID          string
	PromptID        string
	MessageID       string
	ToolCallID      string
	PermissionID    string
	IntentID        string
	ConfirmationID  string
	RuleID          string
	WorkspaceID     string
	DeviceID        string
	AgentID         string // "claude", "registry:<id>", "custom:<slug>"
	ProfileID       string
	NativeSessionID string // agent 自己的 session 或 thread ID，用於 resume
	BlobRef         string // 儲存在 blobs/ 下內容的 SHA-256
	LabelID         string // 全局標籤目錄裡的標籤（D135），由 hub 產生
)

// Seq 是事件在其 Task Stream 中的位置，從 1 開始。它
// 同時也作為客戶端訂閱所用的 Cursor。
type Seq int64

// Origin 表示 Command 的來源。客戶端命令由 hub 依據 Principal
// 填入；執行期事實與 effect 結果使用 OriginRuntime，
// 而 Decide 會拒絕謊稱該來源的客戶端命令。
type Origin string

const (
	OriginDesktop Origin = "desktop"
	OriginRemote  Origin = "remote"
	OriginRuntime Origin = "runtime"
)

// 實體 ID 由建立它的事件的 Seq 衍生：類型前綴加十進位 Seq（"s12"、"t15"）。
func sessionID(seq Seq) SessionID { return SessionID("s" + strconv.FormatInt(int64(seq), 10)) }
func turnID(seq Seq) TurnID       { return TurnID("t" + strconv.FormatInt(int64(seq), 10)) }
func promptID(seq Seq) PromptID   { return PromptID("p" + strconv.FormatInt(int64(seq), 10)) }
func messageID(seq Seq) MessageID { return MessageID("m" + strconv.FormatInt(int64(seq), 10)) }
func intentID(seq Seq) IntentID   { return IntentID("i" + strconv.FormatInt(int64(seq), 10)) }
func ruleID(seq Seq) RuleID       { return RuleID("r" + strconv.FormatInt(int64(seq), 10)) }
func confirmationID(seq Seq) ConfirmationID {
	return ConfirmationID("c" + strconv.FormatInt(int64(seq), 10))
}
