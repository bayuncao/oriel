package task

import (
	"cmp"
	"maps"
	"slices"
	"strings"
)

// newSession 建立 Session；fork 為真時同時規劃它的 Fork worktree。
func (d *decision) newSession(l LaunchSnapshot, role Role, fork bool) SessionID {
	id := sessionID(d.next())
	d.emit(Event{Kind: SessionCreatedKind, Session: id, SessionCreated: &SessionCreated{
		Launch: l, Role: role,
	}})
	if fork {
		w := d.forkWorktree(id)
		d.emit(Event{Kind: ForkCreatedKind, Session: id, ForkCreated: &w})
	}
	return id
}

// submit 把 Prompt 入隊（ID 由 Seq 衍生），Session 空閒時立即開始回合（D1）。
func (d *decision) submit(sid SessionID, p Prompt) {
	p.ID = promptID(d.next())
	d.emit(Event{Kind: PromptQueuedKind, Session: sid, PromptQueued: &p})
	d.startNext(sid)
}

// startNext 在空閒且隊列非空的 Session 上開始隊首 Prompt 的回合。
func (d *decision) startNext(sid SessionID) {
	x := d.s.Sessions[sid]
	if x.Status == SessionIdle && x.Turn == nil && len(x.Queue) > 0 {
		d.emit(Event{Kind: TurnStartedKind, Session: sid, TurnStarted: &TurnStarted{
			Turn: turnID(d.next()), Prompt: x.Queue[0].ID,
		}})
	}
}

// session 取出命令所指的 Session；不存在或已關閉時拒絕（closed 是終態，事實也不例外）。
func (d *decision) session(id SessionID) (Session, error) {
	x, ok := d.s.Sessions[id]
	if !ok {
		return Session{}, reject(ReasonUnknownSession)
	}
	if x.Status == SessionClosed {
		return Session{}, reject(ReasonSessionClosed)
	}
	return x, nil
}

// turnSession 是 output 類事實的唯一前提：Session 存在、
// 未關閉、回合進行中；錯誤保留各自的拒絕原因。
func turnSession(s State, sid SessionID) (Session, error) {
	x, ok := s.Sessions[sid]
	if !ok {
		return Session{}, reject(ReasonUnknownSession)
	}
	if x.Status == SessionClosed {
		return Session{}, reject(ReasonSessionClosed)
	}
	if x.Turn == nil {
		return x, invalid("no turn in progress")
	}
	return x, nil
}

// inTurn 取出事實所指、有回合進行中的 Session；
// 回合外的事實是轉接器或 hub 的程式錯誤。
func (d *decision) inTurn(sid SessionID) (Session, error) {
	return turnSession(d.s, sid)
}

// checkLaunch 確認 Launch Snapshot 至少有 agent 與可執行檔（hub 解析 Profile 的結果）。
func checkLaunch(l LaunchSnapshot) error {
	if l.Agent == "" || l.Executable == "" {
		return invalid("launch missing agent or executable")
	}
	return nil
}

// CheckPrompt 確認 Prompt 有內容（hub 在啟動 agent 前也用它先擋）：全是空白的文字也不算（D65），
// Text 本身原樣保留。
func CheckPrompt(p Prompt) error {
	if strings.TrimSpace(p.Text) == "" && len(p.Attachments) == 0 {
		return invalid("empty prompt")
	}
	return nil
}

func createSession(d *decision, c Command) error {
	p := c.CreateSession
	if err := checkLaunch(p.Launch); err != nil {
		return err
	}
	if p.Prompt != nil {
		if err := CheckPrompt(*p.Prompt); err != nil {
			return err
		}
	}
	id := d.newSession(p.Launch, RoleWorker, false)
	if p.Prompt != nil {
		d.submit(id, *p.Prompt)
	}
	return nil
}

func submitPrompt(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	p := c.Submit
	if x.Conflicted {
		return reject(ReasonConflicted)
	}
	if x.Status == SessionExited {
		return reject(ReasonSessionExited)
	}
	if err := CheckPrompt(p.Prompt); err != nil {
		return err
	}
	d.submit(c.Session, p.Prompt)
	return nil
}

func cancelTurn(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	if x.Turn == nil || x.Turn.Cancelling {
		return nil // 回合可能剛好結束：無操作
	}
	d.emit(Event{Kind: TurnCancelRequestedKind, Session: c.Session})
	return nil
}

func resume(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	if x.Status != SessionExited {
		return reject(ReasonNotResumable)
	}
	if x.Conflicted {
		return reject(ReasonConflicted)
	}
	if !x.resumable() {
		return reject(ReasonNotResumable)
	}
	r := &SessionResumed{}
	if x.Retry != nil {
		// agent 沒起來時失敗的那一輪：它的 Prompt 從沒送到 agent，重新排在隊首，以新的 ID（L56）。
		p := *x.Retry
		p.ID = promptID(d.next())
		r.Requeued = &p
	}
	d.emit(Event{Kind: SessionResumedKind, Session: c.Session, SessionResumed: r})
	d.startNext(c.Session) // 重新排上的與退出前排隊的 Prompt 在恢復後繼續
	return nil
}

func closeSession(d *decision, c Command) error {
	p := c.CloseSession
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	t := forkTarget(c.Session)
	open := x.openFork() && !d.hasIntent(t)
	if p.Resolution != nil {
		if c.Origin != OriginDesktop {
			return reject(ReasonNeedsDesktop)
		}
		if !open {
			return invalid("no open fork to resolve")
		}
		if err := d.requestResolution(t, *p.Resolution); err != nil {
			return err
		}
	} else if open {
		// 有未合併改動的 Fork（D3）：desktop 必須給處置，remote 先保留。
		if c.Origin != OriginRemote {
			return reject(ReasonUnresolvedChanges)
		}
		d.emit(Event{Kind: WorktreeKeptKind, Session: c.Session, WorktreeKept: &t})
	}
	d.close(c.Session)
	return nil
}

// closeIfOpen 關閉還沒關閉的 Session。
func (d *decision) closeIfOpen(sid SessionID) {
	if d.s.Sessions[sid].Status != SessionClosed {
		d.close(sid)
	}
}

// close 關閉 Session：結束進行中的回合（或取消未決 Ask），再記錄 SessionClosed。
func (d *decision) close(sid SessionID) {
	x := d.s.Sessions[sid]
	if x.Turn != nil {
		d.endTurn(sid, TurnCancelledKind, nil)
	} else {
		d.cancelAsks(sid)
	}
	d.emit(Event{Kind: SessionClosedKind, Session: sid})
}

func takeOver(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	if !x.Conflicted {
		return nil
	}
	d.emit(Event{Kind: SessionTakenOverKind, Session: c.Session})
	return nil
}

func nativeBound(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	p := c.Bound
	if x.Status == SessionExited {
		return reject(ReasonSessionExited)
	}
	if x.NativeID == p.NativeID && x.Bound {
		return nil
	}
	b := *p
	d.emit(Event{Kind: NativeSessionBoundKind, Session: c.Session, NativeSessionBound: &b})
	return nil
}

// AcceptsOutput 是 output 事實的前提（B4/D60），與 inTurn 共用
// turnSession：hub 依它丟棄晚到的增量；Decide 用它擋下同樣的事實。
func AcceptsOutput(s State, sid SessionID) bool {
	_, err := turnSession(s, sid)
	return err == nil
}

func output(d *decision, c Command) error {
	x, err := d.inTurn(c.Session)
	if err != nil {
		return err
	}
	p := c.Output
	if x.Open != "" && x.OpenChannel != p.Channel {
		d.closeOpen(c.Session) // D26：頻道切換結束目前的訊息
		x = d.s.Sessions[c.Session]
	}
	id := x.Open
	if id == "" {
		id = messageID(d.next())
	}
	d.emit(Event{Kind: MessageAppendedKind, Session: c.Session, MessageAppended: &MessageAppended{
		Message: id, Turn: x.Turn.ID, Channel: p.Channel, Text: p.Text, Partial: p.Partial,
	}})
	return nil
}

func toolStarted(d *decision, c Command) error {
	if _, err := d.inTurn(c.Session); err != nil {
		return err
	}
	d.closeOpen(c.Session) // D26：工具呼叫結束目前的訊息
	tc := *c.ToolStarted
	d.emit(Event{Kind: ToolCallStartedKind, Session: c.Session, ToolCallStarted: &tc})
	return nil
}

// record 是只把 payload 原樣記入事件的回合內事實。
func record[T any](kind EventKind, get func(*Command) *T, set func(*Event, *T)) func(*decision, Command) error {
	return func(d *decision, c Command) error {
		if _, err := d.inTurn(c.Session); err != nil {
			return err
		}
		v := *get(&c)
		e := Event{Kind: kind, Session: c.Session}
		set(&e, &v)
		d.emit(e)
		return nil
	}
}

func turnEnded(d *decision, c Command) error {
	if _, err := d.inTurn(c.Session); err != nil {
		return err
	}
	p := c.TurnEnded
	switch p.Result {
	case TurnOK:
		d.endTurn(c.Session, TurnCompletedKind, nil)
	case TurnCancelled:
		d.endTurn(c.Session, TurnCancelledKind, nil)
	case TurnError:
		if p.Problem == nil {
			return invalid("failed turn without problem")
		}
		d.endTurn(c.Session, TurnFailedKind, p.Problem)
	default:
		return invalid("unknown turn result %q", p.Result)
	}
	d.startNext(c.Session) // 回合結束後按 FIFO 消化隊列（D1）
	return nil
}

func exited(d *decision, c Command) error {
	p := c.Exited
	return d.processExited(c.Session, *p)
}

// processExited 記錄 agent 行程的結束：冪等、令進行中的回合失敗（D2）、取消未決 Ask。
// 連線類 Effect 的失敗也走這裡（start_conn 等）。
func (d *decision) processExited(sid SessionID, e Exited) error {
	x, err := d.session(sid)
	if err != nil {
		return err
	}
	if x.Status == SessionExited {
		return nil // 冪等：已記錄過退出
	}
	if x.Turn != nil {
		d.endTurn(sid, TurnFailedKind, &e.Problem) // D2：行程結束令回合失敗
	} else {
		d.cancelAsks(sid)
	}
	d.emit(Event{Kind: ProcessExitedKind, Session: sid, ProcessExited: &e})
	return nil
}

// setModel 選一個 agent 報告過的模型（m2.md V80–V82、V85，D118）：回合進行中不能換；
// 選的就是要換成的（或已經是的）時無操作。換的動作由 Plan 的 set_model 做。上一次選的還沒換好時不能再選（V82）：
// 不疊起來，set_model 的結果與失敗才一定是對 ModelWanted 的回答。
func setModel(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	if x.Conflicted {
		return reject(ReasonConflicted)
	}
	if x.Status == SessionExited {
		return reject(ReasonSessionExited)
	}
	if x.Turn != nil {
		return reject(ReasonSessionRunning)
	}
	v := c.SetModel.Value
	if !x.Model.offers(v) {
		return reject(ReasonModelUnavailable)
	}
	if v == cmp.Or(x.ModelWanted, x.Model.Current) {
		return nil
	}
	if x.wantsModel() {
		return reject(ReasonModelSwitching)
	}
	d.emit(Event{Kind: ModelChosenKind, Session: c.Session, ModelChosen: &SetModel{Value: v}})
	return nil
}

// modelReported 記下 agent 報告的模型設置（C45、C46，D118）：內容沒變不記；不是 Start 的報告與使用者選的不同時
// 跟著 agent（V84）。使用者選的已不在列表裡時改記 ModelSwitchFailed，保留 agent 的（V86）。
func modelReported(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	if x.Status == SessionExited {
		return nil // 行程已不在：晚到的報告
	}
	p := c.ModelReported
	if !x.Model.equal(p.Setting) || x.follows(*p) {
		r := ModelReport{Start: p.Start}
		if p.Setting != nil {
			m := *p.Setting
			m.Choices = slices.Clone(m.Choices)
			r.Setting = &m
		}
		d.emit(Event{Kind: ModelReportedKind, Session: c.Session, ModelReported: &r})
	}
	if x = d.s.Sessions[c.Session]; x.ModelWanted != "" && !x.Model.offers(x.ModelWanted) {
		d.emit(Event{Kind: ModelSwitchFailedKind, Session: c.Session,
			ModelSwitchFailed: &ModelProblem{Value: x.ModelWanted, Problem: Problem{Code: ReasonModelUnavailable.Code()}}})
	}
	d.defaultModel(c.Session, *p)
	return nil
}

// defaultModel 在新會話開始時換成預設模型（D158，settings.md MD05–MD08、MD12）：只看第一次啟動的 Start 報告
// （還沒有原生會話；Resume 的不算）。已經是它、或 agent 不報告模型時不做；在列表裡就選它（ModelChosen，Plan
// 換好之後才送出第一條 Prompt）；不在列表裡照 agent 自己的，記 ModelSwitchFailed 說明。
func (d *decision) defaultModel(sid SessionID, r ModelReport) {
	x := d.s.Sessions[sid]
	want := x.Launch.Model
	if !r.Start || x.NativeID != "" || want == "" || x.ModelWanted != "" || x.Model == nil || x.Model.Current == want {
		return
	}
	if x.Model.offers(want) {
		d.emit(Event{Kind: ModelChosenKind, Session: sid, ModelChosen: &SetModel{Value: want}})
		return
	}
	d.emit(Event{Kind: ModelSwitchFailedKind, Session: sid,
		ModelSwitchFailed: &ModelProblem{Value: want, Problem: Problem{Code: CodeDefaultModelUnavailable}}})
}

func sessionConflicted(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	if x.Conflicted {
		return nil
	}
	d.emit(Event{Kind: SessionConflictedKind, Session: c.Session})
	return nil
}

// closeOpen 結束 Session 尚未結束的訊息：補一段空的、Partial 為假的片段
// （hub 通常已先送出最後一段，這裡是保險）；空白結尾沿用原訊息的頻道。
func (d *decision) closeOpen(sid SessionID) {
	x := d.s.Sessions[sid]
	if x.Open != "" && x.Turn != nil {
		d.emit(Event{Kind: MessageAppendedKind, Session: sid, MessageAppended: &MessageAppended{
			Message: x.Open, Turn: x.Turn.ID, Channel: x.OpenChannel, Partial: false,
		}})
	}
}

// cancelAsks 取消 Session 全部未決（含已決定未送達）的 Ask，按 ID 排序以求確定。
func (d *decision) cancelAsks(sid SessionID) {
	x := d.s.Sessions[sid]
	for _, id := range slices.Sorted(maps.Keys(x.Pending)) {
		d.emit(Event{Kind: PermissionCancelledKind, Session: sid,
			PermissionCancelled: &PermissionRef{Permission: id}})
	}
}

// endTurn 結束目前的回合：先結束未完的訊息、取消全部未決 Ask（PermissionCancelled，
// 按 ID 排序以求確定），再發出 kind（TurnCompleted / TurnCancelled / TurnFailed，後者帶 problem）。
func (d *decision) endTurn(sid SessionID, kind EventKind, problem *Problem) {
	d.closeOpen(sid)
	d.cancelAsks(sid)
	x := d.s.Sessions[sid]
	if kind == TurnFailedKind {
		d.emit(Event{Kind: kind, Session: sid, TurnFailed: &TurnFailed{Turn: x.Turn.ID, Problem: *problem}})
	} else {
		d.emit(Event{Kind: kind, Session: sid})
	}
}
