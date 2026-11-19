package hub

// search_test.go：搜索命中消息（m2.md 第 32 節 Q26–Q37，D70）。順序與每個 Task 的上限在
// internal/store 的 TestSearchMessageCap 對兩種實作驗收（Q30–Q35、Q38）；這裡驗收哪些事件
// 產生檢索列、增量與重放一致，以及經 hub 的遮蔽、重建與刪除。

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// searchEvents 是一個 Task 的一輪對話：使用者的訊息、思考、分三段寫進的回答（“OAuth”
// 被拆在兩段之間）、工具呼叫，再加一條還沒結束的回答。
func searchEvents() []task.Event {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	launch := task.LaunchSnapshot{Agent: "grok", AgentName: "Grok"}
	text := func(mid task.MessageID, channel task.ChannelKind, s string, partial bool) task.Event {
		return task.Event{Kind: task.MessageAppendedKind, Session: "s-1", MessageAppended: &task.MessageAppended{
			Message: mid, Turn: "u-1", Channel: channel, Text: s, Partial: partial}}
	}
	events := []task.Event{
		{Kind: task.TaskCreatedKind, TaskCreated: &task.TaskCreated{Title: "Login", Workspace: "w-1"}},
		{Kind: task.SessionCreatedKind, Session: "s-1", SessionCreated: &task.SessionCreated{Launch: launch}},
		{Kind: task.PromptQueuedKind, Session: "s-1", PromptQueued: &task.Prompt{ID: "p-1", Text: "把登录页改成 OAuth"}},
		{Kind: task.TurnStartedKind, Session: "s-1", TurnStarted: &task.TurnStarted{Turn: "u-1", Prompt: "p-1"}},
		text("m-1", task.ChannelThinking, "想想登录", true),
		text("m-1", task.ChannelThinking, "", false),
		text("m-2", task.ChannelText, "登录流程已改好，OA", true),
		text("m-2", task.ChannelText, "uth 也接上了\n第二行", true),
		text("m-2", task.ChannelText, "", false),
		{Kind: task.ToolCallStartedKind, Session: "s-1", ToolCallStarted: &task.ToolCall{ID: "c-1", Title: "登录测试"}},
		text("m-3", task.ChannelText, "还在写登录", true),
	}
	for i := range events {
		events[i].Seq, events[i].At = task.Seq(i+1), at.Add(time.Duration(i)*time.Second)
	}
	return events
}

// Q26–Q29：使用者的訊息與結束的正文回答各一列（回答是全部片段連起來，記在最後一段的 Seq，
// 帶 agent 名稱）；思考、工具呼叫與還沒結束的回答不進。逐個事件一批、整批一次與重放三種算法一致。
func TestSearchEntries(t *testing.T) {
	events := searchEvents()
	want := []SearchEntry{
		{Seq: 1, Kind: SearchLabel, Text: "Login"},
		{Seq: 3, Kind: SearchPrompt, Ref: "p-1", Text: "把登录页改成 OAuth"},
		{Seq: 9, Kind: SearchReply, Ref: "m-2", Agent: "Grok", Text: "登录流程已改好，OAuth 也接上了\n第二行"},
	}

	_, drafts, v := projections(task.State{ID: "t-1"}, nil, events, "")
	if !slices.Equal(v.Search, want) {
		t.Fatalf("one batch %+v", v.Search)
	}
	// Q28：還沒結束的回答留在 drafts，等它結束才寫。
	if len(drafts) != 1 || drafts["m-3"] != "还在写登录" {
		t.Fatalf("drafts %v", drafts)
	}

	s, d := task.State{ID: "t-1"}, searchDrafts(nil)
	var got []SearchEntry
	for _, e := range events {
		var views Views
		s, d, views = projections(s, d, []task.Event{e}, "")
		got = append(got, views.Search...)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("event by event %+v", got)
	}

	// projections 不改傳入的 drafts（Append 失敗時 actor 留著舊的）。
	before := searchDrafts{"m-3": "还在写"}
	projections(s, before, []task.Event{{Seq: 12, Kind: task.MessageAppendedKind, Session: "s-1",
		MessageAppended: &task.MessageAppended{Message: "m-3", Turn: "u-1", Channel: task.ChannelText}}}, "")
	if before["m-3"] != "还在写" {
		t.Fatalf("drafts mutated %v", before)
	}

	env := newTestHub(t, nil)
	appendEvents(t, env, "t-1", events)
	l, err := env.h.replay(ctxb(), "t-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.search, want) || l.drafts["m-3"] != "还在写登录" {
		t.Fatalf("replay %+v %v", l.search, l.drafts)
	}
}

// appendEvents 把事件原樣寫進 Log（沒有投影），供重放與重建用。
func appendEvents(t *testing.T, env *testEnv, id task.TaskID, events []task.Event) {
	t.Helper()
	var b Batch
	for _, e := range events {
		payload, err := task.EncodeEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Records = append(b.Records, Record{Seq: e.Seq, Kind: e.Kind, At: e.At, Payload: payload})
	}
	if err := env.store.Append(ctxb(), id, 0, b); err != nil {
		t.Fatal(err)
	}
}

// searchKinds 是 q 的命中，每條寫成“來源:文字”。
func searchKinds(t *testing.T, h *Hub, q string) []string {
	t.Helper()
	hits, err := h.Search(ctxb(), LocalOwner, q, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range hits {
		out = append(out, string(x.Kind)+":"+x.Agent+":"+x.Text)
	}
	return out
}

// Q26、Q35、Q37：經 hub 跑一輪：標籤、使用者的訊息與 agent 的回答都搜得到，標籤在前、
// 訊息新到舊；遮蔽了回答就搜不到它，重啟後也一樣；Task 刪除後什麼都搜不到。
func TestSearchMessagesThroughHub(t *testing.T) {
	env := newTestHub(t, nil)
	id, _, _, _ := archiveIdle(t, env, testLaunch())
	sid, mid := task.SessionID(""), task.MessageID("")
	recs, _ := env.store.Read(ctxb(), id, 0, 0)
	for _, r := range recs {
		e, err := task.DecodeEvent(r.Seq, r.Kind, r.At, r.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == task.MessageAppendedKind && e.MessageAppended.Channel == task.ChannelText {
			sid, mid = e.Session, e.MessageAppended.Message
		}
	}
	if mid == "" {
		t.Fatal("no reply")
	}
	got := searchKinds(t, env.h, "hi")
	if len(got) != 3 || got[0] != "label::t\nsay hi" || got[1] != "reply:Test Agent:hi" || got[2] != "prompt::say hi" {
		t.Fatalf("hits %q", got)
	}

	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdRedactMessage, Session: sid,
		RedactMessage: &task.RedactMessage{Message: mid}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"label::t\nsay hi", "prompt::say hi"}
	if got := searchKinds(t, env.h, "hi"); !slices.Equal(got, want) {
		t.Fatalf("after redaction %q", got)
	}
	env.h.Close()
	env.h = env.restart(t)
	env.store.viewsVersion = 0 // 重建投影：遮蔽的回答重放後也不進
	env.h.Close()
	env.h = env.restart(t)
	if got := searchKinds(t, env.h, "hi"); !slices.Equal(got, want) {
		t.Fatalf("after rebuild %q", got)
	}

	env.command(t, id, task.CmdArchive)
	if err := deleteErr(env.h, LocalOwner, id); err != nil {
		t.Fatal(err)
	}
	if got := searchKinds(t, env.h, "hi"); len(got) != 0 {
		t.Fatalf("after delete %q", got)
	}
}

// Q36：上一個投影版本建的檢索列（回答按片段各一列、沒有來源、沒有使用者的訊息）在啟動時
// 按重放重建成本節的樣子。
func TestSearchRebuildsOldRows(t *testing.T) {
	env := newTestHub(t, nil)
	// 歸檔了的 Task：啟動時只重建投影，不起 actor（測試不需要 Workspace 與 agent）。
	events := append(searchEvents(), task.Event{Seq: 12, Kind: task.TaskArchivedKind,
		At: time.Date(2026, 11, 16, 9, 1, 0, 0, time.UTC)})
	var b Batch
	for _, e := range events {
		payload, err := task.EncodeEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Records = append(b.Records, Record{Seq: e.Seq, Kind: e.Kind, At: e.At, Payload: payload})
		if e.Kind == task.MessageAppendedKind && e.MessageAppended.Text != "" {
			b.Views.Search = append(b.Views.Search, SearchEntry{Seq: e.Seq, Text: e.MessageAppended.Text})
		}
	}
	b.Views.Search = append(b.Views.Search, SearchEntry{Seq: 1, Text: "Login"})
	b.Views.Task = &TaskView{ID: "t-old", Title: "Login", Workspace: "w-1"}
	if err := env.store.Append(ctxb(), "t-old", 0, b); err != nil {
		t.Fatal(err)
	}
	env.store.viewsVersion = 7
	env.h.Close()
	env.h = env.restart(t)
	want := []string{"label::Login", "reply:Grok:登录流程已改好，OAuth 也接上了\n第二行", "prompt::把登录页改成 OAuth"}
	if got := searchKinds(t, env.h, "oauth"); !slices.Equal(got, want[1:]) {
		t.Fatalf("oauth %q", got)
	}
	if got := searchKinds(t, env.h, "想想"); len(got) != 0 {
		t.Fatalf("thinking still searchable %q", got)
	}
	if got := searchKinds(t, env.h, "login"); !slices.Equal(got, want[:1]) {
		t.Fatalf("label %q", got)
	}
}
