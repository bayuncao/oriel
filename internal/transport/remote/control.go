package remote

// control.go：設置 › 遠程背後的控制器（D145；remote.md DV、PA01、PA02、PA10、RC01）。
// 開關與中繼地址記在資料目錄的一個檔案裡（本機的偏好，同 D80 的開關）；Mac 的密鑰與
// 房間密鑰在鑰匙串（app 注入 Secrets）。打開且有中繼地址時，背景以退避重連中繼，
// 連上就以 Server 服務 App。app 建立、啟動與關閉它，mygoipc 的 Remote 服務把它
// 交給 設置 › 遠程 頁。

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/relay"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/internal/usage"
)

// Secrets 是放 Mac 密鑰與房間密鑰的鑰匙串（app 以 mygo.Secrets 實現）。
type Secrets interface {
	// Get 沒有這一項時回 ok == false、err == nil。
	Get(name string) (value []byte, ok bool, err error)
	Set(name string, value []byte) error
}

// 鑰匙串裡的兩項。Mac 的密鑰對存私鑰接公鑰（64 位元組）。
const (
	secretMac  = "remote.mac"
	secretRoom = "remote.room"
)

// 退避：連不上或斷開後 1 秒起每次翻倍，最多 1 分鐘；連上後歸零（DV10）。
const (
	backoffMin = time.Second
	backoffMax = time.Minute
)

// State 是與中繼的連線狀態。
type State string

const (
	StateOff        State = "off"        // 遠程關著
	StateNoRelay    State = "no_relay"   // 開著但沒有中繼地址（DV09）
	StateConnecting State = "connecting" // 正在連
	StateConnected  State = "connected"
	StateRetrying   State = "retrying" // 連不上或斷了，RetryAt 時重連（DV10）
	StateFailed     State = "failed"   // 不重連：鑰匙串讀不出密鑰、hub 已關閉；Retry 再試
)

// Status 是 設置 › 遠程 顯示的狀態。
type Status struct {
	On    bool   `json:"on"`
	Relay string `json:"relay"`
	State State  `json:"state"`
	// Error 是 retrying、failed 的原因。
	Error *transport.Error `json:"error,omitzero"`
	// RetryAt 是 retrying 時下次重連的時間。
	RetryAt time.Time `json:"retryAt,omitzero"`
}

// DeviceRow 是設備列表的一行：hub 裡的設備，加上它現在是否連著（DV03、DV14）。
type DeviceRow struct {
	Device hub.Device `json:"device"`
	Online bool       `json:"online"`
}

// PairingLink 是“添加設備”的二維碼內容與期限（DV13）。
type PairingLink struct {
	Link    string    `json:"link"`
	Expires time.Time `json:"expires"`
}

// ControlOptions 是 Control 的設定；Hub、Agents、Usage、Secrets、Path 必填。
type ControlOptions struct {
	Hub    *hub.Hub
	Agents AgentSource
	Usage  *usage.Tracker
	// Secrets 放 Mac 的密鑰與房間密鑰。
	Secrets Secrets
	// Path 是記開關與中繼地址的檔案。
	Path string
	// HTTPClient 連中繼用（app 注入走代理的）；nil → http.DefaultClient。
	HTTPClient *http.Client
	// Now 是時鐘（配對碼的期限、RetryAt）；nil → time.Now。
	Now func() time.Time
	// After 等退避；nil → time.After。測試注入以掌握退避。
	After func(d time.Duration) <-chan time.Time
	// PingInterval 傳給 relay.Options；0 → 30 秒，負數不發心跳。
	PingInterval time.Duration
	// HandshakeTimeout 傳給 Server；0 → 10 秒。
	HandshakeTimeout time.Duration
	Logger           *slog.Logger
}

// Control 管遠程的開關、中繼地址、密鑰、與中繼的連線、配對碼與設備列表。
// 方法可從任何 goroutine 呼叫。
type Control struct {
	o       ControlOptions
	log     *slog.Logger
	pairing *Pairing

	op sync.Mutex // 改設定、換密鑰、啟停背景連線一次一個；先拿 op 再拿 mu

	mu       sync.Mutex
	settings settings
	keys     *macKeys // 讀到或產生之後才有
	status   Status
	server   *Server       // 背景連線用的；沒在跑時為 nil
	changed  chan struct{} // 狀態或在線的設備變了就關掉換新
	closed   bool
	stop     context.CancelFunc // 背景連線在跑時不為 nil
	done     chan struct{}      // 背景連線結束時關閉
	retry    chan struct{}      // Retry：跳過退避
}

type settings struct {
	On    bool   `json:"on"`
	Relay string `json:"relay"`
}

type macKeys struct {
	static relay.Keypair
	room   relay.RoomKey
}

// NewControl 建立控制器；Start 之前什麼都不做。
func NewControl(o ControlOptions) *Control {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Control{o: o, log: o.Logger, pairing: NewPairing(o.Now),
		status: Status{State: StateOff}, changed: make(chan struct{}), retry: make(chan struct{}, 1)}
}

// Start 讀記住的開關與中繼地址；開著時取回密鑰並在背景連中繼（DV12、PA10）。讀不了
// 設定檔時當作關著，回傳原因（呼叫者記日誌）。
func (c *Control) Start() error {
	c.op.Lock()
	defer c.op.Unlock()
	s, err := readSettings(c.o.Path)
	c.mu.Lock()
	c.settings = s
	c.mu.Unlock()
	if s.On {
		if k, kerr := c.loadKeys(false); kerr != nil {
			c.setStatus(StateFailed, kerr, time.Time{})
		} else {
			c.useKeys(k)
		}
	}
	c.restart()
	return err
}

// Close 斷開中繼與所有設備，結束背景連線與 Watch（冪等）。
func (c *Control) Close() {
	c.op.Lock()
	defer c.op.Unlock()
	c.halt()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.changed)
	}
}

// Status 回目前的狀態。
func (c *Control) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Watch 先推目前的狀態，之後每次變化推一次；ctx 結束或 Close 時回 nil。
func (c *Control) Watch(ctx context.Context, send func(Status) error) error {
	var last *Status
	for {
		c.mu.Lock()
		s, changed, closed := c.status, c.changed, c.closed
		c.mu.Unlock()
		if last == nil || !sameStatus(*last, s) {
			if err := send(s); err != nil {
				return err
			}
			last = &s
		}
		if closed {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		}
	}
}

func sameStatus(a, b Status) bool {
	if a.On != b.On || a.Relay != b.Relay || a.State != b.State || !a.RetryAt.Equal(b.RetryAt) {
		return false
	}
	if (a.Error == nil) != (b.Error == nil) {
		return false
	}
	return a.Error == nil || a.Error.Code == b.Error.Code && maps.Equal(a.Error.Params, b.Error.Params)
}

// SetOn 打開或關掉遠程。第一次打開時產生 Mac 的密鑰對與房間密鑰存進鑰匙串
// （PA01）；鑰匙串不可用時回 remote.keychain_unavailable，保持關著（DV11）。關掉時
// 斷開中繼與所有設備，設備保留（DV07）。
func (c *Control) SetOn(on bool) error {
	c.op.Lock()
	defer c.op.Unlock()
	c.mu.Lock()
	s := c.settings
	c.mu.Unlock()
	if s.On == on {
		return nil
	}
	if on {
		k, err := c.loadKeys(true)
		if err != nil {
			return err
		}
		c.useKeys(k)
	}
	s.On = on
	if err := c.save(s); err != nil {
		return err
	}
	c.restart()
	return nil
}

// SetRelay 換中繼地址（DV08、DV15、DV16）：空字串清掉地址；不是 ws:// 或 wss://
// 開頭的地址回 remote.bad_relay_url，不保存。開著時斷開舊中繼、連新的。
func (c *Control) SetRelay(addr string) error {
	addr, err := normalizeRelay(addr)
	if err != nil {
		return err
	}
	c.op.Lock()
	defer c.op.Unlock()
	c.mu.Lock()
	s := c.settings
	c.mu.Unlock()
	if s.Relay == addr {
		return nil
	}
	s.Relay = addr
	if err := c.save(s); err != nil {
		return err
	}
	c.pairing.Cancel() // 二維碼裡的地址變了
	c.restart()
	return nil
}

// Retry 立即重連（DV10）：退避中跳過等待；failed 時再讀一次密鑰、再連。
func (c *Control) Retry() {
	c.op.Lock()
	defer c.op.Unlock()
	c.mu.Lock()
	st, on, keys := c.status.State, c.settings.On, c.keys
	c.mu.Unlock()
	switch {
	case st == StateRetrying:
		select {
		case c.retry <- struct{}{}:
		default:
		}
	case st == StateFailed && on:
		if keys == nil {
			k, err := c.loadKeys(false)
			if err != nil {
				c.setStatus(StateFailed, err, time.Time{})
				return
			}
			c.useKeys(k)
		}
		c.restart()
	}
}

// Offer 產生新的配對碼，回二維碼的內容（PA02、DV13）；之前還有效的作廢（PA06）。
// 沒連上中繼時回 remote.not_connected（DV09）。
func (c *Control) Offer() (PairingLink, error) {
	c.mu.Lock()
	st, relayURL, keys := c.status.State, c.settings.Relay, c.keys
	c.mu.Unlock()
	if st != StateConnected || keys == nil {
		return PairingLink{}, errNotConnected
	}
	o, err := c.pairing.Offer()
	if err != nil {
		return PairingLink{}, err
	}
	q := url.Values{
		"relay": {relayURL},
		"room":  {keys.room.Room().String()},
		"mac":   {keys.static.Public.String()},
		"key":   {o.Key.String()},
	}
	return PairingLink{Link: "orielhub://pair?" + q.Encode(), Expires: o.Expires}, nil
}

// CancelOffer 作廢顯示中的配對碼（桌面關掉了二維碼）。
func (c *Control) CancelOffer() { c.pairing.Cancel() }

// Reset 重置遠程（DV06、CN05）：換掉 Mac 的密鑰與房間密鑰，撤銷所有設備；開著時
// 以新的房間重連。先換密鑰：鑰匙串不可用時什麼都不變。
func (c *Control) Reset(ctx context.Context) error {
	c.op.Lock()
	defer c.op.Unlock()
	k, err := newKeys()
	if err != nil {
		return err
	}
	if err := c.storeKeys(k); err != nil {
		return err
	}
	c.pairing.Cancel()
	c.useKeys(k)
	c.restart()
	return c.o.Hub.RevokeDevices(ctx, hub.LocalOwner)
}

// Rename 改設備的名字（DV04）。
func (c *Control) Rename(ctx context.Context, id task.DeviceID, name string) (hub.Device, error) {
	return c.o.Hub.RenameDevice(ctx, hub.LocalOwner, id, name)
}

// Revoke 撤銷一台設備，它立即斷開（DV05、CN04）。
func (c *Control) Revoke(ctx context.Context, id task.DeviceID) error {
	return c.o.Hub.RevokeDevice(ctx, hub.LocalOwner, id)
}

// WatchDevices 先推目前的設備列表，之後設備或在線的設備變了就再推一次（DV03、DV14）；
// ctx 結束或 Close 時回 nil，hub 關閉回 hub.closed。
func (c *Control) WatchDevices(ctx context.Context, send func([]DeviceRow) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lists := make(chan []hub.Device, 1)
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- c.o.Hub.WatchDevices(ctx, hub.LocalOwner, func(ds []hub.Device) error {
			select { // 只留最新的一份；只有這個 goroutine 寫，清空後放得進去
			case <-lists:
			default:
			}
			lists <- ds
			return nil
		})
	}()
	var (
		devices    []hub.Device
		have       bool
		sentOnline []task.DeviceID
		dirty      bool
	)
	for {
		c.mu.Lock()
		changed, closed, srv := c.changed, c.closed, c.server
		c.mu.Unlock()
		var online []task.DeviceID
		if srv != nil {
			online = srv.Online()
		}
		if have && (dirty || !slices.Equal(online, sentOnline)) {
			if err := send(deviceRows(devices, online)); err != nil {
				return err
			}
			sentOnline, dirty = online, false
		}
		if closed {
			return nil
		}
		select {
		case ds := <-lists:
			devices, have, dirty = ds, true, true
		case <-changed:
		case err := <-watchErr:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-ctx.Done():
			return nil
		}
	}
}

func deviceRows(devices []hub.Device, online []task.DeviceID) []DeviceRow {
	rows := make([]DeviceRow, len(devices))
	for i, d := range devices {
		_, on := slices.BinarySearch(online, d.ID)
		rows[i] = DeviceRow{Device: d, Online: on}
	}
	return rows
}

// restart 按目前的設定與密鑰（重新）開始背景連線；呼叫者持有 op。
func (c *Control) restart() {
	c.halt()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	s, keys := c.settings, c.keys
	switch {
	case !s.On:
		c.setStatusLocked(StateOff, nil, time.Time{})
		return
	case keys == nil:
		return // 狀態已是 failed（密鑰讀不出來）
	case s.Relay == "":
		c.setStatusLocked(StateNoRelay, nil, time.Time{})
		return
	}
	srv := NewServer(Options{Hub: c.o.Hub, Agents: c.o.Agents, Usage: c.o.Usage, Static: keys.static,
		Pairing: c.pairing, Logger: c.log, HandshakeTimeout: c.o.HandshakeTimeout, OnlineChanged: c.broadcast})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.server, c.stop, c.done = srv, cancel, done
	select { // 上一輪沒用掉的 Retry 不帶到這一輪
	case <-c.retry:
	default:
	}
	c.setStatusLocked(StateConnecting, nil, time.Time{})
	ro := relay.Options{URL: s.Relay, HTTPClient: c.o.HTTPClient, PingInterval: c.o.PingInterval}
	go func() {
		defer close(done)
		c.run(ctx, srv, ro, keys.room)
	}()
}

// halt 結束背景連線並等它結束；呼叫者持有 op，不持有 mu。
func (c *Control) halt() {
	c.mu.Lock()
	stop, done := c.stop, c.done
	c.stop, c.done = nil, nil
	c.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	<-done
	c.mu.Lock()
	c.server = nil
	c.broadcastLocked() // 設備都斷了
	c.mu.Unlock()
}

// run 連中繼、服務 App，斷了按退避重連，直到 ctx 結束或 hub 關閉（RC01、DV10）。
func (c *Control) run(ctx context.Context, srv *Server, ro relay.Options, room relay.RoomKey) {
	delay := backoffMin
	for {
		c.setStatus(StateConnecting, nil, time.Time{})
		m, err := relay.DialMac(ctx, ro, room)
		if err == nil {
			delay = backoffMin
			c.setStatus(StateConnected, nil, time.Time{})
			err = srv.Serve(ctx, m)
			m.Close()
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, hub.ErrClosed) {
			c.setStatus(StateFailed, err, time.Time{})
			return
		}
		c.log.Info("remote: relay unavailable", "err", err, "retry_in", delay)
		c.setStatus(StateRetrying, err, c.o.Now().Add(delay))
		select {
		case <-c.o.After(delay):
		case <-c.retry:
		case <-ctx.Done():
			return
		}
		delay = min(delay*2, backoffMax)
	}
}

func (c *Control) setStatus(st State, err error, retryAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setStatusLocked(st, err, retryAt)
}

func (c *Control) setStatusLocked(st State, err error, retryAt time.Time) {
	s := Status{On: c.settings.On, Relay: c.settings.Relay, State: st, RetryAt: retryAt}
	if err != nil {
		s.Error, _ = transport.Wire(err)
	}
	c.status = s
	c.broadcastLocked()
}

func (c *Control) broadcast() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.broadcastLocked()
}

func (c *Control) broadcastLocked() {
	if c.closed {
		return
	}
	close(c.changed)
	c.changed = make(chan struct{})
}

// save 記下 s 並更新狀態裡的開關與地址；呼叫者持有 op。
func (c *Control) save(s settings) error {
	if err := writeSettings(c.o.Path, s); err != nil {
		return saveError(err)
	}
	c.mu.Lock()
	c.settings = s
	c.mu.Unlock()
	return nil
}

func (c *Control) useKeys(k macKeys) {
	c.mu.Lock()
	c.keys = &k
	c.mu.Unlock()
}

// loadKeys 從鑰匙串取回密鑰；沒有時 create 為真就產生並存進去（PA01）。
func (c *Control) loadKeys(create bool) (macKeys, error) {
	mac, okMac, err := c.o.Secrets.Get(secretMac)
	if err != nil {
		return macKeys{}, keychainError(err)
	}
	room, okRoom, err := c.o.Secrets.Get(secretRoom)
	if err != nil {
		return macKeys{}, keychainError(err)
	}
	if okMac && okRoom {
		if len(mac) != 64 || len(room) != 32 {
			return macKeys{}, keychainError(errors.New("malformed remote keys"))
		}
		var k macKeys
		copy(k.static.Private[:], mac[:32])
		copy(k.static.Public[:], mac[32:])
		copy(k.room[:], room)
		return k, nil
	}
	if !create {
		return macKeys{}, keychainError(errors.New("remote keys missing"))
	}
	k, err := newKeys()
	if err != nil {
		return macKeys{}, err
	}
	return k, c.storeKeys(k)
}

func newKeys() (macKeys, error) {
	static, err := relay.GenerateKeypair()
	if err != nil {
		return macKeys{}, err
	}
	room, err := relay.NewRoomKey()
	if err != nil {
		return macKeys{}, err
	}
	return macKeys{static: static, room: room}, nil
}

func (c *Control) storeKeys(k macKeys) error {
	mac := slices.Concat(k.static.Private[:], k.static.Public[:])
	if err := c.o.Secrets.Set(secretMac, mac); err != nil {
		return keychainError(err)
	}
	if err := c.o.Secrets.Set(secretRoom, k.room[:]); err != nil {
		return keychainError(err)
	}
	return nil
}

// normalizeRelay 去掉空白與結尾的 /；空字串照收（清掉地址）。
func normalizeRelay(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", nil
	}
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", &paramError{code: CodeBadRelayURL, params: map[string]string{"url": addr}, cause: err}
	}
	return strings.TrimSuffix(addr, "/"), nil
}

func readSettings(path string) (settings, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return settings{}, nil
	}
	if err != nil {
		return settings{}, err
	}
	var s settings
	if err := json.Unmarshal(b, &s); err != nil {
		return settings{}, err
	}
	return s, nil
}

// writeSettings 先寫暫存檔再換名，不留寫了一半的檔案。
func writeSettings(path string, s settings) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".remote-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
