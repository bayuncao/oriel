package task

import "time"

var at0 = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

func newLaunch() LaunchSnapshot {
	return LaunchSnapshot{
		Agent: "claude", AgentName: "Claude Code", Protocol: ProtocolClaude, Profile: "default", ProfileName: "default",
		Executable: "/opt/homebrew/bin/claude", Args: []string{"-p"}, Approval: ApprovalOrielHub,
		Caps: Capabilities{Resume: true},
	}
}

// fixture 回傳一個什麼都有的 State，每次呼叫都是全新的值（不共用任何
// map、slice 或指針），所以 Evolve 之後可以與另一次呼叫的結果比較。
func fixture() State {
	return State{
		ID: "t-1", Version: 30, Title: "Login page", Brief: "Build it", Workspace: "w1",
		Isolation: IsolationWorktree, Status: TaskInProgress,
		Worktree: &WorktreeState{Worktree: Worktree{Path: "/wt/t-1", Branch: "orielhub/t-1", BaseBranch: "main", BaseCommit: "abc"}, Outcome: OutcomeOpen},
		Sessions: map[SessionID]Session{
			"s2": {
				ID: "s2", Launch: newLaunch(), Role: RoleWorker, Status: SessionRunning, NativeID: "n2", Bound: true,
				Fork:        &WorktreeState{Worktree: Worktree{Path: "/wt/t-1-s2", Branch: "orielhub/t-1-s2", BaseBranch: "orielhub/t-1", BaseCommit: "def"}, Outcome: OutcomeOpen},
				Turn:        &Turn{ID: "t5", Prompt: Prompt{ID: "p4"}, Reply: draft("m6", "partial ")},
				Open:        "m6",
				OpenChannel: ChannelText,
				Queue:       append(make([]Prompt, 0, 4), Prompt{ID: "p7", Text: "next"}, Prompt{ID: "p8", Text: "later"}), // 有空餘容量：append 不複製就會寫進共用的底層陣列
				Pending: map[PermissionID]Ask{
					"a1": {ID: "a1", Turn: "t5", Request: PermissionRequest{Tool: "Bash", Kind: ToolExecute, Paths: []string{"x"}}, AskedAt: at0},
				},
				CompletedTurns: 1,
			},
			// LastTurn 的正文與 s2 進行中的一輪用同一個訊息 ID，只為讓 MessageRedacted 的兩個分支都跑到。
			"s3": {ID: "s3", Launch: newLaunch(), Role: RoleWorker, Status: SessionExited,
				Exit:     &Exited{Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}},
				LastTurn: &TurnRecord{Turn: "t2", Result: TurnOK, EndedAt: at0, Reply: &Reply{Message: "m6", Text: "old"}}},
		},
		Order:         append(make([]SessionID, 0, 4), "s2", "s3"),
		Rules:         map[RuleID]Rule{"r9": {ID: "r9", Scope: ScopeTask, Match: MatchPrefix, Pattern: "go test", Verdict: Allow}},
		Confirmations: map[ConfirmationID]Confirmation{"c10": {ID: "c10", Command: Command{Kind: CmdArchive}, RequestedBy: "d1", RequestedAt: at0}},
		Intents: map[IntentID]Intent{
			"i11": {ID: "i11", Kind: IntentMerge, Target: Target{Level: TargetFork, Session: "s2"}, Strategy: MergeSquash, Effect: "merge_worktree:i11"},
			"i12": {ID: "i12", Kind: IntentDiscard, Target: Target{Level: TargetWorktree}, Effect: "discard_worktree:i12"},
			"i13": {ID: "i13", Kind: IntentDeleteNative, Target: Target{Session: "s3"}, Effect: "delete_native:i13"},
		},
		Review: &Review{Launch: new(newLaunch()), Candidates: []SessionID{"s2", "s3"}},
		Failed: map[EffectKey]Suspension{
			"create_worktree:s3":   {Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "128"}}, At: at0},
			"discard_worktree:i12": {Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}, At: at0},
			"prepare_review":       {Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}, At: at0},
		},
	}
}

// sampleModel 是 agent 報告的模型設置：fast、deep 兩個，current 是目前的。
func sampleModel(current string) *ModelSetting {
	return &ModelSetting{Option: "model", Current: current, Choices: []ModelChoice{
		{Value: "fast", Name: "Fast", Description: "Short answers"},
		{Value: "deep", Name: "Deep", Group: "Reasoning"},
	}}
}

// samples 是每個 Kind 一個事件，引用 fixture 中的實體；TestSamplesCoverKinds
// 確認沒有遺漏。
func samples() []Event {
	ev := func(kind EventKind, session SessionID, set func(*Event)) Event {
		e := Event{Seq: 31, At: at0, Kind: kind, Session: session}
		if set != nil {
			set(&e)
		}
		return e
	}
	return []Event{
		ev(TaskCreatedKind, "", func(e *Event) {
			e.TaskCreated = &TaskCreated{Title: "New", Brief: "b", Workspace: "w2", Isolation: IsolationNone}
		}),
		ev(TaskEditedKind, "", func(e *Event) { e.TaskEdited = &EditTask{Title: "Renamed", Brief: "b2"} }),
		ev(TaskStatusChangedKind, "", func(e *Event) {
			e.TaskStatusChanged = &TaskStatusChanged{From: TaskInProgress, To: TaskInReview, Cause: CauseAuto}
		}),
		ev(TaskArchivedKind, "", nil),
		ev(TaskUnarchivedKind, "", nil),
		ev(TaskPinnedKind, "", nil),
		ev(TaskUnpinnedKind, "", nil),
		ev(TaskPrioritySetKind, "", func(e *Event) { e.TaskPrioritySet = &SetPriority{Priority: PriorityHigh} }),
		ev(TaskLabelsSetKind, "", func(e *Event) { e.TaskLabelsSet = &SetLabels{Labels: []LabelID{"l-a", "l-b"}} }),
		ev(MessageRedactedKind, "", func(e *Event) { e.MessageRedacted = &RedactMessage{Message: "m6"} }),
		ev(NativeSessionDeletedKind, "s3", func(e *Event) {
			e.NativeSessionDeleted = &Intent{ID: "i31", Kind: IntentDeleteNative, Target: Target{Session: "s3"}}
		}),
		ev(NativeFilesRemovedKind, "s3", func(e *Event) { e.NativeFilesRemoved = &IntentRef{Intent: "i13"} }),
		ev(ConfirmationRequestedKind, "", func(e *Event) {
			e.ConfirmationRequested = &Confirmation{ID: "c31", Command: Command{Kind: CmdMerge, Merge: &Merge{Target: Target{Level: TargetWorktree}}}, RequestedBy: "d1", RequestedAt: at0}
		}),
		ev(ConfirmationGrantedKind, "", func(e *Event) { e.ConfirmationGranted = &ConfirmationRef{Confirmation: "c10"} }),
		ev(ConfirmationCancelledKind, "", func(e *Event) { e.ConfirmationCancelled = &ConfirmationRef{Confirmation: "c10"} }),
		ev(ReviewRequestedKind, "", func(e *Event) {
			e.ReviewRequested = &ReviewRequested{Candidates: []SessionID{"s2"}, Launch: new(newLaunch())}
		}),
		ev(ForkRecommendedKind, "", func(e *Event) { e.ForkRecommended = &Recommendation{Winner: "s2", Rationale: "tests pass"} }),
		ev(ReviewSkippedKind, "", nil),
		ev(ReviewEndedKind, "", nil),
		ev(SessionCreatedKind, "s31", func(e *Event) { e.SessionCreated = &SessionCreated{Launch: newLaunch(), Role: RoleReviewer} }),
		ev(NativeSessionBoundKind, "s3", func(e *Event) { e.NativeSessionBound = &NativeBound{NativeID: "n3"} }),
		ev(SessionResumedKind, "s3", func(e *Event) { e.SessionResumed = &SessionResumed{} }),
		ev(SessionClosedKind, "s3", nil),
		ev(ProcessExitedKind, "s2", func(e *Event) {
			e.ProcessExited = &Exited{Problem: Problem{Code: "proc.signaled", Params: map[string]string{"signal": "9"}}, StderrTail: "boom"}
		}),
		ev(ModelChosenKind, "s2", func(e *Event) { e.ModelChosen = &SetModel{Value: "fast"} }),
		ev(ModelReportedKind, "s2", func(e *Event) { e.ModelReported = &ModelReport{Setting: sampleModel("deep")} }),
		ev(ModelSwitchFailedKind, "s2", func(e *Event) {
			e.ModelSwitchFailed = &ModelProblem{Value: "fast", Problem: Problem{Code: "acp.request_failed"}}
		}),
		ev(PromptQueuedKind, "s2", func(e *Event) { e.PromptQueued = &Prompt{ID: "p31", Text: "more"} }),
		ev(TurnStartedKind, "s3", func(e *Event) { e.TurnStarted = &TurnStarted{Turn: "t31", Prompt: "p30"} }),
		ev(TurnCancelRequestedKind, "s2", nil),
		ev(TurnCompletedKind, "s2", nil),
		ev(TurnCancelledKind, "s2", nil),
		ev(TurnFailedKind, "s2", func(e *Event) { e.TurnFailed = &TurnFailed{Turn: "t5", Problem: Problem{Code: "proc.exited"}} }),
		ev(MessageAppendedKind, "s2", func(e *Event) {
			e.MessageAppended = &MessageAppended{Message: "m6", Turn: "t5", Channel: ChannelText, Text: "done"}
		}),
		ev(ToolCallStartedKind, "s2", func(e *Event) {
			e.ToolCallStarted = &ToolCall{ID: "toolu_1", Tool: "Bash", Kind: ToolExecute, Title: "go test", Paths: []string{"."}}
		}),
		ev(ToolCallUpdatedKind, "s2", func(e *Event) { e.ToolCallUpdated = &ToolUpdate{ID: "toolu_1", Status: ToolCompleted, Output: "ok"} }),
		ev(PlanUpdatedKind, "s2", func(e *Event) {
			e.PlanUpdated = &PlanUpdate{Entries: []PlanEntry{{Text: "write", Status: PlanInProgress}}}
		}),
		ev(UsageReportedKind, "s2", func(e *Event) { e.UsageReported = &Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.01} }),
		ev(PermissionAskedKind, "s2", func(e *Event) {
			e.PermissionAsked = &Ask{ID: "a2", Turn: "t5", Request: PermissionRequest{Tool: "Edit", Kind: ToolEdit, Paths: []string{"a.go"}}, AskedAt: at0}
		}),
		ev(PermissionDecidedKind, "s2", func(e *Event) {
			e.PermissionDecided = &PermissionDecided{Permission: "a1", Decision: Decision{Verdict: Allow, Source: DecidedByUser, Origin: OriginDesktop}}
		}),
		ev(PermissionAnsweredKind, "s2", func(e *Event) { e.PermissionAnswered = &PermissionRef{Permission: "a1"} }),
		ev(PermissionCancelledKind, "s2", func(e *Event) { e.PermissionCancelled = &PermissionRef{Permission: "a1"} }),
		ev(RuleAddedKind, "", func(e *Event) {
			e.RuleAdded = &Rule{ID: "r31", Scope: ScopeSession, ScopeID: "s2", Match: MatchTool, Tool: "Read", Verdict: Allow}
		}),
		ev(WorktreeAttachedKind, "", func(e *Event) {
			e.WorktreeAttached = &Worktree{Path: "/wt/t-2", Branch: "orielhub/t-2", BaseBranch: "main", BaseCommit: "123"}
		}),
		ev(ForkCreatedKind, "s3", func(e *Event) {
			e.ForkCreated = &Worktree{Path: "/wt/t-1-s3", Branch: "orielhub/t-1-s3", BaseBranch: "orielhub/t-1", BaseCommit: "def"}
		}),
		ev(MergeRequestedKind, "", func(e *Event) {
			e.MergeRequested = &Intent{ID: "i31", Kind: IntentMerge, Target: Target{Level: TargetWorktree}, Strategy: MergePullRequest, Message: "Login"}
		}),
		ev(DiscardRequestedKind, "", func(e *Event) {
			e.DiscardRequested = &Intent{ID: "i31", Kind: IntentDiscard, Target: Target{Level: TargetFork, Session: "s3"}}
		}),
		ev(IntentCancelledKind, "", func(e *Event) { e.IntentCancelled = &IntentRef{Intent: "i12"} }),
		ev(ForkMergedKind, "s2", func(e *Event) { e.Merged = &IntentResult{Intent: "i11"} }),
		ev(ForkDiscardedKind, "s2", func(e *Event) { e.Discarded = &IntentRef{Intent: "i11"} }),
		ev(WorktreeMergedKind, "", func(e *Event) { e.Merged = &IntentResult{Intent: "i12", URL: "https://example.test/pr/1"} }),
		ev(WorktreeDiscardedKind, "", func(e *Event) { e.Discarded = &IntentRef{Intent: "i12"} }),
		ev(MergeConflictedKind, "", func(e *Event) { e.MergeConflicted = &MergeConflict{Intent: "i11", Files: []string{"a.go"}} }),
		ev(WorktreeKeptKind, "", func(e *Event) { e.WorktreeKept = &Target{Level: TargetFork, Session: "s2"} }),
		ev(SessionConflictedKind, "s2", nil),
		ev(SessionTakenOverKind, "s2", nil),
		ev(EffectFailedKind, "", func(e *Event) {
			e.EffectFailed = &EffectFailure{Effect: "merge_worktree:i11", Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}}
		}),
		ev(EffectRetriedKind, "", func(e *Event) { e.EffectRetried = &RetryEffect{Effect: "create_worktree:s3"} }),
	}
}

// draft 是依序收到 parts 這幾段的同一條正文（與折疊 MessageAppended 走同一條路）。
func draft(m MessageID, parts ...string) *ReplyDraft {
	var d *ReplyDraft
	for _, p := range parts {
		d = appendReply(d, &MessageAppended{Message: m, Text: p})
	}
	return d
}
