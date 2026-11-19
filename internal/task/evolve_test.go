package task

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestFoldsCoverKinds(t *testing.T) {
	for k := range payloads {
		if _, ok := folds[k]; !ok {
			t.Errorf("no fold for %s", k)
		}
	}
	if len(folds) != len(payloads) {
		t.Errorf("folds has %d kinds, payloads %d", len(folds), len(payloads))
	}
}

// TestEvolveDoesNotMutate：每種事件摺疊後，傳入的 State 與一份全新的
// fixture 完全相同（docs/architecture/domain.md 的 copy-on-write 規則）。
func TestEvolveDoesNotMutate(t *testing.T) {
	for _, e := range samples() {
		t.Run(string(e.Kind), func(t *testing.T) {
			s := fixture()
			_ = Evolve(s, e)
			if !reflect.DeepEqual(s, fixture()) {
				t.Fatalf("Evolve changed its input:\n%+v", s)
			}
		})
	}
}

// TestEvolveAppendsDoNotAlias：從同一個 State 以兩個不同事件 append 出的
// 結果互不影響。TestEvolveDoesNotMutate 抓不到這種錯：append 寫進共用
// 底層陣列的空餘容量時輸入的長度不變，要第二次 append 才會覆蓋第一個結果。
func TestEvolveAppendsDoNotAlias(t *testing.T) {
	queued := func(id PromptID) Event {
		return Event{Seq: 31, Kind: PromptQueuedKind, Session: "s2", PromptQueued: &Prompt{ID: id}}
	}
	created := func(id SessionID) Event {
		return Event{Seq: 31, Kind: SessionCreatedKind, Session: id, SessionCreated: &SessionCreated{Role: RoleWorker}}
	}
	s := fixture()
	a, b := Evolve(s, queued("p31")), Evolve(s, queued("p32"))
	if q := a.Sessions["s2"].Queue; q[len(q)-1].ID != "p31" || b.Sessions["s2"].Queue[2].ID != "p32" {
		t.Errorf("queues share an array: %v", q)
	}
	a, b = Evolve(s, created("s31")), Evolve(s, created("s32"))
	if a.Order[2] != "s31" || b.Order[2] != "s32" {
		t.Errorf("orders share an array: %v", a.Order)
	}
}

// TestEvolveOnEmptyState：沒有任何實體的 State 也能摺疊每種事件
// （nil map 不會 panic，引用不存在的 Session 時不改變它）。
func TestEvolveOnEmptyState(t *testing.T) {
	for _, e := range samples() {
		if got := Evolve(State{ID: "t-1"}, e); got.Version != e.Seq {
			t.Errorf("%s: Version = %d", e.Kind, got.Version)
		}
	}
}

func TestEvolveUnknownKindPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	Evolve(State{}, Event{Kind: "Nope"})
}

// TestEvolve 逐一規定每種事件對 fixture 的影響：want 從一份全新的
// fixture 出發，寫出預期的改變（Version 一律變為事件的 Seq）。
func TestEvolve(t *testing.T) {
	session := func(s *State, id SessionID, f func(*Session)) {
		x := s.Sessions[id]
		f(&x)
		s.Sessions[id] = x
	}
	want := map[EventKind]func(*State){
		TaskCreatedKind: func(s *State) {
			s.Title, s.Brief, s.Workspace, s.Isolation, s.Status, s.Priority = "New", "b", "w2", IsolationNone, TaskTodo, PriorityNone
		},
		TaskEditedKind:        func(s *State) { s.Title, s.Brief = "Renamed", "b2" },
		TaskStatusChangedKind: func(s *State) { s.Status = TaskInReview },
		TaskArchivedKind:      func(s *State) { s.Archived = true },
		TaskUnarchivedKind:    func(s *State) {},
		TaskPinnedKind:        func(s *State) { s.PinnedAt = at0 },
		TaskUnpinnedKind:      func(s *State) {},
		TaskPrioritySetKind:   func(s *State) { s.Priority = PriorityHigh },
		TaskLabelsSetKind:     func(s *State) { s.Labels = []LabelID{"l-a", "l-b"} },
		MessageRedactedKind: func(s *State) {
			session(s, "s2", func(x *Session) { x.Turn.Reply = &ReplyDraft{Message: "m6", Redacted: true} })
			session(s, "s3", func(x *Session) { x.LastTurn.Reply = &Reply{Message: "m6", Redacted: true} })
		},
		NativeSessionDeletedKind: func(s *State) {
			session(s, "s3", func(x *Session) { x.NativeGone = true })
			s.Intents["i31"] = Intent{ID: "i31", Kind: IntentDeleteNative, Target: Target{Session: "s3"}}
		},
		NativeFilesRemovedKind: func(s *State) { delete(s.Intents, "i13") },
		ConfirmationRequestedKind: func(s *State) {
			s.Confirmations["c31"] = Confirmation{ID: "c31", Command: Command{Kind: CmdMerge, Merge: &Merge{Target: Target{Level: TargetWorktree}}}, RequestedBy: "d1", RequestedAt: at0}
		},
		ConfirmationGrantedKind:   func(s *State) { delete(s.Confirmations, "c10") },
		ConfirmationCancelledKind: func(s *State) { delete(s.Confirmations, "c10") },
		ReviewRequestedKind: func(s *State) {
			s.Review = &Review{Launch: new(newLaunch()), Candidates: []SessionID{"s2"}}
		},
		ForkRecommendedKind: func(s *State) { s.Review.Recommended, s.Review.Rationale = "s2", "tests pass" },
		ReviewSkippedKind: func(s *State) {
			s.Review.Launch = nil
			delete(s.Failed, "prepare_review")
		},
		ReviewEndedKind: func(s *State) { s.Review = nil },
		SessionCreatedKind: func(s *State) {
			s.Sessions["s31"] = Session{ID: "s31", Launch: newLaunch(), Role: RoleReviewer, Status: SessionIdle}
			s.Order = append(s.Order, "s31")
			s.Review.Reviewer = "s31"
		},
		NativeSessionBoundKind: func(s *State) {
			session(s, "s3", func(x *Session) { x.NativeID, x.Bound = "n3", true })
		},
		SessionResumedKind: func(s *State) { session(s, "s3", func(x *Session) { x.Status, x.Exit = SessionIdle, nil }) },
		SessionClosedKind:  func(s *State) { session(s, "s3", func(x *Session) { x.Status = SessionClosed }) },
		ProcessExitedKind: func(s *State) {
			session(s, "s2", func(x *Session) {
				x.Status, x.Open, x.OpenChannel, x.Bound = SessionExited, "", "", false
				x.Exit = &Exited{Problem: Problem{Code: "proc.signaled", Params: map[string]string{"signal": "9"}}, StderrTail: "boom"}
			})
		},
		ModelChosenKind:   func(s *State) { session(s, "s2", func(x *Session) { x.ModelWanted = "fast" }) },
		ModelReportedKind: func(s *State) { session(s, "s2", func(x *Session) { x.Model = sampleModel("deep") }) },
		ModelSwitchFailedKind: func(s *State) {
			session(s, "s2", func(x *Session) {
				x.ModelProblem = &ModelProblem{Value: "fast", Problem: Problem{Code: "acp.request_failed"}}
			})
		},
		PromptQueuedKind: func(s *State) {
			session(s, "s2", func(x *Session) { x.Queue = append(x.Queue, Prompt{ID: "p31", Text: "more"}) })
		},
		TurnStartedKind: func(s *State) {
			session(s, "s3", func(x *Session) { x.Turn, x.Status = &Turn{ID: "t31", Prompt: Prompt{ID: "p30"}}, SessionRunning })
		},
		TurnCancelRequestedKind: func(s *State) { session(s, "s2", func(x *Session) { x.Turn.Cancelling = true }) },
		TurnCompletedKind: func(s *State) {
			session(s, "s2", func(x *Session) {
				x.LastTurn = &TurnRecord{Turn: "t5", Result: TurnOK, EndedAt: at0, Reply: &Reply{Message: "m6", Text: "partial "}}
				x.Turn, x.Open, x.OpenChannel, x.Status, x.CompletedTurns = nil, "", "", SessionIdle, 2
			})
		},
		TurnCancelledKind: func(s *State) {
			session(s, "s2", func(x *Session) {
				x.LastTurn = &TurnRecord{Turn: "t5", Result: TurnCancelled, EndedAt: at0, Reply: &Reply{Message: "m6", Text: "partial "}}
				x.Turn, x.Open, x.OpenChannel, x.Status = nil, "", "", SessionIdle
			})
		},
		TurnFailedKind: func(s *State) {
			session(s, "s2", func(x *Session) {
				x.LastTurn = &TurnRecord{Turn: "t5", Result: TurnError, Problem: &Problem{Code: "proc.exited"}, EndedAt: at0,
					Reply: &Reply{Message: "m6", Text: "partial "}}
				x.Turn, x.Open, x.OpenChannel, x.Status = nil, "", "", SessionIdle
			})
		},
		MessageAppendedKind: func(s *State) {
			session(s, "s2", func(x *Session) {
				x.Open, x.OpenChannel = "", ""
				x.Turn.Reply = draft("m6", "partial ", "done")
			})
		},
		ToolCallStartedKind: func(s *State) {
			session(s, "s2", func(x *Session) {
				x.Turn.Tools = map[ToolCallID]ToolRef{"toolu_1": {Kind: ToolExecute, Paths: []string{"."}}}
			})
		},
		ToolCallUpdatedKind: func(s *State) {}, // 沒帶分類與路徑：不變
		PlanUpdatedKind:     func(s *State) {},
		UsageReportedKind:   func(s *State) {},
		PermissionAskedKind: func(s *State) {
			session(s, "s2", func(x *Session) {
				x.Pending["a2"] = Ask{ID: "a2", Turn: "t5", Request: PermissionRequest{Tool: "Edit", Kind: ToolEdit, Paths: []string{"a.go"}}, AskedAt: at0}
			})
		},
		PermissionDecidedKind: func(s *State) {
			session(s, "s2", func(x *Session) {
				a := x.Pending["a1"]
				a.Decision = &Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop}
				x.Pending["a1"] = a
			})
		},
		PermissionAnsweredKind:  func(s *State) { session(s, "s2", func(x *Session) { delete(x.Pending, "a1") }) },
		PermissionCancelledKind: func(s *State) { session(s, "s2", func(x *Session) { delete(x.Pending, "a1") }) },
		RuleAddedKind: func(s *State) {
			s.Rules["r31"] = Rule{ID: "r31", Scope: ScopeSession, ScopeID: "s2", Match: MatchTool, Tool: "Read", Verdict: Allow}
		},
		WorktreeAttachedKind: func(s *State) {
			s.Worktree = &WorktreeState{Worktree: Worktree{Path: "/wt/t-2", Branch: "orielhub/t-2", BaseBranch: "main", BaseCommit: "123"}, Outcome: OutcomeOpen}
		},
		ForkCreatedKind: func(s *State) {
			session(s, "s3", func(x *Session) {
				x.Fork = &WorktreeState{Worktree: Worktree{Path: "/wt/t-1-s3", Branch: "orielhub/t-1-s3", BaseBranch: "orielhub/t-1", BaseCommit: "def"}, Outcome: OutcomeOpen}
			})
		},
		MergeRequestedKind: func(s *State) {
			s.Intents["i31"] = Intent{ID: "i31", Kind: IntentMerge, Target: Target{Level: TargetWorktree}, Strategy: MergePullRequest, Message: "Login"}
		},
		DiscardRequestedKind: func(s *State) {
			s.Intents["i31"] = Intent{ID: "i31", Kind: IntentDiscard, Target: Target{Level: TargetFork, Session: "s3"}}
		},
		IntentCancelledKind: func(s *State) {
			delete(s.Intents, "i12")
			delete(s.Failed, "discard_worktree:i12")
		},
		ForkMergedKind: func(s *State) {
			delete(s.Intents, "i11")
			session(s, "s2", func(x *Session) { x.Fork.Outcome = OutcomeMerged })
		},
		ForkDiscardedKind: func(s *State) {
			delete(s.Intents, "i11")
			session(s, "s2", func(x *Session) { x.Fork.Outcome = OutcomeDiscarded })
		},
		WorktreeMergedKind: func(s *State) {
			delete(s.Intents, "i12")
			s.Worktree.Outcome = OutcomeMerged
		},
		WorktreeDiscardedKind: func(s *State) {
			delete(s.Intents, "i12")
			s.Worktree.Outcome = OutcomeDiscarded
		},
		MergeConflictedKind: func(s *State) {
			in := s.Intents["i11"]
			in.Conflicts = []string{"a.go"}
			s.Intents["i11"] = in
		},
		WorktreeKeptKind:      func(s *State) {},
		SessionConflictedKind: func(s *State) { session(s, "s2", func(x *Session) { x.Conflicted = true }) },
		SessionTakenOverKind:  func(s *State) {},
		EffectFailedKind: func(s *State) {
			s.Failed["merge_worktree:i11"] = Suspension{Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}, At: at0}
		},
		EffectRetriedKind: func(s *State) { delete(s.Failed, "create_worktree:s3") },
	}
	for _, e := range samples() {
		t.Run(string(e.Kind), func(t *testing.T) {
			w := fixture()
			want[e.Kind](&w)
			w.Version = e.Seq
			if touches(e.Kind) {
				w.UpdatedAt = e.At
			}
			if got := Evolve(fixture(), e); !reflect.DeepEqual(got, w) {
				t.Fatalf("got\n%+v\nwant\n%+v", got, w)
			}
		})
	}
	if len(want) != len(payloads) {
		t.Errorf("want covers %d kinds, there are %d", len(want), len(payloads))
	}
}

// TestEvolveScenario 從空的 State 重放一段完整的對話：建立、排隊、
// 分段訊息、審批、完成；並確認重放是確定的。
func TestEvolveScenario(t *testing.T) {
	seq := Seq(0)
	ev := func(kind EventKind, session SessionID, set func(*Event)) Event {
		seq++
		e := Event{Seq: seq, At: at0, Kind: kind, Session: session}
		if set != nil {
			set(&e)
		}
		return e
	}
	events := []Event{
		ev(TaskCreatedKind, "", func(e *Event) { e.TaskCreated = &TaskCreated{Title: "T", Isolation: IsolationNone} }),
		ev(SessionCreatedKind, "s2", func(e *Event) { e.SessionCreated = &SessionCreated{Launch: newLaunch(), Role: RoleWorker} }),
		ev(PromptQueuedKind, "s2", func(e *Event) { e.PromptQueued = &Prompt{ID: "p3", Text: "go"} }),
		ev(TaskStatusChangedKind, "", func(e *Event) {
			e.TaskStatusChanged = &TaskStatusChanged{From: TaskTodo, To: TaskInProgress, Cause: CauseAuto}
		}),
		ev(TurnStartedKind, "s2", func(e *Event) { e.TurnStarted = &TurnStarted{Turn: "t5", Prompt: "p3"} }),
		ev(PromptQueuedKind, "s2", func(e *Event) { e.PromptQueued = &Prompt{ID: "p6", Text: "then"} }),
		ev(MessageAppendedKind, "s2", func(e *Event) {
			e.MessageAppended = &MessageAppended{Message: "m7", Turn: "t5", Channel: ChannelText, Text: "a", Partial: true}
		}),
	}
	s := State{ID: "t-1"}
	for _, e := range events {
		s = Evolve(s, e)
	}
	x := s.Sessions["s2"]
	if x.Status != SessionRunning || x.Turn.ID != "t5" || x.Open != "m7" || len(x.Queue) != 1 || x.Queue[0].ID != "p6" {
		t.Fatalf("mid-turn session %+v", x)
	}
	more := []Event{
		ev(MessageAppendedKind, "s2", func(e *Event) {
			e.MessageAppended = &MessageAppended{Message: "m7", Turn: "t5", Channel: ChannelText, Text: "b"}
		}),
		ev(PermissionAskedKind, "s2", func(e *Event) { e.PermissionAsked = &Ask{ID: "a1", Turn: "t5", AskedAt: at0} }),
		ev(PermissionDecidedKind, "s2", func(e *Event) {
			e.PermissionDecided = &PermissionDecided{Permission: "a1", Decision: Decision{Verdict: Deny, Source: DecidedByUser}}
		}),
		ev(PermissionAnsweredKind, "s2", func(e *Event) { e.PermissionAnswered = &PermissionRef{Permission: "a1"} }),
		ev(TurnCompletedKind, "s2", nil),
	}
	events = append(events, more...)
	for _, e := range more {
		s = Evolve(s, e)
	}
	x = s.Sessions["s2"]
	if x.Status != SessionIdle || x.Turn != nil || x.Open != "" || x.CompletedTurns != 1 || len(x.Pending) != 0 {
		t.Fatalf("finished session %+v", x)
	}
	if s.Version != seq || s.Status != TaskInProgress || !slices.Equal(s.Order, []SessionID{"s2"}) {
		t.Fatalf("state %+v", s)
	}
	replay := State{ID: "t-1"}
	for _, e := range events {
		replay = Evolve(replay, e)
	}
	if !reflect.DeepEqual(replay, s) {
		t.Fatal("replay differs")
	}
	if len(s.Rules) != 0 {
		t.Fatalf("rules %v", s.Rules)
	}
}

// L25–L29、L34（m2.md）：一輪結束時記下結局、時間與最後一條正文；同一條訊息的各段拼起來，
// 另一條取代它，思考不算；遮蔽清掉正文；下一輪開始時上一輪的記錄還在。
func TestEvolveLastTurn(t *testing.T) {
	at1 := at0.Add(time.Minute)
	msg := func(id MessageID, ch ChannelKind, text string, partial bool) Event {
		return Event{At: at0, Kind: MessageAppendedKind, Session: "s1",
			MessageAppended: &MessageAppended{Message: id, Turn: "t1", Channel: ch, Text: text, Partial: partial}}
	}
	start := []Event{
		{At: at0, Kind: TaskCreatedKind, TaskCreated: &TaskCreated{Title: "T", Isolation: IsolationNone}},
		{At: at0, Kind: SessionCreatedKind, Session: "s1", SessionCreated: &SessionCreated{Launch: newLaunch(), Role: RoleWorker}},
		{At: at0, Kind: PromptQueuedKind, Session: "s1", PromptQueued: &Prompt{ID: "p1", Text: "go"}},
		{At: at0, Kind: TurnStartedKind, Session: "s1", TurnStarted: &TurnStarted{Turn: "t1", Prompt: "p1"}},
	}
	completed := Event{At: at1, Kind: TurnCompletedKind, Session: "s1"}
	for _, tc := range []struct {
		name   string
		events []Event
		want   *TurnRecord
	}{
		{"L25 segments of one message join", []Event{msg("m1", ChannelText, "hel", true), msg("m1", ChannelText, "lo", false), completed},
			&TurnRecord{Turn: "t1", Result: TurnOK, EndedAt: at1, Reply: &Reply{Message: "m1", Text: "hello"}}},
		{"L26 the last message wins, thinking does not count", []Event{
			msg("m1", ChannelText, "first", false), msg("m2", ChannelText, "second", false), msg("m3", ChannelThinking, "hmm", false), completed},
			&TurnRecord{Turn: "t1", Result: TurnOK, EndedAt: at1, Reply: &Reply{Message: "m2", Text: "second"}}},
		{"L27 no text", []Event{msg("m1", ChannelThinking, "hmm", false), completed},
			&TurnRecord{Turn: "t1", Result: TurnOK, EndedAt: at1}},
		{"L28 cancelled keeps what arrived", []Event{msg("m1", ChannelText, "half", true), {At: at1, Kind: TurnCancelledKind, Session: "s1"}},
			&TurnRecord{Turn: "t1", Result: TurnCancelled, EndedAt: at1, Reply: &Reply{Message: "m1", Text: "half"}}},
		{"L29 failed with its reason", []Event{msg("m1", ChannelText, "half", false),
			{At: at1, Kind: TurnFailedKind, Session: "s1", TurnFailed: &TurnFailed{Turn: "t1", Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}}}},
			&TurnRecord{Turn: "t1", Result: TurnError, Problem: &Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}, EndedAt: at1,
				Reply: &Reply{Message: "m1", Text: "half"}}},
		{"L34 redacted after the turn", []Event{msg("m1", ChannelText, "secret", false), completed,
			{At: at1, Kind: MessageRedactedKind, MessageRedacted: &RedactMessage{Message: "m1"}}},
			&TurnRecord{Turn: "t1", Result: TurnOK, EndedAt: at1, Reply: &Reply{Message: "m1", Redacted: true}}},
		{"L34 redacted during the turn", []Event{msg("m1", ChannelText, "secret", false),
			{At: at0, Kind: MessageRedactedKind, MessageRedacted: &RedactMessage{Message: "m1"}}, completed},
			&TurnRecord{Turn: "t1", Result: TurnOK, EndedAt: at1, Reply: &Reply{Message: "m1", Redacted: true}}},
		{"L34 replaying a redacted segment", []Event{{At: at0, Kind: MessageAppendedKind, Session: "s1",
			MessageAppended: &MessageAppended{Message: "m1", Turn: "t1", Channel: ChannelText, Redacted: true}}, completed},
			&TurnRecord{Turn: "t1", Result: TurnOK, EndedAt: at1, Reply: &Reply{Message: "m1", Redacted: true}}},
		{"L30 the next turn keeps the last record", []Event{msg("m1", ChannelText, "done", false), completed,
			{At: at1, Kind: PromptQueuedKind, Session: "s1", PromptQueued: &Prompt{ID: "p2", Text: "more"}},
			{At: at1, Kind: TurnStartedKind, Session: "s1", TurnStarted: &TurnStarted{Turn: "t2", Prompt: "p2"}}},
			&TurnRecord{Turn: "t1", Result: TurnOK, EndedAt: at1, Reply: &Reply{Message: "m1", Text: "done"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := State{ID: "t-1"}
			for _, e := range append(append([]Event{}, start...), tc.events...) {
				s = Evolve(s, e)
			}
			if got := s.Sessions["s1"].LastTurn; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("last turn %+v (reply %+v), want %+v (reply %+v)", got, replyOf(got), tc.want, replyOf(tc.want))
			}
		})
	}
}

func replyOf(r *TurnRecord) *Reply {
	if r == nil {
		return nil
	}
	return r.Reply
}
