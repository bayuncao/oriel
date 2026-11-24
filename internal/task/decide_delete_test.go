package task

import (
	"testing"
)

func launchDelete() LaunchSnapshot {
	l := newLaunch()
	l.Caps.DeleteNative = true
	return l
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
