package task

import (
	"reflect"
	"testing"
)

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

func conflictEv(id IntentID, files ...string) Event {
	return Event{Kind: MergeConflictedKind, MergeConflicted: &MergeConflict{Intent: id, Files: files}}
}
