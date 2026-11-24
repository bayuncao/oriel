package integration

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// IntegrationID 是集成的穩定 IntegrationID。
type IntegrationID string

// GitHub 是第一個集成：經本機的 gh（D108）。
const GitHub IntegrationID = "github"

// IntegrationGroup 是集成在頁面上的分組。
type IntegrationGroup string

// GroupCode 是程式碼託管與協作。
const GroupCode IntegrationGroup = "code"

// IntegrationStatus 是集成現在的狀態。
type IntegrationStatus string

const (
	// StatusUnknown：還沒檢測過（啟動後第一次檢測完成前）。
	StatusUnknown IntegrationStatus = "unknown"
	// StatusUnavailable：接不上；Problem 寫原因。
	StatusUnavailable IntegrationStatus = "unavailable"
	// StatusAvailable：接得上，還沒連接。
	StatusAvailable IntegrationStatus = "available"
	// StatusConnected：已連接，而且最近一次檢測接得上。
	StatusConnected IntegrationStatus = "connected"
)

// Integration 是一個集成在頁面上的樣子。名稱是產品名，不翻譯；說明由前端按 ID 翻譯。
type Integration struct {
	ID    IntegrationID    `json:"id"`
	Name  string           `json:"name"`
	Group IntegrationGroup `json:"group"`
	// Status 由最近一次檢測與 Enabled 決定。
	Status IntegrationStatus `json:"status"`
	// Enabled 是使用者連接過、還沒斷開；檢測不通過時保留（IG07）。
	Enabled bool `json:"enabled"`
	// Account 是 gh 登入的帳號名稱；檢測通過時才有。
	Account string `json:"account,omitzero"`
	// Problem 是 StatusUnavailable 的原因。
	Problem *task.Problem `json:"problem,omitzero"`
}

// IntegrationCatalog 是全部集成，依頁面上的順序。
type IntegrationCatalog struct {
	Items []Integration `json:"items"`
	// Rev 每次變化加一：同一份內容經推送與呼叫結果兩條路到頁面時，取 Rev 大的。
	Rev uint64 `json:"rev"`
}

// Store 存哪些集成已連接（D108：存 store 的設定表）；store 實作它。
type Store interface {
	EnabledIntegrations(ctx context.Context) ([]IntegrationID, error)
	PutEnabledIntegrations(ctx context.Context, ids []IntegrationID) error
}

// Options 是 Service 的依賴。
type Options struct {
	Runner *proc.Runner // 必填
	// Env 取環境快照（同 agent，IG13）；必填。
	Env   func(context.Context) (proc.Env, error)
	Store Store // 必填
	// Root 前綴到系統目錄上；"" 表示 /（測試用，與 proc 一致）。
	Root string
	// Timeout 是一次 gh 的上限（IG10）；0 → 10 秒。
	Timeout time.Duration
}

const defaultTimeout = 10 * time.Second

// detection 是一個集成最近一次檢測的結果。
type detection struct {
	account string
	problem *Error // nil 是接得上
}

// def 是一個集成的定義與它的檢測。
type def struct {
	id    IntegrationID
	name  string
	group IntegrationGroup
	check func(*Service, context.Context) (detection, error)
}

// defs 是 OrielHub 真的接得上的集成（前端不自帶服務列表）。
var defs = []def{
	{id: GitHub, name: "GitHub", group: GroupCode, check: (*Service).checkGitHub},
}

// Service 記住各集成的檢測結果與是否已連接。不自己排程：啟動時檢測一次，之後在
// 頁面要求時（Refresh）與連接時檢測（IG15）。方法可從任何 goroutine 呼叫。
type Service struct {
	o      Options
	ctx    context.Context // 檢測用；Close 取消
	cancel context.CancelFunc
	wg     sync.WaitGroup
	write  sync.Mutex // 一次只寫一筆設定，讀寫之間不夾別的寫

	mu      sync.Mutex
	enabled map[IntegrationID]bool
	det     map[IntegrationID]detection
	cat     IntegrationCatalog
	changed chan struct{} // 每次變化或檢測結束時關閉並換新
	round   uint64        // 檢測完成的次數
	running bool
	closed  bool
}

// New 讀出已連接的集成並回傳 Service；Start 之前不檢測。
func New(ctx context.Context, o Options) (*Service, error) {
	ids, err := o.Store.EnabledIntegrations(ctx)
	if err != nil {
		return nil, err
	}
	o.Timeout = cmp.Or(o.Timeout, defaultTimeout)
	bg, cancel := context.WithCancel(context.Background())
	s := &Service{o: o, ctx: bg, cancel: cancel, enabled: map[IntegrationID]bool{}, det: map[IntegrationID]detection{},
		changed: make(chan struct{})}
	for _, id := range ids {
		if known(id) {
			s.enabled[id] = true
		}
	}
	s.cat = IntegrationCatalog{Items: s.itemsLocked(), Rev: 1}
	return s, nil
}

// Start 開始第一次檢測（環境快照到了才跑，IG15）。
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
}

// List 回傳現在的內容，不檢測。
func (s *Service) List() IntegrationCatalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cat
}

// Refresh 檢測一次：進行中時等它結束，不另起一次。ctx 結束只結束等待，檢測照常進行。
func (s *Service) Refresh(ctx context.Context) (IntegrationCatalog, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return IntegrationCatalog{}, errClosed
	}
	// 進行中的那次可能在呼叫之前就讀了舊的狀態，所以等它之後的下一次。
	target := s.round + 1
	if s.running {
		target++
	}
	s.startLocked()
	s.mu.Unlock()
	for {
		s.mu.Lock()
		cat, round, closed, changed, running := s.cat, s.round, s.closed, s.changed, s.running
		if round < target && !running && !closed {
			s.startLocked() // 進行中的那次結束了，輪到要等的這一次
			changed = s.changed
		}
		s.mu.Unlock()
		if closed {
			return IntegrationCatalog{}, errClosed
		}
		if round >= target {
			return cat, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return IntegrationCatalog{}, ctx.Err()
		}
	}
}

// Connect 先檢測，接得上就記下已連接（IG05）；接不上回報原因，不記（IG06）。
func (s *Service) Connect(ctx context.Context, id IntegrationID) (IntegrationCatalog, error) {
	if !known(id) {
		return IntegrationCatalog{}, fail(CodeUnknown, nil, "id", string(id))
	}
	if _, err := s.Refresh(ctx); err != nil {
		return IntegrationCatalog{}, err
	}
	s.mu.Lock()
	d, checked := s.det[id]
	s.mu.Unlock()
	if !checked {
		// 檢測被中途結束（Runner 關了）、沒有結果：不能當作通過。只在關閉時發生。
		return IntegrationCatalog{}, errClosed
	}
	if d.problem != nil {
		return IntegrationCatalog{}, d.problem
	}
	return s.setEnabled(ctx, id, true)
}

// Disconnect 記下不再使用；沒連接時無操作（IG08）。gh 的登入不變。
func (s *Service) Disconnect(ctx context.Context, id IntegrationID) (IntegrationCatalog, error) {
	if !known(id) {
		return IntegrationCatalog{}, fail(CodeUnknown, nil, "id", string(id))
	}
	return s.setEnabled(ctx, id, false)
}

// setEnabled 把一個集成的連接標記寫進 store，寫成了才改內容。
func (s *Service) setEnabled(ctx context.Context, id IntegrationID, on bool) (IntegrationCatalog, error) {
	s.write.Lock()
	defer s.write.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return IntegrationCatalog{}, errClosed
	}
	if s.enabled[id] == on {
		cat := s.cat
		s.mu.Unlock()
		return cat, nil
	}
	var ids []IntegrationID
	for _, d := range defs {
		if d.id == id && on || d.id != id && s.enabled[d.id] {
			ids = append(ids, d.id)
		}
	}
	s.mu.Unlock()
	if err := s.o.Store.PutEnabledIntegrations(ctx, ids); err != nil {
		return IntegrationCatalog{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled[id] = on
	s.updateLocked()
	return s.cat, nil
}

// Watch 先送現在的內容，之後每次變化送一次（IG12）；ctx 結束或 Service 關閉時返回。
func (s *Service) Watch(ctx context.Context, send func(IntegrationCatalog) error) error {
	first := true
	var rev uint64
	for {
		s.mu.Lock()
		cat, changed, closed := s.cat, s.changed, s.closed
		s.mu.Unlock()
		if first || cat.Rev != rev {
			if err := send(cat); err != nil {
				return err
			}
			first, rev = false, cat.Rev
		}
		if closed {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close 取消進行中的檢測並等它結束，也等寫到一半的連接標記寫完；之後 Refresh、Connect、Disconnect 回
// integration.closed，Watch 回 nil。可以重複呼叫。
func (s *Service) Close() {
	// 先等寫到一半的連接標記寫完，之後 app 才關 store；setEnabled 拿著 write 檢查 closed，
	// 所以關閉之後不會再有寫入。
	s.write.Lock()
	defer s.write.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.notifyLocked()
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

// startLocked 在沒有檢測進行時開始一次。
func (s *Service) startLocked() {
	if s.running || s.closed {
		return
	}
	s.running = true
	s.wg.Add(1)
	go s.check()
}

// check 依序檢測每個集成，最後一起換上結果。被取消（Close）時不換。
func (s *Service) check() {
	defer s.wg.Done()
	got := map[IntegrationID]detection{}
	ok := true
	for _, d := range defs {
		r, err := d.check(s, s.ctx)
		if err != nil {
			ok = false
			break
		}
		got[d.id] = r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.round++
	if ok {
		s.det = got
		s.updateLocked()
	}
	s.notifyLocked() // 叫醒等這一次的 Refresh，內容沒變也一樣
}

// updateLocked 依檢測結果與連接標記重算內容；有變化才加 Rev（IG12）。
func (s *Service) updateLocked() {
	items := s.itemsLocked()
	if reflect.DeepEqual(items, s.cat.Items) {
		return
	}
	s.cat = IntegrationCatalog{Items: items, Rev: s.cat.Rev + 1}
	s.notifyLocked()
}

func (s *Service) itemsLocked() []Integration {
	out := make([]Integration, 0, len(defs))
	for _, d := range defs {
		it := Integration{ID: d.id, Name: d.name, Group: d.group, Enabled: s.enabled[d.id], Status: StatusUnknown}
		if r, ok := s.det[d.id]; ok {
			switch {
			case r.problem != nil:
				it.Status, it.Problem = StatusUnavailable, r.problem.problem()
			case it.Enabled:
				it.Status, it.Account = StatusConnected, r.account
			default:
				it.Status, it.Account = StatusAvailable, r.account
			}
		}
		out = append(out, it)
	}
	return out
}

func (s *Service) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func known(id IntegrationID) bool {
	return slices.ContainsFunc(defs, func(d def) bool { return d.id == id })
}
