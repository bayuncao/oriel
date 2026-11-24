package task

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func gitInputs() Inputs {
	return Inputs{
		Now:          at0,
		Repo:         Repo{IsGit: true, Branch: "main", Commit: "abc"},
		Policy:       PolicyAuto,
		WorktreeRoot: "/wt",
	}
}

// inputs 回傳以 gitInputs 修改後的 Inputs。
func inputs(f func(*Inputs)) Inputs {
	in := gitInputs()
	f(&in)
	return in
}

// seqs 為事件依次編號（從 1 開始）並填上 At。
func seqs(events ...Event) []Event {
	for i := range events {
		events[i].Seq = Seq(i + 1)
		events[i].At = at0
	}
	return events
}

// fold 把 given 摺疊成 State{ID: id}，即 Decide 會看到的 State。
func fold(id TaskID, given []Event) State {
	s := State{ID: id}
	for _, e := range given {
		s = Evolve(s, e)
	}
	return s
}

// 事件建構器；每次呼叫回傳全新的值。

func created(iso IsolationMode) Event {
	return Event{Kind: TaskCreatedKind, TaskCreated: &TaskCreated{
		Title: "Login page", Brief: "Build it", Workspace: "w1", Isolation: iso,
	}}
}

func attached() Event {
	return Event{Kind: WorktreeAttachedKind, WorktreeAttached: &Worktree{
		Path: "/wt/t-1", Branch: "orielhub/t-1", BaseBranch: "main", BaseCommit: "abc",
	}}
}

func sessionEv(sid SessionID) Event {
	return sessionLaunchEv(sid, newLaunch())
}

func sessionLaunchEv(sid SessionID, l LaunchSnapshot) Event {
	return Event{Kind: SessionCreatedKind, Session: sid, SessionCreated: &SessionCreated{
		Launch: l, Role: RoleWorker,
	}}
}

func launchNoResume() LaunchSnapshot {
	l := newLaunch()
	l.Caps.Resume = false
	return l
}

func launchWithTimeout(d time.Duration, v Verdict) LaunchSnapshot {
	l := newLaunch()
	l.AskTimeout, l.TimeoutVerdict = d, v
	return l
}

func boundEv(sid SessionID, nid NativeSessionID) Event {
	return Event{Kind: NativeSessionBoundKind, Session: sid,
		NativeSessionBound: &NativeBound{NativeID: nid}}
}

func exitedEv(sid SessionID) Event {
	return Event{Kind: ProcessExitedKind, Session: sid, ProcessExited: &Exited{
		Problem: Problem{Code: "proc.exited", Params: map[string]string{"status": "0"}},
	}}
}

func askedEv(sid SessionID, id PermissionID, turn TurnID) Event {
	return askedReqEv(sid, id, turn, PermissionRequest{Tool: "Bash", Kind: ToolExecute})
}

func askedReqEv(sid SessionID, id PermissionID, turn TurnID, req PermissionRequest) Event {
	return Event{Kind: PermissionAskedKind, Session: sid, PermissionAsked: &Ask{
		ID: id, Turn: turn, Request: req, AskedAt: at0,
	}}
}

func decidedEv(sid SessionID, id PermissionID, dn Decision) Event {
	return Event{Kind: PermissionDecidedKind, Session: sid, PermissionDecided: &PermissionDecided{
		Permission: id, Decision: dn,
	}}
}

func confReqEv(id ConfirmationID, by DeviceID, cmd Command) Event {
	return Event{Kind: ConfirmationRequestedKind, ConfirmationRequested: &Confirmation{
		ID: id, Command: cmd, RequestedBy: by, RequestedAt: at0,
	}}
}

func grantedEv(id ConfirmationID) Event {
	return Event{Kind: ConfirmationGrantedKind, ConfirmationGranted: &ConfirmationRef{Confirmation: id}}
}

func effectFailedEv(key EffectKey, p Problem) Event {
	return Event{Kind: EffectFailedKind, EffectFailed: &EffectFailure{Effect: key, Problem: p}}
}

func retriedEv(key EffectKey) Event {
	return Event{Kind: EffectRetriedKind, EffectRetried: &RetryEffect{Effect: key}}
}

func intentCancelledEv(id IntentID) Event {
	return Event{Kind: IntentCancelledKind, IntentCancelled: &IntentRef{Intent: id}}
}

func completedEv(sid SessionID) Event {
	return Event{Kind: TurnCompletedKind, Session: sid}
}

func cancelledEv(sid SessionID) Event {
	return Event{Kind: TurnCancelledKind, Session: sid}
}

func cancelReqEv(sid SessionID) Event {
	return Event{Kind: TurnCancelRequestedKind, Session: sid}
}

func messageEv(sid SessionID, mid MessageID, tid TurnID, ch ChannelKind, text string, partial bool) Event {
	return Event{Kind: MessageAppendedKind, Session: sid, MessageAppended: &MessageAppended{
		Message: mid, Turn: tid, Channel: ch, Text: text, Partial: partial,
	}}
}

func closedEv(sid SessionID) Event {
	return Event{Kind: SessionClosedKind, Session: sid}
}

func conflictedEv(sid SessionID) Event {
	return Event{Kind: SessionConflictedKind, Session: sid}
}

func resumedEv(sid SessionID) Event {
	return Event{Kind: SessionResumedKind, Session: sid, SessionResumed: &SessionResumed{}}
}

func nativeDeletedEv(sid SessionID, iid IntentID) Event {
	return Event{Kind: NativeSessionDeletedKind, Session: sid, NativeSessionDeleted: &Intent{
		ID: iid, Kind: IntentDeleteNative, Target: Target{Session: sid},
	}}
}

func cancelledPermEv(sid SessionID, id PermissionID) Event {
	return Event{Kind: PermissionCancelledKind, Session: sid,
		PermissionCancelled: &PermissionRef{Permission: id}}
}

func forkEv(sid SessionID, base string) Event {
	return Event{Kind: ForkCreatedKind, Session: sid, ForkCreated: &Worktree{
		Path: "/wt/t-1-" + string(sid), Branch: "orielhub/t-1-" + string(sid),
		BaseBranch: base, BaseCommit: "abc",
	}}
}

func queuedEv(sid SessionID, pid PromptID, text string) Event {
	return Event{Kind: PromptQueuedKind, Session: sid,
		PromptQueued: &Prompt{ID: pid, Text: text}}
}

func turnEv(sid SessionID, tid TurnID, pid PromptID) Event {
	return Event{Kind: TurnStartedKind, Session: sid,
		TurnStarted: &TurnStarted{Turn: tid, Prompt: pid}}
}

func statusEv(from, to TaskStatus, cause StatusCause) Event {
	return Event{Kind: TaskStatusChangedKind, TaskStatusChanged: &TaskStatusChanged{
		From: from, To: to, Cause: cause,
	}}
}

func mergeEv(id IntentID, t Target, strategy MergeStrategy, msg string) Event {
	return Event{Kind: MergeRequestedKind, Session: t.Session, MergeRequested: &Intent{
		ID: id, Kind: IntentMerge, Target: t, Strategy: strategy, Message: msg,
	}}
}

func discardEv(id IntentID, t Target) Event {
	return Event{Kind: DiscardRequestedKind, Session: t.Session, DiscardRequested: &Intent{
		ID: id, Kind: IntentDiscard, Target: t,
	}}
}

// create 回傳一個 CreateTask 命令；f 可修改 payload。
func create(f func(*CreateTask)) Command {
	p := &CreateTask{Title: "Login page", Brief: "Build it", Workspace: "w1"}
	if f != nil {
		f(p)
	}
	return Command{Kind: CmdCreateTask, Origin: OriginDesktop, CreateTask: p}
}

// sessionCmd 回傳一個 session 層級的命令；set 可填入 payload。
func sessionCmd(kind CommandKind, sid SessionID, origin Origin, set func(*Command)) Command {
	c := Command{Kind: kind, Origin: origin, Session: sid}
	if set != nil {
		set(&c)
	}
	return c
}

func commandKinds(t *testing.T) map[CommandKind]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "command.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[CommandKind]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "CommandKind" {
			return true
		}
		for i, v := range vs.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("%s is not a string literal", vs.Names[i].Name)
			}
			s, _ := strconv.Unquote(lit.Value)
			kinds[CommandKind(s)] = vs.Names[i].Name
		}
		return true
	})
	return kinds
}

// TestKindsCoverCommand 確認 kinds 涵蓋 command.go 宣告的每個
// CommandKind，事實旗標與名稱的 Fact 前綴一致，且 cmdSlots
// 恰好是 Command 的全部指標欄位。
func TestKindsCoverCommand(t *testing.T) {
	declared := commandKinds(t)
	for k, name := range declared {
		ki, ok := kinds[k]
		if !ok {
			t.Errorf("%s is not in kinds", name)
			continue
		}
		if want := strings.HasPrefix(name, "Fact"); ki.fact != want {
			t.Errorf("%s: fact flag = %v, want %v", name, ki.fact, want)
		}
		// 每種 Kind 都有處理函式（CmdConfirm 由 run 直接處理）：漏登記的不等到執行才報 unsupported。
		if _, ok := handlers[k]; ok == (k == CmdConfirm) {
			t.Errorf("%s: in handlers = %v, want %v", name, ok, k != CmdConfirm)
		}
	}
	if got, want := len(kinds), len(declared); got != want {
		t.Errorf("kinds covers %d kinds, command.go declares %d", got, want)
	}
	if got, want := len(handlers), len(declared)-1; got != want {
		t.Errorf("handlers has %d kinds, want %d (all but %s)", got, want, CmdConfirm)
	}
	ty := reflect.TypeOf(Command{})
	var fields []string
	for i := 0; i < ty.NumField(); i++ {
		if ty.Field(i).Type.Kind() == reflect.Pointer {
			fields = append(fields, ty.Field(i).Name)
		}
	}
	var slots []string
	for _, s := range cmdSlots {
		slots = append(slots, s.field)
	}
	if !slices.Equal(slots, fields) {
		t.Errorf("cmdSlots = %v, Command pointer fields = %v", slots, fields)
	}
}

// decideCase 是一次 Decide 的測試案例；status 非空時額外檢查摺疊後的 TaskStatus，
// check 非空時對摺疊後的 State 做額外斷言。
type decideCase struct {
	name   string
	noID   bool // State.ID 為空
	given  []Event
	in     Inputs
	cmd    Command
	want   []Event
	reason Reason
	detail string
	status TaskStatus // 摺疊後應有的 Status
	check  func(t *testing.T, folded State)
}

func runDecide(t *testing.T, cases []decideCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := TaskID("t-1")
			if tc.noID {
				id = ""
			}
			s := fold(id, tc.given)
			got, err := Decide(s, tc.in, tc.cmd)
			if tc.reason != "" {
				var rej *Rejection
				if !errors.As(err, &rej) || rej.Reason != tc.reason {
					t.Fatalf("err = %v, want rejection %s", err, tc.reason)
				}
				if tc.detail != "" && !strings.Contains(rej.Detail, tc.detail) {
					t.Fatalf("detail = %q, want substring %q", rej.Detail, tc.detail)
				}
				if got != nil {
					t.Fatalf("got events %+v on rejection", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			var want []Event
			if tc.want != nil {
				want = make([]Event, len(tc.want))
				copy(want, tc.want)
				for i := range want {
					want[i].Seq = s.Version + Seq(i) + 1
					want[i].At = tc.in.Now
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got\n%+v\nwant\n%+v", got, want)
			}
			for i, e := range got {
				if want := s.Version + Seq(i) + 1; e.Seq != want {
					t.Errorf("event %d seq = %d, want %d", i, e.Seq, want)
				}
				if !e.At.Equal(tc.in.Now) {
					t.Errorf("event %d at = %v, want %v", i, e.At, tc.in.Now)
				}
				if _, err := EncodeEvent(e); err != nil {
					t.Errorf("event %d (%s) does not encode: %v", i, e.Kind, err)
				}
			}
			folded := s
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("folding emitted events panicked: %v", r)
					}
				}()
				for _, e := range got {
					folded = Evolve(folded, e)
				}
			}()
			if tc.status != "" && folded.Status != tc.status {
				t.Errorf("folded status = %s, want %s", folded.Status, tc.status)
			}
			if tc.check != nil {
				tc.check(t, folded)
			}
			if !reflect.DeepEqual(s, fold(id, tc.given)) {
				t.Fatal("Decide modified the input state")
			}
		})
	}
}

// running 是一個回合進行中、狀態為 in_progress 的 Task。
func running() []Event {
	return seqs(created(IsolationNone), sessionEv("s2"), queuedEv("s2", "p3", "Build it"),
		turnEv("s2", "t4", "p3"), statusEv(TaskTodo, TaskInProgress, CauseAuto))
}

// and 在既有事件後續編更多事件（重新編號）。
func and(given []Event, more ...Event) []Event {
	out := make([]Event, 0, len(given)+len(more))
	out = append(out, given...)
	out = append(out, more...)
	for i := range out {
		out[i].Seq = Seq(i + 1)
		out[i].At = at0
	}
	return out
}

// forked 是一個 Session（s2）帶著開著的空閒 Fork 的串流。
func forked() []Event {
	return seqs(created(IsolationNone), sessionEv("s2"), forkEv("s2", "main"))
}

// worktreeTask 是有 Task Worktree 的串流。
func worktreeTask() []Event {
	return seqs(created(IsolationWorktree), attached())
}

func reviewReqEv(cands []SessionID, l *LaunchSnapshot) Event {
	return Event{Kind: ReviewRequestedKind, ReviewRequested: &ReviewRequested{
		Candidates: cands, Launch: l,
	}}
}
