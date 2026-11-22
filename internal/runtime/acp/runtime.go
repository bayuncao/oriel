package acp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// defaultStartTimeout 是 initialize 加建立（或接回）會話的時限（C05）。
const defaultStartTimeout = 60 * time.Second

// defaultSetModelTimeout 是換模型的時限（C48）：hub 等著它才送出這一輪的 Prompt，不能無限等。
const defaultSetModelTimeout = 30 * time.Second

// Options 是 New 的參數。
type Options struct {
	Runner  *proc.Runner // 必填：agent 行程由它啟動，app 退出時一起結束
	Version string       // OrielHub 的版本，填在 initialize 的 clientInfo
	Logger  *slog.Logger // nil → slog.Default()
	// StartTimeout 是啟動的時限；0 → 60 秒。僅測試改它。
	StartTimeout time.Duration
	// SetModelTimeout 是換模型的時限；0 → 30 秒。僅測試改它。
	SetModelTimeout time.Duration
}

// Runtime 是 ACP 的 hub.Runtime：每個 Session 一個 agent 行程，stdio 上
// 的 JSON-RPC（github.com/coder/acp-go-sdk，D96）。SDK 的型別不離開這個套件。
type Runtime struct {
	runner          *proc.Runner
	version         string
	logger          *slog.Logger
	sdkLogger       *slog.Logger
	startTimeout    time.Duration
	setModelTimeout time.Duration
}

var _ hub.Runtime = (*Runtime)(nil)

func New(o Options) *Runtime {
	l := cmp.Or(o.Logger, slog.Default())
	return &Runtime{runner: o.Runner, version: o.Version, logger: l,
		sdkLogger:       slog.New(quiet{l.Handler()}),
		startTimeout:    cmp.Or(o.StartTimeout, defaultStartTimeout),
		setModelTimeout: cmp.Or(o.SetModelTimeout, defaultSetModelTimeout)}
}

// Start 啟動 agent、做 initialize，再建立或接回會話（C01–C08）；第一條
// 事實是 native_bound。agent 在會話建好前就結束時回 hub.ExitError（D102）。
func (r *Runtime) Start(ctx context.Context, o hub.StartOptions) (hub.Conn, error) {
	env := proc.Env(o.Env)
	if o.Launch.Agent == openCodeID && o.Launch.Approval == task.ApprovalOrielHub {
		var err error
		if env, err = openCodeAsk(env); err != nil {
			return nil, err
		}
	}
	p, err := r.runner.Start(proc.Command{Path: o.Launch.Executable, Args: o.Launch.Args,
		Env: env, Dir: o.Workdir})
	if err != nil {
		return nil, err
	}
	c := r.open(p, o.Emit, o.Workdir, o.Launch.AgentName)
	if o.Launch.Agent == grokID && o.Launch.Approval == task.ApprovalOrielHub {
		c.sessionMeta = grokAsk() // D101
	}
	sctx, cancel := context.WithTimeout(ctx, r.startTimeout)
	defer cancel()
	if err := c.handshake(sctx, o.Resume); err != nil {
		return nil, c.startFailed(ctx, sctx, err)
	}
	return c, nil
}

// Capabilities 對這個 agent 發一次 initialize 後結束它（不登入、不花額度），
// 回報它能否恢復會話（D98，C39）。
func (r *Runtime) Capabilities(ctx context.Context, cmd proc.Command) (task.Capabilities, error) {
	p, err := r.runner.Start(cmd)
	if err != nil {
		return task.Capabilities{}, err
	}
	c := r.open(p, func(task.Command) {}, cmd.Dir, "")
	defer c.Close()
	sctx, cancel := context.WithTimeout(ctx, r.startTimeout)
	defer cancel()
	init, err := c.initialize(sctx)
	if err != nil {
		return task.Capabilities{}, c.startFailed(ctx, sctx, err)
	}
	return task.Capabilities{Resume: canResume(init.AgentCapabilities)}, nil
}

func canResume(c sdk.AgentCapabilities) bool {
	return c.LoadSession || c.SessionCapabilities.Resume != nil
}

// initialize 宣告協議版本 1，不提供檔案讀寫與終端（D99）。
func (c *conn) initialize(ctx context.Context) (sdk.InitializeResponse, error) {
	init, err := c.sdk.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientInfo:      &sdk.Implementation{Name: "OrielHub", Version: c.version},
	})
	if err != nil {
		return init, c.requestErr(ctx, "initialize", err)
	}
	if init.ProtocolVersion != sdk.ProtocolVersionNumber {
		return init, fail(CodeProtocolVersion, nil, "version", strconv.Itoa(int(init.ProtocolVersion)))
	}
	return init, nil
}

// handshake 建立新會話，或以 resume / load 接回 NativeID（D98）。
func (c *conn) handshake(ctx context.Context, resume task.NativeSessionID) error {
	init, err := c.initialize(ctx)
	if err != nil {
		return err
	}
	mcp := []sdk.McpServer{}
	caps := init.AgentCapabilities
	sid := sdk.SessionId(resume)
	meta := c.sessionMeta
	var config []sdk.SessionConfigOption // 回應裡的會話設置（C45）
	switch {
	case resume == "":
		s, err := c.sdk.NewSession(ctx, sdk.NewSessionRequest{Meta: meta, Cwd: c.workdir, McpServers: mcp})
		if err != nil {
			return c.requestErr(ctx, "session/new", err)
		}
		sid, config = s.SessionId, s.ConfigOptions
		c.mu.Lock()
		c.sid = sid
		c.mu.Unlock()
	case caps.SessionCapabilities.Resume != nil:
		c.mu.Lock()
		c.sid = sid
		c.mu.Unlock()
		s, err := c.sdk.ResumeSession(ctx, sdk.ResumeSessionRequest{
			Meta: meta, SessionId: sid, Cwd: c.workdir, McpServers: mcp})
		if err != nil {
			return c.requestErr(ctx, "session/resume", err)
		}
		config = s.ConfigOptions
	case caps.LoadSession:
		// 載入時 agent 重放的歷史不是這一輪的輸出（C07）。回應之前的
		// 更新都已由讀取迴圈處理過，所以回應到了就能停止丟棄。
		c.mu.Lock()
		c.sid, c.loading = sid, true
		c.mu.Unlock()
		s, err := c.sdk.LoadSession(ctx, sdk.LoadSessionRequest{
			Meta: meta, SessionId: sid, Cwd: c.workdir, McpServers: mcp})
		c.mu.Lock()
		c.loading = false
		c.mu.Unlock()
		if err != nil {
			return c.requestErr(ctx, "session/load", err)
		}
		config = s.ConfigOptions
	default:
		return fail(CodeResumeUnsupported, nil)
	}
	c.bind(sid, config)
	return nil
}

// bind 報出握手的結果：模型設置在綁定之前報，hub 在連線登記時依序處理，Plan 看到連線時已知道 agent 接回後是什麼
// 模型（D118）。回應裡的設置比握手中的更新新（SDK 先處理完回應之前的通知才交回回應）；回應沒帶時用握手中最後一則
// 更新。回應之後、這裡之前讀到的更新（route 記下回應後）是 agent 之後的變化，在綁定之後另報（C46）。
func (c *conn) bind(sid sdk.SessionId, config []sdk.SessionConfigOption) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if config == nil {
		config = c.startConfig
	}
	after := c.afterConfig
	c.bound, c.startConfig, c.afterConfig = true, nil, nil
	c.out.push(task.Command{Kind: task.FactModelReported,
		ModelReported: &task.ModelReport{Setting: modelOf(config), Start: true}})
	c.out.push(task.Command{Kind: task.FactNativeBound,
		Bound: &task.NativeBound{NativeID: task.NativeSessionID(sid)}})
	if after != nil {
		// 回應之後、綁定之前 agent 又換了：照之後的更新另報（C46）。
		c.out.push(task.Command{Kind: task.FactModelReported, ModelReported: &task.ModelReport{Setting: modelOf(after)}})
	}
}

// errDisconnected 表示 agent 沒有回應就斷線了（行程結束或 stdout 關閉）。
var errDisconnected = errors.New("acp: the agent went away before answering")

// startFailed 收掉啟動失敗的連線。agent 沒回應就斷線了，回 hub.ExitError，
// 帶結束原因與 stderr 尾部（D102）；agent 回了錯誤（即使隨即結束）就回那個
// 原因（C03）；否則結束行程，回原因。
func (c *conn) startFailed(parent, sctx context.Context, err error) error {
	if errors.Is(err, errDisconnected) {
		<-c.ended
		c.Close()
		c.mu.Lock()
		p := c.final
		c.mu.Unlock()
		return &exitError{code: p.Code, params: p.Params, tail: c.proc.StderrTail()}
	}
	c.Close()
	switch {
	case parent.Err() != nil:
		return parent.Err()
	case sctx.Err() != nil:
		return fail(CodeStartTimedOut, err)
	}
	return err
}

// requestErr 把請求的失敗做成代碼。agent 回了錯誤就照它（-32000 是要先登入，
// C03），即使 SDK 先看到了斷線；沒回應就斷線是 errDisconnected。
func (c *conn) requestErr(ctx context.Context, method string, err error) error {
	if re := c.rpc.replyError(method); re != nil {
		err = re
	} else if c.gone(ctx) {
		return errDisconnected
	}
	if re, ok := errors.AsType[*sdk.RequestError](err); ok && re.Code == codeAuthRequired {
		return fail(CodeAuthRequired, err, "agent", c.agent)
	}
	return fail(CodeRequestFailed, err, "method", method, "detail", detail(err))
}

// codeAuthRequired 是 ACP 的「需要先認證」錯誤碼。
const codeAuthRequired = -32000

// detail 是 agent 錯誤的說明：訊息加上 data 裡的 error（SDK 的慣例）。
func detail(err error) string {
	re, ok := errors.AsType[*sdk.RequestError](err)
	if !ok {
		return err.Error()
	}
	if m, ok := re.Data.(map[string]any); ok {
		if s, ok := m["error"].(string); ok && s != "" {
			return re.Message + ": " + s
		}
	}
	if re.Data != nil {
		return fmt.Sprintf("%s: %v", re.Message, re.Data)
	}
	return re.Message
}
