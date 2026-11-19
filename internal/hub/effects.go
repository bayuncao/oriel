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

// dispatch 分派一個 Effect：schedule_timer 在迴圈內處理，
// 連線類先取好連線再進 goroutine（不佔迴圈）。
func (a *actor) dispatch(e task.Effect) {
	sid := e.Session
	switch e.Kind {
	case task.EffScheduleTimer:
		a.armTimer(e)
		return
	case task.EffStartConn:
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
	case task.EffSendPrompt:
		conn := a.conns[sid]
		var turn task.TurnID
		if x := a.state.Sessions[sid]; x.Turn != nil {
			turn = x.Turn.ID
		}
		a.spawn(func() *effectDone {
			if conn == nil {
				return a.failed(e, fail(CodeConnectionLost, nil))
			}
			if err := conn.Prompt(a.h.ctx, *e.SendPrompt); err != nil {
				return a.failed(e, err)
			}
			return &effectDone{key: e.Key, apply: func(a *actor) {
				ci := a.live.Conns[sid]
				ci.Turn = turn
				a.live.Conns[sid] = ci
			}}
		})
	case task.EffCancelTurn:
		conn := a.conns[sid]
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
		a.spawn(func() *effectDone {
			if conn == nil {
				return a.failed(e, fail(CodeConnectionLost, nil))
			}
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
	case task.EffAnswerPermission:
		conn := a.conns[sid]
		p := e.AnswerPermission
		a.spawn(func() *effectDone {
			if conn == nil {
				return a.failed(e, fail(CodeConnectionLost, nil))
			}
			if err := conn.Answer(a.h.ctx, p.Permission, p.Verdict); err != nil {
				return a.failed(e, err)
			}
			return &effectDone{key: e.Key, facts: []task.Command{{
				Kind: task.FactAnswered, Origin: task.OriginRuntime, Session: sid,
				Answered: &task.PermissionRef{Permission: p.Permission}}}}
		})
	case task.EffSetModel:
		conn := a.conns[sid]
		m := *e.SetModel
		a.spawn(func() *effectDone {
			var d *effectDone
			if conn == nil {
				return a.failed(e, fail(CodeConnectionLost, nil))
			}
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
	case task.EffCloseConn:
		// 連線當下就不算數：關的途中它回報的事實（行程被停掉的 exited、晚到的輸出）在 actor 裡丟掉，
		// 歸檔不把 Session 記成退出；還綁定著的記進 Live.Lost，下一輪以 Resume 接回（D117）。
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
	case task.EffCreateWorktree:
		c := *e.CreateWorktree
		a.spawn(func() *effectDone {
			if err := a.h.git.Create(a.h.ctx, c); err != nil {
				return a.failed(e, err)
			}
			return &effectDone{key: e.Key, apply: func(a *actor) {
				a.live.Worktrees[c.Worktree.Path] = true
			}}
		})
	case task.EffMergeWorktree:
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
	case task.EffDiscardWorktree:
		w := *e.DiscardWorktree
		a.spawn(func() *effectDone {
			if err := a.h.git.Discard(a.h.ctx, w.Worktree); err != nil {
				return a.failed(e, err)
			}
			return &effectDone{key: e.Key, facts: []task.Command{{
				Kind: task.FactDiscarded, Origin: task.OriginRuntime, Session: sid,
				Discarded: &task.IntentResult{Intent: w.Intent}}}}
		})
	case task.EffPrepareReview:
		pr := *e.PrepareReview
		a.spawn(func() *effectDone {
			for _, c := range pr.Candidates {
				if _, err := a.h.git.Diff(a.h.ctx, c.Worktree); err != nil {
					return a.failed(e, err)
				}
			}
			return a.failed(e, fail(CodeUnsupported, nil, "op", "blob")) // blob 儲存隨 store
		})
	case task.EffDeleteNative:
		a.spawn(func() *effectDone {
			return a.failed(e, fail(CodeUnsupported, nil, "op", "delete_native"))
		})
	}
}

// armTimer 處理 schedule_timer：記 Live.Timers、清 InFlight、排定事實。
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
