package hub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// W1：初值全集後，EditTask 推新列。
func TestWatchTasks(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "hi", testLaunch())
	rows := make(chan TaskView, 16)
	ctx, cancel := context.WithCancel(ctxb())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- env.h.WatchTasks(ctx, LocalOwner, func(v TaskView) error {
			rows <- v
			return nil
		})
	}()
	w := watch(t, env.h, id)
	w.next(task.TurnCompletedKind)
	w.cancel()
	env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "new title"}})
	deadline := time.After(testWait)
	var last TaskView
	for last.Title != "new title" {
		select {
		case last = <-rows:
		case <-deadline:
			t.Fatalf("no pushed row; last %+v", last)
		}
	}
	cancel()
	<-done
}

// W2：觀察者太慢 → channel 填滿被踢出，回 hub.lagging；
// Command 從不被觀察者拖慢。
func TestWatchTasksEvicted(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "hi", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.TurnCompletedKind)
	w.cancel()
	release := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		done <- env.h.WatchTasks(ctxb(), LocalOwner, func(TaskView) error {
			once.Do(func() { close(blocked) })
			<-release // 初值第一筆就堵：ch 由 offer 端填滿
			return nil
		})
	}()
	// 等觀察者登記好並堵在初值上再提交：WatchTasks 先登記再推初值，所以 sink 被呼叫時已登記。
	// 不等的話（單核時常見）300 次提交可能都在登記之前，觀察者沒落後，永遠等下一行。
	<-blocked
	for i := range 300 {
		if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
			Kind: task.CmdEditTask,
			EditTask: &task.EditTask{
				Title: fmt.Sprintf("t%d", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	var he *Error
	if err := <-done; !errors.As(err, &he) || he.Code != CodeLagging {
		t.Fatalf("done %v", err)
	}
}

// W3：Close 後 WatchTasks 回 hub.closed。
func TestWatchTasksClosed(t *testing.T) {
	env := newTestHub(t, nil)
	env.create(t, t.TempDir(), "", testLaunch())
	env.h.Close()
	if err := env.h.WatchTasks(ctxb(), LocalOwner, func(TaskView) error {
		return nil
	}); err != ErrClosed {
		t.Fatalf("got %v", err)
	}
}
