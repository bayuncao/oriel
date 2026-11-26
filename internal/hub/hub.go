package hub

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// Options 是 New 的參數。
type Options struct {
	Log    Log
	Config Config
	// Index 是投影讀端口（必須；app 與測試都傳真實現）。
	Index Index
	// Notifier 發系統通知；nil 不發。
	Notifier Notifier
	// Runtimes 每個 task.Protocol 一個 agent 連線轉接器。
	Runtimes map[task.Protocol]Runtime
	// Launcher 把 Agent 解析為 Launch Snapshot；nil → 不能帶 agent 建 Task。
	Launcher Launcher
	Git      Git
	// Env 是 agent 的環境快照；nil → proc.Inherited()。
	Env func(context.Context) (proc.Env, error)
	// Secret 解析 EnvVar.Secret；nil → 一律 hub.secret_missing。
	Secret func(key string) (string, error)
	// WorktreeRoot 是 worktree 的建立位置（Inputs.WorktreeRoot）。
	WorktreeRoot string
	// Now 是時鐘；nil → time.Now。
	Now func() time.Time
	// Timer 排定計時器；nil → time.AfterFunc 的包裝。
	Timer func(d time.Duration, f func()) Timer
	// Logger 是除錯日誌；nil → slog.Default()。
	Logger *slog.Logger
}

// Hub 是包在 task 外的命令式外殼：每個活的 Task 一個 Actor。
// cfgCache 是 Config 的記憶體快照；hub 是 Config 的唯一寫者，
// 快取永遠跟落盤一致。短互斥只保護記憶體存取，不做 I/O。
type cfgCache struct {
	mu         sync.Mutex
	workspaces map[task.WorkspaceID]Workspace
	rules      []task.Rule
	settings   Settings
	labels     map[task.LabelID]Label
	devices    map[task.DeviceID]Device
	models     map[task.AgentID]AgentModels
}

type Hub struct {
	log          Log
	config       Config
	index        Index
	notifier     Notifier
	runtimes     map[task.Protocol]Runtime
	launcher     Launcher
	git          Git
	env          func(context.Context) (proc.Env, error)
	secret       func(string) (string, error)
	worktreeRoot string
	now          func() time.Time
	timer        func(time.Duration, func()) Timer
	logger       *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	hooks struct {
		fact         func(task.TaskID, task.Command) // 測試用：事實入隊後呼叫
		deleteQueued func(task.TaskID)               // 測試用：刪除在 Actor 裡排上、等 Effect 回來時呼叫
		snapQueued   func(task.TaskID)               // 測試用：running 的快照要求進了 Actor 的郵箱後呼叫
	}

	mu       sync.Mutex
	closed   bool
	actors   map[task.TaskID]*actor
	actorWg  sync.WaitGroup
	notifyWg sync.WaitGroup // 通知 dispatcher goroutine
	notes    chan Notification
	watchers map[*taskWatcher]struct{} // 在 mu 下存取
	// deviceWatchers 是 WatchDevices 的觀察者，在 mu 下存取。
	deviceWatchers map[*deviceWatcher]struct{}

	cfg      cfgCache
	cfgWrite sync.Mutex // 序列化設定寫入（含新 Workspace 登記與 LastUsedAt）
	// reports 是 actor 交來、還沒寫進設定的模型報告（models.go）。
	reports modelReports
}

func New(o Options) *Hub {
	h := &Hub{
		log:          o.Log,
		config:       o.Config,
		index:        o.Index,
		notifier:     o.Notifier,
		runtimes:     o.Runtimes,
		launcher:     o.Launcher,
		git:          o.Git,
		env:          o.Env,
		secret:       o.Secret,
		worktreeRoot: o.WorktreeRoot,
		now:          o.Now,
		timer:        o.Timer,
		logger:       slog.Default(),
		actors:       map[task.TaskID]*actor{},
	}
	h.cfg.workspaces = map[task.WorkspaceID]Workspace{}
	h.cfg.labels = map[task.LabelID]Label{}
	h.cfg.devices = map[task.DeviceID]Device{}
	h.cfg.models = map[task.AgentID]AgentModels{}
	h.reports.init()
	h.deviceWatchers = map[*deviceWatcher]struct{}{}
	h.notes = make(chan Notification, 64)
	h.watchers = map[*taskWatcher]struct{}{}
	if h.now == nil {
		h.now = time.Now
	}
	if h.env == nil {
		h.env = func(context.Context) (proc.Env, error) { return proc.Inherited(), nil }
	}
	if h.timer == nil {
		h.timer = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	if o.Logger != nil {
		h.logger = o.Logger
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.reports.wg.Add(1)
	go h.reportLoop()
	if h.notifier != nil {
		h.notifyWg.Add(1)
		go h.notifyLoop()
	}
	return h
}

// Start 重放所有 Stream，載入未結束的 Task 並重跑 Plan（恢復）。
// 儲存的投影版本與 viewsVersion 不同時，順手以重放結果整體
// 替換每條 Stream 的投影，全部寫完才記下新版本：中途失敗下次
// 重做，替換是冪等的。
func (h *Hub) Start(ctx context.Context) error {
	if err := h.loadCache(ctx); err != nil {
		return err
	}
	stored, err := h.log.ViewsVersion(ctx)
	if err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	rebuild := stored != viewsVersion
	ids, err := h.log.Streams(ctx)
	if err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	for _, id := range ids {
		l, err := h.replay(ctx, id, rebuild)
		if err != nil {
			return err
		}
		if rebuild {
			if err := h.log.Reproject(ctx, id, rebuildViews(l)); err != nil {
				return fail(CodeStoreFailed, err, "detail", err.Error())
			}
			l.search, l.files, l.links, l.reported = nil, nil, nil, nil
		}
		if terminal(l.state) {
			continue
		}
		if _, err := h.startActor(id, l); err != nil {
			return err // actor 讀任何訊息之前先跑一次 Plan，完成崩潰恢復
		}
	}
	if rebuild {
		if err := h.log.SetViewsVersion(ctx, viewsVersion); err != nil {
			return fail(CodeStoreFailed, err, "detail", err.Error())
		}
		if len(ids) > 0 {
			h.logger.Info("hub: projections rebuilt", "from", stored,
				"to", viewsVersion, "streams", len(ids))
		}
	}
	return nil
}

// terminal 回報 Task 是否已結束：歸檔，或沒有任何活的東西。
func terminal(s task.State) bool {
	if !s.Archived {
		for _, x := range s.Sessions {
			if x.Status != task.SessionClosed || len(x.Pending) > 0 {
				return false
			}
		}
		if len(s.Intents) > 0 || len(s.Confirmations) > 0 {
			return false
		}
	}
	return true
}

// Close 關閉 hub：停掉所有 Actor（計時器、連線、訂閱），
// 不記任何事件（A9）；下次 Start 以 Live.Lost 恢復（D47、D117）。
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	actors := make([]*actor, 0, len(h.actors))
	for _, a := range h.actors {
		actors = append(actors, a)
	}
	for w := range h.watchers {
		close(w.ch)
		delete(h.watchers, w)
	}
	for w := range h.deviceWatchers {
		close(w.ch)
		delete(h.deviceWatchers, w)
	}
	h.mu.Unlock()
	h.cancel()
	for _, a := range actors {
		a.halt()
	}
	h.actorWg.Wait()
	// Actor 都停了，不會再有報告：寫完手上的再返回。
	close(h.reports.stop)
	h.reports.wg.Wait()
	h.notifyWg.Wait()
}

// CreateTask 建立 Task；dir 第一次出現時以 desktop 註冊 Workspace。
func (h *Hub) CreateTask(ctx context.Context, p Principal, dir string, c task.CreateTask) (task.TaskID, task.Seq, error) {
	if err := h.alive(); err != nil {
		return "", 0, err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", 0, fail(CodeUnknownWorkspace, err, "path", dir)
	}
	dir = filepath.Clean(dir)
	if p.Origin() == task.OriginDesktop {
		// 桌面先解符號連結：登記的路徑一律是真實路徑（D72）；
		// 遠端不解析——比對登記路徑時不碰檔案系統。
		if dir, err = h.git.Resolve(dir); err != nil {
			return "", 0, err
		}
	}
	w, ok := h.findWorkspaceByPath(dir)
	if !ok && p.Origin() != task.OriginDesktop {
		// 遠端先擋：不做 Inspect，不透露路徑存在與否
		return "", 0, fail(CodeUnknownWorkspace, nil, "path", dir)
	}
	repo, err := h.git.Inspect(ctx, dir) // I/O 不持鎖
	if err != nil {
		return "", 0, err
	}
	if !ok {
		// K2：慢查一次後在 cfgWrite 下複查，未登記才連建帶登記。
		h.cfgWrite.Lock()
		if w2, ok2 := h.findWorkspaceByPath(dir); ok2 {
			h.cfgWrite.Unlock()
			w, ok = w2, true
		} else {
			w2 := Workspace{ID: task.WorkspaceID(newID("w-")), Path: dir,
				Name: workspaceName(dir), Policy: task.PolicyAuto,
				LastUsedAt: h.now()}
			id, ver, cerr := h.createTask(ctx, p, c, repo, w2, true)
			h.cfgWrite.Unlock()
			return id, ver, cerr
		}
	}
	id, ver, err := h.createTask(ctx, p, c, repo, w, false)
	if err != nil {
		return "", 0, err
	}
	h.touchWorkspace(w.ID) // G6：提示性的最後使用時間
	return id, ver, nil
}

// Isolation 是在 dir 從桌面新建一個不扇出的 Task 會用的隔離方式（D97）：同 CreateTask 解開符號連結、
// 檢查倉庫、看同倉庫有沒有在跑的 Task，但不登記 Workspace、不建 Task。
func (h *Hub) Isolation(ctx context.Context, dir string) (task.IsolationMode, error) {
	if err := h.alive(); err != nil {
		return "", err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", fail(CodeUnknownWorkspace, err, "path", dir)
	}
	if dir, err = h.git.Resolve(filepath.Clean(dir)); err != nil {
		return "", err
	}
	repo, err := h.git.Inspect(ctx, dir)
	if err != nil {
		return "", err
	}
	w, _ := h.findWorkspaceByPath(dir) // 未登記的 Workspace 照 auto
	in := h.inputs(w.ID)
	in.Repo = repo
	in.OtherTasksRunning = h.otherRunning(repo.Root, nil)
	return task.IsolationFor(in), nil
}

// CreateTaskWithAgent 建立 Task；agent 非空時先用它的 Profile profile（空著是預設的，m4.md PO18）
// 解析為 Launch Snapshot，Task 帶一個 Session，說明是第一個 Prompt（D14、M2-2）。解析失敗、或這個
// agent 的協議還沒有轉接器（hub.agent_unsupported）時不建 Task，也不登記
// Workspace。agent 為空時同 CreateTask。
func (h *Hub) CreateTaskWithAgent(ctx context.Context, p Principal, dir string, c task.CreateTask,
	agent task.AgentID, profile task.ProfileID) (task.TaskID, task.Seq, error) {
	if agent != "" {
		if err := h.alive(); err != nil {
			return "", 0, err
		}
		// 先做不必啟動 agent 的檢查：Decide 會拒絕的標題與空說明（說明是第一個
		// Prompt），遠端與未登記的目錄。解析要跑 agent（版本、探測）。
		if _, ok := task.Label(c.Title); !ok {
			return "", 0, &task.Rejection{Reason: task.ReasonInvalid, Detail: "title must be one non-blank line"}
		}
		if strings.TrimSpace(c.Brief) == "" {
			return "", 0, &task.Rejection{Reason: task.ReasonInvalid, Detail: "brief is the first prompt"}
		}
		if p.Origin() != task.OriginDesktop {
			abs, err := filepath.Abs(dir)
			if err != nil {
				return "", 0, fail(CodeUnknownWorkspace, err, "path", dir)
			}
			if _, ok := h.findWorkspaceByPath(filepath.Clean(abs)); !ok {
				return "", 0, fail(CodeUnknownWorkspace, nil, "path", filepath.Clean(abs))
			}
		}
		l, release, err := h.resolveLaunch(ctx, agent, profile)
		if err != nil {
			return "", 0, err
		}
		defer release() // 快照寫進事件流（或建不成）之後
		c.Launches = []task.LaunchSnapshot{l}
	}
	return h.CreateTask(ctx, p, dir, c)
}

// CreateSessionWithAgent 給 Task 啟動 agent（m2.md I05–I11）或接力（m4.md HO01–HO17）：
// 用它的 Profile profile（空著是預設的）解析為 Launch Snapshot（D95），建一個 Session，prompt 是它的
// 第一個 Prompt。from 不空時帶上從那個 Session 交接的內容（D83）：hub 先讀好 Task 的改動，交接的文字由
// Decide 組。先做不必啟動 agent 的檢查（agent 與 prompt 不能空、Task 存在、未歸檔、資料夾裡沒有 Session
// 在運行、worktree 沒處置、沒有未選定的 Fork、from 是 Task 資料夾裡的 Session），再解析、讀改動；Decide 在 Actor 裡再查一次，
// 同時來的兩次只有一次建得成（新 Session 立刻開始一輪，另一次是 task.workdir_busy）。
func (h *Hub) CreateSessionWithAgent(ctx context.Context, p Principal, id task.TaskID,
	agent task.AgentID, profile task.ProfileID, prompt task.Prompt, from task.SessionID) (task.Seq, error) {
	if err := h.alive(); err != nil {
		return 0, err
	}
	if agent == "" {
		return 0, &task.Rejection{Reason: task.ReasonInvalid, Detail: "no agent"}
	}
	if err := task.CheckPrompt(prompt); err != nil {
		return 0, err
	}
	s, err := h.Snapshot(ctx, p, id)
	if err != nil {
		return 0, err
	}
	if err := task.CanStartSession(s); err != nil {
		return 0, err
	}
	if from != "" {
		if _, err := task.HandoffSource(s, from); err != nil {
			return 0, err
		}
	}
	l, release, err := h.resolveLaunch(ctx, agent, profile)
	if err != nil {
		return 0, err
	}
	defer release() // 快照寫進事件流（或建不成）之後
	var handoff *task.Handoff
	if from != "" { // 解析成功後才讀改動：agent 用不了時不白算 diff
		handoff = &task.Handoff{From: from, Changes: h.changes(ctx, s)}
	}
	a, err := h.loadActor(ctx, id)
	if err != nil {
		return 0, err
	}
	c := task.Command{Kind: task.CmdCreateSession, Origin: p.Origin(), Device: p.Device,
		CreateSession: &task.CreateSession{Launch: l, Prompt: &prompt, Handoff: handoff}}
	reply := make(chan cmdResult, 1)
	if err := a.send(h.ctx, msg{cmd: &c, in: h.inputs(a.wsid), reply: reply}); err != nil {
		return 0, err
	}
	r, err := await(a, reply)
	if err != nil {
		return 0, err
	}
	return r.version, r.err
}

// resolveLaunch 用 agent 的 Profile profile 解析出 Launch Snapshot（D95）：要跑 agent（版本、探測），所以呼叫者先做完
// 不必啟動 agent 的檢查。沒有 Launcher、或這個 agent 的協議還沒有轉接器時是 hub.agent_unsupported。
// 成功時呼叫者在快照寫進事件流（或建不成）之後呼叫 release（Launcher.Launch）。
func (h *Hub) resolveLaunch(ctx context.Context, agent task.AgentID, profile task.ProfileID) (task.LaunchSnapshot, func(), error) {
	if h.launcher == nil {
		return task.LaunchSnapshot{}, nil, fail(CodeAgentUnsupported, nil, "agent", string(agent))
	}
	l, release, err := h.launcher.Launch(ctx, agent, profile)
	if err != nil {
		return task.LaunchSnapshot{}, nil, err
	}
	if h.runtimes[l.Protocol] == nil {
		release()
		return task.LaunchSnapshot{}, nil, fail(CodeAgentUnsupported, nil, "agent", l.AgentName)
	}
	// 預設模型（MD05）：設定頁設的；會話開始時換成它（task 的 modelReported）。
	l.Model = h.defaultModel(agent)
	return l, release, nil
}

// checkAttachments 是附件的前提（D100）：以路徑參照的附件必須是 Prompt 送去的工作目錄裡的絕對路徑——
// Submit 是那個 Session 的，CreateSession 與 FanOut 是 Task 的（Worktree 或 Workspace 目錄）。
// 頁面以 Desktop.Attachable 先篩過；這裡在 Actor 裡以目前的 State 核對，不信頁面給的目錄。
// 以 blob 給的附件不受限。dir 是 Workspace 目錄。
func checkAttachments(s task.State, c task.Command, dir string) error {
	var p *task.Prompt
	var session task.SessionID
	switch {
	case c.Kind == task.CmdSubmit && c.Submit != nil:
		p, session = &c.Submit.Prompt, c.Session
	case c.Kind == task.CmdCreateSession && c.CreateSession != nil:
		p = c.CreateSession.Prompt
	case c.Kind == task.CmdFanOut && c.FanOut != nil:
		p = &c.FanOut.Prompt
	}
	if p == nil {
		return nil
	}
	root := workdirOf(s, session, dir)
	for _, a := range p.Attachments {
		if a.Path == "" {
			continue
		}
		if _, outside, ok := relPath(root, a.Path); !filepath.IsAbs(a.Path) || outside || !ok {
			return &task.Rejection{Reason: task.ReasonInvalid, Detail: "attachment outside the working directory"}
		}
	}
	return nil
}

// createTask 跑過一個新 actor 的 create_task；newWS 為真時
// Batch 攜帶 NewWorkspace。
func (h *Hub) createTask(ctx context.Context, p Principal, c task.CreateTask,
	repo task.Repo, w Workspace, newWS bool) (task.TaskID, task.Seq, error) {
	id := task.TaskID(newID("t-"))
	a, err := h.spawnActor(id, loadedTask{
		state:    task.State{ID: id, Workspace: w.ID},
		segments: map[task.MessageID][]segment{}, drafts: searchDrafts{}}, w.Path)
	if err != nil {
		return "", 0, err
	}
	c.Workspace = w.ID
	in := h.inputs(w.ID)
	in.Repo = repo
	in.OtherTasksRunning = h.otherRunning(repo.Root, a)
	reply := make(chan cmdResult, 1)
	var wsp *Workspace
	if newWS {
		wsp = &w
	}
	m := msg{cmd: &task.Command{Kind: task.CmdCreateTask, Origin: p.Origin(),
		Device: p.Device, CreateTask: &c}, in: in, reply: reply, ws: wsp}
	if err := a.send(h.ctx, m); err != nil {
		h.dropActor(a)
		return "", 0, err
	}
	r, err := await(a, reply)
	if err != nil {
		return "", 0, err
	}
	if r.err != nil {
		h.dropActor(a)
		return "", 0, r.err
	}
	return id, r.version, nil
}

// touchWorkspace 更新 LastUsedAt；失敗只記錄（提示性欄位）。
func (h *Hub) touchWorkspace(id task.WorkspaceID) {
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	h.cfg.mu.Lock()
	w, ok := h.cfg.workspaces[id]
	h.cfg.mu.Unlock()
	if !ok {
		return
	}
	w.LastUsedAt = h.now()
	if err := h.config.PutWorkspace(h.ctx, w); err != nil {
		h.logger.Debug("hub: last_used_at update failed", "ws", id, "err", err)
		return
	}
	h.cfg.mu.Lock()
	h.cfg.workspaces[id] = w
	h.cfg.mu.Unlock()
}

// Command 把客戶端命令送進 Task 的 Actor；回傳處理後的 State 版本。
func (h *Hub) Command(ctx context.Context, p Principal, id task.TaskID, c task.Command) (task.Seq, error) {
	if err := h.alive(); err != nil {
		return 0, err
	}
	if c.Kind == task.CmdCreateTask {
		return 0, &task.Rejection{Reason: task.ReasonInvalid, Detail: "use CreateTask"}
	}
	c.Origin, c.Device = p.Origin(), p.Device
	a, err := h.loadActor(ctx, id)
	if err != nil {
		return 0, err
	}
	in := h.inputs(a.wsid)
	if c.Kind == task.CmdSetLabels {
		in.Labels = h.labelIDs()
	}
	if c.Kind == task.CmdFanOut {
		snap := make(chan snapshot, 1)
		if err := a.send(h.ctx, msg{snap: snap}); err != nil {
			return 0, err
		}
		s, err := await(a, snap)
		if err != nil {
			return 0, err
		}
		dir := s.Live.WorkspaceDir
		if s.State.Worktree != nil {
			dir = s.State.Worktree.Path
		}
		repo, err := h.git.Inspect(ctx, dir)
		if err != nil {
			return 0, err
		}
		in.Repo = repo
	}
	reply := make(chan cmdResult, 1)
	m := msg{cmd: &c, in: in, reply: reply}
	if err := a.send(h.ctx, m); err != nil {
		return 0, err
	}
	r, err := await(a, reply)
	if err != nil {
		return 0, err
	}
	return r.version, r.err
}

// DeleteTask 徹底刪除一個已歸檔的 Task（D15，m2.md DL01–DL10）：它的事件流、投影與只被它引用的 blob，
// 不能恢復。只有桌面能用；沒歸檔是 hub.not_archived，還有開著的 worktree 是 task.unresolved_changes。
// 經 Task 的 Actor 排隊：歸檔時關的連線還沒關完就等它關完再刪（DL06），與同時到的命令按到達的
// 順序處理（DL07）。刪除後 WatchTasks 推一列 Removed，Actor 收掉，之後對它的操作是 hub.unknown_task。
// ctx 結束時不再等、回 ctx 的錯誤，已排上的刪除照樣做。
func (h *Hub) DeleteTask(ctx context.Context, p Principal, id task.TaskID) error {
	if err := h.alive(); err != nil {
		return err
	}
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	a, err := h.loadActor(ctx, id)
	if err != nil {
		return err
	}
	reply := make(chan cmdResult, 1)
	if err := a.send(h.ctx, msg{del: reply}); err != nil {
		return err
	}
	// 等連線關完可能要一陣子：呼叫者走了（ctx 結束）就不等，回 ctx 的錯誤；刪除照樣排著，完成後推 Removed。
	r, err := awaitCtx(ctx, a, reply)
	if err != nil {
		return err
	}
	return r.err
}

func (h *Hub) alive() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	return nil
}

// loadCache 從 Config 載入快取；失敗時 Start 報錯。
func (h *Hub) loadCache(ctx context.Context) error {
	ws, err := h.config.Workspaces(ctx)
	if err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	rules, err := h.config.Rules(ctx)
	if err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	st, err := h.config.Settings(ctx)
	if err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	labels, err := h.config.Labels(ctx)
	if err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	devices, err := h.config.Devices(ctx)
	if err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	models, err := h.config.Models(ctx)
	if err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	for _, w := range ws {
		h.cfg.workspaces[w.ID] = w
	}
	for _, l := range labels {
		h.cfg.labels[l.ID] = l
	}
	for _, d := range devices {
		h.cfg.devices[d.ID] = d
	}
	for _, m := range models {
		h.cfg.models[m.Agent] = m
	}
	h.cfg.rules = rules
	h.cfg.settings = st
	return nil
}

// findWorkspaceByPath 從快取按目錄找 Workspace。
func (h *Hub) findWorkspaceByPath(path string) (Workspace, bool) {
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	for _, w := range h.cfg.workspaces {
		if w.Path == path {
			return w, true
		}
	}
	return Workspace{}, false
}

// workspaceDir 是 Workspace 的目錄；不認得時是空的。
func (h *Hub) workspaceDir(id task.WorkspaceID) string {
	w, _ := h.workspaceOf(id)
	return w.Path
}

// workspaceOf 從快取按 ID 找 Workspace。
func (h *Hub) workspaceOf(id task.WorkspaceID) (Workspace, bool) {
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	w, ok := h.cfg.workspaces[id]
	return w, ok
}

// cacheApply 把 Append 攜帶的設定變更套進快取（Append 已成功）。
func (h *Hub) cacheApply(b Batch) {
	if b.NewWorkspace == nil && len(b.NewRules) == 0 {
		return
	}
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	if b.NewWorkspace != nil {
		h.cfg.workspaces[b.NewWorkspace.ID] = *b.NewWorkspace
	}
	for _, r := range b.NewRules {
		if i := slices.IndexFunc(h.cfg.rules, func(x task.Rule) bool { return x.ID == r.ID }); i >= 0 {
			h.cfg.rules[i] = r
		} else {
			h.cfg.rules = append(h.cfg.rules, r)
		}
	}
}

// loadActor 回傳已載入的 actor，或從 Log 重放後建立。
// 重放不持有 h.mu，避免與其他 actor 的郵件往返互鎖。
func (h *Hub) loadActor(ctx context.Context, id task.TaskID) (*actor, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	if a, ok := h.actors[id]; ok {
		h.mu.Unlock()
		return a, nil
	}
	h.mu.Unlock()
	l, err := h.replay(ctx, id, false)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	if a, ok := h.actors[id]; ok {
		h.mu.Unlock()
		return a, nil
	}
	h.mu.Unlock()
	return h.startActor(id, l)
}

// loadedTask 是重放的結果：State 加上遮蔽用的片段索引、
// 目前標籤檢索列的 Seq、還沒結束的回答的文字；search 只在
// 重建投影時收集（整條 Stream 的檢索列，標籤只留最後一條）。
type loadedTask struct {
	state    task.State
	segments map[task.MessageID][]segment
	labelSeq task.Seq
	drafts   searchDrafts
	search   []SearchEntry
	files    fileSet
	turns    []TurnEnd
	links    []SessionLink
	reported []ReportedUsage
}

// replay 從 Log 重放一個 Task（沒有記錄是 unknown_task）；collect 為真時一併收集檢索列、用過的檔案與結束的回合（rebuildViews）。
func (h *Hub) replay(ctx context.Context, id task.TaskID, collect bool) (loadedTask, error) {
	l := loadedTask{state: task.State{ID: id},
		segments: map[task.MessageID][]segment{}, drafts: searchDrafts{}}
	if collect {
		l.files = fileSet{}
	}
	// Workspace 目錄（用過的檔案的相對路徑以它為基準），TaskCreated 之後才知道。
	var ws task.WorkspaceID
	dir := ""
	var from task.Seq
	for {
		recs, err := h.log.Read(ctx, id, from, 500)
		if err != nil {
			return l, fail(CodeStoreFailed, err, "detail", err.Error())
		}
		if len(recs) == 0 && from == 0 {
			return l, fail(CodeUnknownTask, nil)
		}
		for _, r := range recs {
			e, err := task.DecodeEvent(r.Seq, r.Kind, r.At, r.Payload)
			if err != nil {
				return l, fail(CodeStoreFailed, err, "detail", err.Error())
			}
			if collect {
				if t, ok := turnEnd(l.state, e); ok {
					l.turns = append(l.turns, t)
				}
			}
			l.state = task.Evolve(l.state, e)
			// 還沒結束的回答總要重建：actor 接著收到的片段要接在後面。
			x, ok := l.drafts.entry(l.state, e)
			if collect {
				if ok {
					if isLabel(e) && l.labelSeq != 0 {
						l.search = slices.DeleteFunc(l.search,
							func(s SearchEntry) bool { return s.Seq == l.labelSeq })
					}
					l.search = append(l.search, x)
				}
				if l.state.Workspace != ws {
					ws, dir = l.state.Workspace, h.workspaceDir(l.state.Workspace)
				}
				l.files.add(fileUses(l.state, e, dir))
				if k, r := usageRows(l.state, e, dir); k != nil {
					l.links = append(l.links, *k)
				} else if r != nil {
					l.reported = append(l.reported, *r)
				}
			}
			if isLabel(e) {
				l.labelSeq = e.Seq
			}
			if e.Kind == task.MessageAppendedKind && e.MessageAppended != nil {
				m := e.MessageAppended
				l.segments[m.Message] = append(l.segments[m.Message], segment{
					Seq: e.Seq, At: e.At, Session: e.Session,
					Turn: m.Turn, Channel: m.Channel, Partial: m.Partial})
			}
		}
		if len(recs) < 500 {
			break
		}
		from = recs[len(recs)-1].Seq
	}
	return l, nil
}

// startActor 從已重放的 State 建 actor，Workspace 的目錄取自設定快取；不持有 h.mu。
func (h *Hub) startActor(id task.TaskID, l loadedTask) (*actor, error) {
	dir := h.workspaceDir(l.state.Workspace)
	if dir == "" && l.state.Workspace != "" {
		return nil, fail(CodeUnknownWorkspace, nil, "path", string(l.state.Workspace))
	}
	return h.spawnActor(id, l, dir)
}

// spawnActor 建 actor、登記並啟動；workspaceDir 由呼叫者給定。
func (h *Hub) spawnActor(id task.TaskID, l loadedTask, workspaceDir string) (*actor, error) {
	s := l.state
	live := task.Live{
		Conns:     map[task.SessionID]task.ConnInfo{},
		Worktrees: map[string]bool{},
		Timers:    map[task.AskKey]bool{},
		InFlight:  map[task.EffectKey]bool{},
		Lost:      map[task.SessionID]task.TurnID{},
	}
	// 日誌記著綁定的 Session，這個新起的 actor 都沒有它的連線：行程不在了（D117）。
	for sid, x := range s.Sessions {
		if x.Bound {
			live.Lost[sid] = ""
			if x.Turn != nil {
				live.Lost[sid] = x.Turn.ID
			}
		}
	}
	for _, p := range worktreePaths(s) {
		live.Worktrees[p] = h.git.Exists(p)
	}
	live.WorkspaceDir = workspaceDir
	a := newActor(h, id, s, live, l.segments)
	a.labelSeq, a.drafts = l.labelSeq, l.drafts
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	if existing, ok := h.actors[id]; ok { // 兩個載入者同時到：只能有一個寫入者
		h.mu.Unlock()
		return existing, nil
	}
	h.actors[id] = a
	h.actorWg.Add(1)
	h.mu.Unlock()
	go a.loop()
	return a, nil
}

// dropActor 停掉並移除一個 actor（CreateTask 被拒絕時）。
func (h *Hub) dropActor(a *actor) {
	h.mu.Lock()
	delete(h.actors, a.id)
	h.mu.Unlock()
	a.halt()
}

// worktreePaths 列出 State 中 Task Worktree 與各 Fork 的路徑。
func worktreePaths(s task.State) []string {
	var out []string
	if s.Worktree != nil {
		out = append(out, s.Worktree.Path)
	}
	for _, x := range s.Sessions {
		if x.Fork != nil {
			out = append(out, x.Fork.Path)
		}
	}
	return out
}

// inputs 組裝 Decide 的輸入：時鐘、規則、策略、WorktreeRoot。
// Repo 與 OtherTasksRunning 由呼叫者視命令補上。
func (h *Hub) inputs(ws task.WorkspaceID) task.Inputs {
	in := task.Inputs{
		Now:          h.now(),
		WorktreeRoot: h.worktreeRoot,
	}
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	for _, r := range h.cfg.rules {
		if r.Scope == task.ScopeGlobal ||
			r.Scope == task.ScopeWorkspace && r.ScopeID == string(ws) {
			in.Rules = append(in.Rules, r)
		}
	}
	if w, ok := h.cfg.workspaces[ws]; ok {
		in.Policy = w.Policy
	}
	return in
}

// otherRunning 回傳同一 git 工作樹（D72）內是否有其他在跑的
// Task：actor 的 Workspace 路徑等於 root 或在 root 之下；root
// 為空（非 git）一律不算。只在 CreateTask 與 Isolation 的 goroutine 上呼叫
// （snap 往返），不在 actor 上。exclude 是要建立的 actor（Isolation 沒有，為 nil）。往返時不持 cfg.mu：被問的 Actor 處理排在
// 快照前面的訊息時自己要讀設定快取（inputs）。
func (h *Hub) otherRunning(root string, exclude *actor) bool {
	if root == "" {
		return false
	}
	h.mu.Lock()
	var others []*actor
	for _, a := range h.actors {
		if a != exclude {
			others = append(others, a)
		}
	}
	h.mu.Unlock()
	prefix := root + string(filepath.Separator)
	h.cfg.mu.Lock()
	inside := others[:0]
	for _, a := range others {
		if w, ok := h.cfg.workspaces[a.wsid]; ok && (w.Path == root || strings.HasPrefix(w.Path, prefix)) {
			inside = append(inside, a)
		}
	}
	h.cfg.mu.Unlock()
	return slices.ContainsFunc(inside, (*actor).running)
}

// emit 是轉接器的事實入口：填 Session 與 Origin，組 Inputs 後入隊。
// 阻塞即背壓（A4）；hub 關閉時丟棄。
func (h *Hub) emit(a *actor, sid task.SessionID, c task.Command) {
	c.Session, c.Origin = sid, task.OriginRuntime
	in := h.inputs(a.wsid)
	m := msg{cmd: &c, in: in}
	select {
	case a.mail <- m:
		if h.hooks.fact != nil {
			h.hooks.fact(a.id, c)
		}
	case <-h.ctx.Done():
	case <-a.gone:
	}
}

// newID 產生前綴 + 10 個小寫 base32 字元的 ID。
func newID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString(b[:])[:10])
}

// workspaceName 從目錄名取 Workspace 名稱（D65）：檔名中的換行換成
// 空白再修邊；結果為空（名稱全是換行等）時對整個路徑做同樣處理。
// 回傳值一定符合 task.Label：路徑是絕對的，不可能全是空白。
func workspaceName(dir string) string {
	clean := func(s string) string {
		return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
	}
	if n := clean(filepath.Base(dir)); n != "" {
		return n
	}
	return clean(dir)
}
