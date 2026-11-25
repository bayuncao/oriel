package task

import (
	"maps"
	"slices"
	"time"
)

// Evolve 把事件 e 摺疊進 s，回傳新的 State。它不做校驗（事件已由 Decide
// 決定或由 DecodeEvent 校驗過），也不改動傳入的 s：被修改的 map 與 slice
// 先複製，指針欄位整個替換（docs/architecture/domain.md「建模約定」）。
// 事件引用不存在的 Session 時不改變它；未知的 Kind 是程式錯誤，會 panic。
func Evolve(s State, e Event) State {
	fold, ok := folds[e.Kind]
	if !ok {
		panic("task: Evolve: unknown event kind " + string(e.Kind))
	}
	s = fold(s, e)
	s.Version = e.Seq
	if touches(e.Kind) {
		s.UpdatedAt = e.At
	}
	return s
}

// touches 回報事件是否算 Task 的更新：置頂與取消置頂只改側欄（D109），優先級與標籤只是整理（D135），模型只改
// 輸入框（D118，接回時的報告不該讓舊 Task 跑到前面），都不改更新時間，所以看板裡的排序與“幾分鐘前”不因它們變
// （m2.md G02、G12、B05）。
func touches(k EventKind) bool {
	switch k {
	case TaskPinnedKind, TaskUnpinnedKind, TaskPrioritySetKind, TaskLabelsSetKind, ModelChosenKind, ModelReportedKind, ModelSwitchFailedKind:
		return false
	}
	return true
}

// folds 是每個 Kind 的摺疊；TestFoldsCoverKinds 確認它與 payloads 的 Kind 相同。
var folds = map[EventKind]func(State, Event) State{
	TaskCreatedKind: func(s State, e Event) State {
		c := e.TaskCreated
		s.Title, s.Brief, s.Workspace, s.Isolation, s.Status, s.Priority = c.Title, c.Brief, c.Workspace, c.Isolation, TaskTodo, PriorityNone
		return s
	},
	TaskEditedKind: func(s State, e Event) State {
		s.Title, s.Brief = e.TaskEdited.Title, e.TaskEdited.Brief
		return s
	},
	TaskStatusChangedKind: func(s State, e Event) State {
		s.Status = e.TaskStatusChanged.To
		return s
	},
	TaskArchivedKind:   func(s State, _ Event) State { s.Archived = true; return s },
	TaskUnarchivedKind: func(s State, _ Event) State { s.Archived = false; return s },
	TaskPinnedKind:     func(s State, e Event) State { s.PinnedAt = e.At; return s },
	TaskUnpinnedKind:   func(s State, _ Event) State { s.PinnedAt = time.Time{}; return s },
	TaskPrioritySetKind: func(s State, e Event) State {
		s.Priority = e.TaskPrioritySet.Priority
		return s
	},
	TaskLabelsSetKind: func(s State, e Event) State {
		s.Labels = slices.Clone(e.TaskLabelsSet.Labels)
		return s
	},

	MessageRedactedKind: func(s State, e Event) State {
		id := e.MessageRedacted.Message
		for _, sid := range s.Order {
			x := s.Sessions[sid]
			if x.Turn != nil && x.Turn.Reply != nil && x.Turn.Reply.Message == id {
				s = withSession(s, sid, func(x *Session) {
					t := *x.Turn
					t.Reply = &ReplyDraft{Message: id, Redacted: true}
					x.Turn = &t
				})
			}
			if x.LastTurn != nil && x.LastTurn.Reply != nil && x.LastTurn.Reply.Message == id {
				s = withSession(s, sid, func(x *Session) {
					r := *x.LastTurn
					r.Reply = &Reply{Message: id, Redacted: true}
					x.LastTurn = &r
				})
			}
		}
		return s
	},
	NativeSessionDeletedKind: func(s State, e Event) State {
		in := *e.NativeSessionDeleted
		s = withSession(s, in.Target.Session, func(x *Session) { x.NativeGone = true })
		s.Intents = putIntent(s.Intents, in)
		return s
	},
	NativeFilesRemovedKind: func(s State, e Event) State {
		s.Intents = del(s.Intents, e.NativeFilesRemoved.Intent)
		return s
	},

	ConfirmationRequestedKind: func(s State, e Event) State {
		c := *e.ConfirmationRequested
		s.Confirmations = put(s.Confirmations, c.ID, c)
		return s
	},
	ConfirmationGrantedKind: func(s State, e Event) State {
		s.Confirmations = del(s.Confirmations, e.ConfirmationGranted.Confirmation)
		return s
	},
	ConfirmationCancelledKind: func(s State, e Event) State {
		s.Confirmations = del(s.Confirmations, e.ConfirmationCancelled.Confirmation)
		return s
	},

	ReviewRequestedKind: func(s State, e Event) State {
		r := e.ReviewRequested
		s.Review = &Review{Launch: r.Launch, Candidates: slices.Clone(r.Candidates)}
		return s
	},
	ForkRecommendedKind: func(s State, e Event) State {
		return withReview(s, func(r *Review) {
			r.Recommended, r.Rationale = e.ForkRecommended.Winner, e.ForkRecommended.Rationale
		})
	},
	ReviewSkippedKind: func(s State, e Event) State {
		s = withReview(s, func(r *Review) { r.Launch = nil })
		s.Failed = del(s.Failed, newEffectKey(EffPrepareReview, ""))
		return s
	},
	ReviewEndedKind: func(s State, e Event) State {
		s.Review = nil
		return s
	},

	SessionCreatedKind: func(s State, e Event) State {
		c := e.SessionCreated
		s.Sessions = put(s.Sessions, e.Session, Session{ID: e.Session, Launch: c.Launch, Role: c.Role, Status: SessionIdle})
		s.Order = append(slices.Clip(s.Order), e.Session)
		if c.Role == RoleReviewer {
			s = withReview(s, func(r *Review) { r.Reviewer = e.Session })
		}
		return s
	},
	NativeSessionBoundKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			x.NativeID, x.Bound = e.NativeSessionBound.NativeID, true
		})
	},
	SessionResumedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			x.Status, x.Exit, x.Retry = SessionIdle, nil, nil
			if p := e.SessionResumed.Requeued; p != nil {
				x.Queue = append([]Prompt{*p}, x.Queue...)
			}
		})
	},
	SessionClosedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) { x.Status = SessionClosed })
	},
	ProcessExitedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			x.Status, x.Open, x.OpenChannel, x.Bound = SessionExited, "", "", false
			x.Exit = new(*e.ProcessExited)
		})
	},

	ModelChosenKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			x.ModelWanted, x.ModelProblem = e.ModelChosen.Value, nil
		})
	},
	ModelReportedKind: func(s State, e Event) State {
		r := e.ModelReported
		return withSession(s, e.Session, func(x *Session) {
			// agent 自己換了（不是接回時的報告）：跟著它，不與它來回搶（V84）。
			if x.follows(*r) {
				x.ModelWanted = ""
			}
			x.Model = r.Setting
		})
	},
	ModelSwitchFailedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			x.ModelWanted, x.ModelProblem = "", new(*e.ModelSwitchFailed)
		})
	},

	PromptQueuedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) { x.Queue = append(slices.Clip(x.Queue), *e.PromptQueued) })
	},
	TurnStartedKind: func(s State, e Event) State {
		t := e.TurnStarted
		return withSession(s, e.Session, func(x *Session) {
			p := Prompt{ID: t.Prompt}
			if i := slices.IndexFunc(x.Queue, func(q Prompt) bool { return q.ID == t.Prompt }); i >= 0 {
				p = x.Queue[i]
			}
			x.Queue = slices.DeleteFunc(slices.Clone(x.Queue), func(q Prompt) bool { return q.ID == t.Prompt })
			x.Turn = &Turn{ID: t.Turn, Prompt: p}
			x.Retry = nil
			x.Status = SessionRunning
		})
	},
	TurnCancelRequestedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			if x.Turn != nil {
				t := *x.Turn
				t.Cancelling = true
				x.Turn = &t
			}
		})
	},
	TurnCompletedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			recordTurn(x, TurnOK, nil, e.At)
			endTurn(x)
			x.CompletedTurns++
		})
	},
	TurnCancelledKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			recordTurn(x, TurnCancelled, nil, e.At)
			endTurn(x)
		})
	},
	TurnFailedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			if !x.Bound && x.Turn != nil {
				// 這個行程沒綁定原生會話就失敗了（agent 沒起來，或接回時起不來）：這條 Prompt 從沒送到，Resume 時重新排上（L56）。
				x.Retry = new(x.Turn.Prompt)
			}
			recordTurn(x, TurnError, new(e.TurnFailed.Problem), e.At)
			endTurn(x)
		})
	},

	MessageAppendedKind: func(s State, e Event) State {
		m := e.MessageAppended
		return withSession(s, e.Session, func(x *Session) {
			x.Open, x.OpenChannel = "", ""
			if m.Partial {
				x.Open, x.OpenChannel = m.Message, m.Channel
			}
			if m.Channel == ChannelText && x.Turn != nil && x.Turn.ID == m.Turn {
				t := *x.Turn
				t.Reply = appendReply(t.Reply, m)
				x.Turn = &t
			}
		})
	},
	ToolCallStartedKind: func(s State, e Event) State {
		c := e.ToolCallStarted
		return withTurn(s, e.Session, func(t *Turn) {
			t.Tools = put(t.Tools, c.ID, ToolRef{Kind: c.Kind, Paths: c.Paths})
		})
	},
	ToolCallUpdatedKind: func(s State, e Event) State {
		u := e.ToolCallUpdated
		x, ok := s.Sessions[e.Session]
		if !ok || x.Turn == nil {
			return s
		}
		r := x.Turn.Tools[u.ID]
		next := r
		if u.Kind != "" {
			next.Kind = u.Kind
		}
		if u.Paths != nil {
			next.Paths = u.Paths
		}
		if next.Kind == r.Kind && slices.Equal(next.Paths, r.Paths) {
			return s // 大多數更新只帶狀態與輸出：不複製
		}
		return withTurn(s, e.Session, func(t *Turn) { t.Tools = put(t.Tools, u.ID, next) })
	},
	PlanUpdatedKind:   keep,
	UsageReportedKind: keep,

	PermissionAskedKind: func(s State, e Event) State {
		a := *e.PermissionAsked
		return withSession(s, e.Session, func(x *Session) { x.Pending = put(x.Pending, a.ID, a) })
	},
	PermissionDecidedKind: func(s State, e Event) State {
		d := e.PermissionDecided
		return withSession(s, e.Session, func(x *Session) {
			if a, ok := x.Pending[d.Permission]; ok {
				decision := d.Decision
				a.Decision = &decision
				x.Pending = put(x.Pending, a.ID, a)
			}
		})
	},
	PermissionAnsweredKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) { x.Pending = del(x.Pending, e.PermissionAnswered.Permission) })
	},
	PermissionCancelledKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) { x.Pending = del(x.Pending, e.PermissionCancelled.Permission) })
	},
	RuleAddedKind: func(s State, e Event) State {
		// Workspace 與全域規則存在 hub 的設定表，不屬於 State。
		if r := *e.RuleAdded; r.Scope == ScopeSession || r.Scope == ScopeTask {
			s.Rules = put(s.Rules, r.ID, r)
		}
		return s
	},

	WorktreeAttachedKind: func(s State, e Event) State {
		s.Worktree = &WorktreeState{Worktree: *e.WorktreeAttached, Outcome: OutcomeOpen}
		return s
	},
	ForkCreatedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) {
			x.Fork = &WorktreeState{Worktree: *e.ForkCreated, Outcome: OutcomeOpen}
		})
	},
	MergeRequestedKind:   addIntent(func(e Event) Intent { return *e.MergeRequested }),
	DiscardRequestedKind: addIntent(func(e Event) Intent { return *e.DiscardRequested }),
	IntentCancelledKind: func(s State, e Event) State {
		in, ok := s.Intents[e.IntentCancelled.Intent]
		if !ok {
			return s
		}
		s.Intents = del(s.Intents, in.ID)
		s.Failed = del(s.Failed, in.effectKey())
		return s
	},
	ForkMergedKind:        resolve(OutcomeMerged, mergedRef),
	ForkDiscardedKind:     resolve(OutcomeDiscarded, discardedRef),
	WorktreeMergedKind:    resolve(OutcomeMerged, mergedRef),
	WorktreeDiscardedKind: resolve(OutcomeDiscarded, discardedRef),
	MergeConflictedKind: func(s State, e Event) State {
		c := e.MergeConflicted
		if in, ok := s.Intents[c.Intent]; ok {
			in.Conflicts = slices.Clone(c.Files)
			s.Intents = putIntent(s.Intents, in)
		}
		return s
	},
	WorktreeKeptKind: keep,

	SessionConflictedKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) { x.Conflicted = true })
	},
	SessionTakenOverKind: func(s State, e Event) State {
		return withSession(s, e.Session, func(x *Session) { x.Conflicted = false })
	},

	EffectFailedKind: func(s State, e Event) State {
		s.Failed = put(s.Failed, e.EffectFailed.Effect, Suspension{Problem: e.EffectFailed.Problem, At: e.At})
		return s
	},
	EffectRetriedKind: func(s State, e Event) State {
		s.Failed = del(s.Failed, e.EffectRetried.Effect)
		return s
	},
}

// keep 是只記錄、不改變 State 的事件（內容由投影保存）。
func keep(s State, _ Event) State { return s }

// recordTurn 把結束的這一輪記進 LastTurn（在 endTurn 清掉 Turn 之前）。
func recordTurn(x *Session, result TurnResult, problem *Problem, at time.Time) {
	if x.Turn == nil {
		return
	}
	x.LastTurn = &TurnRecord{Turn: x.Turn.ID, Result: result, Problem: problem, EndedAt: at, Reply: x.Turn.Reply.Reply()}
}

// appendReply 是收到一段正文後這一輪的最後一條正文：同一條訊息接在後面，另一條訊息取代它。
// 遮蔽過的段（重放時）只留標記。不改 d：接上的一段指向 d 的鏈，舊的 State 不受影響。
func appendReply(d *ReplyDraft, m *MessageAppended) *ReplyDraft {
	if d == nil || d.Message != m.Message {
		d = &ReplyDraft{Message: m.Message}
	}
	return &ReplyDraft{Message: m.Message, Redacted: d.Redacted || m.Redacted,
		last: &replyPart{text: m.Text, prev: d.last}}
}

func endTurn(x *Session) {
	x.Turn, x.Open, x.OpenChannel, x.Status = nil, "", "", SessionIdle
}

// putIntent 是意圖進 State 的唯一路徑：填上它的 Effect 的鍵（Intent.Effect）。
func putIntent(m map[IntentID]Intent, in Intent) map[IntentID]Intent {
	in.Effect = in.effectKey()
	return put(m, in.ID, in)
}

func addIntent(of func(Event) Intent) func(State, Event) State {
	return func(s State, e Event) State {
		s.Intents = putIntent(s.Intents, of(e))
		return s
	}
}

// resolve 把意圖的目標（Fork 或 Task Worktree）標為 outcome 並移除意圖。
// mergedRef 與 discardedRef 取出結局事件所指的意圖與 pull request 的地址（只有以 PR 合併時有）。
func mergedRef(e Event) (IntentID, string)    { return e.Merged.Intent, e.Merged.URL }
func discardedRef(e Event) (IntentID, string) { return e.Discarded.Intent, "" }

func resolve(outcome Outcome, ref func(Event) (IntentID, string)) func(State, Event) State {
	return func(s State, e Event) State {
		id, url := ref(e)
		in, ok := s.Intents[id]
		if !ok {
			return s
		}
		s.Intents = del(s.Intents, in.ID)
		if in.Target.Level == TargetFork {
			return withSession(s, in.Target.Session, func(x *Session) { x.Fork = withOutcome(x.Fork, outcome, url) })
		}
		s.Worktree = withOutcome(s.Worktree, outcome, url)
		return s
	}
}

func withOutcome(w *WorktreeState, o Outcome, pr string) *WorktreeState {
	if w == nil {
		return nil
	}
	c := *w
	c.Outcome = o
	c.PullRequest = pr
	return &c
}

// withSession 以 f 修改 Session id 的副本並放回（複製 Sessions）；
// f 修改 Session 內的 map 與 slice 時同樣要先複製。
func withSession(s State, id SessionID, f func(*Session)) State {
	x, ok := s.Sessions[id]
	if !ok {
		return s
	}
	f(&x)
	s.Sessions = put(s.Sessions, id, x)
	return s
}

func withReview(s State, f func(*Review)) State {
	if s.Review != nil {
		r := *s.Review
		f(&r)
		s.Review = &r
	}
	return s
}

// put 回傳設定了 m[k] = v 的副本；m 不變。
func put[K comparable, V any](m map[K]V, k K, v V) map[K]V {
	c := make(map[K]V, len(m)+1)
	maps.Copy(c, m)
	c[k] = v
	return c
}

// withTurn 以 f 改 Session id 進行中這一輪的副本；沒有這個 Session 或它不在一輪裡時不變。
func withTurn(s State, id SessionID, f func(*Turn)) State {
	if x, ok := s.Sessions[id]; !ok || x.Turn == nil {
		return s
	}
	return withSession(s, id, func(x *Session) {
		t := *x.Turn
		f(&t)
		x.Turn = &t
	})
}

// del 回傳刪除了 k 的副本；k 不在 m 中時回傳 m 本身。
func del[K comparable, V any](m map[K]V, k K) map[K]V {
	if _, ok := m[k]; !ok {
		return m
	}
	c := maps.Clone(m)
	delete(c, k)
	return c
}
