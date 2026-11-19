package task

import (
	"reflect"
	"testing"
	"time"
)

// baseLive 是預設的觀察值：Workspace 目錄固定、其他都沒有。
func baseLive(f func(*Live)) Live {
	l := Live{WorkspaceDir: "/ws",
		Conns: map[SessionID]ConnInfo{}, Worktrees: map[string]bool{},
		Timers: map[AskKey]bool{}, InFlight: map[EffectKey]bool{}, Lost: map[SessionID]TurnID{}}
	if f != nil {
		f(&l)
	}
	return l
}

// runPlan 摺疊 given、呼叫 Plan，並做逐案的固定檢查。
func runPlan(t *testing.T, cases []struct {
	name  string
	given []Event
	live  func(*Live)
	want  []Effect
}) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fold("t-1", tc.given)
			l := baseLive(tc.live)
			got := Plan(s, l)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			seen := map[EffectKey]bool{}
			for _, e := range got {
				if seen[e.Key] {
					t.Errorf("duplicate key %q", e.Key)
				}
				seen[e.Key] = true
			}
			if again := Plan(s, l); !reflect.DeepEqual(again, got) {
				t.Errorf("not deterministic: %+v", again)
			}
			if again := fold("t-1", tc.given); !reflect.DeepEqual(again, s) {
				t.Errorf("State mutated")
			}
			if again := baseLive(tc.live); !reflect.DeepEqual(again, l) {
				t.Errorf("Live mutated")
			}
		})
	}
}

func TestPlan(t *testing.T) {
	forkS2 := Target{Level: TargetFork, Session: "s2"}
	forkS3 := Target{Level: TargetFork, Session: "s3"}
	worktree := Target{Level: TargetWorktree}
	// 兩個 Fork 停止後已請求評審的串流。
	reviewableDone := func() []Event {
		return seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"),
			forkEv("s2", "main"), forkEv("s3", "main"),
			queuedEv("s2", "p6", "x"), turnEv("s2", "t7", "p6"), completedEv("s2"),
			queuedEv("s3", "p9", "x"), turnEv("s3", "t10", "p9"), completedEv("s3"))
	}
	bothForks := func(l *Live) {
		l.Worktrees["/wt/t-1-s2"] = true
		l.Worktrees["/wt/t-1-s3"] = true
	}
	launchNoResume := func() LaunchSnapshot {
		l := newLaunch()
		l.Caps.Resume = false
		return l
	}
	// 閒著、綁定過原生會話 n2 的 s2。
	idleBound := func(l LaunchSnapshot) []Event {
		return seqs(created(IsolationNone), sessionLaunchEv("s2", l),
			queuedEv("s2", "p3", "x"), turnEv("s2", "t4", "p3"), boundEv("s2", "n2"), completedEv("s2"))
	}
	lostAt := func(turn TurnID) func(*Live) {
		return func(l *Live) { l.Lost["s2"] = turn }
	}
	launchResume := func() LaunchSnapshot {
		l := newLaunch()
		l.Caps.Resume = true
		return l
	}
	runPlan(t, []struct {
		name  string
		given []Event
		live  func(*Live)
		want  []Effect
	}{
		// 連線生命週期。
		{name: "L1 idle no conn",
			given: seqs(created(IsolationNone), sessionEv("s2")),
			want:  nil},
		{name: "L2 start conn no bound",
			given: running(),
			want: []Effect{{Key: "start_conn:s2", Kind: EffStartConn, Session: "s2",
				StartConn: &StartConn{Launch: newLaunch(), Workdir: "/ws"}}}},
		{name: "L3 resume with native id",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchResume()),
				boundEv("s2", "n2"), queuedEv("s2", "p4", "x"), turnEv("s2", "t5", "p4"),
				completedEv("s2"), exitedEv("s2"), resumedEv("s2"),
				turnEv("s2", "t9", "p8")),
			want: []Effect{{Key: "start_conn:s2", Kind: EffStartConn, Session: "s2",
				StartConn: &StartConn{Launch: launchResume(), Workdir: "/ws", Resume: "n2"}}}},
		{name: "L4 bound conn gone reports exited",
			given: and(running(), boundEv("s2", "n2")),
			want:  []Effect{{Key: "report_exited:t4", Kind: EffReportExited, Session: "s2"}}},
		// 綁定的行程不在了（hub 重啟後，Live.Lost；D117，m2.md H07–H10）。
		{name: "L4a lost idle, agent can resume: nothing to record",
			given: and(idleBound(launchResume())),
			live:  lostAt(""),
			want:  nil},
		{name: "L4b lost idle, agent cannot resume, reports exited",
			given: and(idleBound(launchNoResume())),
			live:  lostAt(""),
			want:  []Effect{{Key: "report_exited:s2", Kind: EffReportExited, Session: "s2"}}},
		{name: "L4c lost idle, then a turn resumes the native session",
			given: and(idleBound(launchResume()), queuedEv("s2", "p8", "y"), turnEv("s2", "t9", "p8")),
			live:  lostAt(""),
			want: []Effect{{Key: "start_conn:s2", Kind: EffStartConn, Session: "s2",
				StartConn: &StartConn{Launch: launchResume(), Workdir: "/ws", Resume: "n2"}}}},
		{name: "L4d lost with its turn: that turn reports exited",
			given: and(running(), boundEv("s2", "n2")),
			live:  lostAt("t4"),
			want:  []Effect{{Key: "report_exited:t4", Kind: EffReportExited, Session: "s2"}}},
		{name: "L4e lost idle, agent cannot resume, a turn reports exited",
			given: and(idleBound(launchNoResume()), queuedEv("s2", "p8", "y"), turnEv("s2", "t9", "p8")),
			live:  lostAt(""),
			want:  []Effect{{Key: "report_exited:t9", Kind: EffReportExited, Session: "s2"}}},
		{name: "L4f bound idle with a conn",
			given: and(idleBound(launchResume())),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{NativeID: "n2", Turn: "t4"}; l.Lost["s2"] = "" },
			want:  nil},
		{name: "L4h lost by archiving, a turn waits for the old conn to close",
			given: and(idleBound(launchResume()), queuedEv("s2", "p8", "y"), turnEv("s2", "t9", "p8")),
			live:  func(l *Live) { l.Lost["s2"] = ""; l.InFlight["close_conn:s2"] = true },
			want:  nil},
		{name: "L4g bound idle, not lost: nothing",
			given: and(idleBound(launchNoResume())),
			want:  nil},
		{name: "L5 conn wrong turn sends prompt",
			given: running(),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{} },
			want: []Effect{{Key: "send_prompt:t4", Kind: EffSendPrompt, Session: "s2",
				SendPrompt: &Prompt{ID: "p3", Text: "Build it"}}}},
		{name: "L6 conn same turn",
			given: running(),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{Turn: "t4"} },
			want:  nil},
		{name: "L7 cancelling same turn",
			given: and(running(), cancelReqEv("s2")),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{Turn: "t4"} },
			want:  []Effect{{Key: "cancel_turn:t4", Kind: EffCancelTurn, Session: "s2"}}},
		{name: "L8 cancelling wrong turn only cancels",
			given: and(running(), cancelReqEv("s2")),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{} },
			want:  []Effect{{Key: "cancel_turn:t4", Kind: EffCancelTurn, Session: "s2"}}},
		{name: "L7b cancel already sent for this turn",
			given: and(running(), cancelReqEv("s2")),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{Turn: "t4", Cancelled: "t4"} },
			want:  nil},
		{name: "L8b cancelling while the prompt is being sent waits for it",
			given: and(running(), cancelReqEv("s2")),
			live: func(l *Live) {
				l.Conns["s2"] = ConnInfo{}
				l.InFlight["send_prompt:t4"] = true
			},
			want: nil},
		{name: "L9 closed with conn",
			given: and(running(), closedEv("s2")),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{} },
			want:  []Effect{{Key: "close_conn:s2", Kind: EffCloseConn, Session: "s2"}}},
		{name: "L10 idle with conn",
			given: seqs(created(IsolationNone), sessionEv("s2")),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{} },
			want:  nil},
		{name: "L11 start conn in flight",
			given: running(),
			live:  func(l *Live) { l.InFlight["start_conn:s2"] = true },
			want:  nil},
		{name: "L12 archived only closes conns",
			given: and(running(), mergeEv("i6", forkS2, MergeSquash, "m"), Event{Kind: TaskArchivedKind}),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{} },
			want:  []Effect{{Key: "close_conn:s2", Kind: EffCloseConn, Session: "s2"}}},

		// Ask。
		{name: "A1 decided unanswered answers",
			given: and(running(), askedEv("s2", "a6", "t4"),
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser})),
			live: func(l *Live) { l.Conns["s2"] = ConnInfo{Turn: "t4"} },
			want: []Effect{{Key: "answer_permission:s2/a6", Kind: EffAnswerPermission, Session: "s2",
				AnswerPermission: &AnswerPermission{Permission: "a6", Verdict: Allow}}}},
		{name: "A2 decided but no conn",
			given: and(running(), askedEv("s2", "a6", "t4"),
				decidedEv("s2", "a6", Decision{Verdict: Allow, Source: DecidedByUser})),
			want: []Effect{{Key: "start_conn:s2", Kind: EffStartConn, Session: "s2",
				StartConn: &StartConn{Launch: newLaunch(), Workdir: "/ws"}}}},
		{name: "A3 pending schedules timer",
			given: seqs(created(IsolationNone),
				sessionLaunchEv("s2", launchWithTimeout(30*time.Second, "")),
				queuedEv("s2", "p3", "x"), turnEv("s2", "t4", "p3"), askedEv("s2", "a5", "t4")),
			live: func(l *Live) { l.Conns["s2"] = ConnInfo{Turn: "t4"} },
			want: []Effect{{Key: "schedule_timer:s2/a5", Kind: EffScheduleTimer, Session: "s2",
				ScheduleTimer: &ScheduleTimer{Permission: "a5", At: at0.Add(30 * time.Second)}}}},
		{name: "A4 timer exists",
			given: seqs(created(IsolationNone),
				sessionLaunchEv("s2", launchWithTimeout(30*time.Second, "")),
				queuedEv("s2", "p3", "x"), turnEv("s2", "t4", "p3"), askedEv("s2", "a5", "t4")),
			live: func(l *Live) {
				l.Timers[AskKey{Session: "s2", Permission: "a5"}] = true
				l.Conns["s2"] = ConnInfo{Turn: "t4"}
			},
			want: nil},
		{name: "A4b no timeout",
			given: and(running(), askedEv("s2", "a6", "t4")),
			live:  func(l *Live) { l.Conns["s2"] = ConnInfo{Turn: "t4"} },
			want:  nil},
		{name: "A4c decided schedules nothing",
			given: seqs(created(IsolationNone),
				sessionLaunchEv("s2", launchWithTimeout(30*time.Second, "")),
				queuedEv("s2", "p3", "x"), turnEv("s2", "t4", "p3"), askedEv("s2", "a5", "t4"),
				decidedEv("s2", "a5", Decision{Verdict: Deny, Source: DecidedByUser})),
			live: func(l *Live) { l.Conns["s2"] = ConnInfo{Turn: "t4"} },
			want: []Effect{{Key: "answer_permission:s2/a5", Kind: EffAnswerPermission, Session: "s2",
				AnswerPermission: &AnswerPermission{Permission: "a5", Verdict: Deny}}}},

		// Worktree。
		{name: "W1 task worktree missing",
			given: worktreeTask(),
			want: []Effect{{Key: "create_worktree:task", Kind: EffCreateWorktree,
				CreateWorktree: &CreateWorktree{Repo: "/ws", Worktree: Worktree{
					Path: "/wt/t-1", Branch: "orielhub/t-1", BaseBranch: "main", BaseCommit: "abc"}}}}},
		{name: "W2 running session still only creates",
			given: and(worktreeTask(), sessionEv("s3"), queuedEv("s3", "p4", "x"), turnEv("s3", "t5", "p4")),
			want: []Effect{{Key: "create_worktree:task", Kind: EffCreateWorktree,
				CreateWorktree: &CreateWorktree{Repo: "/ws", Worktree: Worktree{
					Path: "/wt/t-1", Branch: "orielhub/t-1", BaseBranch: "main", BaseCommit: "abc"}}}}},
		{name: "W3 worktree exists starts conn",
			given: and(worktreeTask(), sessionEv("s3"), queuedEv("s3", "p4", "x"), turnEv("s3", "t5", "p4")),
			live:  func(l *Live) { l.Worktrees["/wt/t-1"] = true },
			want: []Effect{{Key: "start_conn:s3", Kind: EffStartConn, Session: "s3",
				StartConn: &StartConn{Launch: newLaunch(), Workdir: "/wt/t-1"}}}},
		{name: "W4 fork missing creates under task worktree",
			given: and(worktreeTask(), sessionEv("s3"), forkEv("s3", "orielhub/t-1"),
				queuedEv("s3", "p5", "x"), turnEv("s3", "t6", "p5")),
			live: func(l *Live) { l.Worktrees["/wt/t-1"] = true },
			want: []Effect{{Key: "create_worktree:s3", Kind: EffCreateWorktree, Session: "s3",
				CreateWorktree: &CreateWorktree{Repo: "/wt/t-1", Worktree: Worktree{
					Path: "/wt/t-1-s3", Branch: "orielhub/t-1-s3", BaseBranch: "orielhub/t-1", BaseCommit: "abc"}}}}},
		{name: "W5 both missing only task",
			given: and(worktreeTask(), sessionEv("s3"), forkEv("s3", "orielhub/t-1"),
				queuedEv("s3", "p5", "x"), turnEv("s3", "t6", "p5")),
			want: []Effect{{Key: "create_worktree:task", Kind: EffCreateWorktree,
				CreateWorktree: &CreateWorktree{Repo: "/ws", Worktree: Worktree{
					Path: "/wt/t-1", Branch: "orielhub/t-1", BaseBranch: "main", BaseCommit: "abc"}}}}},
		{name: "W6 fork exists starts conn",
			given: and(worktreeTask(), sessionEv("s3"), forkEv("s3", "orielhub/t-1"),
				queuedEv("s3", "p5", "x"), turnEv("s3", "t6", "p5")),
			live: func(l *Live) {
				l.Worktrees["/wt/t-1"] = true
				l.Worktrees["/wt/t-1-s3"] = true
			},
			want: []Effect{{Key: "start_conn:s3", Kind: EffStartConn, Session: "s3",
				StartConn: &StartConn{Launch: newLaunch(), Workdir: "/wt/t-1-s3"}}}},
		{name: "W7 create worktree failed",
			given: and(worktreeTask(),
				effectFailedEv("create_worktree:task", Problem{Code: "proc.exited"})),
			want: nil},

		// 意圖。
		{name: "I1 fork merge intent",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "msg")),
			live:  func(l *Live) { l.Worktrees["/wt/t-1-s2"] = true },
			want: []Effect{{Key: "merge_worktree:i4", Kind: EffMergeWorktree, Session: "s2",
				MergeWorktree: &MergeWorktree{Intent: "i4",
					From:     Worktree{Path: "/wt/t-1-s2", Branch: "orielhub/t-1-s2", BaseBranch: "main", BaseCommit: "abc"},
					Into:     "/ws",
					Strategy: MergeSquash, Message: "msg"}}}},
		{name: "I2 fork merge while running",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m"),
				queuedEv("s2", "p5", "x"), turnEv("s2", "t6", "p5")),
			live: func(l *Live) {
				l.Worktrees["/wt/t-1-s2"] = true
				l.Conns["s2"] = ConnInfo{Turn: "t6"}
			},
			want: nil},
		{name: "I3 worktree intent with open fork",
			given: and(worktreeTask(), sessionEv("s3"), forkEv("s3", "orielhub/t-1"),
				mergeEv("i5", worktree, MergeSquash, "m")),
			live: func(l *Live) {
				l.Worktrees["/wt/t-1"] = true
				l.Worktrees["/wt/t-1-s3"] = true
			},
			want: nil},
		{name: "I4 worktree intent forks resolved",
			given: and(worktreeTask(), mergeEv("i3", worktree, MergePullRequest, "m")),
			live:  func(l *Live) { l.Worktrees["/wt/t-1"] = true },
			want: []Effect{{Key: "merge_worktree:i3", Kind: EffMergeWorktree,
				MergeWorktree: &MergeWorktree{Intent: "i3",
					From:     Worktree{Path: "/wt/t-1", Branch: "orielhub/t-1", BaseBranch: "main", BaseCommit: "abc"},
					Into:     "/ws",
					Strategy: MergePullRequest, Message: "m"}}}},
		{name: "I5 discard intent",
			given: and(forked(), discardEv("i4", forkS2)),
			live:  func(l *Live) { l.Worktrees["/wt/t-1-s2"] = true },
			want: []Effect{{Key: "discard_worktree:i4", Kind: EffDiscardWorktree, Session: "s2",
				DiscardWorktree: &DiscardWorktree{Intent: "i4", Worktree: Worktree{
					Path: "/wt/t-1-s2", Branch: "orielhub/t-1-s2", BaseBranch: "main", BaseCommit: "abc"}}}}},
		{name: "I6 suspended intent",
			given: and(forked(), mergeEv("i4", forkS2, MergeSquash, "m"),
				effectFailedEv("merge_worktree:i4", Problem{Code: "proc.exited"})),
			live: func(l *Live) { l.Worktrees["/wt/t-1-s2"] = true },
			want: nil},
		{name: "I7 intents ordered by id",
			given: seqs(created(IsolationNone), sessionEv("s2"), sessionEv("s3"),
				forkEv("s2", "main"), forkEv("s3", "main"),
				discardEv("i9", forkS3), discardEv("i7", forkS2)),
			live: func(l *Live) {
				l.Worktrees["/wt/t-1-s2"] = true
				l.Worktrees["/wt/t-1-s3"] = true
			},
			want: []Effect{
				{Key: "discard_worktree:i7", Kind: EffDiscardWorktree, Session: "s2",
					DiscardWorktree: &DiscardWorktree{Intent: "i7", Worktree: Worktree{
						Path: "/wt/t-1-s2", Branch: "orielhub/t-1-s2", BaseBranch: "main", BaseCommit: "abc"}}},
				{Key: "discard_worktree:i9", Kind: EffDiscardWorktree, Session: "s3",
					DiscardWorktree: &DiscardWorktree{Intent: "i9", Worktree: Worktree{
						Path: "/wt/t-1-s3", Branch: "orielhub/t-1-s3", BaseBranch: "main", BaseCommit: "abc"}}},
			}},
		{name: "I8 delete native no conn",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), closedEv("s2"), nativeDeletedEv("s2", "i5")),
			want: []Effect{{Key: "delete_native:i5", Kind: EffDeleteNative, Session: "s2",
				DeleteNative: &DeleteNative{Intent: "i5", Protocol: newLaunch().Protocol,
					NativeID: "n2", Workdir: "/ws"}}}},
		{name: "I9 delete native with conn only closes",
			given: seqs(created(IsolationNone), sessionLaunchEv("s2", launchDelete()),
				boundEv("s2", "n2"), closedEv("s2"), nativeDeletedEv("s2", "i5")),
			live: func(l *Live) { l.Conns["s2"] = ConnInfo{} },
			want: []Effect{{Key: "close_conn:s2", Kind: EffCloseConn, Session: "s2"}}},

		// 評審。
		{name: "R1 prepare review",
			given: and(reviewableDone(), reviewReqEv([]SessionID{"s2", "s3"}, &LaunchSnapshot{Agent: "claude"})),
			live: func(l *Live) {
				l.Worktrees["/wt/t-1-s2"] = true
				l.Worktrees["/wt/t-1-s3"] = true
			},
			want: []Effect{{Key: "prepare_review", Kind: EffPrepareReview,
				PrepareReview: &PrepareReview{Candidates: []ReviewCandidate{
					{Session: "s2", Worktree: Worktree{Path: "/wt/t-1-s2", Branch: "orielhub/t-1-s2", BaseBranch: "main", BaseCommit: "abc"}},
					{Session: "s3", Worktree: Worktree{Path: "/wt/t-1-s3", Branch: "orielhub/t-1-s3", BaseBranch: "main", BaseCommit: "abc"}},
				}}}}},
		{name: "R2 launch nil no prepare",
			given: and(reviewableDone(), reviewReqEv([]SessionID{"s2", "s3"}, nil)),
			live:  bothForks,
			want:  nil},
		{name: "R2b reviewer set no prepare",
			given: and(reviewableDone(), reviewReqEv([]SessionID{"s2", "s3"}, new(newLaunch())),
				reviewerEv("s13", newLaunch())),
			live: bothForks,
			want: nil},
		{name: "R2c prepare suspended",
			given: and(reviewableDone(), reviewReqEv([]SessionID{"s2", "s3"}, new(newLaunch())),
				effectFailedEv("prepare_review", Problem{Code: "proc.exited"})),
			live: bothForks,
			want: nil},
		{name: "R3 reviewer session starts on task worktree",
			given: and(worktreeTask(), sessionEv("s3"), sessionEv("s4"),
				forkEv("s3", "orielhub/t-1"), forkEv("s4", "orielhub/t-1"),
				reviewReqEv([]SessionID{"s3", "s4"}, new(newLaunch())),
				reviewerEv("s9", newLaunch()),
				queuedEv("s9", "p10", "x"), turnEv("s9", "t11", "p10")),
			live: func(l *Live) {
				l.Worktrees["/wt/t-1"] = true
				l.Worktrees["/wt/t-1-s3"] = true
				l.Worktrees["/wt/t-1-s4"] = true
			},
			want: []Effect{{Key: "start_conn:s9", Kind: EffStartConn, Session: "s9",
				StartConn: &StartConn{Launch: newLaunch(), Workdir: "/wt/t-1"}}}},

		// 排序：Task 級 → Session → 意圖。
		{name: "O1 ordering",
			given: and(worktreeTask(), sessionEv("s3"), sessionEv("s4"),
				queuedEv("s3", "p5", "x"), turnEv("s3", "t6", "p5")),
			want: []Effect{{Key: "create_worktree:task", Kind: EffCreateWorktree,
				CreateWorktree: &CreateWorktree{Repo: "/ws", Worktree: Worktree{
					Path: "/wt/t-1", Branch: "orielhub/t-1", BaseBranch: "main", BaseCommit: "abc"}}}}},
	})
}

// TestPlanTimersPerSession：計時器按 Session 鍵（AskKey）。
func TestPlanTimersPerSession(t *testing.T) {
	lt := launchWithTimeout(30*time.Second, "")
	given := seqs(created(IsolationNone),
		sessionLaunchEv("s2", lt), sessionLaunchEv("s3", lt),
		queuedEv("s2", "p4", "x"), turnEv("s2", "t5", "p4"), askedEv("s2", "a1", "t5"),
		queuedEv("s3", "p6", "x"), turnEv("s3", "t7", "p6"), askedEv("s3", "a1", "t7"))
	s := fold("t-1", given)
	l := baseLive(func(l *Live) {
		l.Conns["s2"] = ConnInfo{Turn: "t5"}
		l.Conns["s3"] = ConnInfo{Turn: "t7"}
		l.Timers[AskKey{Session: "s3", Permission: "a1"}] = true
	})
	want := []Effect{{Key: "schedule_timer:s2/a1", Kind: EffScheduleTimer, Session: "s2",
		ScheduleTimer: &ScheduleTimer{Permission: "a1", At: at0.Add(30 * time.Second)}}}
	if got := Plan(s, l); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
