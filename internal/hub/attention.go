package hub

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// AttentionKind 是“需要處理”條目的種類（D78）。待確認（遠端，M5）與評審推薦
// （扇出，M4）到產生它們的里程碑再加。
type AttentionKind string

const (
	AttentionAsk       AttentionKind = "ask"       // 等使用者回答的審批
	AttentionSuspended AttentionKind = "suspended" // 失敗而掛起的操作（D71）
)

// AttentionItem 是“需要處理”的一條：Kind 加恰好一個非 nil 的載荷。
// 條數與 TaskView 的 PendingAsks + Failed 之和相同（D68），菜單欄的數字由後者算。
type AttentionItem struct {
	Kind      AttentionKind    `json:"kind"`
	Task      task.TaskID      `json:"task"`
	TaskTitle string           `json:"taskTitle"`
	Workspace task.WorkspaceID `json:"workspace"`
	// Since 是開始等使用者的時間：審批的 AskedAt、掛起的失敗時間。
	Since     time.Time      `json:"since"`
	Ask       *AskItem       `json:"ask,omitzero"`
	Suspended *SuspendedItem `json:"suspended,omitzero"`
}

// AskItem 是一條等使用者回答的審批；回答用 decide_permission，
// Command.Session 是 Session、DecidePermission.Permission 是 Ask.ID。
type AskItem struct {
	Session   task.SessionID `json:"session"`
	Agent     task.AgentID   `json:"agent"`
	AgentName string         `json:"agentName"`
	Ask       task.Ask       `json:"ask"`
}

// SuspendedItem 是一個掛起的操作；M2 只在 Task 裡看原因（D78），重試隨 M4 的 worktree。
type SuspendedItem struct {
	Effect  task.EffectKey `json:"effect"`
	Problem task.Problem   `json:"problem"`
}

// Attention 回傳全部未歸檔 Task 的“需要處理”條目，等得最久的在前（D78）。
// 從投影挑出有條目的 Task，再取它們的 State；不另建投影。
func (h *Hub) Attention(ctx context.Context, p Principal) ([]AttentionItem, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	rows, err := h.index.TaskViews(ctx)
	if err != nil {
		return nil, err
	}
	items := []AttentionItem{}
	for _, v := range rows {
		if v.Archived || v.PendingAsks+v.Failed == 0 {
			continue
		}
		s, err := h.Snapshot(ctx, p, v.ID)
		if err != nil {
			return nil, err
		}
		items = append(items, attentionOf(s)...)
	}
	sortAttention(items)
	return items, nil
}

// attentionOf 是一個 Task 的條目：未決定的審批（與 TaskView.PendingAsks 同一口徑）
// 與 State.Failed；已歸檔的沒有條目。
func attentionOf(s task.State) []AttentionItem {
	if s.Archived {
		return nil
	}
	var items []AttentionItem
	base := AttentionItem{Task: s.ID, TaskTitle: s.Title, Workspace: s.Workspace}
	for _, sid := range s.Order {
		x := s.Sessions[sid]
		for _, a := range x.Pending {
			if a.Decision != nil {
				continue // 已決定，只等送達 agent
			}
			it := base
			it.Kind, it.Since = AttentionAsk, a.AskedAt
			it.Ask = &AskItem{Session: sid, Agent: x.Launch.Agent, AgentName: x.Launch.AgentName, Ask: a}
			items = append(items, it)
		}
	}
	for key, f := range s.Failed {
		it := base
		it.Kind, it.Since = AttentionSuspended, f.At
		it.Suspended = &SuspendedItem{Effect: key, Problem: f.Problem}
		items = append(items, it)
	}
	return items
}

// sortAttention 按開始等待的時間排，同一時刻按 Task 與條目的鍵，結果固定（D07）。
func sortAttention(items []AttentionItem) {
	slices.SortFunc(items, func(a, b AttentionItem) int {
		return cmp.Or(a.Since.Compare(b.Since), cmp.Compare(a.Task, b.Task), cmp.Compare(a.key(), b.key()))
	})
}

// key 是條目在 Task 內的鍵：審批是 Session 與 Permission，掛起的是 EffectKey。
func (it AttentionItem) key() string {
	switch {
	case it.Ask != nil:
		return "ask:" + string(it.Ask.Session) + ":" + string(it.Ask.Ask.ID)
	case it.Suspended != nil:
		return "suspended:" + string(it.Suspended.Effect)
	}
	return ""
}
