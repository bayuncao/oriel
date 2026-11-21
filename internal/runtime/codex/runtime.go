package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// defaultStartTimeout 是 initialize 到會話建好的時限（CX05）。
const defaultStartTimeout = 60 * time.Second

// defaultExitGrace 見 Options.ExitGrace。
const defaultExitGrace = 3 * time.Second

// 審批經 OrielHub 時開會話用的設定（CX01）：untrusted 下每個補丁都問、命令除非用戶的 exec policy
// 規則放行也都問；審批交給 user（客戶端），不讓 auto_review 的子 agent 自己決定；沙箱留在工作區內。
const (
	askPolicy   = "untrusted"
	askReviewer = "user"
	askSandbox  = "workspace-write"
)

// Options 是 New 的參數。
type Options struct {
	Runner  *proc.Runner // 必填：Codex 行程由它啟動，app 退出時一起結束
	Version string       // OrielHub 的版本，填在 initialize 的 clientInfo
	Logger  *slog.Logger // nil → slog.Default()
	// StartTimeout 是啟動的時限；0 → 60 秒。僅測試改它。
	StartTimeout time.Duration
	// ExitGrace 是 stdout 關閉後等行程結束、行程結束後等 stdout 關閉的時間，寫入失敗時也等這麼久看
	// 行程是否走了（CX21）；0 → 3 秒。僅測試改它。
	ExitGrace time.Duration
}

// Runtime 是 Codex 的 hub.Runtime：每個 Session 一個常駐的 `codex app-server` 行程，stdin/stdout 上
// 一行一則的 JSON-RPC（不帶 "jsonrpc" 欄位，docs/research/codex-app-server.md）。
type Runtime struct {
	runner       *proc.Runner
	version      string
	logger       *slog.Logger
	startTimeout time.Duration
	exitGrace    time.Duration
}

var _ hub.Runtime = (*Runtime)(nil)

func New(o Options) *Runtime {
	return &Runtime{runner: o.Runner, version: o.Version, logger: cmp.Or(o.Logger, slog.Default()),
		startTimeout: cmp.Or(o.StartTimeout, defaultStartTimeout),
		exitGrace:    cmp.Or(o.ExitGrace, defaultExitGrace)}
}

// Start 啟動 Codex，做 initialize、列出模型，再開或接回會話（CX01–CX06）；第一條事實是 native_bound
// （可選的模型在它之前）。Codex 在會話建好前就結束時回 hub.ExitError（D102）。
func (r *Runtime) Start(ctx context.Context, o hub.StartOptions) (hub.Conn, error) {
	p, err := r.runner.Start(proc.Command{Path: o.Launch.Executable, Args: o.Launch.Args, Env: proc.Env(o.Env), Dir: o.Workdir})
	if err != nil {
		return nil, err
	}
	c := r.open(p, o.Emit, o.Workdir)
	sctx, cancel := context.WithTimeout(ctx, r.startTimeout)
	defer cancel()
	models, th, err := c.handshake(sctx, o.Resume, o.Launch.Approval == task.ApprovalOrielHub)
	if err != nil {
		return nil, c.startFailed(ctx, sctx, err)
	}
	if o.Launch.Approval == task.ApprovalOrielHub {
		if policy, ok := asksOrielHub(th); !ok {
			c.Close()
			return nil, fail(CodeApprovalPolicy, nil, "policy", policy)
		}
	}
	c.bind(th.Thread.ID, models, th.Model)
	return c, nil
}

// threadResponse 是 thread/start 與 thread/resume 的回應裡 OrielHub 讀的部分。
type threadResponse struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
	Model    string          `json:"model"`
	Policy   json.RawMessage `json:"approvalPolicy"`
	Reviewer string          `json:"approvalsReviewer"`
}

// asksOrielHub 核對會話真的以 untrusted 開、審批交給客戶端（CX01）；不是時回報給使用者看的策略。
// 回應沒帶這個欄位（或是 null）時無從核對，同 Claude 不報 current_permission_mode 時。
func asksOrielHub(th threadResponse) (string, bool) {
	if len(th.Policy) == 0 || string(th.Policy) == "null" {
		return "", true
	}
	var policy string
	if json.Unmarshal(th.Policy, &policy) != nil {
		policy = "granular" // 物件形式只有 granular
	}
	switch {
	case policy != askPolicy:
		return policy, false
	case th.Reviewer != "" && th.Reviewer != askReviewer:
		return policy + " (approvals reviewed by " + th.Reviewer + ")", false
	}
	return policy, true
}

// handshake 是 initialize、initialized、model/list，再以 thread/start 開新會話或以 thread/resume
// 接回（excludeTurns：不送回歷史，CX02）。asks 時帶上每次都詢問 OrielHub 的設定；否則照用戶自己的
// Codex 設定（原生寬鬆模式，CX03）。
func (c *conn) handshake(ctx context.Context, resume task.NativeSessionID, asks bool) ([]model, threadResponse, error) {
	var th threadResponse
	info := map[string]any{"name": "orielhub", "title": "OrielHub", "version": cmp.Or(c.version, "dev")}
	if _, err := c.request(ctx, "initialize", map[string]any{"clientInfo": info}); err != nil {
		return nil, th, err
	}
	if err := c.notify("initialized", nil); err != nil {
		return nil, th, c.requestFailed(ctx, "initialized", err)
	}
	raw, err := c.request(ctx, "model/list", map[string]any{})
	if err != nil {
		return nil, th, err
	}
	var list struct {
		Data []model `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, th, fail(CodeProtocolError, err, "detail", "the model/list result is not an object")
	}
	params := map[string]any{"cwd": c.workdir}
	if asks {
		params["approvalPolicy"], params["approvalsReviewer"], params["sandbox"] = askPolicy, askReviewer, askSandbox
	}
	method := "thread/start"
	if resume != "" {
		method = "thread/resume"
		params["threadId"], params["excludeTurns"] = string(resume), true
	}
	if raw, err = c.request(ctx, method, params); err != nil {
		return nil, th, err
	}
	if err := json.Unmarshal(raw, &th); err != nil || th.Thread.ID == "" {
		return nil, th, fail(CodeProtocolError, err, "detail", "the "+method+" result has no thread")
	}
	return list.Data, th, nil
}

// errDisconnected 表示 Codex 沒有回應就斷線了（行程結束或 stdout 關閉）。
var errDisconnected = errors.New("codex: Codex went away before answering")

// startFailed 收掉啟動失敗的連線。沒回應就斷線時回 hub.ExitError，帶結束原因與 stderr 尾部（D102，
// CX04）；逾時是 codex.start_timed_out（CX05）；否則結束行程，回原因。
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
