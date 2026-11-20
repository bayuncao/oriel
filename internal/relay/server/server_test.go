package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bayuncao/oriel/internal/relay"
)

// 中繼的驗收場景（docs/acceptance/remote.md 第 1 節）：真的中繼跑在本機
// 埠上，Mac 與 App 兩側都用 relay 的真客戶端。

const timeout = 5 * time.Second

type harness struct {
	t    *testing.T
	srv  *Server
	opts relay.Options
	logs *syncBuffer
	left chan relay.RoomID // 每條連線清理完畢送一次
}

// syncBuffer 讓 slog 從多個 goroutine 寫入。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// newHarness 起一個中繼；configure 在它開始服務之前改 Server 的測試欄位。
func newHarness(t *testing.T, o Options, configure ...func(*Server)) *harness {
	t.Helper()
	logs := &syncBuffer{}
	o.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	o.PingInterval = -1
	srv := New(o)
	left := make(chan relay.RoomID, 64)
	srv.left = func(id relay.RoomID) { left <- id }
	for _, f := range configure {
		f(srv)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.Close()
		hs.Close()
	})
	url := "ws" + strings.TrimPrefix(hs.URL, "http")
	return &harness{t: t, srv: srv, opts: relay.Options{URL: url, PingInterval: -1}, logs: logs, left: left}
}

// waitLeft 等一條連線在中繼那邊清理完畢。
func (h *harness) waitLeft() {
	h.t.Helper()
	select {
	case <-h.left:
	case <-ctx(h.t).Done():
		h.t.Fatal("relay never cleaned up a connection")
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return c
}

func newKey(t *testing.T) relay.RoomKey {
	t.Helper()
	key, err := relay.NewRoomKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func (h *harness) mac(key relay.RoomKey) *relay.Mac {
	h.t.Helper()
	m, err := relay.DialMac(ctx(h.t), h.opts, key)
	if err != nil {
		h.t.Fatalf("DialMac: %v", err)
	}
	h.t.Cleanup(func() { m.Close() })
	return m
}

func (h *harness) app(room relay.RoomID) *relay.App {
	h.t.Helper()
	a, err := relay.DialApp(ctx(h.t), h.opts, room)
	if err != nil {
		h.t.Fatalf("DialApp: %v", err)
	}
	h.t.Cleanup(func() { a.Close() })
	return a
}

func accept(t *testing.T, m *relay.Mac) *relay.Stream {
	t.Helper()
	s, err := m.Accept(ctx(t))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	return s
}

func send(t *testing.T, c relay.Conn, msg string) {
	t.Helper()
	if err := c.Send(ctx(t), []byte(msg)); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func expect(t *testing.T, c relay.Conn, want string) {
	t.Helper()
	got, err := c.Recv(ctx(t))
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if string(got) != want {
		t.Fatalf("Recv = %q, want %q", got, want)
	}
}

// waitOnline 等 App 看到 Mac 的狀態變成 want；在線時回傳這一世代的 Stream。
func waitOnline(t *testing.T, a *relay.App, want bool) *relay.Stream {
	t.Helper()
	c := ctx(t)
	for {
		s, changed := a.Online()
		if (s != nil) == want {
			return s
		}
		select {
		case <-changed:
		case <-c.Done():
			t.Fatalf("app never saw online=%v", want)
		}
	}
}

func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-ctx(t).Done():
		t.Fatalf("%s not closed", what)
	}
}

// roundTrip 確認 App 與 Mac 之間雙向通。
func roundTrip(t *testing.T, a, m *relay.Stream, tag string) {
	t.Helper()
	send(t, a, "from app "+tag)
	expect(t, m, "from app "+tag)
	send(t, m, "from mac "+tag)
	expect(t, a, "from mac "+tag)
}

// raw 是不經 relay 客戶端、直接說幀協定的連線，用來送客戶端不會送的幀。
func (h *harness) raw(hello relay.Hello) *websocket.Conn {
	h.t.Helper()
	ws, _, err := websocket.Dial(ctx(h.t), h.opts.URL+relay.Path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { ws.CloseNow() })
	b, _ := json.Marshal(hello)
	writeFrame(h.t, ws, relay.Frame{Kind: relay.KindHello, Payload: b})
	return ws
}

func writeFrame(t *testing.T, ws *websocket.Conn, f relay.Frame) {
	t.Helper()
	if err := ws.Write(ctx(t), websocket.MessageBinary, f.Marshal()); err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, ws *websocket.Conn) (relay.Frame, error) {
	t.Helper()
	_, b, err := ws.Read(ctx(t))
	if err != nil {
		return relay.Frame{}, err
	}
	return relay.ParseFrame(b)
}

func TestRL01MacEntersTheRoomOfItsKey(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	a := h.app(key.Room())
	roundTrip(t, waitOnline(t, a, true), accept(t, m), "1")
}

func TestRL02RoomIDAloneCannotActAsMac(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	owner := h.mac(key)

	// 知道房間號的人（例如已撤銷的手機）以 Mac 身份報上房間號：拒絕。
	impostor := h.raw(relay.Hello{V: relay.Version, Role: relay.RoleMac, Room: key.Room(), Key: newKey(t).String()})
	if f, err := readFrame(t, impostor); err == nil {
		t.Fatalf("impostor got %s", f.Kind)
	}
	// 用自己的密鑰只進得了自己的房間。
	h.mac(newKey(t))

	if err := owner.Err(); err != nil {
		t.Fatalf("owner disconnected: %v", err)
	}
	a := h.app(key.Room())
	roundTrip(t, waitOnline(t, a, true), accept(t, owner), "after impostor")
}

func TestRL03AppJoinsOnlineMac(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	a := h.app(key.Room())

	as, _ := a.Online()
	if as == nil {
		t.Fatal("app sees the Mac offline")
	}
	s := accept(t, m)
	roundTrip(t, as, s, "1")
	roundTrip(t, as, s, "2")
}

func TestRL04AppWaitsForMac(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	a := h.app(key.Room())
	if s, _ := a.Online(); s != nil {
		t.Fatal("app sees an absent Mac online")
	}

	m := h.mac(key)
	roundTrip(t, waitOnline(t, a, true), accept(t, m), "late mac")
}

func TestRL05RelaySeesNoPlaintext(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	a := h.app(key.Room())
	as := waitOnline(t, a, true)
	s := accept(t, m)

	const secret = "SECRET-PAYLOAD-7f3a"
	send(t, as, secret)
	expect(t, s, secret)
	send(t, s, secret)
	expect(t, as, secret)
	a.Close()
	h.waitLeft()

	logs := h.logs.String()
	if !strings.Contains(logs, key.Room().String()) {
		t.Errorf("logs lack the room id:\n%s", logs)
	}
	for _, leak := range []string{secret, key.String()} {
		if strings.Contains(logs, leak) {
			t.Errorf("logs contain %q:\n%s", leak, logs)
		}
	}
}

func TestRL06RepliesGoToTheirOwnApp(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	a1 := waitOnline(t, h.app(key.Room()), true)
	s1 := accept(t, m)
	a2 := waitOnline(t, h.app(key.Room()), true)
	s2 := accept(t, m)

	send(t, a1, "q1")
	send(t, a2, "q2")
	expect(t, s1, "q1")
	expect(t, s2, "q2")
	send(t, s2, "r2")
	send(t, s1, "r1")
	expect(t, a1, "r1")
	expect(t, a2, "r2")
}

func TestRL07UnknownRoomLooksOffline(t *testing.T) {
	h := newHarness(t, Options{})
	if s, _ := h.app(newKey(t).Room()).Online(); s != nil {
		t.Fatal("unknown room reported online")
	}

	// 與 Mac 曾在、現在不在的房間沒有分別。
	key := newKey(t)
	h.mac(key).Close()
	h.waitLeft()
	if s, _ := h.app(key.Room()).Online(); s != nil {
		t.Fatal("room whose Mac left reported online")
	}
}

func TestMacLeavingTellsApps(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	a := h.app(key.Room())
	old := waitOnline(t, a, true)
	accept(t, m)

	m.Close()
	waitOnline(t, a, false)
	if _, err := old.Recv(ctx(t)); !errors.Is(err, relay.ErrClosed) {
		t.Fatalf("Recv on the gone Mac's stream: %v, want ErrClosed", err)
	}

	m2 := h.mac(key) // 重連（RC01）
	roundTrip(t, waitOnline(t, a, true), accept(t, m2), "reconnected")
}

func TestNewMacConnectionReplacesOld(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	old := h.mac(key)
	a := h.app(key.Room())
	accept(t, old)
	first, changed := a.Online()

	m := h.mac(key)
	waitDone(t, old.Done(), "old Mac connection")
	waitDone(t, changed, "app status")
	waitDone(t, first.Done(), "first generation's stream") // App 在新的 Stream 上重新握手
	next := waitOnline(t, a, true)
	if next == first {
		t.Fatal("app kept the old generation's stream")
	}
	roundTrip(t, next, accept(t, m), "new mac")
}

func TestStaleGenerationIsDropped(t *testing.T) {
	// App 還沒看到 Mac 換了連線就送出的幀屬於舊的 Noise 會話：中繼丟掉，
	// 新的 Mac 只收到新世代的幀。
	h := newHarness(t, Options{})
	key := newKey(t)
	h.mac(key)
	app := h.raw(relay.Hello{V: relay.Version, Role: relay.RoleApp, Room: key.Room()})
	first, err := readFrame(t, app)
	if err != nil || first.Kind != relay.KindStatus || first.Payload[0] != 1 {
		t.Fatalf("first status: %+v, %v", first, err)
	}

	m := h.mac(key)
	next, err := readFrame(t, app)
	if err != nil || next.Kind != relay.KindStatus || next.Conn == first.Conn {
		t.Fatalf("status after replacement: %+v, %v", next, err)
	}
	writeFrame(t, app, relay.Frame{Kind: relay.KindData, Conn: first.Conn, Payload: []byte("stale")})
	writeFrame(t, app, relay.Frame{Kind: relay.KindData, Conn: next.Conn, Payload: []byte("fresh")})
	expect(t, accept(t, m), "fresh")
}

func TestMacClosesAnApp(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	a := h.app(key.Room())
	s := accept(t, m)
	other := h.app(key.Room())
	so := accept(t, m)

	s.Close()
	waitDone(t, a.Done(), "closed app")
	if !errors.Is(a.Err(), relay.ErrRefused) {
		t.Fatalf("closed app: %v, want ErrRefused", a.Err())
	}
	roundTrip(t, waitOnline(t, other, true), so, "other app")
}

func TestAppLeavingClosesItsStream(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	a := h.app(key.Room())
	s := accept(t, m)

	send(t, waitOnline(t, a, true), "last words")
	a.Close()
	expect(t, s, "last words") // 已收到的先交出
	if _, err := s.Recv(ctx(t)); !errors.Is(err, relay.ErrClosed) {
		t.Fatalf("Recv after app left: %v, want ErrClosed", err)
	}
}

func TestCancelledSendKeepsTheLink(t *testing.T) {
	// 一台 App 的請求被取消，不能斷開 Mac 到中繼、連著其他 App 的連線。
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	a := h.app(key.Room())
	as := waitOnline(t, a, true)
	s := accept(t, m)

	cancelled, cancel := context.WithCancel(ctx(t))
	cancel()
	if err := s.Send(cancelled, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Mac Send with a cancelled ctx: %v", err)
	}
	if err := as.Send(cancelled, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("App Send with a cancelled ctx: %v", err)
	}
	if m.Err() != nil || a.Err() != nil {
		t.Fatalf("link closed: mac %v, app %v", m.Err(), a.Err())
	}
	roundTrip(t, as, s, "after cancel")
}

func TestRL17SlowStreamIsClosedAlone(t *testing.T) {
	// Mac 不讀的那條連線收件匣滿了：只斷開它，Mac 的其他連線照常。中繼
	// 讓送得快的 App 等（RL16），不斷開 Mac，所以 257 則都送得到 Mac。
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	slow := h.app(key.Room())
	slowS := waitOnline(t, slow, true)
	accept(t, m)
	other := waitOnline(t, h.app(key.Room()), true)
	so := accept(t, m)

	const inboxLen = 256 // relay 的 inboxLen
	for i := range inboxLen + 1 {
		if err := slowS.Send(ctx(t), []byte{byte(i)}); err != nil {
			break // 中繼已斷開它
		}
	}
	waitDone(t, slow.Done(), "slow app")
	if !errors.Is(slow.Err(), relay.ErrRefused) {
		t.Fatalf("slow app: %v, want ErrRefused", slow.Err())
	}
	if m.Err() != nil {
		t.Fatalf("mac link closed: %v", m.Err())
	}
	roundTrip(t, other, so, "other app")
}

// maxFlood 是 fullMac 最多送的幀數：每則一個 MaxPayload，共約 256 MiB，
// 遠大於 App → 中繼 → Mac 一路上的 socket 緩衝，Mac 不讀時中繼的 Mac 佇
// 列一定在這之前滿。
const maxFlood = 4096

// floodEnd 是 fullMac 最後一則的序號，表示 App 送完了。
const floodEnd = ^uint32(0)

// withWaiting 在 App 轉給 Mac 的幀開始等 Mac 時通知 waiting。
func withWaiting(waiting chan struct{}) func(*Server) {
	return func(s *Server) {
		s.waiting = func() {
			select {
			case waiting <- struct{}{}:
			default:
			}
		}
	}
}

// fullMac 讓房間裡的一台 App 不停送幀給一個還不讀的 Mac，等到中繼的 Mac
// 佇列滿了、App 轉來的幀開始等（RL16）；之後 App 送完手上那則就以
// floodEnd 收尾。回傳 Mac 的原始連線，與 App 送完時收到結果的通道。
func fullMac(t *testing.T, h *harness, key relay.RoomKey, waiting <-chan struct{}) (*websocket.Conn, <-chan error) {
	t.Helper()
	mac := h.raw(relay.Hello{V: relay.Version, Role: relay.RoleMac, Key: key.String()})
	mac.SetReadLimit(relay.MaxFrame)
	s := waitOnline(t, h.app(key.Room()), true)
	stop := make(chan struct{})
	sent := make(chan error, 1)
	go func() {
		msg := make([]byte, relay.MaxPayload)
		for i := range uint32(maxFlood) {
			select {
			case <-stop:
				i = floodEnd
			default:
			}
			binary.BigEndian.PutUint32(msg, i)
			if err := s.Send(context.Background(), msg); err != nil {
				sent <- fmt.Errorf("send %d: %w", i, err)
				return
			}
			if i == floodEnd {
				sent <- nil
				return
			}
		}
		sent <- fmt.Errorf("sent %d frames without waiting for the Mac", maxFlood)
	}()
	select {
	case <-waiting:
	case err := <-sent:
		t.Fatal(err)
	case <-ctx(t).Done():
		t.Fatal("app frames never waited for the Mac")
	}
	close(stop)
	return mac, sent
}

// drain 讀到 floodEnd 為止，確認資料幀依序來自 conn，回傳期間的控制幀。
// 中繼斷開 Mac 時失敗。
func drain(t *testing.T, mac *websocket.Conn, conn uint32) []relay.Frame {
	t.Helper()
	var control []relay.Frame
	for next := uint32(0); ; {
		f, err := readFrame(t, mac)
		if err != nil {
			t.Fatalf("mac disconnected after %d frames: %v", next, err)
		}
		if f.Kind != relay.KindData {
			control = append(control, f)
			continue
		}
		seq := binary.BigEndian.Uint32(f.Payload)
		if seq == floodEnd && f.Conn == conn {
			if next <= queueLen {
				t.Fatalf("only %d frames before the end, want more than the queue holds", next)
			}
			return control
		}
		if f.Conn != conn || seq != next {
			t.Fatalf("frame %d: conn %d seq %d, want conn %d seq %d", next, f.Conn, seq, conn, next)
		}
		next++
	}
}

func TestRL16FastAppWaitsForTheMac(t *testing.T) {
	waiting := make(chan struct{}, 1)
	h := newHarness(t, Options{}, withWaiting(waiting))
	key := newKey(t)
	mac, sent := fullMac(t, h, key, waiting)

	// 佇列滿著，另一個房間照常。
	other := newKey(t)
	m := h.mac(other)
	roundTrip(t, waitOnline(t, h.app(other.Room()), true), accept(t, m), "other room")

	// Mac 開始讀：沒被斷開，App 送的全部依序送到。
	control := drain(t, mac, 1)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	want := []relay.Frame{{Kind: relay.KindReady}, {Kind: relay.KindOpen, Conn: 1}}
	if !equalFrames(control, want) {
		t.Fatalf("control frames %v, want %v", control, want)
	}
}

func TestRL18JoinAndLeaveReachAFullMac(t *testing.T) {
	waiting := make(chan struct{}, 1)
	h := newHarness(t, Options{}, withWaiting(waiting))
	key := newKey(t)
	mac, sent := fullMac(t, h, key, waiting)

	// Mac 的資料幀佔滿佇列時，另一台 App 加入又離開。
	b := h.app(key.Room())
	waitOnline(t, b, true)
	b.Close()
	h.waitLeft()

	control := drain(t, mac, 1)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	want := []relay.Frame{{Kind: relay.KindReady}, {Kind: relay.KindOpen, Conn: 1}, {Kind: relay.KindOpen, Conn: 2}, {Kind: relay.KindClose, Conn: 2}}
	if !equalFrames(control, want) {
		t.Fatalf("control frames %v, want %v", control, want)
	}
}

func equalFrames(got, want []relay.Frame) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Kind != want[i].Kind || got[i].Conn != want[i].Conn || !bytes.Equal(got[i].Payload, want[i].Payload) {
			return false
		}
	}
	return true
}

func TestHandshakeRefusedThroughRelay(t *testing.T) {
	// Mac 解不開握手就斷開這台 App；App 得到 ErrHandshake，不是斷線。
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	a := h.app(key.Room())
	as := waitOnline(t, a, true)
	s := accept(t, m)

	macKeys, appKeys, wrong := keypair(t), keypair(t), keypair(t)
	go func() {
		if _, err := relay.Respond(ctx(t), s, macKeys, nil); err != nil {
			s.Close()
		}
	}()
	_, _, err := relay.Initiate(ctx(t), as, relay.Initiator{Static: appKeys, Mac: wrong.Public})
	if !errors.Is(err, relay.ErrHandshake) {
		t.Fatalf("Initiate = %v, want ErrHandshake", err)
	}
}

func TestRoomFull(t *testing.T) {
	h := newHarness(t, Options{MaxApps: 1})
	key := newKey(t)
	h.mac(key)
	h.app(key.Room())
	if _, err := relay.DialApp(ctx(t), h.opts, key.Room()); err == nil {
		t.Fatal("second app joined a full room")
	}
}

func TestMalformedHello(t *testing.T) {
	key := newKey(t)
	cases := []struct {
		name  string
		hello relay.Hello
	}{
		{name: "app without a room", hello: relay.Hello{V: relay.Version, Role: relay.RoleApp}},
		{name: "app with a room key", hello: relay.Hello{V: relay.Version, Role: relay.RoleApp, Room: key.Room(), Key: key.String()}},
		{name: "mac without a key", hello: relay.Hello{V: relay.Version, Role: relay.RoleMac}},
		{name: "mac naming a room", hello: relay.Hello{V: relay.Version, Role: relay.RoleMac, Room: key.Room(), Key: key.String()}},
		{name: "unknown role", hello: relay.Hello{V: relay.Version, Role: "watch", Room: key.Room()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Options{})
			if f, err := readFrame(t, h.raw(tc.hello)); err == nil {
				t.Fatalf("got %s, want the connection closed", f.Kind)
			}
		})
	}
}

func TestVersionUnsupported(t *testing.T) {
	h := newHarness(t, Options{})
	ws := h.raw(relay.Hello{V: relay.Version + 1, Role: relay.RoleApp, Room: newKey(t).Room()})
	f, err := readFrame(t, ws)
	if err != nil || f.Kind != relay.KindError || relay.Code(f.Payload) != relay.CodeVersionUnsupported {
		t.Fatalf("got %+v (%v), want error %s", f, err, relay.CodeVersionUnsupported)
	}
}

func keypair(t *testing.T) relay.Keypair {
	t.Helper()
	k, err := relay.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSecureChannelThroughRelay(t *testing.T) {
	h := newHarness(t, Options{})
	key := newKey(t)
	m := h.mac(key)
	as := waitOnline(t, h.app(key.Room()), true)
	s := accept(t, m)

	macKeys, appKeys := keypair(t), keypair(t)
	big := bytes.Repeat([]byte("0123456789abcdef"), 20000) // 320 KB：要分段

	done := make(chan error, 1)
	go func() {
		in, err := relay.Respond(ctx(t), s, macKeys, nil)
		if err != nil {
			done <- err
			return
		}
		if in.Peer != appKeys.Public || string(in.Payload) != "iPhone" {
			done <- errors.New("wrong peer or payload")
			return
		}
		sess, err := in.Accept(ctx(t), []byte("welcome"))
		if err != nil {
			done <- err
			return
		}
		msg, err := sess.Recv(ctx(t))
		if err == nil {
			err = sess.Send(ctx(t), msg)
		}
		done <- err
	}()

	sess, reply, err := relay.Initiate(ctx(t), as, relay.Initiator{Static: appKeys, Mac: macKeys.Public, Payload: []byte("iPhone")})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	if string(reply) != "welcome" {
		t.Fatalf("reply = %q", reply)
	}
	if err := sess.Send(ctx(t), big); err != nil {
		t.Fatal(err)
	}
	echo, err := sess.Recv(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(echo, big) {
		t.Fatal("echo differs")
	}
	if err := <-done; err != nil {
		t.Fatalf("mac side: %v", err)
	}
}
