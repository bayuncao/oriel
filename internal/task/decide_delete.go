package task

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
