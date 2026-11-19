package hub

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// D01–D03、D05、D07、D08（m2.md）：一個 Task 的條目與排序，純函式。
func TestAttentionOf(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	launch := task.LaunchSnapshot{Agent: "grok", AgentName: "Grok"}
	req := task.PermissionRequest{Tool: "execute", Kind: task.ToolExecute, Title: "bun test", Command: "bun test"}
	ask := func(id task.PermissionID, at time.Time, decided bool) task.Ask {
		a := task.Ask{ID: id, Turn: "t1", Request: req, AskedAt: at}
		if decided {
			a.Decision = &task.Decision{Verdict: task.Allow, Source: task.DecidedByUser}
		}
		return a
	}
	state := func(id task.TaskID, pending map[task.PermissionID]task.Ask, failed map[task.EffectKey]task.Suspension) task.State {
		return task.State{ID: id, Title: "title " + string(id), Workspace: "w1", Order: []task.SessionID{"s1"},
			Sessions: map[task.SessionID]task.Session{"s1": {ID: "s1", Launch: launch, Pending: pending}},
			Failed:   failed}
	}
	prob := task.Problem{Code: "workspace.unsupported", Params: map[string]string{"op": "create_worktree"}}
	askItem := func(tid task.TaskID, a task.Ask) AttentionItem {
		return AttentionItem{Kind: AttentionAsk, Task: tid, TaskTitle: "title " + string(tid), Workspace: "w1", Since: a.AskedAt,
			Ask: &AskItem{Session: "s1", Agent: "grok", AgentName: "Grok", Ask: a}}
	}
	suspendedItem := func(tid task.TaskID, key task.EffectKey, at time.Time) AttentionItem {
		return AttentionItem{Kind: AttentionSuspended, Task: tid, TaskTitle: "title " + string(tid), Workspace: "w1", Since: at,
			Suspended: &SuspendedItem{Effect: key, Problem: prob}}
	}
	archived := state("t1", map[task.PermissionID]task.Ask{"a1": ask("a1", t0, false)}, nil)
	archived.Archived = true
	for _, tc := range []struct {
		name   string
		states []task.State
		want   []AttentionItem
	}{
		{"D01 pending ask", []task.State{state("t1", map[task.PermissionID]task.Ask{"a1": ask("a1", t0, false)}, nil)},
			[]AttentionItem{askItem("t1", ask("a1", t0, false))}},
		{"D03 decided, not yet answered", []task.State{state("t1", map[task.PermissionID]task.Ask{"a1": ask("a1", t0, true)}, nil)}, nil},
		{"D05 suspended effect", []task.State{state("t1", nil, map[task.EffectKey]task.Suspension{"create_worktree:task": {Problem: prob, At: t0}})},
			[]AttentionItem{suspendedItem("t1", "create_worktree:task", t0)}},
		{"D08 archived", []task.State{archived}, nil},
		{"D07 oldest first, ties by task then key", []task.State{
			state("t2", map[task.PermissionID]task.Ask{"a2": ask("a2", t0, false), "a1": ask("a1", t0, false)}, nil),
			state("t1", map[task.PermissionID]task.Ask{"a9": ask("a9", t0.Add(time.Minute), false)},
				map[task.EffectKey]task.Suspension{"create_worktree:task": {Problem: prob, At: t0}}),
		}, []AttentionItem{
			suspendedItem("t1", "create_worktree:task", t0),
			askItem("t2", ask("a1", t0, false)),
			askItem("t2", ask("a2", t0, false)),
			askItem("t1", ask("a9", t0.Add(time.Minute), false)),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []AttentionItem
			for _, s := range tc.states {
				got = append(got, attentionOf(s)...)
			}
			sortAttention(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// D01、D05、D06、D09、D10：經 hub 取列表；條數與 Task 列的 PendingAsks + Failed 相同；
// 回答後審批消失；重啟後掛起的操作仍在、時間不變；關閉後回 closed。
func TestAttentionThroughHub(t *testing.T) {
	env := newTestHub(t, nil)
	suspendedTask := alwaysTask(t, env)
	watch(t, env.h, suspendedTask).next(task.EffectFailedKind)
	env.clock.Advance(time.Minute)
	askTask := env.create(t, t.TempDir(), "ask a1 rm -rf x", testLaunch())
	w := watch(t, env.h, askTask)
	sid := w.next(task.SessionCreatedKind).Session
	pid := w.next(task.PermissionAskedKind).PermissionAsked.ID

	items, err := env.h.Attention(ctxb(), LocalOwner)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Task != suspendedTask || items[0].Kind != AttentionSuspended ||
		items[1].Task != askTask || items[1].Kind != AttentionAsk || items[1].Ask.Session != sid || items[1].Ask.Ask.ID != pid {
		t.Fatalf("items %+v", items)
	}
	if !items[0].Since.Before(items[1].Since) {
		t.Fatalf("since %v, %v", items[0].Since, items[1].Since)
	}
	if got := attentionCount(t, env.h); got != len(items) {
		t.Fatalf("D09 rows count %d, items %d", got, len(items))
	}
	since := items[0].Since

	if _, err := env.h.Command(ctxb(), LocalOwner, askTask, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{Permission: pid, Verdict: task.Allow}}); err != nil {
		t.Fatal(err)
	}
	w.next(task.PermissionDecidedKind)
	if items, err = env.h.Attention(ctxb(), LocalOwner); err != nil || len(items) != 1 || items[0].Task != suspendedTask {
		t.Fatalf("after deciding %+v %v", items, err)
	}
	w.next(task.TurnCompletedKind)

	env.h.Close()
	env.h = env.restart(t)
	items, err = env.h.Attention(ctxb(), LocalOwner)
	if err != nil || len(items) != 1 || items[0].Kind != AttentionSuspended || !items[0].Since.Equal(since) {
		t.Fatalf("D06 after restart %+v %v, want suspended since %v", items, err, since)
	}
	env.h.Close()
	if _, err := env.h.Attention(ctxb(), LocalOwner); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close %v", err)
	}
}

// attentionCount 是 Task 列算出的“需處理”數（菜單欄的口徑）。
func attentionCount(t *testing.T, h *Hub) int {
	t.Helper()
	rows, err := h.Tasks(ctxb(), LocalOwner)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range rows {
		if !v.Archived {
			n += v.PendingAsks + v.Failed
		}
	}
	return n
}
