package hub

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// loadedActor 是 Task 已載入的 actor。
func loadedActor(t *testing.T, env *testEnv, id task.TaskID) *actor {
	t.Helper()
	a, err := env.h.loadActor(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// attach 登記一條沒有人讀的訂閱（同 Subscribe 建的：容量 256），從 from 之後補起。
func attach(t *testing.T, env *testEnv, a *actor, from task.Seq) *subscriber {
	t.Helper()
	sub := &subscriber{ch: make(chan Update, 256), done: make(chan struct{}), last: from}
	if err := a.send(env.h.ctx, msg{sub: sub}); err != nil {
		t.Fatal(err)
	}
	return sub
}

// version 是 actor 處理完之前的訊息後的 State 版本。
func version(t *testing.T, env *testEnv, a *actor) task.Seq {
	t.Helper()
	snap := make(chan snapshot, 1)
	if err := a.send(env.h.ctx, msg{snap: snap}); err != nil {
		t.Fatal(err)
	}
	s, err := await(a, snap)
	if err != nil {
		t.Fatal(err)
	}
	return s.State.Version
}

// edit 送 n 個各記一個事件的命令。
func edit(t *testing.T, env *testEnv, id task.TaskID, n int) {
	t.Helper()
	for i := range n {
		if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
			Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "t" + strconv.Itoa(i)}}); err != nil {
			t.Fatal(err)
		}
	}
}

// evicted 讀完被踢出的訂閱：channel 已關閉、標了 lagging；回傳關閉前送達的筆數。
func evicted(t *testing.T, sub *subscriber) int {
	t.Helper()
	n := 0
	for {
		select {
		case _, ok := <-sub.ch:
			if !ok {
				if !sub.lagging {
					t.Fatal("closed without lagging")
				}
				return n
			}
			n++
		case <-time.After(testWait):
			t.Fatalf("subscriber not evicted after %d updates", n)
		}
	}
}

// S2：一批多個事件的第一個把慢訂閱者踢出後，同一批其餘的事件不再推給它；Actor 照常處理之後的命令。
func TestSubscriberEvictedMidBatch(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	a := loadedActor(t, env, id)
	sub := attach(t, env, a, version(t, env, a))
	edit(t, env, id, cap(sub.ch)) // 沒有人讀：緩衝滿了
	before := version(t, env, a)
	after, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{Kind: task.CmdCreateSession,
		CreateSession: &task.CreateSession{Launch: testLaunch(), Prompt: &task.Prompt{Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if after-before < 2 {
		t.Fatalf("the command recorded %d events; the scenario needs a batch of several", after-before)
	}
	if n := evicted(t, sub); n != cap(sub.ch) {
		t.Fatalf("delivered %d updates before the eviction, want %d", n, cap(sub.ch))
	}
	edit(t, env, id, 1)
}

// S3：登記時要補的缺口比緩衝大，訂閱者在補的途中被踢出：不再推給它，也不登記；Actor 照常處理之後的命令。
func TestSubscriberEvictedWhileCatchingUp(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	edit(t, env, id, 300)
	a := loadedActor(t, env, id)
	sub := attach(t, env, a, 0)
	version(t, env, a) // 登記處理完了才開始讀：補的途中沒有人讀
	if n := evicted(t, sub); n != cap(sub.ch) {
		t.Fatalf("delivered %d updates before the eviction, want %d", n, cap(sub.ch))
	}
	edit(t, env, id, 1) // 仍登記著的話，這個事件會推給已關閉的 channel
}

// S4：同一輪要推幾個 Reset，第一個把慢訂閱者踢出後，其餘的不再推給它。
func TestSubscriberEvictedByResets(t *testing.T) {
	sub := &subscriber{ch: make(chan Update, 1), done: make(chan struct{})}
	sub.ch <- Update{}
	a := &actor{subs: map[*subscriber]struct{}{sub: {}},
		pendingReset: map[task.SessionID]task.ChannelKind{"s-1": task.ChannelText, "s-2": task.ChannelText}}
	a.drainResets()
	if n := evicted(t, sub); n != 1 {
		t.Fatalf("delivered %d updates before the eviction, want 1", n)
	}
	if len(a.subs) != 0 {
		t.Fatal("the evicted subscriber is still registered")
	}
}

// P12：算 OtherTasksRunning 時，被問的 Actor 正在收一個 Effect 的結果（要讀設定快取）：兩邊都做得完。
func TestOtherRunningWhileActorFinishesEffect(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	a := loadedActor(t, env, id)
	w, ok := env.h.workspaceOf(a.wsid)
	if !ok {
		t.Fatal("workspace not registered")
	}
	queued := make(chan struct{}, 1)
	env.h.hooks.snapQueued = func(task.TaskID) { queued <- struct{}{} }
	entered, gate := make(chan struct{}), make(chan struct{})
	if err := a.send(env.h.ctx, msg{done: &effectDone{apply: func(*actor) { close(entered); <-gate }}}); err != nil {
		t.Fatal(err)
	}
	<-entered // Actor 停在 Effect 的結果上，還沒讀設定快取
	got := make(chan bool, 1)
	go func() { got <- env.h.otherRunning(w.Path, nil) }()
	<-queued    // 快照的要求排在那個結果後面
	close(gate) // Actor 接著讀設定快取，再回答快照
	select {
	case running := <-got:
		if running {
			t.Fatal("a task without sessions counted as running")
		}
	case <-time.After(testWait):
		t.Fatal("otherRunning and the actor are waiting on each other")
	}
}

// A10：hub 關閉與建立被拒同時要停同一個 Actor：停兩次也只停一次，Actor 照常收掉。
func TestActorStoppedTwice(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	a := loadedActor(t, env, id)
	env.h.dropActor(a)
	env.h.dropActor(a)
	select {
	case <-a.gone:
	case <-time.After(testWait):
		t.Fatal("actor still running")
	}
}
