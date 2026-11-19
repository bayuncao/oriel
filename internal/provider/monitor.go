package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Options 是 Monitor 的依賴；都可缺省。
type Options struct {
	// HTTP 發請求；nil → http.DefaultClient。app 傳帶本機代理的（PS28）。
	HTTP *http.Client
	// UserAgent 是請求的 User-Agent（PS28）；"" → "OrielHub"。
	UserAgent string
	// Vendors 是要讀的廠商；nil → Catalog()。
	Vendors []Vendor
	// Now 是時鐘；nil → time.Now。
	Now func() time.Time
	// Zone 是現在的本機時區，每次讀都重新取（同 usage，U09）；nil → time.Local。
	Zone func() *time.Location
	// OnResume 登記電腦醒來時呼叫的函式，回傳取消登記的函式；nil 不理會睡眠。
	OnResume func(fn func()) (off func())
	// Logger 記錄沒讀成的廠商；nil → 記錄時的 slog.Default()。
	Logger *slog.Logger
	// Timeout 是一家的讀取上限（PS23）；0 → 10 秒。
	Timeout time.Duration
}

const (
	defaultTimeout = 10 * time.Second
	maxBody        = 8 << 20
)

// Monitor 讀各家的狀態頁並記住上次的結果。不自己排程：概覽頁開著時由頁面每 60 秒
// 呼叫 Refresh（PS20、PS22）；有頁面在 Watch 時，電腦醒來立刻讀一次（PS26）。
// 方法可從任何 goroutine 呼叫。
type Monitor struct {
	o      Options
	ctx    context.Context // 讀取用；Close 取消
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	board    Board
	last     map[string]ProviderRow // 各家上次讀到的結果
	changed  chan struct{}          // 每次變化關閉並換新
	round    uint64                 // 讀完的次數
	running  bool
	closed   bool
	watchers int
	offWake  func()

	// joined 在 Refresh 登記之後呼叫；僅測試，用來確定合併的時機。
	joined func()
}

// New 回傳 Monitor；第一次 Refresh 才讀。
func New(o Options) *Monitor {
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
	o.UserAgent = cmp.Or(o.UserAgent, "OrielHub")
	if o.Vendors == nil {
		o.Vendors = Catalog()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Zone == nil {
		o.Zone = func() *time.Location { return time.Local }
	}
	o.Timeout = cmp.Or(o.Timeout, defaultTimeout)
	ctx, cancel := context.WithCancel(context.Background())
	m := &Monitor{o: o, ctx: ctx, cancel: cancel, last: map[string]ProviderRow{}, changed: make(chan struct{}),
		board: Board{Rows: []ProviderRow{}, Unread: []UnreadVendor{}}}
	if o.OnResume != nil {
		m.offWake = o.OnResume(m.resumed)
	}
	return m
}

// Refresh 讀一次各家：進行中時等它讀完，不另讀（PS21）。ctx 結束只結束等待，
// 讀取照常進行。
func (m *Monitor) Refresh(ctx context.Context) (Board, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Board{}, errClosed
	}
	target := m.round + 1
	m.startLocked()
	joined := m.joined
	m.mu.Unlock()
	if joined != nil {
		joined()
	}
	for {
		m.mu.Lock()
		b, round, closed, changed := m.board, m.round, m.closed, m.changed
		m.mu.Unlock()
		if round >= target {
			return b, nil
		}
		if closed {
			return Board{}, errClosed
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return Board{}, ctx.Err()
		}
	}
}

// Watch 先送現在的內容（PS19），之後每次變化送一次；ctx 結束（頁面關掉 channel）
// 或 Monitor 關閉時返回。Watch 期間電腦醒來會立刻讀一次（PS26）。
func (m *Monitor) Watch(ctx context.Context, send func(Board) error) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.watchers++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.watchers--
		m.mu.Unlock()
	}()
	first := true
	var rev uint64
	for {
		m.mu.Lock()
		b, changed, closed := m.board, m.changed, m.closed
		m.mu.Unlock()
		if first || b.Rev != rev {
			if err := send(b); err != nil {
				return err
			}
			first, rev = false, b.Rev
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

// Close 取消進行中的讀取並等它結束（PS27）；之後 Refresh 回 provider.closed，
// Watch 回 nil。可以重複呼叫。
func (m *Monitor) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	off := m.offWake
	m.offWake = nil
	m.notifyLocked()
	m.mu.Unlock()
	if off != nil {
		off()
	}
	m.cancel()
	m.wg.Wait()
}

// resumed 是電腦醒來：有頁面在看時立刻讀一次。
func (m *Monitor) resumed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watchers > 0 && !m.closed {
		m.startLocked()
	}
}

// startLocked 在沒有讀取進行時開始一次。
func (m *Monitor) startLocked() {
	if m.running || m.closed {
		return
	}
	m.running = true
	b := m.board
	b.Reading = true
	m.setLocked(b)
	m.wg.Add(1)
	go m.read()
}

// result 是一家讀一次的結果。
type result struct {
	row ProviderRow
	err *Error
}

func (m *Monitor) read() {
	defer m.wg.Done()
	now := m.o.Now()
	zone := m.o.Zone()
	results := make([]result, len(m.o.Vendors))
	var wg sync.WaitGroup
	for i, v := range m.o.Vendors {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(m.ctx, m.o.Timeout)
			defer cancel()
			r, err := m.readVendor(ctx, v, now)
			if err != nil {
				results[i].err = fetchFailed(v.Name, err)
				return
			}
			results[i].row = ProviderRow{ID: v.ID, Name: v.Name, Group: v.Group, Page: v.Page,
				Components: nonNil(r.components), WholePage: r.wholePage, Health: r.health,
				Days: days(r.history, now, zone), Incident: r.incident, ReadAt: now}
		})
	}
	wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = false
	if m.ctx.Err() != nil {
		return
	}
	b := Board{Rows: []ProviderRow{}, Unread: []UnreadVendor{}, ReadAt: m.o.Now()}
	for i, v := range m.o.Vendors {
		r := results[i]
		if r.err == nil {
			m.last[v.ID] = r.row
			b.Rows = append(b.Rows, r.row)
			continue
		}
		m.logger().Warn("provider status not read", "provider", v.ID, "err", r.err)
		p := r.err.problem()
		if prev, ok := m.last[v.ID]; ok {
			prev.Problem = &p
			b.Rows = append(b.Rows, prev)
			continue
		}
		b.Unread = append(b.Unread, UnreadVendor{ID: v.ID, Name: v.Name, Page: v.Page, Problem: p})
	}
	m.round++
	m.setLocked(b)
}

func (m *Monitor) readVendor(ctx context.Context, v Vendor, now time.Time) (reading, error) {
	switch v.Feed {
	case FeedStatuspage:
		root := strings.TrimRight(v.Source, "/")
		var sum, inc []byte
		var sumErr, incErr error
		var wg sync.WaitGroup
		wg.Go(func() { sum, sumErr = m.get(ctx, root+"/api/v2/summary.json") })
		wg.Go(func() { inc, incErr = m.get(ctx, root+"/api/v2/incidents.json") })
		wg.Wait()
		if err := errors.Join(sumErr, incErr); err != nil {
			return reading{}, err
		}
		return parseStatuspage(v, sum, inc, now)
	case FeedFlashduty:
		b, err := m.get(ctx, v.Source)
		if err != nil {
			return reading{}, err
		}
		return parseFlashduty(v, b, now)
	case FeedGoogleCloud:
		b, err := m.get(ctx, v.Source)
		if err != nil {
			return reading{}, err
		}
		return parseGoogleCloud(v, b, now)
	case FeedAWSHealth:
		b, err := m.get(ctx, v.Source)
		if err != nil {
			return reading{}, err
		}
		return parseAWSHealth(v, b)
	}
	return reading{}, fmt.Errorf("unknown feed %q", v.Feed)
}

// get 發一個不帶 cookie 與使用者資訊的 GET（PS28）。
func (m *Monitor) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", m.o.UserAgent)
	req.Header.Set("Accept", "application/json, application/rss+xml, */*")
	resp, err := m.o.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("GET %s: timed out after %s", url, m.o.Timeout)
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, maxBody)
	}
	return b, nil
}

func (m *Monitor) logger() *slog.Logger {
	if m.o.Logger != nil {
		return m.o.Logger
	}
	return slog.Default()
}

// setLocked 換上新內容（Rev 加一）並叫醒等待者。
func (m *Monitor) setLocked(b Board) {
	b.Rev = m.board.Rev + 1
	m.board = b
	m.notifyLocked()
}

func (m *Monitor) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
