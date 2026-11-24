package task

import (
	"testing"
)

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
		// L56：agent 沒起來（沒有原生會話）時失敗的那一輪，它的 Prompt 重新排在隊首（新的 ID），在排隊的之前。
		{name: "resume requeues the failed first prompt",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), queuedEv("s2", "p5", "Then test"), failedEv("s2", "t4"), exitedEv("s2")),
			in:  gitInputs(),
			cmd: sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			want: []Event{
				{Kind: SessionResumedKind, Session: "s2", SessionResumed: &SessionResumed{Requeued: &Prompt{ID: "p8", Text: "Build it"}}},
				turnEv("s2", "t9", "p8"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			status: TaskInProgress},
		// 接回時起不來（這個行程沒綁定原生會話就失敗）：那條 Prompt 也從沒送到，同樣重新排上。
		{name: "resume after a failed restart requeues",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"), turnEv("s2", "t4", "p3"),
				boundEv("s2", "n5"), completedEv("s2"), exitedEv("s2"), resumedEv("s2"),
				queuedEv("s2", "p9", "Again"), turnEv("s2", "t10", "p9"), failedEv("s2", "t10"), exitedEv("s2")),
			in:  gitInputs(),
			cmd: sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			want: []Event{
				{Kind: SessionResumedKind, Session: "s2", SessionResumed: &SessionResumed{Requeued: &Prompt{ID: "p13", Text: "Again"}}},
				turnEv("s2", "t14", "p13"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			status: TaskInProgress},
		// 重新排上的那一輪又失敗（agent 仍沒登錄）：再重試時再排上一次，排隊的仍在它後面。
		{name: "resume requeues again after the retry fails",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"), turnEv("s2", "t4", "p3"),
				queuedEv("s2", "p5", "Then test"), failedEv("s2", "t4"), exitedEv("s2"),
				Event{Kind: SessionResumedKind, Session: "s2", SessionResumed: &SessionResumed{Requeued: &Prompt{ID: "p8", Text: "Build it"}}},
				turnEv("s2", "t9", "p8"), failedEv("s2", "t9"), exitedEv("s2")),
			in:  gitInputs(),
			cmd: sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			want: []Event{
				{Kind: SessionResumedKind, Session: "s2", SessionResumed: &SessionResumed{Requeued: &Prompt{ID: "p12", Text: "Build it"}}},
				turnEv("s2", "t13", "p12"), statusEv(TaskTodo, TaskInProgress, CauseAuto),
			},
			status: TaskInProgress},
		// 之後又開始了一輪（綁定後才失敗）：先前記下的那條已清掉，不重新排上。
		{name: "resume after a later bound turn does not requeue",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"), turnEv("s2", "t4", "p3"),
				failedEv("s2", "t4"), queuedEv("s2", "p6", "Again"), turnEv("s2", "t7", "p6"), boundEv("s2", "n8"),
				failedEv("s2", "t7"), exitedEv("s2")),
			in:   gitInputs(),
			cmd:  sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			want: []Event{resumedEv("s2")}},
		// 原生會話已綁定：agent 起來過，Prompt 已送到，不重新排上。
		{name: "resume bound does not requeue",
			given: seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
				turnEv("s2", "t4", "p3"), boundEv("s2", "n5"), failedEv("s2", "t4"), exitedEv("s2")),
			in:   gitInputs(),
			cmd:  sessionCmd(CmdResume, "s2", OriginDesktop, nil),
			want: []Event{resumedEv("s2")}},
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

func failedEv(sid SessionID, tid TurnID) Event {
	return Event{Kind: TurnFailedKind, Session: sid, TurnFailed: &TurnFailed{
		Turn: tid, Problem: Problem{Code: "acp.auth_required", Params: map[string]string{"agent": "Gemini CLI"}}}}
}
