//go:build unix

package claude

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

// 夾具的常用行（測試二進位扮演 Claude Code，見 contract.Main）。
const (
	lInit = `{"client":{"type":"control_request","request_id":"oriel-1","request":{"subtype":"initialize"}}}`
	// lInitOK 回報每次都詢問的模式與兩個可選的模型。
	lInitOK = `{"agent":{"type":"control_response","response":{"subtype":"success","request_id":"oriel-1","response":{` +
		`"current_permission_mode":"default","models":[` +
		`{"value":"default","displayName":"Default","description":"The default","resolvedModel":"claude-a"},` +
		`{"value":"fast","displayName":"Fast","description":"Quick","resolvedModel":"claude-b"}]}}}}`
	// lUser 是 OrielHub 送的一則 user 訊息（內容另由 lUserText 核對）。
	lUser = `{"client":{"type":"user","message":{"role":"user"}}}`
)

// started 是 initialize 完成為止的兩行。
var started = []string{lInit, lInitOK}

// lInitWith 是 initialize 的回應，內容由呼叫者給。
func lInitWith(response string) string {
	return `{"agent":{"type":"control_response","response":{"subtype":"success","request_id":"oriel-1","response":` + response + `}}}`
}

// lUserText 是 OrielHub 送的一則只有一段文字的 user 訊息。
func lUserText(text string) string {
	return fmt.Sprintf(`{"client":{"type":"user","message":{"role":"user","content":[{"type":"text","text":%q}]}}}`, text)
}

// lAgent 是 Claude Code 寫出的一則訊息。
func lAgent(msg string) string { return `{"agent":` + msg + `}` }

// lDelta 是主對話的一段增量；kind 是 text_delta 或 thinking_delta。
func lDelta(kind, field, text string) string {
	return lAgent(fmt.Sprintf(`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","delta":{"type":%q,%q:%q}}}`,
		kind, field, text))
}

// lStart 是一則 assistant 訊息的開始（之後的增量屬於它）。
func lStart(id string) string {
	return lAgent(fmt.Sprintf(`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":%q}}}`, id))
}

// lAssistant 是一則完整的 assistant 訊息；content 是內容區塊的 JSON 陣列。
func lAssistant(id, content string) string {
	return lAgent(fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":%q,"content":%s}}`, id, content))
}

// lResult 是這一輪的 result。
func lResult(fields string) string { return lAgent(`{"type":"result",` + fields + `}`) }

// lOK 是成功的 result。
var lOK = lResult(`"subtype":"success","is_error":false,"result":"done"`)

// fixture 把夾具寫進暫存檔。
func fixture(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.ndjson")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testLaunch 是扮演 Claude Code 的測試二進位的 Launch。
func testLaunch() task.LaunchSnapshot {
	return task.LaunchSnapshot{Agent: "claude", AgentName: "Claude Code", Protocol: task.ProtocolClaude,
		Executable: os.Args[0], Approval: task.ApprovalOrielHub, Caps: task.Capabilities{Resume: true}}
}

// replayEnv 是讓測試二進位依夾具扮演 Claude Code 的環境（多個夾具依次給每次啟動）。
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
	c, err := r.Start(context.Background(), hub.StartOptions{Launch: l, Workdir: t.TempDir(), Resume: resume,
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
