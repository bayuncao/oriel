package task

import "slices"

// autoReview 在 Fork 的 Session 都停下後開啟評審（D17、D38）：本次有 Session 停下
// （回合結束、進程退出、Session 關閉），Task 未歸檔、沒有 Review，開著且無意圖的
// Fork ≥ 2 且其 Session 都不在運行。
func (d *decision) autoReview() {
	var stopped bool
	for _, e := range d.out {
		switch e.Kind {
		case TurnCompletedKind, TurnCancelledKind, TurnFailedKind,
			ProcessExitedKind, SessionClosedKind:
			stopped = true
		}
	}
	if !stopped || d.s.Review != nil || d.s.Archived {
		return // 歸檔的 Task 不開評審：沒人會看到它
	}
	candidates := d.unresolvedForks()
	for _, sid := range candidates {
		if d.s.Sessions[sid].Status == SessionRunning {
			return
		}
	}
	if len(candidates) < 2 {
		return
	}
	var launch *LaunchSnapshot
	if d.in.Reviewer != nil {
		l := *d.in.Reviewer
		launch = &l
	}
	d.emit(Event{Kind: ReviewRequestedKind, ReviewRequested: &ReviewRequested{
		Candidates: candidates, Launch: launch,
	}})
}

func fanOut(d *decision, c Command) error {
	p := c.FanOut
	if len(p.Launches) < 2 {
		return invalid("fan-out needs at least two launches")
	}
	if err := CheckPrompt(p.Prompt); err != nil {
		return err
	}
	for _, l := range p.Launches {
		if err := checkLaunch(l); err != nil {
			return err
		}
	}
	if !d.in.Repo.IsGit {
		return reject(ReasonNotGit)
	}
	if d.in.Repo.Branch == "" {
		return reject(ReasonDetachedHead)
	}
	if d.in.Repo.Dirty {
		return reject(ReasonDirtyWorkdir)
	}
	for _, x := range d.s.Sessions {
		if x.openFork() {
			return reject(ReasonUnresolvedChanges)
		}
	}
	for _, l := range p.Launches {
		id := d.newSession(l, RoleWorker, true)
		d.submit(id, p.Prompt) // Prompt 是值；submit 為每份複本編新 ID
	}
	return nil
}

// reviewPrepared 是 prepare_review 的結果：檢查候選名單一致後建立審查者 Session。
func reviewPrepared(d *decision, c Command) error {
	p := c.ReviewPrepared
	r := d.s.Review
	if r == nil || r.Launch == nil || r.Reviewer != "" {
		return nil // 過期或重複的回報
	}
	if len(p.Diffs) != len(r.Candidates) {
		return invalid("diff count %d, want %d", len(p.Diffs), len(r.Candidates))
	}
	bySession := make(map[SessionID]ForkDiff, len(p.Diffs))
	for _, fd := range p.Diffs {
		if _, dup := bySession[fd.Session]; dup {
			return invalid("duplicate diff for %s", fd.Session)
		}
		bySession[fd.Session] = fd
	}
	ordered := make([]ForkDiff, len(r.Candidates))
	for i, sid := range r.Candidates {
		fd, ok := bySession[sid]
		if !ok {
			return invalid("no diff for candidate %s", sid)
		}
		ordered[i] = fd
	}
	id := d.newSession(*r.Launch, RoleReviewer, false)
	d.submit(id, reviewPrompt(d.s, r.Candidates, ordered))
	return nil
}

func recommendation(d *decision, c Command) error {
	p := c.Recommendation
	r := d.s.Review
	if r == nil {
		return nil
	}
	if r.Reviewer == "" {
		return invalid("no reviewer")
	}
	if r.Recommended != "" {
		return nil
	}
	if !slices.Contains(r.Candidates, p.Winner) {
		return invalid("winner %s is not a candidate", p.Winner)
	}
	rc := *p
	d.emit(Event{Kind: ForkRecommendedKind, ForkRecommended: &rc})
	return nil
}

func chooseFork(d *decision, c Command) error {
	p := c.ChooseFork
	winnerTarget := forkTarget(p.Winner)
	x, ok := d.s.Sessions[p.Winner]
	if !ok || x.Fork == nil {
		return invalid("winner %s has no fork", p.Winner)
	}
	var losers []Target
	for _, sid := range d.unresolvedForks() {
		if sid != p.Winner {
			losers = append(losers, forkTarget(sid))
		}
	}
	noop, err := d.checkResolve(winnerTarget, IntentMerge)
	if err != nil {
		return err
	}
	if noop {
		return invalid("winner already has an intent")
	}
	for _, t := range losers {
		if _, err := d.checkResolve(t, IntentDiscard); err != nil {
			return err
		}
	}
	if c.Origin == OriginRemote {
		d.requestConfirmation(c, func(stored Command) bool {
			return stored.Kind == CmdChooseFork
		})
		return nil
	}
	if err := d.requestResolution(winnerTarget,
		Resolution{Action: IntentMerge, Strategy: p.Strategy, Message: p.Message}); err != nil {
		return err
	}
	for _, t := range losers {
		if err := d.requestResolution(t, Resolution{Action: IntentDiscard}); err != nil {
			return err
		}
	}
	return nil
}

// endReview 在候選剩不到兩個時結束評審（D43）：關掉評審者（若未關）再記
// ReviewEnded；意圖登記即算處置，與 done 的判定一致。
func (d *decision) endReview() {
	r := d.s.Review
	if r == nil {
		return
	}
	left := 0
	for _, sid := range r.Candidates {
		if d.s.Sessions[sid].openFork() && !d.hasIntent(forkTarget(sid)) {
			left++
		}
	}
	if left >= 2 {
		return
	}
	if r.Reviewer != "" {
		d.closeIfOpen(r.Reviewer)
	}
	d.emit(Event{Kind: ReviewEndedKind})
}

// skipReview 改為手動選擇：關掉審查者（若有）、把 Launch 置空。
func skipReview(d *decision, c Command) error {
	r := d.s.Review
	if r == nil || r.Launch == nil {
		return nil
	}
	if r.Reviewer != "" {
		d.closeIfOpen(r.Reviewer)
	}
	d.emit(Event{Kind: ReviewSkippedKind})
	return nil
}
