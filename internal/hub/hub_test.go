package hub

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

func TestCreateTaskRegistersWorkspace(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	id, seq, err := env.h.CreateTask(context.Background(), LocalOwner, dir,
		task.CreateTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || seq != 1 {
		t.Fatalf("id %q seq %d", id, seq)
	}
	ws, _ := env.store.Workspaces(context.Background())
	if len(ws) != 1 || ws[0].Path != realPath(t, dir) {
		t.Fatalf("workspaces %+v", ws)
	}
	recs, _ := env.store.Read(context.Background(), id, 0, 10)
	if len(recs) != 1 || recs[0].Kind != task.TaskCreatedKind {
		t.Fatalf("records %+v", recs)
	}
}

func TestRemoteCannotRegisterWorkspace(t *testing.T) {
	env := newTestHub(t, nil)
	_, _, err := env.h.CreateTask(context.Background(), Principal{Device: "d1"}, t.TempDir(),
		task.CreateTask{Title: "t"})
	var herr *Error
	if !errors.As(err, &herr) || herr.Code != CodeUnknownWorkspace {
		t.Fatalf("got %v", err)
	}
	ids, _ := env.store.Streams(context.Background())
	if len(ids) != 0 {
		t.Fatalf("streams %v", ids)
	}
}

func TestClientOriginOverwritten(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	_, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdEditTask, Origin: task.OriginRuntime,
		EditTask: &task.EditTask{Title: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	w := watch(t, env.h, id)
	w.next(task.TaskEditedKind)
}

func TestUnknownTask(t *testing.T) {
	env := newTestHub(t, nil)
	_, err := env.h.Command(context.Background(), LocalOwner, "t-nope", task.Command{
		Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "x"}})
	var herr *Error
	if !errors.As(err, &herr) || herr.Code != CodeUnknownTask {
		t.Fatalf("got %v", err)
	}
}

func TestRejectionPassesThrough(t *testing.T) {
	env := newTestHub(t, nil)
	id, seq, err := env.h.CreateTask(context.Background(), LocalOwner, t.TempDir(),
		task.CreateTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdEditTask, EditTask: &task.EditTask{}})
	var rej *task.Rejection
	if !errors.As(err, &rej) {
		t.Fatalf("got %v", err)
	}
	if seq2 != seq {
		t.Fatalf("version moved: %d → %d", seq, seq2)
	}
}

func TestSubscribeReplaysThenFollows(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	for i := range 4 {
		if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
			Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: string(rune('A' + i))}}); err != nil {
			t.Fatal(err)
		}
	}
	var got []task.Seq
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := env.h.Subscribe(context.Background(), LocalOwner, id, 0, func(u Update) error {
			if u.Event == nil {
				return nil
			}
			mu.Lock()
			got = append(got, u.Event.Seq)
			mu.Unlock()
			return nil
		}); err != nil {
			t.Logf("subscribe returned: %v", err)
		}
	}()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 5
	})
	for i := range 3 {
		env.h.Command(context.Background(), LocalOwner, id, task.Command{
			Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: string(rune('x' + i))}})
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 8
	})
	mu.Lock()
	defer mu.Unlock()
	want := []task.Seq{1, 2, 3, 4, 5, 6, 7, 8}
	if !slices.Equal(got, want) {
		t.Fatalf("seqs %v", got)
	}
}

func TestTwoSubscribersSeeOneSequence(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	w1 := watch(t, env.h, id)
	w2 := watch(t, env.h, id)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env.h.Command(context.Background(), LocalOwner, id, task.Command{
				Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: string(rune('A' + i))}})
		}()
	}
	wg.Wait()
	// 21 = TaskCreated + 20 TaskEdited；推送在 Command 回覆前就
	// 進了 sub.ch，接下來只等訂閱 goroutine 送進 w.events。
	for _, w := range []*watcher{w1, w2} {
		deadline := time.Now().Add(testWait)
		for len(w.events) < 21 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if len(w.events) < 21 {
			t.Fatalf("watcher got %d events, want 21", len(w.events))
		}
	}
	seqs := func(w *watcher) []task.Seq {
		var out []task.Seq
		for {
			select {
			case e := <-w.events:
				out = append(out, e.Seq)
			default:
				return out
			}
		}
	}
	s1, s2 := seqs(w1), seqs(w2)
	if !slices.Equal(s1, s2) {
		t.Fatalf("subscribers differ: %v vs %v", s1, s2)
	}
	for i, seq := range s1 {
		if seq != task.Seq(i+1) {
			t.Fatalf("seqs not contiguous: %v", s1)
		}
	}
}

func TestSinkErrorEndsSubscription(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	boom := errors.New("boom")
	err := env.h.Subscribe(context.Background(), LocalOwner, id, 0, func(Update) error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	// ctx cancel 也是乾淨結束。
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := env.h.Subscribe(ctx, LocalOwner, id, 0, func(Update) error {
		return nil
	}); err != nil {
		t.Fatalf("got %v", err)
	}
	// Actor 還能服務。
	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "z"}}); err != nil {
		t.Fatal(err)
	}
}

func TestClosedHub(t *testing.T) {
	env := newTestHub(t, nil)
	env.h.Close()
	var herr *Error
	for _, err := range []error{
		func() error {
			_, _, e := env.h.CreateTask(context.Background(), LocalOwner, t.TempDir(), task.CreateTask{Title: "t"})
			return e
		}(),
		func() error {
			_, e := env.h.Command(context.Background(), LocalOwner, "t-1", task.Command{Kind: task.CmdArchive})
			return e
		}(),
		env.h.Subscribe(context.Background(), LocalOwner, "t-1", 0, func(Update) error { return nil }),
	} {
		if !errors.As(err, &herr) || herr.Code != CodeClosed {
			t.Fatalf("got %v", err)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out in waitFor")
}

// --- MemStore 單元測試 ---

func TestMemStoreLog(t *testing.T) {
	ctx := context.Background()
	l := NewMemStore()
	rec := func(s task.Seq) Record {
		return Record{Seq: s, Kind: task.TaskCreatedKind, At: time.Now(), Payload: []byte("x")}
	}
	if err := l.Append(ctx, "t-1", 0, Batch{Records: []Record{rec(1), rec(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ctx, "t-1", 0, Batch{Records: []Record{rec(3)}}); err == nil {
		t.Fatal("concurrent write accepted")
	}
	if err := l.Append(ctx, "t-1", 2, Batch{Records: []Record{rec(3)}}); err != nil {
		t.Fatal(err)
	}
	got, _ := l.Read(ctx, "t-1", 1, 1)
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("read %+v", got)
	}
	got, _ = l.Read(ctx, "t-1", 0, 0)
	if len(got) != 3 {
		t.Fatalf("read %+v", got)
	}
	// Redact 替換 payload。
	if err := l.Append(ctx, "t-1", 3, Batch{
		Records: []Record{rec(4)},
		Redact:  []Record{{Seq: 2, Payload: []byte("REDACTED")}},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = l.Read(ctx, "t-1", 1, 1)
	if string(got[0].Payload) != "REDACTED" {
		t.Fatalf("payload %q", got[0].Payload)
	}
	ids, _ := l.Streams(ctx)
	if len(ids) != 1 || ids[0] != "t-1" {
		t.Fatalf("streams %v", ids)
	}
	l.Delete(ctx, "t-1")
	ids, _ = l.Streams(ctx)
	if len(ids) != 0 {
		t.Fatalf("streams %v", ids)
	}
}

func TestMemStoreConfig(t *testing.T) {
	ctx := context.Background()
	c := NewMemStore()
	c.PutWorkspace(ctx, Workspace{ID: "w-1", Path: "/a"})
	c.PutWorkspace(ctx, Workspace{ID: "w-1", Path: "/b"})
	ws, _ := c.Workspaces(ctx)
	if len(ws) != 1 || ws[0].Path != "/b" {
		t.Fatalf("workspaces %+v", ws)
	}
	c.PutRule(task.Rule{ID: "r1", Pattern: "x"})
	r, _ := c.Rules(ctx)
	if len(r) != 1 {
		t.Fatal("rule missing")
	}
	c.DeleteRule(ctx, "r1")
	r, _ = c.Rules(ctx)
	if len(r) != 0 {
		t.Fatal("rule not deleted")
	}
	x := DefaultSettings()
	x.Notify.Ask = false
	c.PutSettings(ctx, x)
	s, _ := c.Settings(ctx)
	if s.Notify.Ask {
		t.Fatal("settings lost")
	}
	// NewRules 按 ID upsert，不重複。
	b := Batch{NewRules: []task.Rule{{ID: "r1", Pattern: "a"}}}
	if err := c.Append(ctx, "t-1", 0, b); err != nil {
		t.Fatal(err)
	}
	b = Batch{NewRules: []task.Rule{{ID: "r1", Pattern: "b"}}}
	if err := c.Append(ctx, "t-1", 0, b); err != nil {
		t.Fatal(err)
	}
	rules, _ := c.Rules(ctx)
	if len(rules) != 1 || rules[0].Pattern != "b" {
		t.Fatalf("rules %+v", rules)
	}
}
