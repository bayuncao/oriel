package hub

import (
	"errors"
	"sync"

	"github.com/bayuncao/oriel/internal/task"
)

// coded 是有錯誤代碼的錯誤（proc/workspace/transport 的形狀）；
// hub 不定義也不匯入它們的型別。
type coded interface {
	ErrorCode() string
	ErrorParams() map[string]string
}

// problemOf 把 Effect 的錯誤做成 Problem：有代碼的沿用，否則
// 一律 hub.effect_failed{detail}。
func problemOf(err error) task.Problem {
	var c coded
	if errors.As(err, &c) {
		return task.Problem{Code: c.ErrorCode(), Params: c.ErrorParams()}
	}
	return task.Problem{Code: string(CodeEffectFailed),
		Params: map[string]string{"detail": err.Error()}}
}

// effectorOf 回傳分派該種 Effect 的方法，沒有就回 nil；report_exited 不在其中，由 plan 就地處理（D117）。
// 方法在迴圈內執行：要讀的 actor 狀態（連線、回合）先取好，耗時的工作經 spawn 進 goroutine。
// 用 switch 而不是套件層級的表：方法經 fact → plan 回頭呼叫 dispatch，表會形成初始化循環。
func effectorOf(k task.EffectKind) func(*actor, task.Effect) {
	switch k {
	case task.EffScheduleTimer:
		return (*actor).armTimer
	case task.EffStartConn:
		return (*actor).startConn
	case task.EffSendPrompt:
		return (*actor).sendPrompt
	case task.EffCancelTurn:
		return (*actor).cancelTurn
	case task.EffAnswerPermission:
		return (*actor).answerPermission
	case task.EffSetModel:
		return (*actor).setModel
	case task.EffCloseConn:
		return (*actor).closeConn
	case task.EffCreateWorktree:
		return (*actor).createWorktree
	case task.EffMergeWorktree:
		return (*actor).mergeWorktree
	case task.EffDiscardWorktree:
		return (*actor).discardWorktree
	case task.EffPrepareReview:
		return (*actor).prepareReview
	case task.EffDeleteNative:
		return (*actor).deleteNative
	}
	return nil
}

// dispatch 分派一個 Effect。沒有方法的 Kind 是程式錯誤（TestEffectorsCoverEffects 擋著）：
// 只記錯誤日誌，Effect 留在 InFlight，不會每跑一次 Plan 就再分派、再記一次。
func (a *actor) dispatch(e task.Effect) {
	if f := effectorOf(e.Kind); f != nil {
		f(a, e)
		return
	}
	a.h.logger.Error("hub: no effector", "task", a.id, "kind", e.Kind)
}

// onConn 在迴圈內取好 Session 的連線，在 goroutine 裡用它執行 work；沒有連線時回報 connection_lost。
func (a *actor) onConn(e task.Effect, work func(Conn) *effectDone) {
	conn := a.conns[e.Session]
	a.spawn(func() *effectDone {
		if conn == nil {
			return a.failed(e, fail(CodeConnectionLost, nil))
		}
		return work(conn)
	})
}

// startConn 處理 start_conn：在 goroutine 裡組環境、啟動行程，連線在 apply 時註冊。
func (a *actor) startConn(e task.Effect) {
	sid := e.Session
	st := *e.StartConn
	var mu sync.Mutex
	started := false
	var queued []task.Command
	a.spawn(func() *effectDone {
		rt := a.h.runtimes[st.Launch.Protocol]
		if rt == nil {
			return a.failed(e, fail(CodeNoRuntime, nil, "protocol", string(st.Launch.Protocol)))
		}
		env, err := a.h.env(a.h.ctx)
		if err != nil {
			return a.failed(e, err)
		}
		var kv []string
		for _, v := range st.Launch.Env {
			val := v.Value
			if v.Secret != "" {
				if a.h.secret == nil {
					return a.failed(e, fail(CodeSecretMissing, nil, "name", v.Secret))
				}
				s, err := a.h.secret(v.Secret)
				if err != nil {
					return a.failed(e, fail(CodeSecretMissing, err, "name", v.Secret))
				}
				val = s
			}
			kv = append(kv, v.Name+"="+val)
		}
		env = env.With(kv...)
		// 連線還沒註冊時先暫存事實；apply 後轉直送，
		// 否則 native_bound 會先於連線註冊被處理。
		emit := func(c task.Command) {
			mu.Lock()
			if !started {
				queued = append(queued, c)
				mu.Unlock()
				return
			}
			mu.Unlock()
			a.h.emit(a, sid, c)
		}
		conn, err := rt.Start(a.h.ctx, StartOptions{
			Launch: st.Launch, Workdir: st.Workdir, Resume: st.Resume, Env: env, Emit: emit})
		if xe, ok := errors.AsType[ExitError](err); ok {
			// 行程已結束（D102）：記為 exited，stderr 尾部一起留下。
			return &effectDone{key: e.Key, facts: []task.Command{{
				Kind: task.FactExited, Origin: task.OriginRuntime, Session: sid,
				Exited: &task.Exited{Problem: problemOf(xe), StderrTail: xe.StderrTail()}}}}
		}
		if err != nil {
			return a.failed(e, err)
		}
		return &effectDone{key: e.Key, apply: func(a *actor) {
			a.conns[sid] = conn
			a.live.Conns[sid] = task.ConnInfo{NativeID: conn.NativeID()}
			delete(a.live.Lost, sid)
			mu.Lock()
			started = true
			drained := queued
			queued = nil
			mu.Unlock()
			in := a.h.inputs(a.wsid)
			for _, c := range drained {
				c.Session, c.Origin = sid, task.OriginRuntime
				a.fact(c, in)
			}
		}}
	})
}

// sendPrompt 處理 send_prompt：送出後把回合記進 Live.Conns，取消時據此判斷要不要打擾行程。
func (a *actor) sendPrompt(e task.Effect) {
	sid := e.Session
	var turn task.TurnID
	if x := a.state.Sessions[sid]; x.Turn != nil {
		turn = x.Turn.ID
	}
	a.onConn(e, func(conn Conn) *effectDone {
		if err := conn.Prompt(a.h.ctx, *e.SendPrompt); err != nil {
			return a.failed(e, err)
		}
		return &effectDone{key: e.Key, apply: func(a *actor) {
			ci := a.live.Conns[sid]
			ci.Turn = turn
			a.live.Conns[sid] = ci
		}}
	})
}

// cancelTurn 處理 cancel_turn：Prompt 還沒送出就本地結束回合，否則請行程取消。
func (a *actor) cancelTurn(e task.Effect) {
	sid := e.Session
	var turn task.TurnID
	if x := a.state.Sessions[sid]; x.Turn != nil {
		turn = x.Turn.ID
	}
	if a.live.Conns[sid].Turn != turn {
		// Prompt 還沒送出去：本地結束回合，不打擾行程。
		// 結果經郵箱回來，不在 plan() 迭代中內嵌處理。
		a.spawn(func() *effectDone {
			return &effectDone{key: e.Key, facts: []task.Command{{
				Kind: task.FactTurnEnded, Origin: task.OriginRuntime, Session: sid,
				TurnEnded: &task.TurnEnded{Result: task.TurnCancelled}}}}
		})
		return
	}
	a.onConn(e, func(conn Conn) *effectDone {
		if err := conn.Cancel(a.h.ctx); err != nil {
			return a.failed(e, err)
		}
		// 結束由轉接器回報；這一輪不再送取消。
		return &effectDone{key: e.Key, apply: func(a *actor) {
			if ci, ok := a.live.Conns[sid]; ok {
				ci.Cancelled = turn
				a.live.Conns[sid] = ci
			}
		}}
	})
}

// answerPermission 處理 answer_permission：答覆送到後記下 answered。
func (a *actor) answerPermission(e task.Effect) {
	p := e.AnswerPermission
	a.onConn(e, func(conn Conn) *effectDone {
		if err := conn.Answer(a.h.ctx, p.Permission, p.Verdict); err != nil {
			return a.failed(e, err)
		}
		return &effectDone{key: e.Key, facts: []task.Command{{
			Kind: task.FactAnswered, Origin: task.OriginRuntime, Session: e.Session,
			Answered: &task.PermissionRef{Permission: p.Permission}}}}
	})
}

// setModel 處理 set_model：結果為 model_reported；途中連線換掉時結果不算數。
func (a *actor) setModel(e task.Effect) {
	sid := e.Session
	m := *e.SetModel
	a.onConn(e, func(conn Conn) *effectDone {
		var d *effectDone
		if setting, err := conn.SetModel(a.h.ctx, m); err != nil {
			d = a.failed(e, err)
		} else {
			// 結果與完成一起套用：Plan 再跑時看到的已是換完的模型，不會再送一次（D118）。
			d = &effectDone{key: e.Key, facts: []task.Command{{
				Kind: task.FactModelReported, Origin: task.OriginRuntime, Session: sid,
				ModelReported: &task.ModelReport{Setting: setting}}}}
		}
		// 途中連線被關掉（歸檔，D117）時結果不算數，同它關的途中回報的事實：被停掉的不是沒能換成，
		// 選擇留著，接回時再設（V85）。
		d.apply = func(a *actor) {
			if a.conns[sid] != conn {
				d.facts = nil
			}
		}
		return d
	})
}

// closeConn 處理 close_conn：連線當下就從 actor 移除，關閉在 goroutine 裡進行。
func (a *actor) closeConn(e task.Effect) {
	// 連線當下就不算數：關的途中它回報的事實（行程被停掉的 exited、晚到的輸出）在 actor 裡丟掉，
	// 歸檔不把 Session 記成退出；還綁定著的記進 Live.Lost，下一輪以 Resume 接回（D117）。
	sid := e.Session
	conn := a.conns[sid]
	delete(a.conns, sid)
	delete(a.live.Conns, sid)
	if x, ok := a.state.Sessions[sid]; ok && x.Bound && x.Status != task.SessionClosed && x.Status != task.SessionExited {
		a.live.Lost[sid] = ""
		if x.Turn != nil {
			a.live.Lost[sid] = x.Turn.ID
		}
	}
	a.spawn(func() *effectDone {
		if conn != nil {
			if err := conn.Close(); err != nil {
				a.h.logger.Debug("hub: conn close failed", "task", a.id, "err", err)
			}
		}
		// 行程結束了：再跑一次 Plan，等著它的 start_conn 這時才分派。
		return &effectDone{key: e.Key, apply: func(*actor) {}}
	})
}

// createWorktree 處理 create_worktree：建好後記進 Live.Worktrees。
func (a *actor) createWorktree(e task.Effect) {
	c := *e.CreateWorktree
	a.spawn(func() *effectDone {
		if err := a.h.git.Create(a.h.ctx, c); err != nil {
			return a.failed(e, err)
		}
		return &effectDone{key: e.Key, apply: func(a *actor) {
			a.live.Worktrees[c.Worktree.Path] = true
		}}
	})
}

// mergeWorktree 處理 merge_worktree：結果為 merge_conflict 或 merged。
func (a *actor) mergeWorktree(e task.Effect) {
	sid := e.Session
	m := *e.MergeWorktree
	a.spawn(func() *effectDone {
		conflicts, url, err := a.h.git.Merge(a.h.ctx, m)
		if err != nil {
			return a.failed(e, err)
		}
		if len(conflicts) > 0 {
			return &effectDone{key: e.Key, facts: []task.Command{{
				Kind: task.FactMergeConflict, Origin: task.OriginRuntime, Session: sid,
				MergeConflict: &task.MergeConflict{Intent: m.Intent, Files: conflicts}}}}
		}
		return &effectDone{key: e.Key, facts: []task.Command{{
			Kind: task.FactMerged, Origin: task.OriginRuntime, Session: sid,
			Merged: &task.IntentResult{Intent: m.Intent, URL: url}}}}
	})
}

// discardWorktree 處理 discard_worktree：結果為 discarded。
func (a *actor) discardWorktree(e task.Effect) {
	w := *e.DiscardWorktree
	a.spawn(func() *effectDone {
		if err := a.h.git.Discard(a.h.ctx, w.Worktree); err != nil {
			return a.failed(e, err)
		}
		return &effectDone{key: e.Key, facts: []task.Command{{
			Kind: task.FactDiscarded, Origin: task.OriginRuntime, Session: e.Session,
			Discarded: &task.IntentResult{Intent: w.Intent}}}}
	})
}

// prepareReview 處理 prepare_review：把 diff 存成 blob 要等 store，目前回報 unsupported。
func (a *actor) prepareReview(e task.Effect) {
	a.spawn(func() *effectDone {
		return a.failed(e, fail(CodeUnsupported, nil, "op", "blob"))
	})
}

// deleteNative 處理 delete_native：目前回報 unsupported。
func (a *actor) deleteNative(e task.Effect) {
	a.spawn(func() *effectDone {
		return a.failed(e, fail(CodeUnsupported, nil, "op", "delete_native"))
	})
}

// armTimer 處理 schedule_timer：在迴圈內記 Live.Timers、清 InFlight、排定事實。
func (a *actor) armTimer(e task.Effect) {
	sid, pid := e.Session, e.ScheduleTimer.Permission
	key := task.AskKey{Session: sid, Permission: pid}
	a.live.Timers[key] = true
	delete(a.live.InFlight, e.Key)
	d := max(0, e.ScheduleTimer.At.Sub(a.h.now()))
	a.timers[key] = a.h.timer(d, func() {
		done := &effectDone{
			apply: func(a *actor) {
				delete(a.live.Timers, key)
				delete(a.timers, key)
			},
			facts: []task.Command{{Kind: task.FactAskExpired, Origin: task.OriginRuntime,
				Session: sid, AskExpired: &task.PermissionRef{Permission: pid}}},
		}
		select {
		case a.mail <- msg{done: done}:
		case <-a.stop:
		}
	})
}

// failed 是 Effect 失敗的固定形狀：EffectFailed 事實帶著 Problem。
func (a *actor) failed(e task.Effect, err error) *effectDone {
	return &effectDone{key: e.Key, facts: []task.Command{{
		Kind: task.FactEffectFailed, Origin: task.OriginRuntime, Session: e.Session,
		EffectFailed: &task.EffectFailure{Effect: e.Key, Problem: problemOf(err)}}}}
}

// spawn 在迴圈外執行 Effect；結果送進郵箱，停止時丟棄。
func (a *actor) spawn(work func() *effectDone) {
	a.inflight.Add(1)
	go func() {
		defer a.inflight.Done()
		d := work()
		if d == nil {
			return
		}
		select {
		case a.mail <- msg{done: d}:
		case <-a.stop:
		}
	}()
}
