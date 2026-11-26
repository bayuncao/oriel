package hub

// models.go：各 agent 最近報告的模型與預設模型（D158，docs/acceptance/settings.md 第 4 節）。

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// AgentModels 是一個 agent 的模型：最近一次會話報告的列表（MD01、MD02）與使用者設的預設（MD03）。
type AgentModels struct {
	Agent task.AgentID `json:"agent"`
	// Choices 是最近一次報告的可選模型；還沒報告過時是空的（MD23）。
	Choices []task.ModelChoice `json:"choices"`
	// Current 是報告時的模型。
	Current string `json:"current,omitzero"`
	// ReportedAt 是報告的時間（事件的 At）；還沒報告過時是零值。
	ReportedAt time.Time `json:"reportedAt,omitzero"`
	// Default 是新會話開始時要換成的模型；"" 是跟隨 agent 自己的設定（MD04）。
	Default string `json:"default,omitzero"`
}

// Models 回傳各 agent 的模型，依 agent ID 排序。
func (h *Hub) Models(ctx context.Context, p Principal) ([]AgentModels, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	return sortedModels(h.cfg.models), nil
}

// SetDefaultModel 設 agent 的預設模型（MD03、MD04），只有桌面能設。model 去掉首尾空白，"" 清掉；
// 不在最近報告的列表裡也照存（列表可能過時，MD11）。不認得的 agent 回 agent.unknown_agent（經 Launcher）。
// 只影響之後新建的會話（MD09）。
func (h *Hub) SetDefaultModel(ctx context.Context, p Principal, agent task.AgentID, model string) error {
	if err := h.alive(); err != nil {
		return err
	}
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	if h.launcher == nil {
		return fail(CodeAgentUnsupported, nil, "agent", string(agent))
	}
	if err := h.launcher.CheckAgent(agent); err != nil {
		return err
	}
	model = strings.TrimSpace(model)
	return h.updateModels(ctx, agent, func(m *AgentModels) { m.Default = model })
}

// defaultModel 是新會話要用的模型（MD05）；沒設時是 ""。
func (h *Hub) defaultModel(agent task.AgentID) string {
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	return h.cfg.models[agent].Default
}

// recordModels 把這批事件裡各 agent 報告的模型（MD01、MD02）交給 reportLoop 寫進設定；只看帶設定的報告。
// Actor 不拿 cfgWrite：CreateTask 登記新 Workspace 時拿著 cfgWrite 等 actor 回答（otherRunning），
// actor 若在這裡等 cfgWrite 就兩邊互等。
func (a *actor) recordModels(events []task.Event) {
	for _, e := range events {
		if e.Kind != task.ModelReportedKind || e.ModelReported == nil || e.ModelReported.Setting == nil {
			continue
		}
		x, ok := a.state.Sessions[e.Session]
		if !ok {
			continue
		}
		set := e.ModelReported.Setting
		a.h.reports.add(x.Launch.Agent, modelReport{choices: slices.Clone(set.Choices), current: set.Current, at: e.At})
	}
}

// modelReport 是一次報告裡要記下的部分。
type modelReport struct {
	choices []task.ModelChoice
	current string
	at      time.Time
}

// apply 把報告記到 m 上。空的列表不蓋掉記下的：換模型的回覆在 agent 沒給過列表時不帶列表，
// 那不表示可選的變了（否則已設的預設在設定頁上清不掉）。
func (r modelReport) apply(m *AgentModels) {
	if len(r.choices) > 0 {
		m.Choices = r.choices
	}
	m.Current, m.ReportedAt = r.current, r.at
}

// modelReports 是 actor 交來、還沒寫進設定的報告，每個 agent 只留合併後的一筆。
type modelReports struct {
	mu      sync.Mutex
	pending map[task.AgentID]modelReport
	busy    bool          // reportLoop 正在寫
	idle    chan struct{} // 每寫完一輪關閉並換新
	wake    chan struct{} // 容量 1：有新報告
	stop    chan struct{} // Close 在 actor 都停了之後關閉
	wg      sync.WaitGroup
}

func (r *modelReports) init() {
	r.pending = map[task.AgentID]modelReport{}
	r.idle = make(chan struct{})
	r.wake = make(chan struct{}, 1)
	r.stop = make(chan struct{})
}

// add 記下一筆報告，不等寫入。
func (r *modelReports) add(agent task.AgentID, rep modelReport) {
	r.mu.Lock()
	if prev, ok := r.pending[agent]; ok && len(rep.choices) == 0 {
		rep.choices = prev.choices
	}
	r.pending[agent] = rep
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// reportLoop 是唯一寫模型報告的 goroutine；Close 時寫完手上的才返回。
func (h *Hub) reportLoop() {
	defer h.reports.wg.Done()
	for {
		select {
		case <-h.reports.wake:
			h.writeReports()
		case <-h.reports.stop:
			h.writeReports()
			return
		}
	}
}

// writeReports 寫下目前收到的報告。寫不進去只記日誌：這是給設定頁看的快取，不影響對話。
// 關閉時 h.ctx 已取消，所以用不跟著取消的 ctx，讓最後一輪也寫得進去。
func (h *Hub) writeReports() {
	r := &h.reports
	r.mu.Lock()
	batch := r.pending
	r.pending = map[task.AgentID]modelReport{}
	r.busy = true
	r.mu.Unlock()
	ctx := context.WithoutCancel(h.ctx)
	for agent, rep := range batch {
		if err := h.updateModels(ctx, agent, rep.apply); err != nil {
			h.logger.Warn("hub: recording reported models failed", "agent", agent, "err", err)
		}
	}
	r.mu.Lock()
	r.busy = false
	close(r.idle)
	r.idle = make(chan struct{})
	r.mu.Unlock()
}

// waitReports 等到交來的報告都寫完（測試用）。
func (h *Hub) waitReports() {
	r := &h.reports
	for {
		r.mu.Lock()
		done, idle := len(r.pending) == 0 && !r.busy, r.idle
		r.mu.Unlock()
		if done {
			return
		}
		<-idle
	}
}

// updateModels 改一個 agent 的模型並存下；改完與原來一樣時不寫（報告每次帶新的 ReportedAt，所以照寫）。
// 寫在 cfgWrite 下序列化，存好才換掉快取。不可在 actor 裡呼叫（見 recordModels）。
func (h *Hub) updateModels(ctx context.Context, agent task.AgentID, change func(*AgentModels)) error {
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	h.cfg.mu.Lock()
	old, had := h.cfg.models[agent]
	next := old
	next.Agent = agent
	next.Choices = slices.Clone(old.Choices)
	change(&next)
	if had && reflect.DeepEqual(old, next) {
		h.cfg.mu.Unlock()
		return nil
	}
	all := maps.Clone(h.cfg.models)
	h.cfg.mu.Unlock()
	all[agent] = next
	if err := h.config.PutModels(ctx, sortedModels(all)); err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	h.cfg.models[agent] = next
	h.cfg.mu.Unlock()
	return nil
}

// ForgetAgent 刪掉 agent 記下的模型與預設模型（m4.md AC22：移除 Registry 條目或自訂 agent 時，由 app 呼叫）。
// 沒記過時什麼都不做；與 updateModels 同在 cfgWrite 下。之後它留下的 Session 再報告模型時照常記下
// （只有列表，沒有預設）。
func (h *Hub) ForgetAgent(ctx context.Context, agent task.AgentID) error {
	if err := h.alive(); err != nil {
		return err
	}
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	h.cfg.mu.Lock()
	if _, had := h.cfg.models[agent]; !had {
		h.cfg.mu.Unlock()
		return nil
	}
	all := maps.Clone(h.cfg.models)
	h.cfg.mu.Unlock()
	delete(all, agent)
	if err := h.config.PutModels(ctx, sortedModels(all)); err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	delete(h.cfg.models, agent)
	h.cfg.mu.Unlock()
	return nil
}

func sortedModels(m map[task.AgentID]AgentModels) []AgentModels {
	out := slices.Collect(maps.Values(m))
	slices.SortFunc(out, func(a, b AgentModels) int { return strings.Compare(string(a.Agent), string(b.Agent)) })
	if out == nil {
		out = []AgentModels{}
	}
	for i := range out {
		if out[i].Choices == nil {
			out[i].Choices = []task.ModelChoice{}
		}
	}
	return out
}
