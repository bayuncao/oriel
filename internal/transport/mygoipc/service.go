// Package mygoipc 是頁面與後端之間的 IPC 服務層：
// 各 bound service（Hub、Agents、Desktop、Keys、Providers、Usage、Integrations、Remote）把後端的方法
// 包成頁面可用的形狀，錯誤一律經 transport.Wire。
package mygoipc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
)

// 事件：頁面訂閱。
var (
	// AgentsChanged 在發現表格變化時廣播最新的 List。
	AgentsChanged = mygo.NewEvent[[]agent.AgentRow]("agents:changed")
	// OpenTask 在通知被點擊時廣播。
	OpenTask = mygo.NewEvent[OpenTaskEvent]("open:task")
)

// OpenTaskEvent 是 open:task 的 payload。
type OpenTaskEvent struct {
	Task task.TaskID `json:"task"`
}

// AgentSource 是 Agents 服務的後端；app 的 *Agents 滿足它。
// 如此 mygoipc 不依賴 app（組合根依賴 transport，不能反過來）。
type AgentSource interface {
	Definitions() []agent.Definition
	Installations() []agent.Installation
	Refresh(ctx context.Context) error
	Registry(ctx context.Context, force bool) (agent.RegistryList, error)
}

// late 讓 service 在後端就緒前先被 Bind（mygo generate）：後端 T 與 log
// 在同一把鎖下一起接上、一起取得；接上之前（或接上的是零值）get 回 mygoipc.not_ready。
type late[T comparable] struct {
	mu  sync.Mutex
	src T
	log *slog.Logger
}

func (l *late[T]) connect(src T, log *slog.Logger) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.src, l.log = src, log
}

func (l *late[T]) get() (T, *slog.Logger, *transport.Error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var zero T
	if l.src == zero {
		e, _ := transport.Wire(errNotReady)
		return zero, nil, e
	}
	return l.src, l.log, nil
}

// ConnectHub 給 Hub 服務接上後端。是包級函式而非方法：綁定型別的
// 每個導出方法都是頁面可呼叫的 API（D61）。
func ConnectHub(s *Hub, h *hub.Hub, log *slog.Logger) { s.connect(h, log) }

// ConnectAgents 給 Agents 服務接上後端（D61）。
func ConnectAgents(s *Agents, src AgentSource, log *slog.Logger) { s.connect(src, log) }

// wire 把任何錯誤轉成給前端的 *transport.Error；
// 沒代碼的錯誤記原文在 Error 級。
func wire(log *slog.Logger, method string, err error) *transport.Error {
	e, known := transport.Wire(err)
	if !known && log != nil {
		log.Error("mygoipc: uncoded error", "method", method, "err", err)
	}
	return e
}

// Hub 是任務相關的 IPC 服務（BindAs "Hub"）。
type Hub struct{ late[*hub.Hub] }

// CreateTask 建立 Task；agent 非空時帶一個用它的預設 Profile 的 Session，
// 說明是第一個 Prompt（M2-2）。頁面只給 agent ID，不給 Launch Snapshot（D57）。
func (s *Hub) CreateTask(ctx context.Context, dir, title, brief string, agent task.AgentID) (task.TaskID, error) {
	h, log, e := s.get()
	if e != nil {
		return "", e
	}
	id, _, err := h.CreateTaskWithAgent(ctx, hub.LocalOwner, dir,
		task.CreateTask{Title: title, Brief: brief}, agent)
	if err != nil {
		return "", wire(log, "CreateTask", err)
	}
	return id, nil
}

// StartAgent 給還沒有 Session 的 Task 啟動 agent，message 是它的第一個 Prompt
// （m2.md I05–I11，D104）。同 CreateTask，頁面只給 agent ID，不給 Launch
// Snapshot（D57）；create_session 仍在 Command 被拒絕（I12）。
func (s *Hub) StartAgent(ctx context.Context, id task.TaskID, agent task.AgentID, message string) (task.Seq, error) {
	h, log, e := s.get()
	if e != nil {
		return 0, e
	}
	seq, err := h.CreateSessionWithAgent(ctx, hub.LocalOwner, id, agent, task.Prompt{Text: message})
	if err != nil {
		return 0, wire(log, "StartAgent", err)
	}
	return seq, nil
}

func (s *Hub) Command(ctx context.Context, id task.TaskID, c task.Command) (task.Seq, error) {
	h, log, e := s.get()
	if e != nil {
		return 0, e
	}
	// 頁面不能提供 LaunchSnapshot（D57）：M2 由 hub 解析 Profile。
	switch c.Kind {
	case task.CmdCreateSession, task.CmdFanOut:
		er, _ := transport.Wire(codeError{CodeNeedsProfile})
		return 0, er
	}
	seq, err := h.Command(ctx, hub.LocalOwner, id, c)
	if err != nil {
		return 0, wire(log, "Command", err)
	}
	return seq, nil
}

func (s *Hub) Subscribe(ctx context.Context, id task.TaskID, from task.Seq,
	updates *mygo.Channel[hub.Update]) error {
	h, log, e := s.get()
	if e != nil {
		return e
	}
	if err := pageLeft(ctx, h.Subscribe(ctx, hub.LocalOwner, id, from, updates.Send)); err != nil {
		return wire(log, "Subscribe", err)
	}
	return nil
}

func (s *Hub) WatchTasks(ctx context.Context,
	rows *mygo.Channel[hub.TaskView]) error {
	h, log, e := s.get()
	if e != nil {
		return e
	}
	if err := pageLeft(ctx, h.WatchTasks(ctx, hub.LocalOwner, rows.Send)); err != nil {
		return wire(log, "WatchTasks", err)
	}
	return nil
}

func (s *Hub) Tasks(ctx context.Context) ([]hub.TaskView, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	v, err := h.Tasks(ctx, hub.LocalOwner)
	if err != nil {
		return nil, wire(log, "Tasks", err)
	}
	return v, nil
}

func (s *Hub) Snapshot(ctx context.Context, id task.TaskID) (task.State, error) {
	h, log, e := s.get()
	if e != nil {
		return task.State{}, e
	}
	v, err := h.Snapshot(ctx, hub.LocalOwner, id)
	if err != nil {
		return task.State{}, wire(log, "Snapshot", err)
	}
	return v, nil
}

// Attention 是“需要處理”的條目，等得最久的在前（D78）。
func (s *Hub) Attention(ctx context.Context) ([]hub.AttentionItem, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	v, err := h.Attention(ctx, hub.LocalOwner)
	if err != nil {
		return nil, wire(log, "Attention", err)
	}
	return v, nil
}

func (s *Hub) Confirmations(ctx context.Context) ([]hub.ConfirmationView, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	v, err := h.Confirmations(ctx, hub.LocalOwner)
	if err != nil {
		return nil, wire(log, "Confirmations", err)
	}
	return v, nil
}

func (s *Hub) Search(ctx context.Context, q string, limit int) ([]hub.SearchHit, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	v, err := h.Search(ctx, hub.LocalOwner, q, limit)
	if err != nil {
		return nil, wire(log, "Search", err)
	}
	return v, nil
}

// Files 是文件頁的資料：Task 用過的檔案（D107）。
func (s *Hub) Files(ctx context.Context) ([]hub.FileView, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	v, err := h.Files(ctx, hub.LocalOwner)
	if err != nil {
		return nil, wire(log, "Files", err)
	}
	return v, nil
}

// Models 回傳各 agent 最近報告的模型與預設模型（設定 › 模型，settings.md MD01–MD03）。
func (s *Hub) Models(ctx context.Context) ([]hub.AgentModels, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	out, err := h.Models(ctx, hub.LocalOwner)
	if err != nil {
		return nil, wire(log, "Models", err)
	}
	return out, nil
}

// SetDefaultModel 設 agent 的預設模型；"" 是跟隨 agent 自己的設定（MD03、MD04、MD24）。
func (s *Hub) SetDefaultModel(ctx context.Context, agent task.AgentID, model string) error {
	h, log, e := s.get()
	if e != nil {
		return e
	}
	if err := h.SetDefaultModel(ctx, hub.LocalOwner, agent, model); err != nil {
		return wire(log, "SetDefaultModel", err)
	}
	return nil
}

// Recap 是首頁“離開期間”的資料（D115）：since 之後結束的回合，最早 7 天前。
func (s *Hub) Recap(ctx context.Context, since time.Time) ([]hub.RecapTurn, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	v, err := h.Recap(ctx, hub.LocalOwner, since)
	if err != nil {
		return nil, wire(log, "Recap", err)
	}
	return v, nil
}

func (s *Hub) Workspaces(ctx context.Context) ([]hub.Workspace, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	v, err := h.Workspaces(ctx, hub.LocalOwner)
	if err != nil {
		return nil, wire(log, "Workspaces", err)
	}
	return v, nil
}

func (s *Hub) Rules(ctx context.Context) ([]task.Rule, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	v, err := h.Rules(ctx, hub.LocalOwner)
	if err != nil {
		return nil, wire(log, "Rules", err)
	}
	return v, nil
}

func (s *Hub) Settings(ctx context.Context) (hub.Settings, error) {
	h, log, e := s.get()
	if e != nil {
		return hub.Settings{}, e
	}
	v, err := h.Settings(ctx, hub.LocalOwner)
	if err != nil {
		return hub.Settings{}, wire(log, "Settings", err)
	}
	return v, nil
}

func (s *Hub) PutWorkspace(ctx context.Context, w hub.Workspace) error {
	h, log, e := s.get()
	if e != nil {
		return e
	}
	if err := h.PutWorkspace(ctx, hub.LocalOwner, w); err != nil {
		return wire(log, "PutWorkspace", err)
	}
	return nil
}

func (s *Hub) DeleteRule(ctx context.Context, id task.RuleID) error {
	h, log, e := s.get()
	if e != nil {
		return e
	}
	if err := h.DeleteRule(ctx, hub.LocalOwner, id); err != nil {
		return wire(log, "DeleteRule", err)
	}
	return nil
}

// DeleteTask 徹底刪除一個已歸檔的 Task（D15，m2.md DL01–DL10）；不能恢復。
func (s *Hub) DeleteTask(ctx context.Context, id task.TaskID) error {
	h, log, e := s.get()
	if e != nil {
		return e
	}
	if err := h.DeleteTask(ctx, hub.LocalOwner, id); err != nil {
		return wire(log, "DeleteTask", err)
	}
	return nil
}

// Labels 是標籤目錄（D135），按 ID 排序。
func (s *Hub) Labels(ctx context.Context) ([]hub.Label, error) {
	h, log, e := s.get()
	if e != nil {
		return nil, e
	}
	v, err := h.Labels(ctx, hub.LocalOwner)
	if err != nil {
		return nil, wire(log, "Labels", err)
	}
	return v, nil
}

// PutLabel 新建（ID 為空）或改掉一個標籤，回傳存下的標籤。
func (s *Hub) PutLabel(ctx context.Context, l hub.Label) (hub.Label, error) {
	h, log, e := s.get()
	if e != nil {
		return hub.Label{}, e
	}
	v, err := h.PutLabel(ctx, hub.LocalOwner, l)
	if err != nil {
		return hub.Label{}, wire(log, "PutLabel", err)
	}
	return v, nil
}

// DeleteLabel 從目錄刪掉一個標籤。
func (s *Hub) DeleteLabel(ctx context.Context, id task.LabelID) error {
	h, log, e := s.get()
	if e != nil {
		return e
	}
	if err := h.DeleteLabel(ctx, hub.LocalOwner, id); err != nil {
		return wire(log, "DeleteLabel", err)
	}
	return nil
}

func (s *Hub) PutSettings(ctx context.Context, x hub.Settings) error {
	h, log, e := s.get()
	if e != nil {
		return e
	}
	if err := h.PutSettings(ctx, hub.LocalOwner, x); err != nil {
		return wire(log, "PutSettings", err)
	}
	return nil
}

// Agents 是 Agent 目錄的 IPC 服務（BindAs "Agents"）。
type Agents struct{ late[AgentSource] }

func (s *Agents) List(ctx context.Context) ([]agent.AgentRow, error) {
	g, _, e := s.get()
	if e != nil {
		return nil, e
	}
	return agent.Rows(g.Definitions(), g.Installations()), nil
}

func (s *Agents) Refresh(ctx context.Context) error {
	g, log, e := s.get()
	if e != nil {
		return e
	}
	if err := g.Refresh(ctx); err != nil {
		return wire(log, "Refresh", err)
	}
	return nil
}

func (s *Agents) Registry(ctx context.Context, refresh bool) (agent.RegistryList, error) {
	g, log, e := s.get()
	if e != nil {
		return agent.RegistryList{}, e
	}
	v, err := g.Registry(ctx, refresh)
	if err != nil {
		return agent.RegistryList{}, wire(log, "Registry", err)
	}
	return v, nil
}

// BroadcastChanges 跟著 Discovery 的 changes channel 廣播
// agents:changed；channel 關閉時結束。由 main 在後端就緒後啟動。
func BroadcastChanges(changes <-chan []agent.Installation, src AgentSource) {
	for range changes {
		AgentsChanged.Broadcast(agent.Rows(src.Definitions(), src.Installations()))
	}
}

// pageLeft 把頁面離開造成的錯誤（Send 回 ErrChannelClosed，或 ctx 已取消時的任何錯誤）當成正常結束：
// 每個把 mygo.Channel 的 Send 接成後端 Watch／Subscribe 的方法都經過它。
func pageLeft(ctx context.Context, err error) error {
	if errors.Is(err, mygo.ErrChannelClosed) || (err != nil && ctx.Err() != nil) {
		return nil
	}
	return err
}
