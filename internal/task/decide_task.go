package task

import (
	"path/filepath"
	"slices"
	"strings"
)

// autoStatus 套用 Task 狀態的自動規則（D7）：本次有回合開始 → in_progress；
// 本次有回合結束、所有 Session 都不在運行且至少完成過一輪 → in_review。
func (d *decision) autoStatus() {
	var started, stopped bool
	for _, e := range d.out {
		switch e.Kind {
		case TurnStartedKind:
			started = true
		case TurnCompletedKind, TurnCancelledKind, TurnFailedKind:
			stopped = true
		}
	}
	switch {
	case started:
		d.setStatus(TaskInProgress, CauseAuto)
	case stopped:
		var completed bool
		for _, x := range d.s.Sessions {
			completed = completed || x.CompletedTurns > 0
		}
		if !d.anyRunning() && completed {
			d.setStatus(TaskInReview, CauseAuto)
		}
	}
}

func (d *decision) setStatus(to TaskStatus, cause StatusCause) {
	if d.s.Status == to {
		return
	}
	d.emit(Event{Kind: TaskStatusChangedKind, TaskStatusChanged: &TaskStatusChanged{
		From: d.s.Status, To: to, Cause: cause,
	}})
}

func createTask(d *decision, c Command) error {
	p := c.CreateTask
	title, ok := Label(p.Title)
	brief := p.Brief
	if strings.TrimSpace(brief) == "" {
		brief = ""
	}
	switch {
	case d.s.ID == "":
		return invalid("task id missing")
	case p.Workspace == "":
		return invalid("workspace missing")
	case !ok:
		return invalid("title must be one non-blank line")
	case len(p.Launches) > 0 && brief == "":
		return invalid("brief is the first prompt")
	}
	for _, l := range p.Launches {
		if err := checkLaunch(l); err != nil {
			return err
		}
	}
	fanout := len(p.Launches) > 1
	mode, err := chooseIsolation(d.in, c.Origin == OriginRemote, fanout)
	if err != nil {
		return err
	}
	if fanout && mode == IsolationNone && d.in.Repo.Dirty {
		return reject(ReasonDirtyWorkdir)
	}
	d.emit(Event{Kind: TaskCreatedKind, TaskCreated: &TaskCreated{
		Title: title, Brief: brief, Workspace: p.Workspace, Isolation: mode,
	}})
	if mode == IsolationWorktree {
		w := d.taskWorktree()
		d.emit(Event{Kind: WorktreeAttachedKind, WorktreeAttached: &w})
	}
	for _, l := range p.Launches {
		id := d.newSession(l, RoleWorker, fanout)
		d.submit(id, Prompt{Text: brief})
	}
	return nil
}

// IsolationFor 是在 in 下從桌面新建、不扇出的 Task 會用的隔離方式；介面據此判斷
// 新 Task 會不會與在跑的 Task 共用目錄（D97）。
func IsolationFor(in Inputs) IsolationMode {
	mode, _ := chooseIsolation(in, false, false) // 不扇出、不是遠端時不會拒絕
	return mode
}

// chooseIsolation 依 domain.md「Isolation 與 Fork」的表選擇：非 git 與 detached HEAD 兩列優先於 Policy。
func chooseIsolation(in Inputs, remote, fanout bool) (IsolationMode, error) {
	switch {
	case !in.Repo.IsGit:
		if fanout {
			return "", reject(ReasonNotGit)
		}
		return IsolationNone, nil
	case in.Repo.Branch == "":
		if fanout || remote {
			return "", reject(ReasonDetachedHead)
		}
		return IsolationNone, nil
	case in.Policy == PolicyAlways:
		return IsolationWorktree, nil
	case in.Policy == PolicyNever:
		return IsolationNone, nil
	case fanout || remote || in.OtherTasksRunning:
		return IsolationWorktree, nil
	}
	return IsolationNone, nil
}

func (d *decision) taskWorktree() Worktree {
	id := string(d.s.ID)
	return Worktree{
		Path:       filepath.Join(d.in.WorktreeRoot, id),
		Branch:     "orielhub/" + id,
		BaseBranch: d.in.Repo.Branch,
		BaseCommit: d.in.Repo.Commit,
	}
}

func (d *decision) forkWorktree(sid SessionID) Worktree {
	name := string(d.s.ID) + "-" + string(sid)
	base := d.in.Repo.Branch
	if d.s.Worktree != nil {
		base = d.s.Worktree.Branch
	}
	return Worktree{
		Path:       filepath.Join(d.in.WorktreeRoot, name),
		Branch:     "orielhub/" + name,
		BaseBranch: base,
		BaseCommit: d.in.Repo.Commit,
	}
}

func editTask(d *decision, c Command) error {
	p := c.EditTask
	title, ok := Label(p.Title)
	brief := p.Brief
	if strings.TrimSpace(brief) == "" {
		brief = ""
	}
	if !ok {
		return invalid("title must be one non-blank line")
	}
	if title == d.s.Title && brief == d.s.Brief {
		return nil
	}
	e := *p
	e.Title, e.Brief = title, brief
	d.emit(Event{Kind: TaskEditedKind, TaskEdited: &e})
	return nil
}

func setStatus(d *decision, c Command) error {
	p := c.SetStatus
	switch p.Status {
	case TaskTodo, TaskInProgress, TaskInReview, TaskDone:
	default:
		return invalid("unknown status %q", p.Status)
	}
	if p.Status != TaskDone && len(p.Resolutions) > 0 {
		return invalid("resolutions only apply to done")
	}
	if p.Status == TaskDone {
		if len(p.Resolutions) > 0 && c.Origin != OriginDesktop {
			return reject(ReasonNeedsDesktop)
		}
		for _, r := range p.Resolutions {
			if err := d.requestResolution(r.Target, r.Resolution); err != nil {
				return err
			}
		}
		if d.unresolved() {
			return reject(ReasonUnresolvedChanges)
		}
	}
	d.setStatus(p.Status, CauseUser)
	return nil
}

// anyRunning 回報是否有 Session 在運行。
func (d *decision) anyRunning() bool {
	for _, x := range d.s.Sessions {
		if x.Status == SessionRunning {
			return true
		}
	}
	return false
}

func archive(d *decision, _ Command) error {
	if d.s.Archived {
		return nil
	}
	if d.anyRunning() {
		return reject(ReasonSessionRunning)
	}
	d.emit(Event{Kind: TaskArchivedKind})
	return nil
}

func unarchive(d *decision, _ Command) error {
	if !d.s.Archived {
		return nil
	}
	d.emit(Event{Kind: TaskUnarchivedKind})
	return nil
}

// pin 置頂 Task（D109）；已置頂時不產生事件，置頂的時間不變。
func pin(d *decision, _ Command) error {
	if !d.s.PinnedAt.IsZero() {
		return nil
	}
	d.emit(Event{Kind: TaskPinnedKind})
	return nil
}

// unpin 取消置頂；未置頂時不產生事件。
func unpin(d *decision, _ Command) error {
	if d.s.PinnedAt.IsZero() {
		return nil
	}
	d.emit(Event{Kind: TaskUnpinnedKind})
	return nil
}

// setPriority 設定優先級（D135）；與現值相同時不產生事件。
func setPriority(d *decision, c Command) error {
	p := c.SetPriority.Priority
	switch p {
	case PriorityNone, PriorityLow, PriorityMedium, PriorityHigh, PriorityUrgent:
	default:
		return invalid("unknown priority %q", p)
	}
	if p == d.s.Priority {
		return nil
	}
	d.emit(Event{Kind: TaskPrioritySetKind, TaskPrioritySet: &SetPriority{Priority: p}})
	return nil
}

// setLabels 以完整的集合取代標籤（D135）：排序、去重，每個都要在目錄裡（Inputs.Labels）；與現值相同時不產生事件。
func setLabels(d *decision, c Command) error {
	labels := slices.Clone(c.SetLabels.Labels)
	slices.Sort(labels)
	labels = slices.Compact(labels)
	for _, id := range labels {
		if !slices.Contains(d.in.Labels, id) {
			return &Rejection{Reason: ReasonUnknownLabel, Detail: string(id)}
		}
	}
	if slices.Equal(labels, d.s.Labels) {
		return nil
	}
	if len(labels) == 0 {
		labels = nil
	}
	d.emit(Event{Kind: TaskLabelsSetKind, TaskLabelsSet: &SetLabels{Labels: labels}})
	return nil
}

// Label 正規化單行標籤（Task 標題、Workspace 名稱，D65）：去掉首尾空白；
// 結果為空或含換行（\n、\r）時 ok 為 false。中間的 tab 等照常保留。
func Label(s string) (string, bool) {
	out := strings.TrimSpace(s)
	return out, out != "" && !strings.ContainsAny(out, "\r\n")
}
