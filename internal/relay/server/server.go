// Package server 是遠程操控的中繼服務（ADR-37）：Mac 以房間密鑰連入，
// 房間號由密鑰算出（relay.RoomKey.Room），App 以房間號連入同一房間，中
// 繼在兩者之間按連線號轉發不透明的幀。中繼不存密鑰，只在記憶體裡記有
// 連線的房間，不解讀也不記錄負載（RL05）；重啟後什麼也不必恢復。入口是
// cmd/orielhub-relay。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/bayuncao/oriel/internal/relay"
)

const (
	defaultMaxApps       = 16
	defaultMaxConns      = 1024
	defaultMaxConnsPerIP = 32
	defaultPing          = 30 * time.Second
	pingTimeout          = 10 * time.Second
	helloTimeout         = 10 * time.Second
	// queueLen 是一條連線待寫的資料幀上限（RL15）。一幀最多
	// relay.MaxFrame（約 64 KiB），所以一條連線最多積壓約 4 MiB 的資料，全
	// 部連線最多約 MaxConns × 4 MiB。寫給 App 的幀超過它，斷開這台 App：
	// Mac 不被讀得慢的 App 拖住（RL06）；App 轉給 Mac 的幀到了它，讓送的
	// App 等（RL16）。
	queueLen = 64
	// controlLen 是 Mac 的寫入佇列另外留給控制幀（Open、Close）的位置：
	// App 轉來的資料幀佔滿 queueLen 時，加入與離開照常通知 Mac（RL18）。
	// 控制幀很小，連資料一起仍約 4 MiB。
	controlLen = 64
)

// Options 是中繼服務的設定；零值可用。
type Options struct {
	// Logger 為 nil 時用 slog.Default()。
	Logger *slog.Logger
	// MaxApps 是一個房間同時連著的 App 上限；不大於 0 時為 16。
	MaxApps int
	// MaxConns 是整個中繼同時連著的連線上限，含還沒送 Hello 的（RL08）；
	// 不大於 0 時為 1024。它也限住了房間數與記憶體（RL14、RL15）。
	MaxConns int
	// MaxConnsPerIP 是一個客戶端地址同時連著的連線上限（RL09）；IPv6 以
	// /64 為一個地址（RL10）。不大於 0 時為 32。
	MaxConnsPerIP int
	// ClientIPHeader 是反向代理寫入客戶端地址的標頭（如 X-Forwarded-For、
	// X-Real-IP），取最後一個值（RL11）；為空時用 TCP 對端地址，不理會客
	// 戶端自帶的標頭（RL12）。
	ClientIPHeader string
	// PingInterval 為 0 時用 30 秒，負數不發心跳。
	PingInterval time.Duration
}

// Server 是中繼服務，實作 http.Handler（路徑 relay.Path）。
type Server struct {
	opts   Options
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	rooms  map[relay.RoomID]*room
	conns  int                  // 連著的連線數，含還沒送 Hello 的
	perIP  map[netip.Prefix]int // 各客戶端地址的連線數；計數歸零即刪

	refusals   map[int]int // 上次記日誌之後，各 HTTP 狀態拒絕的次數
	refusedLog time.Time   // 上次記拒絕日誌的時間

	// helloTimeout 是等 Hello 的期限（RL13）；測試把它縮短。
	helloTimeout time.Duration
	// left 不為 nil 時，每條連線清理完畢後呼叫；測試靠它等非同步的清理。
	left func(relay.RoomID)
	// freed 不為 nil 時，每條連線的名額還回去之後呼叫（RL13）。
	freed func()
	// waiting 不為 nil 時，App 轉給 Mac 的幀因 Mac 的佇列滿了而要等時呼
	// 叫（RL16）；測試靠它知道佇列已滿。
	waiting func()
}

type room struct {
	id   relay.RoomID
	mac  *peer
	gen  uint32 // Mac 的世代號：每次 Mac 連上加一，跳過 0
	apps map[uint32]*peer
	next uint32
}

// idle 是房間沒有任何連線。
func (r *room) idle() bool { return r.mac == nil && len(r.apps) == 0 }

// New 建立中繼服務；Close 斷開所有連線。
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MaxApps <= 0 {
		o.MaxApps = defaultMaxApps
	}
	if o.MaxConns <= 0 {
		o.MaxConns = defaultMaxConns
	}
	if o.MaxConnsPerIP <= 0 {
		o.MaxConnsPerIP = defaultMaxConnsPerIP
	}
	if o.PingInterval == 0 {
		o.PingInterval = defaultPing
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		opts: o, log: o.Logger, ctx: ctx, cancel: cancel,
		rooms: map[relay.RoomID]*room{}, perIP: map[netip.Prefix]int{}, refusals: map[int]int{},
		helloTimeout: helloTimeout,
	}
}

// Close 斷開所有連線並等它們結束。
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	return nil
}

// client 是請求的客戶端地址，IPv4 為 /32、IPv6 為 /64（RL10）：設了
// ClientIPHeader 時取標頭的最後一個值，是自己的反向代理加上的那個
// （RL11）；否則是 TCP 對端地址（RL12）。
func (s *Server) client(r *http.Request) (netip.Prefix, error) {
	var addr netip.Addr
	if h := s.opts.ClientIPHeader; h != "" {
		vals := r.Header.Values(h)
		if len(vals) == 0 {
			return netip.Prefix{}, fmt.Errorf("no %s header", h)
		}
		last := vals[len(vals)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		a, err := netip.ParseAddr(strings.TrimSpace(last))
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%s header: %w", h, err)
		}
		addr = a
	} else {
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("remote address: %w", err)
		}
		addr = ap.Addr()
	}
	addr = addr.Unmap().WithZone("")
	bits := 64
	if addr.Is4() {
		bits = 32
	}
	return addr.Prefix(bits)
}

// admit 為一條新連線佔下名額；回 0 表示收下，否則是拒絕的 HTTP 狀態：
// 全局滿了 503（RL08），這個地址滿了 429（RL09），中繼已關 503。收下的
// 連線結束時要呼叫 free。
func (s *Server) admit(client netip.Prefix) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed, s.conns >= s.opts.MaxConns:
		return http.StatusServiceUnavailable
	case s.perIP[client] >= s.opts.MaxConnsPerIP:
		return http.StatusTooManyRequests
	}
	s.conns++
	s.perIP[client]++
	s.wg.Add(1)
	return 0
}

// refuse 以 status 拒絕還沒升級的請求。日誌每分鐘至多一行，帶這段時間
// 各狀態的次數：被灌連線時日誌不跟著灌爆磁碟；不記客戶端地址（RL05）。
// cause 是這次 400 的原因（反向代理沒寫入客戶端地址，RL11），輪到記日誌時一併記下。
func (s *Server) refuse(w http.ResponseWriter, status int, cause error) {
	http.Error(w, http.StatusText(status), status)
	s.mu.Lock()
	s.refusals[status]++
	if now := time.Now(); now.Sub(s.refusedLog) >= time.Minute {
		counts := s.refusals
		s.refusals, s.refusedLog = map[int]int{}, now
		s.mu.Unlock()
		attrs := []any{"service_unavailable", counts[http.StatusServiceUnavailable],
			"too_many_requests", counts[http.StatusTooManyRequests], "bad_request", counts[http.StatusBadRequest]}
		if cause != nil {
			attrs = append(attrs, "err", cause)
		}
		s.log.Warn("relay: refused connections", attrs...)
		return
	}
	s.mu.Unlock()
}

// free 還回 admit 佔下的名額。
func (s *Server) free(client netip.Prefix) {
	s.mu.Lock()
	s.conns--
	if s.perIP[client]--; s.perIP[client] == 0 {
		delete(s.perIP, client)
	}
	s.mu.Unlock()
	s.wg.Done()
	if s.freed != nil {
		s.freed()
	}
}

// lookup 取得或建立房間。持有 s.mu。
func (s *Server) lookup(id relay.RoomID) *room {
	r := s.rooms[id]
	if r == nil {
		r = &room{id: id, apps: map[uint32]*peer{}}
		s.rooms[id] = r
	}
	return r
}

// release 在房間沒有連線時忘掉它。持有 s.mu。
func (s *Server) release(r *room) {
	if r.idle() && s.rooms[r.id] == r {
		delete(s.rooms, r.id)
	}
}

// peer 是一條客戶端連線：讀在 ServeHTTP 的 goroutine，寫在 writeLoop。
type peer struct {
	ws     *websocket.Conn
	out    chan outbound
	ctx    context.Context
	cancel context.CancelFunc
	// credit 是 Mac 的佇列裡 App 轉來的資料幀的名額，寫出一則還一個
	// （RL16）；App 的 peer 沒有它。
	credit chan struct{}
}

// outbound 是寫入佇列裡的一則訊息。b 為 nil 表示寫完前面的之後斷開；
// credit 表示它佔著一個 credit 名額。
type outbound struct {
	b      []byte
	credit bool
}

// queue 依角色建立寫入佇列：Mac 的佇列讓轉來的資料幀等名額，並給控制
// 幀留位置（RL16、RL18）；App 的佇列滿了就斷開它（RL06）。
func (p *peer) queue(role relay.Role) {
	if role == relay.RoleMac {
		p.out = make(chan outbound, queueLen+controlLen)
		p.credit = make(chan struct{}, queueLen)
		return
	}
	p.out = make(chan outbound, queueLen)
}

// send 把幀排進寫入佇列。
func (p *peer) send(f relay.Frame) { p.enqueue(f.Marshal()) }

// enqueue 把一則訊息排進寫入佇列；佇列滿表示對方讀得太慢，斷開它。nil
// 表示寫完前面的之後斷開。
func (p *peer) enqueue(b []byte) {
	select {
	case p.out <- outbound{b: b}:
	default:
		p.cancel()
	}
}

// forward 把 App 轉來的資料幀排進 Mac 的寫入佇列。佇列裡已有 queueLen
// 則轉來的幀時，等 Mac 讀走一則再排：送得快的 App 等，Mac 不被斷開
// （RL16）。等的時候不讀這台 App，收不到它的 Pong：Mac 久讀不動時，心跳
// 逾時斷開它，不會一直等下去。waiting 不為 nil 時在開始等時呼叫。Mac 斷
// 開時丟掉這則；from 結束時放棄並回 false。
func (p *peer) forward(from context.Context, b []byte, waiting func()) bool {
	select {
	case p.credit <- struct{}{}:
	default:
		if waiting != nil {
			waiting()
		}
		select {
		case p.credit <- struct{}{}:
		case <-p.ctx.Done():
			return true
		case <-from.Done():
			return false
		}
	}
	select {
	case p.out <- outbound{b: b, credit: true}:
	default:
		// 控制幀把留給它們的位置也佔滿了：Mac 確實讀不動。
		<-p.credit
		p.cancel()
	}
	return true
}

// finish 送完已排的幀與 f 之後斷開。
func (p *peer) finish(f relay.Frame) {
	p.send(f)
	p.enqueue(nil)
}

func (p *peer) writeLoop() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case o := <-p.out:
			if o.b == nil {
				p.cancel()
				return
			}
			err := p.ws.Write(p.ctx, websocket.MessageBinary, o.b)
			if o.credit {
				<-p.credit
			}
			if err != nil {
				p.cancel()
				return
			}
		}
	}
}

func (p *peer) pingLoop(every time.Duration) {
	if every < 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(p.ctx, pingTimeout)
			err := p.ws.Ping(ctx)
			cancel()
			if err != nil {
				p.cancel()
				return
			}
		}
	}
}

// read 讀一幀，連同原始訊息（轉發時就地改寫幀頭，見 relay.Reframe）；
// 不是二進位或解不開時回錯。
func (p *peer) read(ctx context.Context) (relay.Frame, []byte, error) {
	typ, b, err := p.ws.Read(ctx)
	if err != nil {
		return relay.Frame{}, nil, err
	}
	if typ != websocket.MessageBinary {
		return relay.Frame{}, nil, relay.ErrBadFrame
	}
	f, err := relay.ParseFrame(b)
	return f, b, err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != relay.Path {
		http.NotFound(w, r)
		return
	}
	client, err := s.client(r)
	if err != nil {
		s.refuse(w, http.StatusBadRequest, err)
		return
	}
	// 名額在升級成 WebSocket 之前就佔下：超過上限的連線不花升級與 goroutine
	// 的成本（RL08、RL09）。
	if status := s.admit(client); status != 0 {
		s.refuse(w, status, nil)
		return
	}
	defer s.free(client)
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept 已回應 HTTP 錯誤
	}
	ws.SetReadLimit(relay.MaxFrame)
	ctx, cancel := context.WithCancel(s.ctx)
	p := &peer{ws: ws, ctx: ctx, cancel: cancel}
	var loops sync.WaitGroup
	defer func() {
		cancel() // 先停寫入與心跳，再等它們，最後關連線
		loops.Wait()
		ws.CloseNow()
	}()

	hello, err := s.hello(p)
	if err != nil {
		s.reject(p, err)
		return
	}
	p.queue(hello.Role)
	loops.Go(p.writeLoop)
	loops.Go(func() { p.pingLoop(s.opts.PingInterval) })

	switch hello.Role {
	case relay.RoleMac:
		s.serveMac(p, hello)
	case relay.RoleApp:
		s.serveApp(p, hello)
	}
}

// errReject 是握手被拒；code 為空時不送 KindError，直接斷開。
type errReject struct {
	code   relay.Code
	reason string
}

func (e *errReject) Error() string { return e.reason }

func (s *Server) hello(p *peer) (relay.Hello, error) {
	ctx, cancel := context.WithTimeout(p.ctx, s.helloTimeout)
	defer cancel()
	f, _, err := p.read(ctx)
	if err != nil {
		return relay.Hello{}, &errReject{reason: "no hello"}
	}
	var h relay.Hello
	if f.Kind != relay.KindHello || f.Conn != 0 || json.Unmarshal(f.Payload, &h) != nil {
		return h, &errReject{reason: "malformed hello"}
	}
	if h.V != relay.Version {
		return h, &errReject{code: relay.CodeVersionUnsupported, reason: "unsupported version"}
	}
	switch h.Role {
	case relay.RoleMac:
		key, err := relay.ParseRoomKey(h.Key)
		if err != nil {
			return h, &errReject{reason: "malformed room key"}
		}
		if h.Room != (relay.RoomID{}) {
			return h, &errReject{reason: "mac sent a room id"}
		}
		h.Room = key.Room()
	case relay.RoleApp:
		if h.Key != "" {
			return h, &errReject{reason: "app sent a room key"}
		}
		if h.Room == (relay.RoomID{}) {
			return h, &errReject{reason: "no room id"}
		}
	default:
		return h, &errReject{reason: "unknown role"}
	}
	return h, nil
}

// reject 送出代碼（若有）後斷開。
func (s *Server) reject(p *peer, err error) {
	var rej *errReject
	ctx, cancel := context.WithTimeout(p.ctx, pingTimeout)
	defer cancel()
	if errors.As(err, &rej) && rej.code != "" {
		_ = p.ws.Write(ctx, websocket.MessageBinary, relay.Frame{Kind: relay.KindError, Payload: []byte(rej.code)}.Marshal())
	}
	p.ws.Close(websocket.StatusPolicyViolation, "")
	s.log.Info("relay: rejected", "reason", err.Error())
}

// status 是告訴 App Mac 狀態的幀；在線時帶 Mac 的世代號。持有 s.mu。
func (r *room) status() relay.Frame {
	if r.mac == nil {
		return relay.Frame{Kind: relay.KindStatus, Payload: []byte{0}}
	}
	return relay.Frame{Kind: relay.KindStatus, Conn: r.gen, Payload: []byte{1}}
}

// serveMac 讓持有房間密鑰的 Mac 進入房間（RL01、RL02），接上已在房間裡
// 的 App（RL03、RL04），之後把 Mac 的幀轉給指定的 App。同一把密鑰的新
// 連線取代舊的，世代號加一。
func (s *Server) serveMac(p *peer, h relay.Hello) {
	log := s.log.With("room", h.Room.String(), "role", relay.RoleMac)

	s.mu.Lock()
	r := s.lookup(h.Room)
	old := r.mac
	r.mac = p
	if r.gen++; r.gen == 0 {
		r.gen = 1
	}
	p.send(relay.Frame{Kind: relay.KindReady})
	for conn, a := range r.apps {
		p.send(relay.Frame{Kind: relay.KindOpen, Conn: conn})
		a.send(r.status()) // 新的世代：App 重新握手
	}
	s.mu.Unlock()
	if old != nil {
		old.cancel()
		log.Info("relay: mac replaced")
	}
	log.Info("relay: mac online")

	for p.ctx.Err() == nil {
		f, b, err := p.read(p.ctx)
		if err != nil {
			break
		}
		s.mu.Lock()
		if r.mac != p {
			s.mu.Unlock()
			break
		}
		a := r.apps[f.Conn]
		switch f.Kind {
		case relay.KindData:
			if a != nil {
				a.enqueue(relay.Reframe(b, relay.KindData, r.gen))
			}
		case relay.KindClose:
			if a != nil {
				a.finish(relay.Frame{Kind: relay.KindClose, Conn: r.gen}) // App 知道是 Mac 斷開它
			}
		default:
			p.cancel()
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	if r.mac == p {
		r.mac = nil
		for _, a := range r.apps {
			a.send(r.status())
		}
		log.Info("relay: mac offline")
	}
	s.release(r)
	s.mu.Unlock()
	s.leave(r.id)
}

func (s *Server) leave(id relay.RoomID) {
	if s.left != nil {
		s.left(id)
	}
}

// serveApp 把 App 加進房間（RL03、RL04、RL07），之後把它的幀轉給 Mac，
// 並標上它的連線號（RL06）。
func (s *Server) serveApp(p *peer, h relay.Hello) {
	log := s.log.With("room", h.Room.String(), "role", relay.RoleApp)

	s.mu.Lock()
	r := s.lookup(h.Room)
	if len(r.apps) >= s.opts.MaxApps {
		s.release(r)
		s.mu.Unlock()
		s.reject(p, &errReject{reason: "room full"})
		return
	}
	conn := r.next + 1
	for conn == 0 || r.apps[conn] != nil {
		conn++
	}
	r.next = conn
	r.apps[conn] = p
	p.send(r.status())
	if r.mac != nil {
		r.mac.send(relay.Frame{Kind: relay.KindOpen, Conn: conn})
	}
	s.mu.Unlock()
	log = log.With("conn", conn)
	log.Info("relay: app joined")

	for {
		f, b, err := p.read(p.ctx)
		if err != nil || f.Kind != relay.KindData {
			break
		}
		s.mu.Lock()
		// 送給舊世代的幀丟掉：那是 Mac 換連線之前的 Noise 會話，新的 Mac
		// 會把它當成壞的握手。
		var mac *peer
		if r.mac != nil && f.Conn == r.gen {
			mac = r.mac
		}
		s.mu.Unlock()
		// 不持鎖等：只有送給這個 Mac 的 App 幀等它讀，其他房間與 Mac 寫給
		// App 的方向照常（RL16）。等的時候 Mac 換了連線，這則跟著舊的 Mac
		// 丟掉。
		if mac != nil && !mac.forward(p.ctx, relay.Reframe(b, relay.KindData, conn), s.waiting) {
			break
		}
	}

	s.mu.Lock()
	delete(r.apps, conn)
	if r.mac != nil {
		r.mac.send(relay.Frame{Kind: relay.KindClose, Conn: conn})
	}
	s.release(r)
	s.mu.Unlock()
	log.Info("relay: app left")
	s.leave(r.id)
}
