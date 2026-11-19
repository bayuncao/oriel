package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Conn 是一條有訊息邊界的雙向通道：App 與 Mac 之間經中繼的一條連線。
// Noise 的 Session 架在它上面。Send 可以並行，返回後不再引用 msg；Recv
// 一次一個呼叫者。
type Conn interface {
	Send(ctx context.Context, msg []byte) error
	Recv(ctx context.Context) ([]byte, error)
}

var (
	// ErrClosed 是連線已關閉：本端關閉、對端離開，或 Mac 換了連線（App
	// 一側舊世代的 Stream）。
	ErrClosed = errors.New("relay: connection closed")
	// ErrRefused 是 Mac 斷開了這台 App（拒絕握手或撤銷設備）。只有 App
	// 一側會收到；網路斷線是 CodeDisconnected，不是它。
	ErrRefused = errors.New("relay: closed by the Mac")
	// ErrTooLarge 是一則訊息超過 MaxPayload。
	ErrTooLarge = errors.New("relay: message too large")
)

const (
	// acceptBacklog 是 Mac 還沒 Accept 的 App 連線上限，滿了就斷開新的。
	acceptBacklog = 64
	// inboxLen 是一條連線還沒讀走的訊息上限，滿了就斷開這條連線：讀得
	// 太慢的一方不能拖住同一條 WebSocket 上的其他連線。
	inboxLen = 256
	// defaultPing 是對中繼的心跳間隔；網路換了而 TCP 沒斷時靠它發現。
	defaultPing = 30 * time.Second
	pingTimeout = 10 * time.Second
	// writeTimeout 是一幀寫出去的上限；超過表示連線卡死，斷開。
	writeTimeout = 10 * time.Second
)

// Options 是連到中繼的共同設定。
type Options struct {
	// URL 是中繼的位址（wss://host 或 ws://host），不含 Path。
	URL string
	// HTTPClient 為 nil 時用 http.DefaultClient；app 注入走代理的客戶端。
	HTTPClient *http.Client
	// PingInterval 為 0 時用 30 秒，負數不發心跳。
	PingInterval time.Duration
}

// link 是一條到中繼的 WebSocket，所有幀都經過它。
type link struct {
	ws     *websocket.Conn
	ctx    context.Context // 連線存活期間有效；cancel 後讀寫都結束
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	err    error
	wmu    chan struct{}  // 寫入權：一次一幀
	wg     sync.WaitGroup // 讀迴圈、心跳與讀迴圈發起的寫；shutdown 等它們
}

// dial 連上中繼、送出 Hello，並讀第一幀交給呼叫者判斷。
func dial(ctx context.Context, o Options, hello Hello) (*link, Frame, error) {
	url := strings.TrimSuffix(o.URL, "/") + Path
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: o.HTTPClient})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests) {
			return nil, Frame{}, &Error{Code: CodeBusy, cause: err} // RL08、RL09
		}
		return nil, Frame{}, fail(CodeUnreachable, err)
	}
	ws.SetReadLimit(MaxFrame)
	lctx, cancel := context.WithCancel(context.Background())
	l := &link{ws: ws, ctx: lctx, cancel: cancel, done: make(chan struct{}), wmu: make(chan struct{}, 1)}
	payload, err := json.Marshal(hello)
	if err != nil {
		l.close(err)
		return nil, Frame{}, err
	}
	if err := l.write(ctx, Frame{Kind: KindHello, Payload: payload}); err != nil {
		l.close(err)
		return nil, Frame{}, fail(CodeUnreachable, err)
	}
	f, err := l.read(ctx)
	if err != nil {
		l.close(err)
		return nil, Frame{}, fail(CodeUnreachable, err)
	}
	if f.Kind == KindError {
		err := codeError(f.Payload)
		l.close(err)
		return nil, Frame{}, err
	}
	l.wg.Go(func() { l.ping(o.PingInterval) })
	return l, f, nil
}

// codeError 把中繼送來的代碼轉為 Error；不認得的代碼算斷線。
func codeError(payload []byte) error {
	code := Code(payload)
	if _, ok := codeText[code]; ok && code.Params() == nil {
		return &Error{Code: code}
	}
	return fail(CodeDisconnected, fmt.Errorf("relay sent %q", payload))
}

// write 送一幀。ctx 只限制等待寫入權的時間，不碰寫入本身：coder/websocket
// 在寫入途中 ctx 結束時會關掉整條 WebSocket，那會斷開同一條連線上的所有
// App。寫入用 l.ctx 加 writeTimeout；失敗表示連線壞了，關閉 link。
func (l *link) write(ctx context.Context, f Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.wmu <- struct{}{}:
	case <-l.done:
		return l.err
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.wmu }()
	wctx, cancel := context.WithTimeout(l.ctx, writeTimeout)
	defer cancel()
	if err := l.ws.Write(wctx, websocket.MessageBinary, f.Marshal()); err != nil {
		l.close(err)
		return l.err
	}
	return nil
}

// read 讀一幀。ctx 結束時 websocket 會關掉整條連線，所以長期的讀取只用
// l.ctx。
func (l *link) read(ctx context.Context) (Frame, error) {
	typ, b, err := l.ws.Read(ctx)
	if err != nil {
		return Frame{}, err
	}
	if typ != websocket.MessageBinary {
		return Frame{}, ErrBadFrame
	}
	return ParseFrame(b)
}

func (l *link) ping(every time.Duration) {
	if every == 0 {
		every = defaultPing
	}
	if every < 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(l.ctx, pingTimeout)
			err := l.ws.Ping(ctx)
			cancel()
			if err != nil {
				l.close(fail(CodeDisconnected, err))
				return
			}
		}
	}
}

// shutdown 由使用者關閉：斷開並等讀迴圈與心跳結束。不可在它們裡面呼叫。
func (l *link) shutdown() {
	l.close(ErrClosed)
	l.wg.Wait()
}

// close 只生效一次：記下原因並關掉 WebSocket。ErrClosed、ErrRefused 與
// *Error 原樣記下，其餘算 CodeDisconnected。
func (l *link) close(err error) {
	l.once.Do(func() {
		var e *Error
		if !errors.As(err, &e) && !errors.Is(err, ErrClosed) && !errors.Is(err, ErrRefused) {
			err = fail(CodeDisconnected, err)
		}
		l.err = err
		l.cancel()
		l.ws.CloseNow()
		close(l.done)
	})
}

// Done 在與中繼的連線結束時關閉。
func (l *link) Done() <-chan struct{} { return l.done }

// Err 是連線結束的原因；Done 關閉之前是 nil。
func (l *link) Err() error {
	select {
	case <-l.done:
		return l.err
	default:
		return nil
	}
}

// Stream 是經中繼的一條連線。Mac 一側每台 App 一條（Accept）；App 一側
// Mac 每次上線一條（App.Online），Mac 換了連線，舊的就結束。
type Stream struct {
	l    *link
	id   uint32 // Mac 一側是連線號，App 一側是世代號
	in   chan []byte
	done chan struct{}
	once sync.Once
	err  error
	// release 在本端 Close 時呼叫（Mac 一側通知中繼）；可為 nil。
	release func(*Stream)
}

func newStream(l *link, id uint32, release func(*Stream)) *Stream {
	return &Stream{l: l, id: id, in: make(chan []byte, inboxLen), done: make(chan struct{}), release: release}
}

// ID 是中繼給這條連線的編號，只用於日誌。
func (s *Stream) ID() uint32 { return s.id }

// Send 送一則訊息。ctx 結束只讓這次 Send 放棄等待，不影響連線。
func (s *Stream) Send(ctx context.Context, msg []byte) error {
	if len(msg) > MaxPayload {
		return ErrTooLarge
	}
	select {
	case <-s.done:
		return s.err
	default:
	}
	return s.l.write(ctx, Frame{Kind: KindData, Conn: s.id, Payload: msg})
}

func (s *Stream) Recv(ctx context.Context) ([]byte, error) {
	select {
	case b := <-s.in:
		return b, nil
	default:
	}
	select {
	case b := <-s.in:
		return b, nil
	case <-s.done: // 已收到的先交出，再回報結束
		select {
		case b := <-s.in:
			return b, nil
		default:
			return nil, s.err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close 結束這條連線。Mac 一側同時請中繼斷開這台 App（撤銷設備時，CN04）。
func (s *Stream) Close() error {
	if s.end(ErrClosed) && s.release != nil {
		s.release(s)
	}
	return nil
}

// Done 在這條連線結束時關閉。
func (s *Stream) Done() <-chan struct{} { return s.done }

func (s *Stream) end(err error) bool {
	first := false
	s.once.Do(func() {
		first = true
		s.err = err
		close(s.done)
	})
	return first
}

// deliver 把一則訊息交給 s；收件匣滿了回 false。
func (s *Stream) deliver(b []byte) bool {
	select {
	case s.in <- b:
		return true
	default:
		return false
	}
}

// Mac 是 Mac 一側到中繼的連線：以房間密鑰進入房間，接受房間裡的 App 連線。
type Mac struct {
	*link
	accepts chan *Stream

	mu      sync.Mutex
	streams map[uint32]*Stream
}

// DialMac 以 key 連上中繼（RL01）；房間號是 key.Room()。已在房間裡的 App
// 會隨即出現在 Accept。同一把密鑰的新連線取代舊的。
func DialMac(ctx context.Context, o Options, key RoomKey) (*Mac, error) {
	l, f, err := dial(ctx, o, Hello{V: Version, Role: RoleMac, Key: key.String()})
	if err != nil {
		return nil, err
	}
	if f.Kind != KindReady {
		err := fail(CodeDisconnected, fmt.Errorf("expected ready, got %s", f.Kind))
		l.close(err)
		l.wg.Wait()
		return nil, err
	}
	m := &Mac{link: l, accepts: make(chan *Stream, acceptBacklog), streams: map[uint32]*Stream{}}
	m.wg.Go(m.loop)
	return m, nil
}

func (m *Mac) loop() {
	defer m.closeStreams()
	for {
		f, err := m.read(m.ctx)
		if err != nil {
			m.close(err)
			return
		}
		switch f.Kind {
		case KindOpen:
			s := newStream(m.link, f.Conn, m.release)
			m.mu.Lock()
			old := m.streams[f.Conn]
			m.streams[f.Conn] = s
			m.mu.Unlock()
			if old != nil {
				old.end(ErrClosed)
			}
			select {
			case m.accepts <- s:
			default:
				m.drop(s)
			}
		case KindData:
			m.mu.Lock()
			s := m.streams[f.Conn]
			m.mu.Unlock()
			if s != nil && !s.deliver(f.Payload) {
				m.drop(s)
			}
		case KindClose:
			m.mu.Lock()
			s := m.streams[f.Conn]
			delete(m.streams, f.Conn)
			m.mu.Unlock()
			if s != nil {
				s.end(ErrClosed)
			}
		case KindError:
			m.close(codeError(f.Payload))
			return
		default:
			m.close(fmt.Errorf("unexpected %s frame", f.Kind))
			return
		}
	}
}

// release 把 s 從表裡拿掉並請中繼斷開它。
func (m *Mac) release(s *Stream) {
	m.mu.Lock()
	if m.streams[s.id] == s {
		delete(m.streams, s.id)
	}
	m.mu.Unlock()
	_ = m.write(m.ctx, Frame{Kind: KindClose, Conn: s.id}) // 失敗時 link 已關閉，所有 Stream 隨之結束
}

// drop 由讀迴圈斷開一條跟不上的連線。通知中繼的寫入交給另一個 goroutine：
// 讀迴圈不能卡在寫入上，否則其他連線與心跳的回應都停住。
func (m *Mac) drop(s *Stream) {
	if s.end(ErrClosed) {
		m.wg.Go(func() { m.release(s) })
	}
}

func (m *Mac) closeStreams() {
	m.mu.Lock()
	streams := m.streams
	m.streams = map[uint32]*Stream{}
	m.mu.Unlock()
	for _, s := range streams {
		s.end(m.err)
	}
}

// Accept 等下一條 App 連線。與中繼斷線後回 Err。
func (m *Mac) Accept(ctx context.Context) (*Stream, error) {
	select {
	case s := <-m.accepts:
		return s, nil
	case <-m.done:
		return nil, m.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close 斷開中繼與房間裡的所有 App 連線（遠程關閉，DV07）。
func (m *Mac) Close() error {
	m.shutdown()
	return nil
}

// App 是 App 一側到中繼的連線。iOS App 用 Swift 實作同一套幀；這個 Go
// 客戶端是協定的參考實作，也是測試裡的 App。
type App struct {
	*link

	mu      sync.Mutex
	stream  *Stream // Mac 這次上線的連線；離線時為 nil。只有讀迴圈改它
	changed chan struct{}
}

// DialApp 連入 room。房間不存在與 Mac 不在線一樣回報離線（RL07）。
func DialApp(ctx context.Context, o Options, room RoomID) (*App, error) {
	l, f, err := dial(ctx, o, Hello{V: Version, Role: RoleApp, Room: room})
	if err != nil {
		return nil, err
	}
	if f.Kind != KindStatus || len(f.Payload) != 1 {
		err := fail(CodeDisconnected, fmt.Errorf("expected status, got %s", f.Kind))
		l.close(err)
		l.wg.Wait()
		return nil, err
	}
	a := &App{link: l, changed: make(chan struct{})}
	a.setStatus(f)
	a.wg.Go(a.loop)
	return a, nil
}

func (a *App) loop() {
	defer a.endStream()
	for {
		f, err := a.read(a.ctx)
		if err != nil {
			a.close(err)
			return
		}
		switch f.Kind {
		case KindStatus:
			if len(f.Payload) != 1 {
				a.close(ErrBadFrame)
				return
			}
			a.setStatus(f)
		case KindData:
			s := a.stream
			if s == nil || s.id != f.Conn {
				continue // 舊世代的幀
			}
			if !s.deliver(f.Payload) {
				a.close(errors.New("inbox full"))
				return
			}
		case KindClose:
			a.close(ErrRefused)
			return
		case KindError:
			a.close(codeError(f.Payload))
			return
		default:
			a.close(fmt.Errorf("unexpected %s frame", f.Kind))
			return
		}
	}
}

// setStatus 記下 Mac 的狀態。Mac 每次上線（包括換了連線）都是新的世代：
// 上一段的 Stream 結束，上面的 Noise 會話作廢，App 在新的 Stream 上重新
// 握手；中繼丟掉 App 還送給舊世代的幀。
func (a *App) setStatus(f Frame) {
	var next *Stream
	if f.Payload[0] == 1 {
		next = newStream(a.link, f.Conn, nil)
	}
	a.mu.Lock()
	old := a.stream
	a.stream = next
	close(a.changed)
	a.changed = make(chan struct{})
	a.mu.Unlock()
	if old != nil {
		old.end(ErrClosed)
	}
}

func (a *App) endStream() {
	a.mu.Lock()
	s := a.stream
	a.mu.Unlock()
	if s != nil {
		s.end(a.err)
	}
}

// Online 是 Mac 這次上線的連線（離線時 nil），以及在下一次變化時關閉的
// 通道。
func (a *App) Online() (*Stream, <-chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stream, a.changed
}

// Close 斷開與中繼的連線。
func (a *App) Close() error {
	a.shutdown()
	return nil
}
