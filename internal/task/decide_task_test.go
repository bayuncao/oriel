package task

import (
	"slices"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	worktree := Target{Level: TargetWorktree}
	forkS2 := Target{Level: TargetFork, Session: "s2"}
	resolve := func(t Target, r Resolution) TargetResolution {
		return TargetResolution{Target: t, Resolution: r}
	}
	runDecide(t, []decideCase{
		// Decide 骨架。
		{name: "unknown kind", in: gitInputs(),
			cmd:    Command{Kind: "nope", Origin: OriginDesktop},
			reason: ReasonInvalid, detail: "unknown command kind"},
		{name: "command claims runtime", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    Command{Kind: CmdEditTask, Origin: OriginRuntime, EditTask: &EditTask{Title: "x"}},
			reason: ReasonForgedOrigin},
		{name: "fact without runtime", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    Command{Kind: FactOutput, Origin: OriginDesktop, Output: &Output{Text: "x"}},
			reason: ReasonForgedOrigin},
		{name: "two payloads set", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: Command{Kind: CmdSubmit, Origin: OriginDesktop, Session: "s2",
				Submit: &Submit{Prompt: Prompt{Text: "x"}}, Merge: &Merge{}},
			reason: ReasonInvalid, detail: "payload Merge set"},
		{name: "empty origin", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    Command{Kind: CmdEditTask, EditTask: &EditTask{Title: "x"}},
			reason: ReasonInvalid, detail: "origin"},
		{name: "edit before create", in: gitInputs(),
			cmd:    Command{Kind: CmdEditTask, Origin: OriginDesktop, EditTask: &EditTask{Title: "x"}},
			reason: ReasonInvalid, detail: "does not exist"},
		{name: "create on existing", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    create(nil),
			reason: ReasonInvalid, detail: "already exists"},
		{name: "edit archived", given: seqs(created(IsolationNone), Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd:    Command{Kind: CmdEditTask, Origin: OriginDesktop, EditTask: &EditTask{Title: "x"}},
			reason: ReasonArchived},
		{name: "unarchive", given: seqs(created(IsolationNone), Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd:  Command{Kind: CmdUnarchive, Origin: OriginDesktop},
			want: []Event{{Kind: TaskUnarchivedKind}}},
		{name: "archive archived is a no-op",
			given: seqs(created(IsolationNone), Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd: Command{Kind: CmdArchive, Origin: OriginDesktop}},
		// 所有 CommandKind 皆有處理函式後，unsupported 分支只剩防禦用途；
		// 若新增 kind 而未登記 handler，可在這裡加回對應的測試。

		// CreateTask。
		{name: "create missing payload", in: gitInputs(),
			cmd:    Command{Kind: CmdCreateTask, Origin: OriginDesktop},
			reason: ReasonInvalid},
		{name: "create empty id", noID: true, in: gitInputs(),
			cmd:    create(nil),
			reason: ReasonInvalid, detail: "id"},
		{name: "create empty workspace", in: gitInputs(),
			cmd:    create(func(p *CreateTask) { p.Workspace = "" }),
			reason: ReasonInvalid, detail: "workspace"},
		{name: "create empty title", in: gitInputs(),
			cmd:    create(func(p *CreateTask) { p.Title = "" }),
			reason: ReasonInvalid, detail: "title"},
		{name: "create launches without brief", in: gitInputs(),
			cmd:    create(func(p *CreateTask) { p.Brief, p.Launches = "", []LaunchSnapshot{newLaunch()} }),
			reason: ReasonInvalid, detail: "brief"},
		{name: "create non-git", in: inputs(func(i *Inputs) { i.Repo = Repo{} }),
			cmd:    create(nil),
			want:   []Event{created(IsolationNone)},
			status: TaskTodo},
		{name: "create one launch", in: gitInputs(),
			cmd: create(func(p *CreateTask) { p.Launches = []LaunchSnapshot{newLaunch()} }),
			want: []Event{
				created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			status: TaskInProgress},
		// D97：M4 之前同倉庫有 Task 在跑也共用主目錄。
		{name: "create other tasks running", in: inputs(func(i *Inputs) { i.OtherTasksRunning = true }),
			cmd:  create(nil),
			want: []Event{created(IsolationNone)}},
		{name: "create policy always", in: inputs(func(i *Inputs) { i.Policy = PolicyAlways }),
			cmd:  create(nil),
			want: []Event{created(IsolationWorktree), attached()}},
		{name: "create policy never with others running",
			in:   inputs(func(i *Inputs) { i.Policy, i.OtherTasksRunning = PolicyNever, true }),
			cmd:  create(nil),
			want: []Event{created(IsolationNone)}},
		{name: "create remote", in: gitInputs(),
			cmd: func() Command {
				c := create(nil)
				c.Origin = OriginRemote
				return c
			}(),
			want: []Event{created(IsolationWorktree), attached()}},
		{name: "create remote policy never", in: inputs(func(i *Inputs) { i.Policy = PolicyNever }),
			cmd: func() Command {
				c := create(nil)
				c.Origin = OriginRemote
				return c
			}(),
			want: []Event{created(IsolationNone)}},
		{name: "create detached", in: inputs(func(i *Inputs) { i.Repo.Branch = "" }),
			cmd:  create(nil),
			want: []Event{created(IsolationNone)}},
		{name: "create detached remote",
			in: inputs(func(i *Inputs) { i.Repo.Branch = "" }),
			cmd: func() Command {
				c := create(nil)
				c.Origin = OriginRemote
				return c
			}(),
			reason: ReasonDetachedHead},
		{name: "create detached fanout", in: inputs(func(i *Inputs) { i.Repo.Branch = "" }),
			cmd:    create(func(p *CreateTask) { p.Launches = []LaunchSnapshot{newLaunch(), newLaunch()} }),
			reason: ReasonDetachedHead},
		{name: "create detached policy always",
			in:   inputs(func(i *Inputs) { i.Repo.Branch, i.Policy = "", PolicyAlways }),
			cmd:  create(nil),
			want: []Event{created(IsolationNone)}},
		{name: "create non-git fanout", in: inputs(func(i *Inputs) { i.Repo = Repo{} }),
			cmd:    create(func(p *CreateTask) { p.Launches = []LaunchSnapshot{newLaunch(), newLaunch()} }),
			reason: ReasonNotGit},
		{name: "create fanout", in: gitInputs(),
			cmd: create(func(p *CreateTask) { p.Launches = []LaunchSnapshot{newLaunch(), newLaunch()} }),
			want: []Event{
				created(IsolationWorktree), attached(),
				sessionEv("s3"), forkEv("s3", "orielhub/t-1"), queuedEv("s3", "p5", "Build it"), turnEv("s3", "t6", "p5"),
				sessionEv("s7"), forkEv("s7", "orielhub/t-1"), queuedEv("s7", "p9", "Build it"), turnEv("s7", "t10", "p9"),
				statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			status: TaskInProgress},
		{name: "create fanout dirty",
			in: inputs(func(i *Inputs) { i.Policy, i.Repo.Dirty = PolicyNever, true }),
			cmd: create(func(p *CreateTask) {
				p.Launches = []LaunchSnapshot{newLaunch(), newLaunch()}
			}),
			reason: ReasonDirtyWorkdir},
		{name: "create fanout no isolation", in: inputs(func(i *Inputs) { i.Policy = PolicyNever }),
			cmd: create(func(p *CreateTask) { p.Launches = []LaunchSnapshot{newLaunch(), newLaunch()} }),
			want: []Event{
				created(IsolationNone),
				sessionEv("s2"), forkEv("s2", "main"), queuedEv("s2", "p4", "Build it"), turnEv("s2", "t5", "p4"),
				sessionEv("s6"), forkEv("s6", "main"), queuedEv("s6", "p8", "Build it"), turnEv("s6", "t9", "p8"),
				statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			status: TaskInProgress},

		// EditTask。
		{name: "edit", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdEditTask, Origin: OriginDesktop,
				EditTask: &EditTask{Title: "New", Brief: "Build it"}},
			want: []Event{{Kind: TaskEditedKind, TaskEdited: &EditTask{Title: "New", Brief: "Build it"}}}},
		{name: "edit identical", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdEditTask, Origin: OriginDesktop,
				EditTask: &EditTask{Title: "Login page", Brief: "Build it"}}},
		{name: "edit normalizes to same", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdEditTask, Origin: OriginDesktop,
				EditTask: &EditTask{Title: " Login page ", Brief: "Build it"}}},
		{name: "edit blank brief to empty",
			given: seqs(Event{Kind: TaskCreatedKind, TaskCreated: &TaskCreated{
				Title: "T", Workspace: "w1"}}), in: gitInputs(),
			cmd: Command{Kind: CmdEditTask, Origin: OriginDesktop,
				EditTask: &EditTask{Title: "T", Brief: "   "}}},
		{name: "edit empty title", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    Command{Kind: CmdEditTask, Origin: OriginDesktop, EditTask: &EditTask{Title: ""}},
			reason: ReasonInvalid, detail: "title"},
		{name: "edit missing payload", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    Command{Kind: CmdEditTask, Origin: OriginDesktop},
			reason: ReasonInvalid},

		// SetStatus。
		{name: "status in progress", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskInProgress}},
			want:   []Event{statusEv(TaskTodo, TaskInProgress, CauseUser)},
			status: TaskInProgress},
		{name: "status unchanged", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskTodo}}},
		{name: "status bad value", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: "bogus"}},
			reason: ReasonInvalid, detail: "status"},
		{name: "resolutions only for done", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskInProgress, Resolutions: []TargetResolution{
					resolve(worktree, Resolution{Action: IntentMerge}),
				}}},
			reason: ReasonInvalid, detail: "done"},
		{name: "done with open worktree", given: seqs(created(IsolationWorktree), attached()), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone}},
			reason: ReasonUnresolvedChanges},
		{name: "done with open fork",
			given:  seqs(created(IsolationNone), sessionEv("s2"), forkEv("s2", "main")),
			in:     gitInputs(),
			cmd:    Command{Kind: CmdSetStatus, Origin: OriginDesktop, SetStatus: &SetStatus{Status: TaskDone}},
			reason: ReasonUnresolvedChanges},
		{name: "done merging worktree", given: seqs(created(IsolationWorktree), attached()), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone, Resolutions: []TargetResolution{
					resolve(worktree, Resolution{Action: IntentMerge}),
				}}},
			want: []Event{
				mergeEv("i3", worktree, MergeSquash, "Login page\n\nBuild it"),
				statusEv(TaskTodo, TaskDone, CauseUser),
			},
			status: TaskDone},
		{name: "done discarding fork",
			given: seqs(created(IsolationNone), sessionEv("s2"), forkEv("s2", "main")),
			in:    gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone, Resolutions: []TargetResolution{
					resolve(forkS2, Resolution{Action: IntentDiscard}),
				}}},
			want: []Event{
				discardEv("i4", forkS2),
				statusEv(TaskTodo, TaskDone, CauseUser),
			},
			status: TaskDone},
		{name: "done resolutions from remote", given: seqs(created(IsolationWorktree), attached()), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginRemote,
				SetStatus: &SetStatus{Status: TaskDone, Resolutions: []TargetResolution{
					resolve(worktree, Resolution{Action: IntentMerge}),
				}}},
			reason: ReasonNeedsDesktop},
		{name: "done with pending intent",
			given: seqs(created(IsolationWorktree), attached(), mergeEv("i3", worktree, MergeSquash, "m")),
			in:    gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone}},
			want:   []Event{statusEv(TaskTodo, TaskDone, CauseUser)},
			status: TaskDone},
		{name: "resolution for merged worktree",
			given: seqs(created(IsolationWorktree), attached(),
				mergeEv("i3", worktree, MergeSquash, "m"),
				Event{Kind: WorktreeMergedKind, Merged: &IntentResult{Intent: "i3"}}),
			in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone, Resolutions: []TargetResolution{
					resolve(worktree, Resolution{Action: IntentMerge}),
				}}},
			reason: ReasonInvalid, detail: "resolved"},
		{name: "resolution for unknown session", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone, Resolutions: []TargetResolution{
					resolve(Target{Level: TargetFork, Session: "s9"}, Resolution{Action: IntentMerge}),
				}}},
			reason: ReasonInvalid, detail: "no worktree"},
		{name: "resolution bad action", given: seqs(created(IsolationWorktree), attached()), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone, Resolutions: []TargetResolution{
					resolve(worktree, Resolution{Action: "explode"}),
				}}},
			reason: ReasonInvalid, detail: "action"},
		{name: "resolution bad strategy", given: seqs(created(IsolationWorktree), attached()), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone, Resolutions: []TargetResolution{
					resolve(worktree, Resolution{Action: IntentMerge, Strategy: "rebase"}),
				}}},
			reason: ReasonInvalid, detail: "strategy"},
		{name: "done partial resolutions",
			given: seqs(created(IsolationWorktree), attached(), sessionEv("s3"), forkEv("s3", "orielhub/t-1")),
			in:    gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone, Resolutions: []TargetResolution{
					resolve(worktree, Resolution{Action: IntentMerge}),
				}}},
			reason: ReasonUnresolvedChanges},
		{name: "done plain", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone}},
			want:   []Event{statusEv(TaskTodo, TaskDone, CauseUser)},
			status: TaskDone},
		{name: "done pull request", given: seqs(created(IsolationWorktree), attached()), in: gitInputs(),
			cmd: Command{Kind: CmdSetStatus, Origin: OriginDesktop,
				SetStatus: &SetStatus{Status: TaskDone, Resolutions: []TargetResolution{
					resolve(worktree, Resolution{Action: IntentMerge, Strategy: MergePullRequest, Message: "PR me"}),
				}}},
			want: []Event{
				mergeEv("i3", worktree, MergePullRequest, "PR me"),
				statusEv(TaskTodo, TaskDone, CauseUser),
			},
			status: TaskDone},

		// Archive、Unarchive。
		{name: "archive", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd:  Command{Kind: CmdArchive, Origin: OriginDesktop},
			want: []Event{{Kind: TaskArchivedKind}}},
		{name: "archive running",
			given:  seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"), turnEv("s2", "t4", "p3")),
			in:     gitInputs(),
			cmd:    Command{Kind: CmdArchive, Origin: OriginDesktop},
			reason: ReasonSessionRunning},
		{name: "unarchive not archived", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdUnarchive, Origin: OriginDesktop}},

		// Pin、Unpin（D109，m2.md G01–G06）。
		{name: "pin", given: seqs(created(IsolationNone)), in: laterInputs(),
			cmd:  Command{Kind: CmdPin, Origin: OriginDesktop},
			want: []Event{{Kind: TaskPinnedKind}},
			check: func(t *testing.T, s State) {
				if !s.PinnedAt.Equal(laterInputs().Now) {
					t.Errorf("PinnedAt = %v, want the event's time", s.PinnedAt)
				}
			}},
		{name: "pin pinned keeps the time (G05)", given: seqs(created(IsolationNone), Event{Kind: TaskPinnedKind}), in: laterInputs(),
			cmd: Command{Kind: CmdPin, Origin: OriginDesktop},
			check: func(t *testing.T, s State) {
				if !s.PinnedAt.Equal(at0) {
					t.Errorf("PinnedAt = %v, want the first pin's %v", s.PinnedAt, at0)
				}
			}},
		{name: "unpin (G03)", given: seqs(created(IsolationNone), Event{Kind: TaskPinnedKind}), in: gitInputs(),
			cmd:  Command{Kind: CmdUnpin, Origin: OriginDesktop},
			want: []Event{{Kind: TaskUnpinnedKind}},
			check: func(t *testing.T, s State) {
				if !s.PinnedAt.IsZero() {
					t.Errorf("PinnedAt = %v, want zero", s.PinnedAt)
				}
			}},
		{name: "unpin not pinned (G05)", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdUnpin, Origin: OriginDesktop}},
		{name: "pin again takes the new time (G04)",
			given: seqs(created(IsolationNone), Event{Kind: TaskPinnedKind}, Event{Kind: TaskUnpinnedKind}), in: laterInputs(),
			cmd:  Command{Kind: CmdPin, Origin: OriginDesktop},
			want: []Event{{Kind: TaskPinnedKind}},
			check: func(t *testing.T, s State) {
				if !s.PinnedAt.Equal(laterInputs().Now) {
					t.Errorf("PinnedAt = %v, want the new pin's time", s.PinnedAt)
				}
			}},
		{name: "archive keeps the pin (G06)",
			given: seqs(created(IsolationNone), Event{Kind: TaskPinnedKind}), in: gitInputs(),
			cmd:  Command{Kind: CmdArchive, Origin: OriginDesktop},
			want: []Event{{Kind: TaskArchivedKind}},
			check: func(t *testing.T, s State) {
				if !s.PinnedAt.Equal(at0) {
					t.Errorf("PinnedAt = %v, want it kept", s.PinnedAt)
				}
			}},
		{name: "pin archived",
			given: seqs(created(IsolationNone), Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd:    Command{Kind: CmdPin, Origin: OriginDesktop},
			reason: ReasonArchived},
	})
}

// m2.md B01–B06、B12（D135）：優先級與標籤。
func TestDecidePriorityAndLabels(t *testing.T) {
	prioritySet := func(p TaskPriority) Event {
		return Event{Kind: TaskPrioritySetKind, TaskPrioritySet: &SetPriority{Priority: p}}
	}
	labelsSet := func(ids ...LabelID) Event {
		return Event{Kind: TaskLabelsSetKind, TaskLabelsSet: &SetLabels{Labels: ids}}
	}
	setPriority := func(p TaskPriority) Command {
		return Command{Kind: CmdSetPriority, Origin: OriginDesktop, SetPriority: &SetPriority{Priority: p}}
	}
	setLabels := func(ids ...LabelID) Command {
		return Command{Kind: CmdSetLabels, Origin: OriginDesktop, SetLabels: &SetLabels{Labels: ids}}
	}
	catalog := func() Inputs {
		in := laterInputs()
		in.Labels = []LabelID{"l-a", "l-b", "l-c"}
		return in
	}
	runDecide(t, []decideCase{
		{name: "B01 a new task has no priority and no labels", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdEditTask, Origin: OriginDesktop, EditTask: &EditTask{Title: "Login page", Brief: "Build it"}},
			check: func(t *testing.T, s State) {
				if s.Priority != PriorityNone || s.Labels != nil {
					t.Errorf("Priority, Labels = %q, %v; want none and no labels", s.Priority, s.Labels)
				}
			}},
		{name: "B02 set priority", given: seqs(created(IsolationNone)), in: laterInputs(),
			cmd:  setPriority(PriorityUrgent),
			want: []Event{prioritySet(PriorityUrgent)},
			check: func(t *testing.T, s State) {
				if s.Priority != PriorityUrgent {
					t.Errorf("Priority = %q", s.Priority)
				}
			}},
		{name: "B02 set priority back to none", given: seqs(created(IsolationNone), prioritySet(PriorityLow)), in: gitInputs(),
			cmd:  setPriority(PriorityNone),
			want: []Event{prioritySet(PriorityNone)}},
		{name: "B02 remote can set priority", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:  Command{Kind: CmdSetPriority, Origin: OriginRemote, Device: "d1", SetPriority: &SetPriority{Priority: PriorityMedium}},
			want: []Event{prioritySet(PriorityMedium)}},
		{name: "B03 same priority is a no-op", given: seqs(created(IsolationNone), prioritySet(PriorityHigh)), in: gitInputs(),
			cmd: setPriority(PriorityHigh)},
		{name: "B04 unknown priority", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: setPriority("critical"), reason: ReasonInvalid},
		{name: "B04 empty priority", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: setPriority(""), reason: ReasonInvalid},
		{name: "B05 priority keeps updatedAt", given: seqs(created(IsolationNone)), in: laterInputs(),
			cmd:  setPriority(PriorityHigh),
			want: []Event{prioritySet(PriorityHigh)},
			check: func(t *testing.T, s State) {
				if !s.UpdatedAt.Equal(at0) {
					t.Errorf("UpdatedAt = %v, want the last other event's %v", s.UpdatedAt, at0)
				}
			}},
		{name: "B05 labels keep updatedAt", given: seqs(created(IsolationNone)), in: catalog(),
			cmd:  setLabels("l-a"),
			want: []Event{labelsSet("l-a")},
			check: func(t *testing.T, s State) {
				if !s.UpdatedAt.Equal(at0) {
					t.Errorf("UpdatedAt = %v, want the last other event's %v", s.UpdatedAt, at0)
				}
			}},
		{name: "B06 priority on an archived task", given: seqs(created(IsolationNone), Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd: setPriority(PriorityHigh), reason: ReasonArchived},
		{name: "B06 labels on an archived task", given: seqs(created(IsolationNone), Event{Kind: TaskArchivedKind}), in: catalog(),
			cmd: setLabels("l-a"), reason: ReasonArchived},
		{name: "B06 unarchive keeps priority and labels",
			given: seqs(created(IsolationNone), prioritySet(PriorityHigh), labelsSet("l-a"), Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd:  Command{Kind: CmdUnarchive, Origin: OriginDesktop},
			want: []Event{{Kind: TaskUnarchivedKind}},
			check: func(t *testing.T, s State) {
				if s.Priority != PriorityHigh || !slices.Equal(s.Labels, []LabelID{"l-a"}) {
					t.Errorf("Priority, Labels = %q, %v", s.Priority, s.Labels)
				}
			}},
		{name: "B12 set labels sorts and drops repeats", given: seqs(created(IsolationNone)), in: catalog(),
			cmd:  setLabels("l-c", "l-a", "l-c"),
			want: []Event{labelsSet("l-a", "l-c")},
			check: func(t *testing.T, s State) {
				if !slices.Equal(s.Labels, []LabelID{"l-a", "l-c"}) {
					t.Errorf("Labels = %v", s.Labels)
				}
			}},
		{name: "B12 same set is a no-op", given: seqs(created(IsolationNone), labelsSet("l-a", "l-b")), in: catalog(),
			cmd: setLabels("l-b", "l-a")},
		{name: "B12 clear labels", given: seqs(created(IsolationNone), labelsSet("l-a")), in: catalog(),
			cmd:  setLabels(),
			want: []Event{labelsSet()},
			check: func(t *testing.T, s State) {
				if s.Labels != nil {
					t.Errorf("Labels = %v, want none", s.Labels)
				}
			}},
		{name: "B12 clearing no labels is a no-op", given: seqs(created(IsolationNone)), in: catalog(),
			cmd: setLabels()},
		{name: "B12 unknown label", given: seqs(created(IsolationNone)), in: catalog(),
			cmd: setLabels("l-a", "l-gone"), reason: ReasonUnknownLabel},
		{name: "B11 a deleted label still on the task cannot be set again", given: seqs(created(IsolationNone), labelsSet("l-a", "l-x")), in: catalog(),
			cmd: setLabels("l-a", "l-x"), reason: ReasonUnknownLabel},
		{name: "B11 dropping a deleted label", given: seqs(created(IsolationNone), labelsSet("l-a", "l-x")), in: catalog(),
			cmd:  setLabels("l-a"),
			want: []Event{labelsSet("l-a")}},
	})
}

// laterInputs 是 gitInputs 晚一小時：given 的事件都在 at0，用它看出新事件取的是這次的時間。
func laterInputs() Inputs {
	in := gitInputs()
	in.Now = at0.Add(time.Hour)
	return in
}
