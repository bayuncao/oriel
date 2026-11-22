package task

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// envelope 是事件在 store 中的 payload。seq、kind、at 是 store 的欄位，
// 不在信封裡；沒有 payload 的 Kind 沒有 Data。
type envelope struct {
	V       int             `json:"v"`
	Session SessionID       `json:"session,omitzero"`
	Data    json.RawMessage `json:"data,omitzero"`
}

// EncodeEvent 把 e 編成 store 的 payload（seq、kind、at 由 store 另存）。
// e 必須恰好設定與其 Kind 對應的那一個 payload；否則是程式錯誤，回傳錯誤。
func EncodeEvent(e Event) ([]byte, error) {
	s, err := checkPayload(&e)
	if err != nil {
		return nil, fmt.Errorf("task: encode %s: %w", e.Kind, err)
	}
	env := envelope{V: version(e.Kind), Session: e.Session}
	if s != nil {
		v, _ := s.get(&e)
		if env.Data, err = json.Marshal(v); err != nil {
			return nil, fmt.Errorf("task: encode %s: %w", e.Kind, err)
		}
	}
	return json.Marshal(env)
}

// DecodeEvent 從 store 的一列還原事件：舊版本的 payload 先經 upcaster
// 逐級轉成目前版本。資料不符合 Kind 時回傳錯誤（日誌已損壞或來自較新的程式）。
func DecodeEvent(seq Seq, kind EventKind, at time.Time, payload []byte) (Event, error) {
	e, err := decode(seq, kind, at, payload)
	if err != nil {
		return Event{}, fmt.Errorf("task: decode event %d (%s): %w", seq, kind, err)
	}
	return e, nil
}

func decode(seq Seq, kind EventKind, at time.Time, payload []byte) (Event, error) {
	s, known := payloads[kind]
	if !known {
		return Event{}, errors.New("unknown kind")
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Event{}, err
	}
	data, err := upcast(kind, env.V, env.Data)
	if err != nil {
		return Event{}, err
	}
	e := Event{Seq: seq, At: at, Kind: kind, Session: env.Session}
	switch {
	case s == nil && data != nil:
		return Event{}, errors.New("unexpected data")
	case s == nil:
	case data == nil || bytes.Equal(data, []byte("null")):
		return Event{}, errors.New("missing data")
	default:
		if err := s.decode(&e, data); err != nil {
			return Event{}, err
		}
	}
	return e, nil
}

// checkPayload 確認 e 恰好設定了 Kind 對應的 payload（沒有 payload 的 Kind
// 一個也不設），回傳該 payload 的 slot。
func checkPayload(e *Event) (*slot, error) {
	s, known := payloads[e.Kind]
	if !known {
		return nil, errors.New("unknown kind")
	}
	for _, other := range slots {
		if _, set := other.get(e); set && other != s {
			return nil, fmt.Errorf("payload %s set", other.name)
		}
	}
	if s != nil {
		if _, set := s.get(e); !set {
			return nil, fmt.Errorf("payload %s missing", s.name)
		}
	}
	return s, nil
}

// versions 是結構改過的 Kind 的目前版本；不在表中的是 1。改了某個 Kind
// 的結構時遞增它的版本，並在 upcasters 登記從舊版本轉換的函式。
var versions = map[EventKind]int{
	SessionResumedKind: 2, // 加了載荷（Requeued，m2.md L56）
}

func version(k EventKind) int { return cmp.Or(versions[k], 1) }

type upcastKey struct {
	kind EventKind
	from int
}

// upcasters 把 (kind, v) 的 data 轉成 v+1 的 data。登記的轉換同時寫進
// docs/architecture/domain.md 的「已登記的 upcaster」。
var upcasters = map[upcastKey]func(json.RawMessage) (json.RawMessage, error){
	// 版本 1 的 SessionResumed 沒有載荷：補成沒有 Requeued 的空載荷。
	{SessionResumedKind, 1}: func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
}

func upcast(kind EventKind, v int, data json.RawMessage) (json.RawMessage, error) {
	cur := version(kind)
	if v < 1 || v > cur {
		return nil, fmt.Errorf("version %d, this build knows 1..%d", v, cur)
	}
	for ; v < cur; v++ {
		up := upcasters[upcastKey{kind, v}]
		if up == nil {
			return nil, fmt.Errorf("no upcaster from version %d", v)
		}
		var err error
		if data, err = up(data); err != nil {
			return nil, fmt.Errorf("upcast from version %d: %w", v, err)
		}
	}
	return data, nil
}

// slot 是 Event 的一個 payload 欄位：name 是它的 JSON 鍵。
type slot struct {
	name   string
	get    func(*Event) (any, bool)
	decode func(*Event, []byte) error
}

func field[T any](name string, ptr func(*Event) **T) *slot {
	return &slot{
		name: name,
		get: func(e *Event) (any, bool) {
			p := *ptr(e)
			return p, p != nil
		},
		decode: func(e *Event, b []byte) error {
			v := new(T)
			if err := json.Unmarshal(b, v); err != nil {
				return err
			}
			*ptr(e) = v
			return nil
		},
	}
}

var (
	mergedSlot    = field("merged", func(e *Event) **IntentResult { return &e.Merged })
	discardedSlot = field("discarded", func(e *Event) **IntentRef { return &e.Discarded })
)

// payloads 是每個 Kind 的 payload 欄位；nil 表示沒有 payload。
// TestCodecCoversEvent 確認它涵蓋 event.go 的每個 Kind 與每個 payload 欄位。
var payloads = map[EventKind]*slot{
	TaskCreatedKind:       field("taskCreated", func(e *Event) **TaskCreated { return &e.TaskCreated }),
	TaskEditedKind:        field("taskEdited", func(e *Event) **EditTask { return &e.TaskEdited }),
	TaskStatusChangedKind: field("taskStatusChanged", func(e *Event) **TaskStatusChanged { return &e.TaskStatusChanged }),
	TaskArchivedKind:      nil,
	TaskUnarchivedKind:    nil,
	TaskPinnedKind:        nil,
	TaskUnpinnedKind:      nil,
	TaskPrioritySetKind:   field("taskPrioritySet", func(e *Event) **SetPriority { return &e.TaskPrioritySet }),
	TaskLabelsSetKind:     field("taskLabelsSet", func(e *Event) **SetLabels { return &e.TaskLabelsSet }),

	MessageRedactedKind:      field("messageRedacted", func(e *Event) **RedactMessage { return &e.MessageRedacted }),
	NativeSessionDeletedKind: field("nativeSessionDeleted", func(e *Event) **Intent { return &e.NativeSessionDeleted }),
	NativeFilesRemovedKind:   field("nativeFilesRemoved", func(e *Event) **IntentRef { return &e.NativeFilesRemoved }),

	ConfirmationRequestedKind: field("confirmationRequested", func(e *Event) **Confirmation { return &e.ConfirmationRequested }),
	ConfirmationGrantedKind:   field("confirmationGranted", func(e *Event) **ConfirmationRef { return &e.ConfirmationGranted }),
	ConfirmationCancelledKind: field("confirmationCancelled", func(e *Event) **ConfirmationRef { return &e.ConfirmationCancelled }),

	ReviewRequestedKind: field("reviewRequested", func(e *Event) **ReviewRequested { return &e.ReviewRequested }),
	ForkRecommendedKind: field("forkRecommended", func(e *Event) **Recommendation { return &e.ForkRecommended }),
	ReviewSkippedKind:   nil,
	ReviewEndedKind:     nil,

	SessionCreatedKind:     field("sessionCreated", func(e *Event) **SessionCreated { return &e.SessionCreated }),
	NativeSessionBoundKind: field("nativeSessionBound", func(e *Event) **NativeBound { return &e.NativeSessionBound }),
	SessionResumedKind:     field("sessionResumed", func(e *Event) **SessionResumed { return &e.SessionResumed }),
	SessionClosedKind:      nil,
	ProcessExitedKind:      field("processExited", func(e *Event) **Exited { return &e.ProcessExited }),

	ModelChosenKind:       field("modelChosen", func(e *Event) **SetModel { return &e.ModelChosen }),
	ModelReportedKind:     field("modelReported", func(e *Event) **ModelReport { return &e.ModelReported }),
	ModelSwitchFailedKind: field("modelSwitchFailed", func(e *Event) **ModelProblem { return &e.ModelSwitchFailed }),

	PromptQueuedKind:        field("promptQueued", func(e *Event) **Prompt { return &e.PromptQueued }),
	TurnStartedKind:         field("turnStarted", func(e *Event) **TurnStarted { return &e.TurnStarted }),
	TurnCancelRequestedKind: nil,
	TurnCompletedKind:       nil,
	TurnCancelledKind:       nil,
	TurnFailedKind:          field("turnFailed", func(e *Event) **TurnFailed { return &e.TurnFailed }),

	MessageAppendedKind: field("messageAppended", func(e *Event) **MessageAppended { return &e.MessageAppended }),
	ToolCallStartedKind: field("toolCallStarted", func(e *Event) **ToolCall { return &e.ToolCallStarted }),
	ToolCallUpdatedKind: field("toolCallUpdated", func(e *Event) **ToolUpdate { return &e.ToolCallUpdated }),
	PlanUpdatedKind:     field("planUpdated", func(e *Event) **PlanUpdate { return &e.PlanUpdated }),
	UsageReportedKind:   field("usageReported", func(e *Event) **Usage { return &e.UsageReported }),

	PermissionAskedKind:     field("permissionAsked", func(e *Event) **Ask { return &e.PermissionAsked }),
	PermissionDecidedKind:   field("permissionDecided", func(e *Event) **PermissionDecided { return &e.PermissionDecided }),
	PermissionAnsweredKind:  field("permissionAnswered", func(e *Event) **PermissionRef { return &e.PermissionAnswered }),
	PermissionCancelledKind: field("permissionCancelled", func(e *Event) **PermissionRef { return &e.PermissionCancelled }),
	RuleAddedKind:           field("ruleAdded", func(e *Event) **Rule { return &e.RuleAdded }),

	WorktreeAttachedKind:  field("worktreeAttached", func(e *Event) **Worktree { return &e.WorktreeAttached }),
	ForkCreatedKind:       field("forkCreated", func(e *Event) **Worktree { return &e.ForkCreated }),
	MergeRequestedKind:    field("mergeRequested", func(e *Event) **Intent { return &e.MergeRequested }),
	DiscardRequestedKind:  field("discardRequested", func(e *Event) **Intent { return &e.DiscardRequested }),
	IntentCancelledKind:   field("intentCancelled", func(e *Event) **IntentRef { return &e.IntentCancelled }),
	ForkMergedKind:        mergedSlot,
	ForkDiscardedKind:     discardedSlot,
	WorktreeMergedKind:    mergedSlot,
	WorktreeDiscardedKind: discardedSlot,
	MergeConflictedKind:   field("mergeConflicted", func(e *Event) **MergeConflict { return &e.MergeConflicted }),
	WorktreeKeptKind:      field("worktreeKept", func(e *Event) **Target { return &e.WorktreeKept }),

	SessionConflictedKind: nil,
	SessionTakenOverKind:  nil,

	EffectFailedKind:  field("effectFailed", func(e *Event) **EffectFailure { return &e.EffectFailed }),
	EffectRetriedKind: field("effectRetried", func(e *Event) **RetryEffect { return &e.EffectRetried }),
}

// slots 是全部 payload 欄位，各一次（共用欄位的 Kind 只算一次）。
var slots = func() []*slot {
	var out []*slot
	seen := map[*slot]bool{}
	for _, s := range payloads {
		if s != nil && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}()
