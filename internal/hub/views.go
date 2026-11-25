package hub

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/bayuncao/oriel/internal/task"
)

// 投影列，由 actor 從其 State 計算，並與事件寫在
// 同一個交易中。它們可以透過重放重建。

type TaskView struct {
	ID          task.TaskID      `json:"id"`
	Title       string           `json:"title"`
	Workspace   task.WorkspaceID `json:"workspace"`
	Status      task.TaskStatus  `json:"status"`
	Archived    bool             `json:"archived"`
	Sessions    int              `json:"sessions"`
	Running     int              `json:"running"`
	PendingAsks int              `json:"pendingAsks"`
	// Confirmations 是待確認數，即 Views.Confirmations 的長度；
	// 儲存端由確認列推出，不另存。
	Confirmations int `json:"confirmations"`
	// Failed 是掛起的操作數：失敗而等使用者 RetryEffect 的 Effect，
	// 即 State.Failed 的條數（D71）。
	Failed    int       `json:"failed"`
	UpdatedAt time.Time `json:"updatedAt"`
	// PinnedAt 是置頂的時間，即 State.PinnedAt；未置頂時為零值（D109）。
	PinnedAt time.Time `json:"pinnedAt,omitzero"`
	// Priority 與 Labels 即 State 的（D135）；Labels 可能有目錄裡已刪掉的 ID，由讀端略過。
	Priority task.TaskPriority `json:"priority"`
	Labels   []task.LabelID    `json:"labels"`
	// Agents 是 Task 用到的 agent（看板的“負責人”，D135）：按 Session 建立的順序、不重複；沒有 Session 時為空。
	Agents []TaskAgent `json:"agents"`
	// Version 是這一列反映到的 Seq（本批最後一個事件）：頁面拿命令回傳的 Seq 與它比，
	// 就知道推來的列是不是已包含自己的命令。
	Version task.Seq `json:"version"`
	// Removed 表示 Task 已被刪除（DeleteTask，m2.md DL02）：只在 WatchTasks 推送時出現，
	// 此時只有 ID 有值；讀端把這個 ID 從列表拿掉。
	Removed bool `json:"removed,omitzero"`
}

// TaskAgent 是 Task 的一個 agent：ID 與 Session 建立時的名稱（LaunchSnapshot 的 AgentName）。
type TaskAgent struct {
	ID   task.AgentID `json:"id"`
	Name string       `json:"name"`
}

// ConfirmationView 餵給 desktop 的待處理確認清單。
type ConfirmationView struct {
	ID          task.ConfirmationID `json:"id"`
	Task        task.TaskID         `json:"task"`
	Kind        task.CommandKind    `json:"kind"`
	RequestedBy task.DeviceID       `json:"requestedBy"`
	RequestedAt time.Time           `json:"requestedAt"`
}

// SearchHit 是一次全文檢索的命中。
type SearchHit struct {
	Task task.TaskID `json:"task"`
	Seq  task.Seq    `json:"seq"`
	Kind SearchKind  `json:"kind"`
	// Ref 是命中的訊息：使用者的訊息是 PromptID，回答是 MessageID；標籤為空。頁面用它找到對話裡的那一輪。
	Ref string `json:"ref,omitzero"`
	// Agent 是回答的 agent 的名稱（啟動時的快照）；不是回答時為空。
	Agent string `json:"agent,omitzero"`
	// Text 是標籤（“標題\n說明”，說明為空時只有標題）或整條訊息。
	Text string `json:"text"`
}

// SearchKind 是檢索列的來源（D70）。
type SearchKind string

const (
	// SearchLabel 是 Task 的標題與說明。
	SearchLabel SearchKind = "label"
	// SearchPrompt 是使用者發的訊息（Prompt 的文字）。
	SearchPrompt SearchKind = "prompt"
	// SearchReply 是 agent 的正文回答（一整條訊息；思考不進檢索）。
	SearchReply SearchKind = "reply"
)

// SearchMessagesPerTask 是每個 Task 最多幾條訊息命中（D70）；標籤命中不算在內。
const SearchMessagesPerTask = 3

// SearchTerms 把查詢拆成詞：空白分段，丟棄不含字母或
// 數字的詞；不折大小寫（折法由實現決定）。
func SearchTerms(q string) []string {
	var terms []string
	for _, f := range strings.Fields(q) {
		for _, r := range f {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				terms = append(terms, f)
				break
			}
		}
	}
	return terms
}

// SearchEntry 是全文檢索列：Task 的標籤、使用者的一條訊息或 agent 的一條回答（D70）。
type SearchEntry struct {
	Seq   task.Seq   `json:"seq"`
	Kind  SearchKind `json:"kind"`
	Ref   string     `json:"ref,omitzero"`   // PromptID 或 MessageID；標籤為空
	Agent string     `json:"agent,omitzero"` // 回答的 agent 名稱；其他為空
	Text  string     `json:"text"`
}

// SessionLink 記下 OrielHub 跑的一個原生會話屬於哪個 Task（m2.md 第 33 節）：用量頁據此把
// agent 本機記錄裡這個會話的用量算作經 OrielHub，歸到這個 Task。鍵是 (Agent, Native)。
type SessionLink struct {
	Agent   task.AgentID         `json:"agent"`
	Native  task.NativeSessionID `json:"native"`
	Task    task.TaskID          `json:"task"`
	Session task.SessionID       `json:"session"`
}

// ReportedUsage 是 agent 經 OrielHub 上報的一輪用量（UsageReported，D100）。同一個原生會話在
// agent 的本機記錄裡有任何一條時，用量頁不用它（m1.md 計數規則 2）。鍵是 (Task, Seq)。
type ReportedUsage struct {
	Task   task.TaskID          `json:"task"`
	Seq    task.Seq             `json:"seq"`
	Agent  task.AgentID         `json:"agent"`
	Native task.NativeSessionID `json:"native"`
	At     time.Time            `json:"at"`
	// Model 是 Session 當時的模型（agent 報告的設置）；不知道時為空。Dir 是 Task 的 Workspace 目錄。
	Model string     `json:"model,omitzero"`
	Dir   string     `json:"dir,omitzero"`
	Usage task.Usage `json:"usage"`
}

// usageRows 是一個事件產生的會話歸屬或上報（s 是折疊了 e 之後的 State，dir 是 Workspace 目錄）。
func usageRows(s task.State, e task.Event, dir string) (*SessionLink, *ReportedUsage) {
	x, ok := s.Sessions[e.Session]
	if !ok || x.NativeID == "" {
		return nil, nil
	}
	switch e.Kind {
	case task.NativeSessionBoundKind:
		return &SessionLink{Agent: x.Launch.Agent, Native: x.NativeID, Task: s.ID, Session: x.ID}, nil
	case task.UsageReportedKind:
		r := &ReportedUsage{Task: s.ID, Seq: e.Seq, Agent: x.Launch.Agent, Native: x.NativeID,
			At: e.At, Dir: dir, Usage: *e.UsageReported}
		if x.Model != nil {
			r.Model = x.Model.Current
		}
		return nil, r
	}
	return nil, nil
}

// FileVia 是 Task 用一個檔案的方式（D107）。
type FileVia string

const (
	FileRead   FileVia = "read"
	FileEdit   FileVia = "edit"
	FileDelete FileVia = "delete"
	FileMove   FileVia = "move"
	// FileAttach 是使用者附在消息上的檔案（D100）。
	FileAttach FileVia = "attach"
)

// FileUse 是「用過的檔案」投影的一列：一個 Task 用過的一個路徑（D107）。
type FileUse struct {
	// Path 相對於用它的 Session 的工作目錄，以 / 分隔；在工作目錄之外時是絕對路徑。
	Path    string `json:"path"`
	Outside bool   `json:"outside"`
	// Via 與 At 是最後一次用它的方式與時間。
	Via FileVia   `json:"via"`
	At  time.Time `json:"at"`
	// Edited 是有沒有改過它（edit、delete 或 move）。
	Edited bool `json:"edited"`
}

// TaskFile 是 Index.Files 的一列：哪個 Task（在哪個 Workspace）用過哪個檔案。
type TaskFile struct {
	Task      task.TaskID      `json:"task"`
	Workspace task.WorkspaceID `json:"workspace"`
	Use       FileUse          `json:"use"`
}

// FileView 是文件頁的一行（D107）：一個 Workspace 裡的一個路徑，跨 Task 合併；
// 已歸檔的 Task 不算。Hub.Files 按 At 新到舊、同刻按 Workspace 與 Path 升冪排列。
type FileView struct {
	Workspace task.WorkspaceID `json:"workspace"`
	Path      string           `json:"path"`
	Outside   bool             `json:"outside"`
	Via       FileVia          `json:"via"`
	At        time.Time        `json:"at"`
	Edited    bool             `json:"edited"`
	// Task 是最後用它的 Task；Tasks 是用過它的 Task 數。
	Task  task.TaskID `json:"task"`
	Tasks int         `json:"tasks"`
}

// DiffView 是 Hub.Diff 的結果：Task Worktree 相對它的 BaseCommit 的改動（m4.md WT01–WT04），
// 每個檔案一筆，照 git diff 的順序（路徑序）。
type DiffView struct {
	Files []DiffFile `json:"files"`
	// Omitted：patch 合計超過上限（maxDiffPatch），有檔案只列路徑與行數。
	Omitted bool `json:"omitted"`
}

// DiffFile 是一個檔案的改動。Patch 是它的 hunk（從第一個 @@ 起的 unified diff）；
// 二進位檔與只改模式的檔案沒有 Patch。
type DiffFile struct {
	Path    string     `json:"path"`
	OldPath string     `json:"oldPath,omitzero"` // 僅限 renamed
	Change  FileChange `json:"change"`
	Added   int        `json:"added"`
	Removed int        `json:"removed"`
	Binary  bool       `json:"binary"`
	Patch   string     `json:"patch"`
	Omitted bool       `json:"omitted"` // 合計超過上限，沒放 Patch
}

// FileChange 是檔案改動的種類。
type FileChange string

const (
	ChangeAdded    FileChange = "added"
	ChangeModified FileChange = "modified"
	ChangeDeleted  FileChange = "deleted"
	ChangeRenamed  FileChange = "renamed"
)

// TurnEnd 是「結束的回合」投影的一列（D115，m2.md R01）：一個 Session 完成或失敗的一輪；取消的不算。
type TurnEnd struct {
	Session task.SessionID  `json:"session"`
	Turn    task.TurnID     `json:"turn"`
	Result  task.TurnResult `json:"result"` // completed 或 failed
	// Text 是使用者消息的第一行（去掉首尾空白，最多 200 個字元）；沒有文字時為空、Attachment 是第一個附件的名稱。
	Text       string        `json:"text"`
	Attachment string        `json:"attachment,omitzero"`
	Problem    *task.Problem `json:"problem,omitzero"` // 僅限 failed
	EndedAt    time.Time     `json:"endedAt"`
}

// RecapTurn 是 Hub.Recap 的一行（D115）：一個結束的回合，帶它的 Task 的標題與 Workspace。
type RecapTurn struct {
	Task      task.TaskID      `json:"task"`
	TaskTitle string           `json:"taskTitle"`
	Workspace task.WorkspaceID `json:"workspace"`
	TurnEnd
}

// maxTurnText 是 TurnEnd.Text 最多幾個字元。
const maxTurnText = 200

// turnEnd 是一個事件結束的回合：TurnCompleted 或 TurnFailed。prev 是折疊這個事件之前的 State
// （這一輪的 Prompt 還在 Session.Turn 上）。
func turnEnd(prev task.State, e task.Event) (TurnEnd, bool) {
	var result task.TurnResult
	var problem *task.Problem
	switch {
	case e.Kind == task.TurnCompletedKind:
		result = task.TurnOK
	case e.Kind == task.TurnFailedKind && e.TurnFailed != nil:
		result, problem = task.TurnError, new(e.TurnFailed.Problem)
	default:
		return TurnEnd{}, false
	}
	x, ok := prev.Sessions[e.Session]
	if !ok || x.Turn == nil {
		return TurnEnd{}, false
	}
	t := TurnEnd{Session: e.Session, Turn: x.Turn.ID, Result: result, Problem: problem, EndedAt: e.At,
		Text: firstLine(x.Turn.Prompt.Text, maxTurnText)}
	if t.Text == "" && len(x.Turn.Prompt.Attachments) > 0 {
		t.Attachment = x.Turn.Prompt.Attachments[0].Name
	}
	return t, true
}

// firstLine 是 s 第一個非空的行，去掉首尾空白，最多 n 個字元。
func firstLine(s string, n int) string {
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > n {
				return string(r[:n])
			}
			return line
		}
	}
	return ""
}

// viaOf 是工具分類對應的使用方式；其餘分類的路徑多是目錄或通配，不算用過的檔案。
var viaOf = map[task.ToolKind]FileVia{
	task.ToolRead: FileRead, task.ToolEdit: FileEdit,
	task.ToolDelete: FileDelete, task.ToolMove: FileMove,
}

// fileUses 是一個事件用到的檔案：完成了的 read、edit、delete、move 工具呼叫的路徑與附件的路徑。
// 工具呼叫在完成時才算（被拒絕或失敗的不算），分類與路徑取這一輪記下的、更新改過的（Turn.Tools）。
// s 已折疊這個事件（Session 的工作目錄在它之前就定了）；dir 是 Workspace 目錄。
func fileUses(s task.State, e task.Event, dir string) []FileUse {
	var via FileVia
	var paths []string
	switch {
	case e.Kind == task.ToolCallUpdatedKind && e.ToolCallUpdated != nil:
		if e.ToolCallUpdated.Status != task.ToolCompleted {
			return nil
		}
		x, ok := s.Sessions[e.Session]
		if !ok || x.Turn == nil {
			return nil
		}
		r := x.Turn.Tools[e.ToolCallUpdated.ID]
		v, ok := viaOf[r.Kind]
		if !ok {
			return nil
		}
		via, paths = v, r.Paths
	case e.Kind == task.PromptQueuedKind && e.PromptQueued != nil:
		via = FileAttach
		for _, a := range e.PromptQueued.Attachments {
			paths = append(paths, a.Path)
		}
	default:
		return nil
	}
	root := workdirOf(s, e.Session, dir)
	var out []FileUse
	for _, p := range paths {
		rel, outside, ok := relPath(root, p)
		if !ok {
			continue
		}
		out = append(out, FileUse{Path: rel, Outside: outside, Via: via, At: e.At,
			Edited: via == FileEdit || via == FileDelete || via == FileMove})
	}
	return out
}

// workdirOf 是 Session 的工作目錄：Fork 的路徑、Task Worktree 的路徑或 Workspace 目錄
// （與 task 的 Plan 相同）。
func workdirOf(s task.State, sid task.SessionID, dir string) string {
	if x, ok := s.Sessions[sid]; ok && x.Fork != nil {
		return x.Fork.Path
	}
	if s.Worktree != nil {
		return s.Worktree.Path
	}
	return dir
}

// relPath 把路徑轉成相對 root、以 / 分隔的路徑；不在 root 之下（或 root 未知）時
// 回傳清理過的絕對路徑與 outside。相對路徑以 root 為基準。空路徑與 root 本身不算。
func relPath(root, p string) (path string, outside, ok bool) {
	if strings.TrimSpace(p) == "" {
		return "", false, false
	}
	if !filepath.IsAbs(p) {
		if root == "" {
			return "", false, false
		}
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	if root == "" {
		return p, true, true
	}
	rel, err := filepath.Rel(filepath.Clean(root), p)
	switch {
	case err != nil, rel == "..", strings.HasPrefix(rel, ".."+string(filepath.Separator)):
		return p, true, true
	case rel == ".":
		return "", false, false
	}
	return filepath.ToSlash(rel), false, true
}

// mergeUse 把同一路徑後來的一次 next 併進 old：方式、時間與位置取後來的，改過則一直算改過。
func mergeUse(old, next FileUse) FileUse {
	next.Edited = next.Edited || old.Edited
	return next
}

// fileSet 按 Path 累積用過的檔案。
type fileSet map[string]FileUse

func (f fileSet) add(uses []FileUse) {
	for _, u := range uses {
		if old, ok := f[u.Path]; ok {
			u = mergeUse(old, u)
		}
		f[u.Path] = u
	}
}

// list 按 Path 排列；沒有時是 nil。
func (f fileSet) list() []FileUse {
	if len(f) == 0 {
		return nil
	}
	out := make([]FileUse, 0, len(f))
	for _, k := range slices.Sorted(maps.Keys(f)) {
		out = append(out, f[k])
	}
	return out
}

// viewsVersion 是投影演算法的版本。投影的形狀或算法一改就加一：
// Start 發現儲存的版本不同，就從日誌重建所有 Stream 的投影
// （rebuildViews）。TestViewsGolden 在算出的投影變了而版本沒變時失敗。
// 版本 2：用過的檔案（D107）。版本 3：Task 列帶 Version；用過的檔案的工作目錄
// 按每個事件折疊後的 State 算（與重放一致）。版本 4：工具呼叫完成時才算用過的檔案，
// 分類與路徑取更新改過的。版本 5：Task 列帶置頂的時間（D109）。版本 6：結束的回合（D115）。
// 版本 7：Task 列帶優先級、標籤與 agent（D135）。版本 8：檢索列帶來源與 agent 名稱，
// 回答在訊息結束時整條寫一列，思考不進，使用者的訊息進（D70）。版本 9：會話歸屬與上報的用量
// （m2.md 第 33 節）。版本 10：不再有 Session 列（沒有讀者，store 遷移 v13 刪表）。
const viewsVersion = 10

// projections 把本批事件逐一折疊進 prev，回傳新 State、還沒結束的回答與增量寫入的投影：
// Task/Sessions/Confirmations 是全集替換，Search、Files 與 Turns 只收本批；
// dir 是 Workspace 目錄（Files 的相對路徑以它或 Worktree 為基準）。每個事件的
// 檔案按折疊了它的 State 算，與重放（replay）逐事件收集的相同。drafts 不被修改。
func projections(prev task.State, drafts searchDrafts, events []task.Event, dir string) (task.State, searchDrafts, Views) {
	s := prev
	drafts = maps.Clone(drafts)
	if drafts == nil {
		drafts = searchDrafts{}
	}
	files := fileSet{}
	var search []SearchEntry
	var turns []TurnEnd
	var links []SessionLink
	var reported []ReportedUsage
	for _, e := range events {
		if t, ok := turnEnd(s, e); ok {
			turns = append(turns, t)
		}
		s = task.Evolve(s, e)
		if x, ok := drafts.entry(s, e); ok {
			search = append(search, x)
		}
		files.add(fileUses(s, e, dir))
		if l, r := usageRows(s, e, dir); l != nil {
			links = append(links, *l)
		} else if r != nil {
			reported = append(reported, *r)
		}
	}
	v := stateViews(s)
	v.Search, v.Files, v.Turns, v.Links, v.Reported = search, files.list(), turns, links, reported
	return s, drafts, v
}

// rebuildViews 是一條 Stream 重放後的全部投影（Start 重建時用）：
// 與增量寫入同一套計算，Search、Files 與 Turns 是整條 Stream 累積下來的。
func rebuildViews(l loadedTask) Views {
	v := stateViews(l.state)
	v.Search = l.search
	v.Files = l.files.list()
	v.Turns = l.turns
	v.Links, v.Reported = l.links, l.reported
	return v
}

// searchDrafts 是還沒結束的正文訊息到目前為止的文字：回答按片段寫進事件（邊寫邊出），
// 檢索列在訊息結束（Partial 為假的片段）時以整條文字寫一次，跨片段的詞也能命中（D70）。
// 只在記憶體裡；重放時從事件重建。
type searchDrafts map[task.MessageID]string

// entry 是一個事件產生的檢索列（s 是折疊了 e 之後的 State）：Task 的標籤（標題，說明
// 非空時加一行說明）、使用者的訊息，或結束了的一條正文回答；並記下還沒結束的回答。
// 標籤列被下一個標籤取代（D64），增量時由 actor 以 Unsearch 移除舊的，重建時只留最後一條。
// 遮蔽只發生在結束了的訊息上（task 拒絕遮蔽未結束的），所以不必在這裡處理。
func (d searchDrafts) entry(s task.State, e task.Event) (SearchEntry, bool) {
	switch e.Kind {
	case task.MessageAppendedKind:
		m := e.MessageAppended
		if m.Channel != task.ChannelText || m.Redacted {
			return SearchEntry{}, false // 思考不進檢索
		}
		text := d[m.Message] + m.Text
		if m.Partial {
			if text != "" {
				d[m.Message] = text
			}
			return SearchEntry{}, false
		}
		delete(d, m.Message)
		if strings.TrimSpace(text) == "" {
			return SearchEntry{}, false
		}
		return SearchEntry{Seq: e.Seq, Kind: SearchReply, Ref: string(m.Message),
			Agent: s.Sessions[e.Session].Launch.AgentName, Text: text}, true
	case task.PromptQueuedKind:
		if strings.TrimSpace(e.PromptQueued.Text) == "" {
			return SearchEntry{}, false
		}
		return SearchEntry{Seq: e.Seq, Kind: SearchPrompt, Ref: string(e.PromptQueued.ID), Text: e.PromptQueued.Text}, true
	case task.TaskCreatedKind:
		return SearchEntry{Seq: e.Seq, Kind: SearchLabel, Text: labelText(e.TaskCreated.Title, e.TaskCreated.Brief)}, true
	case task.TaskEditedKind:
		return SearchEntry{Seq: e.Seq, Kind: SearchLabel, Text: labelText(e.TaskEdited.Title, e.TaskEdited.Brief)}, true
	}
	return SearchEntry{}, false
}

// isLabel 回報事件是否產生標籤列。
func isLabel(e task.Event) bool {
	return e.Kind == task.TaskCreatedKind || e.Kind == task.TaskEditedKind
}

// stateViews 是全集替換的投影：Task 與 Confirmations；
// Task 列的 UpdatedAt 是 State.UpdatedAt（最後一個事件的時間，置頂不算）。
func stateViews(s task.State) Views {
	at := s.UpdatedAt
	v := Views{
		Task: &TaskView{
			ID: s.ID, Title: s.Title, Workspace: s.Workspace,
			Status: s.Status, Archived: s.Archived,
			Sessions: len(s.Sessions), Failed: len(s.Failed), UpdatedAt: at,
			PinnedAt: s.PinnedAt, Version: s.Version,
			Priority: s.Priority, Labels: append([]task.LabelID{}, s.Labels...), Agents: []TaskAgent{},
		},
		Confirmations: []ConfirmationView{},
	}
	for _, sid := range s.Order {
		x := s.Sessions[sid]
		if !slices.ContainsFunc(v.Task.Agents, func(a TaskAgent) bool { return a.ID == x.Launch.Agent }) {
			v.Task.Agents = append(v.Task.Agents, TaskAgent{ID: x.Launch.Agent, Name: x.Launch.AgentName})
		}
		for _, a := range x.Pending {
			if a.Decision == nil {
				v.Task.PendingAsks++
			}
		}
		if x.Status == task.SessionRunning {
			v.Task.Running++
		}
	}
	ids := make([]task.ConfirmationID, 0, len(s.Confirmations))
	for id := range s.Confirmations {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		c := s.Confirmations[id]
		v.Confirmations = append(v.Confirmations, ConfirmationView{
			ID: id, Task: s.ID, Kind: c.Command.Kind,
			RequestedBy: c.RequestedBy, RequestedAt: c.RequestedAt,
		})
	}
	v.Task.Confirmations = len(v.Confirmations)
	return v
}

// 設定。

// Workspace 是已註冊的專案目錄，第一次在其中建立
// Task 時自動註冊。
type Workspace struct {
	ID         task.WorkspaceID     `json:"id"`
	Path       string               `json:"path"`
	Name       string               `json:"name"`
	Policy     task.IsolationPolicy `json:"policy"`
	LastUsedAt time.Time            `json:"lastUsedAt"`
}

// NotifySettings 是七類系統通知的開關（JSON 鍵即 NotificationKind）。
type NotifySettings struct {
	Ask          bool `json:"ask"`
	Failed       bool `json:"failed"`
	Stalled      bool `json:"stalled"`
	Recommended  bool `json:"recommended"`
	Confirmation bool `json:"confirmation"`
	Conflict     bool `json:"conflict"`
	Completed    bool `json:"completed"`
}

type Settings struct {
	ReviewerProfile task.ProfileID `json:"reviewerProfile,omitzero"`
	Notify          NotifySettings `json:"notify"`
}

// DefaultSettings 是沒有存過設定時的值，也是預設值唯一的出處：
// completed 關，其餘開。
func DefaultSettings() Settings {
	return Settings{Notify: NotifySettings{
		Ask: true, Failed: true, Stalled: true, Recommended: true,
		Confirmation: true, Conflict: true}}
}

// Notification 是盡力而為的系統通知；不持久化，文字由 app
// 從 locales 依 Kind 與 Params 渲染。
type Notification struct {
	Kind      NotificationKind  `json:"kind"`
	Task      task.TaskID       `json:"task"`
	TaskTitle string            `json:"taskTitle"`
	Session   task.SessionID    `json:"session,omitzero"`
	Params    map[string]string `json:"params,omitzero"`
	// Reason 是回合失敗的原因（NotifyFailed），帶參數，由 app 譯成文字；其他種類為 nil。
	Reason *task.Problem `json:"reason,omitzero"`
}

type NotificationKind string

const (
	NotifyAsk          NotificationKind = "ask"          // 預設開啟
	NotifyFailed       NotificationKind = "failed"       // 回合失敗或 agent 當掉；開啟
	NotifyStalled      NotificationKind = "stalled"      // 回合長時間沒有動靜（D79）；開啟
	NotifyRecommended  NotificationKind = "recommended"  // 開啟
	NotifyConfirmation NotificationKind = "confirmation" // 開啟
	NotifyConflict     NotificationKind = "conflict"     // 合併衝突；開啟
	NotifyCompleted    NotificationKind = "completed"    // 回合完成；關閉
)

// Notifies 回傳某類通知要不要發；不認得的種類不發。
func (s Settings) Notifies(k NotificationKind) bool {
	switch k {
	case NotifyAsk:
		return s.Notify.Ask
	case NotifyFailed:
		return s.Notify.Failed
	case NotifyStalled:
		return s.Notify.Stalled
	case NotifyRecommended:
		return s.Notify.Recommended
	case NotifyConfirmation:
		return s.Notify.Confirmation
	case NotifyConflict:
		return s.Notify.Conflict
	case NotifyCompleted:
		return s.Notify.Completed
	}
	return false
}

// labelText 是標籤檢索列的文字：標題，有說明時加一行說明（D64）。
func labelText(title, brief string) string {
	if brief == "" {
		return title
	}
	return title + "\n" + brief
}
