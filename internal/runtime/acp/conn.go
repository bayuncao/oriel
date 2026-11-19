package acp

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

	sdk "github.com/coder/acp-go-sdk"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

const (
	// maxLine 是一行協議訊息的上限，與 SDK 的相同（C26）。
	maxLine = 10 << 20
	// exitGrace 是 stdout 關閉後等行程結束的時間；過了還在就當作協議錯誤
	// 結束它（C26）。正常結束時行程與 stdout 幾乎同時結束。
	exitGrace = 3 * time.Second
)

// conn 是一個 Session 的 agent 行程與它的 ACP 連線（hub.Conn）。
//
// stdout 由 read 一行一行讀：session/update 通知在這裡就地轉成事實，其餘
// 訊息（回應與 agent 的請求）才交給 SDK。所以通知的順序就是 agent 寫出的
// 順序，SDK 的通知佇列（滿了會斷線）用不到；Prompt 的回應交給 SDK 時，它
// 之前的更新都已轉成事實（C24、C25）。事實進 out 這個不設上限的佇列，由
// pump 依序交給 hub：hub 處理得慢時，讀取不會停下來。
type conn struct {
	proc    *proc.Process
	sdk     *sdk.ClientSideConnection
	logger  *slog.Logger
	version string
	workdir string
	agent   string // 給使用者看的名稱（acp.auth_required）
	// setModelTimeout 是 SetModel 等 agent 回應的時限（C48）。
	setModelTimeout time.Duration
	out             *outbox
	// sessionMeta 是建立與接回會話時帶的 _meta（D101）；Start 之前設好。
	sessionMeta map[string]any

	life     context.Context // Close 時取消：進行中的 Prompt 不再回報
	stopLife context.CancelFunc

	eof    chan struct{} // read 結束（stdout 關閉或協議錯誤）時關閉
	ended  chan struct{} // watch 送出 exited 並關閉 out 後關閉
	pumped chan struct{} // pump 把 out 交完後關閉
	turns  sync.WaitGroup
	close  sync.Once
	rpc    *rpcTrack

	mu      sync.Mutex
	readErr string // read 的協議錯誤；eof 關閉前寫入
	final   task.Problem
	sid     sdk.SessionId
	loading bool
	// bound 在握手報了 native_bound 後為真；之前的 config_option_update 不另報，只記下最後一則的設置
	// （startConfig），回應沒帶設置時用它（C49）。
	bound       bool
	startConfig []sdk.SessionConfigOption
	turn        bool          // 有 Prompt 在等回應
	sent        chan struct{} // 這一輪的 session/prompt 寫出後關閉
	cancelling  bool
	tools       map[sdk.ToolCallId]*toolState
	perms       map[task.PermissionID]*pendingPerm
	permPrefix  string
	permSeq     int
}

// toolState 是一個工具呼叫目前的樣子；更新只帶改了的欄位。
type toolState struct {
	title  string
	kind   task.ToolKind
	input  any
	paths  []string
	status task.ToolStatus
	output string
}

// pendingPerm 是一個掛著等回答的 session/request_permission。
type pendingPerm struct {
	options []sdk.PermissionOption
	answer  chan sdk.RequestPermissionOutcome // 容量 1
}

func (r *Runtime) open(p *proc.Process, emit func(task.Command), workdir, agent string) *conn {
	pr, pw := io.Pipe()
	life, stop := context.WithCancel(context.Background())
	c := &conn{proc: p, logger: r.logger, version: r.version, workdir: workdir, agent: agent, setModelTimeout: r.setModelTimeout,
		out: newOutbox(), life: life, stopLife: stop,
		eof: make(chan struct{}), ended: make(chan struct{}), pumped: make(chan struct{}),
		tools: map[sdk.ToolCallId]*toolState{}, perms: map[task.PermissionID]*pendingPerm{},
		permPrefix: newPrefix(), rpc: newRPCTrack()}
	c.rpc.onSend = c.requestSent
	c.sdk = sdk.NewClientSideConnection(client{c}, tap{w: p.Stdin(), t: c.rpc}, pr)
	c.sdk.SetLogger(r.sdkLogger)
	go c.pump(emit)
	go c.read(p.Stdout(), pw)
	go c.watch()
	return c
}

// newPrefix 讓權限 ID 在 Session 內唯一：恢復後是新的行程、新的前綴（C27）。
func newPrefix() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "p" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])) + "-"
}

func (c *conn) NativeID() task.NativeSessionID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return task.NativeSessionID(c.sid)
}

// read 讀 stdout 的每一行，直到 EOF 或一行超過 maxLine。
func (c *conn) read(out io.Reader, toSDK *io.PipeWriter) {
	br := bufio.NewReaderSize(out, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			c.route(line, toSDK)
		}
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				c.readErr = "a line longer than 10 MiB"
			}
			break
		}
	}
	// 先於關閉 SDK 的輸入：SDK 因斷線回錯時，eof 一定已關閉。持 c.mu 關閉，
	// 讓 Prompt 的「檢查 eof、登記這一輪」與 watch 的等待互不交錯。
	c.mu.Lock()
	close(c.eof)
	c.mu.Unlock()
	toSDK.Close()
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

// route 就地處理 session/update，其餘交給 SDK；不是 JSON 的行跳過（C09）。
func (c *conn) route(line []byte, toSDK io.Writer) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var head struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		c.logger.Debug("acp: skipped a line that is not JSON", "bytes", len(line))
		return
	}
	if head.ID != nil && head.Method == "" {
		c.rpc.reply(head.ID, line) // 回應：先記下，SDK 斷線時也知道 agent 回了什麼
	}
	if head.ID == nil && head.Method == sdk.ClientMethodSessionUpdate {
		var n sdk.SessionNotification
		if err := json.Unmarshal(head.Params, &n); err != nil {
			c.logger.Debug("acp: skipped an update OrielHub does not read", "bytes", len(line))
			return
		}
		c.update(n)
		return
	}
	if head.ID == nil && head.Method != "$/cancel_request" {
		// 其餘通知（各 agent 的擴充）OrielHub 不讀：不交給 SDK，它的通知佇列
		// 滿了會停止讀取，讀取迴圈就再也寫不進去。
		return
	}
	_, _ = toSDK.Write(append(line, '\n'))
}

// watch 在 stdout 關閉後決定結束原因，送出 exited（C24、C26）。行程結束了、
// stdout 卻還被離開了行程群組的子孫開著時，寬限期後關掉 stdout（C42）。
func (c *conn) watch() {
	defer close(c.ended)
	select {
	case <-c.eof:
	case <-c.proc.Done():
		t := time.NewTimer(exitGrace)
		select {
		case <-c.eof:
		case <-t.C:
			c.proc.CloseStdout()
			<-c.eof
		}
		t.Stop()
	}
	var p task.Problem
	if c.readErr != "" {
		c.proc.Stop()
		p = protocolError(c.readErr)
	} else {
		t := time.NewTimer(exitGrace)
		select {
		case <-c.proc.Done():
			p = problemOf(c.proc.Exit())
		case <-t.C:
			c.proc.Stop()
			p = protocolError("stdout closed while the agent kept running")
		}
		t.Stop()
	}
	c.turns.Wait() // 回應先到的那一輪先報完
	c.mu.Lock()
	c.final = p
	c.cancelPerms()
	c.mu.Unlock()
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

// Prompt 在 session/prompt 寫出後返回，不等這一輪結束；回應到了才報
// turn_ended（C10、C21）。寫出之後才返回，hub 隨後送的取消一定排在它後面
// （C43）。還不支援的附件只讓這一輪失敗，行程照常（C12）。
func (c *conn) Prompt(ctx context.Context, p task.Prompt) error {
	blocks, berr := c.blocks(p)
	c.mu.Lock()
	select {
	case <-c.eof:
		c.mu.Unlock()
		return nil // 行程已結束：由 exited 回報原因
	default:
	}
	if c.turn {
		c.mu.Unlock()
		return fail(CodePromptFailed, nil, "detail", "a turn is already running")
	}
	if berr != nil {
		c.out.push(turnEnded(failed(berr)))
		c.mu.Unlock()
		return nil
	}
	c.turn, c.cancelling = true, false
	sent, done := make(chan struct{}), make(chan struct{})
	c.sent = sent
	c.turns.Add(1)
	go func() {
		defer close(done)
		c.runTurn(blocks)
	}()
	c.mu.Unlock()
	select {
	case <-sent:
	case <-done: // 沒寫出就結束了（寫入失敗）：結局已由 runTurn 或 exited 報告
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// gone 回報 agent 是否已斷線：stdout 已結束，或在 2×exitGrace 內結束（寫入失敗
// 可能早於 stdout 的 EOF；行程結束後 watch 最多再等 exitGrace 才強制 EOF）。
// ctx 已結束時不等。呼叫者不能持有 c.mu。
func (c *conn) gone(ctx context.Context) bool {
	select {
	case <-c.eof:
		return true
	default:
	}
	if ctx.Err() != nil {
		return false
	}
	t := time.NewTimer(2 * exitGrace)
	defer t.Stop()
	select {
	case <-c.eof:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

// requestSent 在 SDK 寫出一個請求後呼叫：session/prompt 寫出了就讓 Prompt 返回。
func (c *conn) requestSent(method string) {
	if method != sdk.AgentMethodSessionPrompt {
		return
	}
	c.mu.Lock()
	ch := c.sent
	c.sent = nil
	c.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (c *conn) runTurn(blocks []sdk.ContentBlock) {
	defer c.turns.Done()
	c.mu.Lock()
	sid := c.sid
	c.mu.Unlock()
	resp, err := c.sdk.Prompt(c.life, sdk.PromptRequest{SessionId: sid, Prompt: blocks})
	replied := false
	if err != nil {
		// SDK 回錯不一定是 agent 沒回應：agent 回完就結束時，SDK 可能先看到
		// 斷線。route 記下的回應才算數（C24）。
		if raw := c.rpc.replied(sdk.AgentMethodSessionPrompt); raw != nil {
			if r, rerr, ok := decodePrompt(raw); ok {
				resp, err, replied = r, rerr, true
			}
		}
	}
	// 沒有回應的錯誤多半是 agent 走了（寫入失敗可能早於 stdout 結束）：等
	// 一下 eof，那就由 exited 回報（C24）。等的時候不持鎖：read 持鎖關 eof。
	gone := err != nil && !replied && c.gone(c.life)
	c.mu.Lock()
	defer c.mu.Unlock()
	cancelling := c.cancelling
	c.turn, c.cancelling, c.sent = false, false, nil
	c.cancelPerms() // 這一輪結束了，還掛著的請求一律取消
	if err != nil {
		if gone || (!replied && c.life.Err() != nil) {
			return // 斷線由 exited 回報；Close 了就不再報
		}
		c.out.push(turnEnded(c.promptErr(err, cancelling)))
		return
	}
	if u, ok := usageOf(resp.Usage); ok {
		c.out.push(task.Command{Kind: task.FactUsage, Usage: &u})
	}
	c.out.push(turnEnded(stopResult(resp.StopReason, cancelling)))
}

func turnEnded(t task.TurnEnded) task.Command {
	return task.Command{Kind: task.FactTurnEnded, TurnEnded: &t}
}

// promptErr 是 Prompt 回錯時這一輪的結局（C23、C34）。
func (c *conn) promptErr(err error, cancelling bool) task.TurnEnded {
	if cancelling {
		return task.TurnEnded{Result: task.TurnCancelled}
	}
	var e *Error
	if re, ok := errors.AsType[*sdk.RequestError](err); ok && re.Code == codeAuthRequired {
		e = fail(CodeAuthRequired, err, "agent", c.agent)
	} else {
		e = fail(CodePromptFailed, err, "detail", detail(err))
	}
	return failed(e)
}

func failed(e *Error) task.TurnEnded {
	return task.TurnEnded{Result: task.TurnError,
		Problem: &task.Problem{Code: e.ErrorCode(), Params: e.ErrorParams()}}
}

// stopResult 對應 stopReason（C21、C22）。
func stopResult(r sdk.StopReason, cancelling bool) task.TurnEnded {
	switch r {
	case sdk.StopReasonEndTurn:
		return task.TurnEnded{Result: task.TurnOK}
	case sdk.StopReasonCancelled:
		return task.TurnEnded{Result: task.TurnCancelled}
	case sdk.StopReasonMaxTokens:
		return failed(fail(CodeMaxTokens, nil))
	case sdk.StopReasonMaxTurnRequests:
		return failed(fail(CodeMaxTurnRequests, nil))
	case sdk.StopReasonRefusal:
		return failed(fail(CodeRefusal, nil))
	}
	if cancelling {
		return task.TurnEnded{Result: task.TurnCancelled}
	}
	return failed(fail(CodePromptFailed, nil, "detail", "unknown stop reason "+strconv.Quote(string(r))))
}

// Cancel 送出 session/cancel，掛著的請求回 cancelled；結局由 Prompt 的
// 回應報告（C33–C35）。
func (c *conn) Cancel(ctx context.Context) error {
	c.mu.Lock()
	if !c.turn || c.cancelling { // 沒有一輪在跑，或這一輪已要求取消
		c.mu.Unlock()
		return nil
	}
	c.cancelling = true
	sid := c.sid
	c.mu.Unlock()
	err := c.sdk.Cancel(ctx, sdk.CancelNotification{SessionId: sid})
	c.mu.Lock()
	c.cancelPerms()
	c.mu.Unlock()
	if err != nil {
		if c.gone(ctx) { // 寫入失敗可能早於 stdout 結束
			return nil // 行程已結束：由 exited 回報原因與 stderr 尾部
		}
		return fail(CodeRequestFailed, err, "method", "session/cancel", "detail", err.Error())
	}
	return nil
}

// SetModel 送出 session/set_config_option，回傳回應裡的模型設置（C47，D118）；agent 在時限內沒回應時是
// acp.set_model_timed_out（C48），之後才到的回應丟掉。
func (c *conn) SetModel(ctx context.Context, m task.ModelSwitch) (*task.ModelSetting, error) {
	c.mu.Lock()
	sid := c.sid
	c.mu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, c.setModelTimeout)
	defer cancel()
	r, err := c.sdk.SetSessionConfigOption(sctx, sdk.SetSessionConfigOptionRequest{ValueId: &sdk.SetSessionConfigOptionValueId{
		SessionId: sid, ConfigId: sdk.SessionConfigId(m.Option), Value: sdk.SessionConfigValueId(m.Value)}})
	if err != nil {
		if ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
			return nil, fail(CodeSetModelTimedOut, err)
		}
		return nil, c.requestErr(ctx, "session/set_config_option", err)
	}
	return modelOf(r.ConfigOptions), nil
}

// Answer 回答掛著的權限請求（C28、C29、C31）。
func (c *conn) Answer(_ context.Context, id task.PermissionID, v task.Verdict) error {
	c.mu.Lock()
	p := c.perms[id]
	delete(c.perms, id)
	c.mu.Unlock()
	if p != nil {
		p.answer <- outcome(p.options, v)
	}
	return nil
}

// outcome 選出要回的選項（D99）。
func outcome(opts []sdk.PermissionOption, v task.Verdict) sdk.RequestPermissionOutcome {
	kinds := []sdk.PermissionOptionKind{sdk.PermissionOptionKindRejectOnce, sdk.PermissionOptionKindRejectAlways}
	if v == task.Allow {
		kinds = []sdk.PermissionOptionKind{sdk.PermissionOptionKindAllowOnce, sdk.PermissionOptionKindAllowAlways}
	}
	for _, k := range kinds {
		for _, o := range opts {
			if o.Kind == k {
				return sdk.NewRequestPermissionOutcomeSelected(o.OptionId)
			}
		}
	}
	return sdk.NewRequestPermissionOutcomeCancelled()
}

// cancelPerms 讓每個掛著的請求回 cancelled；呼叫者持有 c.mu。
func (c *conn) cancelPerms() {
	for id, p := range c.perms {
		p.answer <- sdk.NewRequestPermissionOutcomeCancelled()
		delete(c.perms, id)
	}
}

// Close 結束行程並等讀取與交付都結束（C36）；可以重複呼叫。
func (c *conn) Close() error {
	c.close.Do(func() {
		c.stopLife()
		c.proc.Stop()
	})
	<-c.ended
	<-c.pumped
	return nil
}

// requestPermission 把 agent 的權限請求報成 asked，掛著等 Answer（C27）。
func (c *conn) requestPermission(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	cancelled := sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}
	c.mu.Lock()
	if !c.turn || req.SessionId != c.sid {
		c.mu.Unlock()
		return cancelled, nil
	}
	tc := req.ToolCall
	st := c.ensureTool(tc.ToolCallId, tc.Title, tc.Kind, tc.RawInput, tc.Locations)
	c.permSeq++
	id := task.PermissionID(c.permPrefix + strconv.Itoa(c.permSeq))
	p := &pendingPerm{options: req.Options, answer: make(chan sdk.RequestPermissionOutcome, 1)}
	c.perms[id] = p
	c.out.push(task.Command{Kind: task.FactAsked, Asked: &task.Asked{ID: id, Request: task.PermissionRequest{
		Tool: string(st.kind), Kind: st.kind, Title: st.title, Command: command(st.kind, st.input),
		Paths: st.paths, Input: rawJSON(st.input)}}})
	c.mu.Unlock()
	select {
	case o := <-p.answer:
		return sdk.RequestPermissionResponse{Outcome: o}, nil
	case <-ctx.Done(): // agent 撤回了請求，或連線斷了
		c.mu.Lock()
		delete(c.perms, id)
		c.mu.Unlock()
		return cancelled, nil
	}
}

// outbox 是不設上限、依序的事實佇列。
type outbox struct {
	mu     sync.Mutex
	items  []task.Command
	closed bool
	wake   chan struct{} // 容量 1
}

func newOutbox() *outbox { return &outbox{wake: make(chan struct{}, 1)} }

func (b *outbox) push(c task.Command) {
	b.mu.Lock()
	if !b.closed {
		b.items = append(b.items, c)
	}
	b.mu.Unlock()
	b.signal()
}

func (b *outbox) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.signal()
}

func (b *outbox) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// take 取走目前的全部事實；沒有也沒關閉時等。
func (b *outbox) take() ([]task.Command, bool) {
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

// client 是 SDK 的 acp.Client：只接權限請求。檔案讀寫與終端沒有宣告，
// agent 用自己的工具（D99）；通知已在 read 處理過，這裡只是保險。
type client struct{ c *conn }

func (k client) RequestPermission(ctx context.Context, r sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	return k.c.requestPermission(ctx, r)
}

func (k client) SessionUpdate(_ context.Context, n sdk.SessionNotification) error {
	k.c.update(n)
	return nil
}

func (client) ReadTextFile(context.Context, sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodFsReadTextFile)
}

func (client) WriteTextFile(context.Context, sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodFsWriteTextFile)
}

func (client) CreateTerminal(context.Context, sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalCreate)
}

func (client) KillTerminal(context.Context, sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalKill)
}

func (client) TerminalOutput(context.Context, sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalOutput)
}

func (client) ReleaseTerminal(context.Context, sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalRelease)
}

func (client) WaitForTerminalExit(context.Context, sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalWaitForExit)
}
