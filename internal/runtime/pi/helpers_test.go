//go:build unix

package pi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/runtime/contract"
	"github.com/bayuncao/oriel/internal/task"
)

// testWait 只是把卡住變成失敗的保險。
const testWait = 30 * time.Second

// 夾具的常用行（測試二進位扮演 pi，見 contract.Main）。命令的 id 依序是 get_state oriel-1、
// get_available_models oriel-2、get_commands oriel-3，之後的命令 oriel-4、oriel-5……
const (
	lState    = `{"client":{"id":"oriel-1","type":"get_state"}}`
	lStateOK  = `{"agent":{"id":"oriel-1","type":"response","command":"get_state","success":true,"data":{"sessionId":"<session>","model":{"provider":"anthropic","id":"claude-a","name":"Claude A"}}}}`
	lModels   = `{"client":{"id":"oriel-2","type":"get_available_models"}}`
	lModelsOK = `{"agent":{"id":"oriel-2","type":"response","command":"get_available_models","success":true,"data":{"models":[` +
		`{"provider":"anthropic","id":"claude-a","name":"Claude A"},{"provider":"openai","id":"gpt-b","name":"GPT B"}]}}}`
	lCommands   = `{"client":{"id":"oriel-3","type":"get_commands"}}`
	lCommandsOK = `{"agent":{"id":"oriel-3","type":"response","command":"get_commands","success":true,"data":{"commands":[{"name":"orielhub-approvals","source":"extension"}]}}}`
)

// started 是會話建好為止的各行（審批經 OrielHub）。
var started = []string{lState, lStateOK, lModels, lModelsOK, lCommands, lCommandsOK}

// lPrompt 是 id 為 oriel-<n> 的 prompt 與它的回應（disposition started）。
func lPrompt(n int, message string) []string {
	return []string{fmt.Sprintf(`{"client":{"id":"oriel-%d","type":"prompt","message":%q}}`, n, message),
		lResponse(n, "prompt", `{"disposition":"started"}`)}
}

// lResponse 是對 oriel-<n> 的成功回應。
func lResponse(n int, command, data string) string {
	return fmt.Sprintf(`{"agent":{"id":"oriel-%d","type":"response","command":%q,"success":true,"data":%s}}`, n, command, data)
}

// lEvent 是 pi 寫出的一則記錄（type 與其餘欄位）。
func lEvent(typ, fields string) string {
	if fields == "" {
		return `{"agent":{"type":"` + typ + `"}}`
	}
	return `{"agent":{"type":"` + typ + `",` + fields + `}}`
}

// lDelta 是 assistant 的一段增量（text_delta 或 thinking_delta）。
func lDelta(kind, delta string) string {
	return lEvent("message_update", fmt.Sprintf(`"assistantMessageEvent":{"type":%q,"contentIndex":0,"delta":%q}`, kind, delta))
}

// lAssistant 是一條 assistant 訊息的結束；fields 是 message 的其餘欄位。
func lAssistant(stop, fields string) string {
	m := `"role":"assistant","provider":"anthropic","model":"claude-a","stopReason":"` + stop + `"`
	if fields != "" {
		m += "," + fields
	}
	return lEvent("message_end", `"message":{`+m+`}`)
}

// lSettled 是這一輪結束。
func lSettled(aborted bool) string {
	return lEvent("agent_settled", fmt.Sprintf(`"aborted":%t`, aborted))
}

// lAsk 是審批擴充的詢問：UI 請求 id、工具呼叫 id、工具名與輸入。
func lAsk(uid, call, tool, input string) string {
	msg := fmt.Sprintf(`{"toolCallId":%q,"toolName":%q,"input":%s}`, call, tool, input)
	return lEvent("extension_ui_request", fmt.Sprintf(`"id":%q,"method":"confirm","title":"orielhub.permission","message":%q`, uid, msg))
}

// lAnswer 是 OrielHub 對 UI 請求 uid 的回答（field 是 confirmed 或 cancelled）。
func lAnswer(uid, field string, value bool) string {
	return fmt.Sprintf(`{"client":{"type":"extension_ui_response","id":%q,%q:%t}}`, uid, field, value)
}

// fixture 把夾具寫進暫存檔。
func fixture(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.ndjson")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testLaunch 是扮演 pi 的測試二進位的 Launch。
func testLaunch() task.LaunchSnapshot {
	return task.LaunchSnapshot{Agent: "pi", AgentName: "pi", Protocol: task.ProtocolPi,
		Executable: os.Args[0], Approval: task.ApprovalOrielHub, Caps: task.Capabilities{Resume: true}}
}

// replayEnv 是讓測試二進位依夾具扮演 pi 的環境（多個夾具依次給每次啟動）。
func replayEnv(t *testing.T, fixtures ...string) []string {
	return append(os.Environ(), contract.EnvReplay+"="+strings.Join(fixtures, ","),
		contract.EnvReplayState+"="+filepath.Join(t.TempDir(), "state"))
}

// rec 收集轉接器發出的事實。
type rec struct {
	mu     sync.Mutex
	list   []task.Command
	cursor int
	wake   chan struct{}
}

func newRec() *rec { return &rec{wake: make(chan struct{}, 1)} }

func (r *rec) emit(c task.Command) {
	r.mu.Lock()
	r.list = append(r.list, c)
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// next 等下一條符合 pred 的事實，略過中間的。
func (r *rec) next(t *testing.T, pred func(task.Command) bool) task.Command {
	t.Helper()
	deadline := time.NewTimer(testWait)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		for r.cursor < len(r.list) {
			c := r.list[r.cursor]
			r.cursor++
			if pred(c) {
				r.mu.Unlock()
				return c
			}
		}
		r.mu.Unlock()
		select {
		case <-r.wake:
		case <-deadline.C:
			t.Fatalf("no matching fact; got %v", r.kinds())
		}
	}
}

// nextKind 等下一條指定種類的事實。
func (r *rec) nextKind(t *testing.T, k task.CommandKind) task.Command {
	t.Helper()
	return r.next(t, func(c task.Command) bool { return c.Kind == k })
}

// all 是到目前為止的全部事實。
func (r *rec) all() []task.Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]task.Command(nil), r.list...)
}

func (r *rec) kinds() []task.CommandKind {
	var out []task.CommandKind
	for _, c := range r.all() {
		out = append(out, c.Kind)
	}
	return out
}

// turnEnd 等這一輪結束（或行程結束）。
func (r *rec) turnEnd(t *testing.T) task.Command {
	t.Helper()
	return r.next(t, func(c task.Command) bool { return c.Kind == task.FactTurnEnded || c.Kind == task.FactExited })
}

// start 依夾具啟動一個連線；testing 結束時關閉。
func start(t *testing.T, o Options, l task.LaunchSnapshot, resume task.NativeSessionID, fixtures ...string) (hub.Conn, *rec, error) {
	t.Helper()
	r := newRuntime(t, o)
	got := newRec()
	dir, err := filepath.EvalSymlinks(t.TempDir()) // 夾具的 <cwd> 是行程看到的真實路徑
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Start(context.Background(), hub.StartOptions{Launch: l, Workdir: dir, Resume: resume,
		Env: replayEnv(t, fixtures...), Emit: got.emit})
	if c != nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, got, err
}

// mustStart 同 start，失敗時結束測試。
func mustStart(t *testing.T, lines ...string) (hub.Conn, *rec) {
	t.Helper()
	c, got, err := start(t, Options{}, testLaunch(), "", fixture(t, lines...))
	if err != nil {
		if xe, ok := err.(hub.ExitError); ok {
			t.Fatalf("start: %v; stderr: %s", err, xe.StderrTail())
		}
		t.Fatalf("start: %v", err)
	}
	return c, got
}

// prompt 送一段文字，失敗時結束測試。
func prompt(t *testing.T, c hub.Conn, text string) {
	t.Helper()
	if err := c.Prompt(context.Background(), task.Prompt{ID: "pr-1", Text: text}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
}

// wantTurn 確認這一輪的結局與原因。
func wantTurn(t *testing.T, c task.Command, r task.TurnResult, code string, params map[string]string) {
	t.Helper()
	if c.Kind != task.FactTurnEnded || c.TurnEnded.Result != r {
		t.Fatalf("got %s %+v, want turn_ended %s", c.Kind, c.TurnEnded, r)
	}
	p := c.TurnEnded.Problem
	if code == "" {
		if p != nil {
			t.Fatalf("problem %+v, want none", p)
		}
		return
	}
	if p == nil || p.Code != code {
		t.Fatalf("problem %+v, want %s", p, code)
	}
	for k, v := range params {
		if p.Params[k] != v {
			t.Errorf("problem param %s = %q, want %q", k, p.Params[k], v)
		}
	}
}
