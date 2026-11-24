package task

import (
	"testing"
)

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
