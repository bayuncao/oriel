package task

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
