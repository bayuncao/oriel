package task

import "testing"

func reviewerEv(sid SessionID, l LaunchSnapshot) Event {
	return Event{Kind: SessionCreatedKind, Session: sid, SessionCreated: &SessionCreated{
		Launch: l, Role: RoleReviewer,
	}}
}

func recommendedEv(winner SessionID) Event {
	return Event{Kind: ForkRecommendedKind,
		ForkRecommended: &Recommendation{Winner: winner, Rationale: "better"}}
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

func keptEv(sid SessionID) Event {
	return Event{Kind: WorktreeKeptKind, Session: sid,
		WorktreeKept: &Target{Level: TargetFork, Session: sid}}
}
