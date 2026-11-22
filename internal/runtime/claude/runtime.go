package claude

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// defaultStartTimeout 是 initialize 的時限（CL04）。
const defaultStartTimeout = 60 * time.Second

// defaultSetModelTimeout 是換模型的時限（CL07）：hub 等著它才送出這一輪的 Prompt，不能無限等。
const defaultSetModelTimeout = 30 * time.Second

// askMode 是每次都詢問 OrielHub 的權限模式，啟動時傳它。--help 自 2.1.200 起把它列為 manual，
// 但兩個名字都接受，CLI 內部仍叫它 default（initialize 回的 current_permission_mode）；askModes
// 是核對時接受的名字：哪天 CLI 改報 manual，仍是同一個模式。
const askMode = "default"

var askModes = []string{askMode, "manual"}

// defaultExitGrace 見 Options.ExitGrace。
const defaultExitGrace = 3 * time.Second

// Options 是 New 的參數。
type Options struct {
	Runner *proc.Runner // 必填：Claude Code 行程由它啟動，app 退出時一起結束
	Logger *slog.Logger // nil → slog.Default()
	// StartTimeout 是啟動的時限；0 → 60 秒。僅測試改它。
	StartTimeout time.Duration
	// SetModelTimeout 是換模型的時限；0 → 30 秒。僅測試改它。
	SetModelTimeout time.Duration
	// ExitGrace 是 stdout 關閉後等行程結束、行程結束後等 stdout 關閉的時間，寫入失敗時也等這麼久看
	// 行程是否走了（CL23）；0 → 3 秒。僅測試改它。
	ExitGrace time.Duration
	// Quota 收下 Claude 報來的額度快照（rate_limit_event，D87、m3.md CQ）；app 傳
	// usage.Tracker.Report。不能阻塞（讀取迴圈直接呼叫）；nil → 不記。
	Quota func(usage.Quota)
}

// Runtime 是 Claude Code 的 hub.Runtime：每個 Session 一個常駐的 claude 行程，stdin/stdout 上
// 一行一個 JSON 的 stream-json 與控制協議（docs/research/claude-control-protocol.md）。
type Runtime struct {
	runner          *proc.Runner
	logger          *slog.Logger
	startTimeout    time.Duration
	setModelTimeout time.Duration
	exitGrace       time.Duration
	quota           func(usage.Quota)
}

var _ hub.Runtime = (*Runtime)(nil)

func New(o Options) *Runtime {
	return &Runtime{runner: o.Runner, logger: cmp.Or(o.Logger, slog.Default()),
		startTimeout:    cmp.Or(o.StartTimeout, defaultStartTimeout),
		setModelTimeout: cmp.Or(o.SetModelTimeout, defaultSetModelTimeout),
		exitGrace:       cmp.Or(o.ExitGrace, defaultExitGrace), quota: o.Quota}
}

// Start 啟動 Claude Code 並做 initialize（CL01–CL06）。新會話的 ID 由 OrielHub 產生、以
// --session-id 交給它，恢復時以 --resume 接回同一個 ID：CLI 要等第一則訊息才報出自己的
// 會話 ID，而第一條事實必須是 native_bound。兩者都用 --flag=value：--resume 的值是選填的，
// 分成兩個參數時以 "-" 開頭的值會被當成另一個參數。Claude Code 在會話建好前就結束時回
// hub.ExitError（D102）。
func (r *Runtime) Start(ctx context.Context, o hub.StartOptions) (hub.Conn, error) {
	sid := o.Resume
	args := slices.Clone(o.Launch.Args)
	if sid == "" {
		sid = newSessionID()
		args = append(args, "--session-id="+string(sid))
	} else {
		args = append(args, "--resume="+string(sid))
	}
	p, err := r.runner.Start(proc.Command{Path: o.Launch.Executable, Args: args, Env: proc.Env(o.Env), Dir: o.Workdir})
	if err != nil {
		return nil, err
	}
	c := r.open(p, o.Emit, sid, o.Workdir)
	sctx, cancel := context.WithTimeout(ctx, r.startTimeout)
	defer cancel()
	init, err := c.initialize(sctx)
	if err != nil {
		return nil, c.startFailed(ctx, sctx, err)
	}
	// 設定檔或日後的版本可能讓工具不經詢問就執行：審批經 OrielHub 時，起始模式必須是
	// 每次都詢問的那個，否則不啟動。舊版不報這個欄位時無從核對。
	if o.Launch.Approval == task.ApprovalOrielHub && init.Mode != "" && !slices.Contains(askModes, init.Mode) {
		c.Close()
		return nil, fail(CodePermissionMode, nil, "mode", init.Mode)
	}
	c.bind(init.Models)
	return c, nil
}

// newSessionID 是隨機的 UUID（第 4 版），Claude Code 的 --session-id 只收 UUID。
func newSessionID() task.NativeSessionID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return task.NativeSessionID(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}

// errDisconnected 表示 Claude Code 沒有回應就斷線了（行程結束或 stdout 關閉）。
var errDisconnected = errors.New("claude: Claude Code went away before answering")

// startFailed 收掉啟動失敗的連線。沒回應就斷線時回 hub.ExitError，帶結束原因與 stderr
// 尾部（D102，CL03）；逾時是 claude.start_timed_out（CL04）；否則結束行程，回原因。
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
