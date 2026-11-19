package remote

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/relay"
	"github.com/bayuncao/oriel/internal/relay/server"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/internal/usage"
	"github.com/bayuncao/oriel/internal/workspace"
)

// 配對的驗收場景（docs/acceptance/remote.md 第 2 節）：真的中繼跑在本機埠上，
// Mac 一側是 Pairing 加真的 hub（MemStore），App 一側是 PairWith。

const timeout = 5 * time.Second

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return c
}

// clock 是測試的時鐘：配對碼的期限按它算。
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t       *testing.T
	opts    relay.Options
	room    relay.RoomID
	mac     *relay.Mac
	macKeys relay.Keypair
	hub     *hub.Hub
	clock   *clock
	pairing *Pairing
	usage   *usage.Tracker
	// usageStore 是 usage 的存儲：測試直接放入用量記錄。
	usageStore *usage.MemStore
	agents     AgentSource // Agents.List 用；nil 時 Server 沒有 agent 目錄
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := server.New(server.Options{PingInterval: -1})
	hs := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.Close()
		hs.Close()
	})
	opts := relay.Options{URL: "ws" + strings.TrimPrefix(hs.URL, "http"), PingInterval: -1}

	key, err := relay.NewRoomKey()
	if err != nil {
		t.Fatal(err)
	}
	mac, err := relay.DialMac(ctx(t), opts, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mac.Close() })

	ms := hub.NewMemStore()
	c := &clock{now: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	h := hub.New(hub.Options{Log: ms, Config: ms, Index: ms, Now: c.Now,
		Git: workspace.New(workspace.Options{Runner: runner, Env: func(context.Context) (proc.Env, error) {
			return proc.Inherited(), nil
		}}),
		WorktreeRoot: t.TempDir(),
	})
	t.Cleanup(h.Close)
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	us := usage.NewMemStore()
	tracker := usage.New(usage.Options{Store: us, Now: c.Now,
		Zone: func() *time.Location { return time.UTC }})
	t.Cleanup(tracker.Close)
	return &env{t: t, opts: opts, room: key.Room(), mac: mac, macKeys: keypair(t), hub: h, clock: c,
		pairing: NewPairing(c.Now), usage: tracker, usageStore: us}
}

func keypair(t *testing.T) relay.Keypair {
	t.Helper()
	k, err := relay.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (e *env) offer() relay.PairingKey {
	e.t.Helper()
	o, err := e.pairing.Offer()
	if err != nil {
		e.t.Fatal(err)
	}
	if want := e.clock.Now().Add(PairingTTL); !o.Expires.Equal(want) {
		e.t.Fatalf("offer expires %v, want %v", o.Expires, want)
	}
	return o.Key
}

// connect 以一台新的 App 連入，回 App 那端與 Mac 那端的 Stream。
func (e *env) connect() (*relay.Stream, *relay.Stream) {
	e.t.Helper()
	app, err := relay.DialApp(ctx(e.t), e.opts, e.room)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { app.Close() })
	stream := online(e.t, app)
	s, err := e.mac.Accept(ctx(e.t))
	if err != nil {
		e.t.Fatal(err)
	}
	return stream, s
}

// macSide 在背景以 Mac 的身份配對，照 transport/remote 的做法：Respond 或 Pair
// 失敗就斷開這條連線。成功時 then 拿到加密通道。
func (e *env) macSide(s *relay.Stream, then func(*relay.Session) error) <-chan error {
	done := make(chan error, 1)
	go func() {
		keys := e.pairing.Keys()
		in, err := relay.Respond(ctx(e.t), s, e.macKeys, keys)
		var sess *relay.Session
		if err == nil {
			_, sess, err = e.pairing.Pair(ctx(e.t), e.hub, keys, in)
		}
		if err != nil {
			s.Close()
		} else if then != nil {
			err = then(sess)
		}
		done <- err
	}()
	return done
}

// pair 以一台新的 App 用 key 配對；回 App 看到的結果與 Mac 一側的錯誤。
func (e *env) pair(phone relay.Keypair, key relay.PairingKey, name string) (task.DeviceID, error, error) {
	e.t.Helper()
	stream, s := e.connect()
	macErr := e.macSide(s, nil)
	id, _, err := PairWith(ctx(e.t), stream, phone, e.macKeys.Public, key, name)
	return id, err, <-macErr
}

func online(t *testing.T, a *relay.App) *relay.Stream {
	t.Helper()
	for {
		s, changed := a.Online()
		if s != nil {
			return s
		}
		select {
		case <-changed:
		case <-ctx(t).Done():
			t.Fatal("Mac never came online")
		}
	}
}

func (e *env) devices() []hub.Device {
	e.t.Helper()
	ds, err := e.hub.Devices(ctx(e.t), hub.LocalOwner)
	if err != nil {
		e.t.Fatal(err)
	}
	return ds
}

// wantCode 確認 err 經 transport.Wire 是 code。
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if w, known := transport.Wire(err); !known || w.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func TestPA03PairingRegistersTheDevice(t *testing.T) {
	e := newEnv(t)
	phone := keypair(t)
	id, err, macErr := e.pair(phone, e.offer(), " May 的 iPhone ")
	if err != nil || macErr != nil {
		t.Fatalf("app: %v; mac: %v", err, macErr)
	}
	key := hub.DeviceKey(phone.Public)
	want := hub.Device{ID: key.ID(), Name: "May 的 iPhone", Key: key, PairedAt: e.clock.Now()}
	if id != want.ID {
		t.Fatalf("app got device %q, want %q", id, want.ID)
	}
	if ds := e.devices(); len(ds) != 1 || ds[0] != want {
		t.Fatalf("devices %+v", ds)
	}
}

func TestPairingCodes(t *testing.T) {
	cases := []struct {
		name string
		// setup 準備好配對碼並回 App 要用的那一把（可能已先用掉）。
		setup func(e *env) relay.PairingKey
		code  string // App 與 Mac 看到的代碼；空字串是握手失敗（PA07）
	}{
		{
			name: "PA04 expired after 5 minutes",
			setup: func(e *env) relay.PairingKey {
				k := e.offer()
				e.clock.advance(PairingTTL)
				return k
			},
			code: string(CodePairingExpired),
		},
		{
			name: "PA05 used by another phone",
			setup: func(e *env) relay.PairingKey {
				k := e.offer()
				if _, err, _ := e.pair(keypair(e.t), k, "first"); err != nil {
					e.t.Fatalf("first pairing: %v", err)
				}
				return k
			},
			code: string(CodePairingUsed),
		},
		{
			name: "PA06 replaced by a new code",
			setup: func(e *env) relay.PairingKey {
				old := e.offer()
				e.offer()
				return old
			},
			code: string(CodePairingExpired),
		},
		{
			name: "code closed on the desktop",
			setup: func(e *env) relay.PairingKey {
				k := e.offer()
				e.pairing.Cancel()
				return k
			},
			code: string(CodePairingExpired),
		},
		{
			name: "PA07 forged key",
			setup: func(e *env) relay.PairingKey {
				e.offer()
				forged, _ := relay.NewPairingKey()
				return forged
			},
		},
		{
			name: "no code shown",
			setup: func(e *env) relay.PairingKey {
				k, _ := relay.NewPairingKey()
				return k
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			key := tc.setup(e)
			before := len(e.devices())
			_, err, macErr := e.pair(keypair(t), key, "iPhone")
			if tc.code == "" {
				if !errors.Is(err, relay.ErrHandshake) || !errors.Is(macErr, relay.ErrHandshake) {
					t.Fatalf("app: %v; mac: %v; want ErrHandshake on both", err, macErr)
				}
			} else {
				wantCode(t, err, tc.code)
				wantCode(t, macErr, tc.code)
			}
			if got := e.devices(); len(got) != before {
				t.Fatalf("devices %+v, want %d", got, before)
			}
		})
	}
}

func TestPA08RepairKeepsTheDevice(t *testing.T) {
	e := newEnv(t)
	phone := keypair(t)
	first, err, _ := e.pair(phone, e.offer(), "iPhone")
	if err != nil {
		t.Fatal(err)
	}
	paired := e.clock.Now()
	e.clock.advance(time.Hour)
	again, err, _ := e.pair(phone, e.offer(), "iPhone 16")
	if err != nil {
		t.Fatal(err)
	}
	ds := e.devices()
	if again != first || len(ds) != 1 || ds[0].Name != "iPhone 16" || !ds[0].PairedAt.Equal(paired) {
		t.Fatalf("re-paired %q (first %q), devices %+v", again, first, ds)
	}
}

func TestBadNameKeepsTheCode(t *testing.T) {
	// 名字不合規：回 hub.invalid_field，碼不算用掉，App 改了名字可以再試。
	e := newEnv(t)
	key := e.offer()
	_, err, _ := e.pair(keypair(t), key, "  ")
	wantCode(t, err, string(hub.CodeInvalidField))
	if len(e.devices()) != 0 {
		t.Fatal("device registered with a blank name")
	}
	if _, err, _ := e.pair(keypair(t), key, "iPhone"); err != nil {
		t.Fatalf("retry with a name: %v", err)
	}
}

func TestPairedChannelWorks(t *testing.T) {
	// 配對完成後的加密通道可以直接用，App 不必重連。
	e := newEnv(t)
	stream, s := e.connect()
	key := e.offer()
	echoed := e.macSide(s, func(sess *relay.Session) error {
		msg, err := sess.Recv(ctx(t))
		if err != nil {
			return err
		}
		return sess.Send(ctx(t), msg)
	})
	_, sess, err := PairWith(ctx(t), stream, keypair(t), e.macKeys.Public, key, "iPhone")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Send(ctx(t), []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if got, err := sess.Recv(ctx(t)); err != nil || string(got) != "ping" {
		t.Fatalf("echo %q %v", got, err)
	}
	if err := <-echoed; err != nil {
		t.Fatalf("mac: %v", err)
	}
}

func TestPairRejectsIK(t *testing.T) {
	// 不是配對的握手交給 Pair 是呼叫者的錯，不登記任何設備。
	e := newEnv(t)
	stream, s := e.connect()
	initiated := make(chan struct{})
	go func() {
		defer close(initiated)
		_, _, _ = relay.Initiate(ctx(t), stream, relay.Initiator{Static: keypair(t), Mac: e.macKeys.Public})
	}()
	in, err := relay.Respond(ctx(t), s, e.macKeys, e.pairing.Keys())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.pairing.Pair(ctx(t), e.hub, nil, in); !errors.Is(err, errNotPairing) {
		t.Fatalf("Pair on an IK handshake: %v", err)
	}
	s.Close()
	<-initiated
	if len(e.devices()) != 0 {
		t.Fatal("device registered")
	}
}

func TestPairingRemembersRecentCodes(t *testing.T) {
	// 只記住最近 keepCodes 個碼；更早的碼當成過期，而不是握手失敗。
	p := NewPairing(nil)
	first, err := p.Offer()
	if err != nil {
		t.Fatal(err)
	}
	for range keepCodes {
		if _, err := p.Offer(); err != nil {
			t.Fatal(err)
		}
	}
	if keys := p.Keys(); len(keys) != keepCodes {
		t.Fatalf("%d keys remembered", len(keys))
	}
	if err := p.claim(first.Key); !errors.Is(err, errPairingExpired) {
		t.Fatalf("forgotten code: %v", err)
	}
}
