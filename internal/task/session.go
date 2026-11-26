package task

import (
	"slices"
	"strings"
	"time"
)

// Session 是 Task 內部的實體：與一個 agent 的一段對話，
// 由 Profile 的快照啟動。
type Session struct {
	ID          SessionID       `json:"id"`
	Launch      LaunchSnapshot  `json:"launch"`
	Role        Role            `json:"role"`
	Fork        *WorktreeState  `json:"fork"` // nil = 在 Task 的工作目錄中作業
	NativeID    NativeSessionID `json:"nativeId,omitzero"`
	NativeGone  bool            `json:"nativeGone"` // 原生檔案已刪除；無法 resume
	Bound       bool            `json:"bound"`      // 目前有進程綁定著原生會話：native_bound 置真、ProcessExited 置假；Plan 據此分辨「還沒啟動」與「崩潰了」
	Conflicted  bool            `json:"conflicted"` // 原生 session 在 OrielHub 之外被使用；唯讀
	Status      SessionStatus   `json:"status"`
	Exit        *Exited         `json:"exit"`                 // 最近一次行程結束的原因與 stderr 結尾；SessionResumed 清掉
	Turn        *Turn           `json:"turn"`                 // 最多一個
	LastTurn    *TurnRecord     `json:"lastTurn"`             // 最近結束的一輪；Turn 在跑時是上一輪的
	Open        MessageID       `json:"open,omitzero"`        // 尚未結束的訊息：最後一段 Partial 為真
	OpenChannel ChannelKind     `json:"openChannel,omitzero"` // Open 那則訊息的頻道；頻道切換結束它
	Queue       []Prompt        `json:"queue"`
	// Retry 是行程還沒綁定原生會話（Bound 為假：agent 沒起來，或接回時起不來）就失敗的那一輪的 Prompt：它從沒
	// 送到 agent，Resume 時重新排在隊首（m2.md L56）。之後再開始一輪或 Resume 時清掉。
	Retry          *Prompt              `json:"retry,omitzero"`
	Pending        map[PermissionID]Ask `json:"pending"` // 未決定的 ask，以及已決定但尚未送達的
	CompletedTurns int                  `json:"completedTurns"`
	// Model 是 agent 最近報告的模型設置（D118）：Start 時與 agent 自己改了時報告；nil = 沒報告（或還沒啟動過）。
	Model *ModelSetting `json:"model"`
	// ModelWanted 是使用者選的模型（ModelChosen）：連線存在、與 Model.Current 不同時 Plan 去設它，
	// 接回後 agent 回到預設的也設回來（m2.md V86）。空 = 跟著 agent。
	ModelWanted string `json:"modelWanted,omitzero"`
	// ModelProblem 是最近一次沒能換成的模型與原因（ModelSwitchFailed）；下一次 ModelChosen 清掉。
	ModelProblem *ModelProblem `json:"modelProblem"`
}

// ModelSetting 是 agent 報告的模型設置：ACP 會話設置（session config option）中分類為 model 的那一項（D118）。
type ModelSetting struct {
	Option  string        `json:"option"`  // 設置的 ID，切換時原樣送回
	Current string        `json:"current"` // 目前的值
	Choices []ModelChoice `json:"choices"`
}

// ModelChoice 是一個可選的模型。
type ModelChoice struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
	Group       string `json:"group,omitzero"` // agent 分組時所在組的名稱
}

// ModelProblem 是沒能換成的模型（Value）與原因：agent 拒絕或切換失敗，或接回後已不在列表裡（task.model_unavailable）。
type ModelProblem struct {
	Value   string  `json:"value"`
	Problem Problem `json:"problem"`
}

// offers 回報 agent 的模型列表裡有沒有這個值。
func (m *ModelSetting) offers(value string) bool {
	return m != nil && slices.ContainsFunc(m.Choices, func(c ModelChoice) bool { return c.Value == value })
}

// equal 比較兩次報告的內容。
func (m *ModelSetting) equal(o *ModelSetting) bool {
	if m == nil || o == nil {
		return m == o
	}
	return m.Option == o.Option && m.Current == o.Current && slices.Equal(m.Choices, o.Choices)
}

// follows 回報這次報告是不是 agent 自己換了（不是 Start 時的報告、與使用者選的不同）：跟著它，清掉 ModelWanted（V84）。
func (x Session) follows(r ModelReport) bool {
	return !r.Start && r.Setting != nil && x.ModelWanted != "" && r.Setting.Current != x.ModelWanted
}

// wantsModel 回報 Plan 是否要把模型設成使用者選的（D118）。
func (x Session) wantsModel() bool {
	return x.ModelWanted != "" && x.Model != nil && x.Model.Current != x.ModelWanted
}

type SessionStatus string

const (
	SessionIdle    SessionStatus = "idle"
	SessionRunning SessionStatus = "running"
	SessionExited  SessionStatus = "exited"
	SessionClosed  SessionStatus = "closed" // 終態
)

type Role string

const (
	RoleWorker   Role = "worker"
	RoleReviewer Role = "reviewer" // 在 fan-out 之後推薦一個 Fork
)

// resumable 回報行程不在之後還接不接得回這段對話：有原生會話時要 agent 能 Resume 且原生檔案還在；
// 還沒綁定過原生會話的，只有一輪都沒完成時能重新開始。
func (x Session) resumable() bool {
	if x.NativeGone {
		return false
	}
	if x.NativeID != "" {
		return x.Launch.Caps.Resume
	}
	return x.CompletedTurns == 0
}

// openFork 回報 Session 有一個尚未處置的 Fork。
func (x Session) openFork() bool {
	return x.Fork != nil && x.Fork.Outcome == OutcomeOpen
}

// Turn 是一次 prompt 執行到 agent 回覆結束。
type Turn struct {
	ID         TurnID      `json:"id"`
	Prompt     Prompt      `json:"prompt"`     // 是這回合執行的 Prompt（從 Queue 移來）；Plan 的 send_prompt 送它
	Cancelling bool        `json:"cancelling"` // 已請求取消，但 agent 尚未確認
	Reply      *ReplyDraft `json:"-"`          // 這一輪到目前為止最後一條正文，回合結束時拼好移進 Session.LastTurn；不送給頁面（進行中只顯示“正在處理”，D104）
	// Tools 是這一輪的工具呼叫目前的分類與路徑（更新改過的取更新的）：hub 在工具呼叫完成時按它算用過的檔案（D107）。
	// 不送給頁面（頁面從事件折疊時間線）。
	Tools map[ToolCallID]ToolRef `json:"-"`
}

// ToolRef 是一次工具呼叫目前的分類與路徑。
type ToolRef struct {
	Kind  ToolKind
	Paths []string
}

// ReplyDraft 是進行中這一輪的最後一條正文：收到的各段倒著串成不可變的鏈，接一段是 O(1)、不複製已收到的
// 文字（每次折疊都拼接會隨正文長度平方增長），舊的 State 仍看得到它自己的那一份；回合結束時 Reply 才拼成一條。
type ReplyDraft struct {
	Message  MessageID
	Redacted bool
	last     *replyPart
}

type replyPart struct {
	text string
	prev *replyPart
}

// Reply 把各段拼成一條正文；遮蔽過的只留標記。nil 是沒有正文。
func (d *ReplyDraft) Reply() *Reply {
	if d == nil {
		return nil
	}
	r := &Reply{Message: d.Message, Redacted: d.Redacted}
	if d.Redacted {
		return r
	}
	var parts []string
	n := 0
	for p := d.last; p != nil; p = p.prev {
		parts = append(parts, p.text)
		n += len(p.text)
	}
	var b strings.Builder
	b.Grow(n)
	for i := len(parts) - 1; i >= 0; i-- {
		b.WriteString(parts[i])
	}
	r.Text = b.String()
	return r
}

// Reply 是一輪裡 agent 的一條正文（text 頻道，不含思考）：同一條訊息的各段拼起來。
// 遮蔽後 Text 為空、Redacted 為真（m2.md L34）。
type Reply struct {
	Message  MessageID `json:"message"`
	Text     string    `json:"text"`
	Redacted bool      `json:"redacted"`
}

// TurnRecord 是結束了的一輪（m2.md L25–L29）：結局、失敗的原因、結束的時間
// （結局事件的 At），與這一輪最後一條正文（沒有正文時為 nil）。
type TurnRecord struct {
	Turn    TurnID     `json:"turn"`
	Result  TurnResult `json:"result"`
	Problem *Problem   `json:"problem,omitzero"` // 僅限 TurnError
	EndedAt time.Time  `json:"endedAt"`
	Reply   *Reply     `json:"reply"`
}

// Prompt 是來自使用者的一次輸入。
type Prompt struct {
	ID          PromptID     `json:"id"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
}

// Attachment 是隨 Prompt 傳送的圖片或檔案，可能以 blob
// 儲存，或以工作目錄內的路徑參照。
type Attachment struct {
	Name string  `json:"name"`
	MIME string  `json:"mime"`
	Blob BlobRef `json:"blob,omitzero"`
	Path string  `json:"path,omitzero"`
}

// LaunchSnapshot 是建立 Session 時解析出的 Profile。之後
// 對 Profile 的變更不影響它，Resume 也使用它。它不持有
// 秘密值本身，只持有對秘密值的參照。
type LaunchSnapshot struct {
	Agent          AgentID       `json:"agent"`
	AgentName      string        `json:"agentName"`
	Protocol       Protocol      `json:"protocol"`
	Profile        ProfileID     `json:"profile"`
	ProfileName    string        `json:"profileName"`
	Executable     string        `json:"executable"`
	Args           []string      `json:"args"`
	Env            []EnvVar      `json:"env"`
	Model          string        `json:"model,omitzero"` // 設定裡的預設模型（MD05，hub 解析時填）
	Approval       ApprovalMode  `json:"approval"`
	AskTimeout     time.Duration `json:"askTimeout"`              // 0 = 永不
	TimeoutVerdict Verdict       `json:"timeoutVerdict,omitzero"` // AskTimeout 到期時套用
	Caps           Capabilities  `json:"caps"`
}

// Protocol 是 agent 使用的線路協議；它決定執行期
// 轉接器的選擇。
type Protocol string

const (
	ProtocolClaude Protocol = "claude-stream-json"
	ProtocolCodex  Protocol = "codex-app-server"
	ProtocolACP    Protocol = "acp"
	ProtocolPi     Protocol = "pi-rpc"
)

// EnvVar 是給 agent 行程的環境變數：字面值，
// 或存放在系統憑證儲存中秘密值的鍵。
type EnvVar struct {
	Name   string `json:"name"`
	Value  string `json:"value,omitzero"`
	Secret string `json:"secret,omitzero"`
}

// ApprovalMode 表示由誰核准 agent 的動作。
type ApprovalMode string

const (
	ApprovalOrielHub ApprovalMode = "orielhub" // agent 每次都會請求；由 OrielHub 決定
	ApprovalNative   ApprovalMode = "native"   // agent 自身的寬鬆模式；請求不會到達 OrielHub
)

// Capabilities 是 agent 轉接器所支援的能力，隨快照一起保存，
// 因為 Decide 的規則依賴它們。
type Capabilities struct {
	Resume         bool `json:"resume"`
	DetectConflict bool `json:"detectConflict"` // 能偵測原生 session 是否在別處被使用
	DeleteNative   bool `json:"deleteNative"`   // 知道其原生 session 檔案的位置
}
