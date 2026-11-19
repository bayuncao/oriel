package hub

import (
	"context"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// flushTimer 取走下一個排定的 5 秒輸出計時器；其他計時器（沉默計時）略過。
func flushTimer(t *testing.T, env *testEnv) *fakeTimer {
	t.Helper()
	for {
		select {
		case ft := <-env.clock.armed:
			if ft.d == 5*time.Second {
				return ft
			}
		case <-time.After(testWait):
			t.Fatal("no output flush timer armed")
			return nil
		}
	}
}

// partials 是 Log 裡記下的 Partial 片段的文字。
func partials(t *testing.T, env *testEnv, id task.TaskID) []string {
	t.Helper()
	recs, err := env.store.Read(context.Background(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		e, err := task.DecodeEvent(r.Seq, r.Kind, r.At, r.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == task.MessageAppendedKind && e.MessageAppended.Partial {
			out = append(out, e.MessageAppended.Text)
		}
	}
	return out
}

// F1、F2：輸出計時器的回呼已經在路上（點火時它剛被停掉）時，它的點火作廢：不冲出之後的緩衝。
// 之後的文字等它自己的計時器到期才以 Partial 冲出（F3 的正常情況見 TestPeriodicFlush）。
func TestStaleFlushTimerIgnored(t *testing.T) {
	for _, tc := range []struct {
		name string
		// reset 讓第一個計時器作廢：以工具呼叫冲出緩衝（F1），或整個丟掉緩衝（F2）。
		reset func(env *testEnv, a *actor, sid task.SessionID)
	}{
		{"F1 flushed by a tool call", func(env *testEnv, a *actor, sid task.SessionID) {
			env.h.emit(a, sid, task.Command{Kind: task.FactToolStarted,
				ToolStarted: &task.ToolCall{ID: "c1", Title: "run"}})
		}},
		{"F2 buffer dropped and recreated", func(env *testEnv, a *actor, sid task.SessionID) {
			a.mail <- msg{done: &effectDone{apply: func(a *actor) { a.dropBuf(sid) }}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := stallEnv(t, nil) // 每排定一個計時器都送進 env.clock.armed
			id, sid, w, _ := startHang(t, env)
			defer w.cancel()
			a := loadedActor(t, env, id)

			env.h.emit(a, sid, task.Command{Kind: task.FactOutput,
				Output: &task.Output{Channel: task.ChannelText, Text: "a"}})
			first := flushTimer(t, env)
			tc.reset(env, a, sid)
			env.h.emit(a, sid, task.Command{Kind: task.FactOutput,
				Output: &task.Output{Channel: task.ChannelText, Text: "b"}})
			flushTimer(t, env) // 第二個計時器：之後 Advance 讓它到期
			first.f()          // 停掉之前就已點火：回呼照樣把訊息送進郵箱
			snapshotOf(t, env.h, id)
			if got := partials(t, env, id); len(got) != 0 {
				t.Fatalf("the stale timer flushed %q", got)
			}

			env.clock.Advance(5 * time.Second)
			snapshotOf(t, env.h, id)
			if got := partials(t, env, id); len(got) != 1 || got[0] != "b" {
				t.Fatalf("partials %q, want [b]", got)
			}
		})
	}
}
