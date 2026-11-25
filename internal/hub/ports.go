package hub

import (
	"context"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// Port 在此由其消費者宣告；store、runtime 與 workspace
// 實作它們，測試使用記憶體內的真實現。

// Log 是事件儲存。每次 Append 是一個交易。
type Log interface {
	// Append 為一個 Task 寫入 Batch。expected 是最後一個
	// 已儲存事件的 Seq；不符表示有並行寫入者，會失敗。
	Append(ctx context.Context, id task.TaskID, expected task.Seq, b Batch) error
	// Read 依序回傳 from 之後最多 limit 筆記錄。
	// Read 依序回傳 Seq > from 的記錄；limit <= 0 表示不限筆數。
	Read(ctx context.Context, id task.TaskID, from task.Seq, limit int) ([]Record, error)
	// Streams 回傳所有 Stream 的 TaskID（順序不定）。
	Streams(ctx context.Context) ([]task.TaskID, error)
	// Delete 移除 Task 的 Stream、投影列（含用過的檔案與回合）與檢索列。
	Delete(ctx context.Context, id task.TaskID) error
	// ViewsVersion 回傳已儲存的投影所依的演算法版本；從未記過是 0。
	ViewsVersion(ctx context.Context) (int, error)
	// SetViewsVersion 記下投影版本（重建完所有 Stream 之後）。
	SetViewsVersion(ctx context.Context, v int) error
	// Reproject 在一個交易中以 v 整體替換一條 Stream 的投影：
	// Task 列、Session 列、確認列、檢索列與用過的檔案；v.Unsearch 不使用。
	Reproject(ctx context.Context, id task.TaskID, v Views) error
}

// Record 是儲存形式的事件：payload 由 task 的編解碼器編碼，
// 因此 store 從不解讀它。
type Record struct {
	Seq     task.Seq       `json:"seq"`
	Kind    task.EventKind `json:"kind"`
	At      time.Time      `json:"at"`
	Payload []byte         `json:"payload"`
}

// Batch 是一次 Decide 所寫的全部內容，以原子方式寫入。
type Batch struct {
	Records []Record
	// Redact 將這些記錄的 payload 替換為遮蔽標記；
	// 只有 RedactMessage 命令會產生它。
	Redact []Record
	Views  Views
	// NewWorkspace 註冊新 Task 建立所在的 Workspace。
	NewWorkspace *Workspace
	// NewRules 是隨「一律允許」儲存的 workspace 與全域規則。
	NewRules []task.Rule
}

// Index 是投影的讀端口；由 store 實現（MemStore 是測試的
// 記憶體版本）。
type Index interface {
	// TaskViews 依 UpdatedAt 新到舊、同刻按 ID 升冪。
	TaskViews(ctx context.Context) ([]TaskView, error)
	// Confirmations 是全部 Task 的待確認，按請求時間、Task、ID 升冪。
	Confirmations(ctx context.Context) ([]ConfirmationView, error)
	// Search 是全文檢索；limit 由呼叫方保證大於 0。
	Search(ctx context.Context, q string, limit int) ([]SearchHit, error)
	// Files 是未歸檔的 Task 用過的檔案（D107），帶 Task 的 Workspace，順序不定。
	Files(ctx context.Context) ([]TaskFile, error)
	// Recap 是未歸檔的 Task 在 since 之後（含）結束的回合（D115），帶 Task 的標題與 Workspace，順序不定。
	Recap(ctx context.Context, since time.Time) ([]RecapTurn, error)
}

// Views 是從新 State 衍生的投影列。
type Views struct {
	Task          *TaskView
	Confirmations []ConfirmationView // Task 的完整集合，取代已儲存的
	Search        []SearchEntry
	Unsearch      []task.Seq // 要移除的條目，用於遮蔽
	// Files 是用過的檔案（D107），按 Path 排列。Append 時是本批用到的，按 (Task, Path)
	// 以 mergeUse 併進已存的列（批次按 Seq 依序寫入，所以本批的總是較新）；Reproject 時
	// 是整條 Stream 的，整體替換。
	Files []FileUse
	// Turns 是結束的回合（D115），按結束的順序。Append 時是本批結束的，加進已存的列；
	// Reproject 時是整條 Stream 的，整體替換。
	Turns []TurnEnd
	// Links 是 OrielHub 跑的原生會話屬於哪個 Task，Reported 是 agent 上報的用量（m2.md 第 33 節）。
	// Append 時是本批的，Reproject 時是整條 Stream 的；兩者都只增不刪：已有的列照舊（同一鍵的
	// 內容相同），Delete 也不刪（刪掉的 Task 的用量仍算，DL12）。
	Links    []SessionLink
	Reported []ReportedUsage
}

// Config 保存跨 Task 的設定，由 hub 直接寫入，而非
// 透過 Task 的 Stream。
type Config interface {
	Workspaces(ctx context.Context) ([]Workspace, error)
	PutWorkspace(ctx context.Context, w Workspace) error
	Rules(ctx context.Context) ([]task.Rule, error)
	DeleteRule(ctx context.Context, id task.RuleID) error
	Settings(ctx context.Context) (Settings, error)
	PutSettings(ctx context.Context, s Settings) error
	// 標籤目錄（D135）。
	Labels(ctx context.Context) ([]Label, error)
	PutLabel(ctx context.Context, l Label) error
	DeleteLabel(ctx context.Context, id task.LabelID) error
	// 遠程設備（D145）：PutDevice 按 ID upsert，DeleteDevice 不存在不算錯，
	// ClearDevices 刪掉全部（重置遠程）。
	Devices(ctx context.Context) ([]Device, error)
	PutDevice(ctx context.Context, d Device) error
	DeleteDevice(ctx context.Context, id task.DeviceID) error
	ClearDevices(ctx context.Context) error
	// 各 agent 的模型（D158）：最近報告的列表與預設模型；PutModels 換掉全部。
	Models(ctx context.Context) ([]AgentModels, error)
	PutModels(ctx context.Context, all []AgentModels) error
}

// Runtime 啟動 agent 連線；每個 task.Protocol 一個轉接器。
type Runtime interface {
	Start(ctx context.Context, opts StartOptions) (Conn, error)
}

type StartOptions struct {
	Launch  task.LaunchSnapshot
	Workdir string
	Resume  task.NativeSessionID // 新的原生 session 時為空
	Env     []string             // 已解析秘密值的環境快照
	// Emit 向 Task 的 actor 回報執行期事實：Session 與 Origin
	// 留空，hub 填入。`output` 事實是小的增量文字，Partial
	// 旗標被忽略（hub 依 D26 切段）。權限請求在轉接器中保持
	// pending，直到 Answer 被呼叫；這是唯一的核准途徑。
	Emit func(task.Command)
}

// Launcher 把 Agent 解析為 Session 啟動時保存的 Launch Snapshot（預設 Profile，
// D95）：先確認 agent 裝好了、能啟動，ACP agent 探測它能否恢復（D98）。
// 由 app 實作。
type Launcher interface {
	Launch(ctx context.Context, agent task.AgentID) (task.LaunchSnapshot, error)
	// CheckAgent 在 agent 在目錄裡時回 nil（不檢查是否安裝），否則回 agent.unknown_agent
	// （hub 不導入 agent，錯誤由它給）；設預設模型時用（MD11）。
	CheckAgent(agent task.AgentID) error
}

// ExitError 是 Start 在 agent 行程已經結束時回的錯誤（D102）：hub 把它
// 記為 exited，原因取 ErrorCode / ErrorParams（沒有時同 Effect 失敗），
// 附上 StderrTail，不經不帶尾部的 effect_failed。
type ExitError interface {
	error
	StderrTail() string
}

// Conn 是服務一個 Session 的執行中 agent 行程。
type Conn interface {
	NativeID() task.NativeSessionID
	Prompt(ctx context.Context, p task.Prompt) error // 送出後即回傳；輸出經由 Emit 抵達
	Answer(ctx context.Context, id task.PermissionID, v task.Verdict) error
	Cancel(ctx context.Context) error
	// SetModel 把模型換成 m.Value（D118），回傳 agent 換完之後的模型設置；
	// 不另經 Emit 回報這個結果。要有時限：這一輪的 Prompt 等著它（ACP 是 C48）。
	SetModel(ctx context.Context, m task.ModelSwitch) (*task.ModelSetting, error)
	Close() error
}

// Git 建立、合併、捨棄 worktree 並計算差異；由 workspace
// 套件實作。Create、Merge、Discard 都可重試：上次做到一半（崩潰、逾時）時再做一次得到同樣的結果。
type Git interface {
	Inspect(ctx context.Context, dir string) (task.Repo, error)
	Resolve(dir string) (string, error)
	Exists(path string) bool
	Create(ctx context.Context, c task.CreateWorktree) error
	// Merge 把 From 合進它的 BaseBranch，成功後刪掉 From 的 worktree 與分支（D37）；
	// 有衝突時什麼都不改，回傳衝突的檔案；MergePullRequest 回傳 pull request 的地址。
	Merge(ctx context.Context, m task.MergeWorktree) (conflicts []string, url string, err error)
	// Discard 刪掉 worktree 與它的分支，未提交的改動一併丟掉。
	Discard(ctx context.Context, d task.DiscardWorktree) error
	// Diff 是 worktree 相對 BaseCommit 的全部改動（已提交、未提交與未追蹤的檔案），不改動 worktree。
	Diff(ctx context.Context, w task.Worktree) (string, error)
}

// Notifier 透過 mygo 的 Notification API 發送系統通知。
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Timer 是排定的計時器；Stop 停掉它，回傳是否停得及。
type Timer interface {
	Stop() bool
}
