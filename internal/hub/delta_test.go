package hub

// delta_test.go：B4 — 訂閱流裡未落庫的文字增量（D60）。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// upWatcher 收集整條 Update 流（事件與 Delta 都要）。
type upWatcher struct {
	t      *testing.T
	ups    chan Update
	done   chan error
	cancel context.CancelFunc
}

func watchUps(t *testing.T, h *Hub, id task.TaskID, from task.Seq) *upWatcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &upWatcher{t: t, ups: make(chan Update, 2048),
		done: make(chan error, 1), cancel: cancel}
	t.Cleanup(cancel)
	go func() {
		w.done <- h.Subscribe(ctx, LocalOwner, id, from, func(u Update) error {
			select {
			case w.ups <- u:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	return w
}

// until 等滿足 pred 的 Update；前面讀到的收進 seen。
func (w *upWatcher) until(pred func(Update) bool, seen *[]Update) Update {
	w.t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case u := <-w.ups:
			*seen = append(*seen, u)
			if pred(u) {
				return u
			}
		case err := <-w.done:
			w.t.Fatalf("subscription ended: %v", err)
		case <-deadline:
			w.t.Fatalf("timed out")
		}
	}
}

func isEvent(u Update, k task.EventKind) bool {
	return u.Event != nil && u.Event.Kind == k
}

func isDelta(u Update, ch task.ChannelKind, text string, reset bool) bool {
	return u.Delta != nil && u.Delta.Channel == ch &&
		u.Delta.Text == text && u.Delta.Reset == reset
}

// collect 讀到 kind 為止，回傳全部 Update。
func (w *upWatcher) collect(k task.EventKind) []Update {
	var seen []Update
	w.until(func(u Update) bool { return isEvent(u, k) }, &seen)
	return seen
}

// gate 釋放測試 agent 的 slow N 第 i 步。
func (env *testEnv) gate(t *testing.T, i int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(env.dir, "go-"+
		strconv.Itoa(i)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// gated 建一個等啟動柵門的 Task：agent 程序被攔在柵門前，
// 訂閱登記完成後才放行（start），增量才能逐筆對上。
func (env *testEnv) gated(t *testing.T, dir, brief string) task.TaskID {
	l := testLaunch()
	l.Env = append(l.Env, task.EnvVar{
		Name:  "ORIELHUB_TEST_START_GATE",
		Value: filepath.Join(env.dir, "start-gate")})
	return env.create(t, dir, brief, l)
}

// start 放行被攔的 agent。
func (env *testEnv) start(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(env.dir, "start-gate"),
		nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// D1：三個增量 → 完整落庫段 → Reset → TurnCompleted；
// 增量相接等於落庫文字。
func TestDeltaSlowTurn(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.gated(t, t.TempDir(), "slow 3")
	w := watchUps(t, env.h, id, 0)
	snapshotOf(t, env.h, id) // 訂閱登記完才放行：c1 是增量不是快照
	env.start(t)
	var got []Update
	w.until(func(u Update) bool { return isDelta(u, task.ChannelText, "c1", false) }, &got)
	env.gate(t, 1)
	w.until(func(u Update) bool { return isDelta(u, task.ChannelText, "c2", false) }, &got)
	env.gate(t, 2)
	w.until(func(u Update) bool { return isDelta(u, task.ChannelText, "c3", false) }, &got)
	env.gate(t, 3)
	w.until(func(u Update) bool { return isEvent(u, task.TurnCompletedKind) }, &got)
	var incs, persisted string
	order := []string{}
	for _, u := range got {
		if u.Delta != nil {
			if u.Delta.Reset {
				order = append(order, "reset")
			} else {
				incs += u.Delta.Text
				order = append(order, "delta:"+u.Delta.Text)
			}
		}
		if isEvent(u, task.MessageAppendedKind) {
			persisted += u.Event.MessageAppended.Text
			order = append(order, "appended")
		}
		if isEvent(u, task.TurnCompletedKind) {
			order = append(order, "completed")
		}
	}
	if incs != persisted || persisted != "c1c2c3" {
		t.Fatalf("incs %q persisted %q", incs, persisted)
	}
	want := []string{"delta:c1", "delta:c2", "delta:c3",
		"appended", "reset", "completed"}
	if !suffixMatch(order, want) {
		t.Fatalf("order %v", order)
	}
}

func suffixMatch(got, want []string) bool {
	i := len(got) - len(want)
	if i < 0 {
		return false
	}
	for j, w := range want {
		if got[i+j] != w {
			return false
		}
	}
	return true
}

// D2：換頻道：thinking 增量 → thinking 落庫 → Reset →
// text 增量 → text 落庫 → Reset。
func TestDeltaChannelSwitch(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.gated(t, t.TempDir(), "think a; say b")
	w := watchUps(t, env.h, id, 0)
	snapshotOf(t, env.h, id)
	env.start(t)
	got := w.collect(task.TurnCompletedKind)
	var order []string
	for _, u := range got {
		if u.Delta != nil {
			if u.Delta.Reset {
				order = append(order, "reset")
			} else {
				order = append(order, string(u.Delta.Channel)+":"+u.Delta.Text)
			}
		}
		if isEvent(u, task.MessageAppendedKind) {
			m := u.Event.MessageAppended
			order = append(order, string(m.Channel)+"|"+m.Text)
		}
	}
	want := []string{
		"thinking:a", // think 的增量
		"thinking|a", // 換頻道先沖出 thinking 段
		"reset",      // 沖出後 Reset
		"text:b",     // 新頻道的增量
		"text|b",     // 回合結束沖出 text 段
		"reset",
	}
	if !suffixMatch(order, want) {
		t.Fatalf("order %v", order)
	}
}

// D3：半落庫計時器：Partial 段 + Reset 後，後續增量照常；
// 段落相接等於全部增量。
func TestDeltaPartialFlush(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.gated(t, t.TempDir(), "slow 4")
	w := watchUps(t, env.h, id, 0)
	snapshotOf(t, env.h, id)
	env.start(t)
	var got []Update
	w.until(func(u Update) bool { return isDelta(u, task.ChannelText, "c1", false) }, &got)
	env.gate(t, 1)
	w.until(func(u Update) bool { return isDelta(u, task.ChannelText, "c2", false) }, &got)
	env.clock.Advance(6 * time.Second) // 觸發 5 秒半落庫
	w.until(func(u Update) bool {
		return isEvent(u, task.MessageAppendedKind) &&
			u.Event.MessageAppended.Partial
	}, &got)
	env.gate(t, 2)
	env.gate(t, 3)
	env.gate(t, 4)
	w.until(func(u Update) bool { return isEvent(u, task.TurnCompletedKind) }, &got)
	var incs, persisted string
	for _, u := range got {
		if u.Delta != nil && !u.Delta.Reset {
			incs += u.Delta.Text
		}
		if isEvent(u, task.MessageAppendedKind) {
			persisted += u.Event.MessageAppended.Text
		}
	}
	if incs != persisted || incs != "c1c2c3c4" {
		t.Fatalf("incs %q persisted %q", incs, persisted)
	}
}

// D4：緩衝中訂閱 → 快照 Reset 帶整段緩衝，之後增量照常。
func TestDeltaRegisterSnapshot(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.gated(t, t.TempDir(), "slow 3")
	w := watchUps(t, env.h, id, 0)
	snapshotOf(t, env.h, id)
	env.start(t)
	var first []Update
	w.until(func(u Update) bool { return isDelta(u, task.ChannelText, "c1", false) }, &first)
	env.gate(t, 1)
	w.until(func(u Update) bool { return isDelta(u, task.ChannelText, "c2", false) }, &first)
	// c3 被擋住：此刻緩衝是 "c1c2"，新訂閱者先看到它。
	w2 := watchUps(t, env.h, id, 0)
	var seen []Update
	u := w2.until(func(u Update) bool {
		return u.Delta != nil && u.Delta.Reset && u.Delta.Text != ""
	}, &seen)
	if u.Delta.Text != "c1c2" || u.Delta.Channel != task.ChannelText {
		t.Fatalf("snapshot %+v", u.Delta)
	}
	env.gate(t, 2)
	u = w2.until(func(u Update) bool {
		return u.Delta != nil && !u.Delta.Reset
	}, &seen)
	if u.Delta.Text != "c3" {
		t.Fatalf("increment %+v", u.Delta)
	}
	env.gate(t, 3)
	w2.until(func(u Update) bool { return isEvent(u, task.TurnCompletedKind) }, &seen)
}

// D5（B1）：卡住的訂閱者不會被增量踢出；丟過增量之後，它下一筆能放的
// 增量換成整段緩衝的 Reset；最後「落庫 + 尾巴」等於全部輸出。
// 每一步都等一個可觀察的事件，不靠時間：訂閱者收到第一筆回放之後
// 才送暖身的 EditTask，所以它的 TaskEdited 一定是登記後才到（即時或
// 補缺口）；X 拿到 c1、卡住之後才開門放出 c2…c151；agent 停在第 151
// 道門，c152 要等測試開門才發出；5 秒的分段落庫由假時鐘控制，不會觸發。
func TestDeltaBestEffort(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.gated(t, t.TempDir(), "slow 200")
	var seen []Update
	progress := watchUps(t, env.h, id, 0) // 正常訂閱 P：看進度
	progress.until(func(Update) bool { return true }, &seen)

	// 卡住的訂閱者 X：事件照收，第一筆增量起卡住，直到 release。
	release := make(chan struct{})
	var mu sync.Mutex
	var got []Update
	replayed := make(chan struct{})
	holding := make(chan struct{}) // X 已拿到 c1、卡在 release 上
	edits := make(chan string, 4)
	deltas := make(chan Delta, 1024)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctxb())
	defer cancel()
	go func() {
		first, blocked := true, false
		done <- env.h.Subscribe(ctx, LocalOwner, id, 0, func(u Update) error {
			if u.Delta != nil && !blocked {
				blocked = true
				close(holding)
				<-release
			}
			mu.Lock()
			got = append(got, u)
			mu.Unlock()
			if first {
				first = false
				close(replayed)
			}
			if u.Delta != nil {
				deltas <- *u.Delta
			}
			if isEvent(u, task.TaskEditedKind) {
				edits <- u.Event.TaskEdited.Title
			}
			return nil
		})
	}()
	edit := func(title string) {
		t.Helper()
		if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
			Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: title}}); err != nil {
			t.Fatal(err)
		}
	}
	waitEdit := func(want string) {
		t.Helper()
		select {
		case title := <-edits:
			if title != want {
				t.Fatalf("edit %q, want %q", title, want)
			}
		case err := <-done:
			t.Fatalf("blocked sub ended early: %v", err)
		case <-time.After(testWait):
			t.Fatalf("blocked sub never saw edit %q", want)
		}
	}
	select {
	case <-replayed:
	case <-time.After(testWait):
		t.Fatal("blocked sub got no replay")
	}
	edit("warm")
	waitEdit("warm")
	progress.until(func(u Update) bool { return isEvent(u, task.TaskEditedKind) }, &seen)

	// agent 發出 c1 後停在第 1 道門；X 拿到 c1 就卡住，通道是空的。
	// 這時才開第 1–150 道門：c2…c151 都在 X 卡住時推來，通道半滿
	// （128）之後的增量被丟棄，X 被標為 stale。
	env.start(t)
	select {
	case <-holding:
	case err := <-done:
		t.Fatalf("blocked sub ended early: %v", err)
	case <-time.After(testWait):
		t.Fatal("blocked sub never got c1")
	}
	for i := 1; i <= 150; i++ {
		env.gate(t, i)
	}
	// P 看到 c151：普通增量，或 P 落後時補來的整段 Reset（結尾也是 c151）。
	progress.until(func(u Update) bool {
		return u.Delta != nil && strings.HasSuffix(u.Delta.Text, "c151")
	}, &seen)
	snapshotOf(t, env.h, id) // actor 往返：c151 已推給 X

	// 放開 X，等它讀到 release 後的 TaskEdited：積壓的都讀完了，
	// 在它之前的增量也都已進了 deltas。
	close(release)
	edit("n")
	waitEdit("n")
	increments := 0
	for len(deltas) > 0 {
		if d := <-deltas; !d.Reset {
			increments++
		}
	}
	if increments == 0 || increments >= 151 {
		t.Fatalf("increments before release %d: nothing was dropped", increments)
	}

	// 開第 151 道門：c152 是 X 丟過增量後的第一筆，換成整段緩衝的 Reset。
	env.gate(t, 151)
	buffered := ""
	for i := 1; i <= 152; i++ {
		buffered += "c" + strconv.Itoa(i)
	}
	select {
	case d := <-deltas:
		if !d.Reset || d.Text != buffered {
			t.Fatalf("first delta after release %+v, want Reset of %d bytes", d, len(buffered))
		}
	case err := <-done:
		t.Fatalf("blocked sub ended early: %v", err)
	case <-time.After(testWait):
		t.Fatal("no delta after gate 151")
	}

	// 開其餘的門直到回合結束；再用一個 TaskEdited 當屏障：X 看到它時
	// 之前的 Update 都已收進 got。
	for i := 152; i <= 200; i++ {
		env.gate(t, i)
	}
	progress.collect(task.TurnCompletedKind)
	edit("end")
	waitEdit("end")
	mu.Lock()
	defer mu.Unlock()
	// 事件不漏不重：與 Log 逐筆比對。
	recs, err := env.store.Read(ctxb(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []task.Seq
	for _, u := range got {
		if u.Event != nil {
			seqs = append(seqs, u.Event.Seq)
		}
	}
	if len(seqs) != len(recs) {
		t.Fatalf("events %d, log %d", len(seqs), len(recs))
	}
	for i, r := range recs {
		if seqs[i] != r.Seq {
			t.Fatalf("event %d is seq %d, log has %d", i, seqs[i], r.Seq)
		}
	}
	// 客戶端規則：落庫文字 + 最後尾巴 == 全部輸出。
	var persisted, tail string
	for _, u := range got {
		if u.Delta != nil {
			if u.Delta.Reset {
				tail = u.Delta.Text
			} else {
				tail += u.Delta.Text
			}
		}
		if isEvent(u, task.MessageAppendedKind) {
			persisted += u.Event.MessageAppended.Text
		}
	}
	all := ""
	for i := 1; i <= 200; i++ {
		all += "c" + strconv.Itoa(i)
	}
	if persisted+tail != all {
		t.Fatalf("persisted+tail %d bytes, want %d",
			len(persisted+tail), len(all))
	}
}

// D6：緩衝中關 Session → TurnCancelled/SessionClosed 後 Reset("")；
// 之後晚到的 output 事實沒有 Delta 也沒有事件。
func TestDeltaLateOutputDropped(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.gated(t, t.TempDir(), "say x; hang")
	w := watchUps(t, env.h, id, 0)
	snapshotOf(t, env.h, id)
	env.start(t)
	var seen []Update
	w.until(func(u Update) bool { return isDelta(u, task.ChannelText, "x", false) }, &seen)
	sid := env.sessionOf(t, id)
	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdCloseSession, Session: sid,
		CloseSession: &task.CloseSession{}}); err != nil {
		t.Fatal(err)
	}
	w.until(func(u Update) bool { return isEvent(u, task.SessionClosedKind) }, &seen)
	u := w.until(func(u Update) bool {
		return u.Delta != nil && u.Delta.Reset && u.Delta.Session == sid
	}, &seen)
	if u.Delta.Text != "" {
		t.Fatalf("reset text %q", u.Delta.Text)
	}
	// 晚到的 output：直接送 actor 的 mail（reply=nil → fact 路徑）。
	a := env.h.actors[id]
	a.mail <- msg{cmd: &task.Command{Kind: task.FactOutput, Session: sid,
		Output: &task.Output{Channel: task.ChannelText, Text: "late"}}}
	// 屏障：EditTask 排在那筆事實後面（mail FIFO），它的
	// TaskEdited 到達時晚到事實已被處理。
	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "n"}}); err != nil {
		t.Fatal(err)
	}
	w.until(func(u Update) bool { return isEvent(u, task.TaskEditedKind) }, &seen)
	// Reset("") 之後到 TaskEdited 之間，sid 不能再有 Delta 或落庫段。
	var after bool
	for _, u := range seen {
		if !after {
			if u.Delta != nil && u.Delta.Reset && u.Delta.Session == sid {
				after = true
			}
			continue
		}
		if u.Delta != nil && u.Delta.Session == sid {
			t.Fatalf("late delta %+v", u.Delta)
		}
		if isEvent(u, task.MessageAppendedKind) && u.Event.Session == sid {
			t.Fatalf("late appended %+v", u.Event)
		}
	}
}

// D7：緩衝中 crash → 先沖出（落庫段），再 Reset、TurnFailed、
// ProcessExited。
func TestDeltaCrash(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.gated(t, t.TempDir(), "say x; crash")
	w := watchUps(t, env.h, id, 0)
	snapshotOf(t, env.h, id)
	env.start(t)
	var seen []Update
	w.until(func(u Update) bool { return isEvent(u, task.ProcessExitedKind) }, &seen)
	var order []string
	for _, u := range seen {
		if u.Delta != nil && u.Delta.Reset {
			order = append(order, "reset")
		}
		if u.Delta != nil && !u.Delta.Reset {
			order = append(order, "delta:"+u.Delta.Text)
		}
		if isEvent(u, task.MessageAppendedKind) {
			order = append(order, "appended")
		}
		if isEvent(u, task.TurnFailedKind) {
			order = append(order, "failed")
		}
		if isEvent(u, task.ProcessExitedKind) {
			order = append(order, "exited")
		}
	}
	want := []string{"delta:x", "appended", "reset", "failed", "exited"}
	if !suffixMatch(order, want) {
		t.Fatalf("order %v", order)
	}
}

// drainResets 在 command 的每條路徑都要跑（B4-3）：這裡讓一筆
// Reset 等著，再送一個 Decide 會拒絕的事實（早退路徑），
// Reset 仍要送達。
func TestDeltaResetOnRejectionPath(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.gated(t, t.TempDir(), "say x; hang")
	w := watchUps(t, env.h, id, 0)
	snapshotOf(t, env.h, id)
	env.start(t)
	var seen []Update
	w.until(func(u Update) bool { return isDelta(u, task.ChannelText, "x", false) }, &seen)
	sid := env.sessionOf(t, id)
	a := env.h.actors[id]
	// 緩衝清空記下的 Reset + 一個會被 Decide 拒絕的命令
	// （未知的 permission → Decide 錯誤 → reply==nil 的早退）。
	a.markPendingReset(sid, task.ChannelText)
	a.mail <- msg{cmd: &task.Command{Kind: task.CmdDecidePermission,
		Session: sid,
		DecidePermission: &task.DecidePermission{
			Permission: "p-none", Verdict: task.Allow}}}
	u := w.until(func(u Update) bool {
		return u.Delta != nil && u.Delta.Reset && u.Delta.Session == sid
	}, &seen)
	if u.Delta.Text != "" {
		t.Fatalf("reset text %q", u.Delta.Text)
	}
}
