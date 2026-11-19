package hub

import (
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// outBuf 是 D26 的輸出緩衝：同一 Session 同一頻道的增量文字。timer 是半落庫的計時器；
// gen 在每次停掉它時加一，讓停掉之前已經點火、回呼還在路上的計時器作廢（同 stallTimer）。
type outBuf struct {
	channel task.ChannelKind
	text    strings.Builder
	timer   Timer
	gen     uint64
}

// stopTimer 停掉半落庫的計時器，並讓已經在路上的點火作廢。
func (b *outBuf) stopTimer() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.gen++
}

// coalesce 把轉接器的原始事實整理成送進 Decide 的命令：
// output 增量累積起來、換頻道或訊息邊界時才沖出（D26）。
func (a *actor) coalesce(c task.Command) []task.Command {
	sid := c.Session
	switch c.Kind {
	case task.FactOutput:
		if !task.AcceptsOutput(a.state, sid) {
			a.h.logger.Debug("hub: late output dropped",
				"task", a.id, "session", sid)
			return nil
		}
		b := a.bufs[sid]
		if b == nil {
			b = &outBuf{}
			a.bufs[sid] = b
		}
		var out []task.Command
		if b.text.Len() > 0 && b.channel != c.Output.Channel {
			out = append(out, a.flushCmd(sid, b, false))
		}
		b.channel = c.Output.Channel
		b.text.WriteString(c.Output.Text)
		if b.timer == nil {
			gen := b.gen
			b.timer = a.h.timer(5*time.Second, func() {
				select {
				case a.mail <- msg{done: &effectDone{
					apply: func(a *actor) { a.flushPartial(sid, b, gen) }}}:
				case <-a.stop:
				}
			})
		}
		return out // 增量本身不進 Decide
	case task.FactToolStarted, task.FactTurnEnded, task.FactExited:
		var out []task.Command
		if b := a.bufs[sid]; b != nil {
			if b.text.Len() > 0 {
				out = append(out, a.flushCmd(sid, b, false))
			}
			b.stopTimer()
		}
		return append(out, c)
	}
	return []task.Command{c}
}

// flushCmd 把緩衝內容做成一個 output 事實並清空緩衝；頻道保留。
// 清空會在這批事件之後對每個訂閱者補一個 Reset（B4-3）。
func (a *actor) flushCmd(sid task.SessionID, b *outBuf, partial bool) task.Command {
	text := b.text.String()
	b.text.Reset()
	a.markPendingReset(sid, b.channel)
	return task.Command{Kind: task.FactOutput, Origin: task.OriginRuntime, Session: sid,
		Output: &task.Output{Channel: b.channel, Text: text, Partial: partial}}
}

// flushPartial 計時器到期：以 Partial 送出累積的文字（訊息不結束）。b 與 gen 是排定時的緩衝與世代：
// 期間計時器被停掉（gen 變了），或緩衝被丟掉又重建（不是同一個 b），這次點火作廢。
func (a *actor) flushPartial(sid task.SessionID, b *outBuf, gen uint64) {
	if a.bufs[sid] != b || b.gen != gen {
		return
	}
	b.timer = nil
	if b.text.Len() == 0 {
		return
	}
	a.command(a.flushCmd(sid, b, true), a.h.inputs(a.wsid), nil, nil)
}

// markPendingReset 記一筆待送的 Reset；同一批清空只留頻道。
func (a *actor) markPendingReset(sid task.SessionID, ch task.ChannelKind) {
	if a.pendingReset == nil {
		a.pendingReset = map[task.SessionID]task.ChannelKind{}
	}
	a.pendingReset[sid] = ch
}
