package task

import (
	"maps"
	"slices"
)

// Plan 比較 State 所要求的與 Live 所觀察到的，回傳要執行的 Effect（docs/architecture/domain.md「Plan 與 Effect」）。
// 純函式、不讀時鐘、結果順序固定：Task 級（create_worktree:task、prepare_review）→ 按 Order 每個 Session → 意圖按 ID。
// 跳過 Live.InFlight 與 State.Failed 中的 key；歸檔的 Task 只產出 close_conn。
func Plan(s State, l Live) []Effect {
	p := &planner{s: s, l: l}
	if s.Archived {
		for _, sid := range s.Order {
			if _, ok := l.Conns[sid]; ok {
				p.emit(Effect{Kind: EffCloseConn, Session: sid}, string(sid))
			}
		}
		return p.out
	}
	p.taskLevel()
	for _, sid := range s.Order {
		p.session(sid)
	}
	p.intents()
	return p.out
}

type planner struct {
	s   State
	l   Live
	out []Effect
}

// emit 記錄 Effect；Key 由 kind 與對象組成。已在執行或掛起的跳過。
func (p *planner) emit(e Effect, object string) {
	e.Key = newEffectKey(e.Kind, object)
	if p.l.InFlight[e.Key] {
		return
	}
	if _, ok := p.s.Failed[e.Key]; ok {
		return
	}
	p.out = append(p.out, e)
}

// workdir 是 Session 的工作目錄與它是否存在：Fork 與 Task Worktree 要先建立。
func (p *planner) workdir(sid SessionID) (path string, exists bool) {
	x := p.s.Sessions[sid]
	if x.Fork != nil {
		return x.Fork.Path, p.l.Worktrees[x.Fork.Path]
	}
	if p.s.Worktree != nil {
		return p.s.Worktree.Path, p.l.Worktrees[p.s.Worktree.Path]
	}
	return p.l.WorkspaceDir, true
}

// taskWorkdir 是 Task Worktree 的路徑；沒有隔離時是 Workspace 目錄。
func (p *planner) taskWorkdir() string {
	if p.s.Worktree != nil {
		return p.s.Worktree.Path
	}
	return p.l.WorkspaceDir
}

func (p *planner) taskLevel() {
	if w := p.s.Worktree; w != nil && w.Outcome == OutcomeOpen && !p.l.Worktrees[w.Path] {
		p.emit(Effect{Kind: EffCreateWorktree,
			CreateWorktree: &CreateWorktree{Repo: p.l.WorkspaceDir, Worktree: w.Worktree}}, "task")
	}
	if r := p.s.Review; r != nil && r.Launch != nil && r.Reviewer == "" {
		pr := &PrepareReview{}
		for _, sid := range r.Candidates {
			if f := p.s.Sessions[sid].Fork; f != nil {
				pr.Candidates = append(pr.Candidates,
					ReviewCandidate{Session: sid, Worktree: f.Worktree})
			}
		}
		p.emit(Effect{Kind: EffPrepareReview, PrepareReview: pr}, "")
	}
}

func (p *planner) session(sid SessionID) {
	x := p.s.Sessions[sid]
	conn, hasConn := p.l.Conns[sid]
	if f := x.Fork; f != nil && f.Outcome == OutcomeOpen && !p.l.Worktrees[f.Path] {
		// Task Worktree 要先建好，Fork 才能從它的分支切出來。
		if p.s.Worktree == nil || p.l.Worktrees[p.s.Worktree.Path] {
			p.emit(Effect{Kind: EffCreateWorktree, Session: sid,
				CreateWorktree: &CreateWorktree{Repo: p.taskWorkdir(), Worktree: f.Worktree}}, string(sid))
		}
	}
	if x.Status == SessionClosed || x.Status == SessionExited {
		if hasConn {
			p.emit(Effect{Kind: EffCloseConn, Session: sid}, string(sid))
		}
		return
	}
	// 日誌記著綁定、hub 卻沒有它的連線（D117）：行程不在了。閒著而原生會話接不回來的記為退出，
	// 不等使用者送了才失敗；接得回來的不必記什麼，下一輪照“還沒啟動”以 Resume 啟動。
	lost, isLost := p.l.Lost[sid]
	if x.Turn == nil && x.Bound && !hasConn && isLost && !x.resumable() {
		p.emit(Effect{Kind: EffReportExited, Session: sid}, string(sid))
	}
	// 換模型（D118）：連線在、這一輪還沒交給 agent 時去設，send_prompt 等它設好（接回後回到預設的也設回來，V86）。
	// 閒著而行程不在（重啟後，接得回的）時以 Resume 接回去設（V85）。
	model := x.wantsModel() && (x.Turn == nil || !x.Turn.Cancelling && conn.Turn != x.Turn.ID)
	if model && hasConn {
		p.emit(Effect{Kind: EffSetModel, Session: sid,
			SetModel: &ModelSwitch{Option: x.Model.Option, Value: x.ModelWanted}}, string(sid))
	} else if model && x.Turn == nil && isLost && x.resumable() {
		p.startConn(sid, x)
	}
	if t := x.Turn; t != nil {
		// 綁定的行程不在之後才開始的一輪：沒有交給任何行程，可以照常啟動。
		startable := !x.Bound || isLost && lost != t.ID && x.resumable()
		if !hasConn {
			if !startable {
				// 這一輪交給了已不在的行程：沒有人在做，回報退出（D2）。
				p.emit(Effect{Kind: EffReportExited, Session: sid}, string(t.ID))
			} else {
				p.startConn(sid, x)
			}
		} else if t.Cancelling {
			// Prompt 正在送出時先等它送完：送到了要經 agent 取消，hub 只在
			// 確定沒送出時才在本地結束這一輪。取消對每個連線的每一輪只送一次。
			if conn.Cancelled != t.ID && !p.l.InFlight[newEffectKey(EffSendPrompt, string(t.ID))] {
				p.emit(Effect{Kind: EffCancelTurn, Session: sid}, string(t.ID))
			}
		} else if conn.Turn != t.ID && !model {
			pr := t.Prompt
			p.emit(Effect{Kind: EffSendPrompt, Session: sid, SendPrompt: &pr}, string(t.ID))
		}
	}
	for _, pid := range slices.Sorted(maps.Keys(x.Pending)) {
		a := x.Pending[pid]
		object := string(sid) + "/" + string(pid)
		switch {
		case a.Decision != nil && !a.Answered && hasConn:
			p.emit(Effect{Kind: EffAnswerPermission, Session: sid,
				AnswerPermission: &AnswerPermission{Permission: pid, Verdict: a.Decision.Verdict}}, object)
		case a.Decision == nil && x.Launch.AskTimeout > 0 && !p.l.Timers[AskKey{Session: sid, Permission: pid}]:
			p.emit(Effect{Kind: EffScheduleTimer, Session: sid,
				ScheduleTimer: &ScheduleTimer{Permission: pid, At: a.AskedAt.Add(x.Launch.AskTimeout)}}, object)
		}
	}
}

// startConn 啟動 Session 的 agent：工作目錄要已存在；舊連線還在關（歸檔時關掉的，D117）時等它的行程結束，
// 才不會有兩個行程接著同一個原生會話。接得回原生會話的以 Resume 接回。
func (p *planner) startConn(sid SessionID, x Session) {
	dir, ok := p.workdir(sid)
	if !ok || p.l.InFlight[newEffectKey(EffCloseConn, string(sid))] {
		return
	}
	st := &StartConn{Launch: x.Launch, Workdir: dir}
	if x.NativeID != "" && x.resumable() {
		st.Resume = x.NativeID
	}
	p.emit(Effect{Kind: EffStartConn, Session: sid, StartConn: st}, string(sid))
}

func (p *planner) intents() {
	for _, id := range slices.Sorted(maps.Keys(p.s.Intents)) {
		in := p.s.Intents[id]
		switch in.Kind {
		case IntentMerge, IntentDiscard:
			p.resolveIntent(in)
		case IntentDeleteNative:
			p.deleteIntent(in)
		}
	}
}

func (p *planner) resolveIntent(in Intent) {
	t := in.Target
	var sid SessionID
	var w *WorktreeState
	switch t.Level {
	case TargetFork:
		x, ok := p.s.Sessions[t.Session]
		if !ok || x.Fork == nil || x.Status == SessionRunning {
			return
		}
		sid, w = t.Session, x.Fork
	case TargetWorktree:
		for _, x := range p.s.Sessions {
			if x.openFork() {
				return // 有開著的 Fork 時不處置 Task Worktree
			}
		}
		if p.s.Worktree == nil {
			return
		}
		w = p.s.Worktree
	default:
		return
	}
	if w.Outcome != OutcomeOpen {
		return
	}
	if in.Kind == IntentMerge {
		into := p.taskWorkdir()
		if t.Level == TargetWorktree {
			into = p.l.WorkspaceDir
		}
		p.emit(Effect{Kind: EffMergeWorktree, Session: sid, MergeWorktree: &MergeWorktree{
			Intent: in.ID, From: w.Worktree, Into: into,
			Strategy: in.Strategy, Message: in.Message,
		}}, string(in.ID))
	} else {
		p.emit(Effect{Kind: EffDiscardWorktree, Session: sid, DiscardWorktree: &DiscardWorktree{
			Intent: in.ID, Worktree: w.Worktree,
		}}, string(in.ID))
	}
}

func (p *planner) deleteIntent(in Intent) {
	sid := in.Target.Session
	if _, ok := p.l.Conns[sid]; ok {
		return // 連線還在：先關閉再刪原生檔案
	}
	x, ok := p.s.Sessions[sid]
	if !ok {
		return
	}
	dir, _ := p.workdir(sid)
	p.emit(Effect{Kind: EffDeleteNative, Session: sid, DeleteNative: &DeleteNative{
		Intent: in.ID, Protocol: x.Launch.Protocol, NativeID: x.NativeID, Workdir: dir,
	}}, string(in.ID))
}
