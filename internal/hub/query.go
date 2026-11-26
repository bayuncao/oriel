package hub

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// --- 讀端：任何 principal 都可以讀；索引與快取都不經 actor ---

// Tasks 回傳投影的 Task 列表。
func (h *Hub) Tasks(ctx context.Context, p Principal) ([]TaskView, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	return h.index.TaskViews(ctx)
}

// Snapshot 回傳 Task 的 State 快照；未載入時即場重放。
func (h *Hub) Snapshot(ctx context.Context, p Principal, id task.TaskID) (task.State, error) {
	if err := h.alive(); err != nil {
		return task.State{}, err
	}
	a, err := h.loadActor(ctx, id)
	if err != nil {
		return task.State{}, err
	}
	ch := make(chan snapshot, 1)
	select {
	case a.mail <- msg{snap: ch}:
	case <-a.gone:
		return task.State{}, a.goneErr()
	case <-h.ctx.Done():
		return task.State{}, ErrClosed
	case <-ctx.Done():
		return task.State{}, ctx.Err()
	}
	select {
	case snap := <-ch:
		return snap.State, nil
	case <-a.gone:
		return task.State{}, a.goneErr()
	case <-h.ctx.Done():
		return task.State{}, ErrClosed
	case <-ctx.Done():
		return task.State{}, ctx.Err()
	}
}

// SecretsInUse 回傳 agent 的 Launch Snapshot（Task 的各 Session 與待審查的審查者，含已歸檔的 Task）引用的
// 密鑰（m4.md AC37–AC39：app 據此決定待清理的密鑰能不能刪）；成功時不是 nil。讀的是隨事件寫入的投影
// （Views.Secrets），不重放事件。
func (h *Hub) SecretsInUse(ctx context.Context, agent task.AgentID) (map[string]bool, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	keys, err := h.index.SecretsInUse(ctx, agent)
	if err != nil {
		return nil, fail(CodeStoreFailed, err, "detail", err.Error())
	}
	used := make(map[string]bool, len(keys))
	for _, k := range keys {
		used[k] = true
	}
	return used, nil
}

// Confirmations 回傳全部 Task 的待確認。
func (h *Hub) Confirmations(ctx context.Context, p Principal) ([]ConfirmationView, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	return h.index.Confirmations(ctx)
}

// 全文檢索的命中上限：不給（limit <= 0）時是搜尋面板顯示的條數（D67），最多 500。
const (
	defaultSearchLimit = 20
	maxSearchLimit     = 500
)

// Search 是全文檢索；上限見 searchLimit。
func (h *Hub) Search(ctx context.Context, p Principal, q string, limit int) ([]SearchHit, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	return h.index.Search(ctx, q, searchLimit(limit))
}

// Files 是文件頁的資料（D107）：每個 Workspace 裡每個用過的路徑一行，跨 Task 合併——
// 方式、時間與位置取最後用它的 Task，改過則算改過；已歸檔的 Task 與已不在的 Task 不算。
// 按 At 新到舊、同刻按 Workspace 與 Path 升冪。
func (h *Hub) Files(ctx context.Context, p Principal) ([]FileView, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	rows, err := h.index.Files(ctx)
	if err != nil {
		return nil, err
	}
	type key struct {
		ws   task.WorkspaceID
		path string
	}
	merged := map[key]*FileView{}
	for _, r := range rows {
		k := key{r.Workspace, r.Use.Path}
		f := merged[k]
		if f == nil {
			f = &FileView{Workspace: r.Workspace, Path: r.Use.Path}
			merged[k] = f
		}
		f.Tasks++
		f.Edited = f.Edited || r.Use.Edited
		// 同刻時取 Task ID 較大的，結果不依 rows 的順序。
		if f.Task == "" || r.Use.At.After(f.At) || (r.Use.At.Equal(f.At) && r.Task > f.Task) {
			f.Task, f.At, f.Via, f.Outside = r.Task, r.Use.At, r.Use.Via, r.Use.Outside
		}
	}
	out := make([]FileView, 0, len(merged))
	for _, f := range merged {
		out = append(out, *f)
	}
	slices.SortFunc(out, func(a, b FileView) int {
		if c := b.At.Compare(a.At); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.Workspace), string(b.Workspace)); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out, nil
}

// recapWindow 是 Hub.Recap 最早看多久以前（D115，m2.md R02）。
const recapWindow = 7 * 24 * time.Hour

// Recap 是首頁“離開期間”的資料（D115）：未歸檔的 Task 在 since 之後（含）結束的回合（完成或失敗），
// 新到舊，同刻按 Task、Session、回合升冪；since 早於 7 天前時按 7 天前算。
func (h *Hub) Recap(ctx context.Context, p Principal, since time.Time) ([]RecapTurn, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	if earliest := h.now().Add(-recapWindow); since.Before(earliest) {
		since = earliest
	}
	out, err := h.index.Recap(ctx, since)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b RecapTurn) int {
		if c := b.EndedAt.Compare(a.EndedAt); c != 0 {
			return c
		}
		return cmp.Or(strings.Compare(string(a.Task), string(b.Task)),
			strings.Compare(string(a.Session), string(b.Session)), strings.Compare(string(a.Turn), string(b.Turn)))
	})
	if out == nil {
		out = []RecapTurn{}
	}
	return out, nil
}

// searchLimit 是實際的命中上限：limit <= 0 取 defaultSearchLimit，超過 maxSearchLimit 取 maxSearchLimit。
func searchLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultSearchLimit
	case limit > maxSearchLimit:
		return maxSearchLimit
	}
	return limit
}

// Workspaces 回傳快取裡的 Workspace，按 Path 升冪。
func (h *Hub) Workspaces(ctx context.Context, p Principal) ([]Workspace, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	h.cfg.mu.Lock()
	out := make([]Workspace, 0, len(h.cfg.workspaces))
	for _, w := range h.cfg.workspaces {
		out = append(out, w)
	}
	h.cfg.mu.Unlock()
	slices.SortFunc(out, func(a, b Workspace) int {
		return strings.Compare(a.Path, b.Path)
	})
	return out, nil
}

// Rules 回傳快取裡的 Rule，按 ID 升冪。
func (h *Hub) Rules(ctx context.Context, p Principal) ([]task.Rule, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	h.cfg.mu.Lock()
	out := slices.Clone(h.cfg.rules)
	h.cfg.mu.Unlock()
	slices.SortFunc(out, func(a, b task.Rule) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return out, nil
}

// Settings 回傳快取的設定。
func (h *Hub) Settings(ctx context.Context, p Principal) (Settings, error) {
	var out Settings
	if err := h.alive(); err != nil {
		return out, err
	}
	h.cfg.mu.Lock()
	out = h.cfg.settings
	h.cfg.mu.Unlock()
	return out, nil
}

// --- 寫端：只有 desktop 能寫，全部在 cfgWrite 下序列化 ---

func (h *Hub) needsDesktop(p Principal) error {
	if p.Origin() != task.OriginDesktop {
		return fail(CodeNeedsDesktop, nil)
	}
	return nil
}

// PutWorkspace 改名或改隔離策略；Path 與 LastUsedAt 不變。
func (h *Hub) PutWorkspace(ctx context.Context, p Principal, w Workspace) error {
	if err := h.alive(); err != nil {
		return err
	}
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	name, ok := task.Label(w.Name)
	if !ok {
		return fail(CodeInvalidField, nil, "field", "name")
	}
	w.Name = name
	switch w.Policy {
	case task.PolicyAuto, task.PolicyAlways, task.PolicyNever:
	default:
		return fail(CodeInvalidField, nil, "field", "policy")
	}
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	h.cfg.mu.Lock()
	cur, ok := h.cfg.workspaces[w.ID]
	h.cfg.mu.Unlock()
	if !ok {
		return fail(CodeUnknownWorkspace, nil, "path", string(w.ID))
	}
	cur.Name, cur.Policy = w.Name, w.Policy
	if err := h.config.PutWorkspace(ctx, cur); err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	h.cfg.workspaces[w.ID] = cur
	h.cfg.mu.Unlock()
	return nil
}

// DeleteRule 移除一條 Rule；未知 ID 是 hub.unknown_rule。
func (h *Hub) DeleteRule(ctx context.Context, p Principal, id task.RuleID) error {
	if err := h.alive(); err != nil {
		return err
	}
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	match := func(r task.Rule) bool { return r.ID == id }
	h.cfg.mu.Lock()
	known := slices.ContainsFunc(h.cfg.rules, match)
	h.cfg.mu.Unlock()
	if !known {
		return fail(CodeUnknownRule, nil, "rule", string(id))
	}
	if err := h.config.DeleteRule(ctx, id); err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	// 解鎖期間 actor 的 Append 可能經 cacheApply 改了規則列表：按 ID 刪，不沿用先前的位置。
	h.cfg.mu.Lock()
	h.cfg.rules = slices.DeleteFunc(h.cfg.rules, match)
	h.cfg.mu.Unlock()
	return nil
}

// PutSettings 寫入設定。
func (h *Hub) PutSettings(ctx context.Context, p Principal, x Settings) error {
	if err := h.alive(); err != nil {
		return err
	}
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	if err := h.config.PutSettings(ctx, x); err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	h.cfg.settings = x
	h.cfg.mu.Unlock()
	return nil
}
