//go:build unix

package codex

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

// 夾具的常用行（測試二進位扮演 Codex，見 contract.Main）。請求的 id 依序是 initialize 1、model/list 2、
// thread/start 或 thread/resume 3、turn/start 4、5……
const (
	lInit     = `{"client":{"id":1,"method":"initialize","params":{"clientInfo":{"name":"orielhub"}}}}`
	lInitOK   = `{"agent":{"id":1,"result":{"userAgent":"codex/test","codexHome":"/tmp/codex","platformFamily":"unix","platformOs":"macos"}}}`
	lInited   = `{"client":{"method":"initialized"}}`
	lModels   = `{"client":{"id":2,"method":"model/list"}}`
	lModelsOK = `{"agent":{"id":2,"result":{"data":[` +
		`{"id":"gpt-a","model":"gpt-a","displayName":"GPT A","description":"The default","hidden":false},` +
		`{"id":"gpt-b","model":"gpt-b","displayName":"GPT B","description":"Quick","hidden":false},` +
		`{"id":"gpt-h","model":"gpt-h","displayName":"Hidden","hidden":true}]}}}`
	lStart   = `{"client":{"id":3,"method":"thread/start","params":{"cwd":"<cwd>","approvalPolicy":"untrusted","approvalsReviewer":"user","sandbox":"workspace-write"}}}`
	lStartOK = `{"agent":{"id":3,"result":{"thread":{"id":"thr_1"},"model":"gpt-a","approvalPolicy":"untrusted","approvalsReviewer":"user"}}}`
)

// started 是會話建好為止的各行。
var started = []string{lInit, lInitOK, lInited, lModels, lModelsOK, lStart, lStartOK}

// startedWith 是 thread/start 回 result 的會話建好為止的各行。
func startedWith(result string) []string {
	return []string{lInit, lInitOK, lInited, lModels, lModelsOK, lStart, `{"agent":{"id":3,"result":` + result + `}}`}
}

// lTurn 是第 n 輪的 turn/start（id 為 id）、它的回應與 turn/started，這一輪的 ID 是 turn_<n>。
func lTurn(id, n int) []string {
	return []string{fmt.Sprintf(`{"client":{"id":%d,"method":"turn/start","params":{"threadId":"thr_1"}}}`, id),
		fmt.Sprintf(`{"agent":{"id":%d,"result":{"turn":{"id":"turn_%d","status":"inProgress"}}}}`, id, n),
		fmt.Sprintf(`{"agent":{"method":"turn/started","params":{"threadId":"thr_1","turn":{"id":"turn_%d","status":"inProgress"}}}}`, n)}
}

// lNote 是 Codex 送的一則通知。
func lNote(method, params string) string {
	return `{"agent":{"method":"` + method + `","params":` + params + `}}`
}

// lItem 是第 1 輪一個條目的開始或完成（item 是 ThreadItem 的 JSON）。
func lItem(done bool, item string) string {
	m := "item/started"
	if done {
		m = "item/completed"
	}
	return lNote(m, `{"threadId":"thr_1","turnId":"turn_1","item":`+item+`}`)
}

// lDone 是第 n 輪的 turn/completed；turn 的其餘欄位（status、error）由 fields 給。
func lDone(n int, fields string) string {
	return lNote("turn/completed", fmt.Sprintf(`{"threadId":"thr_1","turn":{"id":"turn_%d",%s}}`, n, fields))
}

// lOK 是第 n 輪成功結束。
func lOK(n int) string { return lDone(n, `"status":"completed"`) }

// lUsage 是會話的累計用量。
func lUsage(in, cached, out, total int) string {
	return lNote("thread/tokenUsage/updated", fmt.Sprintf(`{"threadId":"thr_1","turnId":"turn_1","tokenUsage":{"total":`+
		`{"inputTokens":%d,"cachedInputTokens":%d,"outputTokens":%d,"reasoningOutputTokens":0,"totalTokens":%d}}}`, in, cached, out, total))
}

// lAgent 是 Codex 寫出的一則訊息。
func lAgent(msg string) string { return `{"agent":` + msg + `}` }

// fixture 把夾具寫進暫存檔。
func fixture(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.ndjson")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testLaunch 是扮演 Codex 的測試二進位的 Launch。
func testLaunch() task.LaunchSnapshot {
	return task.LaunchSnapshot{Agent: "codex", AgentName: "Codex", Protocol: task.ProtocolCodex,
		Executable: os.Args[0], Approval: task.ApprovalOrielHub, Caps: task.Capabilities{Resume: true}}
}

// replayEnv 是讓測試二進位依夾具扮演 Codex 的環境（多個夾具依次給每次啟動）。
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
