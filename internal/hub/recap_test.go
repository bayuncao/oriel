package hub

import (
	"reflect"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// R01–R03：Hub.Recap 取未歸檔 Task 在 since 之後結束的回合：完成的與失敗的（帶原因），取消的不算；
// 新到舊；since 早於 7 天前時按 7 天前算；投影從日誌重建後相同。
func TestHubRecap(t *testing.T) {
	env := newTestHub(t, nil)
	start := env.clock.Now()
	env.clock.Advance(time.Minute)

	done := env.create(t, t.TempDir(), "say hi\nsecond line", testLaunch())
	watch(t, env.h, done).next(task.TurnCompletedKind)
	doneAt := env.clock.Now()
	env.clock.Advance(time.Minute)

	failed := env.create(t, t.TempDir(), "ask a1 python3 --version", testLaunch())
	wf := watch(t, env.h, failed)
	sid := wf.next(task.SessionCreatedKind).Session
	pid := wf.next(task.PermissionAskedKind).PermissionAsked.ID
	if _, err := env.h.Command(ctxb(), LocalOwner, failed, task.Command{Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{Permission: pid, Verdict: task.Deny}}); err != nil {
		t.Fatal(err)
	}
	wf.next(task.TurnFailedKind)
	failedAt := env.clock.Now()

	cancelled := env.create(t, t.TempDir(), "hang", testLaunch())
	wc := watch(t, env.h, cancelled)
	csid := wc.next(task.TurnStartedKind).Session
	if _, err := env.h.Command(ctxb(), LocalOwner, cancelled, task.Command{Kind: task.CmdCancelTurn, Session: csid}); err != nil {
		t.Fatal(err)
	}
	wc.next(task.TurnCancelledKind)

	recap := func(h *Hub, since time.Time) []RecapTurn {
		t.Helper()
		got, err := h.Recap(ctxb(), LocalOwner, since)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := recap(env.h, start)
	if len(got) != 2 {
		t.Fatalf("R01 want 2 turns (cancelled left out), got %+v", got)
	}
	f, d := got[0], got[1]
	if f.Task != failed || f.Result != task.TurnError || f.Problem == nil || f.Text != "ask a1 python3 --version" || !f.EndedAt.Equal(failedAt) {
		t.Fatalf("R01 failed turn %+v", f)
	}
	if d.Task != done || d.Result != task.TurnOK || d.Problem != nil || d.Text != "say hi" || d.TaskTitle == "" || d.Workspace == "" || !d.EndedAt.Equal(doneAt) {
		t.Fatalf("R01 done turn %+v", d)
	}
	if got := recap(env.h, failedAt); len(got) != 1 || got[0].Task != failed {
		t.Fatalf("R02 since %+v", got)
	}

	// R02：since 早於 7 天前按 7 天前算。
	env.clock.Advance(7*24*time.Hour - 30*time.Second) // 7 天前落在兩個回合之間
	if got := recap(env.h, start); len(got) != 1 || got[0].Task != failed {
		t.Fatalf("R02 window %+v", got)
	}
	if got := recap(env.h, time.Time{}); len(got) != 1 {
		t.Fatalf("R02 zero since %+v", got)
	}

	// R03：投影從日誌重建後相同。
	before := recap(env.h, start)
	env.h.Close()
	env.store.mu.Lock()
	env.store.turns = map[task.TaskID][]TurnEnd{}
	env.store.viewsVersion = 1
	env.store.mu.Unlock()
	h2 := env.restart(t)
	if got := recap(h2, start); !reflect.DeepEqual(got, before) {
		t.Fatalf("R03 rebuilt %+v\nwant %+v", got, before)
	}

	// 已歸檔的 Task 不算。
	if _, err := h2.Command(ctxb(), LocalOwner, failed, task.Command{Kind: task.CmdArchive}); err != nil {
		t.Fatal(err)
	}
	if got := recap(h2, start); len(got) != 0 {
		t.Fatalf("archived %+v", got)
	}
}

// R01：消息的第一個非空行，去掉首尾空白，最多 200 個字元。
func TestFirstLine(t *testing.T) {
	long := ""
	for range 210 {
		long += "字"
	}
	cases := []struct{ in, want string }{
		{"", ""},
		{"  \n\t\n", ""},
		{"\n  fix it  \nmore", "fix it"},
		{"one", "one"},
		{long, long[:len("字")*200]},
	}
	for _, c := range cases {
		if got := firstLine(c.in, maxTurnText); got != c.want {
			t.Errorf("firstLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
