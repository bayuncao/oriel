package remote

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/relay"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/internal/usage"
)

// defaultHandshakeTimeout 是一條 App 連線完成握手的期限，也是握手之後送來第一則訊息的期限。
const defaultHandshakeTimeout = 10 * time.Second

// Options 是 Server 的設定。
type Options struct {
	Hub *hub.Hub
	// Agents 是 Agents.List 的 agent 目錄；必填。
	Agents AgentSource
	// Usage 是 Usage.* 的用量；必填。
	Usage *usage.Tracker
	// Static 是 Mac 的 Noise 靜態密鑰對（D145；app 從鑰匙串取來）。
	Static relay.Keypair
	// Pairing 是桌面顯示的配對碼；nil 時不接受配對。
	Pairing *Pairing
	// Logger 為 nil 時用 slog.Default()。日誌只寫設備 ID 與方法名，不寫密鑰與內容（CN08）。
	Logger *slog.Logger
	// HandshakeTimeout 是完成握手、與握手之後送來第一則訊息各自的期限；為 0 時用 10 秒。
	HandshakeTimeout time.Duration
	// OnlineChanged 不為 nil 時，在線的設備可能變了就呼叫（設備連入、斷開；DV14）；
	// 不持鎖呼叫，不能阻塞。
	OnlineChanged func()
}

// Server 是 Mac 一側的遠程服務：在中繼上接受 App 的連線，握手、認出設備，
// 再以 Principal{Device} 把白名單裡的方法轉給 hub。
type Server struct {
	hub      *hub.Hub
	agents   AgentSource
	usage    *usage.Tracker
	static   relay.Keypair
	pairing  *Pairing
	log      *slog.Logger
	shakeFor time.Duration
	changed  func()

	mu    sync.Mutex
	conns map[task.DeviceID]*conn // 確認了的連線：每台設備最多一條（RC06、CN12）
	// pending 是握手完成、還沒收到第一則訊息的連線（CN10–CN14）。IK 握手的第一則
	// 訊息可以被重放，Mac 照樣回第二則；只有收到解得開的訊息，才知道對方真的持有
	// 手機的私鑰。在那之前它不算在線、不替換舊連線、不記最後在線，hub 收不到呼叫。
	pending map[*conn]struct{}

	// left 不為 nil 時，每條設備連線清理完畢後呼叫；測試靠它等非同步的清理。
	left func(task.DeviceID)
}

// NewServer 建立遠程服務。
func NewServer(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.HandshakeTimeout == 0 {
		o.HandshakeTimeout = defaultHandshakeTimeout
	}
	return &Server{hub: o.Hub, agents: o.Agents, usage: o.Usage, static: o.Static, pairing: o.Pairing, log: o.Logger,
		shakeFor: o.HandshakeTimeout, changed: o.OnlineChanged, conns: map[task.DeviceID]*conn{},
		pending: map[*conn]struct{}{}}
}

// Online 回目前連著的設備，排序過。
func (s *Server) Online() []task.DeviceID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.conns))
}

func (s *Server) onlineChanged() {
	if s.changed != nil {
		s.changed()
	}
}

// Serve 在一條到中繼的連線上服務 App，直到 ctx 結束、與中繼的連線斷了或
// hub 關閉；返回時所有 App 連線都已斷開、所有 goroutine 都已結束。ctx 結束
// 時回 nil，否則回斷線的原因（呼叫者按退避重連，RC01）。
func (s *Server) Serve(ctx context.Context, m *relay.Mac) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	var watchErr error
	wg.Go(func() {
		// 撤銷的設備立即斷開（CN04、CN05）。hub 關閉時整個服務結束。
		err := s.hub.WatchDevices(ctx, hub.LocalOwner, s.keepOnly)
		if ctx.Err() == nil {
			watchErr = err
			cancel()
		}
	})
	for {
		st, err := m.Accept(ctx)
		if err != nil {
			cancel()
			wg.Wait()
			switch {
			case watchErr != nil:
				return watchErr
			case ctx.Err() != nil && m.Err() == nil:
				return nil
			}
			return m.Err()
		}
		wg.Go(func() { s.serveStream(ctx, st) })
	}
}

// keepOnly 斷開不在 devices 裡的設備的連線，確認了的與還在等第一則訊息的都是。
func (s *Server) keepOnly(devices []hub.Device) error {
	known := make(map[task.DeviceID]bool, len(devices))
	for _, d := range devices {
		known[d.ID] = true
	}
	s.mu.Lock()
	var gone []*conn
	for id, c := range s.conns {
		if !known[id] {
			gone = append(gone, c)
		}
	}
	for c := range s.pending {
		if !known[c.device] {
			gone = append(gone, c)
		}
	}
	// 持鎖時就取消：confirm 在同一把鎖下看得到，不會登記正在被斷開的連線。
	for _, c := range gone {
		c.cancel()
	}
	s.mu.Unlock()
	for _, c := range gone {
		s.log.Info("remote: device revoked", "device", c.device)
		c.close()
	}
	return nil
}

// serveStream 處理一條 App 連線：握手，認出設備，之後服務它的請求。
func (s *Server) serveStream(ctx context.Context, st *relay.Stream) {
	d, sess, err := s.handshake(ctx, st)
	if err != nil {
		st.Close() // App 收到 relay.ErrRefused；握手時的拒絕原因已在回覆裡
		if !errors.Is(err, relay.ErrHandshake) && ctx.Err() == nil {
			s.log.Info("remote: refused", "reason", err.Error())
		}
		return
	}
	c := newConn(ctx, s, d, st, sess)
	s.mu.Lock()
	s.pending[c] = struct{}{}
	s.mu.Unlock()
	defer s.remove(c)
	c.serve()
}

// handshake 回認出的設備與加密通道。配對握手經 Pairing 登記設備（PA03）；
// 其餘按公鑰查 hub，沒登記或已撤銷回 remote.unknown_device（CN02、CN03）。
// 配對碼在連線進來時取：房間寫在二維碼裡，App 掃碼之後才連得進來，所以
// 它要用的碼此時已經在顯示。
func (s *Server) handshake(ctx context.Context, st *relay.Stream) (hub.Device, *relay.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, s.shakeFor)
	defer cancel()
	var keys []relay.PairingKey
	if s.pairing != nil {
		keys = s.pairing.Keys()
	}
	in, err := relay.Respond(ctx, st, s.static, keys)
	if err != nil {
		return hub.Device{}, nil, err
	}
	if in.Pairing >= 0 {
		return s.pairing.Pair(ctx, s.hub, keys, in)
	}
	d, ok, err := s.hub.DeviceByKey(ctx, hub.LocalOwner, hub.DeviceKey(in.Peer))
	if err == nil && !ok {
		err = errUnknownDevice
	}
	if err != nil {
		return hub.Device{}, nil, refuse(ctx, in, err)
	}
	reply, err := json.Marshal(HandshakeReply{Device: d.ID})
	if err != nil {
		return hub.Device{}, nil, err
	}
	sess, err := in.Accept(ctx, reply)
	return d, sess, err
}

// confirm 在 c 收到第一則解得開的訊息時登記它（CN12）：同一台設備的舊連線關掉。
// 先再查一次設備：握手之後被撤銷的不登記（CN14），回 false。之後才撤銷的，
// WatchDevices 推來時 c 在 pending 或 conns 裡，keepOnly 照樣斷開它。
func (s *Server) confirm(c *conn) bool {
	if _, ok, err := s.hub.DeviceByKey(c.ctx, hub.LocalOwner, c.key); err != nil || !ok {
		return false
	}
	s.mu.Lock()
	if _, ok := s.pending[c]; !ok || c.ctx.Err() != nil { // keepOnly 或服務結束已經斷開它
		s.mu.Unlock()
		return false
	}
	delete(s.pending, c)
	old := s.conns[c.device]
	s.conns[c.device] = c
	s.mu.Unlock()
	s.onlineChanged()
	if old != nil {
		s.log.Info("remote: replaced", "device", c.device)
		old.close()
	}
	return true
}

func (s *Server) remove(c *conn) {
	s.mu.Lock()
	delete(s.pending, c)
	gone := s.conns[c.device] == c
	if gone {
		delete(s.conns, c.device)
	}
	s.mu.Unlock()
	if gone {
		s.onlineChanged()
	}
	if s.left != nil {
		s.left(c.device)
	}
}

// conn 是一台設備的一條連線：讀請求的迴圈在 serve，每個請求一個 goroutine。
type conn struct {
	s      *Server
	device task.DeviceID
	key    hub.DeviceKey
	stream *relay.Stream
	sess   *relay.Session
	call   call
	ctx    context.Context // 連線存活期間有效
	cancel context.CancelFunc
	wg     sync.WaitGroup // 請求的 goroutine

	mu   sync.Mutex
	subs map[uint64]context.CancelFunc // 進行中的訂閱
}

func newConn(ctx context.Context, s *Server, d hub.Device, st *relay.Stream, sess *relay.Session) *conn {
	ctx, cancel := context.WithCancel(ctx)
	return &conn{s: s, device: d.ID, key: d.Key, stream: st, sess: sess,
		call: call{hub: s.hub, agents: s.agents, usage: s.usage, p: hub.Principal{Device: d.ID}},
		ctx:  ctx, cancel: cancel, subs: map[uint64]context.CancelFunc{}}
}

// close 斷開這台設備：結束它的請求並請中繼斷開 App（撤銷、被新連線取代）。
func (c *conn) close() {
	c.cancel()
	c.stream.Close()
}

// serve 等第一則訊息（握手期限內），收到了才登記這條連線（CN10–CN14），之後處理請求
// 直到斷開。
func (c *conn) serve() {
	confirmed := false
	defer func() {
		c.cancel()
		c.wg.Wait()
		c.stream.Close()
		if confirmed {
			c.seen()
			c.s.log.Info("remote: disconnected", "device", c.device)
		}
	}()
	b, err := c.recv(c.s.shakeFor)
	if err != nil {
		if c.ctx.Err() == nil { // 不是撤銷或服務結束：期限到了，或 App 斷了
			c.s.log.Info("remote: no first message", "device", c.device) // CN10、CN11、CN13
		}
		return
	}
	if confirmed = c.s.confirm(c); !confirmed {
		return
	}
	c.seen()
	c.s.log.Info("remote: connected", "device", c.device)
	for {
		var req Request
		if json.Unmarshal(b, &req) != nil {
			c.s.log.Warn("remote: malformed request", "device", c.device)
			return
		}
		c.handle(req)
		if b, err = c.recv(0); err != nil {
			return
		}
	}
}

// recv 收一則訊息；limit 大於 0 時最多等這麼久。
func (c *conn) recv(limit time.Duration) ([]byte, error) {
	ctx := c.ctx
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	b, err := c.sess.Recv(ctx)
	if errors.Is(err, relay.ErrCorrupt) {
		c.s.log.Warn("remote: corrupt message", "device", c.device) // CN06
	}
	return b, err
}

// seen 記下最後在線（CN01）；設備已撤銷（unknown_device）或 hub 已關閉時
// 不必理會。
func (c *conn) seen() {
	if err := c.s.hub.DeviceSeen(context.WithoutCancel(c.ctx), hub.LocalOwner, c.device); err != nil {
		var he *hub.Error
		if !errors.As(err, &he) || (he.Code != hub.CodeUnknownDevice && he.Code != hub.CodeClosed) {
			c.s.log.Warn("remote: last seen not saved", "device", c.device, "err", err)
		}
	}
}

func (c *conn) handle(req Request) {
	if req.Cancel {
		c.mu.Lock()
		cancel := c.subs[req.ID]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	m, ok := methods[req.Method]
	if !ok {
		c.reply(req, Reply{ID: req.ID}, errNotAllowed)
		return
	}
	if m.unary != nil {
		c.wg.Go(func() {
			result, err := m.unary(c.ctx, c.call, req.Params)
			r := Reply{ID: req.ID}
			if err == nil {
				r.Result, err = json.Marshal(result)
			}
			c.reply(req, r, err)
		})
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	if _, dup := c.subs[req.ID]; dup {
		c.mu.Unlock()
		cancel()
		c.reply(req, Reply{ID: req.ID}, errBadRequest)
		return
	}
	c.subs[req.ID] = cancel
	c.mu.Unlock()
	c.wg.Go(func() {
		err := m.watch(ctx, c.call, req.Params, func(v any) error {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			return c.send(Reply{ID: req.ID, Push: b})
		})
		if ctx.Err() != nil {
			// App 取消了（連線斷了時 reply 什麼都不做）：一律以 end 結束，不論
			// 方法回什麼（hub 的訂閱回 nil，用量的 Watch 回 ctx 的錯誤）。
			err = nil
		}
		cancel()
		// 先放掉 id 再送結束的回覆：App 收到 end 或 error 就可以重用這個 id，
		// 那時它一定已經空出來（反過來，App 可能撞上還沒放掉的 id）。
		c.mu.Lock()
		delete(c.subs, req.ID)
		c.mu.Unlock()
		c.reply(req, Reply{ID: req.ID, End: err == nil}, err)
	})
}

// reply 送出 r；err 不為 nil 時換成它的代碼。沒有代碼的錯誤只把原文寫進日誌。
// 連線已斷時什麼都不做：進行中的請求因此結束，不是請求失敗。
func (c *conn) reply(req Request, r Reply, err error) {
	if c.ctx.Err() != nil {
		return
	}
	if err != nil {
		w, known := transport.Wire(err)
		if !known {
			c.s.log.Error("remote: request failed", "device", c.device, "method", req.Method, "err", err)
		}
		r = Reply{ID: r.ID, Error: w}
	}
	_ = c.send(r) // 送不出去時連線已在關閉
}

// send 送一則回覆；送不出去表示通道壞了，斷開連線。
func (c *conn) send(r Reply) error {
	b, err := json.Marshal(r)
	if err == nil {
		err = c.sess.Send(c.ctx, b)
	}
	if err != nil {
		c.cancel()
	}
	return err
}
