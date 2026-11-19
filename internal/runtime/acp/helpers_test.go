//go:build unix

package acp

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
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/runtime/contract"
	"github.com/bayuncao/oriel/internal/task"
)

// testWait 只是把卡住變成失敗的保險。
const testWait = 30 * time.Second

// 夾具的常用行（測試二進位扮演 agent，見 contract.Main）。
const (
	lInit   = `{"client":{"id":1,"method":"initialize","params":{"protocolVersion":1}}}`
	lInitOK = `{"agent":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}}`
	lNew    = `{"client":{"id":2,"method":"session/new","params":{"cwd":"<cwd>","mcpServers":[]}}}`
	lNewOK  = `{"agent":{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1"}}}`
)

// started 是建好會話為止的四行。
var started = []string{lInit, lInitOK, lNew, lNewOK}

// lPrompt 是 OrielHub 送出的第 id 個請求：一段文字的 session/prompt。
func lPrompt(id int, text string) string {
	return fmt.Sprintf(`{"client":{"id":%d,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":%q}]}}}`, id, text)
}

// lUpdate 是 agent 對 s1 送的一則 session/update。
func lUpdate(u string) string {
	return `{"agent":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":` + u + `}}}`
}

// lResult 是 agent 對第 id 個請求的回應。
func lResult(id int, result string) string {
	return fmt.Sprintf(`{"agent":{"jsonrpc":"2.0","id":%d,"result":%s}}`, id, result)
}

func lChunk(text string) string {
	return lUpdate(fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}}`, text))
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

// testLaunch 是扮演 agent 的測試二進位的 Launch。
func testLaunch() task.LaunchSnapshot {
	return task.LaunchSnapshot{Agent: "test", AgentName: "Test Agent", Protocol: task.ProtocolACP,
		Executable: os.Args[0], Approval: task.ApprovalOrielHub, Caps: task.Capabilities{Resume: true}}
}

// replayEnv 是讓測試二進位依夾具扮演 agent 的環境（多個夾具依次給每次啟動）。
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
	gate   chan struct{} // 非 nil 時 emit 等它關閉（C25：hub 處理得慢）
}

func newRec() *rec { return &rec{wake: make(chan struct{}, 1)} }

func (r *rec) emit(c task.Command) {
	if r.gate != nil {
		<-r.gate
	}
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

func (r *rec) kinds() []task.CommandKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []task.CommandKind
	for _, c := range r.list {
		out = append(out, c.Kind)
	}
	return out
}

func is(k task.CommandKind) func(task.Command) bool {
	return func(c task.Command) bool { return c.Kind == k }
}

// ends 符合這一輪的結局：turn_ended 或 exited。
func ends(c task.Command) bool { return c.Kind == task.FactTurnEnded || c.Kind == task.FactExited }

// opened 是啟動好的連線與它的事實。
type opened struct {
	conn hub.Conn
	rec  *rec
	dir  string
}

// start 依夾具啟動一個連線；失敗回錯。連線在測試結束時關閉。
func start(t *testing.T, r *Runtime, l task.LaunchSnapshot, resume task.NativeSessionID, fixtures ...string) (opened, error) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return startIn(t, r, l, dir, resume, newRec(), replayEnv(t, fixtures...))
}

func startIn(t *testing.T, r *Runtime, l task.LaunchSnapshot, dir string, resume task.NativeSessionID, x *rec, env []string) (opened, error) {
	t.Helper()
	conn, err := r.Start(context.Background(), hub.StartOptions{Launch: l, Workdir: dir, Resume: resume,
		Env: env, Emit: x.emit})
	if err != nil {
		return opened{}, err
	}
	t.Cleanup(func() { conn.Close() })
	return opened{conn: conn, rec: x, dir: dir}, nil
}

// startFacts 取啟動時的兩條事實：先是 model_reported{start}（C45），再是 native_bound；回傳報告的模型設置。
func startFacts(t *testing.T, x *rec, native task.NativeSessionID) *task.ModelSetting {
	t.Helper()
	m := x.next(t, func(task.Command) bool { return true })
	if m.Kind != task.FactModelReported || !m.ModelReported.Start {
		t.Fatalf("first fact %s %+v, want model_reported{start}", m.Kind, m.ModelReported)
	}
	f := x.next(t, func(task.Command) bool { return true })
	if f.Kind != task.FactNativeBound || f.Bound.NativeID != native {
		t.Fatalf("second fact %s %+v, NativeID %q", f.Kind, f.Bound, native)
	}
	return m.ModelReported.Setting
}

// mustStart 啟動並確認開頭是 model_reported{start} 與 native_bound。
func mustStart(t *testing.T, r *Runtime, l task.LaunchSnapshot, resume task.NativeSessionID, fixtures ...string) opened {
	t.Helper()
	o, err := start(t, r, l, resume, fixtures...)
	if err != nil {
		if xe, ok := err.(hub.ExitError); ok {
			t.Fatalf("start: %v; stderr: %s", err, xe.StderrTail())
		}
		t.Fatalf("start: %v", err)
	}
	startFacts(t, o.rec, o.conn.NativeID())
	return o
}

func (o opened) prompt(t *testing.T, p task.Prompt) {
	t.Helper()
	if err := o.conn.Prompt(context.Background(), p); err != nil {
		t.Fatalf("prompt: %v", err)
	}
}

// procCommand 是依夾具扮演 agent 的測試二進位（給 Capabilities）。
func procCommand(t *testing.T, f string) proc.Command {
	return proc.Command{Path: os.Args[0], Env: replayEnv(t, f), Dir: t.TempDir()}
}

// wantCode 確認錯誤帶這個代碼與參數。
func wantCode(t *testing.T, err error, code string, params map[string]string) {
	t.Helper()
	c, ok := err.(interface {
		ErrorCode() string
		ErrorParams() map[string]string
	})
	if !ok || c.ErrorCode() != code {
		t.Fatalf("error %v, want %s", err, code)
	}
	for k, v := range params {
		if c.ErrorParams()[k] != v {
			t.Fatalf("%s param %s = %q, want %q", code, k, c.ErrorParams()[k], v)
		}
	}
}
