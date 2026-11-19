package hub

// usage_views_test.go：會話歸屬與上報的用量（m2.md 第 33 節）。合計與去重在 internal/store 的
// TestUsageAttribution 對兩種實現驗收；這裡驗收哪些事件產生哪些列，增量與重建一致（U81）。

import (
	"slices"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

func usageEvents() []task.Event {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	launch := task.LaunchSnapshot{Agent: "grok", AgentName: "Grok"}
	used := func(in, out int64) task.Event {
		return task.Event{Kind: task.UsageReportedKind, Session: "s-1",
			UsageReported: &task.Usage{InputTokens: in, OutputTokens: out}}
	}
	events := []task.Event{
		{Kind: task.TaskCreatedKind, TaskCreated: &task.TaskCreated{Title: "t", Workspace: "w-1"}},
		{Kind: task.SessionCreatedKind, Session: "s-1", SessionCreated: &task.SessionCreated{Launch: launch}},
		used(1, 1), // 還沒綁定原生會話：不知道是哪個會話，不記
		{Kind: task.NativeSessionBoundKind, Session: "s-1", NativeSessionBound: &task.NativeBound{NativeID: "g1"}},
		used(100, 50),
		{Kind: task.ModelReportedKind, Session: "s-1", ModelReported: &task.ModelReport{
			Setting: &task.ModelSetting{Option: "model", Current: "grok-4"}}},
		used(200, 20),
		// 接回同一個原生會話：同一列再寫一次（store 照舊）。
		{Kind: task.NativeSessionBoundKind, Session: "s-1", NativeSessionBound: &task.NativeBound{NativeID: "g1"}},
	}
	for i := range events {
		events[i].Seq, events[i].At = task.Seq(i+1), at.Add(time.Duration(i)*time.Second)
	}
	return events
}

func TestUsageViews(t *testing.T) {
	events := usageEvents()
	link := SessionLink{Agent: "grok", Native: "g1", Task: "t-1", Session: "s-1"}
	wantLinks := []SessionLink{link, link}
	wantReported := []ReportedUsage{
		{Task: "t-1", Seq: 5, Agent: "grok", Native: "g1", At: events[4].At, Dir: "/ws",
			Usage: task.Usage{InputTokens: 100, OutputTokens: 50}},
		{Task: "t-1", Seq: 7, Agent: "grok", Native: "g1", At: events[6].At, Model: "grok-4", Dir: "/ws",
			Usage: task.Usage{InputTokens: 200, OutputTokens: 20}},
	}

	_, _, v := projections(task.State{ID: "t-1"}, nil, events, "/ws")
	if !slices.Equal(v.Links, wantLinks) || !slices.Equal(v.Reported, wantReported) {
		t.Fatalf("one batch\nlinks %+v\nreported %+v", v.Links, v.Reported)
	}

	// U81：重建按事件補齊同樣的列。
	env := newTestHub(t, nil)
	env.store.PutWorkspace(ctxb(), Workspace{ID: "w-1", Path: "/ws"})
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	appendEvents(t, env, "t-1", events)
	l, err := env.h.replay(ctxb(), "t-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := rebuildViews(l); !slices.Equal(got.Links, wantLinks) || !slices.Equal(got.Reported, wantReported) {
		t.Fatalf("rebuild\nlinks %+v\nreported %+v", got.Links, got.Reported)
	}
}
