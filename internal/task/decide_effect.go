package task

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
