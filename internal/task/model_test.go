package task

import (
	"testing"
)

// 換模型（m2.md V77–V89，D118）的事件建構器與 Decide 的表。

func modelEv(sid SessionID, start bool, m *ModelSetting) Event {
	return Event{Kind: ModelReportedKind, Session: sid, ModelReported: &ModelReport{Setting: m, Start: start}}
}

func chosenEv(sid SessionID, v string) Event {
	return Event{Kind: ModelChosenKind, Session: sid, ModelChosen: &SetModel{Value: v}}
}

func switchFailedEv(sid SessionID, v string, p Problem) Event {
	return Event{Kind: ModelSwitchFailedKind, Session: sid, ModelSwitchFailed: &ModelProblem{Value: v, Problem: p}}
}

// onlyFast 是只剩 fast 的模型列表（deep 已不在）。
func onlyFast() *ModelSetting {
	return &ModelSetting{Option: "model", Current: "fast", Choices: []ModelChoice{{Value: "fast", Name: "Fast", Description: "Short answers"}}}
}

// idleModel 是閒著、agent 報告了模型設置（目前 fast）的 Session s2。
func idleModel(more ...Event) []Event {
	return and(seqs(created(IsolationNone), sessionEv("s2"), boundEv("s2", "n2"), modelEv("s2", true, sampleModel("fast"))), more...)
}

func setModelCmd(v string) Command {
	return sessionCmd(CmdSetModel, "s2", OriginDesktop, func(c *Command) { c.SetModel = &SetModel{Value: v} })
}

func reportCmd(start bool, m *ModelSetting) Command {
	return sessionCmd(FactModelReported, "s2", OriginRuntime, func(c *Command) { c.ModelReported = &ModelReport{Setting: m, Start: start} })
}

// wanted 檢查摺疊後 s2 的 ModelWanted 與 Model.Current。
func wanted(want, current string) func(*testing.T, State) {
	return func(t *testing.T, s State) {
		t.Helper()
		x := s.Sessions["s2"]
		if x.ModelWanted != want {
			t.Errorf("ModelWanted = %q, want %q", x.ModelWanted, want)
		}
		got := ""
		if x.Model != nil {
			got = x.Model.Current
		}
		if got != current {
			t.Errorf("Model.Current = %q, want %q", got, current)
		}
	}
}

func TestDecideModel(t *testing.T) {
	unavailable := Problem{Code: "task.model_unavailable"}
	failure := Problem{Code: "acp.request_failed", Params: map[string]string{"method": "session/set_config_option", "detail": "no"}}
	runDecide(t, []decideCase{
		// SetModel（V80–V82、V85、V87）。
		{name: "V80 set model", given: idleModel(), in: gitInputs(),
			cmd: setModelCmd("deep"), want: []Event{chosenEv("s2", "deep")}, check: wanted("deep", "fast")},
		{name: "set model already current", given: idleModel(), in: gitInputs(), cmd: setModelCmd("fast")},
		{name: "set model already wanted", given: idleModel(chosenEv("s2", "deep")), in: gitInputs(), cmd: setModelCmd("deep")},
		{name: "V82 set model back while switching", given: idleModel(chosenEv("s2", "deep")), in: gitInputs(),
			cmd: setModelCmd("fast"), reason: ReasonModelSwitching},
		{name: "set model after the switch is confirmed",
			given: idleModel(chosenEv("s2", "deep"), modelEv("s2", false, sampleModel("deep"))), in: gitInputs(),
			cmd: setModelCmd("fast"), want: []Event{chosenEv("s2", "fast")}, check: wanted("fast", "deep")},
		{name: "set model clears the last problem",
			given: idleModel(chosenEv("s2", "deep"), switchFailedEv("s2", "deep", failure)), in: gitInputs(),
			cmd: setModelCmd("deep"), want: []Event{chosenEv("s2", "deep")},
			check: func(t *testing.T, s State) {
				if p := s.Sessions["s2"].ModelProblem; p != nil {
					t.Errorf("ModelProblem = %+v, want nil", p)
				}
			}},
		{name: "V81 set model during a turn",
			given: idleModel(queuedEv("s2", "p6", "go"), turnEv("s2", "t7", "p6")), in: gitInputs(),
			cmd: setModelCmd("deep"), reason: ReasonSessionRunning},
		{name: "set model not offered", given: idleModel(), in: gitInputs(), cmd: setModelCmd("huge"), reason: ReasonModelUnavailable},
		{name: "V78 set model without a report", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: setModelCmd("deep"), reason: ReasonModelUnavailable},
		{name: "V87 set model exited", given: idleModel(exitedEv("s2")), in: gitInputs(),
			cmd: setModelCmd("deep"), reason: ReasonSessionExited},
		{name: "set model closed", given: idleModel(closedEv("s2")), in: gitInputs(),
			cmd: setModelCmd("deep"), reason: ReasonSessionClosed},
		{name: "set model archived", given: idleModel(Event{Kind: TaskArchivedKind}), in: gitInputs(),
			cmd: setModelCmd("deep"), reason: ReasonArchived},
		{name: "set model conflicted", given: idleModel(conflictedEv("s2")), in: gitInputs(),
			cmd: setModelCmd("deep"), reason: ReasonConflicted},
		{name: "set model unknown session", given: idleModel(), in: gitInputs(),
			cmd:    sessionCmd(CmdSetModel, "s9", OriginDesktop, func(c *Command) { c.SetModel = &SetModel{Value: "deep"} }),
			reason: ReasonUnknownSession},
		{name: "set model from runtime", given: idleModel(), in: gitInputs(),
			cmd:    sessionCmd(CmdSetModel, "s2", OriginRuntime, func(c *Command) { c.SetModel = &SetModel{Value: "deep"} }),
			reason: ReasonForgedOrigin},
		{name: "set model missing payload", given: idleModel(), in: gitInputs(),
			cmd: sessionCmd(CmdSetModel, "s2", OriginDesktop, nil), reason: ReasonInvalid},

		// model_reported（V77、V84、V86，C45、C46）。
		{name: "V77 first report", given: seqs(created(IsolationNone), sessionEv("s2"), boundEv("s2", "n2")), in: gitInputs(),
			cmd: reportCmd(true, sampleModel("fast")), want: []Event{modelEv("s2", true, sampleModel("fast"))}, check: wanted("", "fast")},
		{name: "report unchanged", given: idleModel(), in: gitInputs(), cmd: reportCmd(true, sampleModel("fast"))},
		{name: "V78 agent reports no model", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: reportCmd(true, nil)},
		{name: "V84 agent changed the model itself", given: idleModel(chosenEv("s2", "deep"), modelEv("s2", false, sampleModel("deep"))), in: gitInputs(),
			cmd: reportCmd(false, sampleModel("fast")), want: []Event{modelEv("s2", false, sampleModel("fast"))}, check: wanted("", "fast")},
		{name: "V80 switch confirmed", given: idleModel(chosenEv("s2", "deep")), in: gitInputs(),
			cmd: reportCmd(false, sampleModel("deep")), want: []Event{modelEv("s2", false, sampleModel("deep"))}, check: wanted("deep", "deep")},
		{name: "agent ignored the switch", given: idleModel(chosenEv("s2", "deep")), in: gitInputs(),
			cmd: reportCmd(false, sampleModel("fast")), want: []Event{modelEv("s2", false, sampleModel("fast"))}, check: wanted("", "fast")},
		{name: "V86 resumed on the default keeps the choice", given: idleModel(chosenEv("s2", "deep"), modelEv("s2", false, sampleModel("deep"))), in: gitInputs(),
			cmd: reportCmd(true, sampleModel("fast")), want: []Event{modelEv("s2", true, sampleModel("fast"))}, check: wanted("deep", "fast")},
		{name: "V86 choice no longer offered", given: idleModel(chosenEv("s2", "deep"), modelEv("s2", false, sampleModel("deep"))), in: gitInputs(),
			cmd:   reportCmd(true, onlyFast()),
			want:  []Event{modelEv("s2", true, onlyFast()), switchFailedEv("s2", "deep", unavailable)},
			check: wanted("", "fast")},
		{name: "agent stops reporting a model", given: idleModel(chosenEv("s2", "deep")), in: gitInputs(),
			cmd:  reportCmd(true, nil),
			want: []Event{modelEv("s2", true, nil), switchFailedEv("s2", "deep", unavailable)}, check: wanted("", "")},
		{name: "report after exit", given: idleModel(exitedEv("s2")), in: gitInputs(), cmd: reportCmd(false, sampleModel("deep"))},
		{name: "report closed", given: idleModel(closedEv("s2")), in: gitInputs(),
			cmd: reportCmd(false, sampleModel("deep")), reason: ReasonSessionClosed},
		{name: "report from a client", given: idleModel(), in: gitInputs(),
			cmd:    sessionCmd(FactModelReported, "s2", OriginDesktop, func(c *Command) { c.ModelReported = &ModelReport{Setting: sampleModel("deep")} }),
			reason: ReasonForgedOrigin},

		// set_model 失敗（V83）。
		{name: "V83 switch failed", given: idleModel(chosenEv("s2", "deep")), in: gitInputs(),
			cmd: sessionCmd(FactEffectFailed, "s2", OriginRuntime, func(c *Command) {
				c.EffectFailed = &EffectFailure{Effect: "set_model:s2", Problem: failure}
			}),
			want: []Event{switchFailedEv("s2", "deep", failure)}, check: wanted("", "fast")},
		{name: "switch failed after the choice was dropped", given: idleModel(), in: gitInputs(),
			cmd: sessionCmd(FactEffectFailed, "s2", OriginRuntime, func(c *Command) {
				c.EffectFailed = &EffectFailure{Effect: "set_model:s2", Problem: failure}
			})},
	})
}

// TestModelEventsDoNotTouch：模型事件不改 Task 的更新時間（接回時的報告不讓舊 Task 跑到前面）。
func TestModelEventsDoNotTouch(t *testing.T) {
	for _, k := range []EventKind{ModelChosenKind, ModelReportedKind, ModelSwitchFailedKind} {
		if touches(k) {
			t.Errorf("%s touches the task", k)
		}
	}
}

// TestPlanModel：Plan 何時去設模型（V80、V85、V86，D118）。
func TestPlanModel(t *testing.T) {
	setDeep := Effect{Key: "set_model:s2", Kind: EffSetModel, Session: "s2", SetModel: &ModelSwitch{Option: "model", Value: "deep"}}
	withConn := func(turn TurnID) func(*Live) {
		return func(l *Live) { l.Conns["s2"] = ConnInfo{NativeID: "n2", Turn: turn} }
	}
	turn := func(more ...Event) []Event {
		return idleModel(append(more, queuedEv("s2", "p8", "go"), turnEv("s2", "t9", "p8"))...)
	}
	runPlan(t, []struct {
		name  string
		given []Event
		live  func(*Live)
		want  []Effect
	}{
		{name: "M1 V80 idle with a conn switches", given: idleModel(chosenEv("s2", "deep")), live: withConn(""),
			want: []Effect{setDeep}},
		{name: "M2 already on the chosen model", given: idleModel(chosenEv("s2", "fast")), live: withConn(""), want: nil},
		{name: "M3 switch in flight", given: idleModel(chosenEv("s2", "deep")),
			live: func(l *Live) { withConn("")(l); l.InFlight["set_model:s2"] = true }, want: nil},
		{name: "M4 V86 a new turn waits for the switch", given: turn(chosenEv("s2", "deep")), live: withConn(""),
			want: []Effect{setDeep}},
		{name: "M5 a new turn on the chosen model sends", given: turn(chosenEv("s2", "fast")), live: withConn(""),
			want: []Effect{{Key: "send_prompt:t9", Kind: EffSendPrompt, Session: "s2", SendPrompt: &Prompt{ID: "p8", Text: "go"}}}},
		{name: "M6 never switches a turn already sent", given: turn(chosenEv("s2", "deep")), live: withConn("t9"), want: nil},
		{name: "M7 V85 lost idle resumes to switch", given: idleModel(chosenEv("s2", "deep")),
			live: func(l *Live) { l.Lost["s2"] = "" },
			want: []Effect{{Key: "start_conn:s2", Kind: EffStartConn, Session: "s2",
				StartConn: &StartConn{Launch: newLaunch(), Workdir: "/ws", Resume: "n2"}}}},
		{name: "M8 lost idle waits for the old conn to close", given: idleModel(chosenEv("s2", "deep")),
			live: func(l *Live) { l.Lost["s2"] = ""; l.InFlight["close_conn:s2"] = true }, want: nil},
		{name: "M9 not lost and no conn: starting already", given: idleModel(chosenEv("s2", "deep")), want: nil},
		{name: "M10 exited never switches", given: idleModel(chosenEv("s2", "deep"), exitedEv("s2")), live: withConn(""),
			want: []Effect{{Key: "close_conn:s2", Kind: EffCloseConn, Session: "s2"}}},
		{name: "M11 cancelling only cancels", given: and(turn(chosenEv("s2", "deep")), cancelReqEv("s2")), live: withConn(""),
			want: []Effect{{Key: "cancel_turn:t9", Kind: EffCancelTurn, Session: "s2"}}},
		{name: "M12 a turn without a conn starts first", given: and(seqs(created(IsolationNone), sessionEv("s2"), modelEv("s2", true, sampleModel("fast")),
			chosenEv("s2", "deep"), queuedEv("s2", "p6", "go"), turnEv("s2", "t7", "p6"))),
			want: []Effect{{Key: "start_conn:s2", Kind: EffStartConn, Session: "s2", StartConn: &StartConn{Launch: newLaunch(), Workdir: "/ws"}}}},
		{name: "M13 switch failed: the turn goes ahead", given: turn(chosenEv("s2", "deep"), switchFailedEv("s2", "deep", Problem{Code: "acp.request_failed"})),
			live: withConn(""),
			want: []Effect{{Key: "send_prompt:t9", Kind: EffSendPrompt, Session: "s2", SendPrompt: &Prompt{ID: "p8", Text: "go"}}}},
	})
}

// defaultLaunch 是預設模型為 v 的 Launch（settings.md MD05）。
func defaultLaunch(v string) LaunchSnapshot {
	l := newLaunch()
	l.Model = v
	return l
}

// TestDecideDefaultModel 是 MD05–MD08、MD10、MD12：新會話第一次啟動的 Start 報告時換成預設模型。
func TestDecideDefaultModel(t *testing.T) {
	gone := Problem{Code: CodeDefaultModelUnavailable}
	fresh := func(v string, more ...Event) []Event {
		return and(seqs(created(IsolationNone), sessionLaunchEv("s2", defaultLaunch(v))), more...)
	}
	runDecide(t, []decideCase{
		{name: "MD05 default offered and not current", given: fresh("deep"), in: gitInputs(),
			cmd:  reportCmd(true, sampleModel("fast")),
			want: []Event{modelEv("s2", true, sampleModel("fast")), chosenEv("s2", "deep")}, check: wanted("deep", "fast")},
		{name: "MD06 default already current", given: fresh("fast"), in: gitInputs(),
			cmd: reportCmd(true, sampleModel("fast")), want: []Event{modelEv("s2", true, sampleModel("fast"))}, check: wanted("", "fast")},
		{name: "MD07 default not offered", given: fresh("huge"), in: gitInputs(),
			cmd:  reportCmd(true, sampleModel("fast")),
			want: []Event{modelEv("s2", true, sampleModel("fast")), switchFailedEv("s2", "huge", gone)}, check: wanted("", "fast")},
		{name: "MD08 agent reports no model", given: fresh("deep"), in: gitInputs(), cmd: reportCmd(true, nil)},
		{name: "no default", given: seqs(created(IsolationNone), sessionEv("s2")), in: gitInputs(),
			cmd: reportCmd(true, sampleModel("fast")), want: []Event{modelEv("s2", true, sampleModel("fast"))}, check: wanted("", "fast")},
		{name: "MD10 later reports are not starts", given: fresh("deep", modelEv("s2", true, sampleModel("fast")), chosenEv("s2", "deep"),
			modelEv("s2", false, sampleModel("deep")), boundEv("s2", "n2"), chosenEv("s2", "fast"), modelEv("s2", false, sampleModel("fast"))), in: gitInputs(),
			cmd: reportCmd(false, sampleModel("deep")), want: []Event{modelEv("s2", false, sampleModel("deep"))}, check: wanted("", "deep")},
		{name: "MD12 resume keeps the agent's model", given: fresh("deep", boundEv("s2", "n2"), exitedEv("s2"), resumedEv("s2")), in: gitInputs(),
			cmd: reportCmd(true, sampleModel("fast")), want: []Event{modelEv("s2", true, sampleModel("fast"))}, check: wanted("", "fast")},
	})
}
