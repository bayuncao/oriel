package hub

import (
	"maps"
	"strconv"

	"github.com/bayuncao/oriel/internal/task"
)

// notifications 從已提交的一批事件與事後 State 產生系統通知。
// 同一批被規則決定的 ask 不再通知（post-state 已無 pending）。
func notifications(s task.State, events []task.Event) []Notification {
	var out []Notification
	for _, e := range events {
		n := Notification{Task: s.ID, TaskTitle: s.Title, Session: e.Session}
		switch e.Kind {
		case task.PermissionAskedKind:
			a := e.PermissionAsked
			sess, ok := s.Sessions[e.Session]
			still := false
			if ok {
				if cur, has := sess.Pending[a.ID]; has && cur.Decision == nil {
					still = true
				}
			}
			if !still {
				continue
			}
			n.Kind = NotifyAsk
			n.Params = map[string]string{"tool": a.Request.Tool}
			if ok {
				n.Params["agent"] = agentLabel(sess.Launch)
			}
			if a.Request.Title != "" {
				n.Params["title"] = a.Request.Title
			}
			if a.Request.Command != "" {
				n.Params["command"] = a.Request.Command
			}
		case task.TurnFailedKind:
			n.Kind = NotifyFailed
			p := e.TurnFailed.Problem
			p.Params = maps.Clone(p.Params)
			n.Reason = &p
		case task.ForkRecommendedKind:
			n.Kind = NotifyRecommended
			n.Params = map[string]string{
				"winner": string(e.ForkRecommended.Winner)}
		case task.ConfirmationRequestedKind:
			n.Kind = NotifyConfirmation
			n.Params = map[string]string{
				"kind": string(e.ConfirmationRequested.Command.Kind)}
		case task.MergeConflictedKind:
			n.Kind = NotifyConflict
			n.Params = map[string]string{
				"files": strconv.Itoa(len(e.MergeConflicted.Files))}
		case task.TurnCompletedKind:
			n.Kind = NotifyCompleted
		default:
			continue
		}
		out = append(out, n)
	}
	return out
}

// agentLabel 是通知裡的 agent：顯示名稱，沒有時用 ID。
func agentLabel(l task.LaunchSnapshot) string {
	if l.AgentName != "" {
		return l.AgentName
	}
	return string(l.Agent)
}

// notify 在 Append 成功後發這批事件的通知；設置整批讀一次。
func (a *actor) notify(events []task.Event) {
	if a.h.notifier == nil {
		return
	}
	st := a.h.notifySettings()
	for _, n := range notifications(a.state, events) {
		a.deliver(st, n)
	}
}

// notifySettings 是目前的設置（讀快取）。
func (h *Hub) notifySettings() Settings {
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	return h.cfg.settings
}

// deliver 按設置發一條通知：FIFO channel 交給唯一的 dispatcher；滿了就丟（盡力而為，不擋 Actor）。
// 呼叫者先確認有 Notifier。
func (a *actor) deliver(st Settings, n Notification) {
	h := a.h
	if !st.Notifies(n.Kind) {
		return
	}
	select {
	case h.notes <- n:
	default:
		h.logger.Debug("hub: notification dropped",
			"task", a.id, "kind", n.Kind)
	}
}

// notifyLoop 是唯一的通知送出者，保證 FIFO 順序。
func (h *Hub) notifyLoop() {
	defer h.notifyWg.Done()
	for {
		select {
		case n := <-h.notes:
			if err := h.notifier.Notify(h.ctx, n); err != nil &&
				h.ctx.Err() == nil {
				h.logger.Debug("hub: notify failed", "kind", n.Kind, "err", err)
			}
		case <-h.ctx.Done():
			return
		}
	}
}
