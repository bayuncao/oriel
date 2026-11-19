package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/relay"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
)

// 遠程服務的驗收場景（docs/acceptance/remote.md 第 3 節 CN 與 RQ）：真的中繼、
// 真的 hub（MemStore），App 一側是下面的 client，走與 iOS App 同一套握手與 RPC。

// served 是在背景跑的 Server.Serve。
type served struct {
	s    *Server
	left chan task.DeviceID // 每條設備連線清理完畢後收到它的設備
	stop context.CancelFunc
	done chan struct{}
	err  error // Serve 的結果；done 關閉後可讀
}

// serve 以 e 的 hub、Mac 密鑰與配對碼啟動遠程服務；o 的其他欄位照用。
func (e *env) serve(o Options) *served {
	e.t.Helper()
	o.Hub, o.Static, o.Pairing, o.Usage, o.Agents = e.hub, e.macKeys, e.pairing, e.usage, e.agents
	o.Logger = slog.New(slog.DiscardHandler)
	sv := &served{s: NewServer(o), left: make(chan task.DeviceID, 16), done: make(chan struct{})}
	sv.s.left = func(id task.DeviceID) { sv.left <- id }
	c, cancel := context.WithCancel(context.Background())
	sv.stop = cancel
	go func() {
		defer close(sv.done)
		sv.err = sv.s.Serve(c, e.mac)
	}()
	e.t.Cleanup(func() {
		cancel()
		<-sv.done
	})
	return sv
}

// wait 等 Serve 返回，回它的結果。
func (sv *served) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-sv.done:
		return sv.err
	case <-time.After(timeout):
		t.Fatal("Serve did not return")
		return nil
	}
}

// gone 等 n 條設備連線清理完畢，回它們的設備（排序過）。
func (sv *served) gone(t *testing.T, n int) []task.DeviceID {
	t.Helper()
	var ids []task.DeviceID
	for range n {
		select {
		case id := <-sv.left:
			ids = append(ids, id)
		case <-time.After(timeout):
			t.Fatalf("only %d of %d connections cleaned up", len(ids), n)
		}
	}
	slices.Sort(ids)
	return ids
}

// device 在 hub 登記一台手機（同配對完成），回它的密鑰與設備。
func (e *env) device(name string) (relay.Keypair, hub.Device) {
	e.t.Helper()
	phone := keypair(e.t)
	d, err := e.hub.PairDevice(ctx(e.t), hub.LocalOwner, hub.DeviceKey(phone.Public), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return phone, d
}

// newTask 以桌面的身份建一個 Task。
func (e *env) newTask(title string) task.TaskID {
	e.t.Helper()
	id, _, err := e.hub.CreateTask(ctx(e.t), hub.LocalOwner, e.t.TempDir(), task.CreateTask{Title: title})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) command(id task.TaskID, kind task.CommandKind) {
	e.t.Helper()
	if _, err := e.hub.Command(ctx(e.t), hub.LocalOwner, id, task.Command{Kind: kind}); err != nil {
		e.t.Fatal(err)
	}
}

// client 是 App 一側：一條握手完成的連線，背景讀回覆。只在測試的 goroutine 上用。
type client struct {
	t      *testing.T
	device task.DeviceID
	sess   *relay.Session
	nextID uint64

	replies chan Reply // 讀到的回覆，連線斷開時關閉
	err     error      // 斷開的原因；replies 關閉後可讀
	stash   map[uint64][]Reply
}

// dial 以 phone 經中繼連入 Mac（Noise_IK）。Mac 拒絕時回握手回覆裡的
// *transport.Error。
func (e *env) dial(phone relay.Keypair) (*client, error) {
	e.t.Helper()
	app, err := relay.DialApp(ctx(e.t), e.opts, e.room)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { app.Close() })
	sess, b, err := relay.Initiate(ctx(e.t), online(e.t, app), relay.Initiator{Static: phone, Mac: e.macKeys.Public})
	if err != nil {
		return nil, err
	}
	var reply HandshakeReply
	if err := json.Unmarshal(b, &reply); err != nil {
		e.t.Fatalf("handshake reply %q: %v", b, err)
	}
	if reply.Error != nil {
		return nil, reply.Error
	}
	return newClient(e.t, reply.Device, sess), nil
}

// join 同 dial，失敗即中止測試。
func (e *env) join(phone relay.Keypair) *client {
	e.t.Helper()
	c, err := e.dial(phone)
	if err != nil {
		e.t.Fatalf("connect: %v", err)
	}
	return c
}

func newClient(t *testing.T, device task.DeviceID, sess *relay.Session) *client {
	c := &client{t: t, device: device, sess: sess, replies: make(chan Reply, 1024), stash: map[uint64][]Reply{}}
	go func() {
		defer close(c.replies)
		for {
			b, err := sess.Recv(context.Background())
			if err != nil {
				c.err = err
				return
			}
			var r Reply
			if err := json.Unmarshal(b, &r); err != nil {
				c.err = err
				return
			}
			c.replies <- r
		}
	}()
	return c
}

// send 送一則原始訊息。
func (c *client) send(b []byte) {
	c.t.Helper()
	if err := c.sess.Send(ctx(c.t), b); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

// request 送一個請求，回它的 id。params 為 nil 時不帶參數；json.RawMessage 原樣送出。
func (c *client) request(method string, params any) uint64 {
	c.t.Helper()
	c.nextID++
	req := Request{ID: c.nextID, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			c.t.Fatal(err)
		}
		req.Params = b
	}
	c.sendRequest(req)
	return req.ID
}

func (c *client) sendRequest(req Request) {
	c.t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(b)
}

func (c *client) cancel(id uint64) {
	c.t.Helper()
	c.sendRequest(Request{ID: id, Cancel: true})
}

// next 回下一則收到的回覆（不論 id）。
func (c *client) next() Reply {
	c.t.Helper()
	select {
	case r, ok := <-c.replies:
		if !ok {
			c.t.Fatalf("disconnected: %v", c.err)
		}
		return r
	case <-time.After(timeout):
		c.t.Fatal("no reply")
		return Reply{}
	}
}

// reply 回 id 的下一則回覆；先到的其他 id 的回覆留給之後。
func (c *client) reply(id uint64) Reply {
	c.t.Helper()
	if rs := c.stash[id]; len(rs) > 0 {
		c.stash[id] = rs[1:]
		return rs[0]
	}
	for {
		r := c.next()
		if r.ID == id {
			return r
		}
		c.stash[r.ID] = append(c.stash[r.ID], r)
	}
}

// call 呼叫一般方法並等它的回覆。
func (c *client) call(method string, params any) Reply {
	c.t.Helper()
	return c.reply(c.request(method, params))
}

// closed 等 Mac 斷開這條連線，回斷開的原因。
func (c *client) closed() error {
	c.t.Helper()
	for {
		select {
		case _, ok := <-c.replies:
			if !ok {
				return c.err
			}
		case <-time.After(timeout):
			c.t.Fatal("still connected")
			return nil
		}
	}
}

// pushUntil 讀 id 的推送直到 found 為真；推送以外的回覆即中止測試。
func pushUntil[T any](c *client, id uint64, found func(T) bool) T {
	c.t.Helper()
	for {
		r := c.reply(id)
		if r.Push == nil {
			c.t.Fatalf("subscription %d ended: %+v (error %+v)", id, r, r.Error)
		}
		var v T
		if err := json.Unmarshal(r.Push, &v); err != nil {
			c.t.Fatal(err)
		}
		if found(v) {
			return v
		}
	}
}

// endOf 讀 id 的回覆，跳過推送，回結束它的那一則。
func endOf(c *client, id uint64) Reply {
	c.t.Helper()
	for {
		if r := c.reply(id); r.Push == nil {
			return r
		}
	}
}

func any2[T any](T) bool { return true }

// wantError 確認 r 是帶 code 的錯誤回覆。
func wantError(t *testing.T, r Reply, code string) {
	t.Helper()
	if r.Error == nil || r.Error.Code != code || r.Result != nil || r.Push != nil || r.End {
		t.Fatalf("got %+v (error %+v), want error %s", r, r.Error, code)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func compact(t *testing.T, b []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("%q: %v", b, err)
	}
	return buf.String()
}

// 白名單（D145）：加一個方法要同時改這裡，確認它該開放給手機。
func TestMethods(t *testing.T) {
	want := []string{"Agents.List", "Attention", "Command", "Confirmations", "CreateTask", "Snapshot", "Subscribe",
		"Tasks", "Usage.Refresh", "Usage.Summary", "Usage.Watch", "WatchTasks", "Workspaces"}
	if got := Methods(); !slices.Equal(got, want) {
		t.Fatalf("Methods() = %v, want %v", got, want)
	}
}

func TestCN01ConnectRecognisesTheDevice(t *testing.T) {
	e := newEnv(t)
	sv := e.serve(Options{})
	phone, d := e.device("iPhone")
	c := e.join(phone)
	if c.device != d.ID {
		t.Fatalf("connected as %q, want %q", c.device, d.ID)
	}
	// 第一個請求被處理時，連上的最後在線已經記下。
	wantError(t, c.call("PutSettings", nil), string(CodeNotAllowed))
	if ds := e.devices(); len(ds) != 1 || !ds[0].LastSeen.Equal(e.clock.Now()) {
		t.Fatalf("devices %+v, want last seen %v", ds, e.clock.Now())
	}
	e.clock.advance(time.Minute)
	sv.stop()
	if err := sv.wait(t); err != nil {
		t.Fatal(err)
	}
	if ds := e.devices(); !ds[0].LastSeen.Equal(e.clock.Now()) {
		t.Fatalf("last seen %v after disconnect, want %v", ds[0].LastSeen, e.clock.Now())
	}
}

func TestHandshakeRefusals(t *testing.T) {
	cases := []struct {
		name  string
		phone func(e *env) relay.Keypair
	}{
		{"CN02 key not paired", func(e *env) relay.Keypair { return keypair(e.t) }},
		{"CN03 device revoked", func(e *env) relay.Keypair {
			phone, d := e.device("iPhone")
			if err := e.hub.RevokeDevice(ctx(e.t), hub.LocalOwner, d.ID); err != nil {
				e.t.Fatal(err)
			}
			return phone
		}},
		{"CN05 remote reset", func(e *env) relay.Keypair {
			phone, _ := e.device("iPhone")
			if err := e.hub.RevokeDevices(ctx(e.t), hub.LocalOwner); err != nil {
				e.t.Fatal(err)
			}
			return phone
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.serve(Options{})
			_, err := e.dial(tc.phone(e))
			wantCode(t, err, string(CodeUnknownDevice))
		})
	}
}

func TestPairingThroughTheServer(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	phone := keypair(t)
	key := e.offer() // 二維碼先顯示，App 掃了它才知道房間
	app, err := relay.DialApp(ctx(t), e.opts, e.room)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	id, sess, err := PairWith(ctx(t), online(t, app), phone, e.macKeys.Public, key, "iPhone")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if want := hub.DeviceKey(phone.Public).ID(); id != want {
		t.Fatalf("paired as %q, want %q", id, want)
	}
	// 配對的連線接著就能用，不必重連。
	c := newClient(t, id, sess)
	if r := c.call("Tasks", nil); r.Error != nil {
		t.Fatalf("Tasks after pairing: %+v", r.Error)
	}
}

func TestCN04RevokeDisconnects(t *testing.T) {
	e := newEnv(t)
	sv := e.serve(Options{})
	phone, d := e.device("iPhone")
	other, _ := e.device("iPad")
	c := e.join(phone)
	keep := e.join(other)
	sub := c.request("WatchTasks", nil)
	e.newTask("t")
	pushUntil(c, sub, any2[hub.TaskView])

	if err := e.hub.RevokeDevice(ctx(t), hub.LocalOwner, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.closed(); !errors.Is(err, relay.ErrRefused) {
		t.Fatalf("revoked device: %v, want relay.ErrRefused", err)
	}
	if got := sv.gone(t, 1); got[0] != d.ID {
		t.Fatalf("cleaned up %v, want %v", got, d.ID)
	}
	if r := keep.call("Tasks", nil); r.Error != nil {
		t.Fatalf("other device: %+v", r.Error)
	}
}

// RC06、CN12：同一台設備再握手連入，送來第一則訊息時才替換舊連線；在那之前舊連線照常。
func TestRC06NewConnectionReplacesTheOld(t *testing.T) {
	e := newEnv(t)
	sv := e.serve(Options{})
	phone, d := e.device("iPhone")
	old := e.join(phone)
	sub := old.request("WatchTasks", nil)
	e.newTask("t")
	pushUntil(old, sub, any2[hub.TaskView])

	c := e.join(phone)
	if r := old.call("Tasks", nil); r.Error != nil {
		t.Fatalf("old connection before the new one's first message: %+v", r.Error)
	}
	if got := sv.s.Online(); !slices.Equal(got, []task.DeviceID{d.ID}) {
		t.Fatalf("online %v", got)
	}
	if r := c.call("Tasks", nil); r.Error != nil {
		t.Fatalf("new connection's first request: %+v", r.Error)
	}
	if err := old.closed(); !errors.Is(err, relay.ErrRefused) {
		t.Fatalf("old connection: %v, want relay.ErrRefused", err)
	}
	if got := sv.gone(t, 1); got[0] != d.ID {
		t.Fatalf("cleaned up %v", got)
	}
	if r := c.call("Tasks", nil); r.Error != nil {
		t.Fatalf("new connection: %+v", r.Error)
	}
	// 舊連線清理時沒把新連線從表裡拿掉：撤銷仍然斷開新連線。
	if err := e.hub.RevokeDevice(ctx(t), hub.LocalOwner, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.closed(); !errors.Is(err, relay.ErrRefused) {
		t.Fatalf("new connection after revoke: %v", err)
	}
}

func TestRQ01UnaryMatchesTheDesktop(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	id := e.newTask("t")
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	cases := []struct {
		method  string
		params  any
		desktop func(context.Context) (any, error)
	}{
		{"Tasks", nil, func(x context.Context) (any, error) { return e.hub.Tasks(x, hub.LocalOwner) }},
		{"Snapshot", TaskParams{Task: id}, func(x context.Context) (any, error) { return e.hub.Snapshot(x, hub.LocalOwner, id) }},
		{"Attention", nil, func(x context.Context) (any, error) { return e.hub.Attention(x, hub.LocalOwner) }},
		{"Confirmations", nil, func(x context.Context) (any, error) { return e.hub.Confirmations(x, hub.LocalOwner) }},
		{"Workspaces", struct{}{}, func(x context.Context) (any, error) { return e.hub.Workspaces(x, hub.LocalOwner) }},
	}
	for _, tc := range cases {
		want, err := tc.desktop(ctx(t))
		if err != nil {
			t.Fatalf("%s on the desktop: %v", tc.method, err)
		}
		r := c.call(tc.method, tc.params)
		if r.Error != nil || r.Push != nil || r.End {
			t.Fatalf("%s: got %+v (error %+v)", tc.method, r, r.Error)
		}
		if got, want := compact(t, r.Result), jsonOf(t, want); got != want {
			t.Errorf("%s result\n got %s\nwant %s", tc.method, got, want)
		}
	}
}

func TestRQ02HubErrorKeepsTheConnection(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	_, desktopErr := e.hub.Snapshot(ctx(t), hub.LocalOwner, "t-none")
	want, _ := transport.Wire(desktopErr)

	r := c.call("Snapshot", TaskParams{Task: "t-none"})
	wantError(t, r, string(hub.CodeUnknownTask))
	if jsonOf(t, r.Error) != jsonOf(t, want) {
		t.Fatalf("error %+v, want %+v", r.Error, want)
	}
	if r := c.call("Tasks", nil); r.Error != nil {
		t.Fatalf("after the error: %+v", r.Error)
	}
}

// RQ03（參數不對）、CN07 與 PA09（白名單外的方法）：回代碼，hub 不收到呼叫，連線不斷。
func TestRQ03RejectedRequests(t *testing.T) {
	cases := []struct {
		name   string
		method string
		params string // 原樣送出的 params；空字串是不帶參數
		code   Code
	}{
		{"unknown field", "Tasks", `{"x":1}`, CodeBadRequest},
		{"wrong type", "Snapshot", `{"task":1}`, CodeBadRequest},
		{"not an object", "Snapshot", `["t-1"]`, CodeBadRequest},
		// 參數先於 hub 被擋下：任務不存在也回 bad_request，不是 hub.unknown_task。
		{"subscription with unknown field", "Subscribe", `{"task":"t-none","x":true}`, CodeBadRequest},
		{"watch with params", "WatchTasks", `{"from":1}`, CodeBadRequest},
		{"CN07 settings", "PutSettings", `{}`, CodeNotAllowed},
		{"CN07 delete", "DeleteTask", `{"task":"t-none"}`, CodeNotAllowed},
		{"PA09 pair", "PairDevice", "", CodeNotAllowed},
		{"PA09 revoke", "RevokeDevice", "", CodeNotAllowed},
		{"PA09 reset", "RevokeDevices", "", CodeNotAllowed},
		{"PA09 device list", "Devices", "", CodeNotAllowed},
		{"no method", "", "", CodeNotAllowed},
	}
	e := newEnv(t)
	e.serve(Options{})
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	for _, tc := range cases {
		var params any
		if tc.params != "" {
			params = json.RawMessage(tc.params)
		}
		r := c.call(tc.method, params)
		if r.Error == nil || r.Error.Code != string(tc.code) || r.Result != nil || r.Push != nil || r.End {
			t.Errorf("%s: got %+v (error %+v), want %s", tc.name, r, r.Error, tc.code)
		}
	}
	if r := c.call("Tasks", nil); r.Error != nil {
		t.Fatalf("after the rejections: %+v", r.Error)
	}
}

// CN07：白名單外的方法不產生事件。
func TestCN07NotAllowedChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	id := e.newTask("t")
	e.command(id, task.CmdArchive)
	before, err := e.hub.Snapshot(ctx(t), hub.LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	wantError(t, c.call("DeleteTask", map[string]any{"task": id}), string(CodeNotAllowed))
	after, err := e.hub.Snapshot(ctx(t), hub.LocalOwner, id)
	if err != nil {
		t.Fatalf("task after DeleteTask: %v", err)
	}
	if jsonOf(t, after) != jsonOf(t, before) {
		t.Fatal("task changed")
	}
}

func TestRQ04CancelEndsTheSubscription(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	id := e.newTask("t")
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	sub := c.request("WatchTasks", nil)
	other := c.request("Subscribe", SubscribeParams{Task: id})
	pushUntil(c, sub, func(v hub.TaskView) bool { return v.ID == id })
	pushUntil(c, other, any2[hub.Update])

	c.cancel(sub)
	if r := endOf(c, sub); !r.End || r.Error != nil || r.Result != nil {
		t.Fatalf("got %+v (error %+v), want end", r, r.Error)
	}
	// 之後的變化不再推給它；連線與另一個訂閱照常。
	e.command(id, task.CmdPin)
	pushUntil(c, other, func(u hub.Update) bool { return u.Event != nil && u.Event.Kind == task.TaskPinnedKind })
	if r := c.call("Tasks", nil); r.Error != nil {
		t.Fatalf("after cancel: %+v", r.Error)
	}
	if rs := c.stash[sub]; len(rs) > 0 {
		t.Fatalf("pushed after end: %+v", rs)
	}
}

func TestRQ05DuplicateSubscriptionID(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	first := e.newTask("first")
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	sub := c.request("WatchTasks", nil)
	pushUntil(c, sub, func(v hub.TaskView) bool { return v.ID == first })

	c.sendRequest(Request{ID: sub, Method: "WatchTasks"})
	wantError(t, endOf(c, sub), string(CodeBadRequest))
	second := e.newTask("second")
	pushUntil(c, sub, func(v hub.TaskView) bool { return v.ID == second })

	// 收到 end 之後立刻重用這個 id：Mac 送出 end 之前已經放掉它。
	c.cancel(sub)
	if r := endOf(c, sub); !r.End {
		t.Fatalf("got %+v, want end", r)
	}
	c.sendRequest(Request{ID: sub, Method: "WatchTasks"})
	pushUntil(c, sub, func(v hub.TaskView) bool { return v.ID == second })
}

func TestRQ06HubEndsTheSubscription(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	id := e.newTask("t")
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	sub := c.request("Subscribe", SubscribeParams{Task: id})
	pushUntil(c, sub, any2[hub.Update])

	e.command(id, task.CmdArchive)
	if err := e.hub.DeleteTask(ctx(t), hub.LocalOwner, id); err != nil {
		t.Fatal(err)
	}
	wantError(t, endOf(c, sub), string(hub.CodeUnknownTask))
	if r := c.call("Tasks", nil); r.Error != nil {
		t.Fatalf("after the subscription ended: %+v", r.Error)
	}
}

func TestRQ07CancelUnknownIDIsIgnored(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	id := e.newTask("t")
	phone, _ := e.device("iPhone")
	c := e.join(phone)

	ended := c.request("Subscribe", SubscribeParams{Task: id})
	pushUntil(c, ended, any2[hub.Update])
	c.cancel(ended)
	if r := endOf(c, ended); !r.End {
		t.Fatalf("got %+v, want end", r)
	}
	c.cancel(ended) // 已結束
	c.cancel(99)    // 從沒用過
	// 取消在讀迴圈裡同步處理：若有回應，會先於下一個請求的回覆送出。
	call := c.request("Tasks", nil)
	if r := c.next(); r.ID != call || r.Error != nil {
		t.Fatalf("first message after the cancels: %+v", r)
	}
}

func TestRQ08SubscriptionAndCallsDoNotWait(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{})
	id := e.newTask("t")
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	sub := c.request("Subscribe", SubscribeParams{Task: id})
	pushUntil(c, sub, any2[hub.Update])

	// 訂閱在等新事件時，一般方法照樣回覆。
	if r := c.call("Snapshot", TaskParams{Task: id}); r.Error != nil {
		t.Fatalf("Snapshot: %+v", r.Error)
	}
	e.command(id, task.CmdPin)
	pushUntil(c, sub, func(u hub.Update) bool { return u.Event != nil && u.Event.Kind == task.TaskPinnedKind })
	if r := c.call("Tasks", nil); r.Error != nil {
		t.Fatalf("Tasks: %+v", r.Error)
	}
}

func TestRQ09MalformedMessageDisconnects(t *testing.T) {
	cases := []struct{ name, msg string }{
		{"not JSON", "not json"},
		{"not an object", `[1,2]`},
		{"truncated", `{"id":1,"method":"Tasks"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sv := e.serve(Options{})
			phone, d := e.device("iPhone")
			c := e.join(phone)
			c.send([]byte(tc.msg))
			if err := c.closed(); !errors.Is(err, relay.ErrRefused) {
				t.Fatalf("got %v, want relay.ErrRefused", err)
			}
			if got := sv.gone(t, 1); got[0] != d.ID {
				t.Fatalf("cleaned up %v", got)
			}
		})
	}
}

func TestRQ10HandshakeDeadline(t *testing.T) {
	e := newEnv(t)
	e.serve(Options{HandshakeTimeout: time.Second})
	phone, _ := e.device("iPhone")
	c := e.join(phone)
	c.call("Tasks", nil) // 第一則訊息在期限內到了：這條連線不受期限影響

	// 這台 App 連上中繼卻不握手：期限到了 Mac 斷開它。
	app, err := relay.DialApp(ctx(t), e.opts, e.room)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	if _, err := online(t, app).Recv(ctx(t)); !errors.Is(err, relay.ErrRefused) {
		t.Fatalf("silent app: %v, want relay.ErrRefused", err)
	}
	if r := c.call("Tasks", nil); r.Error != nil {
		t.Fatalf("other device: %+v", r.Error)
	}
}

func TestRQ11Shutdown(t *testing.T) {
	cases := []struct {
		name string
		stop func(e *env, sv *served)
		want func(t *testing.T, err error)
	}{
		{"serve context ends", func(_ *env, sv *served) { sv.stop() }, func(t *testing.T, err error) {
			if err != nil {
				t.Fatalf("Serve: %v, want nil", err)
			}
		}},
		{"hub closes", func(e *env, _ *served) { e.hub.Close() }, func(t *testing.T, err error) {
			wantCode(t, err, string(hub.CodeClosed))
		}},
		{"relay connection lost", func(e *env, _ *served) { e.mac.Close() }, func(t *testing.T, err error) {
			if err == nil {
				t.Fatal("Serve returned nil, want the relay's error")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sv := e.serve(Options{})
			id := e.newTask("t")
			p1, d1 := e.device("iPhone")
			p2, d2 := e.device("iPad")
			c1, c2 := e.join(p1), e.join(p2)
			sub := c1.request("Subscribe", SubscribeParams{Task: id})
			pushUntil(c1, sub, any2[hub.Update])
			watch := c2.request("WatchTasks", nil)
			pushUntil(c2, watch, any2[hub.TaskView])

			tc.stop(e, sv)
			// Serve 返回時每條連線都已清理完畢，它們的 goroutine 都已結束。
			tc.want(t, sv.wait(t))
			want := []task.DeviceID{d1.ID, d2.ID}
			slices.Sort(want)
			if got := sv.gone(t, 2); !slices.Equal(got, want) {
				t.Fatalf("cleaned up %v, want %v", got, want)
			}
			for _, c := range []*client{c1, c2} {
				if err := c.closed(); err == nil {
					t.Errorf("%s: closed without an error", c.device)
				}
			}
		})
	}
}

// recorder 記下 App 送出的訊息：中繼（或終結 TLS 的一方）看得到的就是這些。
type recorder struct {
	relay.Conn
	mu   sync.Mutex
	sent [][]byte
}

func (r *recorder) Send(ctx context.Context, msg []byte) error {
	r.mu.Lock()
	r.sent = append(r.sent, slices.Clone(msg))
	r.mu.Unlock()
	return r.Conn.Send(ctx, msg)
}

// joinRecorded 同 join，另回 App 送出的第一則握手訊息。
func (e *env) joinRecorded(phone relay.Keypair) (*client, []byte) {
	e.t.Helper()
	app, err := relay.DialApp(ctx(e.t), e.opts, e.room)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { app.Close() })
	rec := &recorder{Conn: online(e.t, app)}
	sess, b, err := relay.Initiate(ctx(e.t), rec, relay.Initiator{Static: phone, Mac: e.macKeys.Public})
	if err != nil {
		e.t.Fatalf("connect: %v", err)
	}
	var reply HandshakeReply
	if err := json.Unmarshal(b, &reply); err != nil || reply.Error != nil {
		e.t.Fatalf("handshake reply %q: %v", b, err)
	}
	return newClient(e.t, reply.Device, sess), rec.sent[0]
}

// replay 以一條新的 App 連線把 first 原樣送給 Mac，等 Mac 回第二則握手訊息，回這條連線。
func (e *env) replay(first []byte) *relay.Stream {
	e.t.Helper()
	app, err := relay.DialApp(ctx(e.t), e.opts, e.room)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { app.Close() })
	st := online(e.t, app)
	if err := st.Send(ctx(e.t), first); err != nil {
		e.t.Fatal(err)
	}
	if _, err := st.Recv(ctx(e.t)); err != nil {
		e.t.Fatalf("Mac did not answer the replayed handshake: %v", err)
	}
	return st
}

// CN10、CN11：重放一台設備的握手第一則訊息，Mac 照樣回第二則，但重放的一方送不出
// 解得開的訊息：不替換它連著的連線（訂閱照常推送）、不算在線、不記最後在線，期限到了
// 斷開這條連線。
func TestCN10CN11ReplayedHandshake(t *testing.T) {
	for _, connected := range []bool{true, false} {
		name := "CN11 device not connected"
		if connected {
			name = "CN10 device connected"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			var changes atomic.Int32
			sv := e.serve(Options{HandshakeTimeout: time.Second, OnlineChanged: func() { changes.Add(1) }})
			phone, d := e.device("iPhone")
			c, first := e.joinRecorded(phone)
			var sub uint64
			var want []task.DeviceID
			if connected {
				sub = c.request("WatchTasks", nil)
				if r := c.call("Tasks", nil); r.Error != nil {
					t.Fatalf("Tasks: %+v", r.Error)
				}
				want = []task.DeviceID{d.ID}
			} else {
				// 這台 App 握完手就不出聲，期限到了被斷開（CN13）；它的第一則握手訊息留給重放。
				if err := c.closed(); !errors.Is(err, relay.ErrRefused) {
					t.Fatalf("silent app: %v, want relay.ErrRefused", err)
				}
				sv.gone(t, 1)
			}
			seen := e.devices()[0].LastSeen
			before := changes.Load()
			e.clock.advance(time.Minute)

			st := e.replay(first)
			if _, err := st.Recv(ctx(t)); !errors.Is(err, relay.ErrRefused) {
				t.Fatalf("replayed connection: %v, want relay.ErrRefused", err)
			}
			if got := sv.gone(t, 1); got[0] != d.ID {
				t.Fatalf("cleaned up %v", got)
			}
			if got := sv.s.Online(); !slices.Equal(got, want) {
				t.Fatalf("online %v, want %v", got, want)
			}
			if n := changes.Load(); n != before {
				t.Fatalf("online changed %d times during the replay", n-before)
			}
			if ds := e.devices(); !ds[0].LastSeen.Equal(seen) {
				t.Fatalf("last seen %v, want %v", ds[0].LastSeen, seen)
			}
			if connected {
				e.newTask("t")
				pushUntil(c, sub, any2[hub.TaskView])
			}
		})
	}
}

// CN13：握手完成後期限內什麼都沒送：斷開，不影響其他設備，這台設備不算上線過。
func TestCN13SilentAfterHandshake(t *testing.T) {
	e := newEnv(t)
	var changes atomic.Int32
	sv := e.serve(Options{HandshakeTimeout: time.Second, OnlineChanged: func() { changes.Add(1) }})
	phone, d := e.device("iPhone")
	other, _ := e.device("iPad")
	keep := e.join(other)
	if r := keep.call("Tasks", nil); r.Error != nil {
		t.Fatalf("other device: %+v", r.Error)
	}
	before := changes.Load()

	c := e.join(phone)
	if err := c.closed(); !errors.Is(err, relay.ErrRefused) {
		t.Fatalf("silent app: %v, want relay.ErrRefused", err)
	}
	if got := sv.gone(t, 1); got[0] != d.ID {
		t.Fatalf("cleaned up %v", got)
	}
	if n := changes.Load(); n != before {
		t.Fatalf("online changed %d times", n-before)
	}
	for _, x := range e.devices() {
		if x.ID == d.ID && !x.LastSeen.IsZero() {
			t.Fatalf("last seen %v, want never", x.LastSeen)
		}
	}
	if r := keep.call("Tasks", nil); r.Error != nil {
		t.Fatalf("other device after: %+v", r.Error)
	}
}

// CN14：握手完成、第一則訊息還沒到時撤銷設備：斷開，之後送來的請求 hub 收不到。
func TestCN14RevokedBeforeTheFirstMessage(t *testing.T) {
	e := newEnv(t)
	sv := e.serve(Options{})
	id := e.newTask("t")
	before := jsonOf(t, e.snapshot(id))
	phone, d := e.device("iPhone")
	c := e.join(phone)
	if err := e.hub.RevokeDevice(ctx(t), hub.LocalOwner, d.ID); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Request{ID: 1, Method: "Command", Params: json.RawMessage(jsonOf(t, CommandParams{Task: id,
		Command: task.Command{Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "changed"}}}))})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.sess.Send(ctx(t), b) // 可能已斷開：送不送得出去都一樣
	if err := c.closed(); !errors.Is(err, relay.ErrRefused) {
		t.Fatalf("revoked app: %v, want relay.ErrRefused", err)
	}
	if got := sv.gone(t, 1); got[0] != d.ID {
		t.Fatalf("cleaned up %v", got)
	}
	if got := sv.s.Online(); len(got) != 0 {
		t.Fatalf("online %v", got)
	}
	if jsonOf(t, e.snapshot(id)) != before {
		t.Fatal("task changed")
	}
}
