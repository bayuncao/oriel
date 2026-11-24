package hub

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// MemStore 是記憶體內的 Log + Config 真實現：Append 是一個
// 交易，記錄、遮蔽、投影、新 Workspace、新 Rule 一起寫。
type MemStore struct {
	mu         sync.Mutex
	streams    map[task.TaskID][]Record
	views      map[task.TaskID]Views
	search     map[task.TaskID]map[task.Seq]SearchEntry // 累積的全文檢索列
	files      map[task.TaskID]fileSet                  // 累積的用過的檔案
	turns      map[task.TaskID][]TurnEnd                // 累積的結束的回合
	workspaces map[task.WorkspaceID]Workspace
	rules      []task.Rule
	settings   Settings
	labels     map[task.LabelID]Label
	devices    map[task.DeviceID]Device
	models     []AgentModels
	// viewsVersion 是已存投影的版本（Log.ViewsVersion）。
	viewsVersion int
}

func NewMemStore() *MemStore {
	return &MemStore{
		streams:    map[task.TaskID][]Record{},
		views:      map[task.TaskID]Views{},
		search:     map[task.TaskID]map[task.Seq]SearchEntry{},
		files:      map[task.TaskID]fileSet{},
		turns:      map[task.TaskID][]TurnEnd{},
		workspaces: map[task.WorkspaceID]Workspace{},
		labels:     map[task.LabelID]Label{},
		devices:    map[task.DeviceID]Device{},
		settings:   DefaultSettings(),
	}
}

func (s *MemStore) Append(ctx context.Context, id task.TaskID, expected task.Seq, b Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stream := s.streams[id]
	if expected != task.Seq(len(stream)) {
		return errors.New("concurrent write")
	}
	for _, r := range b.Records {
		stream = append(stream, Record{Seq: r.Seq, Kind: r.Kind, At: r.At,
			Payload: slices.Clone(r.Payload)})
	}
	for _, redact := range b.Redact {
		for i := range stream {
			if stream[i].Seq == redact.Seq {
				stream[i].Payload = slices.Clone(redact.Payload)
			}
		}
	}
	if b.NewWorkspace != nil {
		s.workspaces[b.NewWorkspace.ID] = *b.NewWorkspace
	}
	for _, r := range b.NewRules {
		if i := slices.IndexFunc(s.rules, func(x task.Rule) bool { return x.ID == r.ID }); i >= 0 {
			s.rules[i] = r
		} else {
			s.rules = append(s.rules, r)
		}
	}
	s.streams[id] = stream
	// 全文檢索列按 Seq 累積；遮蔽的 Seq 移除。
	acc := s.search[id]
	if acc == nil {
		acc = map[task.Seq]SearchEntry{}
		s.search[id] = acc
	}
	for _, e := range b.Views.Search {
		acc[e.Seq] = e
	}
	for _, seq := range b.Views.Unsearch {
		delete(acc, seq)
	}
	if len(b.Views.Files) > 0 {
		if s.files[id] == nil {
			s.files[id] = fileSet{}
		}
		s.files[id].add(b.Views.Files)
	}
	s.turns[id] = append(s.turns[id], b.Views.Turns...)
	s.views[id] = storedViews(b.Views)
	return nil
}

// storedViews 是存下的全集替換投影：Search/Unsearch/Files/Turns 不存（ViewsOf
// 由累積列組裝），Links/Reported 也不存（只有用量頁讀，經 usage 的 Store，不是 Task 的投影；
// hub 經任何端口都讀不到它們），空的列表是非 nil，待確認數由確認列推出（與 store
// 一致），不改呼叫端的列。
func storedViews(v Views) Views {
	v.Search, v.Unsearch, v.Files, v.Turns, v.Links, v.Reported = nil, nil, nil, nil, nil, nil
	if v.Confirmations == nil {
		v.Confirmations = []ConfirmationView{}
	}
	if v.Task != nil {
		t := *v.Task
		t.Confirmations = len(v.Confirmations)
		v.Task = &t
	}
	return v
}

func (s *MemStore) ViewsVersion(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewsVersion, nil
}

func (s *MemStore) SetViewsVersion(ctx context.Context, v int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.viewsVersion = v
	return nil
}

// Reproject 整體替換一條 Stream 的投影，含檢索列。
func (s *MemStore) Reproject(ctx context.Context, id task.TaskID, v Views) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := map[task.Seq]SearchEntry{}
	for _, e := range v.Search {
		acc[e.Seq] = e
	}
	s.search[id] = acc
	files := fileSet{}
	files.add(v.Files)
	s.files[id] = files
	s.turns[id] = slices.Clone(v.Turns)
	s.views[id] = storedViews(v)
	return nil
}

func (s *MemStore) Read(_ context.Context, id task.TaskID, from task.Seq, limit int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.streams[id] {
		if r.Seq <= from {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, Record{Seq: r.Seq, Kind: r.Kind, At: r.At,
			Payload: slices.Clone(r.Payload)})
	}
	return out, nil
}

func (s *MemStore) Delete(_ context.Context, id task.TaskID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, id)
	delete(s.views, id)
	delete(s.search, id)
	delete(s.files, id)
	delete(s.turns, id)
	return nil
}

func (s *MemStore) Streams(_ context.Context) ([]task.TaskID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Keys(s.streams)), nil
}

// ViewsOf 回傳某個 stream 最新的投影；Search 是累積的
// 全文檢索列（依 Seq 排序），Files 是累積的用過的檔案（依 Path 排序），
// Turns 是累積的結束的回合（依結束的順序），Unsearch 永遠為 nil。
func (s *MemStore) ViewsOf(_ context.Context, id task.TaskID) (Views, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.views[id]
	acc := s.search[id]
	seqs := slices.Sorted(maps.Keys(acc))
	for _, seq := range seqs {
		v.Search = append(v.Search, acc[seq])
	}
	v.Files = s.files[id].list()
	v.Turns = slices.Clone(s.turns[id])
	return v, nil
}

// Recap 是 Index.Recap：未歸檔的 Task 在 since 之後（含）結束的回合。
func (s *MemStore) Recap(ctx context.Context, since time.Time) ([]RecapTurn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RecapTurn
	for id, turns := range s.turns {
		t := s.views[id].Task
		if t == nil || t.Archived {
			continue
		}
		for _, x := range turns {
			if !x.EndedAt.Before(since) {
				out = append(out, RecapTurn{Task: id, TaskTitle: t.Title, Workspace: t.Workspace, TurnEnd: x})
			}
		}
	}
	return out, nil
}

// Files 是 Index.Files：未歸檔的 Task 累積的用過的檔案。
func (s *MemStore) Files(ctx context.Context) ([]TaskFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TaskFile
	for id, f := range s.files {
		t := s.views[id].Task
		if t == nil || t.Archived {
			continue
		}
		for _, u := range f.list() {
			out = append(out, TaskFile{Task: id, Workspace: t.Workspace, Use: u})
		}
	}
	return out, nil
}

// TaskViews 回傳所有 stream 最新的 Task 列，新的在前、
// 同刻按 ID 升冪（與 store 的 ORDER BY 一致）。
func (s *MemStore) TaskViews(_ context.Context) ([]TaskView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskView, 0, len(s.views))
	for _, v := range s.views {
		if v.Task != nil {
			out = append(out, *v.Task)
		}
	}
	slices.SortFunc(out, func(a, b TaskView) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return out, nil
}

var _ Index = (*MemStore)(nil)

// Confirmations 回傳全部 Task 的待確認，按請求時間、Task、ID 升冪。
func (s *MemStore) Confirmations(_ context.Context) ([]ConfirmationView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ConfirmationView{}
	for _, v := range s.views {
		out = append(out, v.Confirmations...)
	}
	slices.SortFunc(out, func(a, b ConfirmationView) int {
		if c := a.RequestedAt.Compare(b.RequestedAt); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.Task), string(b.Task)); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return out, nil
}

// Search 按「每個詞都是子字串」（僅 ASCII 大小寫不敏感）在累積的
// 檢索列中找；不含字母數字的詞丟棄，沒有剩詞就沒有結果。順序與上限同 SQLite 的
// Store.Search：先是標籤命中，再是訊息命中，每個 Task 最多 SearchMessagesPerTask 條、取最新的（D70）。
func (s *MemStore) Search(_ context.Context, q string, limit int) ([]SearchHit, error) {
	terms := SearchTerms(q)
	if len(terms) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SearchHit
	for id, acc := range s.search {
		if s.views[id].Task == nil {
			continue // 與 SQLite 的 JOIN tasks_view 對齊：投影列不存在的命中不算
		}
		messages := 0
		for _, seq := range slices.Backward(slices.Sorted(maps.Keys(acc))) {
			e := acc[seq]
			if e.Kind != SearchLabel && messages == SearchMessagesPerTask {
				continue
			}
			hit := true
			for _, tm := range terms {
				if !asciiFoldContains(e.Text, tm) {
					hit = false
					break
				}
			}
			if !hit {
				continue
			}
			if e.Kind != SearchLabel {
				messages++
			}
			out = append(out, SearchHit{Task: id, Seq: seq, Kind: e.Kind, Ref: e.Ref, Agent: e.Agent, Text: e.Text})
		}
	}
	slices.SortFunc(out, func(a, b SearchHit) int {
		if la, lb := a.Kind == SearchLabel, b.Kind == SearchLabel; la != lb {
			if la {
				return -1
			}
			return 1
		}
		ua, ub := s.views[a.Task].Task.UpdatedAt, s.views[b.Task].Task.UpdatedAt
		if c := ub.Compare(ua); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.Task), string(b.Task)); c != 0 {
			return c
		}
		return cmp.Compare(b.Seq, a.Seq)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// asciiFoldContains 只折 A–Z 的子字串比對（與 SQLite LIKE 一致；
// strings.ToLower 會折 Unicode，不能用）。
func asciiFoldContains(text, term string) bool {
	return strings.Contains(asciiLower(text), asciiLower(term))
}

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, s)
}

func (s *MemStore) Workspaces(_ context.Context) ([]Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Workspace, 0, len(s.workspaces))
	for _, w := range s.workspaces {
		out = append(out, w)
	}
	return out, nil
}

func (s *MemStore) PutWorkspace(_ context.Context, w Workspace) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaces[w.ID] = w
	return nil
}

func (s *MemStore) Rules(_ context.Context) ([]task.Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.rules), nil
}

func (s *MemStore) DeleteRule(_ context.Context, id task.RuleID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = slices.DeleteFunc(s.rules, func(r task.Rule) bool { return r.ID == id })
	return nil
}

func (s *MemStore) Settings(_ context.Context) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings, nil
}

func (s *MemStore) PutSettings(_ context.Context, x Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = x
	return nil
}

func (s *MemStore) Models(_ context.Context) ([]AgentModels, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.models), nil
}

func (s *MemStore) PutModels(_ context.Context, all []AgentModels) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = slices.Clone(all)
	return nil
}

func (s *MemStore) Labels(_ context.Context) ([]Label, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedLabels(s.labels), nil
}

func (s *MemStore) PutLabel(_ context.Context, l Label) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labels[l.ID] = l
	return nil
}

func (s *MemStore) DeleteLabel(_ context.Context, id task.LabelID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.labels, id)
	return nil
}

func (s *MemStore) Devices(_ context.Context) ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedDevices(s.devices), nil
}

func (s *MemStore) PutDevice(_ context.Context, d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[d.ID] = d
	return nil
}

func (s *MemStore) DeleteDevice(_ context.Context, id task.DeviceID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, id)
	return nil
}

func (s *MemStore) ClearDevices(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.devices)
	return nil
}
