package task

import (
	"cmp"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Decide 對 Command 套用業務規則，回傳要追加到 Stream 的事件；違反規則時回傳 *Rejection。
// 事件的 Seq 從 s.Version+1 依次編號，At 一律是 in.Now；新實體的 ID 由建立它的事件的 Seq 衍生。
// 不改變狀態的命令（如內容相同的 EditTask）回傳零個事件、不報錯。Decide 不改動傳入的 s、in 與 c；
// 回傳的事件可能與 c 共用切片（Launch 的 Args、Prompt 的 Attachments），hub 在 Decide 之後不得再改動 c。
func Decide(s State, in Inputs, c Command) ([]Event, error) {
	ki, known := kinds[c.Kind]
	if !known {
		return nil, invalid("unknown command kind %q", c.Kind)
	}
	switch c.Origin {
	case OriginDesktop, OriginRemote, OriginRuntime:
	default:
		return nil, invalid("unknown origin %q", c.Origin)
	}
	if (c.Origin == OriginRuntime) != ki.fact {
		return nil, reject(ReasonForgedOrigin)
	}
	if err := ki.checkPayload(&c); err != nil {
		return nil, err
	}
	if c.Kind == CmdCreateTask {
		if s.Version != 0 {
			return nil, invalid("task already exists")
		}
	} else {
		if s.Version == 0 {
			return nil, invalid("task does not exist")
		}
		if s.Archived && !ki.fact && c.Kind != CmdUnarchive && c.Kind != CmdArchive {
			return nil, reject(ReasonArchived)
		}
	}
	d := &decision{s: s, in: in}
	if err := d.run(c); err != nil {
		return nil, err
	}
	d.endReview()
	d.autoStatus()
	d.autoReview()
	return d.out, nil
}

// run 以已知的 Kind 查表並執行命令；Confirmation 核准後用它重放存入的命令。
func (d *decision) run(c Command) error {
	h, ok := handlers[c.Kind]
	if !ok {
		return invalid("unsupported command kind %q", c.Kind)
	}
	return h(d, c)
}

// cmdSlot 是 Command 裡一個載荷欄位。
type cmdSlot struct {
	field string              // Command 的欄位名
	set   func(*Command) bool // 該欄位非 nil
}

// kindInfo 描述一個 CommandKind：是執行期事實還是客戶端命令、用哪個載荷欄位。
type kindInfo struct {
	fact    bool
	payload *cmdSlot // nil = 沒有載荷的 Kind
}

// cmdSlots 依 Command 的欄位順序列出全部載荷；TestKindsCoverCommand 用
// reflect 確認它恰好涵蓋每個指標欄位。
var cmdSlots = []*cmdSlot{
	{"CreateTask", func(c *Command) bool { return c.CreateTask != nil }},
	{"EditTask", func(c *Command) bool { return c.EditTask != nil }},
	{"SetStatus", func(c *Command) bool { return c.SetStatus != nil }},
	{"SetPriority", func(c *Command) bool { return c.SetPriority != nil }},
	{"SetLabels", func(c *Command) bool { return c.SetLabels != nil }},
	{"CreateSession", func(c *Command) bool { return c.CreateSession != nil }},
	{"Submit", func(c *Command) bool { return c.Submit != nil }},
	{"CloseSession", func(c *Command) bool { return c.CloseSession != nil }},
	{"DecidePermission", func(c *Command) bool { return c.DecidePermission != nil }},
	{"FanOut", func(c *Command) bool { return c.FanOut != nil }},
	{"ChooseFork", func(c *Command) bool { return c.ChooseFork != nil }},
	{"Merge", func(c *Command) bool { return c.Merge != nil }},
	{"Discard", func(c *Command) bool { return c.Discard != nil }},
	{"RedactMessage", func(c *Command) bool { return c.RedactMessage != nil }},
	{"Confirm", func(c *Command) bool { return c.Confirm != nil }},
	{"CancelConfirmation", func(c *Command) bool { return c.CancelConfirmation != nil }},
	{"CancelIntent", func(c *Command) bool { return c.CancelIntent != nil }},
	{"RetryEffect", func(c *Command) bool { return c.RetryEffect != nil }},
	{"SetModel", func(c *Command) bool { return c.SetModel != nil }},
	{"Bound", func(c *Command) bool { return c.Bound != nil }},
	{"Output", func(c *Command) bool { return c.Output != nil }},
	{"ToolStarted", func(c *Command) bool { return c.ToolStarted != nil }},
	{"ToolUpdated", func(c *Command) bool { return c.ToolUpdated != nil }},
	{"Plan", func(c *Command) bool { return c.Plan != nil }},
	{"Usage", func(c *Command) bool { return c.Usage != nil }},
	{"Asked", func(c *Command) bool { return c.Asked != nil }},
	{"TurnEnded", func(c *Command) bool { return c.TurnEnded != nil }},
	{"Exited", func(c *Command) bool { return c.Exited != nil }},
	{"AskExpired", func(c *Command) bool { return c.AskExpired != nil }},
	{"Answered", func(c *Command) bool { return c.Answered != nil }},
	{"Merged", func(c *Command) bool { return c.Merged != nil }},
	{"MergeConflict", func(c *Command) bool { return c.MergeConflict != nil }},
	{"Discarded", func(c *Command) bool { return c.Discarded != nil }},
	{"NativeDeleted", func(c *Command) bool { return c.NativeDeleted != nil }},
	{"Recommendation", func(c *Command) bool { return c.Recommendation != nil }},
	{"ReviewPrepared", func(c *Command) bool { return c.ReviewPrepared != nil }},
	{"EffectFailed", func(c *Command) bool { return c.EffectFailed != nil }},
	{"ModelReported", func(c *Command) bool { return c.ModelReported != nil }},
}

// cmdField 依欄位名取 cmdSlot；欄位名來自 kinds，寫錯是程式錯誤。
func cmdField(field string) *cmdSlot {
	for _, s := range cmdSlots {
		if s.field == field {
			return s
		}
	}
	panic("task: no command field " + field)
}

// kinds 把每個 CommandKind 標為執行期事實（fact）或客戶端命令，並對應到它的
// 載荷欄位；nil 欄位表示該 Kind 沒有載荷。
var kinds = map[CommandKind]kindInfo{
	CmdCreateTask:          {false, cmdField("CreateTask")},
	CmdEditTask:            {false, cmdField("EditTask")},
	CmdSetStatus:           {false, cmdField("SetStatus")},
	CmdArchive:             {false, nil},
	CmdUnarchive:           {false, nil},
	CmdPin:                 {false, nil},
	CmdUnpin:               {false, nil},
	CmdSetPriority:         {false, cmdField("SetPriority")},
	CmdSetLabels:           {false, cmdField("SetLabels")},
	CmdCreateSession:       {false, cmdField("CreateSession")},
	CmdSubmit:              {false, cmdField("Submit")},
	CmdCancelTurn:          {false, nil},
	CmdResume:              {false, nil},
	CmdCloseSession:        {false, cmdField("CloseSession")},
	CmdTakeOver:            {false, nil},
	CmdDecidePermission:    {false, cmdField("DecidePermission")},
	CmdFanOut:              {false, cmdField("FanOut")},
	CmdChooseFork:          {false, cmdField("ChooseFork")},
	CmdSkipReview:          {false, nil},
	CmdMerge:               {false, cmdField("Merge")},
	CmdDiscard:             {false, cmdField("Discard")},
	CmdRedactMessage:       {false, cmdField("RedactMessage")},
	CmdDeleteNativeSession: {false, nil},
	CmdConfirm:             {false, cmdField("Confirm")},
	CmdCancelConfirmation:  {false, cmdField("CancelConfirmation")},
	CmdRetryEffect:         {false, cmdField("RetryEffect")},
	CmdCancelIntent:        {false, cmdField("CancelIntent")},
	CmdSetModel:            {false, cmdField("SetModel")},

	FactNativeBound:    {true, cmdField("Bound")},
	FactOutput:         {true, cmdField("Output")},
	FactToolStarted:    {true, cmdField("ToolStarted")},
	FactToolUpdated:    {true, cmdField("ToolUpdated")},
	FactPlan:           {true, cmdField("Plan")},
	FactUsage:          {true, cmdField("Usage")},
	FactAsked:          {true, cmdField("Asked")},
	FactTurnEnded:      {true, cmdField("TurnEnded")},
	FactExited:         {true, cmdField("Exited")},
	FactAskExpired:     {true, cmdField("AskExpired")},
	FactAnswered:       {true, cmdField("Answered")},
	FactConflicted:     {true, nil},
	FactMerged:         {true, cmdField("Merged")},
	FactMergeConflict:  {true, cmdField("MergeConflict")},
	FactDiscarded:      {true, cmdField("Discarded")},
	FactNativeDeleted:  {true, cmdField("NativeDeleted")},
	FactRecommendation: {true, cmdField("Recommendation")},
	FactReviewPrepared: {true, cmdField("ReviewPrepared")},
	FactEffectFailed:   {true, cmdField("EffectFailed")},
	FactModelReported:  {true, cmdField("ModelReported")},
}

// checkPayload 確認命令恰好設定了它的 Kind 對應的載荷欄位。
func (ki kindInfo) checkPayload(c *Command) error {
	for _, s := range cmdSlots {
		set := s.set(c)
		if s == ki.payload {
			if !set {
				return invalid("missing payload")
			}
		} else if set {
			return invalid("payload %s set", s.field)
		}
	}
	return nil
}

// handlers 把每個 Kind 對應到它的處理函式。
var handlers = map[CommandKind]func(*decision, Command) error{
	CmdCreateTask:          createTask,
	CmdEditTask:            editTask,
	CmdSetStatus:           setStatus,
	CmdArchive:             archive,
	CmdUnarchive:           unarchive,
	CmdPin:                 pin,
	CmdUnpin:               unpin,
	CmdSetPriority:         setPriority,
	CmdSetLabels:           setLabels,
	CmdCreateSession:       createSession,
	CmdSubmit:              submitPrompt,
	CmdCancelTurn:          cancelTurn,
	CmdResume:              resume,
	CmdCloseSession:        closeSession,
	CmdTakeOver:            takeOver,
	CmdDecidePermission:    decidePermission,
	CmdCancelConfirmation:  cancelConfirmation,
	CmdRetryEffect:         retryEffect,
	CmdCancelIntent:        cancelIntent,
	CmdMerge:               merge,
	CmdDiscard:             discard,
	CmdFanOut:              fanOut,
	CmdChooseFork:          chooseFork,
	CmdSkipReview:          skipReview,
	CmdRedactMessage:       redactMessage,
	CmdDeleteNativeSession: deleteNativeSession,
	CmdSetModel:            setModel,

	FactNativeBound: nativeBound,
	FactAsked:       permissionAsked,
	FactAskExpired:  askExpired,
	FactAnswered:    answered,
	FactOutput:      output,
	FactToolStarted: toolStarted,
	FactToolUpdated: record(ToolCallUpdatedKind,
		func(c *Command) *ToolUpdate { return c.ToolUpdated },
		func(e *Event, v *ToolUpdate) { e.ToolCallUpdated = v }),
	FactPlan: record(PlanUpdatedKind,
		func(c *Command) *PlanUpdate { return c.Plan },
		func(e *Event, v *PlanUpdate) { e.PlanUpdated = v }),
	FactUsage: record(UsageReportedKind,
		func(c *Command) *Usage { return c.Usage },
		func(e *Event, v *Usage) { e.UsageReported = v }),
	FactTurnEnded:      turnEnded,
	FactExited:         exited,
	FactConflicted:     sessionConflicted,
	FactEffectFailed:   effectFailed,
	FactMerged:         merged,
	FactMergeConflict:  mergeConflict,
	FactDiscarded:      discarded,
	FactReviewPrepared: reviewPrepared,
	FactRecommendation: recommendation,
	FactNativeDeleted:  nativeDeleted,
	FactModelReported:  modelReported,
}

// confirm 經由 run 回頭查表重放存入的命令，寫進字面值會形成初始化循環，所以在 init 登記。
func init() { handlers[CmdConfirm] = confirm }

func reject(r Reason) error { return &Rejection{Reason: r} }

func invalid(format string, args ...any) error {
	return &Rejection{Reason: ReasonInvalid, Detail: fmt.Sprintf(format, args...)}
}

// decision 累積一次 Decide 的事件；每個事件發出後立即摺疊進 s，之後的規則看到的是最新狀態。
type decision struct {
	s   State
	in  Inputs
	out []Event
}

func (d *decision) next() Seq { return d.s.Version + 1 }

func (d *decision) emit(e Event) {
	e.Seq, e.At = d.next(), d.in.Now
	d.s = Evolve(d.s, e)
	d.out = append(d.out, e)
}

// autoStatus 套用 Task 狀態的自動規則（D7）：本次有回合開始 → in_progress；
// 本次有回合結束、所有 Session 都不在運行且至少完成過一輪 → in_review。
func (d *decision) autoStatus() {
	var started, stopped bool
	for _, e := range d.out {
		switch e.Kind {
		case TurnStartedKind:
			started = true
		case TurnCompletedKind, TurnCancelledKind, TurnFailedKind:
			stopped = true
		}
	}
	switch {
	case started:
		d.setStatus(TaskInProgress, CauseAuto)
	case stopped:
		var completed bool
		for _, x := range d.s.Sessions {
			completed = completed || x.CompletedTurns > 0
		}
		if !d.anyRunning() && completed {
			d.setStatus(TaskInReview, CauseAuto)
		}
	}
}

func (d *decision) setStatus(to TaskStatus, cause StatusCause) {
	if d.s.Status == to {
		return
	}
	d.emit(Event{Kind: TaskStatusChangedKind, TaskStatusChanged: &TaskStatusChanged{
		From: d.s.Status, To: to, Cause: cause,
	}})
}

func createTask(d *decision, c Command) error {
	p := c.CreateTask
	title, ok := Label(p.Title)
	brief := p.Brief
	if strings.TrimSpace(brief) == "" {
		brief = ""
	}
	switch {
	case d.s.ID == "":
		return invalid("task id missing")
	case p.Workspace == "":
		return invalid("workspace missing")
	case !ok:
		return invalid("title must be one non-blank line")
	case len(p.Launches) > 0 && brief == "":
		return invalid("brief is the first prompt")
	}
	for _, l := range p.Launches {
		if err := checkLaunch(l); err != nil {
			return err
		}
	}
	fanout := len(p.Launches) > 1
	mode, err := chooseIsolation(d.in, c.Origin == OriginRemote, fanout)
	if err != nil {
		return err
	}
	if fanout && mode == IsolationNone && d.in.Repo.Dirty {
		return reject(ReasonDirtyWorkdir)
	}
	d.emit(Event{Kind: TaskCreatedKind, TaskCreated: &TaskCreated{
		Title: title, Brief: brief, Workspace: p.Workspace, Isolation: mode,
	}})
	if mode == IsolationWorktree {
		w := d.taskWorktree()
		d.emit(Event{Kind: WorktreeAttachedKind, WorktreeAttached: &w})
	}
	for _, l := range p.Launches {
		id := d.newSession(l, RoleWorker, fanout)
		d.submit(id, Prompt{Text: brief})
	}
	return nil
}

// chooseIsolation 依 domain.md「Isolation 與 Fork」的表選擇：非 git 與 detached HEAD 兩列優先於 Policy。
func chooseIsolation(in Inputs, remote, fanout bool) (IsolationMode, error) {
	switch {
	case !in.Repo.IsGit:
		if fanout {
			return "", reject(ReasonNotGit)
		}
		return IsolationNone, nil
	case in.Repo.Branch == "":
		if fanout || remote {
			return "", reject(ReasonDetachedHead)
		}
		return IsolationNone, nil
	case in.Policy == PolicyAlways:
		return IsolationWorktree, nil
	case in.Policy == PolicyNever:
		return IsolationNone, nil
	case fanout || remote || (worktreesForConcurrency && in.OtherTasksRunning):
		return IsolationWorktree, nil
	}
	return IsolationNone, nil
}

// worktreesForConcurrency 是 auto 策略是否因同倉庫已有 Task 在跑而用 worktree。
// M4 做出 worktree 之前為假：否則新 Task 會一直等 create_worktree，Session
// 起不來；兩個 Task 先共用主目錄，由介面提示（D97）。
const worktreesForConcurrency = false

func (d *decision) taskWorktree() Worktree {
	id := string(d.s.ID)
	return Worktree{
		Path:       filepath.Join(d.in.WorktreeRoot, id),
		Branch:     "orielhub/" + id,
		BaseBranch: d.in.Repo.Branch,
		BaseCommit: d.in.Repo.Commit,
	}
}

func (d *decision) forkWorktree(sid SessionID) Worktree {
	name := string(d.s.ID) + "-" + string(sid)
	base := d.in.Repo.Branch
	if d.s.Worktree != nil {
		base = d.s.Worktree.Branch
	}
	return Worktree{
		Path:       filepath.Join(d.in.WorktreeRoot, name),
		Branch:     "orielhub/" + name,
		BaseBranch: base,
		BaseCommit: d.in.Repo.Commit,
	}
}

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

func editTask(d *decision, c Command) error {
	p := c.EditTask
	title, ok := Label(p.Title)
	brief := p.Brief
	if strings.TrimSpace(brief) == "" {
		brief = ""
	}
	if !ok {
		return invalid("title must be one non-blank line")
	}
	if title == d.s.Title && brief == d.s.Brief {
		return nil
	}
	e := *p
	e.Title, e.Brief = title, brief
	d.emit(Event{Kind: TaskEditedKind, TaskEdited: &e})
	return nil
}

func setStatus(d *decision, c Command) error {
	p := c.SetStatus
	switch p.Status {
	case TaskTodo, TaskInProgress, TaskInReview, TaskDone:
	default:
		return invalid("unknown status %q", p.Status)
	}
	if p.Status != TaskDone && len(p.Resolutions) > 0 {
		return invalid("resolutions only apply to done")
	}
	if p.Status == TaskDone {
		if len(p.Resolutions) > 0 && c.Origin != OriginDesktop {
			return reject(ReasonNeedsDesktop)
		}
		for _, r := range p.Resolutions {
			if err := d.requestResolution(r.Target, r.Resolution); err != nil {
				return err
			}
		}
		if d.unresolved() {
			return reject(ReasonUnresolvedChanges)
		}
	}
	d.setStatus(p.Status, CauseUser)
	return nil
}

// unresolved 回報是否仍有未解決的 worktree：Outcome 為 open 且沒有
// 針對它的意圖（等待中的合併或捨棄意圖算已解決）。
func (d *decision) unresolved() bool {
	if d.s.Worktree != nil && d.s.Worktree.Outcome == OutcomeOpen &&
		!d.hasIntent(Target{Level: TargetWorktree}) {
		return true
	}
	return len(d.unresolvedForks()) > 0
}

// forkTarget 是 Session 的 Fork 目標。
func forkTarget(sid SessionID) Target {
	return Target{Level: TargetFork, Session: sid}
}

// unresolvedForks 列出還開著且沒有意圖的 Fork（依 Order）。
func (d *decision) unresolvedForks() []SessionID {
	var out []SessionID
	for _, sid := range d.s.Order {
		if d.s.Sessions[sid].openFork() && !d.hasIntent(forkTarget(sid)) {
			out = append(out, sid)
		}
	}
	return out
}

// anyRunning 回報是否有 Session 在運行。
func (d *decision) anyRunning() bool {
	for _, x := range d.s.Sessions {
		if x.Status == SessionRunning {
			return true
		}
	}
	return false
}

// intentFor 取出針對目標的既有意圖。
func (d *decision) intentFor(t Target) (Intent, bool) {
	for _, in := range d.s.Intents {
		if in.Target == t {
			return in, true
		}
	}
	return Intent{}, false
}

func (d *decision) hasIntent(t Target) bool {
	_, ok := d.intentFor(t)
	return ok
}

// target 找出處置的目標 worktree；找不到是程式或協定錯誤。
func (d *decision) target(t Target) (SessionID, *WorktreeState, error) {
	switch t.Level {
	case TargetFork:
		x, ok := d.s.Sessions[t.Session]
		if !ok || x.Fork == nil {
			return "", nil, invalid("no worktree for target")
		}
		return t.Session, x.Fork, nil
	case TargetWorktree:
		if d.s.Worktree == nil {
			return "", nil, invalid("no worktree for target")
		}
		return "", d.s.Worktree, nil
	}
	return "", nil, invalid("unknown target %q", t.Level)
}

// requestResolution 為目標 worktree 登記合併或捨棄意圖（D9、D16）；已有意圖時不重複。
func (d *decision) requestResolution(t Target, r Resolution) error {
	sid, w, err := d.target(t)
	if err != nil {
		return err
	}
	if w.Outcome != OutcomeOpen {
		return invalid("worktree already resolved")
	}
	if d.hasIntent(t) {
		return nil
	}
	switch r.Action {
	case IntentMerge:
		strategy := cmp.Or(r.Strategy, MergeSquash)
		if strategy != MergeSquash && strategy != MergePullRequest {
			return invalid("unknown merge strategy %q", r.Strategy)
		}
		d.emit(Event{Kind: MergeRequestedKind, Session: sid, MergeRequested: &Intent{
			ID: intentID(d.next()), Kind: IntentMerge, Target: t,
			Strategy: strategy, Message: cmp.Or(r.Message, d.defaultMergeMessage()),
		}})
	case IntentDiscard:
		d.emit(Event{Kind: DiscardRequestedKind, Session: sid, DiscardRequested: &Intent{
			ID: intentID(d.next()), Kind: IntentDiscard, Target: t,
		}})
	default:
		return invalid("unknown resolution action %q", r.Action)
	}
	return nil
}

func (d *decision) defaultMergeMessage() string {
	m := d.s.Title
	if d.s.Brief != "" {
		m += "\n\n" + d.s.Brief
	}
	return m
}

func archive(d *decision, _ Command) error {
	if d.s.Archived {
		return nil
	}
	if d.anyRunning() {
		return reject(ReasonSessionRunning)
	}
	d.emit(Event{Kind: TaskArchivedKind})
	return nil
}

func unarchive(d *decision, _ Command) error {
	if !d.s.Archived {
		return nil
	}
	d.emit(Event{Kind: TaskUnarchivedKind})
	return nil
}

// pin 置頂 Task（D109）；已置頂時不產生事件，置頂的時間不變。
func pin(d *decision, _ Command) error {
	if !d.s.PinnedAt.IsZero() {
		return nil
	}
	d.emit(Event{Kind: TaskPinnedKind})
	return nil
}

// unpin 取消置頂；未置頂時不產生事件。
func unpin(d *decision, _ Command) error {
	if d.s.PinnedAt.IsZero() {
		return nil
	}
	d.emit(Event{Kind: TaskUnpinnedKind})
	return nil
}

// setPriority 設定優先級（D135）；與現值相同時不產生事件。
func setPriority(d *decision, c Command) error {
	p := c.SetPriority.Priority
	switch p {
	case PriorityNone, PriorityLow, PriorityMedium, PriorityHigh, PriorityUrgent:
	default:
		return invalid("unknown priority %q", p)
	}
	if p == d.s.Priority {
		return nil
	}
	d.emit(Event{Kind: TaskPrioritySetKind, TaskPrioritySet: &SetPriority{Priority: p}})
	return nil
}

// setLabels 以完整的集合取代標籤（D135）：排序、去重，每個都要在目錄裡（Inputs.Labels）；與現值相同時不產生事件。
func setLabels(d *decision, c Command) error {
	labels := slices.Clone(c.SetLabels.Labels)
	slices.Sort(labels)
	labels = slices.Compact(labels)
	for _, id := range labels {
		if !slices.Contains(d.in.Labels, id) {
			return &Rejection{Reason: ReasonUnknownLabel, Detail: string(id)}
		}
	}
	if slices.Equal(labels, d.s.Labels) {
		return nil
	}
	if len(labels) == 0 {
		labels = nil
	}
	d.emit(Event{Kind: TaskLabelsSetKind, TaskLabelsSet: &SetLabels{Labels: labels}})
	return nil
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

// Label 正規化單行標籤（Task 標題、Workspace 名稱，D65）：去掉首尾空白；
// 結果為空或含換行（\n、\r）時 ok 為 false。中間的 tab 等照常保留。
func Label(s string) (string, bool) {
	out := strings.TrimSpace(s)
	return out, out != "" && !strings.ContainsAny(out, "\r\n")
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
	d.emit(Event{Kind: SessionResumedKind, Session: c.Session})
	d.startNext(c.Session) // 退出前排隊的 Prompt 在恢復後繼續
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
	return nil
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

// permissionAsked 記錄 agent 的權限請求；命中既有規則時立即自動決定（D19）。
func permissionAsked(d *decision, c Command) error {
	x, err := d.inTurn(c.Session)
	if err != nil {
		return err
	}
	p := c.Asked
	if p.ID == "" {
		return invalid("permission id missing")
	}
	if _, dup := x.Pending[p.ID]; dup {
		return invalid("duplicate permission id")
	}
	d.emit(Event{Kind: PermissionAskedKind, Session: c.Session, PermissionAsked: &Ask{
		ID: p.ID, Turn: x.Turn.ID, Request: p.Request, AskedAt: d.in.Now,
	}})
	if r, ok := d.matchRule(c.Session, p.Request); ok {
		d.emit(Event{Kind: PermissionDecidedKind, Session: c.Session, PermissionDecided: &PermissionDecided{
			Permission: p.ID,
			Decision:   Decision{Verdict: r.Verdict, Source: DecidedByRule, Rule: r.ID},
		}})
	}
	return nil
}

func decidePermission(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	p := c.DecidePermission
	if p.Verdict != Allow && p.Verdict != Deny {
		return invalid("unknown verdict %q", p.Verdict)
	}
	a, pending := x.Pending[p.Permission]
	var ask *Ask
	if pending {
		ask = &a
	}
	var rule Rule
	if p.Always != nil {
		rule, err = d.checkAlways(*p.Always, p.Verdict, ask, x.Launch.Agent, c.Origin)
		if err != nil {
			return err
		}
	}
	if pending && a.Decision == nil {
		d.emit(Event{Kind: PermissionDecidedKind, Session: c.Session, PermissionDecided: &PermissionDecided{
			Permission: p.Permission,
			Decision:   Decision{Verdict: p.Verdict, Source: DecidedByUser, Origin: c.Origin},
		}})
	}
	if p.Always != nil {
		if c.Origin == OriginRemote {
			// D20：規則的一半等 desktop 核准；存入的是校驗並補齊過的規則，批 4 以 desktop 重跑它。
			always := rule
			stored := c
			stored.DecidePermission = &DecidePermission{Permission: p.Permission, Verdict: p.Verdict, Always: &always}
			d.requestConfirmation(stored, nil)
		} else {
			d.addRule(c.Session, rule)
		}
	}
	return nil
}

func askExpired(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	p := c.AskExpired
	a, ok := x.Pending[p.Permission]
	if !ok || a.Decision != nil {
		return nil
	}
	if x.Launch.AskTimeout <= 0 {
		return invalid("ask has no timeout")
	}
	if d.in.Now.Before(a.AskedAt.Add(x.Launch.AskTimeout)) {
		return invalid("timer fired early")
	}
	verdict := Deny // D18 安全預設：沒有明說 allow 就拒絕
	if x.Launch.TimeoutVerdict == Allow {
		verdict = Allow
	}
	d.emit(Event{Kind: PermissionDecidedKind, Session: c.Session, PermissionDecided: &PermissionDecided{
		Permission: p.Permission,
		Decision:   Decision{Verdict: verdict, Source: DecidedByTimeout},
	}})
	return nil
}

func answered(d *decision, c Command) error {
	x, err := d.session(c.Session)
	if err != nil {
		return err
	}
	p := c.Answered
	a, ok := x.Pending[p.Permission]
	if !ok {
		return nil // 期間已被取消
	}
	if a.Decision == nil {
		return invalid("answered before decided")
	}
	d.emit(Event{Kind: PermissionAnsweredKind, Session: c.Session,
		PermissionAnswered: &PermissionRef{Permission: p.Permission}})
	return nil
}

// confirm 核准等待中的 Confirmation（D20）：先記錄核准，再以 desktop
// 重放存入的命令。命令是 Decide 自己存入的，形狀不對是程式錯誤；
// 重放被拒絕時整個決定回傳錯誤，Confirmation 保持待核准。
func confirm(d *decision, c Command) error {
	if c.Origin == OriginRemote {
		return reject(ReasonNeedsDesktop) // remote 連 payload 都不看
	}
	p := c.Confirm
	conf, ok := d.s.Confirmations[p.Confirmation]
	if !ok {
		return nil // 已核准或取消；重複點擊不報錯
	}
	stored := conf.Command
	ki, known := kinds[stored.Kind]
	if !known || ki.fact || stored.Kind == CmdConfirm || stored.Kind == CmdCancelConfirmation {
		return invalid("stored command %q cannot run", stored.Kind)
	}
	d.emit(Event{Kind: ConfirmationGrantedKind, ConfirmationGranted: &ConfirmationRef{Confirmation: p.Confirmation}})
	stored.Origin = OriginDesktop
	stored.Device = c.Device
	return d.run(stored)
}

// cancelConfirmation 撤銷等待中的 Confirmation；任何裝置都可以走安全方向。
func cancelConfirmation(d *decision, c Command) error {
	p := c.CancelConfirmation
	if _, ok := d.s.Confirmations[p.Confirmation]; !ok {
		return nil
	}
	d.emit(Event{Kind: ConfirmationCancelledKind, ConfirmationCancelled: &ConfirmationRef{Confirmation: p.Confirmation}})
	return nil
}

type failureMode int

const (
	suspend failureMode = iota + 1
	asExit
	asModelFailure // 沒能換成模型：ModelSwitchFailed（D118），不掛起、不當作退出
)

// onFailure 是每種 Effect 失敗後的處置（domain.md「Effect 失敗」）；
// 不在表中的 Kind 不會失敗，回報它是程式錯誤。
var onFailure = map[EffectKind]failureMode{
	EffStartConn:        asExit,
	EffSendPrompt:       asExit,
	EffAnswerPermission: asExit,
	EffCancelTurn:       asExit,
	EffCreateWorktree:   suspend,
	EffMergeWorktree:    suspend,
	EffDiscardWorktree:  suspend,
	EffDeleteNative:     suspend,
	EffPrepareReview:    suspend,
	EffSetModel:         asModelFailure,
}

func effectFailed(d *decision, c Command) error {
	p := c.EffectFailed
	switch onFailure[p.Effect.Kind()] {
	case asExit:
		return d.processExited(c.Session, Exited{Problem: p.Problem})
	case suspend:
		if _, ok := d.s.Failed[p.Effect]; ok {
			return nil // 重複回報
		}
		e := *p
		d.emit(Event{Kind: EffectFailedKind, EffectFailed: &e})
		return nil
	case asModelFailure:
		x, err := d.session(c.Session)
		if err != nil {
			return err
		}
		if x.ModelWanted == "" {
			return nil // 已經不換了（例如 agent 自己換了）
		}
		d.emit(Event{Kind: ModelSwitchFailedKind, Session: c.Session,
			ModelSwitchFailed: &ModelProblem{Value: x.ModelWanted, Problem: p.Problem}})
		return nil
	}
	return invalid("effect %q cannot fail", p.Effect)
}

// retryEffect 解除掛起的 Effect；底層的決定早已做出，重試只是讓 Plan 再試一次。
func retryEffect(d *decision, c Command) error {
	p := c.RetryEffect
	if _, ok := d.s.Failed[p.Effect]; !ok {
		return nil
	}
	r := *p
	d.emit(Event{Kind: EffectRetriedKind, EffectRetried: &r})
	return nil
}

// checkResolve 是 Merge / Discard 命令的前提（D35）：目標開著且還沒有意圖；
// Fork 的 Session 不在運行；Task Worktree 要求 Task 內沒有 Session 在運行、沒有未處置的 Fork。
func (d *decision) checkResolve(t Target, kind IntentKind) (noop bool, err error) {
	sid, w, err := d.target(t)
	if err != nil {
		return false, err
	}
	if w.Outcome != OutcomeOpen {
		return false, invalid("worktree already resolved")
	}
	if in, ok := d.intentFor(t); ok {
		if in.Kind == kind {
			return true, nil // 同一意圖已在進行：重複命令不報錯
		}
		return false, invalid("target has a %s intent; cancel it first", in.Kind)
	}
	if sid != "" {
		if d.s.Sessions[sid].Status == SessionRunning {
			return false, reject(ReasonSessionRunning)
		}
	} else {
		if d.anyRunning() {
			return false, reject(ReasonSessionRunning)
		}
		if len(d.unresolvedForks()) > 0 {
			return false, reject(ReasonUnresolvedChanges)
		}
	}
	return false, nil
}

// requestConfirmation 為 remote 命令請求 desktop 核准（D5、D20）；
// dup 非 nil 且已有相符的在等待時當作重複點擊。
func (d *decision) requestConfirmation(c Command, dup func(Command) bool) {
	for _, conf := range d.s.Confirmations {
		if dup != nil && dup(conf.Command) {
			return
		}
	}
	d.emit(Event{Kind: ConfirmationRequestedKind, ConfirmationRequested: &Confirmation{
		ID: confirmationID(d.next()), Command: c, RequestedBy: c.Device, RequestedAt: d.in.Now,
	}})
}

// resolveCmd 是 Merge 與 Discard 共用的流程（D35）：remote 的命令要 desktop
// 核准（D5、D20），desktop 直接登記意圖。
func (d *decision) resolveCmd(c Command, t Target, r Resolution) error {
	noop, err := d.checkResolve(t, r.Action)
	if err != nil || noop {
		return err
	}
	if c.Origin == OriginRemote {
		d.requestConfirmation(c, func(stored Command) bool {
			return stored.Kind == CmdMerge && stored.Merge != nil && stored.Merge.Target == t ||
				stored.Kind == CmdDiscard && stored.Discard != nil && stored.Discard.Target == t
		})
		return nil
	}
	return d.requestResolution(t, r)
}

func merge(d *decision, c Command) error {
	p := c.Merge
	return d.resolveCmd(c, p.Target, Resolution{
		Action: IntentMerge, Strategy: p.Strategy, Message: p.Message,
	})
}

func discard(d *decision, c Command) error {
	p := c.Discard
	return d.resolveCmd(c, p.Target, Resolution{Action: IntentDiscard})
}

func cancelIntent(d *decision, c Command) error {
	p := c.CancelIntent
	in, ok := d.s.Intents[p.Intent]
	if !ok {
		return nil // 意圖已完成或已撤銷
	}
	if _, suspended := d.s.Failed[in.effectKey()]; !suspended {
		return reject(ReasonInFlight)
	}
	d.emit(Event{Kind: IntentCancelledKind, IntentCancelled: &IntentRef{Intent: p.Intent}})
	return nil
}

// intentFact 取出事實所指的意圖；不存在視為已處理過（done），Kind 不符是程式錯誤。
func (d *decision) intentFact(id IntentID, kind IntentKind) (in Intent, done bool, err error) {
	in, ok := d.s.Intents[id]
	if !ok {
		return in, true, nil
	}
	if in.Kind != kind {
		return in, false, invalid("intent %s is a %s", in.ID, in.Kind)
	}
	return in, false, nil
}

// resolvedFact 是 merged / discarded 共用的流程：找意圖、拒絕掛起中與仍在運行的
// （hub 不得在那時執行 Effect）、發出結局事件、收尾（D37）。
func (d *decision) resolvedFact(id IntentID, kind IntentKind, emit func(in Intent)) error {
	in, done, err := d.intentFact(id, kind)
	if err != nil || done {
		return err
	}
	if _, ok := d.s.Failed[in.effectKey()]; ok {
		return invalid("effect is suspended")
	}
	switch in.Target.Level {
	case TargetFork:
		if d.s.Sessions[in.Target.Session].Status == SessionRunning {
			return invalid("session still running")
		}
	case TargetWorktree:
		for _, x := range d.s.Sessions {
			if x.Status == SessionRunning {
				return invalid("session still running")
			}
		}
	default:
		return invalid("unknown target %q", in.Target.Level)
	}
	emit(in)
	d.afterResolved(in)
	return nil
}

func merged(d *decision, c Command) error {
	p := c.Merged
	r := *p
	return d.resolvedFact(p.Intent, IntentMerge, func(in Intent) {
		if in.Target.Level == TargetFork {
			d.emit(Event{Kind: ForkMergedKind, Session: in.Target.Session, Merged: &r})
		} else {
			d.emit(Event{Kind: WorktreeMergedKind, Merged: &r})
		}
	})
}

func discarded(d *decision, c Command) error {
	p := c.Discarded
	ref := &IntentRef{Intent: p.Intent}
	return d.resolvedFact(p.Intent, IntentDiscard, func(in Intent) {
		if in.Target.Level == TargetFork {
			d.emit(Event{Kind: ForkDiscardedKind, Session: in.Target.Session, Discarded: ref})
		} else {
			d.emit(Event{Kind: WorktreeDiscardedKind, Discarded: ref})
		}
	})
}

// mergeConflict 是合併衝突：記錄衝突檔案並把 merge_worktree 掛起，等使用者處理。
func mergeConflict(d *decision, c Command) error {
	p := c.MergeConflict
	in, done, err := d.intentFact(p.Intent, IntentMerge)
	if err != nil || done {
		return err
	}
	if len(p.Files) == 0 {
		return invalid("merge conflict without files")
	}
	if _, ok := d.s.Failed[in.effectKey()]; ok {
		return nil // 已掛起
	}
	d.emit(Event{Kind: MergeConflictedKind, Session: in.Target.Session,
		MergeConflicted: &MergeConflict{Intent: p.Intent, Files: slices.Clone(p.Files)}})
	d.emit(Event{Kind: EffectFailedKind, EffectFailed: &EffectFailure{
		Effect:  in.effectKey(),
		Problem: Problem{Code: CodeMergeConflict, Params: map[string]string{"files": strings.Join(p.Files, "\n")}},
	}})
	return nil
}

// afterResolved 在 worktree 處置完成後收尾（D37）：Fork 關掉它的 Session；
// Task Worktree 關掉所有還開著的 Session 並把 Task 標為 done。
func (d *decision) afterResolved(in Intent) {
	if in.Target.Level == TargetFork {
		d.closeIfOpen(in.Target.Session)
		return
	}
	for _, sid := range d.s.Order {
		d.closeIfOpen(sid)
	}
	d.setStatus(TaskDone, CauseAuto)
}

// autoReview 在 Fork 的 Session 都停下後開啟評審（D17、D38）：本次有 Session 停下
// （回合結束、進程退出、Session 關閉），Task 未歸檔、沒有 Review，開著且無意圖的
// Fork ≥ 2 且其 Session 都不在運行。
func (d *decision) autoReview() {
	var stopped bool
	for _, e := range d.out {
		switch e.Kind {
		case TurnCompletedKind, TurnCancelledKind, TurnFailedKind,
			ProcessExitedKind, SessionClosedKind:
			stopped = true
		}
	}
	if !stopped || d.s.Review != nil || d.s.Archived {
		return // 歸檔的 Task 不開評審：沒人會看到它
	}
	candidates := d.unresolvedForks()
	for _, sid := range candidates {
		if d.s.Sessions[sid].Status == SessionRunning {
			return
		}
	}
	if len(candidates) < 2 {
		return
	}
	var launch *LaunchSnapshot
	if d.in.Reviewer != nil {
		l := *d.in.Reviewer
		launch = &l
	}
	d.emit(Event{Kind: ReviewRequestedKind, ReviewRequested: &ReviewRequested{
		Candidates: candidates, Launch: launch,
	}})
}

func fanOut(d *decision, c Command) error {
	p := c.FanOut
	if len(p.Launches) < 2 {
		return invalid("fan-out needs at least two launches")
	}
	if err := CheckPrompt(p.Prompt); err != nil {
		return err
	}
	for _, l := range p.Launches {
		if err := checkLaunch(l); err != nil {
			return err
		}
	}
	if !d.in.Repo.IsGit {
		return reject(ReasonNotGit)
	}
	if d.in.Repo.Branch == "" {
		return reject(ReasonDetachedHead)
	}
	if d.in.Repo.Dirty {
		return reject(ReasonDirtyWorkdir)
	}
	for _, x := range d.s.Sessions {
		if x.openFork() {
			return reject(ReasonUnresolvedChanges)
		}
	}
	for _, l := range p.Launches {
		id := d.newSession(l, RoleWorker, true)
		d.submit(id, p.Prompt) // Prompt 是值；submit 為每份複本編新 ID
	}
	return nil
}

// reviewPrepared 是 prepare_review 的結果：檢查候選名單一致後建立審查者 Session。
func reviewPrepared(d *decision, c Command) error {
	p := c.ReviewPrepared
	r := d.s.Review
	if r == nil || r.Launch == nil || r.Reviewer != "" {
		return nil // 過期或重複的回報
	}
	if len(p.Diffs) != len(r.Candidates) {
		return invalid("diff count %d, want %d", len(p.Diffs), len(r.Candidates))
	}
	bySession := make(map[SessionID]ForkDiff, len(p.Diffs))
	for _, fd := range p.Diffs {
		if _, dup := bySession[fd.Session]; dup {
			return invalid("duplicate diff for %s", fd.Session)
		}
		bySession[fd.Session] = fd
	}
	ordered := make([]ForkDiff, len(r.Candidates))
	for i, sid := range r.Candidates {
		fd, ok := bySession[sid]
		if !ok {
			return invalid("no diff for candidate %s", sid)
		}
		ordered[i] = fd
	}
	id := d.newSession(*r.Launch, RoleReviewer, false)
	d.submit(id, reviewPrompt(d.s, r.Candidates, ordered))
	return nil
}

func recommendation(d *decision, c Command) error {
	p := c.Recommendation
	r := d.s.Review
	if r == nil {
		return nil
	}
	if r.Reviewer == "" {
		return invalid("no reviewer")
	}
	if r.Recommended != "" {
		return nil
	}
	if !slices.Contains(r.Candidates, p.Winner) {
		return invalid("winner %s is not a candidate", p.Winner)
	}
	rc := *p
	d.emit(Event{Kind: ForkRecommendedKind, ForkRecommended: &rc})
	return nil
}

func chooseFork(d *decision, c Command) error {
	p := c.ChooseFork
	winnerTarget := forkTarget(p.Winner)
	x, ok := d.s.Sessions[p.Winner]
	if !ok || x.Fork == nil {
		return invalid("winner %s has no fork", p.Winner)
	}
	var losers []Target
	for _, sid := range d.unresolvedForks() {
		if sid != p.Winner {
			losers = append(losers, forkTarget(sid))
		}
	}
	noop, err := d.checkResolve(winnerTarget, IntentMerge)
	if err != nil {
		return err
	}
	if noop {
		return invalid("winner already has an intent")
	}
	for _, t := range losers {
		if _, err := d.checkResolve(t, IntentDiscard); err != nil {
			return err
		}
	}
	if c.Origin == OriginRemote {
		d.requestConfirmation(c, func(stored Command) bool {
			return stored.Kind == CmdChooseFork
		})
		return nil
	}
	if err := d.requestResolution(winnerTarget,
		Resolution{Action: IntentMerge, Strategy: p.Strategy, Message: p.Message}); err != nil {
		return err
	}
	for _, t := range losers {
		if err := d.requestResolution(t, Resolution{Action: IntentDiscard}); err != nil {
			return err
		}
	}
	return nil
}

// endReview 在候選剩不到兩個時結束評審（D43）：關掉評審者（若未關）再記
// ReviewEnded；意圖登記即算處置，與 done 的判定一致。
func (d *decision) endReview() {
	r := d.s.Review
	if r == nil {
		return
	}
	left := 0
	for _, sid := range r.Candidates {
		if d.s.Sessions[sid].openFork() && !d.hasIntent(forkTarget(sid)) {
			left++
		}
	}
	if left >= 2 {
		return
	}
	if r.Reviewer != "" {
		d.closeIfOpen(r.Reviewer)
	}
	d.emit(Event{Kind: ReviewEndedKind})
}

// skipReview 改為手動選擇：關掉審查者（若有）、把 Launch 置空。
func skipReview(d *decision, c Command) error {
	r := d.s.Review
	if r == nil || r.Launch == nil {
		return nil
	}
	if r.Reviewer != "" {
		d.closeIfOpen(r.Reviewer)
	}
	d.emit(Event{Kind: ReviewSkippedKind})
	return nil
}

func redactMessage(d *decision, c Command) error {
	if c.Origin == OriginRemote {
		return reject(ReasonNeedsDesktop)
	}
	p := c.RedactMessage
	x, ok := d.s.Sessions[c.Session]
	if !ok {
		return reject(ReasonUnknownSession)
	}
	if x.Open == p.Message {
		return invalid("message still open")
	}
	r := *p
	d.emit(Event{Kind: MessageRedactedKind, Session: c.Session, MessageRedacted: &r})
	return nil
}

func deleteNativeSession(d *decision, c Command) error {
	if c.Origin == OriginRemote {
		return reject(ReasonNeedsDesktop)
	}
	x, ok := d.s.Sessions[c.Session]
	if !ok {
		return reject(ReasonUnknownSession)
	}
	if !x.Launch.Caps.DeleteNative {
		return invalid("agent cannot delete native session")
	}
	if x.Status == SessionIdle || x.Status == SessionRunning {
		return reject(ReasonSessionOpen)
	}
	if x.NativeID == "" || x.NativeGone {
		return nil
	}
	for _, in := range d.s.Intents {
		if in.Kind == IntentDeleteNative && in.Target.Session == c.Session {
			return nil
		}
	}
	d.emit(Event{Kind: NativeSessionDeletedKind, Session: c.Session, NativeSessionDeleted: &Intent{
		ID:     intentID(d.next()),
		Kind:   IntentDeleteNative,
		Target: Target{Session: c.Session},
	}})
	return nil
}

func nativeDeleted(d *decision, c Command) error {
	p := c.NativeDeleted
	in, done, err := d.intentFact(p.Intent, IntentDeleteNative)
	if err != nil || done {
		return err
	}
	if _, ok := d.s.Failed[in.effectKey()]; ok {
		return invalid("effect is suspended")
	}
	d.emit(Event{Kind: NativeFilesRemovedKind, Session: in.Target.Session,
		NativeFilesRemoved: &IntentRef{Intent: p.Intent}})
	return nil
}
