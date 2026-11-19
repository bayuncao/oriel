package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bayuncao/oriel/internal/relay"
)

// 連線上限的驗收場景 RL08–RL15（docs/acceptance/remote.md 第 1 節）。

var (
	errBusy        = &relay.Error{Code: relay.CodeBusy}
	errUnreachable = &relay.Error{Code: relay.CodeUnreachable}
)

// headerTransport 在每個請求上帶一個標頭，扮演反向代理寫入的客戶端地址，
// 或客戶端自己偽造的標頭。
type headerTransport struct{ name, value string }

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(t.name, t.value)
	return http.DefaultTransport.RoundTrip(r)
}

// dialFrom 以 App 身份連入一個沒有 Mac 的房間；value 不為空時帶上 header。
func (h *harness) dialFrom(header, value string) (*relay.App, error) {
	o := h.opts
	if value != "" {
		o.HTTPClient = &http.Client{Transport: headerTransport{header, value}}
	}
	a, err := relay.DialApp(ctx(h.t), o, newKey(h.t).Room())
	if err == nil {
		h.t.Cleanup(func() { a.Close() })
	}
	return a, err
}

// withFreed 讓測試等名額還回去。
func withFreed(freed chan struct{}) func(*Server) {
	return func(s *Server) { s.freed = func() { freed <- struct{}{} } }
}

func waitFreed(t *testing.T, freed <-chan struct{}) {
	t.Helper()
	select {
	case <-freed:
	case <-ctx(t).Done():
		t.Fatal("the relay never freed the connection's slot")
	}
}

func TestRL08ToRL12ConnectionLimits(t *testing.T) {
	const realIP, forwarded = "X-Real-IP", "X-Forwarded-For"
	cases := []struct {
		name   string
		o      Options
		header string   // 客戶端請求帶的標頭
		held   []string // 已連著的連線各自的標頭值（空 = 不帶）
		next   string   // 新連線的標頭值
		want   error    // nil：連得上
	}{
		{name: "RL08 under the global limit", o: Options{MaxConns: 2}, held: []string{""}},
		{name: "RL08 global limit reached", o: Options{MaxConns: 2}, held: []string{"", ""}, want: errBusy},
		{name: "RL08 global limit counts every address", o: Options{MaxConns: 2, ClientIPHeader: realIP}, header: realIP,
			held: []string{"192.0.2.1", "192.0.2.2"}, next: "192.0.2.3", want: errBusy},
		{name: "RL09 address limit reached", o: Options{MaxConnsPerIP: 2, ClientIPHeader: realIP}, header: realIP,
			held: []string{"192.0.2.1", "192.0.2.1"}, next: "192.0.2.1", want: errBusy},
		{name: "RL09 other addresses still join", o: Options{MaxConnsPerIP: 2, ClientIPHeader: realIP}, header: realIP,
			held: []string{"192.0.2.1", "192.0.2.1"}, next: "192.0.2.2"},
		{name: "RL10 an IPv6 /64 is one address", o: Options{MaxConnsPerIP: 1, ClientIPHeader: realIP}, header: realIP,
			held: []string{"2001:db8:1:2::1"}, next: "2001:db8:1:2:ffff::9", want: errBusy},
		{name: "RL10 another /64 is another address", o: Options{MaxConnsPerIP: 1, ClientIPHeader: realIP}, header: realIP,
			held: []string{"2001:db8:1:2::1"}, next: "2001:db8:1:3::1"},
		{name: "RL11 the last value is the client", o: Options{MaxConnsPerIP: 1, ClientIPHeader: forwarded}, header: forwarded,
			held: []string{"198.51.100.7, 192.0.2.1"}, next: "198.51.100.8, 192.0.2.1", want: errBusy},
		{name: "RL11 missing header", o: Options{ClientIPHeader: realIP}, want: errUnreachable},
		{name: "RL11 header is not an address", o: Options{ClientIPHeader: realIP}, header: realIP, next: "unknown", want: errUnreachable},
		{name: "RL12 a forged header is ignored", o: Options{MaxConnsPerIP: 1}, header: forwarded,
			held: []string{"192.0.2.1"}, next: "192.0.2.2", want: errBusy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			freed := make(chan struct{}, 8)
			h := newHarness(t, tc.o, withFreed(freed))
			var held []*relay.App
			for _, v := range tc.held {
				a, err := h.dialFrom(tc.header, v)
				if err != nil {
					t.Fatalf("held connection from %q: %v", v, err)
				}
				waitOnline(t, a, false)
				held = append(held, a)
			}

			_, err := h.dialFrom(tc.header, tc.next)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("new connection: %v, want it to join", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("new connection: %v, want %v", err, tc.want)
			}
			for i, a := range held {
				if a.Err() != nil {
					t.Fatalf("held connection %d closed: %v", i, a.Err())
				}
			}
			if !errors.Is(tc.want, errBusy) {
				return
			}
			// 有一條離開，名額還回去，新連線就連得上。
			held[0].Close()
			waitFreed(t, freed)
			if _, err := h.dialFrom(tc.header, tc.next); err != nil {
				t.Fatalf("after a connection left: %v", err)
			}
		})
	}
}

func TestClientAddress(t *testing.T) {
	cases := []struct {
		name    string
		header  string // Options.ClientIPHeader
		remote  string
		request http.Header
		want    string // 空：出錯
	}{
		{name: "IPv4 peer", remote: "192.0.2.1:4000", want: "192.0.2.1/32"},
		{name: "IPv6 peer counts by /64", remote: "[2001:db8:1:2::5]:4000", want: "2001:db8:1:2::/64"},
		{name: "IPv4-mapped IPv6 is IPv4", remote: "[::ffff:192.0.2.1]:4000", want: "192.0.2.1/32"},
		{name: "zone is dropped", remote: "[fe80::1%en0]:4000", want: "fe80::/64"},
		{name: "forged header without the option", remote: "192.0.2.1:4000",
			request: http.Header{"X-Forwarded-For": {"203.0.113.9"}}, want: "192.0.2.1/32"},
		{name: "last value of the header", header: "X-Forwarded-For", remote: "10.0.0.1:4000",
			request: http.Header{"X-Forwarded-For": {"203.0.113.9, 192.0.2.7"}}, want: "192.0.2.7/32"},
		{name: "last header line", header: "X-Forwarded-For", remote: "10.0.0.1:4000",
			request: http.Header{"X-Forwarded-For": {"203.0.113.9", "192.0.2.7"}}, want: "192.0.2.7/32"},
		{name: "header missing", header: "X-Real-IP", remote: "10.0.0.1:4000"},
		{name: "header is not an address", header: "X-Real-IP", remote: "10.0.0.1:4000",
			request: http.Header{"X-Real-Ip": {"unknown"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{ClientIPHeader: tc.header})
			r := httptest.NewRequest(http.MethodGet, relay.Path, nil)
			r.RemoteAddr = tc.remote
			for k, vs := range tc.request {
				for _, v := range vs {
					r.Header.Add(k, v)
				}
			}
			got, err := s.client(r)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("client = %s, want an error", got)
				}
				return
			}
			if err != nil || got != netip.MustParsePrefix(tc.want) {
				t.Fatalf("client = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestRL13SilentConnectionFreesItsSlot(t *testing.T) {
	freed := make(chan struct{}, 8)
	h := newHarness(t, Options{MaxConns: 1}, withFreed(freed), func(s *Server) { s.helloTimeout = time.Second })

	silent, _, err := websocket.Dial(ctx(t), h.opts.URL+relay.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.CloseNow() })
	// 還沒送 Hello 的連線也佔著名額。
	if _, err := h.dialFrom("", ""); !errors.Is(err, errBusy) {
		t.Fatalf("while the silent connection waits: %v, want %v", err, errBusy)
	}
	// 讀到中繼斷開它（讀的同時回應關閉握手），名額隨即還回去。
	if f, err := readFrame(t, silent); err == nil {
		t.Fatalf("got %s, want the silent connection closed", f.Kind)
	}
	waitFreed(t, freed)
	if _, err := h.dialFrom("", ""); err != nil {
		t.Fatalf("after the silent connection was closed: %v", err)
	}
}

func TestRL14EmptyRoomIsForgotten(t *testing.T) {
	h := newHarness(t, Options{})
	a := h.app(newKey(t).Room()) // 沒有 Mac 的房間
	waitOnline(t, a, false)
	a.Close()
	h.waitLeft()
	h.srv.mu.Lock()
	n := len(h.srv.rooms)
	h.srv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d rooms after the only connection left, want 0", n)
	}
}

func TestRL15QueuePerConnectionIsAbout4MiB(t *testing.T) {
	// remote.md RL15、D148 與 cmd/orielhub-relay 的說明都按約 4 MiB 寫；改
	// queueLen 或 relay.MaxFrame 時一起改它們。
	if got := queueLen * relay.MaxFrame; got>>20 != 4 {
		t.Fatalf("a connection can queue %d bytes, the docs say about 4 MiB", got)
	}
}
