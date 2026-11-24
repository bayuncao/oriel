package task

import (
	"cmp"
	"slices"
	"strings"
)

// unresolved 回報是否仍有未解決的 worktree：Outcome 為 open 且沒有
// 針對它的意圖（等待中的合併或捨棄意圖算已解決）。
func (d *decision) unresolved() bool {
	if d.s.Worktree != nil && d.s.Worktree.Outcome == OutcomeOpen &&
		!d.hasIntent(Target{Level: TargetWorktree}) {
		return true
	}
	return len(d.unresolvedForks()) > 0
}

// forkTarget 是 Session 的 Fork 目標。
func forkTarget(sid SessionID) Target {
	return Target{Level: TargetFork, Session: sid}
}

// unresolvedForks 列出還開著且沒有意圖的 Fork（依 Order）。
func (d *decision) unresolvedForks() []SessionID {
	var out []SessionID
	for _, sid := range d.s.Order {
		if d.s.Sessions[sid].openFork() && !d.hasIntent(forkTarget(sid)) {
			out = append(out, sid)
		}
	}
	return out
}

// intentFor 取出針對目標的既有意圖。
func (d *decision) intentFor(t Target) (Intent, bool) {
	for _, in := range d.s.Intents {
		if in.Target == t {
			return in, true
		}
	}
	return Intent{}, false
}

func (d *decision) hasIntent(t Target) bool {
	_, ok := d.intentFor(t)
	return ok
}

// target 找出處置的目標 worktree；找不到是程式或協定錯誤。
func (d *decision) target(t Target) (SessionID, *WorktreeState, error) {
	switch t.Level {
	case TargetFork:
		x, ok := d.s.Sessions[t.Session]
		if !ok || x.Fork == nil {
			return "", nil, invalid("no worktree for target")
		}
		return t.Session, x.Fork, nil
	case TargetWorktree:
		if d.s.Worktree == nil {
			return "", nil, invalid("no worktree for target")
		}
		return "", d.s.Worktree, nil
	}
	return "", nil, invalid("unknown target %q", t.Level)
}

// requestResolution 為目標 worktree 登記合併或捨棄意圖（D9、D16）；已有意圖時不重複。
func (d *decision) requestResolution(t Target, r Resolution) error {
	sid, w, err := d.target(t)
	if err != nil {
		return err
	}
	if w.Outcome != OutcomeOpen {
		return invalid("worktree already resolved")
	}
	if d.hasIntent(t) {
		return nil
	}
	switch r.Action {
	case IntentMerge:
		strategy := cmp.Or(r.Strategy, MergeSquash)
		if strategy != MergeSquash && strategy != MergePullRequest {
			return invalid("unknown merge strategy %q", r.Strategy)
		}
		d.emit(Event{Kind: MergeRequestedKind, Session: sid, MergeRequested: &Intent{
			ID: intentID(d.next()), Kind: IntentMerge, Target: t,
			Strategy: strategy, Message: cmp.Or(r.Message, d.defaultMergeMessage()),
		}})
	case IntentDiscard:
		d.emit(Event{Kind: DiscardRequestedKind, Session: sid, DiscardRequested: &Intent{
			ID: intentID(d.next()), Kind: IntentDiscard, Target: t,
		}})
	default:
		return invalid("unknown resolution action %q", r.Action)
	}
	return nil
}

func (d *decision) defaultMergeMessage() string {
	m := d.s.Title
	if d.s.Brief != "" {
		m += "\n\n" + d.s.Brief
	}
	return m
}

// checkResolve 是 Merge / Discard 命令的前提（D35）：目標開著且還沒有意圖；
// Fork 的 Session 不在運行；Task Worktree 要求 Task 內沒有 Session 在運行、沒有未處置的 Fork。
func (d *decision) checkResolve(t Target, kind IntentKind) (noop bool, err error) {
	sid, w, err := d.target(t)
	if err != nil {
		return false, err
	}
	if w.Outcome != OutcomeOpen {
		return false, invalid("worktree already resolved")
	}
	if in, ok := d.intentFor(t); ok {
		if in.Kind == kind {
			return true, nil // 同一意圖已在進行：重複命令不報錯
		}
		return false, invalid("target has a %s intent; cancel it first", in.Kind)
	}
	if sid != "" {
		if d.s.Sessions[sid].Status == SessionRunning {
			return false, reject(ReasonSessionRunning)
		}
	} else {
		if d.anyRunning() {
			return false, reject(ReasonSessionRunning)
		}
		if len(d.unresolvedForks()) > 0 {
			return false, reject(ReasonUnresolvedChanges)
		}
	}
	return false, nil
}

// resolveCmd 是 Merge 與 Discard 共用的流程（D35）：remote 的命令要 desktop
// 核准（D5、D20），desktop 直接登記意圖。
func (d *decision) resolveCmd(c Command, t Target, r Resolution) error {
	noop, err := d.checkResolve(t, r.Action)
	if err != nil || noop {
		return err
	}
	if c.Origin == OriginRemote {
		d.requestConfirmation(c, func(stored Command) bool {
			return stored.Kind == CmdMerge && stored.Merge != nil && stored.Merge.Target == t ||
				stored.Kind == CmdDiscard && stored.Discard != nil && stored.Discard.Target == t
		})
		return nil
	}
	return d.requestResolution(t, r)
}

func merge(d *decision, c Command) error {
	p := c.Merge
	return d.resolveCmd(c, p.Target, Resolution{
		Action: IntentMerge, Strategy: p.Strategy, Message: p.Message,
	})
}

func discard(d *decision, c Command) error {
	p := c.Discard
	return d.resolveCmd(c, p.Target, Resolution{Action: IntentDiscard})
}

func cancelIntent(d *decision, c Command) error {
	p := c.CancelIntent
	in, ok := d.s.Intents[p.Intent]
	if !ok {
		return nil // 意圖已完成或已撤銷
	}
	if _, suspended := d.s.Failed[in.effectKey()]; !suspended {
		return reject(ReasonInFlight)
	}
	d.emit(Event{Kind: IntentCancelledKind, IntentCancelled: &IntentRef{Intent: p.Intent}})
	return nil
}

// intentFact 取出事實所指的意圖；不存在視為已處理過（done），Kind 不符是程式錯誤。
func (d *decision) intentFact(id IntentID, kind IntentKind) (in Intent, done bool, err error) {
	in, ok := d.s.Intents[id]
	if !ok {
		return in, true, nil
	}
	if in.Kind != kind {
		return in, false, invalid("intent %s is a %s", in.ID, in.Kind)
	}
	return in, false, nil
}

// resolvedFact 是 merged / discarded 共用的流程：找意圖、拒絕掛起中與仍在運行的
// （hub 不得在那時執行 Effect）、發出結局事件、收尾（D37）。
func (d *decision) resolvedFact(id IntentID, kind IntentKind, emit func(in Intent)) error {
	in, done, err := d.intentFact(id, kind)
	if err != nil || done {
		return err
	}
	if _, ok := d.s.Failed[in.effectKey()]; ok {
		return invalid("effect is suspended")
	}
	switch in.Target.Level {
	case TargetFork:
		if d.s.Sessions[in.Target.Session].Status == SessionRunning {
			return invalid("session still running")
		}
	case TargetWorktree:
		for _, x := range d.s.Sessions {
			if x.Status == SessionRunning {
				return invalid("session still running")
			}
		}
	default:
		return invalid("unknown target %q", in.Target.Level)
	}
	emit(in)
	d.afterResolved(in)
	return nil
}

func merged(d *decision, c Command) error {
	p := c.Merged
	r := *p
	return d.resolvedFact(p.Intent, IntentMerge, func(in Intent) {
		if in.Target.Level == TargetFork {
			d.emit(Event{Kind: ForkMergedKind, Session: in.Target.Session, Merged: &r})
		} else {
			d.emit(Event{Kind: WorktreeMergedKind, Merged: &r})
		}
	})
}

func discarded(d *decision, c Command) error {
	p := c.Discarded
	ref := &IntentRef{Intent: p.Intent}
	return d.resolvedFact(p.Intent, IntentDiscard, func(in Intent) {
		if in.Target.Level == TargetFork {
			d.emit(Event{Kind: ForkDiscardedKind, Session: in.Target.Session, Discarded: ref})
		} else {
			d.emit(Event{Kind: WorktreeDiscardedKind, Discarded: ref})
		}
	})
}

// mergeConflict 是合併衝突：記錄衝突檔案並把 merge_worktree 掛起，等使用者處理。
func mergeConflict(d *decision, c Command) error {
	p := c.MergeConflict
	in, done, err := d.intentFact(p.Intent, IntentMerge)
	if err != nil || done {
		return err
	}
	if len(p.Files) == 0 {
		return invalid("merge conflict without files")
	}
	if _, ok := d.s.Failed[in.effectKey()]; ok {
		return nil // 已掛起
	}
	d.emit(Event{Kind: MergeConflictedKind, Session: in.Target.Session,
		MergeConflicted: &MergeConflict{Intent: p.Intent, Files: slices.Clone(p.Files)}})
	d.emit(Event{Kind: EffectFailedKind, EffectFailed: &EffectFailure{
		Effect:  in.effectKey(),
		Problem: Problem{Code: CodeMergeConflict, Params: map[string]string{"files": strings.Join(p.Files, "\n")}},
	}})
	return nil
}

// afterResolved 在 worktree 處置完成後收尾（D37）：Fork 關掉它的 Session；
// Task Worktree 關掉所有還開著的 Session 並把 Task 標為 done。
func (d *decision) afterResolved(in Intent) {
	if in.Target.Level == TargetFork {
		d.closeIfOpen(in.Target.Session)
		return
	}
	for _, sid := range d.s.Order {
		d.closeIfOpen(sid)
	}
	d.setStatus(TaskDone, CauseAuto)
}
