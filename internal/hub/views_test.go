package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// V1：建立後 Task 列齊全，帶上 Session 的 agent。
func TestProjectionTask(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	v, err := env.store.ViewsOf(context.Background(), id)
	if err != nil || v.Task == nil {
		t.Fatal("no views")
	}
	if v.Task.Title != "t" || v.Task.Sessions != 1 || v.Task.Running != 1 ||
		v.Task.Archived || v.Task.Status != task.TaskInProgress {
		t.Fatalf("task %+v", v.Task)
	}
	if len(v.Task.Agents) != 1 || v.Task.Agents[0].ID != "test" {
		t.Fatalf("agents %+v", v.Task.Agents)
	}
}

// V2：ask 未決定時 PendingAsks=1，決定後為 0。
func TestProjectionPendingAsks(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "ask a1 rm x", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	e := w.next(task.PermissionAskedKind)
	waitFor(t, func() bool {
		v, _ := env.store.ViewsOf(context.Background(), id)
		return v.Task.PendingAsks == 1
	})
	env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{
			Permission: e.PermissionAsked.ID, Verdict: task.Allow}})
	waitFor(t, func() bool {
		v, _ := env.store.ViewsOf(context.Background(), id)
		return v.Task.PendingAsks == 0
	})
}

// V3：遠端 Merge 產生確認列，Task 列的待確認數為 1；桌面核准後清單為空（非 nil）、數為 0。
func TestProjectionConfirmation(t *testing.T) {
	env := newTestHub(t, nil)
	dir := gitRepo(t)
	env.store.PutWorkspace(context.Background(), Workspace{
		ID: "w-a", Path: realPath(t, dir), Policy: task.PolicyAlways})
	if err := env.h.loadCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := env.create(t, dir, "", testLaunch()) // 無 launch：只有掛著的 worktree
	remote := Principal{Device: "d9"}
	if _, err := env.h.Command(context.Background(), remote, id, task.Command{
		Kind:  task.CmdMerge,
		Merge: &task.Merge{Target: task.Target{Level: task.TargetWorktree}}}); err != nil {
		t.Fatal(err)
	}
	v, _ := env.store.ViewsOf(context.Background(), id)
	if len(v.Confirmations) != 1 {
		t.Fatalf("confirmations %+v", v.Confirmations)
	}
	c := v.Confirmations[0]
	if c.Kind != task.CmdMerge || c.RequestedBy != "d9" {
		t.Fatalf("confirmation %+v", c)
	}
	if v.Task.Confirmations != 1 {
		t.Fatalf("task confirmations %d", v.Task.Confirmations)
	}
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind:    task.CmdConfirm,
		Confirm: &task.ConfirmationRef{Confirmation: c.ID}}); err != nil {
		t.Fatal(err)
	}
	v, _ = env.store.ViewsOf(context.Background(), id)
	if v.Confirmations == nil || len(v.Confirmations) != 0 || v.Task.Confirmations != 0 {
		t.Fatalf("confirmations %+v, task %d", v.Confirmations, v.Task.Confirmations)
	}
}

// V4：歸檔後 Task 列 Archived=true。
func TestProjectionArchived(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.TurnCompletedKind)
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdArchive}); err != nil {
		t.Fatal(err)
	}
	v, _ := env.store.ViewsOf(context.Background(), id)
	if !v.Task.Archived {
		t.Fatal("not archived")
	}
}

// m2.md G01、G02、G03、G05、G11、G12：置頂與取消置頂經 Command 寫進 Task 列；重複的命令不改時間；沒有這個 Task 時是 hub.unknown_task。
func TestProjectionPinned(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.TurnCompletedKind)
	ctx := context.Background()
	pinned := func() time.Time {
		t.Helper()
		v, err := env.store.ViewsOf(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return v.Task.PinnedAt
	}
	if !pinned().IsZero() {
		t.Fatal("pinned before pin")
	}
	before, _ := env.store.ViewsOf(ctx, id)
	if _, err := env.h.Command(ctx, LocalOwner, id, task.Command{Kind: task.CmdPin}); err != nil {
		t.Fatal(err)
	}
	at := w.next(task.TaskPinnedKind).At
	if got := pinned(); !got.Equal(at) {
		t.Fatalf("PinnedAt = %v, want the event's %v", got, at)
	}
	// G02、G12：置頂不改 Task 的更新時間（看板的排序與“幾分鐘前”不變）。
	if v, _ := env.store.ViewsOf(ctx, id); !v.Task.UpdatedAt.Equal(before.Task.UpdatedAt) {
		t.Fatalf("UpdatedAt moved from %v to %v on pin", before.Task.UpdatedAt, v.Task.UpdatedAt)
	}
	seq, err := env.h.Command(ctx, LocalOwner, id, task.Command{Kind: task.CmdPin})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := env.store.ViewsOf(ctx, id); v.Task.Version != seq || !v.Task.PinnedAt.Equal(at) {
		t.Fatalf("pin again: version %d (command %d), PinnedAt %v", v.Task.Version, seq, v.Task.PinnedAt)
	}
	if _, err := env.h.Command(ctx, LocalOwner, id, task.Command{Kind: task.CmdUnpin}); err != nil {
		t.Fatal(err)
	}
	w.next(task.TaskUnpinnedKind)
	if !pinned().IsZero() {
		t.Fatal("still pinned after unpin")
	}
	_, err = env.h.Command(ctx, LocalOwner, "t-nope", task.Command{Kind: task.CmdPin})
	var herr *Error
	if !errors.As(err, &herr) || herr.Code != CodeUnknownTask {
		t.Fatalf("unknown task: got %v", err)
	}
}

// V5：MessageAppended 與標籤（標題＋說明）進全文索引；編輯換掉標籤列。
func TestProjectionSearch(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.TurnCompletedKind)
	// 檢索列累積：回合結束後 hi 仍在。
	v, _ := env.store.ViewsOf(context.Background(), id)
	found := func(text string) bool {
		for _, e := range v.Search {
			if e.Text == text {
				return true
			}
		}
		return false
	}
	if !found("hi") || !found("t\nsay hi") {
		t.Fatalf("search %+v", v.Search)
	}
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind:     task.CmdEditTask,
		EditTask: &task.EditTask{Title: "t", Brief: "new brief"}}); err != nil {
		t.Fatal(err)
	}
	v, _ = env.store.ViewsOf(context.Background(), id)
	if !found("t\nnew brief") || !found("hi") || found("t\nsay hi") {
		t.Fatalf("search %+v", v.Search)
	}
}

// S1–S5：標籤檢索列的內容與編輯替換（D64）。
func TestLabelSearch(t *testing.T) {
	// searchTexts 是 q 的標籤命中（kind 為 label）的文字；all 為真時是全部命中。
	searchHits := func(t *testing.T, env *testEnv, q string, all bool) []string {
		t.Helper()
		hits, err := env.h.Search(ctxb(), LocalOwner, q, 20)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			if all || h.Kind == SearchLabel {
				out = append(out, h.Text)
			}
		}
		return out
	}
	searchTexts := func(t *testing.T, env *testEnv, q string) []string {
		t.Helper()
		return searchHits(t, env, q, false)
	}
	edit := func(t *testing.T, env *testEnv, id task.TaskID, title, brief string) {
		t.Helper()
		if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
			Kind:     task.CmdEditTask,
			EditTask: &task.EditTask{Title: title, Brief: brief}}); err != nil {
			t.Fatal(err)
		}
	}
	labelCount := func(t *testing.T, env *testEnv, id task.TaskID) int {
		t.Helper()
		v, _ := env.store.ViewsOf(context.Background(), id)
		return len(v.Search)
	}

	// S1：標題可搜，說明空時列文就是標題。
	t.Run("S1 title only", func(t *testing.T) {
		env := newTestHub(t, nil)
		if _, _, err := env.h.CreateTask(ctxb(), LocalOwner, t.TempDir(),
			task.CreateTask{Title: "Alpha"}); err != nil {
			t.Fatal(err)
		}
		got := searchTexts(t, env, "alpha")
		if len(got) != 1 || got[0] != "Alpha" {
			t.Fatalf("hits %v", got)
		}
	})

	// S2：標題與說明同一列，中間換行。
	t.Run("S2 title and brief", func(t *testing.T) {
		env := newTestHub(t, nil)
		_, _, err := env.h.CreateTask(ctxb(), LocalOwner, t.TempDir(),
			task.CreateTask{Title: "Alpha", Brief: "實現登錄頁"})
		if err != nil {
			t.Fatal(err)
		}
		got := searchTexts(t, env, "Alpha 登錄")
		if len(got) != 1 || got[0] != "Alpha\n實現登錄頁" {
			t.Fatalf("hits %v", got)
		}
	})

	// S3：編輯後舊標籤搜不到、新標籤命中，且只有一條標籤列。
	t.Run("S3 edit replaces", func(t *testing.T) {
		env := newTestHub(t, nil)
		id, _, err := env.h.CreateTask(ctxb(), LocalOwner, t.TempDir(),
			task.CreateTask{Title: "Alpha", Brief: "實現登錄頁"})
		if err != nil {
			t.Fatal(err)
		}
		edit(t, env, id, "Beta", "註冊頁")
		for _, q := range []string{"Alpha", "登錄"} {
			if got := searchTexts(t, env, q); len(got) != 0 {
				t.Fatalf("%q hits %v", q, got)
			}
		}
		if got := searchTexts(t, env, "Beta 註冊"); len(got) != 1 {
			t.Fatalf("hits %v", got)
		}
		if n := labelCount(t, env, id); n != 1 {
			t.Fatalf("label entries %d", n)
		}
	})

	// S4：重開（重放）後再編輯仍只有一條標籤列。
	t.Run("S4 replay seeds labelSeq", func(t *testing.T) {
		env := newTestHub(t, nil)
		id, _, err := env.h.CreateTask(ctxb(), LocalOwner, t.TempDir(),
			task.CreateTask{Title: "Alpha", Brief: "b"})
		if err != nil {
			t.Fatal(err)
		}
		edit(t, env, id, "Beta", "b2")
		env.h.Close()
		env.h = env.restart(t)
		edit(t, env, id, "Gamma", "b3")
		if n := labelCount(t, env, id); n != 1 {
			t.Fatalf("label entries %d", n)
		}
		if got := searchTexts(t, env, "Gamma"); len(got) != 1 {
			t.Fatalf("hits %v", got)
		}
		for _, q := range []string{"Alpha", "Beta"} {
			if got := searchTexts(t, env, q); len(got) != 0 {
				t.Fatalf("%q hits %v", q, got)
			}
		}
	})

	// S5：有訊息列的 Task 被編輯後訊息仍搜得到。
	t.Run("S5 messages survive edit", func(t *testing.T) {
		env := newTestHub(t, nil)
		id := env.create(t, t.TempDir(), "say hi", testLaunch())
		w := watch(t, env.h, id)
		w.next(task.TurnCompletedKind)
		edit(t, env, id, "Beta", "")
		if got := searchHits(t, env, "hi", true); len(got) == 0 {
			t.Fatal("message hit lost")
		}
		if got := searchTexts(t, env, "say hi"); len(got) != 0 {
			t.Fatalf("stale label hits %v", got)
		}
	})
}

// V6：TaskViews 依 UpdatedAt 新到舊排序。
func TestTaskViewsOrdering(t *testing.T) {
	env := newTestHub(t, nil)
	id1 := env.create(t, t.TempDir(), "", testLaunch())
	env.clock.Advance(time.Hour)
	id2 := env.create(t, t.TempDir(), "", testLaunch())
	tv, _ := env.store.TaskViews(context.Background())
	if len(tv) != 2 || tv[0].ID != id2 || tv[1].ID != id1 {
		t.Fatalf("views %+v", tv)
	}
}

// X1：遮蔽一則分段訊息，改寫全部片段並列入 Unsearch。
func TestRedactSegmentedMessage(t *testing.T) {
	env := newTestHub(t, nil)
	arrived := make(chan struct{}, 8)
	env.h.hooks.fact = func(_ task.TaskID, c task.Command) {
		if c.Kind == task.FactOutput {
			arrived <- struct{}{}
		}
	}
	id := env.create(t, t.TempDir(), "slow 3", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	<-arrived
	snapshotOf(t, env.h, id)
	env.clock.Advance(5 * time.Second)
	var seqs []task.Seq
	var mid task.MessageID
	track := func(e task.Event) bool {
		if e.Kind == task.MessageAppendedKind && e.MessageAppended != nil {
			if mid == "" {
				mid = e.MessageAppended.Message
			}
			if e.MessageAppended.Message == mid && e.MessageAppended.Channel == task.ChannelText {
				seqs = append(seqs, e.Seq)
			}
		}
		return false
	}
	w.until(func(e task.Event) bool {
		track(e)
		return e.Kind == task.MessageAppendedKind && e.MessageAppended.Partial
	})
	os.WriteFile(filepath.Join(env.dir, "go-1"), []byte("x"), 0o644)
	<-arrived
	snapshotOf(t, env.h, id)
	env.clock.Advance(5 * time.Second)
	w.until(func(e task.Event) bool {
		track(e)
		return len(seqs) == 2
	})
	os.WriteFile(filepath.Join(env.dir, "go-2"), []byte("x"), 0o644)
	<-arrived
	os.WriteFile(filepath.Join(env.dir, "go-3"), []byte("x"), 0o644)
	w.until(func(e task.Event) bool {
		track(e)
		return e.Kind == task.TurnCompletedKind
	})
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdRedactMessage, Session: sid,
		RedactMessage: &task.RedactMessage{Message: mid}}); err != nil {
		t.Fatal(err)
	}
	w.next(task.MessageRedactedKind)
	v, _ := env.store.ViewsOf(context.Background(), id)
	if len(seqs) != 3 {
		t.Fatalf("segments %v", seqs)
	}
	// Log 裡的三段都被改寫：Redacted、Text 空、Partial 保留。
	var partials []bool
	for _, seq := range seqs {
		recs, _ := env.store.Read(context.Background(), id, seq-1, 1)
		ev, err := task.DecodeEvent(recs[0].Seq, recs[0].Kind, recs[0].At, recs[0].Payload)
		if err != nil {
			t.Fatal(err)
		}
		if !ev.MessageAppended.Redacted || ev.MessageAppended.Text != "" {
			t.Fatalf("record %+v", ev.MessageAppended)
		}
		partials = append(partials, ev.MessageAppended.Partial)
	}
	if len(partials) != 3 || !partials[0] || !partials[1] || partials[2] {
		t.Fatalf("partials %v", partials)
	}
	// 遮蔽後這三個 Seq 從全文檢索移除。
	for _, seq := range seqs {
		for _, e := range v.Search {
			if e.Seq == seq {
				t.Fatalf("seq %d still searchable", seq)
			}
		}
	}
}

// X2：重啟後重放，片段仍是遮蔽狀態。
func TestRedactedSurvivesRestart(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	e := w.next(task.MessageAppendedKind)
	mid := e.MessageAppended.Message
	sid := e.Session
	w.next(task.TurnCompletedKind)
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdRedactMessage, Session: sid,
		RedactMessage: &task.RedactMessage{Message: mid}}); err != nil {
		t.Fatal(err)
	}
	w.cancel()
	env.h.Close()
	h2 := env.restart(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	redacted := make(chan task.Event, 1)
	go h2.Subscribe(ctx, LocalOwner, id, 0, func(u Update) error {
		if u.Event != nil && u.Event.Kind == task.MessageAppendedKind {
			select {
			case redacted <- *u.Event:
			default:
			}
		}
		return nil
	})
	select {
	case ev := <-redacted:
		if !ev.MessageAppended.Redacted || ev.MessageAppended.Text != "" {
			t.Fatalf("replayed %+v", ev.MessageAppended)
		}
	case <-time.After(testWait):
		t.Fatal("no redacted message on replay")
	}
}

// X3：未知的 message id → hub.unknown_message，log 不變。
func TestRedactUnknownMessage(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	n1, _ := env.store.Read(context.Background(), id, 0, 0)
	_, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind:          task.CmdRedactMessage,
		RedactMessage: &task.RedactMessage{Message: "m-nope"}})
	var herr *Error
	if !errors.As(err, &herr) || herr.Code != CodeUnknownMessage {
		t.Fatalf("got %v", err)
	}
	n2, _ := env.store.Read(context.Background(), id, 0, 0)
	if len(n2) != len(n1) {
		t.Fatal("log changed")
	}
}

// X4：訊息還開著（只 flush 過 Partial）→ ReasonInvalid。
func TestRedactOpenMessage(t *testing.T) {
	env := newTestHub(t, nil)
	arrived := make(chan struct{}, 8)
	env.h.hooks.fact = func(_ task.TaskID, c task.Command) {
		if c.Kind == task.FactOutput {
			arrived <- struct{}{}
		}
	}
	id := env.create(t, t.TempDir(), "slow 2", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	<-arrived
	snapshotOf(t, env.h, id)
	env.clock.Advance(5 * time.Second)
	e := w.until(func(e task.Event) bool {
		return e.Kind == task.MessageAppendedKind && e.MessageAppended.Partial
	})
	_, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdRedactMessage, Session: sid,
		RedactMessage: &task.RedactMessage{Message: e.MessageAppended.Message}})
	var rej *task.Rejection
	if !errors.As(err, &rej) || rej.Reason != task.ReasonInvalid {
		t.Fatalf("got %v", err)
	}
	os.WriteFile(filepath.Join(env.dir, "go-1"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(env.dir, "go-2"), []byte("x"), 0o644)
	w.next(task.TurnCompletedKind)
}

// X5：遠端不能遮蔽。
func TestRedactNeedsDesktop(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	e := w.next(task.MessageAppendedKind)
	sid := e.Session
	w.next(task.TurnCompletedKind)
	_, err := env.h.Command(context.Background(), Principal{Device: "d9"}, id, task.Command{
		Kind: task.CmdRedactMessage, Session: sid,
		RedactMessage: &task.RedactMessage{Message: e.MessageAppended.Message}})
	var rej *task.Rejection
	if !errors.As(err, &rej) || rej.Reason != task.ReasonNeedsDesktop {
		t.Fatalf("got %v", err)
	}
}

// S1：慢訂閱者被踢出，回 hub.lagging；重訂閱補齊缺口。
func TestLaggingSubscriberEvicted(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	var block atomic.Bool
	release := make(chan struct{})
	got := make(chan task.Event, 8)
	subErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		subErr <- env.h.Subscribe(ctx, LocalOwner, id, 0, func(u Update) error {
			if u.Event == nil {
				return nil
			}
			select {
			case got <- *u.Event:
			default:
			}
			if block.Load() {
				<-release // 卡住的即時事件 sink；回放期間不卡
			}
			return nil
		})
	}()
	<-got // 回放完才會收到即時事件前的 TaskCreated
	for i := range 2 {
		env.h.Command(context.Background(), LocalOwner, id, task.Command{
			Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "warm-" + string(rune('0'+i))}})
		<-got // 即時事件到了 → 已登記
	}
	block.Store(true)
	for i := range 300 {
		if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
			Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: string(rune('A'+i%26)) + "-" + string(rune('0'+i/26))}}); err != nil {
			t.Fatal(err)
		}
	}
	close(release) // 讓卡住的 sink 返回；Subscribe 發現已被踢出
	select {
	case err := <-subErr:
		var herr *Error
		if !errors.As(err, &herr) || herr.Code != CodeLagging {
			t.Fatalf("got %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("subscriber not evicted")
	}
	// 補缺口：從 sink 最後看到的 Seq 重新訂閱。
	last := task.Seq(0)
	for {
		select {
		case e := <-got:
			last = e.Seq
		default:
			goto have
		}
	}
have:
	var mu sync.Mutex
	var seqs []task.Seq
	go env.h.Subscribe(context.Background(), LocalOwner, id, last, func(u Update) error {
		if u.Event == nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		seqs = append(seqs, u.Event.Seq)
		return nil
	})
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seqs) >= int(301-last)
	})
	mu.Lock()
	defer mu.Unlock()
	// 驗證銜接：last 之後是 last+1..301，不重不漏。
	for i, s := range seqs {
		if s != last+task.Seq(i)+1 {
			t.Fatalf("gap: %v", seqs)
		}
	}
}

// 卡住但在 buffer 滿之前離開 → 乾淨結束（ctx 取消回 nil）。
func TestSlowSubscriberLeavesCleanly(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- env.h.Subscribe(ctx, LocalOwner, id, 0, func(Update) error {
			return nil
		})
	}()
	snapshotOf(t, env.h, id) // 郵箱 FIFO：snap 回覆時 sub 已登記
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("got %v", err)
	}
}

// M1：一個 Task 存的 workspace 規則套用到同 Workspace 的另一個。
func TestWorkspaceRuleAcrossTasks(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	id1 := env.create(t, dir, "ask a1 rm -rf /", testLaunch())
	w1 := watch(t, env.h, id1)
	sid1 := w1.next(task.SessionCreatedKind).Session
	e := w1.next(task.PermissionAskedKind)
	if _, err := env.h.Command(context.Background(), LocalOwner, id1, task.Command{
		Kind: task.CmdDecidePermission, Session: sid1,
		DecidePermission: &task.DecidePermission{
			Permission: e.PermissionAsked.ID, Verdict: task.Allow,
			Always: &task.Rule{Scope: task.ScopeWorkspace,
				Match: task.MatchPrefix, Pattern: "rm"}}}); err != nil {
		t.Fatal(err)
	}
	w1.next(task.RuleAddedKind) // 規則與決定同一次 Decide 寫入
	w1.cancel()
	// 同目錄 → 同一個 Workspace → 規則命中，不用使用者命令。
	id2 := env.create(t, dir, "ask b1 rm -rf /", testLaunch())
	w2 := watch(t, env.h, id2)
	w2.next(task.PermissionAskedKind)
	e2 := w2.next(task.PermissionDecidedKind)
	if e2.PermissionDecided.Decision.Source != task.DecidedByRule {
		t.Fatalf("decision %+v", e2.PermissionDecided)
	}
}

// M2：自動登記的 Workspace 立刻可見（不再經 PutWorkspace）。
func TestWorkspaceVisibleAfterCreate(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	env.create(t, dir, "", testLaunch())
	ws, _ := env.store.Workspaces(context.Background())
	if len(ws) != 1 || ws[0].Path != realPath(t, dir) {
		t.Fatalf("workspaces %+v", ws)
	}
}

// m2.md B15–B17（D135）：Task 列的 agent 按 Session 建立的順序、不重複，關掉的 Session 照算。
func TestTaskViewAgents(t *testing.T) {
	launch := func(id task.AgentID, name string) task.LaunchSnapshot {
		return task.LaunchSnapshot{Agent: id, AgentName: name}
	}
	events := []task.Event{
		{Seq: 1, Kind: task.TaskCreatedKind, TaskCreated: &task.TaskCreated{Title: "t", Isolation: task.IsolationNone}},
		{Seq: 2, Kind: task.SessionCreatedKind, Session: "s2", SessionCreated: &task.SessionCreated{Launch: launch("codex", "Codex"), Role: task.RoleWorker}},
		{Seq: 3, Kind: task.SessionCreatedKind, Session: "s3", SessionCreated: &task.SessionCreated{Launch: launch("claude", "Claude Code"), Role: task.RoleWorker}},
		{Seq: 4, Kind: task.SessionCreatedKind, Session: "s4", SessionCreated: &task.SessionCreated{Launch: launch("codex", "Codex"), Role: task.RoleWorker}},
		{Seq: 5, Kind: task.SessionClosedKind, Session: "s2"},
	}
	s := task.State{ID: "t-1"}
	for _, e := range events {
		s = task.Evolve(s, e)
	}
	want := []TaskAgent{{ID: "codex", Name: "Codex"}, {ID: "claude", Name: "Claude Code"}}
	if got := stateViews(s).Task.Agents; !reflect.DeepEqual(got, want) {
		t.Fatalf("agents %v, want %v", got, want)
	}
}
