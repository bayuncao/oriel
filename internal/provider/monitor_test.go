package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// statusServer 是一個真的 HTTP 伺服器，按路徑前綴給每家狀態頁的回應；每家可以
// 換成失敗、或擋住直到測試放行。
type statusServer struct {
	*httptest.Server
	mu       sync.Mutex
	fail     map[string]bool          // 前綴 → 回 500
	hold     map[string]chan struct{} // 前綴 → 關閉前擋住請求
	arrived  chan string              // 每個請求的路徑
	requests []*http.Request
}

func newStatusServer(t *testing.T) *statusServer {
	s := &statusServer{fail: map[string]bool{}, hold: map[string]chan struct{}{}, arrived: make(chan string, 100)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		s.mu.Lock()
		s.requests = append(s.requests, r.Clone(context.Background()))
		fail, hold := s.fail[prefix], s.hold[prefix]
		s.mu.Unlock()
		s.arrived <- r.URL.Path
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		if fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/summary.json"):
			w.Write(summaryJSON("none", comp{"API", "operational"}))
		case strings.HasSuffix(r.URL.Path, "/incidents.json"):
			w.Write(incidentsJSON(inc{id: "a", status: "resolved", impact: "critical", start: at(12, 10, 0), end: at(12, 12, 0)}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *statusServer) set(prefix string, fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[prefix] = fail
}

func (s *statusServer) block(prefix string) (release func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.hold[prefix] = ch
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.hold, prefix)
		s.mu.Unlock()
		close(ch)
	}
}

func (s *statusServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *statusServer) vendor(id string) Vendor {
	return Vendor{ID: id, Name: strings.ToUpper(id), Group: GroupModels, Page: s.URL + "/" + id, Feed: FeedStatuspage, Source: s.URL + "/" + id, Track: named("API")}
}

type monitorTest struct {
	*Monitor
	server *statusServer
	wake   func()
}

func newMonitor(t *testing.T, ids ...string) *monitorTest {
	s := newStatusServer(t)
	mt := &monitorTest{server: s}
	var vendors []Vendor
	for _, id := range ids {
		vendors = append(vendors, s.vendor(id))
	}
	mt.Monitor = New(Options{HTTP: s.Client(), UserAgent: "OrielHub/1.2.3", Vendors: vendors,
		Now: func() time.Time { return now }, Zone: func() *time.Location { return zone },
		OnResume: func(fn func()) func() { mt.wake = fn; return func() {} }, Timeout: 5 * time.Second})
	t.Cleanup(mt.Close)
	return mt
}

func (m *monitorTest) refresh(t *testing.T) Board {
	t.Helper()
	b, err := m.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ids(b Board) []string {
	var out []string
	for _, r := range b.Rows {
		out = append(out, r.ID)
	}
	return out
}

// PS01：一行一家，按清單順序，不按誰先讀完。
func TestBoardOrderAndRows(t *testing.T) {
	m := newMonitor(t, "b", "a", "c")
	release := m.server.block("b") // b 最後讀完
	done := make(chan Board)
	go func() { b, _ := m.Refresh(t.Context()); done <- b }()
	for range 6 {
		<-m.server.arrived
	}
	release()
	b := <-done
	if got := ids(b); !slices.Equal(got, []string{"b", "a", "c"}) {
		t.Errorf("rows = %v, want catalog order", got)
	}
	r := b.Rows[0]
	if r.Health != HealthOperational || !slices.Equal(r.Components, []string{"API"}) || !r.WholePage || len(r.Days) != 30 || !r.ReadAt.Equal(now) || r.Problem != nil {
		t.Errorf("row = %+v", r)
	}
	if d := dayOf(t, r.Days, "2026-11-12"); d.Health != HealthOutage || uptime(d) != "91.667" {
		t.Errorf("day = %+v", d)
	}
	if len(b.Unread) != 0 || b.Reading || b.ReadAt.IsZero() {
		t.Errorf("board = %+v", b)
	}
}

// PS18、PS19：還沒讀過時是空的；之後 Watch 先送上次的結果。
func TestWatchSendsLastBoardFirst(t *testing.T) {
	m := newMonitor(t, "a")
	boards := make(chan Board, 10)
	ctx, cancel := context.WithCancel(t.Context())
	watched := make(chan error)
	go func() { watched <- m.Watch(ctx, func(b Board) error { boards <- b; return nil }) }()
	if b := <-boards; len(b.Rows) != 0 || b.Reading || !b.ReadAt.IsZero() {
		t.Fatalf("PS18 first board = %+v, want empty", b)
	}
	m.refresh(t)
	for b := range boards {
		if !b.Reading && len(b.Rows) == 1 {
			break
		}
	}
	cancel()
	if err := <-watched; !errors.Is(err, context.Canceled) {
		t.Errorf("watch = %v", err)
	}

	again := make(chan Board, 1)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	go m.Watch(ctx, func(b Board) error { again <- b; return nil })
	if b := <-again; len(b.Rows) != 1 {
		t.Errorf("PS19 first board of a new watch = %+v, want the last result", b)
	}
}

// PS21：讀取進行中又來的請求合成一次，都拿到同一份結果。
func TestRefreshJoinsReadInProgress(t *testing.T) {
	m := newMonitor(t, "a")
	release := m.server.block("a")
	first := make(chan Board)
	go func() { b, _ := m.Refresh(t.Context()); first <- b }()
	<-m.server.arrived
	<-m.server.arrived

	joined := make(chan struct{})
	m.mu.Lock()
	m.joined = func() { close(joined) }
	m.mu.Unlock()
	second := make(chan Board)
	go func() { b, _ := m.Refresh(t.Context()); second <- b }()
	<-joined
	release()
	a, b := <-first, <-second
	if a.Rev != b.Rev || len(a.Rows) != 1 {
		t.Errorf("revs %d and %d, rows %d; want one shared result", a.Rev, b.Rev, len(a.Rows))
	}
	if n := m.server.count(); n != 2 {
		t.Errorf("%d requests, want 2 (summary and incidents once)", n)
	}
}

// PS22：沒有人要求時不讀。
func TestNoReadWithoutRefresh(t *testing.T) {
	m := newMonitor(t, "a")
	boards := make(chan Board, 1)
	go m.Watch(t.Context(), func(b Board) error { boards <- b; return nil })
	<-boards
	if n := m.server.count(); n != 0 {
		t.Errorf("%d requests before any refresh", n)
	}
}

// PS23–PS25：一家沒讀成不影響其他家；讀到過的保留上次的結果並寫明原因。
func TestFailedVendor(t *testing.T) {
	m := newMonitor(t, "a", "b")
	m.refresh(t)
	m.server.set("a", true)
	b := m.refresh(t)
	if got := ids(b); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("rows = %v", got)
	}
	a := b.Rows[0]
	if a.Problem == nil || a.Problem.Code != string(CodeFetchFailed) || a.Problem.Params["provider"] != "A" || !strings.Contains(a.Problem.Params["detail"], "500") {
		t.Errorf("PS23 problem = %+v", a.Problem)
	}
	if !a.ReadAt.Equal(now) || len(a.Days) != 30 || b.Rows[1].Problem != nil {
		t.Errorf("PS23 kept row = %+v, other = %+v", a, b.Rows[1])
	}
	m.server.set("a", false)
	if b := m.refresh(t); b.Rows[0].Problem != nil {
		t.Errorf("read again: problem = %+v", b.Rows[0].Problem)
	}

	fresh := newMonitor(t, "a", "b")
	fresh.server.set("b", true)
	b = fresh.refresh(t)
	if got := ids(b); !slices.Equal(got, []string{"a"}) || len(b.Unread) != 1 || b.Unread[0].ID != "b" || b.Unread[0].Problem.Code != string(CodeFetchFailed) {
		t.Errorf("PS24 rows %v, unread %+v", got, b.Unread)
	}

	none := newMonitor(t, "a", "b")
	none.server.set("a", true)
	none.server.set("b", true)
	b = none.refresh(t)
	if len(b.Rows) != 0 || len(b.Unread) != 2 {
		t.Errorf("PS25 rows %v, unread %d", ids(b), len(b.Unread))
	}
}

// PS23：一家讀太久就算沒讀成。
func TestVendorTimeout(t *testing.T) {
	s := newStatusServer(t)
	release := s.block("slow")
	defer release()
	m := New(Options{HTTP: s.Client(), Vendors: []Vendor{s.vendor("slow")}, Now: func() time.Time { return now },
		Zone: func() *time.Location { return zone }, Timeout: time.Second})
	defer m.Close()
	b, err := m.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Unread) != 1 || !strings.Contains(b.Unread[0].Problem.Params["detail"], "timed out after 1s") {
		t.Errorf("unread = %+v", b.Unread)
	}
}

// PS26：電腦醒來時，有頁面在看就立刻讀一次；沒有就不讀。
func TestWakeReadsWhileWatched(t *testing.T) {
	m := newMonitor(t, "a")
	m.wake()
	m.mu.Lock()
	reading := m.board.Reading
	m.mu.Unlock()
	if reading || m.server.count() != 0 {
		t.Fatal("woke with nobody watching: want no read")
	}
	boards := make(chan Board, 10)
	go m.Watch(t.Context(), func(b Board) error { boards <- b; return nil })
	<-boards
	m.wake()
	for b := range boards {
		if !b.Reading && len(b.Rows) == 1 {
			break
		}
	}
}

// PS27：關閉時取消進行中的讀取；之後 Refresh 回 provider.closed，Watch 返回。
func TestClose(t *testing.T) {
	m := newMonitor(t, "a")
	m.server.block("a")
	refreshed := make(chan error)
	go func() { _, err := m.Refresh(t.Context()); refreshed <- err }()
	<-m.server.arrived
	watched := make(chan error)
	go func() { watched <- m.Watch(t.Context(), func(Board) error { return nil }) }()
	m.Close()
	if err := <-refreshed; !errors.Is(err, errClosed) {
		t.Errorf("refresh during close = %v", err)
	}
	if err := <-watched; err != nil {
		t.Errorf("watch = %v", err)
	}
	if _, err := m.Refresh(t.Context()); !errors.Is(err, errClosed) || err.(*Error).ErrorCode() != "provider.closed" {
		t.Errorf("refresh after close = %v", err)
	}
	if err := m.Watch(t.Context(), func(Board) error { return nil }); err != nil {
		t.Errorf("watch after close = %v", err)
	}
	m.Close()
}

// PS28：請求帶 OrielHub 的 User-Agent，不帶 cookie。
func TestRequestHeaders(t *testing.T) {
	m := newMonitor(t, "a")
	m.refresh(t)
	m.server.mu.Lock()
	defer m.server.mu.Unlock()
	for _, r := range m.server.requests {
		if r.Method != http.MethodGet || r.UserAgent() != "OrielHub/1.2.3" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("%s %s: UA %q, cookie %q", r.Method, r.URL, r.UserAgent(), r.Header.Get("Cookie"))
		}
	}
}

// PS01、PS31：卡片預設只顯示前六家，Moonshot AI 與 MiniMax 排第五、第六；xAI、OpenRouter 讀不了，不在清單裡（PS02）。
func TestCatalogOrder(t *testing.T) {
	var got []string
	for _, v := range Catalog() {
		got = append(got, v.ID)
	}
	want := []string{"openai", "anthropic", "google", "deepseek", "moonshot", "minimax", "mistral", "cohere", "ai21", "perplexity",
		"groq", "fireworks", "cerebras", "baseten", "anyscale", "bedrock", "github", "windsurf", "jina", "elevenlabs"}
	if !slices.Equal(got, want) {
		t.Errorf("catalog = %v\nwant      %v", got, want)
	}
}
