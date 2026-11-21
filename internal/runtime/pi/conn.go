package pi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// maxLine 是一行協議訊息的上限（PI20）。
const maxLine = 10 << 20

// conn 是一個 Session 的 `pi --mode rpc` 行程與它的連線（hub.Conn）。
//
// stdout 由 read 一行一行讀，事件與擴充的詢問在讀取的 goroutine 上就地轉成事實，所以事實的順序就是
// pi 寫出的順序；事實進 out 這個不設上限的佇列，由 pump 依序交給 hub。寫 stdin 從不持有 mu：讀取要
// mu 才能繼續，持著 mu 寫時，pi 寫滿 stdout 停下、不再讀 stdin，兩邊就互相等住；讀取迴圈要回給 pi 的
// 訊息排進 replies，由 writeReplies 依序寫出。
type conn struct {
	proc            *proc.Process
	logger          *slog.Logger
	exitGrace       time.Duration
	setModelTimeout time.Duration
	workdir         string
	ext             string // 審批擴充的暫存檔；原生寬鬆模式時為空
	out             *outbox[task.Command]

	wmu     sync.Mutex // 一次只有一則訊息寫進 stdin
	stdin   io.Writer
	replies *outbox[map[string]any]

	eof     chan struct{} // read 結束（stdout 關閉或協議錯誤）時關閉
	ended   chan struct{} // watch 送出 exited 並關閉 out 後關閉
	pumped  chan struct{} // pump 把 out 交完後關閉
	replied chan struct{} // writeReplies 寫完 replies 後關閉
	close   sync.Once

	mu      sync.Mutex
	readErr string // read 的協議錯誤；eof 關閉前寫入
	final   task.Problem
	session string
	reqSeq  int
	waiting map[string]chan reply // OrielHub 發出、等著回應的命令，以 id 為鍵
	// turn 在送出 prompt 前為真，這一輪結束時為假；promptKey 是還沒回應的 prompt 的 id；cancelling 是
	// 這一輪已要求中斷；abortPending 是 prompt 還沒回應就要求了中斷：回應一到就送 abort（PI40）。
	turn         bool
	promptKey    string
	cancelling   bool
	abortPending bool
	tools        map[string]*toolState
	perms        map[task.PermissionID]string // 權限 ID → pi 的擴充 UI 請求 id
	permPrefix   string
	permSeq      int
	// used 是這一輪累計的用量；lastStop、lastError 是最後一條 assistant 訊息的 stopReason 與
	// errorMessage，retryError 是自動重試最後失敗的原因（PI16、PI17）。
	used                            task.Usage
	lastStop, lastError, retryError string
	model                           *task.ModelSetting // 目前的模型設置；pi 沒有可選的模型時為 nil
}

// toolState 是一個工具呼叫目前的樣子。
type toolState struct {
	name    string
	kind    task.ToolKind
	title   string
	command string
	paths   []string
	running bool // 已報過「執行中」（PI15）
}

// reply 是 pi 對 OrielHub 的命令的回應。
type reply struct {
	ok   bool
	data json.RawMessage
	err  string
}

func (r *Runtime) open(p *proc.Process, emit func(task.Command), workdir, ext string) *conn {
	c := &conn{proc: p, logger: r.logger, exitGrace: r.exitGrace, setModelTimeout: r.setModelTimeout,
		workdir: workdir, ext: ext, out: newOutbox[task.Command](), replies: newOutbox[map[string]any](), stdin: p.Stdin(),
		eof: make(chan struct{}), ended: make(chan struct{}), pumped: make(chan struct{}), replied: make(chan struct{}),
		waiting: map[string]chan reply{}, tools: map[string]*toolState{}, perms: map[task.PermissionID]string{},
		permPrefix: newPrefix()}
	go c.pump(emit)
	go c.writeReplies()
	go c.read(p.Stdout())
	go c.watch()
	return c
}

// newPrefix 讓權限 ID 在 Session 內唯一：恢復後是新的行程、新的前綴（PI31）。
func newPrefix() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "p" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])) + "-"
}

func (c *conn) NativeID() task.NativeSessionID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return task.NativeSessionID(c.session)
}

// bind 記下會話 ID，報出可選的模型（有的話，PI06）與 native_bound（PI01、PI02）。模型設置在綁定之前
// 報：hub 在連線登記時依序處理，Plan 看到連線時已知道 agent 是什麼模型（D118）。
func (c *conn) bind(h handshakeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = h.session
	if setting := c.setModels(h.models, h.current); setting != nil {
		c.out.push(task.Command{Kind: task.FactModelReported,
			ModelReported: &task.ModelReport{Setting: setting, Start: true}})
	}
	c.out.push(task.Command{Kind: task.FactNativeBound, Bound: &task.NativeBound{NativeID: task.NativeSessionID(h.session)}})
}

// nextKey 是下一個命令的 id（依序編號 oriel-1、oriel-2……，回放的夾具才對得上）。呼叫者持有 c.mu。
func (c *conn) nextKey() string {
	c.reqSeq++
	return "oriel-" + strconv.Itoa(c.reqSeq)
}

// command 是一則帶 id 的命令。
func command(key, typ string, fields map[string]any) map[string]any {
	m := map[string]any{"id": key, "type": typ}
	for k, v := range fields {
		m[k] = v
	}
	return m
}

// request 發出一個命令並等回應；回 success: false 是 pi.request_failed（PI05）。
func (c *conn) request(ctx context.Context, typ string, fields map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	key := c.nextKey()
	ch := make(chan reply, 1)
	c.waiting[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, key)
		c.mu.Unlock()
	}()
	if err := c.write(command(key, typ, fields)); err != nil {
		if c.gone(ctx) {
			return nil, errDisconnected
		}
		return nil, fail(CodeRequestFailed, err, "command", typ, "detail", err.Error())
	}
	answered := func(r reply) (json.RawMessage, error) {
		if !r.ok {
			return nil, fail(CodeRequestFailed, nil, "command", typ, "detail", r.err)
		}
		return r.data, nil
	}
	select {
	case r := <-ch:
		return answered(r)
	case <-c.eof:
		select { // 回應先到、stdout 隨即結束：回應算數
		case r := <-ch:
			return answered(r)
		default:
			return nil, errDisconnected
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// write 把一則訊息寫進 stdin。呼叫者不能持有 c.mu。
func (c *conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

// writeReplies 依序寫出讀取迴圈排進 replies 的訊息；寫不進去時記下（行程多半已走，由 exited 回報）。
// watch 關閉 replies 後結束。
func (c *conn) writeReplies() {
	defer close(c.replied)
	for {
		items, closed := c.replies.take()
		for _, x := range items {
			if err := c.write(x); err != nil {
				c.logger.Debug("pi: could not answer", "err", err)
			}
		}
		if closed && len(items) == 0 {
			return
		}
	}
}

// answerUI 排一則對擴充 UI 請求的回答（confirmed 或 cancelled）。
func (c *conn) answerUI(id string, field string, value bool) {
	c.replies.push(map[string]any{"type": "extension_ui_response", "id": id, field: value})
}

// gone 回報 pi 是否已斷線：stdout 已結束，或在兩倍 exitGrace 內結束（寫入失敗可能早於 stdout 的
// EOF）。ctx 已結束時不等。呼叫者不能持有 c.mu。
func (c *conn) gone(ctx context.Context) bool {
	select {
	case <-c.eof:
		return true
	default:
	}
	if ctx.Err() != nil {
		return false
	}
	t := time.NewTimer(2 * c.exitGrace)
	defer t.Stop()
	select {
	case <-c.eof:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

// read 讀 stdout 的每一行（只以 LF 分行：JSON 字串裡的 U+2028/U+2029 不是分行），直到 EOF 或一行
// 超過 maxLine。
func (c *conn) read(out io.Reader) {
	br := bufio.NewReaderSize(out, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			c.route(line)
		}
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				c.mu.Lock()
				c.readErr = "a line longer than 10 MiB"
				c.mu.Unlock()
			}
			break
		}
	}
	c.mu.Lock()
	close(c.eof)
	c.mu.Unlock()
	c.proc.CloseStdout()
}

var errLineTooLong = errors.New("line too long")

func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(line)+len(frag) > maxLine {
			return nil, errLineTooLong
		}
		line = append(line, frag...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// route 分辨回應、擴充的 UI 請求與事件；不是 JSON 的行跳過，日誌只記長度。
func (c *conn) route(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		c.logger.Debug("pi: skipped a line that is not JSON", "bytes", len(line))
		return
	}
	var err error
	switch head.Type {
	case "response":
		err = c.response(line)
	case "extension_ui_request":
		err = c.uiRequest(line)
	default:
		err = c.event(head.Type, line)
	}
	if err != nil {
		c.logger.Debug("pi: skipped a record OrielHub could not read", "type", head.Type, "bytes", len(line))
	}
}

// response 把回應交給等著它的命令；prompt 的回應就地處理（PI10）。
func (c *conn) response(line []byte) error {
	var r struct {
		ID      string          `json:"id"`
		Command string          `json:"command"`
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   string          `json:"error"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return err
	}
	c.mu.Lock()
	if r.ID != "" && r.ID == c.promptKey {
		defer c.mu.Unlock()
		var data struct {
			Disposition string `json:"disposition"`
		}
		_ = json.Unmarshal(r.Data, &data)
		c.prompted(r.Success, r.Error, data.Disposition)
		return nil
	}
	ch := c.waiting[r.ID]
	delete(c.waiting, r.ID)
	c.mu.Unlock()
	switch {
	case ch != nil:
		ch <- reply{ok: r.Success, data: r.Data, err: r.Error}
	case !r.Success:
		c.logger.Debug("pi: a command failed", "command", r.Command)
	}
	return nil
}

// watch 在 stdout 關閉後決定結束原因，送出 exited（PI20）。行程結束了、stdout 卻還被離開了行程群組
// 的子孫開著時，寬限期後關掉 stdout。
func (c *conn) watch() {
	defer close(c.ended)
	select {
	case <-c.eof:
	case <-c.proc.Done():
		t := time.NewTimer(c.exitGrace)
		select {
		case <-c.eof:
		case <-t.C:
			c.proc.CloseStdout()
			<-c.eof
		}
		t.Stop()
	}
	c.mu.Lock()
	readErr := c.readErr
	c.mu.Unlock()
	var p task.Problem
	if readErr != "" {
		c.proc.Stop()
		p = protocolError(readErr)
	} else {
		t := time.NewTimer(c.exitGrace)
		select {
		case <-c.proc.Done():
			p = problemOf(c.proc.Exit())
		case <-t.C:
			c.proc.Stop()
			p = protocolError("stdout closed while pi kept running")
		}
		t.Stop()
	}
	c.mu.Lock()
	c.final = p
	c.turn, c.promptKey = false, ""
	c.perms = map[task.PermissionID]string{}
	c.tools = map[string]*toolState{}
	c.mu.Unlock()
	c.replies.close()
	c.out.push(task.Command{Kind: task.FactExited,
		Exited: &task.Exited{Problem: p, StderrTail: c.proc.StderrTail()}})
	c.out.close()
}

func protocolError(detail string) task.Problem {
	return task.Problem{Code: string(CodeProtocolError), Params: map[string]string{"detail": detail}}
}

func problemOf(e *proc.Error) task.Problem {
	return task.Problem{Code: e.ErrorCode(), Params: e.ErrorParams()}
}

// pump 把 out 依序交給 hub；out 關閉且交完後結束。
func (c *conn) pump(emit func(task.Command)) {
	defer close(c.pumped)
	for {
		items, closed := c.out.take()
		for _, x := range items {
			emit(x)
		}
		if closed && len(items) == 0 {
			return
		}
	}
}

// Prompt 寫出 prompt 就返回，不等回應（PI10）：hub 在它返回後才把這一輪記為已送出，之後的中斷才會
// 送給 pi，所以不能讓它等一次往返。回應由讀取迴圈處理（prompted）；agent_settled 到了才報 turn_ended。
// 還不支援的附件只讓這一輪失敗（PI12）。
func (c *conn) Prompt(ctx context.Context, p task.Prompt) error {
	msg, berr := c.message(p)
	c.mu.Lock()
	select {
	case <-c.eof:
		c.mu.Unlock()
		return nil // 行程已結束：由 exited 回報原因
	default:
	}
	if c.turn {
		c.mu.Unlock()
		return fail(CodeTurnFailed, nil, "detail", "a turn is already running")
	}
	if berr != nil {
		c.out.push(turnEnded(failed(berr)))
		c.mu.Unlock()
		return nil
	}
	c.turn, c.cancelling, c.abortPending = true, false, false
	c.used, c.lastStop, c.lastError, c.retryError = task.Usage{}, "", "", ""
	key := c.nextKey()
	c.promptKey = key
	c.mu.Unlock()
	if err := c.write(command(key, "prompt", map[string]any{"message": msg})); err != nil {
		c.mu.Lock()
		if c.promptKey == key {
			c.turn, c.promptKey = false, ""
		}
		c.mu.Unlock()
		if c.gone(ctx) {
			return nil // 寫入失敗多半是行程走了：由 exited 回報
		}
		return fail(CodeRequestFailed, err, "command", "prompt", "detail", err.Error())
	}
	return nil
}

// prompted 處理 prompt 的回應（PI10）：被拒時這一輪失敗；擴充命令吃掉（handled）時不會開始一輪，就此
// 結束；開始了而之前要求過中斷時隨即送 abort（PI40）。呼叫者持有 c.mu。
func (c *conn) prompted(ok bool, detail, disposition string) {
	c.promptKey = ""
	if !c.turn {
		return
	}
	switch {
	case !ok && !c.cancelling:
		e := fail(CodeRequestFailed, nil, "command", "prompt", "detail", detail)
		if strings.HasPrefix(detail, "No API key") {
			e = fail(CodeAuthRequired, nil)
		}
		c.endTurn(failed(e))
	case !ok || disposition == "handled":
		c.endTurn(c.outcome(task.TurnEnded{Result: task.TurnOK}))
	case c.abortPending:
		c.abortPending = false
		c.replies.push(command(c.nextKey(), "abort", nil))
	}
}

func turnEnded(t task.TurnEnded) task.Command {
	return task.Command{Kind: task.FactTurnEnded, TurnEnded: &t}
}

func failed(e *Error) task.TurnEnded {
	return task.TurnEnded{Result: task.TurnError, Problem: &task.Problem{Code: e.ErrorCode(), Params: e.ErrorParams()}}
}

// Cancel 先把掛著的詢問回成不允許，再送 abort，不等回應（PI40、PI41）；結局由 agent_settled 報告。
// prompt 還沒回應時，等回應到了再送（prompted）。寫不進去而行程還在時回錯誤，還沒回掉的詢問放回去
// （hub 仍可回答），下一次 Cancel 照樣再送。
func (c *conn) Cancel(ctx context.Context) error {
	c.mu.Lock()
	if !c.turn || c.cancelling {
		c.mu.Unlock()
		return nil
	}
	c.cancelling = true
	pending := c.takePerms()
	var key string
	if c.promptKey != "" {
		c.abortPending = true
	} else {
		key = c.nextKey()
	}
	c.mu.Unlock()
	for i, p := range pending {
		if err := c.write(map[string]any{"type": "extension_ui_response", "id": p.ui, "confirmed": false}); err != nil {
			return c.cancelFailed(ctx, err, pending[i:])
		}
	}
	if key == "" {
		return nil
	}
	if err := c.write(command(key, "abort", nil)); err != nil {
		return c.cancelFailed(ctx, err, nil)
	}
	return nil
}

// pendingAsk 是一個掛著的詢問：權限 ID 與 pi 的擴充 UI 請求 id。
type pendingAsk struct {
	id task.PermissionID
	ui string
}

// takePerms 取走掛著的詢問，依權限 ID 排列（寫出的順序固定）。呼叫者持有 c.mu。
func (c *conn) takePerms() []pendingAsk {
	out := make([]pendingAsk, 0, len(c.perms))
	for _, k := range slices.Sorted(maps.Keys(c.perms)) {
		out = append(out, pendingAsk{id: k, ui: c.perms[k]})
	}
	c.perms = map[task.PermissionID]string{}
	return out
}

// cancelFailed：寫入失敗多半是行程走了（由 exited 回報）；否則還原中斷的狀態，把還沒回掉的詢問放回去
// （這一輪仍在時），回 pi.request_failed。
func (c *conn) cancelFailed(ctx context.Context, err error, unanswered []pendingAsk) error {
	if c.gone(ctx) {
		return nil
	}
	c.mu.Lock()
	c.cancelling, c.abortPending = false, false
	if c.turn {
		for _, p := range unanswered {
			c.perms[p.id] = p.ui
		}
	}
	c.mu.Unlock()
	return fail(CodeRequestFailed, err, "command", "abort", "detail", err.Error())
}

// Answer 回答掛著的詢問（PI32）：允許是 confirmed: true，拒絕是 false。這一輪已結束或已在 Cancel 時
// 回掉的，回 nil，不送。
func (c *conn) Answer(ctx context.Context, id task.PermissionID, v task.Verdict) error {
	c.mu.Lock()
	uid, ok := c.perms[id]
	delete(c.perms, id)
	c.mu.Unlock()
	if !ok {
		return nil
	}
	// 直接寫，不經 replies：寫不進去而行程還在時要回報，hub 不能以為 pi 收到了。
	if err := c.write(map[string]any{"type": "extension_ui_response", "id": uid, "confirmed": v == task.Allow}); err != nil {
		if c.gone(ctx) {
			return nil
		}
		return fail(CodeRequestFailed, err, "command", "extension_ui_response", "detail", err.Error())
	}
	return nil
}

// SetModel 送 set_model{provider, modelId}（值按第一個 "/" 拆開），成功時回傳目前的值換成它的設置
// （PI07，D118）；時限內沒回是 pi.set_model_timed_out，之後才到的回應丟掉。
func (c *conn) SetModel(ctx context.Context, m task.ModelSwitch) (*task.ModelSetting, error) {
	provider, id, ok := strings.Cut(m.Value, "/")
	if !ok || provider == "" || id == "" {
		return nil, fail(CodeRequestFailed, nil, "command", "set_model", "detail", m.Value+" is not a provider/model value")
	}
	sctx, cancel := context.WithTimeout(ctx, c.setModelTimeout)
	defer cancel()
	if _, err := c.request(sctx, "set_model", map[string]any{"provider": provider, "modelId": id}); err != nil {
		if ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
			return nil, fail(CodeSetModelTimedOut, err)
		}
		if errors.Is(err, errDisconnected) {
			return nil, fail(CodeRequestFailed, err, "command", "set_model", "detail", "pi exited")
		}
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.model == nil {
		return &task.ModelSetting{Option: modelOption, Current: m.Value}, nil
	}
	c.model.Current = m.Value
	return cloneSetting(c.model), nil
}

// Close 結束行程、刪掉審批擴充的暫存檔，並等讀取、交付與回覆都結束（PI42）；可以重複呼叫。
func (c *conn) Close() error {
	c.close.Do(func() {
		c.proc.Stop()
		removeFile(c.ext)
	})
	<-c.ended
	<-c.pumped
	<-c.replied
	return nil
}

// outbox 是不設上限、依序的佇列：事實交給 hub，回覆寫給 pi。
type outbox[T any] struct {
	mu     sync.Mutex
	items  []T
	closed bool
	wake   chan struct{} // 容量 1
}

func newOutbox[T any]() *outbox[T] { return &outbox[T]{wake: make(chan struct{}, 1)} }

func (b *outbox[T]) push(c T) {
	b.mu.Lock()
	if !b.closed {
		b.items = append(b.items, c)
	}
	b.mu.Unlock()
	b.signal()
}

func (b *outbox[T]) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.signal()
}

func (b *outbox[T]) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// take 取走目前的全部項目；沒有也沒關閉時等。
func (b *outbox[T]) take() ([]T, bool) {
	for {
		b.mu.Lock()
		items, closed := b.items, b.closed
		b.items = nil
		b.mu.Unlock()
		if len(items) > 0 || closed {
			return items, closed
		}
		<-b.wake
	}
}
