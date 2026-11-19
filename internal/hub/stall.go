package hub

// stall.go：回合長時間沒有動靜時通知（D79，m2.md 第 28 節 ST01–ST15）。每個有回合在跑的
// Session 一個計時器，只在 Actor 的記憶體裡，不記事件；agent 報來運行期事實就重新計時，
// 有待答的審批時不計時。判斷規則是純函式 stallStep，Actor 只負責排定與點火。

import (
	"maps"
	"strconv"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

const (
	// stallQuiet 是一般的時限：這麼久沒有動靜就通知。
	stallQuiet = 10 * time.Minute
	// stallTool 是有工具呼叫在執行（如跑測試）時的時限。
	stallTool = 30 * time.Minute
)

// stallState 是一個 Session 的沉默計時狀態。
type stallState struct {
	turn   task.TurnID                  // 計時的回合；空 = 沒有回合在跑
	tools  map[task.ToolCallID]struct{} // 這一輪還沒報完成或失敗的工具呼叫
	paused bool                         // 有待答的審批，暫停計時（ST05）
	armed  bool                         // 計時器在跑
	limit  time.Duration                // armed 時這一次的時限
}

// stallAction 是 stallStep 要 Actor 做的事。
type stallAction int

const (
	stallKeep stallAction = iota // 計時器照舊
	stallArm                     // 以 limit 重新排定（舊的作廢）
	stallStop                    // 停掉計時器
)

// agentActivity 回答事實是不是 agent 報來的動靜：hub 自己產生的（審批超時、Effect 失敗）
// 與回合、行程的結束不算。
func agentActivity(k task.CommandKind) bool {
	switch k {
	case task.FactOutput, task.FactToolStarted, task.FactToolUpdated, task.FactPlan,
		task.FactUsage, task.FactAsked, task.FactModelReported:
		return true
	}
	return false
}

// waitingAsk 回答 Session 有沒有還沒決定的審批。
func waitingAsk(sess task.Session) bool {
	for _, a := range sess.Pending {
		if a.Decision == nil {
			return true
		}
	}
	return false
}

// stallStep 依 Session 現在的回合（turn，空 = 沒有）、有沒有待答的審批，以及這一步 agent 報來的
// 事實（c，沒有則 nil）更新計時狀態，回傳要對計時器做什麼。
func stallStep(s stallState, turn task.TurnID, waiting bool, c *task.Command) (stallState, stallAction) {
	if turn == "" {
		// 回合結束（ST09）。
		act := stallKeep
		if s.armed {
			act = stallStop
		}
		return stallState{}, act
	}
	reset := false
	if s.turn != turn {
		// 新的回合：重新計時，上一輪的工具呼叫不算。
		s = stallState{turn: turn, armed: s.armed}
		reset = true
	}
	if c != nil && agentActivity(c.Kind) {
		reset = true // ST02
		s.tools = trackTool(s.tools, c)
	}
	if waiting {
		s.paused = true // ST05
		if s.armed {
			s.armed = false
			return s, stallStop
		}
		return s, stallKeep
	}
	if s.paused {
		s.paused = false // 審批有了決定，從這一刻重新計時（ST06）
		reset = true
	}
	if !reset {
		return s, stallKeep
	}
	s.armed = true
	s.limit = stallQuiet
	if len(s.tools) > 0 {
		s.limit = stallTool // ST03、ST04
	}
	return s, stallArm
}

// trackTool 記下工具呼叫的開始與結束，回傳新的集合；不改傳入的（stallStep 是純函式）。
func trackTool(tools map[task.ToolCallID]struct{}, c *task.Command) map[task.ToolCallID]struct{} {
	var id task.ToolCallID
	open := false
	switch {
	case c.Kind == task.FactToolStarted && c.ToolStarted != nil:
		id, open = c.ToolStarted.ID, true
	case c.Kind == task.FactToolUpdated && c.ToolUpdated != nil:
		id = c.ToolUpdated.ID
		open = c.ToolUpdated.Status == task.ToolPending || c.ToolUpdated.Status == task.ToolRunning
	default:
		return tools
	}
	if _, has := tools[id]; has == open {
		return tools // 沒有變化
	}
	next := maps.Clone(tools)
	if next == nil {
		next = map[task.ToolCallID]struct{}{}
	}
	if open {
		next[id] = struct{}{}
	} else {
		delete(next, id)
	}
	return next
}

// stallTimer 是 Actor 持有的一個 Session 的計時狀態與計時器。動靜很頻繁（每段輸出都算），所以不每次都換
// 計時器：只把期限往後移，計時器點火時期限還沒到就排到期限再點（stalled）；期限提前時才換一個。
// gen 讓已作廢的計時器點火時被忽略。
type stallTimer struct {
	state    stallState
	deadline time.Time // armed 時：到這一刻還沒有動靜就通知
	timer    Timer     // 排定在 due 點火；nil = 沒有
	due      time.Time
	gen      uint64
}

// stallFire 是計時器點火後送進郵箱的訊息。
type stallFire struct {
	session task.SessionID
	gen     uint64
}

// watchStall 在 Session 的狀態或 agent 的事實（c）改變後更新它的計時。只在 Actor 的 goroutine 上跑。
func (a *actor) watchStall(sid task.SessionID, c *task.Command) {
	t := a.stalls[sid]
	if t == nil {
		t = &stallTimer{}
	}
	var turn task.TurnID
	waiting := false
	if sess, ok := a.state.Sessions[sid]; ok {
		if sess.Turn != nil {
			turn = sess.Turn.ID
		}
		waiting = waitingAsk(sess)
	}
	next, act := stallStep(t.state, turn, waiting, c)
	t.state = next
	switch act {
	case stallArm:
		t.deadline = a.h.now().Add(next.limit)
		if t.timer == nil || t.deadline.Before(t.due) {
			a.scheduleStall(sid, t, t.deadline)
		}
	case stallStop:
		t.cancel()
	}
	if next.turn == "" {
		delete(a.stalls, sid)
		return
	}
	a.stalls[sid] = t
}

// scheduleStall 換掉 t 的計時器，改在 at 點火。
func (a *actor) scheduleStall(sid task.SessionID, t *stallTimer, at time.Time) {
	t.cancel()
	gen := t.gen
	t.due = at
	t.timer = a.h.timer(max(0, at.Sub(a.h.now())), func() {
		select {
		case a.mail <- msg{stall: &stallFire{session: sid, gen: gen}}:
		case <-a.stop:
		}
	})
}

// cancel 停掉計時器並讓已經在路上的點火作廢。
func (t *stallTimer) cancel() {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.gen++
}

// stalled 處理計時器點火：期限還沒到（期間有過動靜）就排到期限；到了才通知，之後不再排定，
// 直到 agent 又有動靜（ST07、ST08）。
func (a *actor) stalled(f *stallFire) {
	t := a.stalls[f.session]
	if t == nil || t.gen != f.gen || !t.state.armed {
		return // 作廢的計時器
	}
	t.timer = nil
	if a.h.now().Before(t.deadline) {
		a.scheduleStall(f.session, t, t.deadline)
		return
	}
	t.state.armed = false
	if a.h.notifier == nil {
		return
	}
	// 設置在滿時限的那一刻讀（ST11）。
	a.deliver(a.h.notifySettings(), Notification{Kind: NotifyStalled, Task: a.id, TaskTitle: a.state.Title, Session: f.session,
		Params: map[string]string{"agent": agentLabel(a.state.Sessions[f.session].Launch),
			"minutes": strconv.Itoa(int(t.state.limit / time.Minute))}})
}

// stopStalls 停掉所有計時器（Actor 關閉時，ST13）。
func (a *actor) stopStalls() {
	for _, t := range a.stalls {
		t.cancel()
	}
	clear(a.stalls)
}

// watchStalls 在一批事件提交後，重算其中每個 Session 的計時：回合開始與結束、審批的待答與決定（ST05、ST06、ST09）。
func (a *actor) watchStalls(events []task.Event) {
	var seen map[task.SessionID]bool
	for _, e := range events {
		if e.Session == "" || seen[e.Session] {
			continue
		}
		if seen == nil {
			seen = map[task.SessionID]bool{}
		}
		seen[e.Session] = true
		a.watchStall(e.Session, nil)
	}
}
