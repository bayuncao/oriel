package hub

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// actor 是一個載入的 Task：它的 goroutine 是唯一的寫入者（A1）。
type actor struct {
	h  *Hub
	id task.TaskID

	wsid  task.WorkspaceID // 建立後不變
	state task.State
	live  task.Live

	conns        map[task.SessionID]Conn
	timers       map[task.AskKey]Timer
	stalls       map[task.SessionID]*stallTimer // 回合沒有動靜的計時（D79）
	subs         map[*subscriber]struct{}
	pendingReset map[task.SessionID]task.ChannelKind // 本批已清空的輸出緩衝
	bufs         map[task.SessionID]*outBuf          // D26 緩衝
	segments     map[task.MessageID][]segment        // 遮蔽索引
	labelSeq     task.Seq                            // 目前標籤檢索列的 Seq
	drafts       searchDrafts                        // 還沒結束的回答的文字（檢索列在結束時寫，D70）
	inflight     sync.WaitGroup                      // Effect goroutine

	// deletes 是等著執行的刪除（DeleteTask）：連線關完、Effect 都回來才刪（m2.md DL06）。
	deletes []chan<- cmdResult
	// removed 在 Task 刪除後、gone 關閉前設上；只在 gone 關閉後由別的 goroutine 讀。
	removed bool

	mail     chan msg
	stop     chan struct{} // halt 時關閉
	stopOnce sync.Once
	gone     chan struct{} // loop 結束時關閉（hub 關閉、建立被拒、Task 被刪除）
}

// halt 要 actor 停下；hub 關閉與建立被拒可能同時要停同一個，停幾次都只關一次 stop。
func (a *actor) halt() { a.stopOnce.Do(func() { close(a.stop) }) }

type msg struct {
	cmd   *task.Command // 客戶端命令或執行期事實
	in    task.Inputs
	reply chan<- cmdResult // 事實為 nil
	ws    *Workspace       // CreateTask 的第一個 Append 帶上新 Workspace
	done  *effectDone      // Effect 結果
	sub   *subscriber      // 訂閱登記
	unsub *subscriber      // 訂閱取消
	snap  chan<- snapshot  // 測試與 OtherTasksRunning 用的快照
	stall *stallFire       // 沉默計時器點火（D79）
	del   chan<- cmdResult // 刪除 Task（DeleteTask）
	// guard 在 Decide 之前以目前的 State 核對 hub 自己的前提（如一個 Task
	// 最多一個 agent）；回錯就不跑命令，原樣回給呼叫者。只用於客戶端命令。
	guard func(task.State) error
}

type cmdResult struct {
	version task.Seq
	err     error
}

// effectDone 是 Effect goroutine 回報的結果。
type effectDone struct {
	key   task.EffectKey
	apply func(*actor)
	facts []task.Command
}

// segment 是訊息的一個片段在 Log 裡的位置。
type segment struct {
	Seq     task.Seq
	At      time.Time
	Session task.SessionID
	Turn    task.TurnID
	Channel task.ChannelKind
	Partial bool
}

// subscriber 是一條訂閱：補缺口後推即時 Update。
// done 關閉後推送側不再阻塞（訂閱者已離開）；lagging 表示被踢出；err 是登記時缺口補不上的原因。
// lagging 與 err 都在關閉 ch 之前寫，訂閱者讀到 ch 關閉後才讀。
// stale 記錄丟過增量的 Session（B4-5）：下次給它的第一筆增量
// 改成整段緩衝的 Reset。
type subscriber struct {
	ch      chan Update
	done    chan struct{}
	last    task.Seq // 已送達的 Seq
	lagging bool
	err     error
	stale   map[task.SessionID]struct{}
}

// snapshot 是 State（寫時複製）與 Live（淺複製）的讀取快照。
type snapshot struct {
	State task.State
	Live  task.Live
}

func newActor(h *Hub, id task.TaskID, s task.State, live task.Live,
	segs map[task.MessageID][]segment) *actor {
	return &actor{
		h: h, id: id, wsid: s.Workspace, state: s, live: live,
		conns:    map[task.SessionID]Conn{},
		timers:   map[task.AskKey]Timer{},
		stalls:   map[task.SessionID]*stallTimer{},
		subs:     map[*subscriber]struct{}{},
		bufs:     map[task.SessionID]*outBuf{},
		segments: segs,
		mail:     make(chan msg, 64),
		stop:     make(chan struct{}),
		gone:     make(chan struct{}),
	}
}

// send 把 msg 放入郵箱；ctx 結束（hub 關閉）回 ErrClosed，actor 已結束回 goneErr。
func (a *actor) send(ctx interface{ Done() <-chan struct{} }, m msg) error {
	select {
	case a.mail <- m:
		return nil
	case <-ctx.Done():
		return ErrClosed
	case <-a.gone:
		return a.goneErr()
	}
}

// goneErr 是 actor 結束後給等著它的呼叫者的錯誤：Task 被刪除是 unknown_task，其餘是 hub 關閉。
// 只在 gone 關閉後呼叫。
func (a *actor) goneErr() error {
	if a.removed {
		return fail(CodeUnknownTask, nil)
	}
	return ErrClosed
}

// awaitCtx 同 await，另在 ctx 結束時回 ctx.Err()（不再等；已排進 Actor 的事照樣做）。
func awaitCtx[T any](ctx context.Context, a *actor, ch <-chan T) (T, error) {
	select {
	case <-ctx.Done():
		select {
		case v := <-ch:
			return v, nil
		default:
		}
		var zero T
		return zero, ctx.Err()
	default:
	}
	select {
	case v := <-ch:
		return v, nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case <-a.gone:
		select {
		case v := <-ch:
			return v, nil
		default:
		}
		var zero T
		return zero, a.goneErr()
	}
}

// await 等 actor 的回覆；actor 先結束時回 goneErr。回覆與結束同時就緒時以回覆為準
// （刪除 Task 的回覆送出後 actor 隨即結束）。
func await[T any](a *actor, ch <-chan T) (T, error) {
	select {
	case v := <-ch:
		return v, nil
	case <-a.gone:
		select {
		case v := <-ch:
			return v, nil
		default:
		}
		var zero T
		return zero, a.goneErr()
	}
}

// running 回報有 Session 在運行；以 snap 訊息取（不能讀 a.state）。
// actor 停了或 hub 關了都視為不在跑，不阻塞。
func (a *actor) running() bool {
	ch := make(chan snapshot, 1)
	select {
	case a.mail <- msg{snap: ch}:
	case <-a.gone:
		return false
	case <-a.h.ctx.Done():
		return false
	}
	if a.h.hooks.snapQueued != nil {
		a.h.hooks.snapQueued(a.id)
	}
	select {
	case snap := <-ch:
		for _, x := range snap.State.Sessions {
			if x.Status == task.SessionRunning {
				return true
			}
		}
	case <-a.gone:
	case <-a.h.ctx.Done():
	}
	return false
}

// loop 是 actor 的主迴圈。讀任何訊息之前先跑一次 Plan，完成崩潰恢復（D47、D117）：
// 交給了已不在的行程的回合、接不回來的閒置 Session 在任何命令之前就記為退出。
func (a *actor) loop() {
	defer a.h.actorWg.Done()
	defer close(a.gone)
	a.plan()
	for {
		select {
		case <-a.stop:
			a.shutdown()
			return
		case m := <-a.mail:
			a.handle(m)
			if len(a.deletes) > 0 {
				a.tryDelete()
			}
			if a.removed {
				a.shutdown()
				return
			}
		}
	}
}

// shutdown 收掉計時器、連線、訂閱與進行中的 Effect。
func (a *actor) shutdown() {
	for _, t := range a.timers {
		t.Stop()
	}
	a.stopStalls()
	for _, b := range a.bufs {
		b.stopTimer()
	}
	for _, c := range a.conns {
		c.Close()
	}
	a.inflight.Wait()
	for sub := range a.subs {
		close(sub.ch)
	}
}

func (a *actor) handle(m msg) {
	switch {
	case m.snap != nil:
		// State 是寫時複製（Evolve 只回傳新值），取值即可讀；
		// Live 的 map 是變動的，淺複製一份。
		l := a.live
		l.Conns = maps.Clone(a.live.Conns)
		l.Worktrees = maps.Clone(a.live.Worktrees)
		l.Timers = maps.Clone(a.live.Timers)
		l.InFlight = maps.Clone(a.live.InFlight)
		l.Lost = maps.Clone(a.live.Lost)
		m.snap <- snapshot{State: a.state, Live: l}
	case m.sub != nil:
		a.register(m.sub)
	case m.unsub != nil:
		delete(a.subs, m.unsub)
	case m.done != nil:
		a.finishEffect(m.done)
	case m.stall != nil:
		a.stalled(m.stall)
	case m.del != nil:
		if err := a.deletable(); err != nil {
			m.del <- cmdResult{version: a.state.Version, err: err}
			return
		}
		a.deletes = append(a.deletes, m.del)
		if a.h.hooks.deleteQueued != nil && (len(a.live.InFlight) > 0 || len(a.conns) > 0) {
			a.h.hooks.deleteQueued(a.id)
		}
	default:
		if m.reply == nil {
			if a.live.InFlight[closeKey(m.cmd.Session)] {
				// hub 自己正在關的連線回報的（D117）：Close 等它的事實都入隊才回來，之後的 start_conn 才分派，
				// 所以這時這個 Session 的事實只可能來自它。
				a.h.logger.Debug("hub: fact from a closing conn dropped", "task", a.id, "session", m.cmd.Session, "kind", m.cmd.Kind)
				return
			}
			a.fact(*m.cmd, m.in)
			return
		}
		if m.guard != nil {
			if err := m.guard(a.state); err != nil {
				m.reply <- cmdResult{version: a.state.Version, err: err}
				return
			}
		}
		if err := checkAttachments(a.state, *m.cmd, a.live.WorkspaceDir); err != nil {
			m.reply <- cmdResult{version: a.state.Version, err: err}
			return
		}
		a.command(*m.cmd, m.in, m.reply, m.ws)
	}
}

// command 跑一次 Decide → 投影 → Append → 換狀態 → 推送 → Plan。
func (a *actor) command(c task.Command, in task.Inputs, reply chan<- cmdResult, ws *Workspace) {
	// 本命令期間清空的輸出緩衝：Reset 跟在它的事件後面，
	// 而且每條路徑都要送（早退也不能漏，B4-3）。
	defer a.drainResets()
	// 遮蔽前先查片段索引；不在 Decide 裡，因為那是純領域。
	if c.Kind == task.CmdRedactMessage &&
		len(a.segments[c.RedactMessage.Message]) == 0 {
		if reply != nil {
			reply <- cmdResult{version: a.state.Version,
				err: fail(CodeUnknownMessage, nil,
					"message", string(c.RedactMessage.Message))}
		}
		return
	}
	events, err := task.Decide(a.state, in, c)
	if err != nil {
		if reply != nil {
			reply <- cmdResult{version: a.state.Version, err: err}
		} else {
			a.h.logger.Debug("hub: fact dropped", "task", a.id, "kind", c.Kind, "err", err)
		}
		return
	}
	if len(events) == 0 {
		if reply != nil {
			reply <- cmdResult{version: a.state.Version}
		}
		return
	}
	// 先折疊出新 State 算投影，Append 成功才換掉 a.state。
	old := a.state
	batch, err := a.encodeBatch(events, ws)
	if err != nil {
		if reply != nil {
			reply <- cmdResult{version: a.state.Version, err: err}
		}
		return
	}
	next, drafts, views := projections(a.state, a.drafts, events, a.live.WorkspaceDir)
	batch.Views = views
	if c.Kind == task.CmdRedactMessage {
		a.buildRedact(c.RedactMessage.Message, &batch)
	}
	// TaskEdited 換掉標籤檢索列：舊 Seq 移出索引（D64）。
	for _, e := range events {
		if e.Kind == task.TaskEditedKind && a.labelSeq != 0 {
			batch.Views.Unsearch = append(batch.Views.Unsearch, a.labelSeq)
		}
	}
	if err := a.h.log.Append(a.h.ctx, a.id, a.state.Version, batch); err != nil {
		e := fail(CodeStoreFailed, err, "detail", err.Error())
		if reply != nil {
			reply <- cmdResult{version: a.state.Version, err: e}
		} else {
			a.h.logger.Debug("hub: append failed", "task", a.id, "err", e)
		}
		return
	}
	a.h.cacheApply(batch)
	a.state, a.drafts = next, drafts
	for _, e := range events {
		if isLabel(e) {
			a.labelSeq = e.Seq
		}
	}
	a.liveFromEvents(old, events)
	a.recordModels(events)
	a.notify(events)
	a.watchStalls(events)
	a.h.offerTaskView(batch.Views.Task)
	for _, e := range events {
		if e.Kind == task.MessageAppendedKind && e.MessageAppended != nil {
			m := e.MessageAppended
			a.segments[m.Message] = append(a.segments[m.Message], segment{
				Seq: e.Seq, At: e.At, Session: e.Session,
				Turn: m.Turn, Channel: m.Channel, Partial: m.Partial})
		}
	}
	for sub := range a.subs {
		for _, e := range events {
			if e.Seq > sub.last {
				a.push(sub, e)
			}
		}
	}
	if reply != nil {
		reply <- cmdResult{version: a.state.Version}
	}
	a.plan()
}

// encodeBatch 把事件編成 Record；規則事件進 NewRules。
func (a *actor) encodeBatch(events []task.Event, ws *Workspace) (Batch, error) {
	b := Batch{NewWorkspace: ws}
	for _, e := range events {
		payload, err := task.EncodeEvent(e)
		if err != nil {
			return b, fail(CodeStoreFailed, err, "detail", err.Error())
		}
		b.Records = append(b.Records, Record{Seq: e.Seq, Kind: e.Kind, At: e.At, Payload: payload})
		if e.RuleAdded != nil &&
			(e.RuleAdded.Scope == task.ScopeWorkspace || e.RuleAdded.Scope == task.ScopeGlobal) {
			b.NewRules = append(b.NewRules, *e.RuleAdded)
		}
	}
	return b, nil
}

// liveFromEvents 依事件維護 Live（見 domain.md Plan 的 Live）。
func (a *actor) liveFromEvents(old task.State, events []task.Event) {
	for _, e := range events {
		sid := e.Session
		switch e.Kind {
		case task.ProcessExitedKind:
			delete(a.live.Conns, sid)
			delete(a.live.Lost, sid)
			if c := a.conns[sid]; c != nil {
				delete(a.conns, sid)
				go c.Close()
			}
			a.dropBuf(sid)
		case task.SessionClosedKind:
			a.dropBuf(sid)
		case task.ForkMergedKind, task.ForkDiscardedKind:
			if f := old.Sessions[sid].Fork; f != nil {
				delete(a.live.Worktrees, f.Path)
			}
		case task.WorktreeMergedKind, task.WorktreeDiscardedKind:
			if old.Worktree != nil {
				delete(a.live.Worktrees, old.Worktree.Path)
			}
		}
	}
}

// dropBuf 丟掉 Session 的輸出緩衝與它的計時器，並記一筆
// 待送的 Reset（B4-3：緩衝被丟棄也算清空）。
func (a *actor) dropBuf(sid task.SessionID) {
	if b := a.bufs[sid]; b != nil {
		b.stopTimer()
		delete(a.bufs, sid)
		a.markPendingReset(sid, b.channel)
	}
}

// buildRedact 把某訊息的所有片段改寫成遮蔽版本，Seqs 進
// Views.Unsearch。
func (a *actor) buildRedact(mid task.MessageID, b *Batch) {
	for _, seg := range a.segments[mid] {
		payload, err := task.EncodeEvent(task.Event{
			Seq: seg.Seq, Kind: task.MessageAppendedKind, At: seg.At, Session: seg.Session,
			MessageAppended: &task.MessageAppended{
				Message: mid, Turn: seg.Turn, Channel: seg.Channel,
				Partial: seg.Partial, Redacted: true}})
		if err != nil {
			continue
		}
		b.Redact = append(b.Redact, Record{Seq: seg.Seq,
			Kind: task.MessageAppendedKind, At: seg.At, Payload: payload})
		b.Views.Unsearch = append(b.Views.Unsearch, seg.Seq)
	}
}

// register 補上缺口後把 subscriber 加入即時推送。缺口比它的緩衝大、補的途中就被踢出的
// 不加入：它拿到 hub.lagging，從最後收到的 Seq 重新訂閱。缺口讀不出來或解不開的也不加入
// （refuse）：加入了它會少看到那一段事件而不知道。
func (a *actor) register(sub *subscriber) {
	recs, err := a.h.log.Read(a.h.ctx, a.id, sub.last, 0)
	if err != nil {
		a.refuse(sub, err)
		return
	}
	for _, r := range recs {
		e, err := task.DecodeEvent(r.Seq, r.Kind, r.At, r.Payload)
		if err != nil {
			a.refuse(sub, err)
			return
		}
		a.push(sub, e)
		if sub.lagging {
			return
		}
	}
	a.subs[sub] = struct{}{}
	// 進行中緩衝的快照：新訂閱者先看見整段未落庫尾巴（B4-3）。
	for sid, b := range a.bufs {
		if b != nil && b.text.Len() > 0 {
			a.pushReset(sub, sid, b.channel, b.text.String())
		}
	}
}

// refuse 結束一條缺口補不上的訂閱：關 ch，Subscribe 回 hub.store_failed。已送達的事件照舊有效，
// 訂閱者可以從最後收到的 Seq 重新訂閱。
func (a *actor) refuse(sub *subscriber, err error) {
	sub.err = fail(CodeStoreFailed, err, "detail", err.Error())
	close(sub.ch)
}

// push 推送事件給訂閱者；buffer 滿了就踢掉（lagging），
// 訂閱者已走則丟棄。事件與 Reset 都是無損類（B4-5）。
func (a *actor) push(sub *subscriber, e task.Event) {
	if a.pushLossless(sub, Update{Event: &e}) {
		sub.last = e.Seq
	}
}

// pushLossless：滿了就踢出；訂閱者已走則丟棄。已踢出的（ch 已關閉）不再送：呼叫者在
// 同一個迴圈裡還會推給它這一批其餘的事件與 Reset。
func (a *actor) pushLossless(sub *subscriber, u Update) bool {
	if sub.lagging {
		return false
	}
	select {
	case sub.ch <- u:
		return true
	case <-sub.done:
		return false
	default:
		a.evict(sub)
		return false
	}
}

// pushReset 推送 Reset Delta（無損），並清掉該 Session 的 stale 標記。
func (a *actor) pushReset(sub *subscriber, sid task.SessionID,
	ch task.ChannelKind, text string) {
	delete(sub.stale, sid)
	a.pushLossless(sub, Update{Delta: &Delta{
		Session: sid, Channel: ch, Text: text, Reset: true}})
}

// pushDelta 推送文字增量（盡力而為，B4-5）：channel 不到半滿才放；
// 放不下就丟棄並把該 Session 標為 stale；stale 的 Session 下一筆
// 能放的增量換成整段緩衝的 Reset。增量永不踢出訂閱者。
func (a *actor) pushDelta(sub *subscriber, d Delta) {
	if sub.lagging {
		return
	}
	if len(sub.ch) >= cap(sub.ch)/2 {
		a.markStale(sub, d.Session)
		return
	}
	if _, ok := sub.stale[d.Session]; ok {
		delete(sub.stale, d.Session)
		var text string
		if b := a.bufs[d.Session]; b != nil {
			text = b.text.String()
		}
		a.pushLossless(sub, Update{Delta: &Delta{
			Session: d.Session, Channel: d.Channel,
			Text: text, Reset: true}})
		return
	}
	select {
	case sub.ch <- Update{Delta: &d}:
	case <-sub.done:
	default:
		a.markStale(sub, d.Session)
	}
}

// markStale 記錄該 Session 丟過增量（懶初始化）。
func (a *actor) markStale(sub *subscriber, sid task.SessionID) {
	if sub.stale == nil {
		sub.stale = map[task.SessionID]struct{}{}
	}
	sub.stale[sid] = struct{}{}
}

// drainResets 把 pendingReset 裡的 Reset 推給所有訂閱者。
func (a *actor) drainResets() {
	if len(a.pendingReset) == 0 {
		return
	}
	for sub := range a.subs {
		for sid, ch := range a.pendingReset {
			a.pushReset(sub, sid, ch, "")
		}
	}
	a.pendingReset = nil
}

// evict 踢掉慢訂閱者：關 ch、移出 subs、標 lagging。之後不能再往 ch 送（pushLossless、pushDelta 先看 lagging）。
func (a *actor) evict(sub *subscriber) {
	sub.lagging = true
	delete(a.subs, sub)
	close(sub.ch)
}

// finishEffect 記錄 Effect 完成，套用結果，再把衍生事實跑一遍（記下事件
// 的事實自己會跑 Plan）。結果改了 Live（apply：Prompt 送到了、worktree 建好了）
// 時再跑一次 Plan，等著它的 Effect（送出期間要求的取消、Session 的啟動）這時
// 才能分派。沒改 Live、事實也沒記下事件時不跑：同一個 Effect 不會立刻重發。
func (a *actor) finishEffect(d *effectDone) {
	delete(a.live.InFlight, d.key)
	if d.apply != nil {
		d.apply(a)
	}
	in := a.h.inputs(a.wsid)
	for _, c := range d.facts {
		a.fact(c, in)
	}
	if d.apply != nil {
		a.plan()
	}
}

// fact 是執行期事實的入口：經 D26 合併後跑 Decide；
// output 事實的增量在該事實的命令都處理完後推給訂閱者（B4-4）。
func (a *actor) fact(c task.Command, in task.Inputs) {
	accept := c.Kind == task.FactOutput &&
		task.AcceptsOutput(a.state, c.Session)
	if agentActivity(c.Kind) {
		// 先記下動靜再跑它的命令：命令改了回合或審批時，watchStalls 以改過的狀態再算一次。
		a.watchStall(c.Session, &c)
	}
	for _, cc := range a.coalesce(c) {
		a.command(cc, in, nil, nil)
	}
	if accept {
		for sub := range a.subs {
			a.pushDelta(sub, Delta{Session: c.Session,
				Channel: c.Output.Channel, Text: c.Output.Text})
		}
	}
}

// plan 比對 State 與 Live，分派該做的 Effect。report_exited 只是本地的事實，不經 goroutine 與郵箱，
// 在分派完之後就地跑（D117）：退出在下一則訊息（例如送訊息的命令）之前就記下了，不會先收下再失敗。
func (a *actor) plan() {
	var exits []task.Command
	for _, e := range task.Plan(a.state, a.live) {
		if e.Kind == task.EffReportExited {
			exits = append(exits, task.Command{Kind: task.FactExited, Origin: task.OriginRuntime, Session: e.Session,
				Exited: &task.Exited{Problem: task.Problem{Code: string(CodeConnectionLost)}}})
			continue
		}
		a.live.InFlight[e.Key] = true
		a.dispatch(e)
	}
	if len(exits) > 0 {
		in := a.h.inputs(a.wsid)
		for _, c := range exits {
			a.fact(c, in) // 記下事件的事實自己會再跑 Plan；同一個 Session 的第二次退出是冪等的
		}
	}
}

// closeKey 是 Session 的 close_conn 的 EffectKey。
func closeKey(sid task.SessionID) task.EffectKey {
	return task.EffectKey(string(task.EffCloseConn) + ":" + string(sid))
}

// deletable 核對 Task 能不能刪除：已歸檔，且沒有開著的 worktree（m2.md DL03、DL10）。
func (a *actor) deletable() error {
	if !a.state.Archived {
		return fail(CodeNotArchived, nil)
	}
	if a.state.OpenWorktrees() {
		return &task.Rejection{Reason: task.ReasonUnresolvedChanges}
	}
	return nil
}

// tryDelete 執行等著的刪除：還有 Effect 在路上（如歸檔時關的連線，DL06）就等它回來再試；
// 期間的命令照常處理，所以先再核對一次（DL07：先到的取消歸檔讓刪除失敗）。刪除成功後設上
// removed，loop 隨即收掉 actor；之後的呼叫者拿到 unknown_task。
func (a *actor) tryDelete() {
	if len(a.live.InFlight) > 0 || len(a.conns) > 0 {
		return
	}
	waiting := a.deletes
	a.deletes = nil
	answer := func(err error) {
		for _, r := range waiting {
			r <- cmdResult{version: a.state.Version, err: err}
		}
	}
	if err := a.deletable(); err != nil {
		answer(err)
		return
	}
	if err := a.h.log.Delete(a.h.ctx, a.id); err != nil {
		answer(fail(CodeStoreFailed, err, "detail", err.Error()))
		return
	}
	a.removed = true
	a.h.mu.Lock()
	if a.h.actors[a.id] == a {
		delete(a.h.actors, a.id)
	}
	a.h.mu.Unlock()
	a.h.offerTaskView(&TaskView{ID: a.id, Removed: true})
	answer(nil)
}
