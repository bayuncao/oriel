package hub

import (
	"context"
	"testing"

	"github.com/bayuncao/oriel/internal/task"
)

// L25、L26、L34、L35（m2.md）：真的一輪結束後，Snapshot 的 Session 有 LastTurn（結局、時間、最後一條正文，
// 之後的思考不算）；經 redact_message 遮蔽後正文清掉；重啟後從日誌折疊出同樣的記錄。
func TestLastTurnThroughHub(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say first; think a; say last; think b", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	var last task.MessageID
	done := w.until(func(e task.Event) bool {
		if e.Kind == task.MessageAppendedKind && e.MessageAppended.Channel == task.ChannelText {
			last = e.MessageAppended.Message
		}
		return e.Kind == task.TurnCompletedKind
	})
	lt := snapshotOf(t, env.h, id).State.Sessions[sid].LastTurn
	if lt == nil || lt.Result != task.TurnOK || !lt.EndedAt.Equal(done.At) || lt.Reply == nil ||
		lt.Reply.Message != last || lt.Reply.Text != "last" || lt.Reply.Redacted {
		t.Fatalf("L25 last turn %+v (reply %+v), want completed at %v with %q", lt, replyIn(lt), done.At, "last")
	}

	if _, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdRedactMessage, Session: sid, RedactMessage: &task.RedactMessage{Message: last}}); err != nil {
		t.Fatal(err)
	}
	w.next(task.MessageRedactedKind)
	want := task.Reply{Message: last, Redacted: true}
	if r := replyIn(snapshotOf(t, env.h, id).State.Sessions[sid].LastTurn); r == nil || *r != want {
		t.Fatalf("L34 reply after redacting %+v, want %+v", r, want)
	}

	w.cancel()
	env.h.Close()
	env.h = env.restart(t)
	s, err := env.h.Snapshot(ctxb(), LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	after := s.Sessions[sid].LastTurn
	if after == nil || after.Result != task.TurnOK || !after.EndedAt.Equal(done.At) || replyIn(after) == nil || *replyIn(after) != want {
		t.Fatalf("L35 after restart %+v (reply %+v)", after, replyIn(after))
	}
}

func replyIn(r *task.TurnRecord) *task.Reply {
	if r == nil {
		return nil
	}
	return r.Reply
}
