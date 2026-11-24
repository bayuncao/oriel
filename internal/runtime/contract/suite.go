package contract

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
)

// 開啟真實 CLI 的環境變數（K03、K04）。
const (
	EnvLive   = "ORIELHUB_LIVE_AGENTS" // 逗號分隔的 agent ID
	EnvRecord = "ORIELHUB_RECORD"      // 設為 1 時把真實執行錄成夾具
)

// Synthetic 是手寫夾具的目錄名：一個最小的 agent，涵蓋每個用例；錄下的
// agent 缺的用例（錄不出來的 crash）也用它的（K05）。
const Synthetic = "synthetic"

// Suite 是一個轉接器的契約測試（K01）。
type Suite struct {
	Runtime hub.Runtime
	// Dir 放夾具：<Dir>/<agent>/<用例>.ndjson；回放時每個子目錄是一個 agent。
	Dir string
	// Live 回傳真實 CLI 的 Launch（Caps 已探測）；找不到時 ok 為 false。
	Live func(t *testing.T, agent string) (l task.LaunchSnapshot, ok bool)
	// Check 在真實執行的每個用例之後檢查錄下的原始訊息（協議特有的規則，如 K07）。
	Check func(t *testing.T, c string, traffic []Line)
}

// cases 是每個轉接器都要過的用例；名稱就是夾具的檔名。
var cases = []struct {
	name string
	run  func(*testing.T, *session)
}{
	{"prompt", casePrompt},
	{"tool", caseTool},
	{"deny", caseDeny},
	{"cancel", caseCancel},
	{"resume", caseResume},
	{"crash", caseCrash},
	{"model", caseModel},
}

// Run 跑全部用例：設了 ORIELHUB_LIVE_AGENTS 時對其中每個真實 CLI 跑（crash
// 錄不出來，只回放），否則回放 Dir 下每個 agent 的夾具。
func Run(t *testing.T, s Suite) {
	dir, err := filepath.Abs(s.Dir) // agent 在別的工作目錄執行
	if err != nil {
		t.Fatal(err)
	}
	s.Dir = dir
	if live := os.Getenv(EnvLive); live != "" {
		for _, a := range strings.Split(live, ",") {
			a = strings.TrimSpace(a)
			t.Run(a, func(t *testing.T) {
				l, ok := s.Live(t, a)
				if !ok {
					t.Skipf("%s is not installed", a)
				}
				for _, c := range cases {
					if c.name == "crash" {
						continue
					}
					t.Run(c.name, func(t *testing.T) { s.live(t, a, c.name, l, c.run) })
				}
			})
		}
		return
	}
	dirs, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		t.Run(d.Name(), func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					x := s.newSession(t, d.Name(), 30*time.Second)
					x.fixture = func(n int) string {
						// 錄不出來的用例（crash）用手寫的 synthetic 夾具（K05）。
						p := filepath.Join(s.Dir, d.Name(), fixtureName(c.name, n))
						if _, err := os.Stat(p); err != nil {
							p = filepath.Join(s.Dir, Synthetic, fixtureName(c.name, n))
						}
						return p
					}
					c.run(t, x)
				})
			}
		})
	}
}

func fixtureName(c string, n int) string {
	if n == 1 {
		return c + ".ndjson"
	}
	return c + "." + string(rune('0'+n)) + ".ndjson"
}

// live 對真實 CLI 跑一個用例：經錄製代理啟動，用例結束後檢查錄下的訊息，
// 設了 ORIELHUB_RECORD 就寫成夾具。
func (s Suite) live(t *testing.T, agent, c string, l task.LaunchSnapshot, run func(*testing.T, *session)) {
	x := s.newSession(t, agent, 3*time.Minute)
	x.launch = l
	// 錄下的原始訊息在用例失敗時留著，好查 agent 實際送了什麼。
	raw, err := os.MkdirTemp("", "orielhub-contract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("raw traffic kept in %s", raw)
			return
		}
		os.RemoveAll(raw)
	})
	x.record = func(n int) string { return filepath.Join(raw, fixtureName(c, n)) }
	run(t, x)
	x.closeAll()
	home, _ := os.UserHomeDir()
	for n := 1; ; n++ {
		lines, err := ReadFixture(x.record(n))
		if err != nil {
			break
		}
		if s.Check != nil {
			s.Check(t, c, lines)
		}
		if os.Getenv(EnvRecord) != "1" || t.Failed() {
			continue
		}
		replace := [][2]string{{x.cwd, PlaceCwd}, {home, PlaceHome}}
		if x.native != "" {
			replace = append([][2]string{{string(x.native), PlaceSession}}, replace...)
		}
		scrubbed, err := Scrub(lines, replace)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(s.Dir, agent)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := WriteFixture(filepath.Join(dir, fixtureName(c, n)), scrubbed); err != nil {
			t.Fatal(err)
		}
	}
}

// session 是一個用例的連線與它收到的事實。
type session struct {
	t       *testing.T
	s       Suite
	agent   string
	wait    time.Duration
	cwd     string
	launch  task.LaunchSnapshot // 真實執行時的 Launch；回放時由 start 填
	fixture func(n int) string  // 回放：第 n 次啟動的夾具
	record  func(n int) string  // 真實執行：第 n 次啟動錄到哪裡
	starts  int
	conn    hub.Conn
	conns   []hub.Conn
	native  task.NativeSessionID
	facts   *facts
	model   *task.ModelSetting // 最近一次啟動時報告的模型設置（C45）；nil = 沒有
}

func (s Suite) newSession(t *testing.T, agent string, wait time.Duration) *session {
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	x := &session{t: t, s: s, agent: agent, wait: wait, cwd: cwd}
	t.Cleanup(x.closeAll)
	return x
}

func (x *session) closeAll() {
	for _, c := range x.conns {
		c.Close()
	}
	x.conns = nil
}

// start 啟動（resume 非空時接回）一個連線；第一條事實必須是 native_bound。
func (x *session) start(resume task.NativeSessionID) error {
	x.t.Helper()
	x.starts++
	l := x.launch
	env := os.Environ()
	switch {
	case x.fixture != nil:
		l = task.LaunchSnapshot{Agent: task.AgentID(x.agent), AgentName: x.agent, Protocol: task.ProtocolACP,
			Executable: os.Args[0], Approval: task.ApprovalOrielHub, Caps: task.Capabilities{Resume: true}}
		env = append(env, EnvReplay+"="+x.fixture(x.starts))
	case x.record != nil:
		// 只換掉可執行檔：參數照常由轉接器組出，經代理的命令列原樣交給真正的 agent。
		env = append(env, EnvRecordTo+"="+x.record(x.starts), EnvRecordExe+"="+l.Executable)
		l.Executable = os.Args[0]
	}
	x.facts = &facts{wake: make(chan struct{}, 1)}
	conn, err := x.s.Runtime.Start(context.Background(), hub.StartOptions{
		Launch: l, Workdir: x.cwd, Resume: resume, Env: env, Emit: x.facts.emit})
	if err != nil {
		return err
	}
	x.conn = conn
	x.conns = append(x.conns, conn)
	f := x.next(func(c task.Command) bool { return true })
	if f.Kind == task.FactModelReported { // 模型設置在綁定之前報（C45）
		if !f.ModelReported.Start {
			x.t.Fatalf("model reported before binding without start: %+v", f.ModelReported)
		}
		x.model = f.ModelReported.Setting
		f = x.next(func(c task.Command) bool { return true })
	}
	if f.Kind != task.FactNativeBound || f.Bound.NativeID != conn.NativeID() || f.Bound.NativeID == "" {
		x.t.Fatalf("first fact %s %+v, want native_bound %q", f.Kind, f.Bound, conn.NativeID())
	}
	x.native = conn.NativeID()
	return nil
}

func (x *session) mustStart(resume task.NativeSessionID) {
	x.t.Helper()
	if err := x.start(resume); err != nil {
		if xe, ok := err.(hub.ExitError); ok {
			x.t.Fatalf("start: %v; stderr: %s", err, xe.StderrTail())
		}
		x.t.Fatalf("start: %v", err)
	}
}

func (x *session) prompt(text string) {
	x.t.Helper()
	if err := x.conn.Prompt(context.Background(), task.Prompt{ID: "pr-1", Text: text}); err != nil {
		x.t.Fatalf("prompt: %v", err)
	}
}

// next 等下一條符合 pred 的事實（略過中間的）。
func (x *session) next(pred func(task.Command) bool) task.Command {
	x.t.Helper()
	c, ok := x.facts.next(pred, x.wait)
	if !ok {
		x.t.Fatalf("no matching fact within %v; got %s", x.wait, x.facts.kinds())
	}
	return c
}

// turnEnd 等這一輪結束（turn_ended，或行程結束）。
func (x *session) turnEnd() task.Command {
	x.t.Helper()
	return x.next(func(c task.Command) bool { return c.Kind == task.FactTurnEnded || c.Kind == task.FactExited })
}

func wantResult(t *testing.T, c task.Command, r task.TurnResult) {
	t.Helper()
	if c.Kind != task.FactTurnEnded || c.TurnEnded.Result != r {
		got := facts{list: []task.Command{c}}
		t.Fatalf("turn ended with %s, want %s", got.kinds(), r)
	}
}

// 真實執行用的 Prompt：寫成能穩定觸發要測的行為（K03）。要審批的命令會寫
// 檔案：唯讀的命令（echo）agent 自己就放行，不會問 OrielHub。
const (
	promptPong   = "Reply with exactly one word: pong. Do not use any tools."
	promptShell  = "Use your shell tool to run exactly this command: printf ok > orielhub-contract.txt. Then reply with: done."
	promptCount  = "Write the numbers from 1 to 400, one per line, with no other text. Do not use any tools."
	promptRemem  = "Remember the code word ORIEL-42. Reply with only: OK. Do not use any tools."
	promptRecall = "What code word did I ask you to remember? Reply with only the code word. Do not use any tools."
)

// model：啟動時報告的模型設置（C45）裡換成另一個，回應裡是換成的那個（C47，D118）；沒有模型設置的 agent 略過。
func caseModel(t *testing.T, x *session) {
	x.mustStart("")
	m := x.model
	if m == nil || len(m.Choices) < 2 {
		t.Skip("the agent reports no model to switch")
	}
	to := m.Choices[0].Value
	if to == m.Current {
		to = m.Choices[1].Value
	}
	got, err := x.conn.SetModel(context.Background(), task.ModelSwitch{Option: m.Option, Value: to})
	if err != nil {
		t.Fatalf("SetModel(%s): %v", to, err)
	}
	if got == nil || got.Current != to {
		t.Fatalf("after SetModel(%s) the agent reports %+v", to, got)
	}
}

// prompt：啟動、流式文字、一輪完成。
func casePrompt(t *testing.T, x *session) {
	x.mustStart("")
	x.prompt(promptPong)
	c := x.next(func(c task.Command) bool {
		return c.Kind == task.FactOutput || c.Kind == task.FactTurnEnded || c.Kind == task.FactExited
	})
	if c.Kind != task.FactOutput {
		t.Fatalf("the turn ended without output: %s", x.facts.kinds())
	}
	wantResult(t, x.turnEnd(), task.TurnOK)
}

// tool：工具呼叫要經 OrielHub 審批（K06）；允許後工具完成、一輪完成。
func caseTool(t *testing.T, x *session) {
	x.mustStart("")
	x.prompt(promptShell)
	asked := false
	for {
		c := x.next(func(c task.Command) bool {
			return c.Kind == task.FactAsked || c.Kind == task.FactToolUpdated ||
				c.Kind == task.FactTurnEnded || c.Kind == task.FactExited
		})
		switch c.Kind {
		case task.FactAsked:
			asked = true
			if c.Asked.Request.Kind != task.ToolExecute || !strings.Contains(c.Asked.Request.Command, "orielhub-contract") {
				t.Errorf("asked %+v, want an execute request for the printf", c.Asked.Request)
			}
			if err := x.conn.Answer(context.Background(), c.Asked.ID, task.Allow); err != nil {
				t.Fatal(err)
			}
		case task.FactToolUpdated:
			if c.ToolUpdated.Status == task.ToolCompleted && !asked {
				t.Fatal("a tool ran without asking OrielHub (K06)")
			}
		default:
			if !asked {
				t.Fatal("the turn ended without a permission request (K06)")
			}
			wantResult(t, c, task.TurnOK)
			return
		}
	}
}

// deny：拒絕後這一輪照樣結束，行程不退出。
func caseDeny(t *testing.T, x *session) {
	x.mustStart("")
	x.prompt(promptShell)
	asked := false
	for {
		c := x.next(func(c task.Command) bool {
			return c.Kind == task.FactAsked || c.Kind == task.FactTurnEnded || c.Kind == task.FactExited
		})
		switch c.Kind {
		case task.FactAsked: // agent 被拒後可能再問一次：每次都拒絕
			asked = true
			if err := x.conn.Answer(context.Background(), c.Asked.ID, task.Deny); err != nil {
				t.Fatal(err)
			}
		case task.FactExited:
			t.Fatalf("the agent exited after a denial: %+v", c.Exited)
		default:
			if !asked {
				t.Fatal("the turn ended without a permission request (K06)")
			}
			return
		}
	}
}

// cancel：輸出開始後取消，這一輪以 cancelled 結束。
func caseCancel(t *testing.T, x *session) {
	x.mustStart("")
	x.prompt(promptCount)
	c := x.next(func(c task.Command) bool {
		return c.Kind == task.FactOutput || c.Kind == task.FactTurnEnded || c.Kind == task.FactExited
	})
	if c.Kind != task.FactOutput {
		t.Fatalf("the turn ended before any output: %s", x.facts.kinds())
	}
	if err := x.conn.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantResult(t, x.turnEnd(), task.TurnCancelled)
}

// resume：關掉連線後以原生 ID 接回，接著對話。
func caseResume(t *testing.T, x *session) {
	if x.fixture == nil && !x.launch.Caps.Resume {
		t.Skip("the agent cannot resume sessions")
	}
	x.mustStart("")
	x.prompt(promptRemem)
	wantResult(t, x.turnEnd(), task.TurnOK)
	id := x.native
	x.closeAll()
	x.mustStart(id)
	if x.native != id {
		t.Fatalf("resumed as %q, want %q", x.native, id)
	}
	x.prompt(promptRecall)
	var text strings.Builder
	for {
		c := x.next(func(c task.Command) bool {
			return c.Kind == task.FactOutput || c.Kind == task.FactTurnEnded || c.Kind == task.FactExited
		})
		if c.Kind == task.FactOutput {
			if c.Output.Channel == task.ChannelText {
				text.WriteString(c.Output.Text)
			}
			continue
		}
		wantResult(t, c, task.TurnOK)
		break
	}
	if !strings.Contains(strings.ToUpper(text.String()), "ORIEL-42") {
		t.Fatalf("after resuming the agent answered %q", text.String())
	}
}

// crash：一輪中途退出，只報 exited（原因與 stderr 尾部），不另報 turn_ended。
func caseCrash(t *testing.T, x *session) {
	x.mustStart("")
	x.prompt(promptPong)
	c := x.turnEnd()
	if c.Kind != task.FactExited || c.Exited.Problem.Code != "proc.exited" ||
		c.Exited.Problem.Params["status"] != "3" || c.Exited.StderrTail != "boom" {
		t.Fatalf("got %s %+v", c.Kind, c.Exited)
	}
}

// facts 收集轉接器發出的事實；next 從上次的位置往後找。
type facts struct {
	mu     sync.Mutex
	list   []task.Command
	cursor int
	wake   chan struct{}
}

func (f *facts) emit(c task.Command) {
	f.mu.Lock()
	f.list = append(f.list, c)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *facts) next(pred func(task.Command) bool, wait time.Duration) (task.Command, bool) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		f.mu.Lock()
		for f.cursor < len(f.list) {
			c := f.list[f.cursor]
			f.cursor++
			if pred(c) {
				f.mu.Unlock()
				return c, true
			}
		}
		f.mu.Unlock()
		select {
		case <-f.wake:
		case <-deadline.C:
			return task.Command{}, false
		}
	}
}

func (f *facts) kinds() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.list {
		s := string(c.Kind)
		switch {
		case c.TurnEnded != nil:
			s += "(" + string(c.TurnEnded.Result)
			if c.TurnEnded.Problem != nil {
				s += " " + c.TurnEnded.Problem.Code + " " + strings.Join(slices.Sorted(maps.Values(c.TurnEnded.Problem.Params)), " ")
			}
			s += ")"
		case c.Exited != nil:
			s += "(" + c.Exited.Problem.Code + " " + c.Exited.StderrTail + ")"
		}
		out = append(out, s)
	}
	return strings.Join(slices.Compact(out), " ")
}
