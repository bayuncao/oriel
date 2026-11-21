package pi

import (
	"cmp"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// approvals 是 OrielHub 的審批擴充（PI03）：審批經 OrielHub 時寫到暫存檔，以 -e 交給 pi。
//
//go:embed approvals.ts
var approvals []byte

// approvalsCommand 是擴充註冊的命令；get_commands 裡有它才算載入了（PI03）。
const approvalsCommand = "orielhub-approvals"

// defaultStartTimeout 是啟動到會話建好的時限（PI05）。
const defaultStartTimeout = 60 * time.Second

// defaultSetModelTimeout 是換模型的時限（PI07）：hub 等著它才送出這一輪的 Prompt，不能無限等。
const defaultSetModelTimeout = 30 * time.Second

// defaultExitGrace 見 Options.ExitGrace。
const defaultExitGrace = 3 * time.Second

// Options 是 New 的參數。
type Options struct {
	Runner *proc.Runner // 必填：pi 行程由它啟動，app 退出時一起結束
	Logger *slog.Logger // nil → slog.Default()
	// StartTimeout 是啟動的時限；0 → 60 秒。僅測試改它。
	StartTimeout time.Duration
	// SetModelTimeout 是換模型的時限；0 → 30 秒。僅測試改它。
	SetModelTimeout time.Duration
	// ExitGrace 是 stdout 關閉後等行程結束、行程結束後等 stdout 關閉的時間，寫入失敗時也等這麼久看
	// 行程是否走了（PI20）；0 → 3 秒。僅測試改它。
	ExitGrace time.Duration
}

// Runtime 是 pi 的 hub.Runtime：每個 Session 一個常駐的 `pi --mode rpc` 行程，stdin/stdout 上一行一則
// 的 JSON 命令、回應與事件（docs/research/pi-rpc.md）。
type Runtime struct {
	runner          *proc.Runner
	logger          *slog.Logger
	startTimeout    time.Duration
	setModelTimeout time.Duration
	exitGrace       time.Duration
}

var _ hub.Runtime = (*Runtime)(nil)

func New(o Options) *Runtime {
	return &Runtime{runner: o.Runner, logger: cmp.Or(o.Logger, slog.Default()),
		startTimeout:    cmp.Or(o.StartTimeout, defaultStartTimeout),
		setModelTimeout: cmp.Or(o.SetModelTimeout, defaultSetModelTimeout),
		exitGrace:       cmp.Or(o.ExitGrace, defaultExitGrace)}
}

// Start 啟動 pi，讀會話狀態與可選的模型，審批經 OrielHub 時確認擴充已載入（PI01–PI06）；第一條事實是
// native_bound（可選的模型在它之前）。新會話與接回都以 --session-id 指定 ID：存在就接著，不存在就以
// 同一個 ID 新建（PI02）。pi 只收分開寫的參數值（`--session-id=…` 是未知選項），ID 總是 UUID，不會以
// "-" 開頭。pi 在會話建好前就結束時回 hub.ExitError（D102）。
func (r *Runtime) Start(ctx context.Context, o hub.StartOptions) (hub.Conn, error) {
	sid := o.Resume
	if sid == "" {
		sid = newSessionID()
	}
	if strings.HasPrefix(string(sid), "-") {
		return nil, fail(CodeProtocolError, nil, "detail", "the session ID starts with a dash")
	}
	args := append(slices.Clone(o.Launch.Args), "--session-id", string(sid))
	asks := o.Launch.Approval == task.ApprovalOrielHub
	var ext string
	if asks {
		var err error
		if ext, err = writeApprovals(); err != nil {
			return nil, err
		}
		args = append(args, "-e", ext)
	}
	p, err := r.runner.Start(proc.Command{Path: o.Launch.Executable, Args: args, Env: proc.Env(o.Env), Dir: o.Workdir})
	if err != nil {
		removeFile(ext)
		return nil, err
	}
	c := r.open(p, o.Emit, o.Workdir, ext)
	sctx, cancel := context.WithTimeout(ctx, r.startTimeout)
	defer cancel()
	h, err := c.handshake(sctx, asks)
	if err != nil {
		return nil, c.startFailed(ctx, sctx, err)
	}
	c.bind(h)
	return c, nil
}

// writeApprovals 把審批擴充寫進只有本使用者能讀寫的暫存檔（os.CreateTemp 以 0600 建檔），回傳路徑；
// Close 時刪掉（PI42）。副檔名 .ts：pi 以 jiti 依副檔名載入。
func writeApprovals() (string, error) {
	f, err := os.CreateTemp("", "orielhub-approvals-*.ts")
	if err != nil {
		return "", err
	}
	_, werr := f.Write(approvals)
	cerr := f.Close()
	if err := cmp.Or(werr, cerr); err != nil {
		removeFile(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func removeFile(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// newSessionID 是隨機的 UUID（第 4 版）。
func newSessionID() task.NativeSessionID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return task.NativeSessionID(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}

// handshakeResult 是啟動時讀到的會話狀態與模型。
type handshakeResult struct {
	session string
	current string // 目前的模型，provider/id；pi 沒選模型時為空
	models  []model
}

// handshake 依序發 get_state、get_available_models，審批經 OrielHub 時再發 get_commands 確認擴充已載入
// （PI01、PI03、PI06）。依序發送：回放的夾具才對得上。
func (c *conn) handshake(ctx context.Context, asks bool) (handshakeResult, error) {
	var h handshakeResult
	raw, err := c.request(ctx, "get_state", nil)
	if err != nil {
		return h, err
	}
	var state struct {
		SessionID string `json:"sessionId"`
		Model     *model `json:"model"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || state.SessionID == "" {
		return h, fail(CodeProtocolError, err, "detail", "the get_state result has no sessionId")
	}
	h.session = state.SessionID
	if state.Model != nil {
		h.current = state.Model.value()
	}
	if raw, err = c.request(ctx, "get_available_models", nil); err != nil {
		return h, err
	}
	var list struct {
		Models []model `json:"models"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return h, fail(CodeProtocolError, err, "detail", "the get_available_models result is not an object")
	}
	h.models = list.Models
	if !asks {
		return h, nil
	}
	if raw, err = c.request(ctx, "get_commands", nil); err != nil {
		return h, err
	}
	var cmds struct {
		Commands []struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &cmds); err != nil {
		return h, fail(CodeProtocolError, err, "detail", "the get_commands result is not an object")
	}
	loaded := false
	for _, x := range cmds.Commands {
		loaded = loaded || x.Name == approvalsCommand
	}
	if !loaded {
		return h, fail(CodeApprovalsUnavailable, nil)
	}
	return h, nil
}

// errDisconnected 表示 pi 沒有回應就斷線了（行程結束或 stdout 關閉）。
var errDisconnected = errors.New("pi: pi went away before answering")

// startFailed 收掉啟動失敗的連線。沒回應就斷線時回 hub.ExitError，帶結束原因與 stderr 尾部（D102，
// PI05）；逾時是 pi.start_timed_out；否則結束行程，回原因。
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
