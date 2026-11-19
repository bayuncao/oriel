package remote

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/relay"
	"github.com/bayuncao/oriel/internal/relay/server"
	"github.com/bayuncao/oriel/internal/task"
)

// 設置 › 遠程背後的控制器（docs/acceptance/remote.md DV、PA01、PA02、PA10、RC01）：
// 真的中繼跑在本機埠上，真的 hub；鑰匙串是記憶體裡的 Secrets，退避的等待由測試放行。

// memSecrets 是記憶體裡的鑰匙串；err 不為 nil 時每次讀寫都失敗（DV11）。
type memSecrets struct {
	mu   sync.Mutex
	vals map[string][]byte
	err  error
}

func newMemSecrets() *memSecrets { return &memSecrets{vals: map[string][]byte{}} }

func (m *memSecrets) Get(name string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, false, m.err
	}
	v, ok := m.vals[name]
	return slices.Clone(v), ok, nil
}

func (m *memSecrets) Set(name string, v []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.vals[name] = slices.Clone(v)
	return nil
}

func (m *memSecrets) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// waits 記下每次退避要等多久，由測試放行。
type waits struct{ ch chan wait }

type wait struct {
	d    time.Duration
	fire chan time.Time
}

func newWaits() *waits { return &waits{ch: make(chan wait, 64)} }

func (w *waits) After(d time.Duration) <-chan time.Time {
	fire := make(chan time.Time, 1)
	w.ch <- wait{d: d, fire: fire}
	return fire
}

func (w *waits) next(t *testing.T) wait {
	t.Helper()
	select {
	case x := <-w.ch:
		return x
	case <-time.After(timeout):
		t.Fatal("no backoff")
		return wait{}
	}
}

// relayServer 是另開的一個中繼，回它的地址與關掉它的函式。
func relayServer(t *testing.T) (string, func()) {
	t.Helper()
	srv := server.New(server.Options{PingInterval: -1})
	hs := httptest.NewServer(srv)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			srv.Close()
			hs.CloseClientConnections()
			hs.Close()
		})
	}
	t.Cleanup(stop)
	return "ws" + strings.TrimPrefix(hs.URL, "http"), stop
}

// unreachable 回一個沒有東西在聽的地址。
func unreachable(t *testing.T) string {
	t.Helper()
	hs := httptest.NewServer(nil)
	addr := "ws" + strings.TrimPrefix(hs.URL, "http")
	hs.Close()
	return addr
}

type ctlEnv struct {
	*env
	secrets *memSecrets
	waits   *waits
	path    string
}

func newCtlEnv(t *testing.T) *ctlEnv {
	t.Helper()
	return &ctlEnv{env: newEnv(t), secrets: newMemSecrets(), waits: newWaits(),
		path: filepath.Join(t.TempDir(), "remote.json")}
}

// control 建立並啟動一個控制器（同一個 ctlEnv 可以建多次，如重啟）。
func (e *ctlEnv) control() *Control {
	e.t.Helper()
	c := NewControl(ControlOptions{Hub: e.hub, Agents: e.agents, Usage: e.usage, Secrets: e.secrets,
		Path: e.path, Now: e.clock.Now, After: e.waits.After, PingInterval: -1})
	if err := c.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(c.Close)
	return c
}

// on 設好中繼地址、打開遠程並等它連上。
func (e *ctlEnv) on(c *Control, addr string) {
	e.t.Helper()
	if err := c.SetRelay(addr); err != nil {
		e.t.Fatal(err)
	}
	if err := c.SetOn(true); err != nil {
		e.t.Fatal(err)
	}
	until(e.t, c, state(StateConnected))
}

func (e *ctlEnv) offer(c *Control) PairingLink {
	e.t.Helper()
	o, err := c.Offer()
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

// until 等 c 的狀態滿足 ok，回那一份。
func until(t *testing.T, c *Control, ok func(Status) bool) Status {
	t.Helper()
	x, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var got Status
	found := errors.New("found")
	err := c.Watch(x, func(s Status) error {
		got = s
		if ok(s) {
			return found
		}
		return nil
	})
	if err != found {
		t.Fatalf("status never matched; last %+v (error %+v)", got, got.Error)
	}
	return got
}

func state(want State) func(Status) bool { return func(s Status) bool { return s.State == want } }

// pairedLink 是解開的配對連結。
type pairedLink struct {
	relay string
	room  relay.RoomID
	mac   relay.PublicKey
	key   relay.PairingKey
}

func parseLink(t *testing.T, link string) pairedLink {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "orielhub" || u.Host != "pair" {
		t.Fatalf("link %q: %v", link, err)
	}
	q := u.Query()
	p := pairedLink{relay: q.Get("relay")}
	if p.room, err = relay.ParseRoomID(q.Get("room")); err != nil {
		t.Fatal(err)
	}
	if err := p.mac.UnmarshalText([]byte(q.Get("mac"))); err != nil {
		t.Fatal(err)
	}
	if err := p.key.UnmarshalText([]byte(q.Get("key"))); err != nil {
		t.Fatal(err)
	}
	return p
}

// pairVia 以一台新手機照連結配對，回它的密鑰與留著的連線。
func (e *ctlEnv) pairVia(link string, name string) (relay.Keypair, *client) {
	e.t.Helper()
	p := parseLink(e.t, link)
	phone := keypair(e.t)
	app, err := relay.DialApp(ctx(e.t), relay.Options{URL: p.relay, PingInterval: -1}, p.room)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { app.Close() })
	id, sess, err := PairWith(ctx(e.t), online(e.t, app), phone, p.mac, p.key, name)
	if err != nil {
		e.t.Fatalf("pair: %v", err)
	}
	return phone, newClient(e.t, id, sess)
}

// rejoin 以配對過的手機照連結裡的中繼與房間再連入（Noise_IK）。
func (e *ctlEnv) rejoin(link string, phone relay.Keypair) (*client, error) {
	e.t.Helper()
	p := parseLink(e.t, link)
	sub := *e.env
	sub.opts = relay.Options{URL: p.relay, PingInterval: -1}
	sub.room = p.room
	sub.macKeys = relay.Keypair{Public: p.mac}
	return sub.dial(phone)
}

// rows 等設備列表滿足 ok。
func rows(t *testing.T, c *Control, ok func([]DeviceRow) bool) []DeviceRow {
	t.Helper()
	x, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var got []DeviceRow
	found := errors.New("found")
	err := c.WatchDevices(x, func(rs []DeviceRow) error {
		got = rs
		if ok(rs) {
			return found
		}
		return nil
	})
	if err != found {
		t.Fatalf("devices never matched; last %+v", got)
	}
	return got
}

func onlineIs(id task.DeviceID, on bool) func([]DeviceRow) bool {
	return func(rs []DeviceRow) bool {
		for _, r := range rs {
			if r.Device.ID == id {
				return r.Online == on
			}
		}
		return false
	}
}

func TestDV01OffByDefault(t *testing.T) {
	e := newCtlEnv(t)
	c := e.control()
	if s := c.Status(); s.On || s.State != StateOff {
		t.Fatalf("status %+v", s)
	}
	_, err := c.Offer()
	wantCode(t, err, string(CodeNotConnected))
}

func TestDV09OnWithoutRelay(t *testing.T) {
	e := newCtlEnv(t)
	c := e.control()
	if err := c.SetOn(true); err != nil {
		t.Fatal(err)
	}
	if s := c.Status(); !s.On || s.State != StateNoRelay {
		t.Fatalf("status %+v", s)
	}
	_, err := c.Offer()
	wantCode(t, err, string(CodeNotConnected))
}

func TestDV11KeychainUnavailable(t *testing.T) {
	e := newCtlEnv(t)
	e.secrets.fail(errors.New("no keychain"))
	c := e.control()
	wantCode(t, c.SetOn(true), string(CodeKeychainUnavailable))
	if s := c.Status(); s.On || s.State != StateOff {
		t.Fatalf("status %+v", s)
	}
	if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings saved: %v", err)
	}
}

// PA01、PA02、PA03、PA05、DV02、DV05、DV13：打開就連上中繼，密鑰進鑰匙串；添加設備的
// 連結讓手機配對並留著連線；用過的連結不能再用；撤銷後連不進來。
func TestDV02ConnectAndPair(t *testing.T) {
	e := newCtlEnv(t)
	addr, _ := relayServer(t)
	c := e.control()
	e.on(c, addr+"/")
	if s := c.Status(); s.Relay != addr || s.Error != nil {
		t.Fatalf("status %+v", s)
	}
	if _, ok, _ := e.secrets.Get(secretMac); !ok {
		t.Fatal("no Mac key in the keychain")
	}

	offer := e.offer(c)
	if want := e.clock.Now().Add(PairingTTL); !offer.Expires.Equal(want) {
		t.Fatalf("expires %v, want %v", offer.Expires, want)
	}
	p := parseLink(t, offer.Link)
	if p.relay != addr {
		t.Fatalf("link relay %q, want %q", p.relay, addr)
	}
	phone, app := e.pairVia(offer.Link, "iPhone")
	// 第一則訊息到了才算在線（CN12）。
	if r := app.call("Tasks", nil); r.Error != nil {
		t.Fatalf("Tasks: %+v", r.Error)
	}
	rows(t, c, onlineIs(app.device, true))

	other, err := relay.DialApp(ctx(t), relay.Options{URL: p.relay, PingInterval: -1}, p.room)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	_, _, err = PairWith(ctx(t), online(t, other), keypair(t), p.mac, p.key, "second")
	wantCode(t, err, string(CodePairingUsed))

	if err := c.Revoke(ctx(t), app.device); err != nil {
		t.Fatal(err)
	}
	if err := app.closed(); err == nil {
		t.Fatal("revoked phone still connected")
	}
	rows(t, c, func(rs []DeviceRow) bool { return len(rs) == 0 })
	_, err = e.rejoin(offer.Link, phone)
	wantCode(t, err, string(CodeUnknownDevice))
}

// DV14、DV04：“在線”隨連線變化；改名。
func TestDV14OnlineFollowsConnections(t *testing.T) {
	e := newCtlEnv(t)
	addr, _ := relayServer(t)
	c := e.control()
	e.on(c, addr)
	offer := e.offer(c)
	phone, app := e.pairVia(offer.Link, "iPhone")
	id := app.device
	app.call("Tasks", nil) // 第一則訊息到了才算在線（CN12）
	rows(t, c, onlineIs(id, true))
	app.send([]byte("not json")) // Mac 斷開它（RQ09）
	if err := app.closed(); err == nil {
		t.Fatal("still connected")
	}
	rows(t, c, onlineIs(id, false))
	again, err := e.rejoin(offer.Link, phone)
	if err != nil {
		t.Fatal(err)
	}
	if again.device != id {
		t.Fatalf("rejoined as %q, want %q", again.device, id)
	}
	again.call("Tasks", nil)
	rows(t, c, onlineIs(id, true))

	d, err := c.Rename(ctx(t), id, "May 的 iPhone")
	if err != nil || d.Name != "May 的 iPhone" {
		t.Fatalf("rename: %+v, %v", d, err)
	}
	rows(t, c, func(rs []DeviceRow) bool {
		return len(rs) == 1 && rs[0].Device.Name == "May 的 iPhone" && rs[0].Online
	})
}

// DV10、RC01：連不上就按 1、2、4 秒……退避；Retry 立即重連；連上後歸零，斷了從 1 秒再來。
func TestDV10Backoff(t *testing.T) {
	e := newCtlEnv(t)
	c := e.control()
	if err := c.SetRelay(unreachable(t)); err != nil {
		t.Fatal(err)
	}
	if err := c.SetOn(true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		w := e.waits.next(t)
		if w.d != want {
			t.Fatalf("backoff %v, want %v", w.d, want)
		}
		s := c.Status()
		if s.State != StateRetrying || s.Error == nil || s.Error.Code != string(relay.CodeUnreachable) ||
			!s.RetryAt.Equal(e.clock.Now().Add(want)) {
			t.Fatalf("status %+v (error %+v)", s, s.Error)
		}
		if want == 4*time.Second {
			c.Retry() // 不等 4 秒
		} else {
			w.fire <- time.Time{}
		}
	}
	if w := e.waits.next(t); w.d != 8*time.Second {
		t.Fatalf("after retry: backoff %v, want 8s", w.d)
	}

	addr, stop := relayServer(t)
	if err := c.SetRelay(addr); err != nil {
		t.Fatal(err)
	}
	until(t, c, state(StateConnected))
	stop()
	if w := e.waits.next(t); w.d != time.Second {
		t.Fatalf("after a connection: backoff %v, want 1s", w.d)
	}
	if s := c.Status(); s.State != StateRetrying || s.Error == nil {
		t.Fatalf("status %+v", s)
	}
}

// DV12、PA10：重啟後自動連上同一個房間，已配對的手機照常連入。
func TestDV12RestartKeepsPairing(t *testing.T) {
	e := newCtlEnv(t)
	addr, _ := relayServer(t)
	c := e.control()
	e.on(c, addr)
	offer := e.offer(c)
	phone, app := e.pairVia(offer.Link, "iPhone")
	c.Close()
	if err := app.closed(); err == nil {
		t.Fatal("still connected after Close")
	}

	again := e.control()
	if s := until(t, again, state(StateConnected)); !s.On || s.Relay != addr {
		t.Fatalf("status %+v", s)
	}
	if _, err := e.rejoin(offer.Link, phone); err != nil {
		t.Fatalf("paired phone after restart: %v", err)
	}
}

// DV12：開著但鑰匙串讀不出密鑰時是 failed；Retry 在鑰匙串恢復後連上。
func TestDV12KeychainUnavailableAtStart(t *testing.T) {
	e := newCtlEnv(t)
	addr, _ := relayServer(t)
	c := e.control()
	e.on(c, addr)
	c.Close()

	e.secrets.fail(errors.New("locked"))
	again := e.control()
	s := again.Status()
	if s.State != StateFailed || s.Error == nil || s.Error.Code != string(CodeKeychainUnavailable) {
		t.Fatalf("status %+v (error %+v)", s, s.Error)
	}
	e.secrets.fail(nil)
	again.Retry()
	until(t, again, state(StateConnected))
}

// DV07：關掉斷開中繼與設備，設備保留；再打開時無需重新配對。
func TestDV07TurnOff(t *testing.T) {
	e := newCtlEnv(t)
	addr, _ := relayServer(t)
	c := e.control()
	e.on(c, addr)
	offer := e.offer(c)
	phone, app := e.pairVia(offer.Link, "iPhone")
	if err := c.SetOn(false); err != nil {
		t.Fatal(err)
	}
	if s := c.Status(); s.On || s.State != StateOff {
		t.Fatalf("status %+v", s)
	}
	if err := app.closed(); err == nil {
		t.Fatal("still connected")
	}
	if ds := e.devices(); len(ds) != 1 {
		t.Fatalf("devices %+v", ds)
	}
	if err := c.SetOn(true); err != nil {
		t.Fatal(err)
	}
	until(t, c, state(StateConnected))
	if _, err := e.rejoin(offer.Link, phone); err != nil {
		t.Fatalf("after turning on again: %v", err)
	}
}

// DV06、CN05：重置換掉房間與密鑰，撤銷所有設備；鑰匙串不可用時什麼都不變。
func TestDV06Reset(t *testing.T) {
	e := newCtlEnv(t)
	addr, _ := relayServer(t)
	c := e.control()
	e.on(c, addr)
	before := e.offer(c)
	_, app := e.pairVia(before.Link, "iPhone")
	if err := c.Reset(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if err := app.closed(); err == nil {
		t.Fatal("still connected")
	}
	if ds := e.devices(); len(ds) != 0 {
		t.Fatalf("devices %+v", ds)
	}
	until(t, c, state(StateConnected))
	after := e.offer(c)
	if a, b := parseLink(t, after.Link), parseLink(t, before.Link); a.room == b.room || a.mac == b.mac {
		t.Fatal("room or Mac key unchanged")
	}

	_, kept := e.pairVia(after.Link, "iPad")
	e.secrets.fail(errors.New("no keychain"))
	wantCode(t, c.Reset(ctx(t)), string(CodeKeychainUnavailable))
	if ds := e.devices(); len(ds) != 1 || ds[0].ID != kept.device {
		t.Fatalf("devices after a failed reset %+v", ds)
	}
}

// DV08：換中繼地址：斷開舊中繼、連新的；顯示中的二維碼作廢。
func TestDV08ChangeRelay(t *testing.T) {
	e := newCtlEnv(t)
	first, _ := relayServer(t)
	second, _ := relayServer(t)
	c := e.control()
	e.on(c, first)
	old := e.offer(c)
	if err := c.SetRelay(second); err != nil {
		t.Fatal(err)
	}
	if s := until(t, c, state(StateConnected)); s.Relay != second {
		t.Fatalf("status %+v", s)
	}
	if p := parseLink(t, e.offer(c).Link); p.relay != second {
		t.Fatalf("link relay %q", p.relay)
	}
	// 新的連結也作廢了舊碼；照新地址用舊碼是過期。
	p := parseLink(t, old.Link)
	app, err := relay.DialApp(ctx(t), relay.Options{URL: second, PingInterval: -1}, p.room)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	_, _, err = PairWith(ctx(t), online(t, app), keypair(t), p.mac, p.key, "x")
	wantCode(t, err, string(CodePairingExpired))
}

// DV15、DV16：中繼地址的格式；關著時也能改，不連。
func TestDV15RelayAddress(t *testing.T) {
	e := newCtlEnv(t)
	c := e.control()
	for _, bad := range []string{"http://relay.example", "relay.example", "wss://", "wss://u@relay.example",
		"wss://relay.example/path", "wss://relay.example?x=1"} {
		wantCode(t, c.SetRelay(bad), string(CodeBadRelayURL))
	}
	if s := c.Status(); s.Relay != "" {
		t.Fatalf("saved a bad address: %+v", s)
	}
	for _, tc := range []struct{ in, want string }{
		{" wss://relay.example/ ", "wss://relay.example"},
		{"ws://127.0.0.1:8080", "ws://127.0.0.1:8080"},
		{"", ""},
	} {
		if err := c.SetRelay(tc.in); err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if s := c.Status(); s.Relay != tc.want || s.On || s.State != StateOff {
			t.Fatalf("%q: status %+v", tc.in, s)
		}
		if s, err := readSettings(e.path); err != nil || s.Relay != tc.want {
			t.Fatalf("%q: saved %+v, %v", tc.in, s, err)
		}
	}
}

// Close 結束 Watch 與 WatchDevices。
func TestCloseEndsWatches(t *testing.T) {
	e := newCtlEnv(t)
	c := e.control()
	done := make(chan error, 2)
	started := make(chan struct{}, 2)
	go func() {
		done <- c.Watch(context.Background(), func(Status) error { started <- struct{}{}; return nil })
	}()
	go func() {
		done <- c.WatchDevices(context.Background(), func([]DeviceRow) error { started <- struct{}{}; return nil })
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(timeout):
			t.Fatal("watch did not start")
		}
	}
	c.Close()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(timeout):
			t.Fatal("watch did not end")
		}
	}
}
