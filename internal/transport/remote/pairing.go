// Package remote 是遠程操控的入站適配器（ADR-37、D145）：手機經中繼（relay）
// 連到 Mac，Noise 握手之後以 hub 的 Principal{Device} 呼叫 hub：配對碼（一次性密鑰）
// 與配對握手（本檔）、RPC 方法與推送訂閱（rpc.go）、Mac 一側的服務（server.go），
// 以及 設置 › 遠程 背後的控制器（control.go）。
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/relay"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
)

// PairingTTL 是一個配對碼的有效時間（D145）。
const PairingTTL = 5 * time.Minute

// keepCodes 是記住的配對碼數：用過、過期或被取代的碼也留著，App 拿它來時
// 才能回確切的原因（PA04、PA05），而不是一律「握手失敗」。
const keepCodes = 8

// Offer 是桌面顯示的配對碼：二維碼裡的一次性密鑰與它的期限。
type Offer struct {
	Key     relay.PairingKey
	Expires time.Time
}

// Pairing 管理配對碼（PA02、PA04–PA07）：同一時間最多一個有效的碼，桌面每點
// 一次「添加設備」產生新的，舊的作廢；5 分鐘有效，用一次即作廢。只在記憶體，
// 重啟後全都作廢。可以並行使用。
type Pairing struct {
	now func() time.Time

	mu    sync.Mutex
	codes []*pairingCode // 新的在前，最多 keepCodes 個
}

type pairingCode struct {
	key     relay.PairingKey
	expires time.Time
	state   codeState
}

type codeState int

const (
	codeOpen codeState = iota // 有效（仍要看期限）
	codeUsed                  // 已有一台 App 用它配對（或正在配對）
	codeVoid                  // 被新的碼取代，或桌面關掉了二維碼
)

// NewPairing 建立配對碼的管理；now 為 nil 時用 time.Now。
func NewPairing(now func() time.Time) *Pairing {
	if now == nil {
		now = time.Now
	}
	return &Pairing{now: now}
}

// Offer 產生新的配對碼，之前還有效的碼作廢（PA02、PA06）。
func (p *Pairing) Offer() (Offer, error) {
	key, err := relay.NewPairingKey()
	if err != nil {
		return Offer{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.voidOpen()
	c := &pairingCode{key: key, expires: p.now().Add(PairingTTL)}
	p.codes = append([]*pairingCode{c}, p.codes[:min(len(p.codes), keepCodes-1)]...)
	return Offer{Key: c.key, Expires: c.expires}, nil
}

// Cancel 作廢還有效的碼：桌面關掉了二維碼。
func (p *Pairing) Cancel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.voidOpen()
}

func (p *Pairing) voidOpen() {
	for _, c := range p.codes {
		if c.state == codeOpen {
			c.state = codeVoid
		}
	}
}

// Keys 是記住的所有配對碼的密鑰，交給 relay.Respond 嘗試；Respond 回的索引
// 連同同一份 Keys 交給 Pair。
func (p *Pairing) Keys() []relay.PairingKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]relay.PairingKey, len(p.codes))
	for i, c := range p.codes {
		keys[i] = c.key
	}
	return keys
}

// claim 把有效的碼標為已用；用過的是 remote.pairing_used，過期、被取代或已
// 忘掉的是 remote.pairing_expired。
func (p *Pairing) claim(key relay.PairingKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := slices.IndexFunc(p.codes, func(c *pairingCode) bool { return c.key == key })
	if i < 0 {
		return errPairingExpired
	}
	c := p.codes[i]
	switch {
	case c.state == codeUsed:
		return errPairingUsed
	case c.state == codeVoid || !p.now().Before(c.expires):
		return errPairingExpired
	}
	c.state = codeUsed
	return nil
}

// unclaim 在登記失敗時把碼還回去（例如名字不合規），App 可以改了再試。
func (p *Pairing) unclaim(key relay.PairingKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.codes {
		if c.key == key && c.state == codeUsed {
			c.state = codeOpen
		}
	}
}

// PairRequest 是 App 隨配對握手的第一則訊息送出的內容（JSON）。
type PairRequest struct {
	Name string `json:"name"`
}

// HandshakeReply 是 Mac 在第二則握手訊息裡的回覆（JSON），配對與之後的連線
// 都是它：成功時有 Device；失敗時有 Error（代碼與參數，同 transport.Wire），
// 之後 Mac 斷開這台 App。
type HandshakeReply struct {
	Device task.DeviceID    `json:"device,omitzero"`
	Error  *transport.Error `json:"error,omitzero"`
}

// errNotPairing 是把不是配對的握手交給了 Pair（呼叫者的錯）。
var errNotPairing = errors.New("remote: not a pairing handshake")

// Pair 完成 Mac 一側的配對（PA03）：in 是 relay.Respond 以 keys（同一次 Keys
// 的結果）解開的配對握手。驗過配對碼、以 LocalOwner 呼叫 hub.PairDevice 登記
// 這台手機，再把 DeviceID 回給它。失敗時把原因回給 App 並回傳它，呼叫者隨即
// 斷開這條連線；配對碼不對（PA07）在 Respond 那一步就失敗，不會到這裡。
func (p *Pairing) Pair(ctx context.Context, h *hub.Hub, keys []relay.PairingKey, in *relay.Incoming) (hub.Device, *relay.Session, error) {
	if in.Pairing < 0 || in.Pairing >= len(keys) {
		return hub.Device{}, nil, errNotPairing
	}
	var req PairRequest
	_ = json.Unmarshal(in.Payload, &req) // 解不開就是沒有名字，由 hub 回 invalid_field
	key := keys[in.Pairing]
	if err := p.claim(key); err != nil {
		return hub.Device{}, nil, refuse(ctx, in, err)
	}
	d, err := h.PairDevice(ctx, hub.LocalOwner, hub.DeviceKey(in.Peer), req.Name)
	if err != nil {
		p.unclaim(key)
		return hub.Device{}, nil, refuse(ctx, in, err)
	}
	reply, err := json.Marshal(HandshakeReply{Device: d.ID})
	if err != nil {
		return hub.Device{}, nil, err
	}
	sess, err := in.Accept(ctx, reply)
	if err != nil {
		return hub.Device{}, nil, err
	}
	return d, sess, nil
}

// refuse 把 cause 的代碼回給 App，回傳 cause。回覆送不出去也一樣：呼叫者隨即
// 斷開，App 看到斷線。
func refuse(ctx context.Context, in *relay.Incoming, cause error) error {
	w, _ := transport.Wire(cause)
	if reply, err := json.Marshal(HandshakeReply{Error: w}); err == nil {
		_, _ = in.Accept(ctx, reply)
	}
	return cause
}

// PairWith 是 App 一側的配對（Go 的參考實作，iOS App 照做；也是測試裡的
// App）：以二維碼裡的 Mac 公鑰與一次性密鑰做 Noise_IKpsk1 握手並報上名字，
// 回 Mac 給的 DeviceID 與加密通道。Mac 拒絕時回 *transport.Error（如
// remote.pairing_expired）；一次性密鑰不對（PA07）時回 relay.ErrHandshake，
// 不細分原因；斷線等傳輸錯誤原樣回傳。
func PairWith(ctx context.Context, c relay.Conn, static relay.Keypair, mac relay.PublicKey, key relay.PairingKey, name string) (task.DeviceID, *relay.Session, error) {
	req, err := json.Marshal(PairRequest{Name: name})
	if err != nil {
		return "", nil, err
	}
	sess, b, err := relay.Initiate(ctx, c, relay.Initiator{Static: static, Mac: mac, Pairing: &key, Payload: req})
	if err != nil {
		return "", nil, err
	}
	var reply HandshakeReply
	if err := json.Unmarshal(b, &reply); err != nil {
		return "", nil, errors.New("remote: malformed pairing reply")
	}
	if reply.Error != nil {
		return "", nil, reply.Error
	}
	if reply.Device == "" {
		return "", nil, errors.New("remote: pairing reply without a device")
	}
	return reply.Device, sess, nil
}
