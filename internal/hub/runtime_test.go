//go:build unix

package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// lineRuntime 是測試用的 Runtime：啟動 ORIELHUB_TEST_AGENT=1
// 的自身二進位，以行 JSON 對話；只認 "test-line" 協議。
type lineRuntime struct{}

func (lineRuntime) Start(ctx context.Context, o StartOptions) (Conn, error) {
	cmd := exec.Command(os.Args[0])
	cmd.Dir = o.Workdir
	env := append(append([]string{}, o.Env...), "ORIELHUB_TEST_AGENT=1")
	if o.Resume != "" {
		env = append(env, "ORIELHUB_TEST_RESUME="+string(o.Resume))
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tail{max: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &lineConn{
		cmd: cmd, stdin: stdin, stderr: stderr,
		bound: make(chan task.NativeSessionID, 1),
		done:  make(chan struct{}),
		model: make(chan modelReply, 1),
	}
	for _, kv := range o.Env {
		if g, ok := strings.CutPrefix(kv, "ORIELHUB_TEST_PROMPT_GATE="); ok {
			c.promptGate = g
		}
		if g, ok := strings.CutPrefix(kv, "ORIELHUB_TEST_CLOSE_GATE="); ok {
			c.closeGate = g
		}
	}
	go c.read(stdout, o.Emit)
	select {
	case id := <-c.bound:
		c.id = id
		o.Emit(task.Command{Kind: task.FactNativeBound,
			Bound: &task.NativeBound{NativeID: id}})
		return c, nil
	case <-c.done: // 綁定前就結束（D102）
		return nil, &lineExit{problem: c.exit, tail: c.stderr.String()}
	case <-ctx.Done():
		killGroup(cmd)
		<-c.done
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		killGroup(cmd)
		<-c.done
		return nil, fmt.Errorf("test agent did not bind")
	}
}

// lineConn 是一條行 JSON 連線。
type lineConn struct {
	id     task.NativeSessionID
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *tail
	mu     sync.Mutex // 寫 stdin
	bound  chan task.NativeSessionID
	done   chan struct{}
	exit   task.Problem // done 關閉前寫入
	// promptGate 非空時，Prompt 等這個檔案出現才送出（send_prompt 停在送出中）。
	promptGate string
	// closeGate 非空時，Close 等這個檔案出現才殺行程（close_conn 停在關的途中）。
	closeGate string
	model     chan modelReply // set_model 的回覆
}

// modelReply 是測試 agent 對 set_model 的回覆：換完的設置，或拒絕的原因。
type modelReply struct {
	setting *task.ModelSetting
	err     string
}

// lineExit 是測試 agent 綁定前就結束時 Start 回的 ExitError。
type lineExit struct {
	problem task.Problem
	tail    string
}

func (e *lineExit) Error() string                  { return "test agent exited: " + e.problem.Code }
func (e *lineExit) ErrorCode() string              { return e.problem.Code }
func (e *lineExit) ErrorParams() map[string]string { return e.problem.Params }
func (e *lineExit) StderrTail() string             { return e.tail }

func (c *lineConn) NativeID() task.NativeSessionID { return c.id }

func (c *lineConn) send(m map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := json.Marshal(m)
	_, err := fmt.Fprintln(c.stdin, string(b))
	return err
}

func (c *lineConn) Prompt(_ context.Context, p task.Prompt) error {
	if !c.waitGate(c.promptGate) {
		return fmt.Errorf("test agent gone")
	}
	return c.send(map[string]any{"type": "prompt", "text": p.Text})
}

func (c *lineConn) Answer(_ context.Context, id task.PermissionID, v task.Verdict) error {
	return c.send(map[string]any{"type": "answer", "id": string(id), "verdict": string(v)})
}

func (c *lineConn) Cancel(context.Context) error {
	return c.send(map[string]any{"type": "cancel"})
}

func (c *lineConn) SetModel(ctx context.Context, m task.ModelSwitch) (*task.ModelSetting, error) {
	if err := c.send(map[string]any{"type": "set_model", "option": m.Option, "value": m.Value}); err != nil {
		return nil, err
	}
	select {
	case r := <-c.model:
		if r.err != "" {
			return nil, fmt.Errorf("test agent: %s", r.err)
		}
		return r.setting, nil
	case <-c.done:
		return nil, fmt.Errorf("test agent gone")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// modelSetting 讀測試 agent 寫出的模型設置：current 與 choices（值即名稱）。
func modelSetting(m map[string]any) *task.ModelSetting {
	s := &task.ModelSetting{Option: "model", Current: text(m, "current")}
	cs, _ := m["choices"].([]any)
	for _, v := range cs {
		if v, ok := v.(string); ok {
			s.Choices = append(s.Choices, task.ModelChoice{Value: v, Name: strings.ToUpper(v[:1]) + v[1:]})
		}
	}
	return s
}

// waitGate 等 gate 檔案出現（gate 為空時不等）；行程先結束了（測試失敗時的清理）就不再等，回假。
func (c *lineConn) waitGate(gate string) bool {
	for gate != "" && !exists(gate) {
		select {
		case <-c.done:
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return true
}

// Close 殺掉整個行程群組並等 reader 結束。
func (c *lineConn) Close() error {
	c.waitGate(c.closeGate)
	killGroup(c.cmd)
	<-c.done
	return nil
}

// read 把子行程的行對映成事實；EOF 時回報 exited。
func (c *lineConn) read(out io.Reader, emit func(task.Command)) {
	defer close(c.done)
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	boundSent := false
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m["type"] {
		case "bound":
			c.bound <- task.NativeSessionID(text(m, "id"))
			boundSent = true
		case "model": // 建立或接回時報告的模型設置（C45）：在 bound 之前
			emit(task.Command{Kind: task.FactModelReported, ModelReported: &task.ModelReport{Setting: modelSetting(m), Start: true}})
		case "model_set":
			c.model <- modelReply{setting: modelSetting(m)}
		case "model_error":
			c.model <- modelReply{err: text(m, "message")}
		case "text":
			emit(task.Command{Kind: task.FactOutput,
				Output: &task.Output{Channel: task.ChannelText, Text: text(m, "text")}})
		case "thinking":
			emit(task.Command{Kind: task.FactOutput,
				Output: &task.Output{Channel: task.ChannelThinking, Text: text(m, "text")}})
		case "tool":
			kind := task.ToolKind(text(m, "kind"))
			if kind == "" {
				kind = task.ToolOther
			}
			var paths []string
			if ps, ok := m["paths"].([]any); ok {
				for _, p := range ps {
					if s, ok := p.(string); ok {
						paths = append(paths, s)
					}
				}
			}
			emit(task.Command{Kind: task.FactToolStarted,
				ToolStarted: &task.ToolCall{ID: task.ToolCallID(text(m, "id")),
					Tool: "test", Kind: kind, Title: text(m, "title"), Paths: paths}})
		case "tool_update":
			emit(task.Command{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{
				ID: task.ToolCallID(text(m, "id")), Status: task.ToolStatus(text(m, "status"))}})
		case "ask":
			cmd := text(m, "command")
			emit(task.Command{Kind: task.FactAsked,
				Asked: &task.Asked{ID: task.PermissionID(text(m, "id")),
					Request: task.PermissionRequest{
						Tool: "Bash", Kind: task.ToolExecute, Title: cmd, Command: cmd}}})
		case "end":
			r := &task.TurnEnded{Result: task.TurnResult(text(m, "result"))}
			if r.Result == task.TurnError {
				r.Problem = &task.Problem{Code: "test.agent_failed"}
			}
			emit(task.Command{Kind: task.FactTurnEnded, TurnEnded: r})
		}
	}
	_ = boundSent
	p := task.Problem{Code: "proc.exited", Params: map[string]string{"status": "0"}}
	if err := c.cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				p = task.Problem{Code: "proc.signaled",
					Params: map[string]string{"signal": strconv.Itoa(int(ws.Signal()))}}
			} else {
				p = task.Problem{Code: "proc.exited",
					Params: map[string]string{"status": strconv.Itoa(ee.ExitCode())}}
			}
		}
	}
	c.exit = p
	emit(task.Command{Kind: task.FactExited,
		Exited: &task.Exited{Problem: p, StderrTail: c.stderr.String()}})
}

// killGroup 殺掉 cmd 的整個行程群組。
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// tail 是最後 max 位元組的寫入緩衝。
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
