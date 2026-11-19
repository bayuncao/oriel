package task

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func gitInputs() Inputs {
	return Inputs{
		Now:          at0,
		Repo:         Repo{IsGit: true, Branch: "main", Commit: "abc"},
		Policy:       PolicyAuto,
		WorktreeRoot: "/wt",
	}
}

// inputs 回傳以 gitInputs 修改後的 Inputs。
func inputs(f func(*Inputs)) Inputs {
	in := gitInputs()
	f(&in)
	return in
}

// seqs 為事件依次編號（從 1 開始）並填上 At。
func seqs(events ...Event) []Event {
	for i := range events {
		events[i].Seq = Seq(i + 1)
		events[i].At = at0
	}
	return events
}

// laterInputs 是 gitInputs 晚一小時：given 的事件都在 at0，用它看出新事件取的是這次的時間。
func laterInputs() Inputs {
	in := gitInputs()
	in.Now = at0.Add(time.Hour)
	return in
}

// fold 把 given 摺疊成 State{ID: id}，即 Decide 會看到的 State。
func fold(id TaskID, given []Event) State {
	s := State{ID: id}
	for _, e := range given {
		s = Evolve(s, e)
	}
	return s
}

// 事件建構器；每次呼叫回傳全新的值。

func created(iso IsolationMode) Event {
	return Event{Kind: TaskCreatedKind, TaskCreated: &TaskCreated{
		Title: "Login page", Brief: "Build it", Workspace: "w1", Isolation: iso,
	}}
}

func attached() Event {
	return Event{Kind: WorktreeAttachedKind, WorktreeAttached: &Worktree{
		Path: "/wt/t-1", Branch: "orielhub/t-1", BaseBranch: "main", BaseCommit: "abc",
	}}
}

func sessionEv(sid SessionID) Event {
	return sessionLaunchEv(sid, newLaunch())
}

func sessionLaunchEv(sid SessionID, l LaunchSnapshot) Event {
	return Event{Kind: SessionCreatedKind, Session: sid, SessionCreated: &SessionCreated{
		Launch: l, Role: RoleWorker,
	}}
}

func launchNoResume() LaunchSnapshot {
	l := newLaunch()
	l.Caps.Resume = false
	return l
}

func launchWithTimeout(d time.Duration, v Verdict) LaunchSnapshot {
	l := newLaunch()
	l.AskTimeout, l.TimeoutVerdict = d, v
	return l
}

func boundEv(sid SessionID, nid NativeSessionID) Event {
	return Event{Kind: NativeSessionBoundKind, Session: sid,
		NativeSessionBound: &NativeBound{NativeID: nid}}
}

func exitedEv(sid SessionID) Event {
	return Event{Kind: ProcessExitedKind, Session: sid, ProcessExited: &Exited{
		Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "0"}},
	}}
}

func askedEv(sid SessionID, id PermissionID, turn TurnID) Event {
	return askedReqEv(sid, id, turn, PermissionRequest{Tool: "Bash", Kind: ToolExecute})
}

func askedReqEv(sid SessionID, id PermissionID, turn TurnID, req PermissionRequest) Event {
	return Event{Kind: PermissionAskedKind, Session: sid, PermissionAsked: &Ask{
		ID: id, Turn: turn, Request: req, AskedAt: at0,
	}}
}

func decidedEv(sid SessionID, id PermissionID, dn Decision) Event {
	return Event{Kind: PermissionDecidedKind, Session: sid, PermissionDecided: &PermissionDecided{
		Permission: id, Decision: dn,
	}}
}

func answeredEv(sid SessionID, id PermissionID) Event {
	return Event{Kind: PermissionAnsweredKind, Session: sid,
		PermissionAnswered: &PermissionRef{Permission: id}}
}

func ruleEv(r Rule) Event {
	return Event{Kind: RuleAddedKind, RuleAdded: &r}
}

func confReqEv(id ConfirmationID, by DeviceID, cmd Command) Event {
	return Event{Kind: ConfirmationRequestedKind, ConfirmationRequested: &Confirmation{
		ID: id, Command: cmd, RequestedBy: by, RequestedAt: at0,
	}}
}

func grantedEv(id ConfirmationID) Event {
	return Event{Kind: ConfirmationGrantedKind, ConfirmationGranted: &ConfirmationRef{Confirmation: id}}
}

func cancelledConfEv(id ConfirmationID) Event {
	return Event{Kind: ConfirmationCancelledKind, ConfirmationCancelled: &ConfirmationRef{Confirmation: id}}
}

func effectFailedEv(key EffectKey, p Problem) Event {
	return Event{Kind: EffectFailedKind, EffectFailed: &EffectFailure{Effect: key, Problem: p}}
}

func retriedEv(key EffectKey) Event {
	return Event{Kind: EffectRetriedKind, EffectRetried: &RetryEffect{Effect: key}}
}

func intentCancelledEv(id IntentID) Event {
	return Event{Kind: IntentCancelledKind, IntentCancelled: &IntentRef{Intent: id}}
}

func conflictEv(id IntentID, files ...string) Event {
	return Event{Kind: MergeConflictedKind, MergeConflicted: &MergeConflict{Intent: id, Files: files}}
}

func completedEv(sid SessionID) Event {
	return Event{Kind: TurnCompletedKind, Session: sid}
}

func cancelledEv(sid SessionID) Event {
	return Event{Kind: TurnCancelledKind, Session: sid}
}

func cancelReqEv(sid SessionID) Event {
	return Event{Kind: TurnCancelRequestedKind, Session: sid}
}

func messageEv(sid SessionID, mid MessageID, tid TurnID, ch ChannelKind, text string, partial bool) Event {
	return Event{Kind: MessageAppendedKind, Session: sid, MessageAppended: &MessageAppended{
		Message: mid, Turn: tid, Channel: ch, Text: text, Partial: partial,
	}}
}

func closedEv(sid SessionID) Event {
	return Event{Kind: SessionClosedKind, Session: sid}
}

func conflictedEv(sid SessionID) Event {
	return Event{Kind: SessionConflictedKind, Session: sid}
}

func resumedEv(sid SessionID) Event {
	return Event{Kind: SessionResumedKind, Session: sid}
}

func nativeDeletedEv(sid SessionID, iid IntentID) Event {
	return Event{Kind: NativeSessionDeletedKind, Session: sid, NativeSessionDeleted: &Intent{
		ID: iid, Kind: IntentDeleteNative, Target: Target{Session: sid},
	}}
}

func cancelledPermEv(sid SessionID, id PermissionID) Event {
	return Event{Kind: PermissionCancelledKind, Session: sid,
		PermissionCancelled: &PermissionRef{Permission: id}}
}

func forkEv(sid SessionID, base string) Event {
	return Event{Kind: ForkCreatedKind, Session: sid, ForkCreated: &Worktree{
		Path: "/wt/t-1-" + string(sid), Branch: "orielhub/t-1-" + string(sid),
		BaseBranch: base, BaseCommit: "abc",
	}}
}

func queuedEv(sid SessionID, pid PromptID, text string) Event {
	return Event{Kind: PromptQueuedKind, Session: sid,
		PromptQueued: &Prompt{ID: pid, Text: text}}
}

func turnEv(sid SessionID, tid TurnID, pid PromptID) Event {
	return Event{Kind: TurnStartedKind, Session: sid,
		TurnStarted: &TurnStarted{Turn: tid, Prompt: pid}}
}

func statusEv(from, to TaskStatus, cause StatusCause) Event {
	return Event{Kind: TaskStatusChangedKind, TaskStatusChanged: &TaskStatusChanged{
		From: from, To: to, Cause: cause,
	}}
}

func mergeEv(id IntentID, t Target, strategy MergeStrategy, msg string) Event {
	return Event{Kind: MergeRequestedKind, Session: t.Session, MergeRequested: &Intent{
		ID: id, Kind: IntentMerge, Target: t, Strategy: strategy, Message: msg,
	}}
}

func discardEv(id IntentID, t Target) Event {
	return Event{Kind: DiscardRequestedKind, Session: t.Session, DiscardRequested: &Intent{
		ID: id, Kind: IntentDiscard, Target: t,
	}}
}

// create 回傳一個 CreateTask 命令；f 可修改 payload。
func create(f func(*CreateTask)) Command {
	p := &CreateTask{Title: "Login page", Brief: "Build it", Workspace: "w1"}
	if f != nil {
		f(p)
	}
	return Command{Kind: CmdCreateTask, Origin: OriginDesktop, CreateTask: p}
}

// sessionCmd 回傳一個 session 層級的命令；set 可填入 payload。
func sessionCmd(kind CommandKind, sid SessionID, origin Origin, set func(*Command)) Command {
	c := Command{Kind: kind, Origin: origin, Session: sid}
	if set != nil {
		set(&c)
	}
	return c
}

func commandKinds(t *testing.T) map[CommandKind]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "command.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[CommandKind]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "CommandKind" {
			return true
		}
		for i, v := range vs.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("%s is not a string literal", vs.Names[i].Name)
			}
			s, _ := strconv.Unquote(lit.Value)
			kinds[CommandKind(s)] = vs.Names[i].Name
		}
		return true
	})
	return kinds
}

// TestKindsCoverCommand 確認 kinds 涵蓋 command.go 宣告的每個
// CommandKind，事實旗標與名稱的 Fact 前綴一致，且 cmdSlots
// 恰好是 Command 的全部指標欄位。
func TestKindsCoverCommand(t *testing.T) {
	declared := commandKinds(t)
	for k, name := range declared {
		ki, ok := kinds[k]
		if !ok {
			t.Errorf("%s is not in kinds", name)
			continue
		}
		if want := strings.HasPrefix(name, "Fact"); ki.fact != want {
			t.Errorf("%s: fact flag = %v, want %v", name, ki.fact, want)
		}
	}
	if got, want := len(kinds), len(declared); got != want {
		t.Errorf("kinds covers %d kinds, command.go declares %d", got, want)
	}
	ty := reflect.TypeOf(Command{})
	var fields []string
	for i := 0; i < ty.NumField(); i++ {
		if ty.Field(i).Type.Kind() == reflect.Pointer {
			fields = append(fields, ty.Field(i).Name)
		}
	}
	var slots []string
	for _, s := range cmdSlots {
		slots = append(slots, s.field)
	}
	if !slices.Equal(slots, fields) {
		t.Errorf("cmdSlots = %v, Command pointer fields = %v", slots, fields)
	}
}

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

// decideCase 是一次 Decide 的測試案例；status 非空時額外檢查摺疊後的 TaskStatus，
// check 非空時對摺疊後的 State 做額外斷言。
type decideCase struct {
	name   string
	noID   bool // State.ID 為空
	given  []Event
	in     Inputs
	cmd    Command
	want   []Event
	reason Reason
	detail string
	status TaskStatus // 摺疊後應有的 Status
	check  func(t *testing.T, folded State)
}

func runDecide(t *testing.T, cases []decideCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := TaskID("t-1")
			if tc.noID {
				id = ""
			}
			s := fold(id, tc.given)
			got, err := Decide(s, tc.in, tc.cmd)
			if tc.reason != "" {
				var rej *Rejection
				if !errors.As(err, &rej) || rej.Reason != tc.reason {
					t.Fatalf("err = %v, want rejection %s", err, tc.reason)
				}
				if tc.detail != "" && !strings.Contains(rej.Detail, tc.detail) {
					t.Fatalf("detail = %q, want substring %q", rej.Detail, tc.detail)
				}
				if got != nil {
					t.Fatalf("got events %+v on rejection", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			var want []Event
			if tc.want != nil {
				want = make([]Event, len(tc.want))
				copy(want, tc.want)
				for i := range want {
					want[i].Seq = s.Version + Seq(i) + 1
					want[i].At = tc.in.Now
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got\n%+v\nwant\n%+v", got, want)
			}
			for i, e := range got {
				if want := s.Version + Seq(i) + 1; e.Seq != want {
					t.Errorf("event %d seq = %d, want %d", i, e.Seq, want)
				}
				if !e.At.Equal(tc.in.Now) {
					t.Errorf("event %d at = %v, want %v", i, e.At, tc.in.Now)
				}
				if _, err := EncodeEvent(e); err != nil {
					t.Errorf("event %d (%s) does not encode: %v", i, e.Kind, err)
				}
			}
			folded := s
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("folding emitted events panicked: %v", r)
					}
				}()
				for _, e := range got {
					folded = Evolve(folded, e)
				}
			}()
			if tc.status != "" && folded.Status != tc.status {
				t.Errorf("folded status = %s, want %s", folded.Status, tc.status)
			}
			if tc.check != nil {
				tc.check(t, folded)
			}
			if !reflect.DeepEqual(s, fold(id, tc.given)) {
				t.Fatal("Decide modified the input state")
			}
		})
	}
}

// running 是一個回合進行中、狀態為 in_progress 的 Task。
func running() []Event {
	return seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
		turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto))
}

func TestDecideSessions(t *testing.T) {
	forkS2 := Target{Level: TargetFork, Session: "s2"}
	runDecide(t, []decideCase{
		// CreateSession。
		{name: "session", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:  Command{Kind: CmdCreateSession, Origin: OriginDesktop, CreateSession: &CreateSession{Launch: newLaunch()}},
			want: []Event{sessionEv("s2")}},
		{name: "session with prompt", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdCreateSession, Origin: OriginDesktop, CreateSession: &CreateSession{
				Launch: newLaunch(), Prompt: &Prompt{Text: "Build it"}}},
			want: []Event{
				sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			status: TaskInProgress},
		{name: "session remote", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:  Command{Kind: CmdCreateSession, Origin: OriginRemote, CreateSession: &CreateSession{Launch: newLaunch()}},
			want: []Event{sessionEv("s2")}},
		{name: "session bad launch", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdCreateSession, Origin: OriginDesktop, CreateSession: &CreateSession{
				Launch: LaunchSnapshot{Agent: "claude"}}},
			reason: ReasonInvalid, detail: "launch"},
		{name: "session empty prompt", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdCreateSession, Origin: OriginDesktop, CreateSession: &CreateSession{
				Launch: newLaunch(), Prompt: &Prompt{}}},
			reason: ReasonInvalid, detail: "empty prompt"},
		{name: "session missing payload", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    Command{Kind: CmdCreateSession, Origin: OriginDesktop},
			reason: ReasonInvalid},
		{name: "create task bad launch", in: gitInputs(),
			cmd:    create(func(p *CreateTask) { p.Launches = []LaunchSnapshot{{Agent: "claude"}} }),
			reason: ReasonInvalid, detail: "launch"},

		// L1–L8：標籤與空白的正規化（D65）。
		{name: "create trims title", in: gitInputs(),
			cmd: create(func(p *CreateTask) { p.Title = "  Foo  " }),
			want: []Event{{Kind: TaskCreatedKind, TaskCreated: &TaskCreated{
				Title: "Foo", Brief: "Build it", Workspace: "w1", Isolation: IsolationNone}}},
			status: TaskTodo},
		{name: "create title all blanks", in: gitInputs(),
			cmd:    create(func(p *CreateTask) { p.Title = "   " }),
			reason: ReasonInvalid, detail: "title"},
		{name: "create title tab only", in: gitInputs(),
			cmd:    create(func(p *CreateTask) { p.Title = "\t" }),
			reason: ReasonInvalid, detail: "title"},
		{name: "create title fullwidth space", in: gitInputs(),
			cmd:    create(func(p *CreateTask) { p.Title = "\u3000" }),
			reason: ReasonInvalid, detail: "title"},
		{name: "create title newline", in: gitInputs(),
			cmd:    create(func(p *CreateTask) { p.Title = "a\nb" }),
			reason: ReasonInvalid, detail: "title"},
		{name: "create title carriage return", in: gitInputs(),
			cmd:    create(func(p *CreateTask) { p.Title = "a\rb" }),
			reason: ReasonInvalid, detail: "title"},
		{name: "create title inner tab kept", in: gitInputs(),
			cmd: create(func(p *CreateTask) { p.Title = "a\tb" }),
			want: []Event{{Kind: TaskCreatedKind, TaskCreated: &TaskCreated{
				Title: "a\tb", Brief: "Build it", Workspace: "w1", Isolation: IsolationNone}}},
			status: TaskTodo},
		{name: "create blank brief empty", in: gitInputs(),
			cmd: create(func(p *CreateTask) { p.Brief = "   " }),
			want: []Event{{Kind: TaskCreatedKind, TaskCreated: &TaskCreated{
				Title: "Login page", Workspace: "w1", Isolation: IsolationNone}}},
			status: TaskTodo},
		{name: "create blank brief with launches", in: gitInputs(),
			cmd: create(func(p *CreateTask) {
				p.Brief = " \n "
				p.Launches = []LaunchSnapshot{newLaunch()}
			}),
			reason: ReasonInvalid, detail: "brief"},
		{name: "create brief kept verbatim", in: gitInputs(),
			cmd: create(func(p *CreateTask) { p.Brief = "  code\n" }),
			want: []Event{{Kind: TaskCreatedKind, TaskCreated: &TaskCreated{
				Title: "Login page", Brief: "  code\n", Workspace: "w1", Isolation: IsolationNone}}},
			status: TaskTodo},

		// Submit。
		{name: "submit idle", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Text: "more"}}
			}),
			want: []Event{
				queuedEv("s2", "p3", "more"), turnEv("s2", "t4", "p3"),
				statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			status: TaskInProgress},
		{name: "submit queues while running", given: running(), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Text: "more"}}
			}),
			want:   []Event{queuedEv("s2", "p6", "more")},
			status: TaskInProgress},
		{name: "submit queues behind pending ask",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
				askedEv("s2", "a6", "t4")),
			in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Text: "more"}}
			}),
			want: []Event{queuedEv("s2", "p7", "more")}},
		{name: "submit unknown session", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s9", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Text: "x"}}
			}),
			reason: ReasonUnknownSession},
		{name: "submit closed", given: seqs(created(IsolationNone), sessionEv("s2"), closedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Text: "x"}}
			}),
			reason: ReasonSessionClosed},
		{name: "submit exited", given: seqs(created(IsolationNone), sessionEv("s2"), exitedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Text: "x"}}
			}),
			reason: ReasonSessionExited},
		{name: "submit conflicted",
			given: seqs(created(IsolationNone), sessionEv("s2"), conflictedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Text: "x"}}
			}),
			reason: ReasonConflicted},
		{name: "submit empty prompt", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{}}
			}),
			reason: ReasonInvalid, detail: "empty prompt"},
		{name: "submit attachments only", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Attachments: []Attachment{{Name: "a.png", MIME: "image/png"}}}}
			}),
			want: []Event{
				{Kind: PromptQueuedKind, Session: "s2", PromptQueued: &Prompt{
					ID: "p3", Attachments: []Attachment{{Name: "a.png", MIME: "image/png"}}}},
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
			}},
		{name: "submit blank text", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Text: "  "}}
			}),
			reason: ReasonInvalid, detail: "empty prompt"},
		{name: "submit blank text with attachment", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdSubmit, "s2", OriginDesktop, func(c *Command) {
				c.Submit = &Submit{Prompt: Prompt{Text: "  ",
					Attachments: []Attachment{{Name: "a.png", MIME: "image/png"}}}}
			}),
			want: []Event{
				{Kind: PromptQueuedKind, Session: "s2", PromptQueued: &Prompt{
					ID: "p3", Text: "  ",
					Attachments: []Attachment{{Name: "a.png", MIME: "image/png"}}}},
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
			}},

		// CancelTurn。
		{name: "cancel turn", given: running(), in: gitInputs(),
			cmd:  sessionCmd(CmdCancelTurn, "s2", OriginDesktop, nil),
			want: []Event{cancelReqEv("s2")}},
		{name: "cancel already cancelling",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), cancelReqEv("s2")),
			in:  gitInputs(),
			cmd: sessionCmd(CmdCancelTurn, "s2", OriginDesktop, nil)},
		{name: "cancel no turn", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdCancelTurn, "s2", OriginDesktop, nil)},
		{name: "cancel closed", given: seqs(created(IsolationNone), sessionEv("s2"), closedEv("s2")), in: gitInputs(),
			cmd:    sessionCmd(CmdCancelTurn, "s2", OriginDesktop, nil),
			reason: ReasonSessionClosed},

		// Resume。
		{name: "resume bound",
			given: seqs(created(IsolationNone), sessionEv("s2"), boundEv("s2", "n5"), exitedEv("s2")),
			in:    gitInputs(),
			cmd:   sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			want:  []Event{resumedEv("s2")}},
		{name: "resume never started",
			given: seqs(created(IsolationNone), sessionEv("s2"), exitedEv("s2")),
			in:    gitInputs(),
			cmd:   sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			want:  []Event{resumedEv("s2")}},
		{name: "resume continues queue",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"), exitedEv("s2")),
			in:    gitInputs(),
			cmd:   sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			want: []Event{
				resumedEv("s2"), turnEv("s2", "t6", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			status: TaskInProgress},
		{name: "resume needs resume cap",
			given:  seqs(created(IsolationNone), sessionLaunchEv("s2", launchNoResume()), boundEv("s2", "n3"), exitedEv("s2")),
			in:     gitInputs(),
			cmd:    sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			reason: ReasonNotResumable},
		{name: "resume completed without native id",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), completedEv("s2"), exitedEv("s2")),
			in:     gitInputs(),
			cmd:    sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			reason: ReasonNotResumable},
		{name: "resume native gone",
			given: seqs(created(IsolationNone), sessionEv("s2"), boundEv("s2", "n3"),
				nativeDeletedEv("s2", "i4"), exitedEv("s2")),
			in:     gitInputs(),
			cmd:    sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			reason: ReasonNotResumable},
		{name: "resume idle", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd:    sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			reason: ReasonNotResumable},
		{name: "resume conflicted",
			given:  seqs(created(IsolationNone), sessionEv("s2"), exitedEv("s2"), conflictedEv("s2")),
			in:     gitInputs(),
			cmd:    sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			reason: ReasonConflicted},

		// CloseSession。
		{name: "close idle", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd:  sessionCmd(CmdCloseSession, "s2", OriginDesktop, func(c *Command) { c.CloseSession = &CloseSession{} }),
			want: []Event{closedEv("s2")}},
		{name: "close running cleans up",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), completedEv("s2"), queuedEv("s2", "p6", "again"),
				turnEv("s2", "t7", "p6"), messageEv("s2", "m8", "t7", ChannelText, "work", true),
				askedEv("s2", "a9", "t7"), statusEv(TaskTodo, TaskInProgress, CauseAuto)),
			in:  gitInputs(),
			cmd: sessionCmd(CmdCloseSession, "s2", OriginDesktop, func(c *Command) { c.CloseSession = &CloseSession{} }),
			want: []Event{
				messageEv("s2", "m8", "t7", ChannelText, "", false), cancelledPermEv("s2", "a9"),
				cancelledEv("s2"), closedEv("s2"), statusEv(TaskInProgress, TaskInReview, CauseAuto),
			},
			status: TaskInReview},
		{name: "close running before first turn ends",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), messageEv("s2", "m5", "t4", ChannelText, "work", true),
				askedEv("s2", "a6", "t4"), statusEv(TaskTodo, TaskInProgress, CauseAuto)),
			in:  gitInputs(),
			cmd: sessionCmd(CmdCloseSession, "s2", OriginDesktop, func(c *Command) { c.CloseSession = &CloseSession{} }),
			want: []Event{
				messageEv("s2", "m5", "t4", ChannelText, "", false), cancelledPermEv("s2", "a6"),
				cancelledEv("s2"), closedEv("s2"),
			},
			status: TaskInProgress},
		{name: "close cancels asks without turn",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), completedEv("s2"), askedEv("s2", "a6", "t4")),
			in:   gitInputs(),
			cmd:  sessionCmd(CmdCloseSession, "s2", OriginDesktop, func(c *Command) { c.CloseSession = &CloseSession{} }),
			want: []Event{cancelledPermEv("s2", "a6"), closedEv("s2")}},
		{name: "close open fork needs resolution",
			given: seqs(created(IsolationNone), sessionEv("s2"), forkEv("s2", "main")), in: gitInputs(),
			cmd:    sessionCmd(CmdCloseSession, "s2", OriginDesktop, func(c *Command) { c.CloseSession = &CloseSession{} }),
			reason: ReasonUnresolvedChanges},
		{name: "close fork with discard",
			given: seqs(created(IsolationNone), sessionEv("s2"), forkEv("s2", "main")), in: gitInputs(),
			cmd: sessionCmd(CmdCloseSession, "s2", OriginDesktop, func(c *Command) {
				c.CloseSession = &CloseSession{Resolution: &Resolution{Action: IntentDiscard}}
			}),
			want: []Event{discardEv("i4", forkS2), closedEv("s2")}},
		{name: "close fork remote keeps",
			given: seqs(created(IsolationNone), sessionEv("s2"), forkEv("s2", "main")), in: gitInputs(),
			cmd: sessionCmd(CmdCloseSession, "s2", OriginRemote, func(c *Command) { c.CloseSession = &CloseSession{} }),
			want: []Event{
				{Kind: WorktreeKeptKind, Session: "s2", WorktreeKept: &forkS2},
				closedEv("s2"),
			}},
		{name: "close fork remote resolution refused",
			given: seqs(created(IsolationNone), sessionEv("s2"), forkEv("s2", "main")), in: gitInputs(),
			cmd: sessionCmd(CmdCloseSession, "s2", OriginRemote, func(c *Command) {
				c.CloseSession = &CloseSession{Resolution: &Resolution{Action: IntentDiscard}}
			}),
			reason: ReasonNeedsDesktop},
		{name: "close fork already has intent",
			given: seqs(created(IsolationNone), sessionEv("s2"), forkEv("s2", "main"), discardEv("i4", forkS2)),
			in:    gitInputs(),
			cmd: sessionCmd(CmdCloseSession, "s2", OriginDesktop, func(c *Command) {
				c.CloseSession = &CloseSession{Resolution: &Resolution{Action: IntentDiscard}}
			}),
			reason: ReasonInvalid, detail: "no open fork"},
		{name: "close closed", given: seqs(created(IsolationNone), sessionEv("s2"), closedEv("s2")), in: gitInputs(),
			cmd:    sessionCmd(CmdCloseSession, "s2", OriginDesktop, func(c *Command) { c.CloseSession = &CloseSession{} }),
			reason: ReasonSessionClosed},
		{name: "close missing payload", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd:    sessionCmd(CmdCloseSession, "s2", OriginDesktop, nil),
			reason: ReasonInvalid},

		// TakeOver。
		{name: "takeover", given: seqs(created(IsolationNone), sessionEv("s2"), conflictedEv("s2")), in: gitInputs(),
			cmd:  sessionCmd(CmdTakeOver, "s2", OriginDesktop, nil),
			want: []Event{{Kind: SessionTakenOverKind, Session: "s2"}}},
		{name: "takeover not conflicted", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdTakeOver, "s2", OriginDesktop, nil)},

		// 執行期事實。
		{name: "native bound", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactNativeBound, "s2", OriginRuntime, func(c *Command) {
				c.Bound = &NativeBound{NativeID: "n9"}
			}),
			want: []Event{boundEv("s2", "n9")}},
		{name: "native bound same",
			given: seqs(created(IsolationNone), sessionEv("s2"), boundEv("s2", "n9")), in: gitInputs(),
			cmd: sessionCmd(FactNativeBound, "s2", OriginRuntime, func(c *Command) {
				c.Bound = &NativeBound{NativeID: "n9"}
			})},
		{name: "native bound rebind after exit",
			given: seqs(created(IsolationNone), sessionEv("s2"), boundEv("s2", "n9"),
				exitedEv("s2"), resumedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactNativeBound, "s2", OriginRuntime, func(c *Command) {
				c.Bound = &NativeBound{NativeID: "n9"}
			}),
			want: []Event{boundEv("s2", "n9")}},
		{name: "native_bound after exit is rejected",
			given: seqs(created(IsolationNone), sessionEv("s2"), boundEv("s2", "n9"),
				exitedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactNativeBound, "s2", OriginRuntime, func(c *Command) {
				c.Bound = &NativeBound{NativeID: "n9"}
			}),
			reason: ReasonSessionExited},
		{name: "native_bound after resume is recorded",
			given: seqs(created(IsolationNone), sessionEv("s2"), boundEv("s2", "n8"),
				exitedEv("s2"), resumedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactNativeBound, "s2", OriginRuntime, func(c *Command) {
				c.Bound = &NativeBound{NativeID: "n9"}
			}),
			want: []Event{boundEv("s2", "n9")}},
		{name: "output first segment", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactOutput, "s2", OriginRuntime, func(c *Command) {
				c.Output = &Output{Channel: ChannelText, Text: "a", Partial: true}
			}),
			want: []Event{messageEv("s2", "m6", "t4", ChannelText, "a", true)}},
		{name: "output continues open message",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
				messageEv("s2", "m6", "t4", ChannelText, "a", true)),
			in: gitInputs(),
			cmd: sessionCmd(FactOutput, "s2", OriginRuntime, func(c *Command) {
				c.Output = &Output{Channel: ChannelText, Text: "b"}
			}),
			want: []Event{messageEv("s2", "m6", "t4", ChannelText, "b", false)}},
		{name: "output switches channel",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
				messageEv("s2", "m6", "t4", ChannelThinking, "think", true)),
			in: gitInputs(),
			cmd: sessionCmd(FactOutput, "s2", OriginRuntime, func(c *Command) {
				c.Output = &Output{Channel: ChannelText, Text: "a"}
			}),
			want: []Event{
				messageEv("s2", "m6", "t4", ChannelThinking, "", false),
				messageEv("s2", "m8", "t4", ChannelText, "a", false),
			}},
		{name: "output without turn", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactOutput, "s2", OriginRuntime, func(c *Command) {
				c.Output = &Output{Channel: ChannelText, Text: "a"}
			}),
			reason: ReasonInvalid, detail: "no turn"},
		{name: "tool started closes message",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
				messageEv("s2", "m6", "t4", ChannelThinking, "think", true)),
			in: gitInputs(),
			cmd: sessionCmd(FactToolStarted, "s2", OriginRuntime, func(c *Command) {
				c.ToolStarted = &ToolCall{ID: "toolu_1", Tool: "Bash", Kind: ToolExecute}
			}),
			want: []Event{
				messageEv("s2", "m6", "t4", ChannelThinking, "", false),
				{Kind: ToolCallStartedKind, Session: "s2",
					ToolCallStarted: &ToolCall{ID: "toolu_1", Tool: "Bash", Kind: ToolExecute}},
			}},
		{name: "tool started", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactToolStarted, "s2", OriginRuntime, func(c *Command) {
				c.ToolStarted = &ToolCall{ID: "toolu_1", Tool: "Bash", Kind: ToolExecute}
			}),
			want: []Event{{Kind: ToolCallStartedKind, Session: "s2",
				ToolCallStarted: &ToolCall{ID: "toolu_1", Tool: "Bash", Kind: ToolExecute}}}},
		{name: "tool updated", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactToolUpdated, "s2", OriginRuntime, func(c *Command) {
				c.ToolUpdated = &ToolUpdate{ID: "toolu_1", Status: ToolRunning, Output: "x"}
			}),
			want: []Event{{Kind: ToolCallUpdatedKind, Session: "s2",
				ToolCallUpdated: &ToolUpdate{ID: "toolu_1", Status: ToolRunning, Output: "x"}}}},
		{name: "plan", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactPlan, "s2", OriginRuntime, func(c *Command) {
				c.Plan = &PlanUpdate{Entries: []PlanEntry{{Text: "step", Status: PlanInProgress}}}
			}),
			want: []Event{{Kind: PlanUpdatedKind, Session: "s2",
				PlanUpdated: &PlanUpdate{Entries: []PlanEntry{{Text: "step", Status: PlanInProgress}}}}}},
		{name: "usage", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactUsage, "s2", OriginRuntime, func(c *Command) {
				c.Usage = &Usage{InputTokens: 10, OutputTokens: 5}
			}),
			want: []Event{{Kind: UsageReportedKind, Session: "s2",
				UsageReported: &Usage{InputTokens: 10, OutputTokens: 5}}}},
		{name: "tool updated without turn", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactToolUpdated, "s2", OriginRuntime, func(c *Command) {
				c.ToolUpdated = &ToolUpdate{ID: "toolu_1"}
			}),
			reason: ReasonInvalid, detail: "no turn"},

		// TurnEnded。
		{name: "turn completed to review", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s2", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want:   []Event{completedEv("s2"), statusEv(TaskInProgress, TaskInReview, CauseAuto)},
			status: TaskInReview},
		{name: "turn completed starts next",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
				queuedEv("s2", "p6", "more")),
			in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s2", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want:   []Event{completedEv("s2"), turnEv("s2", "t8", "p6")},
			status: TaskInProgress},
		{name: "turn completed sibling running",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), sessionEv("s5"), queuedEv("s5", "p6", "Build it"),
				turnEv("s5", "t7", "p6"), statusEv(TaskTodo, TaskInProgress, CauseAuto)),
			in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s2", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want:   []Event{completedEv("s2")},
			status: TaskInProgress},
		{name: "turn cancelled", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s2", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnCancelled}
			}),
			want: []Event{cancelledEv("s2")}},
		{name: "turn failed", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s2", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnError,
					Problem: &Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}}
			}),
			want: []Event{{Kind: TurnFailedKind, Session: "s2", TurnFailed: &TurnFailed{
				Turn: "t4", Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}}}}},
		{name: "turn failed without problem", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s2", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnError}
			}),
			reason: ReasonInvalid, detail: "problem"},
		{name: "turn unknown result", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s2", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: "bogus"}
			}),
			reason: ReasonInvalid, detail: "turn result"},
		{name: "turn ended cancels asks sorted",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
				askedEv("s2", "a8", "t4"), askedEv("s2", "a5", "t4")),
			in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s2", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want: []Event{
				cancelledPermEv("s2", "a5"), cancelledPermEv("s2", "a8"),
				completedEv("s2"), statusEv(TaskInProgress, TaskInReview, CauseAuto),
			},
			status: TaskInReview},

		// Exited、Conflicted。
		{name: "exited fails turn",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), completedEv("s2"), queuedEv("s2", "p6", "more"),
				turnEv("s2", "t7", "p6"), statusEv(TaskTodo, TaskInProgress, CauseAuto)),
			in: gitInputs(),
			cmd: sessionCmd(FactExited, "s2", OriginRuntime, func(c *Command) {
				c.Exited = &Exited{Problem: Problem{Code: "proc.signaled", Params: map[string]string{"signal": "9"}},
					StderrTail: "boom"}
			}),
			want: []Event{
				{Kind: TurnFailedKind, Session: "s2", TurnFailed: &TurnFailed{
					Turn: "t7", Problem: Problem{Code: "proc.signaled", Params: map[string]string{"signal": "9"}}}},
				{Kind: ProcessExitedKind, Session: "s2", ProcessExited: &Exited{
					Problem: Problem{Code: "proc.signaled", Params: map[string]string{"signal": "9"}}, StderrTail: "boom"}},
				statusEv(TaskInProgress, TaskInReview, CauseAuto),
			},
			status: TaskInReview},
		{name: "exited idle", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactExited, "s2", OriginRuntime, func(c *Command) {
				c.Exited = &Exited{Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "0"}}}
			}),
			want: []Event{exitedEv("s2")}},
		{name: "exited cancels asks without turn",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), completedEv("s2"), askedEv("s2", "a6", "t4")),
			in: gitInputs(),
			cmd: sessionCmd(FactExited, "s2", OriginRuntime, func(c *Command) {
				c.Exited = &Exited{Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "0"}}}
			}),
			want: []Event{cancelledPermEv("s2", "a6"), exitedEv("s2")}},
		{name: "exited idempotent",
			given: seqs(created(IsolationNone), sessionEv("s2"), exitedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactExited, "s2", OriginRuntime, func(c *Command) {
				c.Exited = &Exited{Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "0"}}}
			})},
		{name: "conflicted", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd:  sessionCmd(FactConflicted, "s2", OriginRuntime, nil),
			want: []Event{conflictedEv("s2")}},
		{name: "conflicted again",
			given: seqs(created(IsolationNone), sessionEv("s2"), conflictedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactConflicted, "s2", OriginRuntime, nil)},
		{name: "fact on closed session",
			given: seqs(created(IsolationNone), sessionEv("s2"), closedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactOutput, "s2", OriginRuntime, func(c *Command) {
				c.Output = &Output{Channel: ChannelText, Text: "a"}
			}),
			reason: ReasonSessionClosed},
		{name: "fact on archived task",
			given: seqs(created(IsolationNone), sessionEv("s2"), Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd: sessionCmd(FactExited, "s2", OriginRuntime, func(c *Command) {
				c.Exited = &Exited{Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "0"}}}
			}),
			want: []Event{exitedEv("s2")}},
	})
}

// and 在既有事件後續編更多事件（重新編號）。
func and(given []Event, more ...Event) []Event {
	out := make([]Event, 0, len(given)+len(more))
	out = append(out, given...)
	out = append(out, more...)
	for i := range out {
		out[i].Seq = Seq(i + 1)
		out[i].At = at0
	}
	return out
}

func TestDecidePermissions(t *testing.T) {
	bashGo := PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test ./..."}
	read := PermissionRequest{Tool: "Read", Kind: ToolRead}
	edit := PermissionRequest{Tool: "Edit", Kind: ToolEdit}
	taskPrefix := Rule{ID: "r6", Scope: ScopeTask, ScopeID: "t-1", Match: MatchPrefix,
		Tool: "Bash", Pattern: "go test", Verdict: Allow}
	wsRule := Rule{ID: "w9", Scope: ScopeWorkspace, ScopeID: "w1", Match: MatchPrefix,
		Tool: "Bash", Pattern: "go test", Verdict: Allow}
	askedBash := and(running(), askedReqEv("s2", "a6", "t4", bashGo)) // v6
	runDecide(t, []decideCase{
		// FactAsked。
		{name: "asked", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactAsked, "s2", OriginRuntime, func(c *Command) {
				c.Asked = &Asked{ID: "a6", Request: read}
			}),
			want: []Event{askedReqEv("s2", "a6", "t4", read)}},
		{name: "asked matches task rule",
			given: and(running(), ruleEv(taskPrefix)), in: gitInputs(),
			cmd: sessionCmd(FactAsked, "s2", OriginRuntime, func(c *Command) {
				c.Asked = &Asked{ID: "a7", Request: bashGo}
			}),
			want: []Event{
				askedReqEv("s2", "a7", "t4", bashGo),
				decidedEv("s2", "a7", Decision{Verdict: Allow, Source: DecidedByRule, Rule: "r6"}),
			}},
		{name: "asked matches inputs rule", given: running(),
			in: inputs(func(i *Inputs) { i.Rules = []Rule{wsRule} }),
			cmd: sessionCmd(FactAsked, "s2", OriginRuntime, func(c *Command) {
				c.Asked = &Asked{ID: "a6", Request: bashGo}
			}),
			want: []Event{
				askedReqEv("s2", "a6", "t4", bashGo),
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByRule, Rule: "w9"}),
			}},
		{name: "asked other workspace rule ignored", given: running(),
			in: inputs(func(i *Inputs) {
				w := wsRule
				w.ScopeID = "w2"
				i.Rules = []Rule{w}
			}),
			cmd: sessionCmd(FactAsked, "s2", OriginRuntime, func(c *Command) {
				c.Asked = &Asked{ID: "a6", Request: bashGo}
			}),
			want: []Event{askedReqEv("s2", "a6", "t4", bashGo)}},
		{name: "asked deny beats allow",
			given: and(running(), ruleEv(Rule{ID: "r6", Scope: ScopeSession, ScopeID: "s2",
				Match: MatchPrefix, Tool: "Bash", Pattern: "go test", Verdict: Allow})),
			in: inputs(func(i *Inputs) {
				i.Rules = []Rule{{ID: "g9", Scope: ScopeGlobal, Match: MatchPrefix,
					Tool: "Bash", Pattern: "go test", Verdict: Deny}}
			}),
			cmd: sessionCmd(FactAsked, "s2", OriginRuntime, func(c *Command) {
				c.Asked = &Asked{ID: "a7", Request: bashGo}
			}),
			want: []Event{
				askedReqEv("s2", "a7", "t4", bashGo),
				decidedEv("s2", "a7", Decision{Verdict: Deny, Source: DecidedByRule, Rule: "g9"}),
			}},
		{name: "asked compound not auto allowed",
			given: and(running(), ruleEv(taskPrefix)), in: gitInputs(),
			cmd: sessionCmd(FactAsked, "s2", OriginRuntime, func(c *Command) {
				c.Asked = &Asked{ID: "a7", Request: PermissionRequest{
					Tool: "Bash", Kind: ToolExecute, Command: "go test && rm -rf /"}}
			}),
			want: []Event{askedReqEv("s2", "a7", "t4", PermissionRequest{
				Tool: "Bash", Kind: ToolExecute, Command: "go test && rm -rf /"})}},
		{name: "asked empty id", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactAsked, "s2", OriginRuntime, func(c *Command) {
				c.Asked = &Asked{Request: read}
			}),
			reason: ReasonInvalid, detail: "permission id"},
		{name: "asked duplicate id",
			given: and(running(), askedEv("s2", "a6", "t4")), in: gitInputs(),
			cmd: sessionCmd(FactAsked, "s2", OriginRuntime, func(c *Command) {
				c.Asked = &Asked{ID: "a6", Request: read}
			}),
			reason: ReasonInvalid, detail: "duplicate"},
		{name: "asked without turn", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactAsked, "s2", OriginRuntime, func(c *Command) {
				c.Asked = &Asked{ID: "a3", Request: read}
			}),
			reason: ReasonInvalid, detail: "no turn"},

		// CmdDecidePermission（無 Always）。
		{name: "decide allow", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow}
			}),
			want: []Event{decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop})}},
		{name: "decide deny remote", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginRemote, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Deny}
			}),
			want: []Event{decidedEv("s2", "a6", Decision{Verdict: Deny, Source: DecidedByUser, Origin: OriginRemote})}},
		{name: "decide bad verdict", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: "maybe"}
			}),
			reason: ReasonInvalid, detail: "verdict"},
		{name: "decide already decided",
			given: and(askedBash, decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser})),
			in:    gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Deny}
			})},
		{name: "decide unknown ask", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a9", Verdict: Allow}
			})},
		{name: "decide closed",
			given: and(askedBash, closedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow}
			}),
			reason: ReasonSessionClosed},
		{name: "decide missing payload", given: askedBash, in: gitInputs(),
			cmd:    sessionCmd(CmdDecidePermission, "s2", OriginDesktop, nil),
			reason: ReasonInvalid},

		// Always。
		{name: "always session prefix", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchPrefix, Pattern: "  go test  "}}
			}),
			want: []Event{
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop}),
				ruleEv(Rule{ID: "r8", Scope: ScopeSession, ScopeID: "s2", Match: MatchPrefix,
					Tool: "Bash", Pattern: "go test", Verdict: Allow}),
			}},
		{name: "always task", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Deny,
					Always: &Rule{Scope: ScopeTask, Match: MatchPrefix, Pattern: "go test"}}
			}),
			want: []Event{
				decidedEv("s2", "a6", Decision{Verdict: Deny, Source: DecidedByUser, Origin: OriginDesktop}),
				ruleEv(Rule{ID: "r8", Scope: ScopeTask, ScopeID: "t-1", Match: MatchPrefix,
					Tool: "Bash", Pattern: "go test", Verdict: Deny}),
			}},
		{name: "always workspace", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeWorkspace, Match: MatchPrefix, Pattern: "go test"}}
			}),
			want: []Event{
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop}),
				ruleEv(Rule{ID: "t-1:r8", Scope: ScopeWorkspace, ScopeID: "w1", Match: MatchPrefix,
					Tool: "Bash", Pattern: "go test", Verdict: Allow}),
			}},
		{name: "always global", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeGlobal, Match: MatchPrefix, Pattern: "go test"}}
			}),
			want: []Event{
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop}),
				ruleEv(Rule{ID: "t-1:r8", Scope: ScopeGlobal, Match: MatchPrefix,
					Tool: "Bash", Pattern: "go test", Verdict: Allow}),
			}},
		{name: "always tool match on edit",
			given: and(running(), askedReqEv("s2", "a6", "t4", edit)), in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchTool}}
			}),
			want: []Event{
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop}),
				ruleEv(Rule{ID: "r8", Scope: ScopeSession, ScopeID: "s2", Match: MatchTool,
					Tool: "Edit", Verdict: Allow}),
			}},
		{name: "always tool match on bash", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchTool}}
			}),
			reason: ReasonInvalid, detail: "prefix or exact"},
		{name: "always tool differs", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchPrefix, Tool: "Edit", Pattern: "go test"}}
			}),
			reason: ReasonInvalid, detail: "tool differs"},
		{name: "always not covering", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchPrefix, Pattern: "make build"}}
			}),
			reason: ReasonInvalid, detail: "cover"},
		{name: "always empty pattern", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchPrefix, Pattern: "   "}}
			}),
			reason: ReasonInvalid, detail: "pattern"},
		{name: "always bad scope", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: "bogus", Match: MatchPrefix, Pattern: "go test"}}
			}),
			reason: ReasonInvalid, detail: "scope"},
		{name: "always bad match", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: "bogus", Pattern: "go test"}}
			}),
			reason: ReasonInvalid, detail: "match"},
		{name: "always duplicate state rule",
			given: and(askedBash, ruleEv(Rule{ID: "r7", Scope: ScopeSession, ScopeID: "s2",
				Match: MatchPrefix, Tool: "Bash", Pattern: "go test", Verdict: Allow})),
			in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchPrefix, Pattern: "go test"}}
			}),
			want: []Event{decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop})}},
		{name: "always duplicate inputs rule", given: askedBash,
			in: inputs(func(i *Inputs) { i.Rules = []Rule{wsRule} }),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeWorkspace, Match: MatchPrefix, Pattern: "go test"}}
			}),
			want: []Event{decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop})}},
		{name: "always remote asks confirmation", given: askedBash, in: gitInputs(),
			cmd: func() Command {
				c := sessionCmd(CmdDecidePermission, "s2", OriginRemote, func(c *Command) {
					c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
						Always: &Rule{ID: "zzz", Scope: ScopeSession, ScopeID: "zzz",
							Match: MatchPrefix, Pattern: " go test "}}
				})
				c.Device = "d1"
				return c
			}(),
			want: []Event{
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginRemote}),
				{Kind: ConfirmationRequestedKind, ConfirmationRequested: &Confirmation{
					ID: "c8", RequestedBy: "d1", RequestedAt: at0,
					// 存入的是校驗補齊後的規則：Tool 取自 ask，Pattern 已修剪，ID/ScopeID 清空。
					Command: Command{Kind: CmdDecidePermission, Origin: OriginRemote, Session: "s2", Device: "d1",
						DecidePermission: &DecidePermission{Permission: "a6", Verdict: Allow,
							Always: &Rule{Scope: ScopeSession, Match: MatchPrefix,
								Tool: "Bash", Pattern: "go test", Verdict: Allow}}},
				}},
			}},
		{name: "confirmed remote rule keeps filled tool",
			given: and(askedBash, cancelledPermEv("s2", "a6"), completedEv("s2")), // v8，ask 已隨回合結束取消
			in:    gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchPrefix,
						Tool: "Bash", Pattern: "go test", Verdict: Allow}}
			}),
			want: []Event{ruleEv(Rule{ID: "r9", Scope: ScopeSession, ScopeID: "s2",
				Match: MatchPrefix, Tool: "Bash", Pattern: "go test", Verdict: Allow})}},
		{name: "always remote needs pending ask", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginRemote, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a9", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchPrefix, Tool: "Bash", Pattern: "go test"}}
			}),
			reason: ReasonInvalid, detail: "pending ask"},
		{name: "always desktop unknown ask", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a9", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchPrefix, Tool: "Bash", Pattern: "go test"}}
			}),
			want: []Event{ruleEv(Rule{ID: "r7", Scope: ScopeSession, ScopeID: "s2",
				Match: MatchPrefix, Tool: "Bash", Pattern: "go test", Verdict: Allow})}},
		{name: "always on decided ask adds rule only",
			given: and(askedBash, decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser})),
			in:    gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{Scope: ScopeSession, Match: MatchPrefix, Pattern: "go test"}}
			}),
			want: []Event{ruleEv(Rule{ID: "r8", Scope: ScopeSession, ScopeID: "s2",
				Match: MatchPrefix, Tool: "Bash", Pattern: "go test", Verdict: Allow})}},
		{name: "always ignores client ids", given: askedBash, in: gitInputs(),
			cmd: sessionCmd(CmdDecidePermission, "s2", OriginDesktop, func(c *Command) {
				c.DecidePermission = &DecidePermission{Permission: "a6", Verdict: Allow,
					Always: &Rule{ID: "zzz", Scope: ScopeSession, ScopeID: "zzz",
						Match: MatchPrefix, Tool: "Bash", Pattern: "go test"}}
			}),
			want: []Event{
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop}),
				ruleEv(Rule{ID: "r8", Scope: ScopeSession, ScopeID: "s2",
					Match: MatchPrefix, Tool: "Bash", Pattern: "go test", Verdict: Allow}),
			}},

		// FactAskExpired。
		{name: "expired applies timeout verdict",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchWithTimeout(time.Minute, Allow)),
				queuedEv("s2", "p3", "Build it"), turnEv("s2", "t4", "p3"), askedEv("s2", "a5", "t4")),
			in: inputs(func(i *Inputs) { i.Now = at0.Add(time.Minute) }),
			cmd: sessionCmd(FactAskExpired, "s2", OriginRuntime, func(c *Command) {
				c.AskExpired = &PermissionRef{Permission: "a5"}
			}),
			want: []Event{decidedEv("s2", "a5", Decision{Verdict: Allow, Source: DecidedByTimeout})}},
		{name: "expired denies by default",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchWithTimeout(time.Minute, "")),
				queuedEv("s2", "p3", "Build it"), turnEv("s2", "t4", "p3"), askedEv("s2", "a5", "t4")),
			in: inputs(func(i *Inputs) { i.Now = at0.Add(time.Minute) }),
			cmd: sessionCmd(FactAskExpired, "s2", OriginRuntime, func(c *Command) {
				c.AskExpired = &PermissionRef{Permission: "a5"}
			}),
			want: []Event{decidedEv("s2", "a5", Decision{Verdict: Deny, Source: DecidedByTimeout})}},
		{name: "expired fired early",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchWithTimeout(time.Minute, Allow)),
				queuedEv("s2", "p3", "Build it"), turnEv("s2", "t4", "p3"), askedEv("s2", "a5", "t4")),
			in: gitInputs(), // Now = at0，早於期限
			cmd: sessionCmd(FactAskExpired, "s2", OriginRuntime, func(c *Command) {
				c.AskExpired = &PermissionRef{Permission: "a5"}
			}),
			reason: ReasonInvalid, detail: "early"},
		{name: "expired without timeout",
			given: and(running(), askedEv("s2", "a6", "t4")), in: gitInputs(),
			cmd: sessionCmd(FactAskExpired, "s2", OriginRuntime, func(c *Command) {
				c.AskExpired = &PermissionRef{Permission: "a6"}
			}),
			reason: ReasonInvalid, detail: "no timeout"},
		{name: "expired already decided",
			given: and(running(), askedEv("s2", "a6", "t4"),
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser})), in: gitInputs(),
			cmd: sessionCmd(FactAskExpired, "s2", OriginRuntime, func(c *Command) {
				c.AskExpired = &PermissionRef{Permission: "a6"}
			})},
		{name: "expired unknown", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactAskExpired, "s2", OriginRuntime, func(c *Command) {
				c.AskExpired = &PermissionRef{Permission: "a9"}
			})},

		// FactAnswered。
		{name: "answered",
			given: and(running(), askedEv("s2", "a6", "t4"),
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser})), in: gitInputs(),
			cmd: sessionCmd(FactAnswered, "s2", OriginRuntime, func(c *Command) {
				c.Answered = &PermissionRef{Permission: "a6"}
			}),
			want: []Event{answeredEv("s2", "a6")}},
		{name: "answered before decided",
			given: and(running(), askedEv("s2", "a6", "t4")), in: gitInputs(),
			cmd: sessionCmd(FactAnswered, "s2", OriginRuntime, func(c *Command) {
				c.Answered = &PermissionRef{Permission: "a6"}
			}),
			reason: ReasonInvalid, detail: "before decided"},
		{name: "answered unknown", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactAnswered, "s2", OriginRuntime, func(c *Command) {
				c.Answered = &PermissionRef{Permission: "a9"}
			})},
	})
}

func TestDecideConfirmations(t *testing.T) {
	bashGo := PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test ./..."}
	// 與 remote Always 測試相同的存入命令（已校驗補齊的規則）。
	stored := Command{Kind: CmdDecidePermission, Origin: OriginRemote, Session: "s2", Device: "d1",
		DecidePermission: &DecidePermission{Permission: "a6", Verdict: Allow,
			Always: &Rule{Scope: ScopeSession, Match: MatchPrefix, Tool: "Bash", Pattern: "go test", Verdict: Allow}}}
	// c8 等待核准的串流：a6 已被 remote 的一次性決定處理過。
	conf := seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
		turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
		askedReqEv("s2", "a6", "t4", bashGo),
		decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginRemote}),
		confReqEv("c8", "d1", stored)) // v8
	confirmCmd := func(id ConfirmationID) Command {
		return Command{Kind: CmdConfirm, Origin: OriginDesktop, Confirm: &ConfirmationRef{Confirmation: id}}
	}
	cancelCmd := func(id ConfirmationID, origin Origin) Command {
		return Command{Kind: CmdCancelConfirmation, Origin: origin, CancelConfirmation: &ConfirmationRef{Confirmation: id}}
	}
	runDecide(t, []decideCase{
		// Confirm。
		{name: "confirm remote refused", given: conf, in: gitInputs(),
			cmd:    sessionCmd(CmdConfirm, "", OriginRemote, func(c *Command) { c.Confirm = &ConfirmationRef{Confirmation: "c8"} }),
			reason: ReasonNeedsDesktop},
		{name: "confirm missing payload", given: conf, in: gitInputs(),
			cmd:    Command{Kind: CmdConfirm, Origin: OriginDesktop},
			reason: ReasonInvalid},
		{name: "confirm unknown id", given: conf, in: gitInputs(), cmd: confirmCmd("c9")},
		{name: "confirm grants and reruns", given: conf, in: gitInputs(),
			cmd: confirmCmd("c8"),
			want: []Event{
				grantedEv("c8"),
				ruleEv(Rule{ID: "r10", Scope: ScopeSession, ScopeID: "s2", Match: MatchPrefix,
					Tool: "Bash", Pattern: "go test", Verdict: Allow}),
			}},
		{name: "confirm after ask gone",
			given: and(conf, cancelledPermEv("s2", "a6"), completedEv("s2")), // v10
			in:    gitInputs(),
			cmd:   confirmCmd("c8"),
			want: []Event{
				grantedEv("c8"),
				ruleEv(Rule{ID: "r12", Scope: ScopeSession, ScopeID: "s2", Match: MatchPrefix,
					Tool: "Bash", Pattern: "go test", Verdict: Allow}),
			}},
		{name: "confirm stored session closed",
			given: and(conf, closedEv("s2")), in: gitInputs(),
			cmd:    confirmCmd("c8"),
			reason: ReasonSessionClosed},
		{name: "confirm rule already exists",
			given: and(conf, ruleEv(Rule{ID: "r9", Scope: ScopeSession, ScopeID: "s2",
				Match: MatchPrefix, Tool: "Bash", Pattern: "go test", Verdict: Allow})),
			in:   gitInputs(),
			cmd:  confirmCmd("c8"),
			want: []Event{grantedEv("c8")}},
		{name: "confirm on archived task",
			given: and(conf, Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd:    confirmCmd("c8"),
			reason: ReasonArchived},
		{name: "confirm stored fact kind",
			given: seqs(created(IsolationNone), sessionEv("s2"),
				confReqEv("c3", "d1", Command{Kind: FactOutput, Origin: OriginRemote, Session: "s2"})),
			in:     gitInputs(),
			cmd:    confirmCmd("c3"),
			reason: ReasonInvalid},
		{name: "confirm stored confirm kind",
			given: seqs(created(IsolationNone), sessionEv("s2"),
				confReqEv("c3", "d1", Command{Kind: CmdConfirm, Origin: OriginRemote})),
			in:     gitInputs(),
			cmd:    confirmCmd("c3"),
			reason: ReasonInvalid},

		// CancelConfirmation。
		{name: "cancel confirmation", given: conf, in: gitInputs(),
			cmd:  cancelCmd("c8", OriginDesktop),
			want: []Event{cancelledConfEv("c8")},
			check: func(t *testing.T, s State) {
				if len(s.Confirmations) != 0 {
					t.Errorf("confirmations %v remain", s.Confirmations)
				}
			}},
		{name: "cancel confirmation remote", given: conf, in: gitInputs(),
			cmd:  cancelCmd("c8", OriginRemote),
			want: []Event{cancelledConfEv("c8")}},
		{name: "cancel unknown", given: conf, in: gitInputs(), cmd: cancelCmd("c9", OriginDesktop)},
		{name: "cancel missing payload", given: conf, in: gitInputs(),
			cmd:    Command{Kind: CmdCancelConfirmation, Origin: OriginDesktop},
			reason: ReasonInvalid},
	})
}

func TestDecideEffects(t *testing.T) {
	prob := Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}
	failCmd := func(sid SessionID, key EffectKey) Command {
		return sessionCmd(FactEffectFailed, sid, OriginRuntime, func(c *Command) {
			c.EffectFailed = &EffectFailure{Effect: key, Problem: prob}
		})
	}
	retryCmd := func(key EffectKey, origin Origin) Command {
		return Command{Kind: CmdRetryEffect, Origin: origin, RetryEffect: &RetryEffect{Effect: key}}
	}
	runDecide(t, []decideCase{
		// FactEffectFailed。
		{name: "effect failed missing payload", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    sessionCmd(FactEffectFailed, "s2", OriginRuntime, nil),
			reason: ReasonInvalid},
		{name: "effect failed empty key", given: running(), in: gitInputs(),
			cmd: failCmd("s2", ""), reason: ReasonInvalid, detail: "cannot fail"},
		{name: "effect failed unknown kind", given: running(), in: gitInputs(),
			cmd: failCmd("s2", "bogus:x"), reason: ReasonInvalid, detail: "cannot fail"},
		{name: "close conn cannot fail", given: running(), in: gitInputs(),
			cmd: failCmd("s2", "close_conn:s2"), reason: ReasonInvalid, detail: "cannot fail"},
		{name: "report exited cannot fail", given: running(), in: gitInputs(),
			cmd: failCmd("s2", "report_exited:t4"), reason: ReasonInvalid, detail: "cannot fail"},
		{name: "schedule timer cannot fail", given: running(), in: gitInputs(),
			cmd: failCmd("s2", "schedule_timer:s2/a6"), reason: ReasonInvalid, detail: "cannot fail"},
		{name: "conn failure fails turn",
			given: and(running(), askedEv("s2", "a6", "t4")), in: gitInputs(),
			cmd: failCmd("s2", "start_conn:s2"),
			want: []Event{
				cancelledPermEv("s2", "a6"),
				{Kind: TurnFailedKind, Session: "s2",
					TurnFailed: &TurnFailed{Turn: "t4", Problem: prob}},
				{Kind: ProcessExitedKind, Session: "s2", ProcessExited: &Exited{Problem: prob}},
			},
			status: TaskInProgress},
		{name: "send prompt failure fails turn", given: running(), in: gitInputs(),
			cmd: failCmd("s2", "send_prompt:t4"),
			want: []Event{
				{Kind: TurnFailedKind, Session: "s2",
					TurnFailed: &TurnFailed{Turn: "t4", Problem: prob}},
				{Kind: ProcessExitedKind, Session: "s2", ProcessExited: &Exited{Problem: prob}},
			},
			status: TaskInProgress},
		{name: "conn failure already exited",
			given: and(running(), completedEv("s2"), exitedEv("s2")), in: gitInputs(),
			cmd: failCmd("s2", "start_conn:s2")},
		{name: "conn failure closed",
			given: seqs(created(IsolationNone), sessionEv("s2"), closedEv("s2")), in: gitInputs(),
			cmd:    failCmd("s2", "start_conn:s2"),
			reason: ReasonSessionClosed},
		{name: "conn failure unknown session", given: running(), in: gitInputs(),
			cmd:    failCmd("", "start_conn:s2"),
			reason: ReasonUnknownSession},
		{name: "create worktree suspends",
			given: seqs(created(IsolationWorktree), attached()), in: gitInputs(),
			cmd:  failCmd("", "create_worktree:task"),
			want: []Event{effectFailedEv("create_worktree:task", prob)}},
		{name: "merge worktree suspends", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:  failCmd("", "merge_worktree:i21"),
			want: []Event{effectFailedEv("merge_worktree:i21", prob)}},
		{name: "prepare review suspends", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:  failCmd("", "prepare_review"),
			want: []Event{effectFailedEv("prepare_review", prob)}},
		{name: "effect failed duplicate",
			given: seqs(created(IsolationNone), effectFailedEv("create_worktree:task", prob)),
			in:    gitInputs(),
			cmd:   failCmd("", "create_worktree:task")},
		{name: "effect failed from desktop", given: running(), in: gitInputs(),
			cmd: sessionCmd(FactEffectFailed, "s2", OriginDesktop, func(c *Command) {
				c.EffectFailed = &EffectFailure{Effect: "start_conn:s2", Problem: prob}
			}),
			reason: ReasonForgedOrigin},
		{name: "suspend on archived task",
			given: seqs(created(IsolationWorktree), attached(), Event{Kind: TaskArchivedKind}),
			in:    gitInputs(),
			cmd:   failCmd("", "create_worktree:task"),
			want:  []Event{effectFailedEv("create_worktree:task", prob)}},

		// CmdRetryEffect。
		{name: "retry missing payload", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    Command{Kind: CmdRetryEffect, Origin: OriginDesktop},
			reason: ReasonInvalid},
		{name: "retry not failed", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: retryCmd("create_worktree:task", OriginDesktop)},
		{name: "retry desktop",
			given: seqs(created(IsolationNone), effectFailedEv("create_worktree:task", prob)),
			in:    gitInputs(),
			cmd:   retryCmd("create_worktree:task", OriginDesktop),
			want:  []Event{retriedEv("create_worktree:task")},
			check: func(t *testing.T, s State) {
				if len(s.Failed) != 0 {
					t.Errorf("failed effects %v remain", s.Failed)
				}
			}},
		{name: "retry remote",
			given: seqs(created(IsolationNone), effectFailedEv("create_worktree:task", prob)),
			in:    gitInputs(),
			cmd:   retryCmd("create_worktree:task", OriginRemote),
			want:  []Event{retriedEv("create_worktree:task")}},
		{name: "fail again after retry",
			given: seqs(created(IsolationNone), effectFailedEv("create_worktree:task", prob),
				retriedEv("create_worktree:task")),
			in:   gitInputs(),
			cmd:  failCmd("", "create_worktree:task"),
			want: []Event{effectFailedEv("create_worktree:task", prob)}},
	})
}

func TestEffectKey(t *testing.T) {
	if got := newEffectKey(EffPrepareReview, ""); got != "prepare_review" {
		t.Errorf("newEffectKey(prepare_review, \"\") = %q", got)
	}
	if got := newEffectKey(EffMergeWorktree, "i21"); got != "merge_worktree:i21" {
		t.Errorf("newEffectKey(merge_worktree, i21) = %q", got)
	}
	for _, tc := range []struct {
		kind IntentKind
		want EffectKey
	}{
		{IntentMerge, "merge_worktree:i21"},
		{IntentDiscard, "discard_worktree:i21"},
		{IntentDeleteNative, "delete_native:i21"},
	} {
		got := Intent{ID: "i21", Kind: tc.kind}.effectKey()
		if got != tc.want || got.Kind() != tc.want.Kind() {
			t.Errorf("Intent{Kind: %s}.effectKey() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestEffectKeyKind(t *testing.T) {
	for _, tc := range []struct {
		key  EffectKey
		want EffectKind
	}{
		{"start_conn:s2", EffStartConn},
		{"merge_worktree:i21", EffMergeWorktree},
		{"prepare_review", EffPrepareReview},
		{"", ""},
	} {
		if got := tc.key.Kind(); got != tc.want {
			t.Errorf("%q.Kind() = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// forked 是一個 Session（s2）帶著開著的空閒 Fork 的串流。
func forked() []Event {
	return seqs(created(IsolationNone), sessionEv("s2"), forkEv("s2", "main"))
}

// worktreeTask 是有 Task Worktree 的串流。
func worktreeTask() []Event {
	return seqs(created(IsolationWorktree), attached())
}

func TestDecideMerge(t *testing.T) {
	worktree := Target{Level: TargetWorktree}
	forkS2 := Target{Level: TargetFork, Session: "s2"}
	forkS3 := Target{Level: TargetFork, Session: "s3"}
	mergeCmd := func(t Target, strategy MergeStrategy, msg string) Command {
		return Command{Kind: CmdMerge, Origin: OriginDesktop,
			Merge: &Merge{Target: t, Strategy: strategy, Message: msg}}
	}
	discardCmd := func(t Target) Command {
		return Command{Kind: CmdDiscard, Origin: OriginDesktop, Discard: &Discard{Target: t}}
	}
	remoteMerge := Command{Kind: CmdMerge, Origin: OriginRemote, Device: "d9",
		Merge: &Merge{Target: forkS2}}
	remoteDiscard := Command{Kind: CmdDiscard, Origin: OriginRemote, Device: "d9",
		Discard: &Discard{Target: forkS2}}
	wantConf := func(id ConfirmationID, cmd Command) Event {
		return Event{Kind: ConfirmationRequestedKind, ConfirmationRequested: &Confirmation{
			ID: id, Command: cmd, RequestedBy: "d9", RequestedAt: at0}}
	}
	runDecide(t, []decideCase{
		// Merge（desktop）。
		{name: "merge fork", given: forked(), in: gitInputs(),
			cmd:  mergeCmd(forkS2, MergePullRequest, "msg"),
			want: []Event{mergeEv("i4", forkS2, MergePullRequest, "msg")}},
		{name: "merge defaults", given: forked(), in: gitInputs(),
			cmd:  mergeCmd(forkS2, "", ""),
			want: []Event{mergeEv("i4", forkS2, MergeSquash, "Login page\n\nBuild it")}},
		{name: "merge bad strategy", given: forked(), in: gitInputs(),
			cmd:    mergeCmd(forkS2, "rebase", ""),
			reason: ReasonInvalid, detail: "strategy"},
		{name: "merge running session",
			given:  and(forked(), queuedEv("s2", "p4", "x"), turnEv("s2", "t5", "p4")),
			in:     gitInputs(),
			cmd:    mergeCmd(forkS2, "", ""),
			reason: ReasonSessionRunning},
		{name: "merge exited session",
			given: and(forked(), exitedEv("s2")), in: gitInputs(),
			cmd:  mergeCmd(forkS2, "", ""),
			want: []Event{mergeEv("i5", forkS2, MergeSquash, "Login page\n\nBuild it")}},
		{name: "merge already merging",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m")), in: gitInputs(),
			cmd: mergeCmd(forkS2, "", "")},
		{name: "merge has discard intent",
			given: and(forked(), discardEv("i4", forkS2)), in: gitInputs(),
			cmd:    mergeCmd(forkS2, "", ""),
			reason: ReasonInvalid, detail: "cancel it first"},
		{name: "merge already resolved",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m"),
				Event{Kind: ForkMergedKind, Session: "s2", Merged: &IntentResult{Intent: "i4"}}),
			in:     gitInputs(),
			cmd:    mergeCmd(forkS2, "", ""),
			reason: ReasonInvalid, detail: "already resolved"},
		{name: "merge unknown session", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    mergeCmd(Target{Level: TargetFork, Session: "s9"}, "", ""),
			reason: ReasonInvalid, detail: "no worktree"},
		{name: "merge session without fork", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd:    mergeCmd(forkS2, "", ""),
			reason: ReasonInvalid, detail: "no worktree"},
		{name: "merge worktree", given: worktreeTask(), in: gitInputs(),
			cmd:  mergeCmd(worktree, "", ""),
			want: []Event{mergeEv("i3", worktree, MergeSquash, "Login page\n\nBuild it")}},
		{name: "merge worktree with running session",
			given:  and(worktreeTask(), sessionEv("s3"), queuedEv("s3", "p4", "x"), turnEv("s3", "t5", "p4")),
			in:     gitInputs(),
			cmd:    mergeCmd(worktree, "", ""),
			reason: ReasonSessionRunning},
		{name: "merge worktree with open fork",
			given:  and(worktreeTask(), sessionEv("s3"), forkEv("s3", "orielhub/t-1")),
			in:     gitInputs(),
			cmd:    mergeCmd(worktree, "", ""),
			reason: ReasonUnresolvedChanges},
		{name: "merge worktree with fork intent",
			given: and(worktreeTask(), sessionEv("s3"), forkEv("s3", "orielhub/t-1"),
				mergeEv("i5", forkS3, MergeSquash, "m")),
			in:   gitInputs(),
			cmd:  mergeCmd(worktree, "", ""),
			want: []Event{mergeEv("i6", worktree, MergeSquash, "Login page\n\nBuild it")}},
		{name: "merge without worktree", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    mergeCmd(worktree, "", ""),
			reason: ReasonInvalid, detail: "no worktree"},
		{name: "merge missing payload", given: forked(), in: gitInputs(),
			cmd:    Command{Kind: CmdMerge, Origin: OriginDesktop},
			reason: ReasonInvalid},
		{name: "merge unknown target level", given: forked(), in: gitInputs(),
			cmd:    mergeCmd(Target{Level: "bogus"}, "", ""),
			reason: ReasonInvalid, detail: "unknown target"},

		// Merge（remote，D5/D20 要 desktop 核准）。
		{name: "merge remote asks confirmation", given: forked(), in: gitInputs(),
			cmd:  remoteMerge,
			want: []Event{wantConf("c4", remoteMerge)}},
		{name: "merge remote running session",
			given:  and(forked(), queuedEv("s2", "p4", "x"), turnEv("s2", "t5", "p4")),
			in:     gitInputs(),
			cmd:    remoteMerge,
			reason: ReasonSessionRunning},
		{name: "merge remote already pending",
			given: and(forked(), confReqEv("c4", "d9", remoteMerge)), in: gitInputs(),
			cmd: remoteMerge},
		{name: "discard remote already pending",
			given: and(forked(), confReqEv("c4", "d9", remoteDiscard)), in: gitInputs(),
			cmd: remoteDiscard},
		{name: "confirm runs stored merge",
			given: and(forked(), confReqEv("c4", "d9", remoteMerge)), in: gitInputs(),
			cmd: Command{Kind: CmdConfirm, Origin: OriginDesktop, Confirm: &ConfirmationRef{Confirmation: "c4"}},
			want: []Event{
				grantedEv("c4"),
				mergeEv("i6", forkS2, MergeSquash, "Login page\n\nBuild it"),
			}},

		// Discard。
		{name: "discard fork", given: forked(), in: gitInputs(),
			cmd:  discardCmd(forkS2),
			want: []Event{discardEv("i4", forkS2)}},
		{name: "discard running session",
			given:  and(forked(), queuedEv("s2", "p4", "x"), turnEv("s2", "t5", "p4")),
			in:     gitInputs(),
			cmd:    discardCmd(forkS2),
			reason: ReasonSessionRunning},
		{name: "discard has merge intent",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m")), in: gitInputs(),
			cmd:    discardCmd(forkS2),
			reason: ReasonInvalid, detail: "cancel it first"},
		{name: "discard already discarding",
			given: and(forked(), discardEv("i4", forkS2)), in: gitInputs(),
			cmd: discardCmd(forkS2)},
		{name: "discard worktree with open fork",
			given:  and(worktreeTask(), sessionEv("s3"), forkEv("s3", "orielhub/t-1")),
			in:     gitInputs(),
			cmd:    discardCmd(worktree),
			reason: ReasonUnresolvedChanges},
		{name: "discard remote asks confirmation", given: forked(), in: gitInputs(),
			cmd:  remoteDiscard,
			want: []Event{wantConf("c4", remoteDiscard)}},
		{name: "discard missing payload", given: forked(), in: gitInputs(),
			cmd:    Command{Kind: CmdDiscard, Origin: OriginDesktop},
			reason: ReasonInvalid},
	})
}

func TestDecideMergeFacts(t *testing.T) {
	worktree := Target{Level: TargetWorktree}
	forkS2 := Target{Level: TargetFork, Session: "s2"}
	conflictProb := Problem{Code: CodeMergeConflict, Params: map[string]string{"files": "a\nb"}}
	runDecide(t, []decideCase{
		// merged。
		{name: "fork merged closes session",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m")), in: gitInputs(),
			cmd: sessionCmd(FactMerged, "", OriginRuntime, func(c *Command) {
				c.Merged = &IntentResult{Intent: "i4", URL: "u"}
			}),
			want: []Event{
				{Kind: ForkMergedKind, Session: "s2", Merged: &IntentResult{Intent: "i4", URL: "u"}},
				closedEv("s2"),
			}},
		{name: "worktree merged closes all and done",
			given: and(worktreeTask(), sessionEv("s3"), sessionEv("s4"),
				mergeEv("i5", worktree, MergeSquash, "m")),
			in: gitInputs(),
			cmd: sessionCmd(FactMerged, "", OriginRuntime, func(c *Command) {
				c.Merged = &IntentResult{Intent: "i5"}
			}),
			want: []Event{
				{Kind: WorktreeMergedKind, Merged: &IntentResult{Intent: "i5"}},
				closedEv("s3"), closedEv("s4"),
				statusEv(TaskTodo, TaskDone, CauseAuto),
			},
			status: TaskDone},
		{name: "worktree merged already done",
			given: and(worktreeTask(), sessionEv("s3"), sessionEv("s4"),
				mergeEv("i5", worktree, MergeSquash, "m"), statusEv(TaskTodo, TaskDone, CauseUser)),
			in: gitInputs(),
			cmd: sessionCmd(FactMerged, "", OriginRuntime, func(c *Command) {
				c.Merged = &IntentResult{Intent: "i5"}
			}),
			want: []Event{
				{Kind: WorktreeMergedKind, Merged: &IntentResult{Intent: "i5"}},
				closedEv("s3"), closedEv("s4"),
			},
			status: TaskDone},
		{name: "merged unknown intent", given: forked(), in: gitInputs(),
			cmd: sessionCmd(FactMerged, "", OriginRuntime, func(c *Command) {
				c.Merged = &IntentResult{Intent: "i9"}
			})},
		{name: "merged wrong intent kind",
			given: and(forked(), discardEv("i4", forkS2)), in: gitInputs(),
			cmd: sessionCmd(FactMerged, "", OriginRuntime, func(c *Command) {
				c.Merged = &IntentResult{Intent: "i4"}
			}),
			reason: ReasonInvalid, detail: "is a discard"},
		{name: "merged suspended",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m"),
				effectFailedEv("merge_worktree:i4", Problem{Code: "proc.exited"})), in: gitInputs(),
			cmd: sessionCmd(FactMerged, "", OriginRuntime, func(c *Command) {
				c.Merged = &IntentResult{Intent: "i4"}
			}),
			reason: ReasonInvalid, detail: "suspended"},
		{name: "fork merged session closed",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m"), closedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactMerged, "", OriginRuntime, func(c *Command) {
				c.Merged = &IntentResult{Intent: "i4"}
			}),
			want: []Event{{Kind: ForkMergedKind, Session: "s2", Merged: &IntentResult{Intent: "i4"}}}},
		{name: "merged missing payload", given: forked(), in: gitInputs(),
			cmd:    sessionCmd(FactMerged, "", OriginRuntime, nil),
			reason: ReasonInvalid},
		{name: "merged fork session running",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m"),
				queuedEv("s2", "p5", "x"), turnEv("s2", "t6", "p5")), in: gitInputs(),
			cmd: sessionCmd(FactMerged, "", OriginRuntime, func(c *Command) {
				c.Merged = &IntentResult{Intent: "i4"}
			}),
			reason: ReasonInvalid, detail: "still running"},
		{name: "merged from desktop",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m")), in: gitInputs(),
			cmd: sessionCmd(FactMerged, "", OriginDesktop, func(c *Command) {
				c.Merged = &IntentResult{Intent: "i4"}
			}),
			reason: ReasonForgedOrigin},

		// merge_conflict。
		{name: "merge conflict suspends",
			given: and(worktreeTask(), mergeEv("i3", worktree, MergeSquash, "m")), in: gitInputs(),
			cmd: sessionCmd(FactMergeConflict, "", OriginRuntime, func(c *Command) {
				c.MergeConflict = &MergeConflict{Intent: "i3", Files: []string{"a", "b"}}
			}),
			want: []Event{
				conflictEv("i3", "a", "b"),
				effectFailedEv("merge_worktree:i3", conflictProb),
			}},
		{name: "merge conflict empty files",
			given: and(worktreeTask(), mergeEv("i3", worktree, MergeSquash, "m")), in: gitInputs(),
			cmd: sessionCmd(FactMergeConflict, "", OriginRuntime, func(c *Command) {
				c.MergeConflict = &MergeConflict{Intent: "i3"}
			}),
			reason: ReasonInvalid, detail: "files"},
		{name: "merge conflict unknown intent", given: worktreeTask(), in: gitInputs(),
			cmd: sessionCmd(FactMergeConflict, "", OriginRuntime, func(c *Command) {
				c.MergeConflict = &MergeConflict{Intent: "i9", Files: []string{"a"}}
			})},
		{name: "merge conflict discard intent",
			given: and(forked(), discardEv("i4", forkS2)), in: gitInputs(),
			cmd: sessionCmd(FactMergeConflict, "", OriginRuntime, func(c *Command) {
				c.MergeConflict = &MergeConflict{Intent: "i4", Files: []string{"a"}}
			}),
			reason: ReasonInvalid, detail: "is a discard"},
		{name: "merge conflict already suspended",
			given: and(worktreeTask(), mergeEv("i3", worktree, MergeSquash, "m"),
				effectFailedEv("merge_worktree:i3", conflictProb)), in: gitInputs(),
			cmd: sessionCmd(FactMergeConflict, "", OriginRuntime, func(c *Command) {
				c.MergeConflict = &MergeConflict{Intent: "i3", Files: []string{"c"}}
			})},
		{name: "retry suspended merge",
			given: and(worktreeTask(), mergeEv("i3", worktree, MergeSquash, "m"),
				conflictEv("i3", "a", "b"), effectFailedEv("merge_worktree:i3", conflictProb)),
			in: gitInputs(),
			cmd: Command{Kind: CmdRetryEffect, Origin: OriginDesktop,
				RetryEffect: &RetryEffect{Effect: "merge_worktree:i3"}},
			want: []Event{retriedEv("merge_worktree:i3")},
			check: func(t *testing.T, s State) {
				if len(s.Failed) != 0 {
					t.Errorf("failed effects %v remain", s.Failed)
				}
				if !reflect.DeepEqual(s.Intents["i3"].Conflicts, []string{"a", "b"}) {
					t.Errorf("conflicts %v", s.Intents["i3"].Conflicts)
				}
			}},

		// discarded。
		{name: "fork discarded closes session",
			given: and(forked(), discardEv("i4", forkS2)), in: gitInputs(),
			cmd: sessionCmd(FactDiscarded, "", OriginRuntime, func(c *Command) {
				c.Discarded = &IntentResult{Intent: "i4"}
			}),
			want: []Event{
				{Kind: ForkDiscardedKind, Session: "s2", Discarded: &IntentRef{Intent: "i4"}},
				closedEv("s2"),
			}},
		{name: "worktree discarded closes all and done",
			given: and(worktreeTask(), sessionEv("s3"), sessionEv("s4"), discardEv("i5", worktree)),
			in:    gitInputs(),
			cmd: sessionCmd(FactDiscarded, "", OriginRuntime, func(c *Command) {
				c.Discarded = &IntentResult{Intent: "i5"}
			}),
			want: []Event{
				{Kind: WorktreeDiscardedKind, Discarded: &IntentRef{Intent: "i5"}},
				closedEv("s3"), closedEv("s4"),
				statusEv(TaskTodo, TaskDone, CauseAuto),
			},
			status: TaskDone},
		{name: "discarded unknown intent", given: forked(), in: gitInputs(),
			cmd: sessionCmd(FactDiscarded, "", OriginRuntime, func(c *Command) {
				c.Discarded = &IntentResult{Intent: "i9"}
			})},
		{name: "discarded merge intent",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m")), in: gitInputs(),
			cmd: sessionCmd(FactDiscarded, "", OriginRuntime, func(c *Command) {
				c.Discarded = &IntentResult{Intent: "i4"}
			}),
			reason: ReasonInvalid, detail: "is a merge"},
		{name: "discarded suspended",
			given: and(forked(), discardEv("i4", forkS2),
				effectFailedEv("discard_worktree:i4", conflictProb)), in: gitInputs(),
			cmd: sessionCmd(FactDiscarded, "", OriginRuntime, func(c *Command) {
				c.Discarded = &IntentResult{Intent: "i4"}
			}),
			reason: ReasonInvalid, detail: "suspended"},
		{name: "discarded fork session running",
			given: and(forked(), discardEv("i4", forkS2),
				queuedEv("s2", "p5", "x"), turnEv("s2", "t6", "p5")), in: gitInputs(),
			cmd: sessionCmd(FactDiscarded, "", OriginRuntime, func(c *Command) {
				c.Discarded = &IntentResult{Intent: "i4"}
			}),
			reason: ReasonInvalid, detail: "still running"},
		{name: "discarded worktree with running session",
			given: and(worktreeTask(), sessionEv("s3"), queuedEv("s3", "p4", "x"),
				turnEv("s3", "t5", "p4"), discardEv("i6", worktree)), in: gitInputs(),
			cmd: sessionCmd(FactDiscarded, "", OriginRuntime, func(c *Command) {
				c.Discarded = &IntentResult{Intent: "i6"}
			}),
			reason: ReasonInvalid, detail: "still running"},
		{name: "discarded missing payload", given: forked(), in: gitInputs(),
			cmd:    sessionCmd(FactDiscarded, "", OriginRuntime, nil),
			reason: ReasonInvalid},

		// CancelIntent。
		{name: "cancel suspended merge intent",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m"),
				effectFailedEv("merge_worktree:i4", conflictProb)), in: gitInputs(),
			cmd: Command{Kind: CmdCancelIntent, Origin: OriginDesktop,
				CancelIntent: &IntentRef{Intent: "i4"}},
			want: []Event{intentCancelledEv("i4")},
			check: func(t *testing.T, s State) {
				if len(s.Intents) != 0 || len(s.Failed) != 0 {
					t.Errorf("intents %v, failed %v", s.Intents, s.Failed)
				}
			}},
		{name: "cancel suspended intent remote",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m"),
				effectFailedEv("merge_worktree:i4", conflictProb)), in: gitInputs(),
			cmd: Command{Kind: CmdCancelIntent, Origin: OriginRemote,
				CancelIntent: &IntentRef{Intent: "i4"}},
			want: []Event{intentCancelledEv("i4")}},
		{name: "cancel in-flight intent",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m")), in: gitInputs(),
			cmd: Command{Kind: CmdCancelIntent, Origin: OriginDesktop,
				CancelIntent: &IntentRef{Intent: "i4"}},
			reason: ReasonInFlight},
		{name: "cancel unknown intent", given: forked(), in: gitInputs(),
			cmd: Command{Kind: CmdCancelIntent, Origin: OriginDesktop,
				CancelIntent: &IntentRef{Intent: "i9"}}},
		{name: "cancel suspended discard intent",
			given: and(forked(), discardEv("i4", forkS2),
				effectFailedEv("discard_worktree:i4", conflictProb)), in: gitInputs(),
			cmd: Command{Kind: CmdCancelIntent, Origin: OriginDesktop,
				CancelIntent: &IntentRef{Intent: "i4"}},
			want: []Event{intentCancelledEv("i4")}},
		{name: "discard after cancel",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m"),
				effectFailedEv("merge_worktree:i4", conflictProb), intentCancelledEv("i4")),
			in:   gitInputs(),
			cmd:  Command{Kind: CmdDiscard, Origin: OriginDesktop, Discard: &Discard{Target: forkS2}},
			want: []Event{discardEv("i7", forkS2)}},
		{name: "cancel intent missing payload", given: forked(), in: gitInputs(),
			cmd:    Command{Kind: CmdCancelIntent, Origin: OriginDesktop},
			reason: ReasonInvalid},
	})
}

func reviewerEv(sid SessionID, l LaunchSnapshot) Event {
	return Event{Kind: SessionCreatedKind, Session: sid, SessionCreated: &SessionCreated{
		Launch: l, Role: RoleReviewer,
	}}
}

func reviewReqEv(cands []SessionID, l *LaunchSnapshot) Event {
	return Event{Kind: ReviewRequestedKind, ReviewRequested: &ReviewRequested{
		Candidates: cands, Launch: l,
	}}
}

func keptEv(sid SessionID) Event {
	return Event{Kind: WorktreeKeptKind, Session: sid,
		WorktreeKept: &Target{Level: TargetFork, Session: sid}}
}

func recommendedEv(winner SessionID) Event {
	return Event{Kind: ForkRecommendedKind,
		ForkRecommended: &Recommendation{Winner: winner, Rationale: "better"}}
}

func launchDelete() LaunchSnapshot {
	l := newLaunch()
	l.Caps.DeleteNative = true
	return l
}

// reviewable 是兩個 Fork Session 的串流：s2 已完成一回合（idle），s3 的回合在跑。
func reviewable() []Event {
	return seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"),
		forkEv("s2", "main"), forkEv("s3", "main"),
		queuedEv("s2", "p6", "x"), turnEv("s2", "t7", "p6"), completedEv("s2"),
		queuedEv("s3", "p9", "x"), turnEv("s3", "t10", "p9")) // v10
}

// reviewed 是兩個 idle Fork 且 Review 已請求的串流。
func reviewed(l *LaunchSnapshot) []Event {
	return seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"),
		forkEv("s2", "main"), forkEv("s3", "main"),
		queuedEv("s2", "p6", "x"), turnEv("s2", "t7", "p6"), completedEv("s2"),
		queuedEv("s3", "p9", "x"), turnEv("s3", "t10", "p9"), completedEv("s3"),
		reviewReqEv([]SessionID{"s2", "s3"}, l)) // v12
}

func TestDecideFanOut(t *testing.T) {
	fanOutCmd := func(n int, origin Origin) Command {
		ls := make([]LaunchSnapshot, n)
		for i := range ls {
			ls[i] = newLaunch()
		}
		return Command{Kind: CmdFanOut, Origin: origin, FanOut: &FanOut{
			Prompt: Prompt{Text: "Build it"}, Launches: ls}}
	}
	twoFan := []Event{
		sessionEv("s3"), forkEv("s3", "main"), queuedEv("s3", "p5", "Build it"), turnEv("s3", "t6", "p5"),
		sessionEv("s7"), forkEv("s7", "main"), queuedEv("s7", "p9", "Build it"), turnEv("s7", "t10", "p9"),
		statusEv(TaskTodo, TaskInProgress, CauseAuto),
	}
	rev := newLaunch()
	runDecide(t, []decideCase{
		// fan_out。
		{name: "fan out two launches", given: seqs(created(IsolationNone), sessionEv("s2")),
			in: gitInputs(), cmd: fanOutCmd(2, OriginDesktop), want: twoFan, status: TaskInProgress},
		{name: "fan out on worktree task", given: worktreeTask(), in: gitInputs(),
			cmd: fanOutCmd(2, OriginDesktop),
			want: []Event{
				sessionEv("s3"), forkEv("s3", "orielhub/t-1"), queuedEv("s3", "p5", "Build it"), turnEv("s3", "t6", "p5"),
				sessionEv("s7"), forkEv("s7", "orielhub/t-1"), queuedEv("s7", "p9", "Build it"), turnEv("s7", "t10", "p9"),
				statusEv(TaskTodo, TaskInProgress, CauseAuto),
			}},
		{name: "fan out one launch", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    fanOutCmd(1, OriginDesktop),
			reason: ReasonInvalid, detail: "two launches"},
		{name: "fan out not git", given: seqs(created(IsolationNone)),
			in:     inputs(func(i *Inputs) { i.Repo.IsGit = false }),
			cmd:    fanOutCmd(2, OriginDesktop),
			reason: ReasonNotGit},
		{name: "fan out detached", given: seqs(created(IsolationNone)),
			in:     inputs(func(i *Inputs) { i.Repo.Branch = "" }),
			cmd:    fanOutCmd(2, OriginDesktop),
			reason: ReasonDetachedHead},
		{name: "fan out dirty", given: seqs(created(IsolationNone)),
			in:     inputs(func(i *Inputs) { i.Repo.Dirty = true }),
			cmd:    fanOutCmd(2, OriginDesktop),
			reason: ReasonDirtyWorkdir},
		{name: "fan out with open fork", given: forked(), in: gitInputs(),
			cmd:    fanOutCmd(2, OriginDesktop),
			reason: ReasonUnresolvedChanges},
		{name: "fan out remote", given: seqs(created(IsolationNone), sessionEv("s2")),
			in: gitInputs(), cmd: fanOutCmd(2, OriginRemote), want: twoFan},
		{name: "fan out empty prompt", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: func() Command {
				c := fanOutCmd(2, OriginDesktop)
				c.FanOut.Prompt = Prompt{}
				return c
			}(),
			reason: ReasonInvalid, detail: "prompt"},
		{name: "fan out bad launch", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: func() Command {
				c := fanOutCmd(2, OriginDesktop)
				c.FanOut.Launches[0].Executable = ""
				return c
			}(),
			reason: ReasonInvalid, detail: "launch"},
		{name: "fan out missing payload", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    Command{Kind: CmdFanOut, Origin: OriginDesktop},
			reason: ReasonInvalid},

		// autoReview（D17、D38）。
		{name: "last fork stop requests review", given: reviewable(),
			in: inputs(func(i *Inputs) { i.Reviewer = &rev }),
			cmd: sessionCmd(FactTurnEnded, "s3", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want: []Event{
				completedEv("s3"),
				statusEv(TaskTodo, TaskInReview, CauseAuto),
				reviewReqEv([]SessionID{"s2", "s3"}, &rev),
			},
			status: TaskInReview},
		{name: "review without reviewer", given: reviewable(), in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s3", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want: []Event{
				completedEv("s3"),
				statusEv(TaskTodo, TaskInReview, CauseAuto),
				reviewReqEv([]SessionID{"s2", "s3"}, nil),
			}},
		{name: "fork still running no review",
			given: seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"),
				forkEv("s2", "main"), forkEv("s3", "main"),
				queuedEv("s2", "p6", "x"), queuedEv("s2", "p7", "x"), turnEv("s2", "t8", "p6"),
				queuedEv("s3", "p9", "x"), turnEv("s3", "t10", "p9"), completedEv("s3")),
			in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s2", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want: []Event{
				completedEv("s2"), turnEv("s2", "t13", "p7"),
				statusEv(TaskTodo, TaskInProgress, CauseAuto),
			}},
		{name: "fork with intent no review",
			given: and(reviewable(), discardEv("i11", Target{Level: TargetFork, Session: "s3"})),
			in:    gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s3", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want: []Event{completedEv("s3"), statusEv(TaskTodo, TaskInReview, CauseAuto)}},
		{name: "review exists already",
			given: and(reviewable(), reviewReqEv([]SessionID{"s2", "s3"}, &rev)), in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s3", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want: []Event{completedEv("s3"), statusEv(TaskTodo, TaskInReview, CauseAuto)}},
		{name: "no review on archived task",
			given: and(reviewable(), completedEv("s3"), Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd:  sessionCmd(FactExited, "s3", OriginRuntime, func(c *Command) { c.Exited = &Exited{Problem: Problem{Code: "proc.exited"}} }),
			want: []Event{{Kind: ProcessExitedKind, Session: "s3", ProcessExited: &Exited{Problem: Problem{Code: "proc.exited"}}}}},
		{name: "remote close requests review",
			given: reviewable(),
			in:    inputs(func(i *Inputs) { i.Reviewer = &rev }),
			cmd: sessionCmd(CmdCloseSession, "s3", OriginRemote, func(c *Command) {
				c.CloseSession = &CloseSession{}
			}),
			want: []Event{
				keptEv("s3"), cancelledEv("s3"), closedEv("s3"),
				statusEv(TaskTodo, TaskInReview, CauseAuto),
				reviewReqEv([]SessionID{"s2", "s3"}, &rev),
			}},
		{name: "exit requests review",
			given: reviewable(),
			in:    inputs(func(i *Inputs) { i.Reviewer = &rev }),
			cmd: sessionCmd(FactExited, "s3", OriginRuntime, func(c *Command) {
				c.Exited = &Exited{Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}}
			}),
			want: []Event{
				{Kind: TurnFailedKind, Session: "s3", TurnFailed: &TurnFailed{
					Turn: "t10", Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}}},
				{Kind: ProcessExitedKind, Session: "s3", ProcessExited: &Exited{
					Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}}},
				statusEv(TaskTodo, TaskInReview, CauseAuto),
				reviewReqEv([]SessionID{"s2", "s3"}, &rev),
			}},
		{name: "edit with stopped forks no review",
			given: seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"),
				forkEv("s2", "main"), forkEv("s3", "main"),
				queuedEv("s2", "p6", "x"), turnEv("s2", "t7", "p6"), completedEv("s2"),
				queuedEv("s3", "p9", "x"), turnEv("s3", "t10", "p9"), completedEv("s3")),
			in: gitInputs(),
			cmd: Command{Kind: CmdEditTask, Origin: OriginDesktop,
				EditTask: &EditTask{Title: "New", Brief: "Build it"}},
			want: []Event{{Kind: TaskEditedKind, TaskEdited: &EditTask{Title: "New", Brief: "Build it"}}}},
	})
}

func TestDecideReview(t *testing.T) {
	rev := newLaunch()
	forkS2 := Target{Level: TargetFork, Session: "s2"}
	forkS3 := Target{Level: TargetFork, Session: "s3"}
	diffs := []ForkDiff{
		{Session: "s2", Diff: Attachment{Name: "s2.diff", MIME: "text/x-diff"}},
		{Session: "s3", Diff: Attachment{Name: "s3.diff", MIME: "text/x-diff"}},
	}
	promptState := State{Title: "Login page", Brief: "Build it",
		Sessions: map[SessionID]Session{"s2": {Launch: newLaunch()}, "s3": {Launch: newLaunch()}}}
	wantPrompt := reviewPrompt(promptState, []SessionID{"s2", "s3"}, diffs)
	runDecide(t, []decideCase{
		// review_prepared。
		{name: "review prepared creates reviewer",
			given: reviewed(&rev), in: gitInputs(),
			cmd: sessionCmd(FactReviewPrepared, "", OriginRuntime, func(c *Command) {
				c.ReviewPrepared = &ReviewPrepared{Diffs: diffs}
			}),
			want: []Event{
				reviewerEv("s13", rev),
				{Kind: PromptQueuedKind, Session: "s13",
					PromptQueued: &Prompt{ID: "p14", Text: wantPrompt.Text, Attachments: wantPrompt.Attachments}},
				turnEv("s13", "t15", "p14"),
				statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			check: func(t *testing.T, s State) {
				if s.Review.Reviewer != "s13" {
					t.Errorf("reviewer = %q", s.Review.Reviewer)
				}
			}},
		{name: "review prepared wrong count", given: reviewed(&rev), in: gitInputs(),
			cmd: sessionCmd(FactReviewPrepared, "", OriginRuntime, func(c *Command) {
				c.ReviewPrepared = &ReviewPrepared{Diffs: diffs[:1]}
			}),
			reason: ReasonInvalid, detail: "diff count"},
		{name: "review prepared non-candidate", given: reviewed(&rev), in: gitInputs(),
			cmd: sessionCmd(FactReviewPrepared, "", OriginRuntime, func(c *Command) {
				c.ReviewPrepared = &ReviewPrepared{Diffs: []ForkDiff{
					{Session: "s2", Diff: Attachment{Name: "a"}},
					{Session: "s9", Diff: Attachment{Name: "b"}},
				}}
			}),
			reason: ReasonInvalid, detail: "s3"},
		{name: "review prepared no review", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: sessionCmd(FactReviewPrepared, "", OriginRuntime, func(c *Command) {
				c.ReviewPrepared = &ReviewPrepared{Diffs: diffs}
			})},
		{name: "review prepared launch nil",
			given: reviewed(nil), in: gitInputs(),
			cmd: sessionCmd(FactReviewPrepared, "", OriginRuntime, func(c *Command) {
				c.ReviewPrepared = &ReviewPrepared{Diffs: diffs}
			})},
		{name: "review prepared reviewer set",
			given: and(reviewed(&rev), reviewerEv("s13", rev)), in: gitInputs(),
			cmd: sessionCmd(FactReviewPrepared, "", OriginRuntime, func(c *Command) {
				c.ReviewPrepared = &ReviewPrepared{Diffs: diffs}
			})},
		{name: "review prepared missing payload", given: reviewed(&rev), in: gitInputs(),
			cmd:    sessionCmd(FactReviewPrepared, "", OriginRuntime, nil),
			reason: ReasonInvalid},

		// recommendation。
		{name: "recommendation",
			given: and(reviewed(&rev), reviewerEv("s13", rev)), in: gitInputs(),
			cmd: sessionCmd(FactRecommendation, "", OriginRuntime, func(c *Command) {
				c.Recommendation = &Recommendation{Winner: "s2", Rationale: "better"}
			}),
			want: []Event{recommendedEv("s2")}},
		{name: "recommendation not candidate",
			given: and(reviewed(&rev), reviewerEv("s13", rev)), in: gitInputs(),
			cmd: sessionCmd(FactRecommendation, "", OriginRuntime, func(c *Command) {
				c.Recommendation = &Recommendation{Winner: "s9"}
			}),
			reason: ReasonInvalid, detail: "candidate"},
		{name: "recommendation no reviewer", given: reviewed(&rev), in: gitInputs(),
			cmd: sessionCmd(FactRecommendation, "", OriginRuntime, func(c *Command) {
				c.Recommendation = &Recommendation{Winner: "s2"}
			}),
			reason: ReasonInvalid, detail: "no reviewer"},
		{name: "recommendation no review", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: sessionCmd(FactRecommendation, "", OriginRuntime, func(c *Command) {
				c.Recommendation = &Recommendation{Winner: "s2"}
			})},
		{name: "recommendation duplicate",
			given: and(reviewed(&rev), reviewerEv("s13", rev), recommendedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(FactRecommendation, "", OriginRuntime, func(c *Command) {
				c.Recommendation = &Recommendation{Winner: "s3"}
			})},

		// choose_fork。
		{name: "choose fork with review",
			given: and(reviewed(&rev), reviewerEv("s9", rev)), in: gitInputs(),
			cmd: Command{Kind: CmdChooseFork, Origin: OriginDesktop,
				ChooseFork: &ChooseFork{Winner: "s2", Strategy: MergeSquash, Message: "m"}},
			want: []Event{
				mergeEv("i14", forkS2, MergeSquash, "m"),
				discardEv("i15", forkS3),
				closedEv("s9"),
				{Kind: ReviewEndedKind},
			}},
		{name: "choose fork no review",
			given: seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"),
				forkEv("s2", "main"), forkEv("s3", "main"),
				queuedEv("s2", "p6", "x"), turnEv("s2", "t7", "p6"), completedEv("s2"),
				queuedEv("s3", "p9", "x"), turnEv("s3", "t10", "p9"), completedEv("s3")),
			in: gitInputs(),
			cmd: Command{Kind: CmdChooseFork, Origin: OriginDesktop,
				ChooseFork: &ChooseFork{Winner: "s2"}},
			want: []Event{
				mergeEv("i12", forkS2, MergeSquash, "Login page\n\nBuild it"),
				discardEv("i13", forkS3),
			}},
		{name: "choose fork loser has intent",
			given: and(reviewed(&rev), discardEv("i13", forkS3)), in: gitInputs(),
			cmd: Command{Kind: CmdChooseFork, Origin: OriginDesktop,
				ChooseFork: &ChooseFork{Winner: "s2"}},
			want: []Event{
				mergeEv("i14", forkS2, MergeSquash, "Login page\n\nBuild it"),
				{Kind: ReviewEndedKind},
			}},
		{name: "choose fork winner running",
			given: seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"),
				forkEv("s2", "main"), forkEv("s3", "main"),
				queuedEv("s2", "p6", "x"), turnEv("s2", "t7", "p6"),
				queuedEv("s3", "p8", "x"), turnEv("s3", "t9", "p8"), completedEv("s3")),
			in: gitInputs(),
			cmd: Command{Kind: CmdChooseFork, Origin: OriginDesktop,
				ChooseFork: &ChooseFork{Winner: "s2"}},
			reason: ReasonSessionRunning},
		{name: "choose fork loser running", given: reviewable(), in: gitInputs(),
			cmd: Command{Kind: CmdChooseFork, Origin: OriginDesktop,
				ChooseFork: &ChooseFork{Winner: "s2"}},
			reason: ReasonSessionRunning},
		{name: "choose fork winner not fork",
			given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: Command{Kind: CmdChooseFork, Origin: OriginDesktop,
				ChooseFork: &ChooseFork{Winner: "s2"}},
			reason: ReasonInvalid, detail: "no fork"},
		{name: "choose fork winner has intent",
			given: and(reviewed(&rev), mergeEv("i13", forkS2, MergeSquash, "m")), in: gitInputs(),
			cmd: Command{Kind: CmdChooseFork, Origin: OriginDesktop,
				ChooseFork: &ChooseFork{Winner: "s2"}},
			reason: ReasonInvalid, detail: "already has an intent"},
		{name: "choose fork remote asks confirmation",
			given: and(reviewed(&rev), reviewerEv("s9", rev)), in: gitInputs(),
			cmd: Command{Kind: CmdChooseFork, Origin: OriginRemote, Device: "d9",
				ChooseFork: &ChooseFork{Winner: "s2"}},
			want: []Event{{Kind: ConfirmationRequestedKind, ConfirmationRequested: &Confirmation{
				ID: "c14", RequestedBy: "d9", RequestedAt: at0,
				Command: Command{Kind: CmdChooseFork, Origin: OriginRemote, Device: "d9",
					ChooseFork: &ChooseFork{Winner: "s2"}},
			}}}},
		{name: "confirm stored choose fork",
			given: and(reviewed(&rev), reviewerEv("s9", rev), confReqEv("c14", "d9",
				Command{Kind: CmdChooseFork, Origin: OriginRemote, Device: "d9",
					ChooseFork: &ChooseFork{Winner: "s2"}})),
			in:  gitInputs(),
			cmd: Command{Kind: CmdConfirm, Origin: OriginDesktop, Confirm: &ConfirmationRef{Confirmation: "c14"}},
			want: []Event{
				grantedEv("c14"),
				mergeEv("i16", forkS2, MergeSquash, "Login page\n\nBuild it"),
				discardEv("i17", forkS3),
				closedEv("s9"),
				{Kind: ReviewEndedKind},
			}},
		{name: "choose fork reviewer running",
			given: and(reviewed(&rev), reviewerEv("s9", rev),
				queuedEv("s9", "p14", "x"), turnEv("s9", "t15", "p14")), in: gitInputs(),
			cmd: Command{Kind: CmdChooseFork, Origin: OriginDesktop,
				ChooseFork: &ChooseFork{Winner: "s2"}},
			want: []Event{
				mergeEv("i16", forkS2, MergeSquash, "Login page\n\nBuild it"),
				discardEv("i17", forkS3),
				cancelledEv("s9"), closedEv("s9"),
				{Kind: ReviewEndedKind},
				statusEv(TaskTodo, TaskInReview, CauseAuto),
			}},

		// skip_review。
		{name: "skip review clears launch",
			given: and(reviewed(&rev), effectFailedEv("prepare_review", Problem{Code: "proc.exited"})),
			in:    gitInputs(),
			cmd:   Command{Kind: CmdSkipReview, Origin: OriginDesktop},
			want:  []Event{{Kind: ReviewSkippedKind}},
			check: func(t *testing.T, s State) {
				if s.Review == nil || s.Review.Launch != nil {
					t.Errorf("review %+v", s.Review)
				}
				if len(s.Failed) != 0 {
					t.Errorf("failed %v", s.Failed)
				}
			}},
		{name: "skip review closes reviewer",
			given: and(reviewed(&rev), reviewerEv("s9", rev),
				queuedEv("s9", "p14", "x"), turnEv("s9", "t15", "p14")), in: gitInputs(),
			cmd: Command{Kind: CmdSkipReview, Origin: OriginRemote},
			want: []Event{cancelledEv("s9"), closedEv("s9"), {Kind: ReviewSkippedKind},
				statusEv(TaskTodo, TaskInReview, CauseAuto)}},
		{name: "skip no review", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: Command{Kind: CmdSkipReview, Origin: OriginDesktop}},
		{name: "skip review launch nil", given: reviewed(nil), in: gitInputs(),
			cmd: Command{Kind: CmdSkipReview, Origin: OriginDesktop}},
		{name: "no re-review after skip",
			given: and(reviewable(), reviewReqEv([]SessionID{"s2", "s3"}, &rev),
				Event{Kind: ReviewSkippedKind}), in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s3", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want: []Event{completedEv("s3"), statusEv(TaskTodo, TaskInReview, CauseAuto)}},
	})
}

func TestDecideDeletion(t *testing.T) {
	msgGiven := seqs(created(IsolationNone), sessionEv("s2"),
		queuedEv("s2", "p3", "x"), turnEv("s2", "t4", "p3"),
		messageEv("s2", "m5", "t4", ChannelText, "hi", false))
	redactCmd := func(origin Origin) Command {
		return sessionCmd(CmdRedactMessage, "s2", origin, func(c *Command) {
			c.RedactMessage = &RedactMessage{Message: "m5"}
		})
	}
	runDecide(t, []decideCase{
		// redact_message。
		{name: "redact message", given: msgGiven, in: gitInputs(),
			cmd: redactCmd(OriginDesktop),
			want: []Event{{Kind: MessageRedactedKind, Session: "s2",
				MessageRedacted: &RedactMessage{Message: "m5"}}}},
		{name: "redact on closed session",
			given: and(msgGiven, closedEv("s2")), in: gitInputs(),
			cmd: redactCmd(OriginDesktop),
			want: []Event{{Kind: MessageRedactedKind, Session: "s2",
				MessageRedacted: &RedactMessage{Message: "m5"}}}},
		{name: "redact open message",
			given: seqs(created(IsolationNone), sessionEv("s2"),
				queuedEv("s2", "p3", "x"), turnEv("s2", "t4", "p3"),
				messageEv("s2", "m5", "t4", ChannelText, "hi", true)),
			in:     gitInputs(),
			cmd:    redactCmd(OriginDesktop),
			reason: ReasonInvalid, detail: "still open"},
		{name: "redact remote", given: msgGiven, in: gitInputs(),
			cmd:    redactCmd(OriginRemote),
			reason: ReasonNeedsDesktop},
		{name: "redact unknown session", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: sessionCmd(CmdRedactMessage, "s9", OriginDesktop, func(c *Command) {
				c.RedactMessage = &RedactMessage{Message: "m1"}
			}),
			reason: ReasonUnknownSession},
		{name: "redact missing payload", given: msgGiven, in: gitInputs(),
			cmd:    sessionCmd(CmdRedactMessage, "s2", OriginDesktop, nil),
			reason: ReasonInvalid},

		// delete_native_session。
		{name: "delete native closed",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), closedEv("s2")), in: gitInputs(),
			cmd:  sessionCmd(CmdDeleteNativeSession, "s2", OriginDesktop, nil),
			want: []Event{nativeDeletedEv("s2", "i5")},
			check: func(t *testing.T, s State) {
				if !s.Sessions["s2"].NativeGone {
					t.Error("NativeGone not set")
				}
			}},
		{name: "delete native exited",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), exitedEv("s2")), in: gitInputs(),
			cmd:  sessionCmd(CmdDeleteNativeSession, "s2", OriginDesktop, nil),
			want: []Event{nativeDeletedEv("s2", "i5")}},
		{name: "delete native idle",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2")), in: gitInputs(),
			cmd:    sessionCmd(CmdDeleteNativeSession, "s2", OriginDesktop, nil),
			reason: ReasonSessionOpen},
		{name: "delete native running",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), queuedEv("s2", "p4", "x"), turnEv("s2", "t5", "p4")),
			in:     gitInputs(),
			cmd:    sessionCmd(CmdDeleteNativeSession, "s2", OriginDesktop, nil),
			reason: ReasonSessionOpen},
		{name: "delete native remote",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), closedEv("s2")), in: gitInputs(),
			cmd:    sessionCmd(CmdDeleteNativeSession, "s2", OriginRemote, nil),
			reason: ReasonNeedsDesktop},
		{name: "delete native no cap",
			given:  seqs(created(IsolationNone), sessionEv("s2"), boundEv("s2", "n2"), closedEv("s2")),
			in:     gitInputs(),
			cmd:    sessionCmd(CmdDeleteNativeSession, "s2", OriginDesktop, nil),
			reason: ReasonInvalid, detail: "cannot delete"},
		{name: "delete native no native id",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				closedEv("s2")), in: gitInputs(),
			cmd: sessionCmd(CmdDeleteNativeSession, "s2", OriginDesktop, nil)},
		{name: "delete native gone",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), closedEv("s2"), nativeDeletedEv("s2", "i5"),
				Event{Kind: NativeFilesRemovedKind, Session: "s2",
					NativeFilesRemoved: &IntentRef{Intent: "i5"}}),
			in:  gitInputs(),
			cmd: sessionCmd(CmdDeleteNativeSession, "s2", OriginDesktop, nil)},
		{name: "delete native intent exists",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), closedEv("s2"), nativeDeletedEv("s2", "i5")),
			in:  gitInputs(),
			cmd: sessionCmd(CmdDeleteNativeSession, "s2", OriginDesktop, nil)},
		{name: "delete native unknown session", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd:    sessionCmd(CmdDeleteNativeSession, "s9", OriginDesktop, nil),
			reason: ReasonUnknownSession},

		// native_deleted 事實。
		{name: "native deleted fact",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), closedEv("s2"), nativeDeletedEv("s2", "i5")), in: gitInputs(),
			cmd: sessionCmd(FactNativeDeleted, "", OriginRuntime, func(c *Command) {
				c.NativeDeleted = &IntentResult{Intent: "i5"}
			}),
			want: []Event{{Kind: NativeFilesRemovedKind, Session: "s2",
				NativeFilesRemoved: &IntentRef{Intent: "i5"}}},
			check: func(t *testing.T, s State) {
				if len(s.Intents) != 0 {
					t.Errorf("intents %v", s.Intents)
				}
			}},
		{name: "native deleted missing intent", given: seqs(created(IsolationNone)), in: gitInputs(),
			cmd: sessionCmd(FactNativeDeleted, "", OriginRuntime, func(c *Command) {
				c.NativeDeleted = &IntentResult{Intent: "i9"}
			})},
		{name: "native deleted merge intent",
			given: and(forked(), mergeEv("i4", Target{Level: TargetFork, Session: "s2"}, MergeSquash, "m")),
			in:    gitInputs(),
			cmd: sessionCmd(FactNativeDeleted, "", OriginRuntime, func(c *Command) {
				c.NativeDeleted = &IntentResult{Intent: "i4"}
			}),
			reason: ReasonInvalid, detail: "is a merge"},
		{name: "native deleted suspended",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), closedEv("s2"), nativeDeletedEv("s2", "i5"),
				effectFailedEv("delete_native:i5", Problem{Code: "proc.exited"})),
			in: gitInputs(),
			cmd: sessionCmd(FactNativeDeleted, "", OriginRuntime, func(c *Command) {
				c.NativeDeleted = &IntentResult{Intent: "i5"}
			}),
			reason: ReasonInvalid, detail: "suspended"},
		{name: "resume after delete native",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), exitedEv("s2"), nativeDeletedEv("s2", "i5")),
			in:     gitInputs(),
			cmd:    sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			reason: ReasonNotResumable},
	})
}

func TestReviewPrompt(t *testing.T) {
	s := State{Title: "Login page", Brief: "Build it",
		Sessions: map[SessionID]Session{
			"s2": {ID: "s2", Launch: newLaunch()},
			"s3": {ID: "s3", Launch: LaunchSnapshot{AgentName: "Codex", Profile: "p9", ProfileName: "cli"}},
		}}
	diff := func(name string) Attachment { return Attachment{Name: name, MIME: "text/x-diff"} }
	diffs := []ForkDiff{
		{Session: "s2", Diff: diff("s2.diff")},
		{Session: "s3", Diff: diff("s3.diff")},
	}
	p := reviewPrompt(s, []SessionID{"s2", "s3"}, diffs)
	want := "You are reviewing 2 independent attempts at the same task.\n\n" +
		"Task: Login page\n\nBuild it\n\nCandidates:\n" +
		"- s2: Claude Code (default) — attachment s2.diff\n" +
		"- s3: Codex (cli) — attachment s3.diff\n" +
		"\nCompare the diffs for correctness, completeness and code quality. " +
		"Explain your reasoning, then end your reply with exactly one final line of the form:\n" +
		"WINNER: <session id>"
	if p.Text != want {
		t.Errorf("text =\n%s\nwant\n%s", p.Text, want)
	}
	if !reflect.DeepEqual(p.Attachments, []Attachment{diff("s2.diff"), diff("s3.diff")}) {
		t.Errorf("attachments %v", p.Attachments)
	}
	s.Brief = ""
	if got := reviewPrompt(s, []SessionID{"s2", "s3"}, diffs).Text; !strings.Contains(got, "Task: Login page\n\nCandidates:\n") {
		t.Errorf("empty brief leaves blank lines:\n%s", got)
	}
}

// TestDecideReviewEnd 涵蓋 D43：候選剩不到兩個時評審自動結束。
func TestDecideReviewEnd(t *testing.T) {
	rev := newLaunch()
	forkS2 := Target{Level: TargetFork, Session: "s2"}
	forkS3 := Target{Level: TargetFork, Session: "s3"}
	remoteMerge := Command{Kind: CmdMerge, Origin: OriginRemote, Device: "d9",
		Merge: &Merge{Target: forkS2}}
	reviewEnded := Event{Kind: ReviewEndedKind}
	runDecide(t, []decideCase{
		{name: "review ends when one candidate left", given: reviewed(&rev), in: gitInputs(),
			cmd: Command{Kind: CmdMerge, Origin: OriginDesktop,
				Merge: &Merge{Target: forkS2, Strategy: MergeSquash, Message: "m"}},
			want: []Event{mergeEv("i13", forkS2, MergeSquash, "m"), reviewEnded}},
		{name: "review ends and closes idle reviewer",
			given: and(reviewed(&rev), reviewerEv("s9", rev)), in: gitInputs(),
			cmd:  Command{Kind: CmdDiscard, Origin: OriginDesktop, Discard: &Discard{Target: forkS2}},
			want: []Event{discardEv("i14", forkS2), closedEv("s9"), reviewEnded}},
		{name: "review ends and cancels running reviewer",
			given: and(reviewed(&rev), reviewerEv("s9", rev),
				queuedEv("s9", "p14", "x"), turnEv("s9", "t15", "p14")), in: gitInputs(),
			cmd: Command{Kind: CmdMerge, Origin: OriginDesktop,
				Merge: &Merge{Target: forkS2, Strategy: MergeSquash, Message: "m"}},
			want: []Event{
				mergeEv("i16", forkS2, MergeSquash, "m"),
				cancelledEv("s9"), closedEv("s9"), reviewEnded,
				statusEv(TaskTodo, TaskInReview, CauseAuto),
			}},
		{name: "review stays with two candidates left",
			given: seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"), sessionEv("s4"),
				forkEv("s2", "main"), forkEv("s3", "main"), forkEv("s4", "main"),
				reviewReqEv([]SessionID{"s2", "s3", "s4"}, &rev)), in: gitInputs(),
			cmd: Command{Kind: CmdMerge, Origin: OriginDesktop,
				Merge: &Merge{Target: forkS2, Strategy: MergeSquash, Message: "m"}},
			want: []Event{mergeEv("i9", forkS2, MergeSquash, "m")}},
		{name: "fan-out after review ended triggers review again",
			given: seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"),
				forkEv("s2", "main"), forkEv("s3", "main"),
				queuedEv("s2", "p6", "x"), turnEv("s2", "t7", "p6"), completedEv("s2"),
				queuedEv("s3", "p9", "x"), turnEv("s3", "t10", "p9"), completedEv("s3"),
				reviewReqEv([]SessionID{"s2", "s3"}, &rev),
				mergeEv("i13", forkS2, MergeSquash, "m"), reviewEnded,
				Event{Kind: ForkMergedKind, Session: "s2", Merged: &IntentResult{Intent: "i13"}},
				closedEv("s2"),
				discardEv("i17", forkS3),
				Event{Kind: ForkDiscardedKind, Session: "s3", Discarded: &IntentRef{Intent: "i17"}},
				closedEv("s3"),
				sessionEv("s20"), forkEv("s20", "main"), queuedEv("s20", "p22", "x"), turnEv("s20", "t23", "p22"),
				sessionEv("s24"), forkEv("s24", "main"), queuedEv("s24", "p26", "x"), turnEv("s24", "t27", "p26"),
				statusEv(TaskTodo, TaskInProgress, CauseAuto),
				completedEv("s20")), // v29
			in: gitInputs(),
			cmd: sessionCmd(FactTurnEnded, "s24", OriginRuntime, func(c *Command) {
				c.TurnEnded = &TurnEnded{Result: TurnOK}
			}),
			want: []Event{
				completedEv("s24"),
				statusEv(TaskInProgress, TaskInReview, CauseAuto),
				reviewReqEv([]SessionID{"s20", "s24"}, nil),
			}},
		{name: "remote merge keeps review", given: reviewed(&rev), in: gitInputs(),
			cmd:  remoteMerge,
			want: []Event{confReqEv("c13", "d9", remoteMerge)}},
		{name: "cancel intent does not reopen review",
			given: and(reviewed(&rev), mergeEv("i13", forkS2, MergeSquash, "m"), reviewEnded,
				effectFailedEv("merge_worktree:i13", Problem{Code: "proc.exited"})),
			in: gitInputs(),
			cmd: Command{Kind: CmdCancelIntent, Origin: OriginDesktop,
				CancelIntent: &IntentRef{Intent: "i13"}},
			want: []Event{intentCancelledEv("i13")}},
		{name: "skipped review ends when resolved", given: reviewed(nil), in: gitInputs(),
			cmd: Command{Kind: CmdMerge, Origin: OriginDesktop,
				Merge: &Merge{Target: forkS2, Strategy: MergeSquash, Message: "m"}},
			want: []Event{mergeEv("i13", forkS2, MergeSquash, "m"), reviewEnded}},
		{name: "remote close keeps review", given: reviewed(&rev), in: gitInputs(),
			cmd: sessionCmd(CmdCloseSession, "s2", OriginRemote, func(c *Command) {
				c.CloseSession = &CloseSession{}
			}),
			want: []Event{keptEv("s2"), closedEv("s2")}},
	})
}
