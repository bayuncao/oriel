package usage

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// days 是圖上的日曆日數；範圍（Period）是今天、7 天與 30 天（今天和之前 6 天、29 天）。
const days = 30

var periodDays = []int{1, 7, 30}

// Summary 是用量頁要的全部內容（docs/acceptance/m1.md「用量」）。
type Summary struct {
	// Days 是最近 30 個本機時區的日曆日，最早的在前，今天在最後。
	Days []Day `json:"days"`
	// Periods 依序是今天、7 天、30 天的拆分。
	Periods []Period `json:"periods"`
	// Quotas 是各額度桶最新的快照。
	Quotas []Quota `json:"quotas"`
	// Sources 是每個內建 agent 的記錄來源，依 agent 的順序。
	Sources []SourceInfo `json:"sources"`
	// ReadAt 是上一輪讀完的時間；這次啟動還沒讀完過一輪時為零值。
	ReadAt time.Time `json:"readAt,omitzero"`
}

// Day 是一個日曆日每個 agent 的用量。
type Day struct {
	// Date 是本機時區的日期，如 2026-11-02。
	Date   string       `json:"date"`
	Agents []AgentShare `json:"agents"`
}

// AgentShare 是一個 agent 的用量。
type AgentShare struct {
	Agent  task.AgentID `json:"agent"`
	Tokens Tokens       `json:"tokens"`
	// Hub 是其中經 OrielHub 運行的部分（m2.md 第 33 節）；直接使用是 Tokens 減去它。
	Hub Tokens `json:"hub"`
}

// Period 是一段日曆日的總數與拆分，各拆分依總數多到少。
type Period struct {
	Days   int          `json:"days"`
	Total  Tokens       `json:"total"`
	Agents []AgentShare `json:"agents"`
	Models []Share      `json:"models"`
	// Dirs 的 Name 是真實路徑；不知道目錄的用量 Name 為空。
	Dirs []Share `json:"dirs"`
	// Tasks 是經 OrielHub 運行的用量按 Task 的拆分（m2.md U85）；已刪除的 Task 照舊列出它的 ID，
	// 頁面認不得就合成“已刪除的 Task”（U79）。
	Tasks []TaskShare `json:"tasks"`
}

// TaskShare 是一個 Task 經 OrielHub 運行的用量。
type TaskShare struct {
	Task task.TaskID `json:"task"`
	// Agents 是用到的 agent，按用量多到少。
	Agents []task.AgentID `json:"agents"`
	Tokens Tokens         `json:"tokens"`
}

// Share 是按模型或目錄的一份用量；同一個名稱在不同 agent 下分開列。
type Share struct {
	Name   string       `json:"name"`
	Agent  task.AgentID `json:"agent"`
	Tokens Tokens       `json:"tokens"`
}

// SourceInfo 是一個 agent 的記錄來源。
type SourceInfo struct {
	Agent task.AgentID `json:"agent"`
	// Searched 表示這次啟動已找過；還沒找過時 Roots、Files、Problem 是空的。
	Searched bool `json:"searched"`
	// Roots 是找過的位置。
	Roots []string `json:"roots"`
	// Files 是找到的記錄檔數。
	Files int `json:"files"`
	// Bad 是沒讀懂的行數。
	Bad int `json:"bad"`
	// Problem 是讀取失敗的原因（usage.read_failed）。
	Problem    *task.Problem `json:"problem,omitzero"`
	Unreported []Category    `json:"unreported"`
	Notes      []Note        `json:"notes"`
}

// Summary 按現在的本機時區彙總存下的用量（讀過的與以前存下的，D86）。
func (t *Tracker) Summary(ctx context.Context) (Summary, error) {
	t.mu.Lock()
	closed, readAt := t.closed, t.state.ReadAt
	infos := t.infos
	t.mu.Unlock()
	if closed {
		return Summary{}, errClosed
	}
	loc := t.o.Zone()
	now := t.o.Now().In(loc)
	// 每個日界都按本機時區算：夏令時切換的那天不是 24 小時。
	bounds := make([]time.Time, days+1)
	for i := range bounds {
		bounds[i] = time.Date(now.Year(), now.Month(), now.Day()-days+1+i, 0, 0, 0, 0, loc)
	}
	totals, err := t.o.Store.UsageTotals(ctx, bounds)
	if err != nil {
		return Summary{}, err
	}
	quotas, err := t.o.Store.UsageQuotas(ctx)
	if err != nil {
		return Summary{}, err
	}
	out := Summary{Quotas: quotas, ReadAt: readAt}
	if out.Quotas == nil {
		out.Quotas = []Quota{}
	}
	out.Days = make([]Day, days)
	for i := range out.Days {
		out.Days[i] = Day{Date: bounds[i].Format(time.DateOnly), Agents: []AgentShare{}}
	}
	for _, tot := range totals {
		if tot.Day < 0 || tot.Day >= days {
			continue
		}
		out.Days[tot.Day].Agents = addAgent(out.Days[tot.Day].Agents, tot)
	}
	for _, n := range periodDays {
		out.Periods = append(out.Periods, periodOf(totals, n))
	}
	for _, agent := range t.sortedAgents() {
		info, ok := infos[agent]
		if !ok {
			src := t.source(agent)
			info = SourceInfo{Agent: agent, Unreported: src.Unreported(), Notes: src.Notes()}
		}
		if info.Roots == nil {
			info.Roots = []string{}
		}
		if info.Unreported == nil {
			info.Unreported = []Category{}
		}
		if info.Notes == nil {
			info.Notes = []Note{}
		}
		out.Sources = append(out.Sources, info)
	}
	if out.Sources == nil {
		out.Sources = []SourceInfo{}
	}
	return out, nil
}

func (t *Tracker) source(agent task.AgentID) Source {
	for _, s := range t.o.Sources {
		if s.Agent() == agent {
			return s
		}
	}
	return nil
}

// periodOf 是最後 n 個日曆日的總數與拆分。
func periodOf(totals []Total, n int) Period {
	r := Period{Days: n, Agents: []AgentShare{}, Models: []Share{}, Dirs: []Share{}, Tasks: []TaskShare{}}
	// 每個 Task 裡每個 agent 的用量：排出 TaskShare.Agents 的先後。
	perAgent := map[task.TaskID][]AgentShare{}
	for _, tot := range totals {
		if tot.Day < days-n || tot.Day >= days {
			continue
		}
		r.Total = r.Total.Add(tot.Tokens)
		r.Agents = addAgent(r.Agents, tot)
		r.Models = addShare(r.Models, tot.Model, tot.Agent, tot.Tokens)
		r.Dirs = addShare(r.Dirs, tot.Dir, "", tot.Tokens)
		if tot.Task != "" {
			perAgent[tot.Task] = addAgent(perAgent[tot.Task], tot)
		}
	}
	byTotal := func(a, b Tokens) int { return cmp.Compare(b.Total(), a.Total()) }
	byAgent := func(a, b AgentShare) int {
		return cmp.Or(byTotal(a.Tokens, b.Tokens), strings.Compare(string(a.Agent), string(b.Agent)))
	}
	for id, agents := range perAgent {
		slices.SortFunc(agents, byAgent)
		s := TaskShare{Task: id}
		for _, a := range agents {
			s.Agents = append(s.Agents, a.Agent)
			s.Tokens = s.Tokens.Add(a.Tokens)
		}
		r.Tasks = append(r.Tasks, s)
	}
	slices.SortFunc(r.Tasks, func(a, b TaskShare) int {
		return cmp.Or(byTotal(a.Tokens, b.Tokens), strings.Compare(string(a.Task), string(b.Task)))
	})
	slices.SortStableFunc(r.Agents, byAgent)
	for _, s := range [][]Share{r.Models, r.Dirs} {
		slices.SortStableFunc(s, func(a, b Share) int {
			return cmp.Or(byTotal(a.Tokens, b.Tokens), strings.Compare(a.Name, b.Name),
				strings.Compare(string(a.Agent), string(b.Agent)))
		})
	}
	return r
}

// addAgent 把一組用量加到它的 agent 上；經 OrielHub 的（有 Task）也加進 Hub。
func addAgent(list []AgentShare, tot Total) []AgentShare {
	var hub Tokens
	if tot.Task != "" {
		hub = tot.Tokens
	}
	for i := range list {
		if list[i].Agent == tot.Agent {
			list[i].Tokens = list[i].Tokens.Add(tot.Tokens)
			list[i].Hub = list[i].Hub.Add(hub)
			return list
		}
	}
	return append(list, AgentShare{Agent: tot.Agent, Tokens: tot.Tokens, Hub: hub})
}

func addShare(list []Share, name string, agent task.AgentID, t Tokens) []Share {
	for i := range list {
		if list[i].Name == name && list[i].Agent == agent {
			list[i].Tokens = list[i].Tokens.Add(t)
			return list
		}
	}
	return append(list, Share{Name: name, Agent: agent, Tokens: t})
}
