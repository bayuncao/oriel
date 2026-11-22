package usage

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// Store 存取讀到的用量；由 store 實作（表結構見 docs/architecture/store.md），
// 測試用 MemStore。
type Store interface {
	// UsageFiles 回傳存下的全部讀取位置。
	UsageFiles(ctx context.Context) ([]FileState, error)
	// ApplyUsage 在一個事務裡寫入一段讀取結果：用量按 (Agent, Key) 合併
	// （Record.Merge），額度每個桶只留 At 最新的一份，File 非 nil 時一併寫入
	// 這個檔案的讀取位置。
	ApplyUsage(ctx context.Context, b Batch) error
	// DeleteUsageFiles 刪掉這些檔案的讀取位置；已讀到的用量保留（D86）。
	DeleteUsageFiles(ctx context.Context, paths []string) error
	// UsageTotals 回傳 bounds[i] <= At < bounds[i+1] 的用量按（i、agent、模型、
	// 目錄、Task）分組的和；bounds 至少兩個、依時間排序，範圍外的不計入。本機記錄的會話或
	// 父會話是 OrielHub 跑的（hub 寫下的會話歸屬）就歸到那個 Task；hub 寫下的上報只在那個
	// 原生會話沒有任何本機記錄時算（m2.md 第 33 節，m1.md 計數規則 2、3）。
	UsageTotals(ctx context.Context, bounds []time.Time) ([]Total, error)
	// UsageQuotas 回傳各額度桶最新的快照，依 agent、桶排序。
	UsageQuotas(ctx context.Context) ([]Quota, error)
}

// Batch 是一段讀取結果，一個事務寫入。
type Batch struct {
	Records []Record
	Quotas  []Quota
	File    *FileState
}

// FileState 是一個記錄檔讀到哪裡。
type FileState struct {
	Path  string
	Agent task.AgentID
	// Size 與 ModTime 是上次寫入讀取位置時檔案（連同 File.Watch）的大小之和與
	// 最晚的修改時間；Done 而且兩者都沒變的檔案不再讀。
	Size    int64
	ModTime time.Time
	// Head 是開頭一段的指紋（Lines 格式）：變了表示換成了別的檔案，從頭重讀。
	Head string
	// ReadTo 是讀到的位置：Lines 是最後一個完整行之後的位元組位置，
	// Compressed 是解壓後的位置。
	ReadTo int64
	// State 是 Source 的解析狀態（Parser.State 或 DatabaseSource 的水位）。
	State []byte
	// Bad 是這份內容裡沒讀懂的行數。
	Bad int
	// Done 表示上次讀到了檔尾；還沒讀到時下一輪接著讀。
	Done bool
}

// Total 是 Store.UsageTotals 的一組。
type Total struct {
	Day   int
	Agent task.AgentID
	Model string
	Dir   string
	// Task 是經 OrielHub 運行時的 Task；直接使用為空。Task 被刪除了也照舊（DL12）。
	Task   task.TaskID
	Tokens Tokens
}

// SessionLink 是 OrielHub 跑的一個原生會話屬於哪個 Task（m2.md 第 33 節）。正式的由 hub 隨事件寫進
// store（hub.SessionLink），usage 只在 UsageTotals 裡讀；MemStore 由 LinkSession 放入。
type SessionLink struct {
	Agent  task.AgentID
	Native string
	Task   task.TaskID
}

// Reported 是 agent 經 OrielHub 上報的一輪用量（hub.ReportedUsage）；鍵是 (Task, Seq)。
type Reported struct {
	Task   task.TaskID
	Seq    int64
	Agent  task.AgentID
	Native string
	At     time.Time
	Model  string
	Dir    string
	Tokens Tokens
}

// MemStore 是 Store 的記憶體真實現，語意與 store.Store 相同；store 的一致性
// 測試同時跑兩者。
type MemStore struct {
	mu       sync.Mutex
	records  map[recordKey]Record
	quotas   map[quotaKey]Quota
	files    map[string]FileState
	links    map[recordKey]task.TaskID // 鍵的 key 是原生會話 ID
	reported map[reportKey]Reported
	applyErr error // FailWrites 設的錯誤
}

type reportKey struct {
	task task.TaskID
	seq  int64
}

type recordKey struct {
	agent task.AgentID
	key   string
}

type quotaKey struct {
	agent  task.AgentID
	bucket string
}

var _ Store = (*MemStore)(nil)

// NewMemStore 回傳空的 MemStore。
func NewMemStore() *MemStore {
	return &MemStore{records: map[recordKey]Record{}, quotas: map[quotaKey]Quota{}, files: map[string]FileState{},
		links: map[recordKey]task.TaskID{}, reported: map[reportKey]Reported{}}
}

// LinkSession 記下會話歸屬；同一 agent 與原生會話已有的照舊（同 store 的 INSERT OR IGNORE）。
func (m *MemStore) LinkSession(l SessionLink) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := recordKey{l.Agent, l.Native}
	if _, ok := m.links[k]; !ok {
		m.links[k] = l.Task
	}
}

// AddReported 記下一輪上報的用量；同一 (Task, Seq) 已有的照舊。
func (m *MemStore) AddReported(r Reported) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := reportKey{r.Task, r.Seq}
	if _, ok := m.reported[k]; !ok {
		m.reported[k] = r
	}
}

func (m *MemStore) UsageFiles(ctx context.Context) ([]FileState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]FileState, 0, len(m.files))
	for _, f := range m.files {
		f.State = slices.Clone(f.State)
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b FileState) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// FailWrites 讓之後的 ApplyUsage 都回 err（nil 恢復正常），像磁碟寫不進去時的 store.Store。
func (m *MemStore) FailWrites(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyErr = err
}

func (m *MemStore) ApplyUsage(ctx context.Context, b Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.applyErr != nil {
		return m.applyErr
	}
	for _, r := range b.Records {
		k := recordKey{r.Agent, r.Key}
		if old, ok := m.records[k]; ok {
			r = old.Merge(r)
		}
		m.records[k] = r
	}
	for _, q := range b.Quotas {
		k := quotaKey{q.Agent, q.Bucket}
		if old, ok := m.quotas[k]; ok && old.At.After(q.At) {
			continue
		}
		q.Windows = slices.Clone(q.Windows)
		m.quotas[k] = q
	}
	if f := b.File; f != nil {
		c := *f
		c.State = slices.Clone(f.State)
		m.files[f.Path] = c
	}
	return nil
}

func (m *MemStore) DeleteUsageFiles(ctx context.Context, paths []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range paths {
		delete(m.files, p)
	}
	return nil
}

func (m *MemStore) UsageTotals(ctx context.Context, bounds []time.Time) ([]Total, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	type group struct {
		day        int
		agent      task.AgentID
		model, dir string
		task       task.TaskID
	}
	sums := map[group]Tokens{}
	add := func(at time.Time, agent task.AgentID, model, dir string, id task.TaskID, t Tokens) {
		day := dayOf(bounds, at)
		if day < 0 {
			return
		}
		g := group{day, agent, model, dir, id}
		sums[g] = sums[g].Add(t)
	}
	// 有本機記錄的原生會話：它的上報不算（計數規則 2）。
	recorded := map[recordKey]bool{}
	for _, r := range m.records {
		recorded[recordKey{r.Agent, r.Session}] = true
		id := task.TaskID("")
		if r.Session != "" {
			id = m.links[recordKey{r.Agent, r.Session}]
		}
		if id == "" && r.Parent != "" {
			id = m.links[recordKey{r.Agent, r.Parent}]
		}
		add(r.At, r.Agent, r.Model, r.Dir, id, r.Tokens)
	}
	for _, r := range m.reported {
		if !recorded[recordKey{r.Agent, r.Native}] {
			add(r.At, r.Agent, r.Model, r.Dir, r.Task, r.Tokens)
		}
	}
	out := make([]Total, 0, len(sums))
	for g, t := range sums {
		out = append(out, Total{Day: g.day, Agent: g.agent, Model: g.model, Dir: g.dir, Task: g.task, Tokens: t})
	}
	slices.SortFunc(out, compareTotals)
	return out, nil
}

func (m *MemStore) UsageQuotas(ctx context.Context) ([]Quota, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Quota, 0, len(m.quotas))
	for _, q := range m.quotas {
		q.Windows = slices.Clone(q.Windows)
		out = append(out, q)
	}
	slices.SortFunc(out, func(a, b Quota) int {
		return strings.Compare(string(a.Agent)+"\x00"+a.Bucket, string(b.Agent)+"\x00"+b.Bucket)
	})
	return out, nil
}

// dayOf 回傳 bounds[i] <= at < bounds[i+1] 的 i，範圍外回 -1。
func dayOf(bounds []time.Time, at time.Time) int {
	for i := 0; i+1 < len(bounds); i++ {
		if !at.Before(bounds[i]) && at.Before(bounds[i+1]) {
			return i
		}
	}
	return -1
}

// compareTotals 是 UsageTotals 的順序：日、agent、模型、目錄。
func compareTotals(a, b Total) int {
	if a.Day != b.Day {
		return a.Day - b.Day
	}
	return strings.Compare(string(a.Agent)+"\x00"+a.Model+"\x00"+a.Dir+"\x00"+string(a.Task),
		string(b.Agent)+"\x00"+b.Model+"\x00"+b.Dir+"\x00"+string(b.Task))
}
