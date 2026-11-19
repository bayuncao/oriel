package hub

import (
	"context"

	"github.com/bayuncao/oriel/internal/task"
)

// Subscribe 依序回放 from 之後的事件給 sink，再推即時事件；
// ctx 結束回傳 nil，sink 失敗回傳它的錯誤，hub 關閉回傳 hub.closed，
// 事件讀不出來（回放時，或向 Actor 登記補缺口時）回傳 hub.store_failed。
func (h *Hub) Subscribe(ctx context.Context, _ Principal, id task.TaskID, from task.Seq, sink func(Update) error) error {
	if err := h.alive(); err != nil {
		return err
	}
	a, err := h.loadActor(ctx, id)
	if err != nil {
		return err
	}
	last := from
	for {
		recs, err := h.log.Read(ctx, id, last, 500)
		if err != nil {
			return fail(CodeStoreFailed, err, "detail", err.Error())
		}
		for _, r := range recs {
			e, err := task.DecodeEvent(r.Seq, r.Kind, r.At, r.Payload)
			if err != nil {
				return fail(CodeStoreFailed, err, "detail", err.Error())
			}
			if err := sink(Update{Event: &e}); err != nil {
				return err
			}
			last = e.Seq
		}
		if len(recs) < 500 {
			break
		}
	}
	sub := &subscriber{ch: make(chan Update, 256),
		done: make(chan struct{}), last: last}
	if err := a.send(h.ctx, msg{sub: sub}); err != nil {
		return err
	}
	defer func() {
		close(sub.done) // 先放行正在 push 的 actor，再取消登記
		a.send(h.ctx, msg{unsub: sub})
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case u, ok := <-sub.ch:
			if !ok {
				if sub.lagging {
					return fail(CodeLagging, nil)
				}
				if err := h.alive(); err != nil {
					return err
				}
				if sub.err != nil {
					return sub.err
				}
				// actor 收掉了訂閱：hub 關閉，或 Task 被刪除（DL16）。
				<-a.gone
				if a.removed {
					return a.goneErr()
				}
				return nil
			}
			if err := sink(u); err != nil {
				return err
			}
		}
	}
}

// taskWatcher 是一個 TaskViews 觀察者：滿了就踢出（lagging）。
type taskWatcher struct {
	ch      chan TaskView
	lagging bool // 踢出原因；關閉前寫
}

// offerTaskView 由 Actor 在 Append 成功後呼叫；絕不阻塞。
func (h *Hub) offerTaskView(v *TaskView) {
	if v == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for w := range h.watchers {
		select {
		case w.ch <- *v:
		default: // 滿了：踢出
			w.lagging = true
			close(w.ch)
			delete(h.watchers, w)
		}
	}
}

// WatchTasks 推送任務列表變化：先給目前的全集，之後每次
// 提交推一行（at-least-once；同一 ID 以 Version 較大者為準——UpdatedAt 是牆上時間，可能同刻或倒退）。
func (h *Hub) WatchTasks(ctx context.Context, _ Principal, sink func(TaskView) error) error {
	w := &taskWatcher{ch: make(chan TaskView, 256)}
	// 登記與 closed 檢查在同一把鎖下：Close 遍歷 watchers 也拿這把鎖，
	// 才不會漏掉剛登記的觀察者。
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	h.watchers[w] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.watchers, w)
		h.mu.Unlock()
	}()
	// 登記後才取初值，不會漏掉之間的變化（可能重複）。
	rows, err := h.index.TaskViews(ctx)
	if err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	for _, r := range rows {
		if err := sink(r); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case r, ok := <-w.ch:
			if !ok {
				if w.lagging {
					return fail(CodeLagging, nil)
				}
				return ErrClosed
			}
			if err := sink(r); err != nil {
				return err
			}
		}
	}
}
