package task

import "fmt"

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
// confirm 不在表裡：它經 run 回頭查表，寫進 handlers 的字面值會形成初始化循環。
func (d *decision) run(c Command) error {
	if c.Kind == CmdConfirm {
		return confirm(d, c)
	}
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

// handlers 把每個 Kind 對應到它的處理函式；CmdConfirm 由 run 直接處理。
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
