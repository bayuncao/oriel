package codex

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// maxLine 是一行協議訊息的上限（CX21）。
const maxLine = 10 << 20

// conn 是一個 Session 的 `codex app-server` 行程與它的連線（hub.Conn）。
//
// stdout 由 read 一行一行讀，通知與 Codex 發起的請求在讀取的 goroutine 上就地轉成事實，所以事實的
// 順序就是 Codex 寫出的順序；事實進 out 這個不設上限的佇列，由 pump 依序交給 hub。寫 stdin 從不
// 持有 mu：讀取要 mu 才能繼續，持著 mu 寫時，Codex 寫滿 stdout 停下、不再讀 stdin，兩邊就互相等住；
// 讀取迴圈要回給 Codex 的訊息排進 replies，由 writeReplies 依序寫出。
type conn struct {
	proc      *proc.Process
	logger    *slog.Logger
	version   string
	exitGrace time.Duration
	workdir   string
	out       *outbox[task.Command]

	wmu     sync.Mutex // 一次只有一則訊息寫進 stdin
	stdin   io.Writer
	replies *outbox[map[string]any]

	eof     chan struct{} // read 結束（stdout 關閉或協議錯誤）時關閉
	ended   chan struct{} // watch 送出 exited 並關閉 out 後關閉
	pumped  chan struct{} // pump 把 out 交完後關閉
	replied chan struct{} // writeReplies 寫完 replies 後關閉
	close   sync.Once
	turns   sync.WaitGroup // 等 turn/start 回應的 goroutine；Close 等它們

	mu      sync.Mutex
	readErr string // read 的協議錯誤；eof 關閉前寫入
	final   task.Problem
	thread  string
	reqSeq  int
	waiting map[string]chan rpcReply // OrielHub 發出、等著回應的請求，以 id 的 JSON 為鍵
	// turn 在送出 turn/start 前為真，turn/completed 到了為假；turnID 是 turn/start 回的 ID（中斷要用）；
	// cancelling 是這一輪已要求中斷。
	turn       bool
	turnID     string
	cancelling bool
	// interruptPending 是這一輪的 ID 還沒到就要求了中斷：ID 一到就送（CX40）。
	interruptPending bool
	// turnSeq 每送一輪加一：晚到的上一輪 turn/start 回應不會記到這一輪上。
	turnSeq    int
	streamed   map[string]bool // 這一輪以增量送過文字的條目 ID（CX13）
	summarized map[string]bool // 這一輪送過推理摘要的條目 ID：同一條推理不再送原始文字
	tools      map[string]*toolState
	perms      map[task.PermissionID]*pendingPerm
	permOf     map[string]task.PermissionID // Codex 的請求 id（JSON）→ 權限 ID
	permPrefix string
	permSeq    int
	// total 是 Codex 報的整個會話的累計用量，start 是這一輪開始時的（CX18）。
	total, start tokens
	model        *task.ModelSetting // 目前的模型設置；Codex 沒報可選的模型時為 nil
	nextModel    string             // SetModel 記下的模型，下一輪的 turn/start 帶上（CX07）
}

// toolState 是一個工具呼叫目前的樣子。
type toolState struct {
	name  string
	kind  task.ToolKind
	title string
	paths []string
}

// pendingPerm 是一個掛著等回答的審批請求。
type pendingPerm struct {
	id json.RawMessage // Codex 的請求 id，原樣回
}

// rpcReply 是 Codex 對 OrielHub 的請求的回應。
type rpcReply struct {
	result json.RawMessage
	err    *rpcError
}

// rpcError 是 JSON-RPC 的錯誤。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (r *Runtime) open(p *proc.Process, emit func(task.Command), workdir string) *conn {
	c := &conn{proc: p, logger: r.logger, version: r.version, exitGrace: r.exitGrace, workdir: workdir,
		out: newOutbox[task.Command](), replies: newOutbox[map[string]any](), stdin: p.Stdin(),
		eof: make(chan struct{}), ended: make(chan struct{}), pumped: make(chan struct{}), replied: make(chan struct{}),
		waiting: map[string]chan rpcReply{}, streamed: map[string]bool{}, summarized: map[string]bool{}, tools: map[string]*toolState{},
		perms: map[task.PermissionID]*pendingPerm{}, permOf: map[string]task.PermissionID{}, permPrefix: newPrefix()}
	go c.pump(emit)
	go c.writeReplies()
	go c.read(p.Stdout())
	go c.watch()
	return c
}

// newPrefix 讓權限 ID 在 Session 內唯一：恢復後是新的行程、新的前綴（CX30）。
func newPrefix() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "p" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])) + "-"
}

func (c *conn) NativeID() task.NativeSessionID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return task.NativeSessionID(c.thread)
}

// bind 記下會話 ID，報出可選的模型（有的話，CX06）與 native_bound（CX01、CX02）。模型設置在綁定
// 之前報：hub 在連線登記時依序處理，Plan 看到連線時已知道 agent 是什麼模型（D118）。
func (c *conn) bind(thread string, models []model, current string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.thread = thread
	if setting := c.setModels(models, current); setting != nil {
		c.out.push(task.Command{Kind: task.FactModelReported,
			ModelReported: &task.ModelReport{Setting: setting, Start: true}})
	}
	c.out.push(task.Command{Kind: task.FactNativeBound, Bound: &task.NativeBound{NativeID: task.NativeSessionID(thread)}})
}

// nextID 是下一個請求的 id（依序編號，回放的夾具才對得上）。呼叫者持有 c.mu。
func (c *conn) nextID() int {
	c.reqSeq++
	return c.reqSeq
}

// request 發出一個請求並等回應；回了錯誤是 codex.request_failed（CX05）。
func (c *conn) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID()
	key := strconv.Itoa(id)
	ch := make(chan rpcReply, 1)
	c.waiting[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, key)
		c.mu.Unlock()
	}()
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, c.requestFailed(ctx, method, err)
	}
	answered := func(r rpcReply) (json.RawMessage, error) {
		if r.err != nil {
			return nil, fail(CodeRequestFailed, nil, "method", method, "detail", r.err.Message)
		}
		return r.result, nil
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

// requestFailed：寫不進去時行程多半已走（errDisconnected，由 exited 回報）；否則是這個請求失敗。
func (c *conn) requestFailed(ctx context.Context, method string, err error) error {
	if c.gone(ctx) {
		return errDisconnected
	}
	return fail(CodeRequestFailed, err, "method", method, "detail", err.Error())
}

// notify 發出一則通知（沒有 id、不等回應）。
func (c *conn) notify(method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return c.write(m)
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
				c.logger.Debug("codex: could not answer a request", "err", err)
			}
		}
		if closed && len(items) == 0 {
			return
		}
	}
}

// reply 排一則對 Codex 的請求 id 的回應（result 或 error 擇一）。
func (c *conn) reply(id json.RawMessage, result any, rerr *rpcError) {
	m := map[string]any{"id": id}
	if rerr != nil {
		m["error"] = rerr
	} else {
		m["result"] = result
	}
	c.replies.push(m)
}

// gone 回報 Codex 是否已斷線：stdout 已結束，或在兩倍 exitGrace 內結束（寫入失敗可能早於 stdout
// 的 EOF）。ctx 已結束時不等。呼叫者不能持有 c.mu。
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

// read 讀 stdout 的每一行，直到 EOF 或一行超過 maxLine。
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

// route 分辨回應、Codex 發起的請求與通知；不是 JSON 的行跳過，日誌只記長度。
func (c *conn) route(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		c.logger.Debug("codex: skipped a line that is not JSON", "bytes", len(line))
		return
	}
	switch {
	case m.ID != nil && m.Method == "":
		c.mu.Lock()
		ch := c.waiting[string(m.ID)]
		delete(c.waiting, string(m.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- rpcReply{result: m.Result, err: m.Error}
		}
	case m.ID != nil:
		c.serverRequest(m.ID, m.Method, m.Params)
	default:
		if err := c.notification(m.Method, m.Params); err != nil {
			c.logger.Debug("codex: skipped a notification OrielHub could not read", "method", m.Method, "bytes", len(line))
		}
	}
}

// watch 在 stdout 關閉後決定結束原因，送出 exited（CX21）。行程結束了、stdout 卻還被離開了行程群組
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
			p = protocolError("stdout closed while Codex kept running")
		}
		t.Stop()
	}
	c.mu.Lock()
	c.final = p
	c.turn = false
	c.perms, c.permOf = map[task.PermissionID]*pendingPerm{}, map[string]task.PermissionID{}
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

// Prompt 寫出 turn/start 就返回，不等回應（CX10）：hub 在它返回後才把這一輪記為已送出，之後的中斷
// 才會送給 Codex，所以不能讓它等一次往返。這一輪的 ID 從回應或 turn/started 得知；回應是錯誤時這一輪
// 失敗。輸出經由事實抵達，turn/completed 到了才報 turn_ended。還不支援的附件只讓這一輪失敗（CX12）。
func (c *conn) Prompt(ctx context.Context, p task.Prompt) error {
	input, berr := c.input(p)
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
	c.turn, c.turnID, c.cancelling, c.interruptPending = true, "", false, false
	c.turnSeq++
	seq := c.turnSeq
	c.streamed, c.summarized, c.start = map[string]bool{}, map[string]bool{}, c.total
	params := map[string]any{"threadId": c.thread, "input": input}
	if c.nextModel != "" {
		params["model"] = c.nextModel
	}
	id := c.nextID()
	key := strconv.Itoa(id)
	ch := make(chan rpcReply, 1)
	c.waiting[key] = ch
	c.turns.Add(1)
	c.mu.Unlock()
	if err := c.write(map[string]any{"id": id, "method": "turn/start", "params": params}); err != nil {
		c.mu.Lock()
		delete(c.waiting, key)
		c.turn = false
		c.mu.Unlock()
		c.turns.Done()
		if c.gone(ctx) {
			return nil // 寫入失敗多半是行程走了：由 exited 回報
		}
		return fail(CodeRequestFailed, err, "method", "turn/start", "detail", err.Error())
	}
	go c.awaitTurn(key, ch, seq)
	return nil
}

// awaitTurn 等 turn/start 的回應：錯誤時這一輪失敗；成功時記下這一輪的 ID（若 turn/started 還沒帶來），
// 要求過的中斷隨即送出。
func (c *conn) awaitTurn(key string, ch chan rpcReply, seq int) {
	defer c.turns.Done()
	var r rpcReply
	select {
	case r = <-ch:
	case <-c.eof:
		select {
		case r = <-ch:
		default:
			c.mu.Lock()
			delete(c.waiting, key)
			c.mu.Unlock()
			return // 行程走了：由 exited 回報
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if seq != c.turnSeq || !c.turn {
		return // 這一輪已結束（之後可能已送了下一輪）：回應不再算數
	}
	if r.err != nil {
		c.turn = false
		c.out.push(turnEnded(failed(fail(CodeRequestFailed, nil, "method", "turn/start", "detail", r.err.Message))))
		return
	}
	c.nextModel = ""
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(r.result, &res) == nil {
		c.setTurnID(res.Turn.ID)
	}
}

// setTurnID 記下這一輪的 ID；ID 到之前要求過中斷時，排進 replies 送出（讀取迴圈也會呼叫它，不能直接寫）。
// 呼叫者持有 c.mu。
func (c *conn) setTurnID(id string) {
	if !c.turn || c.turnID != "" || id == "" {
		return
	}
	c.turnID = id
	if c.cancelling && c.interruptPending {
		c.interruptPending = false
		c.replies.push(map[string]any{"id": c.nextID(), "method": "turn/interrupt",
			"params": map[string]any{"threadId": c.thread, "turnId": id}})
	}
}

func turnEnded(t task.TurnEnded) task.Command {
	return task.Command{Kind: task.FactTurnEnded, TurnEnded: &t}
}

func failed(e *Error) task.TurnEnded {
	return task.TurnEnded{Result: task.TurnError, Problem: &task.Problem{Code: e.ErrorCode(), Params: e.ErrorParams()}}
}

// Cancel 送 turn/interrupt；結局由 turn/completed 報告（CX40、CX41）。掛著的審批由 Codex 在中斷時
// 清掉（serverRequest/resolved）。寫不進去而行程還在時回錯誤，下一次 Cancel 照樣再送。
func (c *conn) Cancel(ctx context.Context) error {
	c.mu.Lock()
	if !c.turn || c.cancelling {
		c.mu.Unlock()
		return nil
	}
	c.cancelling = true
	if c.turnID == "" { // 這一輪的 ID 還沒到：到了就送（setTurnID）
		c.interruptPending = true
		c.mu.Unlock()
		return nil
	}
	id := c.nextID()
	params := map[string]any{"threadId": c.thread, "turnId": c.turnID}
	c.mu.Unlock()
	err := c.write(map[string]any{"id": id, "method": "turn/interrupt", "params": params})
	if err == nil {
		return nil
	}
	if c.gone(ctx) {
		return nil
	}
	c.mu.Lock()
	c.cancelling = false
	c.mu.Unlock()
	return fail(CodeRequestFailed, err, "method", "turn/interrupt", "detail", err.Error())
}

// Answer 回答掛著的審批（CX32）：允許 accept、拒絕 decline，不回 acceptForSession，總是允許由
// OrielHub 的規則負責。Codex 已清掉或這一輪已結束時回 nil，不送（CX33）。
func (c *conn) Answer(ctx context.Context, id task.PermissionID, v task.Verdict) error {
	c.mu.Lock()
	p := c.perms[id]
	delete(c.perms, id)
	if p != nil {
		delete(c.permOf, string(p.id))
	}
	c.mu.Unlock()
	if p == nil {
		return nil
	}
	decision := "decline"
	if v == task.Allow {
		decision = "accept"
	}
	// 直接寫，不經 replies：寫不進去而行程還在時要回報，hub 不能以為 Codex 收到了。
	if err := c.write(map[string]any{"id": p.id, "result": map[string]any{"decision": decision}}); err != nil {
		if c.gone(ctx) {
			return nil
		}
		return fail(CodeRequestFailed, err, "method", "approval", "detail", err.Error())
	}
	return nil
}

// SetModel 記下要換的模型，下一輪的 turn/start 帶上它（Codex 的說明：覆蓋這一輪與之後各輪），立即
// 回傳換完之後的設置（CX07，D118）；不另發請求。
func (c *conn) SetModel(_ context.Context, m task.ModelSwitch) (*task.ModelSetting, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextModel = m.Value
	if c.model == nil {
		return &task.ModelSetting{Option: modelOption, Current: m.Value}, nil
	}
	c.model.Current = m.Value
	return cloneSetting(c.model), nil
}

// Close 結束行程並等讀取、交付與回覆都結束（CX42）；可以重複呼叫。
func (c *conn) Close() error {
	c.close.Do(c.proc.Stop)
	<-c.ended
	<-c.pumped
	<-c.replied
	c.turns.Wait()
	return nil
}

// outbox 是不設上限、依序的佇列：事實交給 hub，回覆寫給 Codex。
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
