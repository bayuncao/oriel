package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"

	"github.com/flynn/noise"
)

// App 與 Mac 之間的端到端加密（ADR-37）：Noise，X25519 + ChaChaPoly +
// BLAKE2s。之後的連線用 Noise_IK（App 已知 Mac 的公鑰）；配對用
// Noise_IKpsk1，psk 是二維碼裡的一次性密鑰（D145）。psk 放在第一則訊息
// （psk1，不是 psk2），Mac 讀第一則訊息時就能驗出密鑰對不對、是哪一把。
// 中繼只看到密文。
//
// 握手的第一則訊息以一個位元組的模式開頭（modeIK / modePairing），模式
// 也寫進 prologue，改動它握手就失敗。

var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

const (
	modeIK      byte = 1
	modePairing byte = 2
)

const prologue = "orielhub-remote/1"

// MaxMessage 是 Session 上一則訊息的上限；超過 MaxPayload 的訊息分段送。
const MaxMessage = 16 << 20

// chunkLen 是一段明文的長度：一則 Noise 訊息扣掉驗證碼與一個位元組的
// 「還有下一段」旗標。
const chunkLen = MaxPayload - 16 - 1

var (
	// ErrHandshake 是握手失敗：對方不是預期的 Mac，或配對密鑰不對、已過
	// 期（PA07）。不細分原因，免得洩露。
	ErrHandshake = errors.New("relay: handshake failed")
	// ErrCorrupt 是一則訊息解不開：被改動、重放或順序錯了（CN06）。
	// 之後這個 Session 不能再用，呼叫者斷開連線。
	ErrCorrupt = errors.New("relay: message failed authentication")
)

// PublicKey 是一方的 Noise 靜態公鑰；手機的公鑰就是設備身份（D145）。
type PublicKey [32]byte

func (k PublicKey) String() string { return b64.EncodeToString(k[:]) }

func (k PublicKey) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

func (k *PublicKey) UnmarshalText(b []byte) error { return decodeFixed(k[:], b, "public key") }

// Keypair 是一方的 Noise 靜態密鑰對。Private 只放鑰匙串，不寫日誌。
type Keypair struct {
	Private [32]byte
	Public  PublicKey
}

// GenerateKeypair 產生新的靜態密鑰對。
func GenerateKeypair() (Keypair, error) {
	k, err := suite.GenerateKeypair(rand.Reader)
	if err != nil {
		return Keypair{}, err
	}
	var kp Keypair
	copy(kp.Private[:], k.Private)
	copy(kp.Public[:], k.Public)
	return kp, nil
}

// PairingKey 是配對二維碼裡的一次性密鑰（Noise_IKpsk1 的 psk）。
type PairingKey [32]byte

// NewPairingKey 產生一次性密鑰。
func NewPairingKey() (PairingKey, error) {
	var k PairingKey
	_, err := rand.Read(k[:])
	return k, err
}

func (k PairingKey) String() string { return b64.EncodeToString(k[:]) }

func (k PairingKey) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

func (k *PairingKey) UnmarshalText(b []byte) error { return decodeFixed(k[:], b, "pairing key") }

func handshakeState(mode byte, initiator bool, static Keypair, peer *PublicKey, psk *PairingKey) (*noise.HandshakeState, error) {
	c := noise.Config{
		CipherSuite:   suite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeIK,
		Initiator:     initiator,
		StaticKeypair: noise.DHKey{Private: static.Private[:], Public: static.Public[:]},
		Prologue:      append([]byte(prologue), mode),
	}
	if peer != nil {
		c.PeerStatic = peer[:]
	}
	if psk != nil {
		c.PresharedKey = psk[:]
		c.PresharedKeyPlacement = 1
	}
	return noise.NewHandshakeState(c)
}

// Initiator 是 App 一側的握手設定。
type Initiator struct {
	Static Keypair
	// Mac 是二維碼裡的 Mac 公鑰。
	Mac PublicKey
	// Pairing 不為 nil 時以 Noise_IKpsk1 配對。
	Pairing *PairingKey
	// Payload 隨第一則訊息加密送出（例如設備名）。
	Payload []byte
}

// Initiate 在 c 上以 App 的身份握手，回傳 Session 與 Mac 在第二則訊息裡
// 的回覆。Mac 拒絕（以 ErrRefused 斷開）或回覆解不開時回 ErrHandshake；
// 傳輸的錯誤（斷線、Mac 換了連線）原樣回傳，呼叫者可以重試。
func Initiate(ctx context.Context, c Conn, in Initiator) (*Session, []byte, error) {
	mode := modeIK
	if in.Pairing != nil {
		mode = modePairing
	}
	hs, err := handshakeState(mode, true, in.Static, &in.Mac, in.Pairing)
	if err != nil {
		return nil, nil, err
	}
	msg, _, _, err := hs.WriteMessage([]byte{mode}, in.Payload)
	if err != nil {
		return nil, nil, err
	}
	if len(msg) > MaxPayload {
		return nil, nil, ErrTooLarge
	}
	if err := c.Send(ctx, msg); err != nil {
		return nil, nil, err
	}
	reply, err := c.Recv(ctx)
	if errors.Is(err, ErrRefused) {
		return nil, nil, ErrHandshake
	}
	if err != nil {
		return nil, nil, err
	}
	payload, send, recv, err := hs.ReadMessage(nil, reply)
	if err != nil || send == nil {
		return nil, nil, ErrHandshake
	}
	return &Session{c: c, peer: in.Mac, send: send, recv: recv}, payload, nil
}

// Incoming 是 Mac 讀到、已解開的第一則握手訊息。呼叫者據 Peer 決定接受
// 與否，再以 Accept 回覆。注意第一則訊息可以被重放：在 Session 上收到
// App 的第一則訊息之前，不能認定對方真的在線。
type Incoming struct {
	c  Conn
	hs *noise.HandshakeState
	// Peer 是 App 的靜態公鑰。
	Peer PublicKey
	// Payload 是 App 隨第一則訊息送來的內容。
	Payload []byte
	// Pairing 是配中的配對密鑰在 Respond 的 pairing 裡的索引；不是配對
	// 時為 -1。
	Pairing int
}

// Respond 在 c 上以 Mac 的身份讀第一則握手訊息。配對時依序以 pairing 裡
// 的每把密鑰嘗試（讓呼叫者分辨有效、過期與已用過的二維碼）；都解不開
// 回 ErrHandshake。
func Respond(ctx context.Context, c Conn, static Keypair, pairing []PairingKey) (*Incoming, error) {
	msg, err := c.Recv(ctx)
	if err != nil {
		return nil, err
	}
	if len(msg) < 1 {
		return nil, ErrHandshake
	}
	mode, body := msg[0], msg[1:]
	in := &Incoming{c: c, Pairing: -1}
	switch mode {
	case modeIK:
		in.hs, in.Payload, err = readFirst(modeIK, static, nil, body)
	case modePairing:
		err = ErrHandshake
		for i := range pairing {
			if in.hs, in.Payload, err = readFirst(modePairing, static, &pairing[i], body); err == nil {
				in.Pairing = i
				break
			}
		}
	default:
		err = ErrHandshake
	}
	if err != nil {
		return nil, ErrHandshake
	}
	copy(in.Peer[:], in.hs.PeerStatic())
	return in, nil
}

func readFirst(mode byte, static Keypair, psk *PairingKey, body []byte) (*noise.HandshakeState, []byte, error) {
	hs, err := handshakeState(mode, false, static, nil, psk)
	if err != nil {
		return nil, nil, err
	}
	payload, _, _, err := hs.ReadMessage(nil, body)
	return hs, payload, err
}

// Accept 送出第二則握手訊息（帶 payload，例如拒絕的代碼），完成握手。
func (in *Incoming) Accept(ctx context.Context, payload []byte) (*Session, error) {
	msg, recv, send, err := in.hs.WriteMessage(nil, payload)
	if err != nil {
		return nil, err
	}
	if len(msg) > MaxPayload {
		return nil, ErrTooLarge
	}
	if err := in.c.Send(ctx, msg); err != nil {
		return nil, err
	}
	return &Session{c: in.c, peer: in.Peer, send: send, recv: recv}, nil
}

// Session 是握手後的加密通道。Send 可以並行；Recv 一次一個呼叫者。
// 任何一次失敗之後 Session 都不能再用（broken）。
type Session struct {
	c    Conn
	peer PublicKey

	sendMu     sync.Mutex
	send       *noise.CipherState
	sendBroken error
	plain, ct  []byte // 送出時重用的緩衝；Conn.Send 返回後不再引用

	recvMu     sync.Mutex
	recv       *noise.CipherState
	recvBroken error
}

// Peer 是對方的靜態公鑰。
func (s *Session) Peer() PublicKey { return s.peer }

// Send 加密並送出一則訊息，超過一則 Noise 訊息的部分分段送。
func (s *Session) Send(ctx context.Context, msg []byte) error {
	if len(msg) > MaxMessage {
		return ErrTooLarge
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendBroken != nil {
		return s.sendBroken
	}
	for {
		n := min(len(msg), chunkLen)
		more := byte(0)
		if n < len(msg) {
			more = 1
		}
		s.plain = append(append(s.plain[:0], more), msg[:n]...)
		var err error
		s.ct, err = s.send.Encrypt(s.ct[:0], nil, s.plain)
		if err == nil {
			err = s.c.Send(ctx, s.ct)
		}
		if err != nil {
			s.sendBroken = err
			return err
		}
		msg = msg[n:]
		if more == 0 {
			return nil
		}
	}
}

// Recv 收一則訊息並解密。訊息被改動、重放或亂序時回 ErrCorrupt。
func (s *Session) Recv(ctx context.Context) ([]byte, error) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	if s.recvBroken != nil {
		return nil, s.recvBroken
	}
	var buf []byte
	partial := false
	for {
		ct, err := s.c.Recv(ctx)
		if err != nil {
			if partial { // 收到一半：之後的分段對不上了
				s.recvBroken = err
			}
			return nil, err
		}
		plain, err := s.recv.Decrypt(nil, nil, ct)
		if err != nil || len(plain) < 1 || plain[0] > 1 || len(buf)+len(plain)-1 > MaxMessage {
			s.recvBroken = ErrCorrupt
			return nil, ErrCorrupt
		}
		if plain[0] == 0 && !partial {
			return plain[1:], nil // 只有一段：不必再複製
		}
		buf = append(buf, plain[1:]...)
		if plain[0] == 0 {
			return buf, nil
		}
		partial = true
	}
}
