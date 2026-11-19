package relay

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// pipe 是記憶體裡的一對 Conn（真的傳輸，不是替身）：一端 Send 的訊息原樣
// 出現在另一端的 Recv。mangle 不為 nil 時先改動要送出的訊息，模擬中繼
// 竄改、重放或亂序（CN06）。對端 refuse 之後 Recv 回 closed（預設是
// ErrRefused，即 Mac 拒絕；改成別的錯就是傳輸斷了）。
type pipeEnd struct {
	in     <-chan []byte
	out    chan<- []byte
	closed error

	mu     sync.Mutex
	mangle func([]byte) [][]byte
}

func pipe() (*pipeEnd, *pipeEnd) {
	a, b := make(chan []byte, 64), make(chan []byte, 64)
	return &pipeEnd{in: a, out: b, closed: ErrRefused}, &pipeEnd{in: b, out: a, closed: ErrRefused}
}

func (p *pipeEnd) setMangle(f func([]byte) [][]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mangle = f
}

func (p *pipeEnd) Send(ctx context.Context, msg []byte) error {
	msgs := [][]byte{bytes.Clone(msg)}
	p.mu.Lock()
	if p.mangle != nil {
		msgs = p.mangle(msgs[0])
	}
	p.mu.Unlock()
	for _, m := range msgs {
		select {
		case p.out <- m:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (p *pipeEnd) Recv(ctx context.Context) ([]byte, error) {
	select {
	case m, ok := <-p.in:
		if !ok {
			return nil, p.closed
		}
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *pipeEnd) refuse() { close(p.out) }

func testCtx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func keypair(t *testing.T) Keypair {
	t.Helper()
	k, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pairingKey(t *testing.T) PairingKey {
	t.Helper()
	k, err := NewPairingKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type macResult struct {
	in   *Incoming
	sess *Session
	err  error
}

// macSide 在背景以 Mac 的身份回應握手；失敗時像 transport/remote 一樣斷開
// （經中繼時 App 收到 ErrRefused）。
func macSide(t *testing.T, c *pipeEnd, static Keypair, pairing []PairingKey) <-chan macResult {
	out := make(chan macResult, 1)
	go func() {
		in, err := Respond(testCtx(t), c, static, pairing)
		if err != nil {
			c.refuse()
			out <- macResult{err: err}
			return
		}
		sess, err := in.Accept(testCtx(t), []byte("ok"))
		out <- macResult{in: in, sess: sess, err: err}
	}()
	return out
}

func TestHandshake(t *testing.T) {
	mac, app := keypair(t), keypair(t)
	current, expired, used, forged := pairingKey(t), pairingKey(t), pairingKey(t), pairingKey(t)
	wrongMac := keypair(t)
	shown := []PairingKey{current, expired, used}

	cases := []struct {
		name    string
		target  PublicKey   // App 以為的 Mac 公鑰
		pairing *PairingKey // App 用的一次性密鑰
		offered []PairingKey
		want    int  // Incoming.Pairing
		fails   bool // 握手失敗（ErrHandshake）
	}{
		{name: "ik", target: mac.Public, want: -1},
		{name: "pairing with the current key", target: mac.Public, pairing: &current, offered: shown, want: 0},
		{name: "pairing with an expired key", target: mac.Public, pairing: &expired, offered: shown, want: 1},
		{name: "pairing with a used key", target: mac.Public, pairing: &used, offered: shown, want: 2},
		{name: "forged pairing key (PA07)", target: mac.Public, pairing: &forged, offered: shown, fails: true},
		{name: "pairing when no code is shown", target: mac.Public, pairing: &current, fails: true},
		{name: "wrong Mac key", target: wrongMac.Public, fails: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, m := pipe()
			res := macSide(t, m, mac, tc.offered)
			sess, reply, err := Initiate(testCtx(t), a, Initiator{Static: app, Mac: tc.target, Pairing: tc.pairing, Payload: []byte("iPhone")})
			r := <-res
			if tc.fails {
				if !errors.Is(r.err, ErrHandshake) || !errors.Is(err, ErrHandshake) {
					t.Fatalf("Respond err = %v, Initiate err = %v; want ErrHandshake on both", r.err, err)
				}
				return
			}
			if err != nil || r.err != nil {
				t.Fatalf("Initiate: %v; Respond: %v", err, r.err)
			}
			if r.in.Pairing != tc.want || r.in.Peer != app.Public || string(r.in.Payload) != "iPhone" || string(reply) != "ok" {
				t.Fatalf("got pairing %d peer %v payload %q reply %q", r.in.Pairing, r.in.Peer, r.in.Payload, reply)
			}
			if sess.Peer() != mac.Public || r.sess.Peer() != app.Public {
				t.Fatal("session peers wrong")
			}
			exchange(t, sess, r.sess, []byte("hello"))
			exchange(t, r.sess, sess, []byte("hi back"))
		})
	}
}

func TestInitiateKeepsTransportErrors(t *testing.T) {
	// 握手途中斷線或 Mac 換了連線不是配對碼錯：原樣回傳，App 可以重試。
	mac := keypair(t)
	a, m := pipe()
	a.closed = ErrClosed
	go func() {
		if _, err := m.Recv(testCtx(t)); err == nil {
			m.refuse()
		}
	}()
	_, _, err := Initiate(testCtx(t), a, Initiator{Static: keypair(t), Mac: mac.Public, Pairing: new(pairingKey(t))})
	if !errors.Is(err, ErrClosed) || errors.Is(err, ErrHandshake) {
		t.Fatalf("Initiate = %v, want ErrClosed", err)
	}
}

func exchange(t *testing.T, from, to *Session, msg []byte) {
	t.Helper()
	if err := from.Send(testCtx(t), msg); err != nil {
		t.Fatal(err)
	}
	got, err := to.Recv(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %d bytes, want %d", len(got), len(msg))
	}
}

// connected 回傳握手完成的一對 Session 與 App 那端的管子。
func connected(t *testing.T) (app, mac *Session, appEnd *pipeEnd) {
	t.Helper()
	macKeys := keypair(t)
	a, m := pipe()
	res := macSide(t, m, macKeys, nil)
	app, _, err := Initiate(testCtx(t), a, Initiator{Static: keypair(t), Mac: macKeys.Public})
	if err != nil {
		t.Fatal(err)
	}
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	return app, r.sess, a
}

func TestSessionMessageSizes(t *testing.T) {
	app, mac, _ := connected(t)
	for _, n := range []int{0, 1, chunkLen - 1, chunkLen, chunkLen + 1, 3*chunkLen + 7} {
		exchange(t, app, mac, bytes.Repeat([]byte{byte(n)}, n))
	}
	if err := app.Send(testCtx(t), make([]byte, MaxMessage+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized Send: %v", err)
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	cases := []struct {
		name     string
		mangle   func([]byte) [][]byte
		original bool // 原訊息照常送達，之後的才是壞的
	}{
		{name: "flipped bit", mangle: func(m []byte) [][]byte { m[len(m)-1] ^= 1; return [][]byte{m} }},
		{name: "truncated", mangle: func(m []byte) [][]byte { return [][]byte{m[:len(m)-1]} }},
		{name: "replayed", mangle: func(m []byte) [][]byte { return [][]byte{m, bytes.Clone(m)} }, original: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, mac, a := connected(t)
			a.setMangle(tc.mangle)
			if err := app.Send(testCtx(t), []byte("one")); err != nil {
				t.Fatal(err)
			}
			if tc.original {
				if _, err := mac.Recv(testCtx(t)); err != nil {
					t.Fatalf("original message: %v", err)
				}
			}
			if _, err := mac.Recv(testCtx(t)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Recv = %v, want ErrCorrupt", err)
			}
			if _, err := mac.Recv(testCtx(t)); !errors.Is(err, ErrCorrupt) {
				t.Fatal("session usable after corruption")
			}
		})
	}
}

func TestSessionRejectsReordering(t *testing.T) {
	app, mac, a := connected(t)
	var held []byte
	a.setMangle(func(m []byte) [][]byte {
		if held == nil {
			held = m
			return nil
		}
		return [][]byte{m, held}
	})
	for _, msg := range []string{"first", "second"} {
		if err := app.Send(testCtx(t), []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mac.Recv(testCtx(t)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Recv = %v, want ErrCorrupt", err)
	}
}

func TestPrologueBindsMode(t *testing.T) {
	// 把 IK 的第一則訊息改標成配對模式：Mac 解不開。
	macKeys := keypair(t)
	a, m := pipe()
	a.setMangle(func(msg []byte) [][]byte { msg[0] = modePairing; return [][]byte{msg} })
	res := macSide(t, m, macKeys, []PairingKey{pairingKey(t)})
	if _, _, err := Initiate(testCtx(t), a, Initiator{Static: keypair(t), Mac: macKeys.Public}); !errors.Is(err, ErrHandshake) {
		t.Fatalf("Initiate: %v", err)
	}
	if r := <-res; !errors.Is(r.err, ErrHandshake) {
		t.Fatalf("Respond: %v", r.err)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	f := Frame{Kind: KindData, Conn: 0xdeadbeef, Payload: []byte("x")}
	got, err := ParseFrame(f.Marshal())
	if err != nil || got.Kind != f.Kind || got.Conn != f.Conn || string(got.Payload) != "x" {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, bad := range [][]byte{nil, {byte(KindData), 0, 0, 0}, {0, 0, 0, 0, 0}, {byte(KindError) + 1, 0, 0, 0, 0}, make([]byte, MaxFrame+1)} {
		if _, err := ParseFrame(bad); !errors.Is(err, ErrBadFrame) {
			t.Errorf("ParseFrame(%d bytes) = %v", len(bad), err)
		}
	}
}

func TestRoomTextRoundTrip(t *testing.T) {
	key, err := NewRoomKey()
	if err != nil {
		t.Fatal(err)
	}
	id := key.Room()
	gotID, err := ParseRoomID(id.String())
	if err != nil || gotID != id {
		t.Fatalf("room id: %v", err)
	}
	gotKey, err := ParseRoomKey(key.String())
	if err != nil || gotKey != key {
		t.Fatalf("room key: %v", err)
	}
	if _, err := ParseRoomKey(id.String()); err == nil {
		t.Fatal("a room id parsed as a room key")
	}
	if other, _ := NewRoomKey(); other.Room() == id {
		t.Fatal("two keys share a room")
	}
}
