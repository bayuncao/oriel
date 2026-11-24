package task

import (
	"testing"
	"time"
)

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

func answeredEv(sid SessionID, id PermissionID) Event {
	return Event{Kind: PermissionAnsweredKind, Session: sid,
		PermissionAnswered: &PermissionRef{Permission: id}}
}

func ruleEv(r Rule) Event {
	return Event{Kind: RuleAddedKind, RuleAdded: &r}
}

func cancelledConfEv(id ConfirmationID) Event {
	return Event{Kind: ConfirmationCancelledKind, ConfirmationCancelled: &ConfirmationRef{Confirmation: id}}
}
