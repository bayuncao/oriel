package claude

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

const (
	// maxLine 是一行協議訊息的上限（CL23）。
	maxLine = 10 << 20
	// denied 是 OrielHub 拒絕工具呼叫時給 Claude 的說明（CL32）。
	denied = "Denied in OrielHub"
)

// conn 是一個 Session 的 claude 行程與它的連線（hub.Conn）。
//
// stdout 由 read 一行一行讀，每則訊息在讀取的 goroutine 上就地轉成事實，所以事實的順序就是
// Claude 寫出的順序。事實進 out 這個不設上限的佇列，由 pump 依序交給 hub：hub 處理得慢時，
// 讀取不會停下來。寫 stdin 從不持有 mu：讀取迴圈要 mu 才能繼續讀，持著 mu 寫時，Claude 寫滿
// stdout 停下來、不再讀 stdin，兩邊就互相等住了。
type conn struct {
	proc            *proc.Process
	logger          *slog.Logger
	setModelTimeout time.Duration
	exitGrace       time.Duration
	workdir         string
	sid             task.NativeSessionID
	out             *outbox[task.Command]

	wmu   sync.Mutex // 一次只有一則訊息寫進 stdin
	stdin io.Writer
	// replies 是讀取迴圈要回給 Claude 的訊息（拒絕、錯誤），由 writeReplies 依序寫出：讀取迴圈自己
	// 不能寫，理由見上。
	replies *outbox[map[string]any]

	eof     chan struct{} // read 結束（stdout 關閉或協議錯誤）時關閉
	ended   chan struct{} // watch 送出 exited 並關閉 out 後關閉
	pumped  chan struct{} // pump 把 out 交完後關閉
	replied chan struct{} // writeReplies 寫完 replies 後關閉
	close   sync.Once

	mu      sync.Mutex
	readErr string // read 的協議錯誤；eof 關閉前寫入
	final   task.Problem
	reqSeq  int
	waiting map[string]chan reply // OrielHub 發出、等著回應的控制請求
	// turn 在送出 user 訊息後為真，result 到了為假；cancelling 是這一輪已要求中斷。
	turn       bool
	cancelling bool
	streamed   map[string]bool // 這一輪以 stream_event 送過文字的訊息 ID（CL13）
	tools      map[string]*toolState
	perms      map[task.PermissionID]*pendingPerm
	permOf     map[string]task.PermissionID // Claude 的 request_id → 權限 ID
	permPrefix string
	permSeq    int
	choices    []choice
	model      *task.ModelSetting // 目前的模型設置；Claude 沒報可選的模型時為 nil
}

// toolState 是一個工具呼叫目前的樣子。
type toolState struct {
	name  string
	kind  task.ToolKind
	title string
	input any
	paths []string
}

// pendingPerm 是一個掛著等回答的 can_use_tool。
type pendingPerm struct {
	request string // Claude 的 request_id
	input   json.RawMessage
}

// reply 是 Claude 對 OrielHub 的控制請求的回應。
type reply struct {
	ok       bool
	response json.RawMessage
	err      string
}

func (r *Runtime) open(p *proc.Process, emit func(task.Command), sid task.NativeSessionID, workdir string) *conn {
	c := &conn{proc: p, logger: r.logger, setModelTimeout: r.setModelTimeout, exitGrace: r.exitGrace, workdir: workdir, sid: sid,
		out: newOutbox[task.Command](), replies: newOutbox[map[string]any](), stdin: p.Stdin(),
		eof: make(chan struct{}), ended: make(chan struct{}), pumped: make(chan struct{}), replied: make(chan struct{}),
		waiting: map[string]chan reply{}, streamed: map[string]bool{}, tools: map[string]*toolState{},
		perms: map[task.PermissionID]*pendingPerm{}, permOf: map[string]task.PermissionID{}, permPrefix: newPrefix()}
	go c.pump(emit)
	go c.writeReplies()
	go c.read(p.Stdout())
	go c.watch()
	return c
}

// writeReplies 依序寫出讀取迴圈排進 replies 的訊息；寫不進去時記下（行程多半已走，由 exited 回報）。
// watch 關閉 replies 後結束。
func (c *conn) writeReplies() {
	defer close(c.replied)
	for {
		items, closed := c.replies.take()
		for _, x := range items {
			if err := c.write(x); err != nil {
				c.logger.Debug("claude: could not answer a control request", "err", err)
			}
		}
		if closed && len(items) == 0 {
			return
		}
	}
}

// queueReply 排一則要回給 Claude 的訊息。
func (c *conn) queueReply(v map[string]any) { c.replies.push(v) }

// newPrefix 讓權限 ID 在 Session 內唯一：恢復後是新的行程、新的前綴（CL30）。
func newPrefix() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "p" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])) + "-"
}

func (c *conn) NativeID() task.NativeSessionID { return c.sid }

// initInfo 是 initialize 的回應裡 OrielHub 讀的部分。
type initInfo struct {
	Mode   string  `json:"current_permission_mode"`
	Models []model `json:"models"`
}

// model 是 initialize 回應裡的一個可選模型（CL06）。
type model struct {
	Value       string `json:"value"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Resolved    string `json:"resolvedModel"`
}

// choice 是可選的模型與它實際解析成的模型（對照 system init 報的模型）。
type choice struct {
	value, resolved string
}

// initialize 發出 initialize 控制請求並等回應（CL01）。
func (c *conn) initialize(ctx context.Context) (initInfo, error) {
	raw, err := c.request(ctx, "initialize", map[string]any{"subtype": "initialize", "hooks": nil})
	if err != nil {
		return initInfo{}, err
	}
	var init initInfo
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &init); err != nil {
			return initInfo{}, fail(CodeProtocolError, err, "detail", "the initialize response is not an object")
		}
	}
	return init, nil
}

// bind 報出可選的模型（有的話，CL06）與 native_bound（CL01、CL02）。模型設置在綁定之前報：
// hub 在連線登記時依序處理，Plan 看到連線時已知道 agent 是什麼模型（D118）。
func (c *conn) bind(models []model) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if setting := c.setModels(models); setting != nil {
		c.out.push(task.Command{Kind: task.FactModelReported,
			ModelReported: &task.ModelReport{Setting: setting, Start: true}})
	}
	c.out.push(task.Command{Kind: task.FactNativeBound, Bound: &task.NativeBound{NativeID: c.sid}})
}

// request 發出一個控制請求並等 Claude 回應。request_id 依序編號，回放的夾具才對得上。
func (c *conn) request(ctx context.Context, name string, req map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.reqSeq++
	id := "oriel-" + strconv.Itoa(c.reqSeq)
	ch := make(chan reply, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
	}()
	if err := c.write(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		if c.gone(ctx) {
			return nil, errDisconnected
		}
		return nil, fail(CodeRequestFailed, err, "request", name, "detail", err.Error())
	}
	answered := func(r reply) (json.RawMessage, error) {
		if !r.ok {
			return nil, fail(CodeRequestFailed, nil, "request", name, "detail", r.err)
		}
		return r.response, nil
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

// gone 回報 Claude Code 是否已斷線：stdout 已結束，或在兩倍 exitGrace 內結束（寫入失敗可能早於
// stdout 的 EOF；行程結束後 watch 最多再等 exitGrace 才強制 EOF）。ctx 已結束時不等。
// 呼叫者不能持有 c.mu。
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
	// 持 c.mu 關閉，讓 Prompt 的「檢查 eof、登記這一輪」與 watch 的等待互不交錯。
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

// route 依訊息的 type 處理一行；不是 JSON 的行跳過，日誌只記長度（CL05）。
func (c *conn) route(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		c.logger.Debug("claude: skipped a line that is not JSON", "bytes", len(line))
		return
	}
	var err error
	switch head.Type {
	case "control_response":
		err = c.controlResponse(line)
	case "control_request":
		err = c.controlRequest(line)
	case "control_cancel_request":
		err = c.controlCancel(line)
	case "stream_event":
		err = c.streamEvent(line)
	case "assistant":
		err = c.assistant(line)
	case "user":
		err = c.user(line)
	case "result":
		err = c.result(line)
	case "system":
		err = c.system(line)
	}
	if err != nil {
		c.logger.Debug("claude: skipped a message OrielHub could not read", "type", head.Type, "bytes", len(line))
	}
}

// controlResponse 把回應交給等著它的請求；沒人等的（如中斷的回應）丟掉。
func (c *conn) controlResponse(line []byte) error {
	var m struct {
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
			Error     string          `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	c.mu.Lock()
	ch := c.waiting[m.Response.RequestID]
	delete(c.waiting, m.Response.RequestID)
	c.mu.Unlock()
	if ch != nil {
		ch <- reply{ok: m.Response.Subtype == "success", response: m.Response.Response, err: m.Response.Error}
	}
	return nil
}

// controlRequest 處理 Claude 發起的請求：can_use_tool 報成 asked，掛著等 Answer（CL30）；
// 其餘（hook、MCP）OrielHub 沒有註冊，回錯誤，不讓 Claude 等下去。
func (c *conn) controlRequest(line []byte) error {
	var m struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype     string          `json:"subtype"`
			ToolName    string          `json:"tool_name"`
			Input       json.RawMessage `json:"input"`
			ToolUseID   string          `json:"tool_use_id"`
			Title       string          `json:"title"`
			DisplayName string          `json:"display_name"`
			BlockedPath string          `json:"blocked_path"`
		} `json:"request"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	r := m.Request
	if r.Subtype != "can_use_tool" {
		c.queueReply(errorReply(m.RequestID, "OrielHub does not handle "+r.Subtype))
		return nil
	}
	input := decode(r.Input)
	c.mu.Lock()
	if !c.turn || c.cancelling { // 一輪之外，或已要求中斷：不再問使用者（CL36、CL40）
		c.mu.Unlock()
		c.queueReply(decision(m.RequestID, task.Deny, nil))
		return nil
	}
	st := c.tools[r.ToolUseID]
	if st == nil && r.ToolUseID != "" && !isPlanTool(r.ToolName) {
		st = c.startTool(r.ToolUseID, r.ToolName, input) // 沒出現過的工具呼叫先補 tool_started
	}
	kind := kindOf(r.ToolName)
	paths := pathsOf(input)
	if r.BlockedPath != "" && !slices.Contains(paths, r.BlockedPath) {
		paths = append(paths, r.BlockedPath)
	}
	title := firstNonEmpty(r.Title, r.DisplayName)
	if title == "" && st != nil {
		title = st.title
	}
	c.permSeq++
	id := task.PermissionID(c.permPrefix + strconv.Itoa(c.permSeq))
	c.perms[id] = &pendingPerm{request: m.RequestID, input: r.Input}
	c.permOf[m.RequestID] = id
	c.out.push(task.Command{Kind: task.FactAsked, Asked: &task.Asked{ID: id, Request: task.PermissionRequest{
		Tool: r.ToolName, Kind: kind, Title: firstNonEmpty(title, r.ToolName), Command: command(kind, input),
		Paths: paths, Input: compact(r.Input)}}})
	c.mu.Unlock()
	return nil
}

// controlCancel：Claude 撤回了請求（CL33），之後的 Answer 不再送；hub 裡的詢問留到這一輪結束。
func (c *conn) controlCancel(line []byte) error {
	var m struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	c.mu.Lock()
	if id, ok := c.permOf[m.RequestID]; ok {
		delete(c.perms, id)
		delete(c.permOf, m.RequestID)
	}
	c.mu.Unlock()
	return nil
}

func errorReply(request, msg string) map[string]any {
	return map[string]any{"type": "control_response",
		"response": map[string]any{"subtype": "error", "request_id": request, "error": msg}}
}

// decision 是對 can_use_tool 的回答：允許時原樣帶回原來的 input（CL31），拒絕時附說明（CL32）。
func decision(request string, v task.Verdict, input json.RawMessage) map[string]any {
	answer := map[string]any{"behavior": "deny", "message": denied}
	if v == task.Allow {
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		answer = map[string]any{"behavior": "allow", "updatedInput": input}
	}
	return map[string]any{"type": "control_response",
		"response": map[string]any{"subtype": "success", "request_id": request, "response": answer}}
}

// watch 在 stdout 關閉後決定結束原因，送出 exited（CL22、CL23）。行程結束了、stdout 卻還被
// 離開了行程群組的子孫開著時，寬限期後關掉 stdout。
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
			p = protocolError("stdout closed while Claude Code kept running")
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

// Prompt 把 user 訊息寫進 stdin 後返回，不等這一輪結束；result 到了才報 turn_ended（CL10）。
// 寫出之後才返回，hub 隨後送的中斷一定排在它後面（CL42）。還不支援的附件只讓這一輪失敗，
// 行程照常（CL12）。
func (c *conn) Prompt(ctx context.Context, p task.Prompt) error {
	content, berr := c.content(p)
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
	c.turn, c.cancelling = true, false
	c.streamed = map[string]bool{}
	c.mu.Unlock()
	err := c.write(map[string]any{"type": "user", "session_id": "",
		"message": map[string]any{"role": "user", "content": content}, "parent_tool_use_id": nil})
	if err == nil {
		return nil
	}
	if c.gone(ctx) {
		return nil // 寫入失敗多半是行程走了：由 exited 回報原因與 stderr 尾部
	}
	c.mu.Lock()
	c.turn = false
	c.mu.Unlock()
	return fail(CodeTurnFailed, err, "detail", err.Error())
}

func turnEnded(t task.TurnEnded) task.Command {
	return task.Command{Kind: task.FactTurnEnded, TurnEnded: &t}
}

func failed(e *Error) task.TurnEnded {
	return task.TurnEnded{Result: task.TurnError, Problem: &task.Problem{Code: e.ErrorCode(), Params: e.ErrorParams()}}
}

// Cancel 先拒絕掛著的審批，再送 interrupt；結局由這一輪的 result 報告（CL40、CL41）。
func (c *conn) Cancel(ctx context.Context) error {
	c.mu.Lock()
	if !c.turn || c.cancelling { // 沒有一輪在跑，或這一輪已要求中斷
		c.mu.Unlock()
		return nil
	}
	c.cancelling = true
	var pending []string
	for id, p := range c.perms {
		pending = append(pending, p.request)
		delete(c.permOf, p.request)
		delete(c.perms, id)
	}
	c.reqSeq++
	id := "oriel-" + strconv.Itoa(c.reqSeq)
	c.mu.Unlock()
	err := func() error {
		for _, r := range pending {
			if err := c.write(decision(r, task.Deny, nil)); err != nil {
				return err
			}
		}
		return c.write(map[string]any{"type": "control_request", "request_id": id,
			"request": map[string]any{"subtype": "interrupt"}})
	}()
	if err == nil {
		return nil
	}
	if werr := c.writeFailed(ctx, "interrupt", err); werr != nil {
		// 行程還在，只是這次沒送到：讓下一次 Cancel 再送，否則這一輪再也停不下來。
		c.mu.Lock()
		c.cancelling = false
		c.mu.Unlock()
		return werr
	}
	return nil
}

// writeFailed：寫入失敗時行程多半已結束，那就由 exited 回報；否則是這個請求失敗。
func (c *conn) writeFailed(ctx context.Context, request string, err error) error {
	if c.gone(ctx) {
		return nil
	}
	return fail(CodeRequestFailed, err, "request", request, "detail", err.Error())
}

// Answer 回答掛著的 can_use_tool（CL31、CL32）；Claude 已撤回或這一輪已結束時回 nil，不送（CL33）。
func (c *conn) Answer(ctx context.Context, id task.PermissionID, v task.Verdict) error {
	c.mu.Lock()
	p := c.perms[id]
	delete(c.perms, id)
	if p != nil {
		delete(c.permOf, p.request)
	}
	c.mu.Unlock()
	if p == nil {
		return nil
	}
	if err := c.write(decision(p.request, v, p.input)); err != nil {
		return c.writeFailed(ctx, "can_use_tool", err)
	}
	return nil
}

// SetModel 送 set_model，回傳換完之後的模型設置（CL07，D118）；Claude 在時限內沒回應時是
// claude.set_model_timed_out，之後才到的回應丟掉。
func (c *conn) SetModel(ctx context.Context, m task.ModelSwitch) (*task.ModelSetting, error) {
	sctx, cancel := context.WithTimeout(ctx, c.setModelTimeout)
	defer cancel()
	if _, err := c.request(sctx, "set_model", map[string]any{"subtype": "set_model", "model": m.Value}); err != nil {
		if ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
			return nil, fail(CodeSetModelTimedOut, err)
		}
		if errors.Is(err, errDisconnected) {
			return nil, fail(CodeRequestFailed, err, "request", "set_model", "detail", "Claude Code exited")
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

// Close 結束行程並等讀取、交付與回覆都結束（CL43）；可以重複呼叫。
func (c *conn) Close() error {
	c.close.Do(c.proc.Stop)
	<-c.ended
	<-c.pumped
	<-c.replied
	return nil
}

// outbox 是不設上限、依序的佇列：事實交給 hub，回覆寫給 Claude。
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
