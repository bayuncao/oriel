//go:build unix

package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/runtime/contract"
	"github.com/bayuncao/oriel/internal/task"
)

// C01：initialize 宣告協議版本 1 與客戶端資訊，不提供檔案讀寫與終端；
// session/new 在工作目錄、沒有 MCP 伺服器；第一條事實是 native_bound。
// 經錄製代理看 OrielHub 實際送出的訊息。
func TestStartSession(t *testing.T) {
	r := newRuntime(t, Options{Version: "1.2.3"})
	f := fixture(t, append(started, lPrompt(3, "hi"), lChunk("hello"), lResult(3, `{"stopReason":"end_turn"}`))...)
	raw := filepath.Join(t.TempDir(), "raw.ndjson")
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	env := append(replayEnv(t, f), contract.EnvRecordTo+"="+raw, contract.EnvRecordExe+"="+os.Args[0])
	x := newRec()
	o, err := startIn(t, r, testLaunch(), dir, "", x, env)
	if err != nil {
		t.Fatal(err)
	}
	if m := startFacts(t, x, "s1"); m != nil || o.conn.NativeID() != "s1" {
		t.Fatalf("model %+v, NativeID %q", m, o.conn.NativeID())
	}
	o.prompt(t, task.Prompt{Text: "hi"})
	if c := x.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnOK {
		t.Fatalf("turn ended %+v", c)
	}
	lines, err := contract.ReadFixture(raw)
	if err != nil {
		t.Fatal(err)
	}
	var sent []map[string]any
	for _, l := range lines {
		if l.Client != nil {
			var m map[string]any
			_ = json.Unmarshal(l.Client, &m)
			sent = append(sent, m)
		}
	}
	init := sent[0]["params"].(map[string]any)
	caps, _ := json.Marshal(init["clientCapabilities"])
	if init["protocolVersion"] != 1.0 || strings.Contains(string(caps), "true") ||
		!reflect.DeepEqual(init["clientInfo"], map[string]any{"name": "OrielHub", "version": "1.2.3"}) {
		t.Fatalf("initialize %v", init)
	}
	nw := sent[1]["params"].(map[string]any)
	if sent[1]["method"] != "session/new" || nw["cwd"] != dir || !reflect.DeepEqual(nw["mcpServers"], []any{}) || nw["_meta"] != nil {
		t.Fatalf("session/new %v", sent[1])
	}
}

// C02–C05、C08：啟動失敗的原因；行程都已結束。
func TestStartFailures(t *testing.T) {
	exit2 := 2
	early, _ := json.Marshal(contract.Line{Stderr: "not logged in"})
	quit, _ := json.Marshal(contract.Line{Exit: &exit2})
	for _, tc := range []struct {
		id      string
		lines   []string
		resume  task.NativeSessionID
		timeout time.Duration
		code    string
		params  map[string]string
		tail    string // 非空時要求 hub.ExitError
	}{
		{id: "C02", lines: []string{lInit, `{"agent":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":2}}}`},
			code: "acp.protocol_version", params: map[string]string{"version": "2"}},
		{id: "C03", lines: []string{lInit, lInitOK, lNew,
			`{"agent":{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"Authentication required"}}}`},
			code: "acp.auth_required", params: map[string]string{"agent": "Test Agent"}},
		{id: "C03 other error", lines: []string{lInit,
			`{"agent":{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error","data":{"error":"no config"}}}}`},
			code: "acp.request_failed", params: map[string]string{"method": "initialize", "detail": "Internal error: no config"}},
		{id: "C04", lines: []string{lInit, string(early), string(quit)},
			code: "proc.exited", params: map[string]string{"status": "2"}, tail: "not logged in"},
		{id: "C05", lines: []string{lInit}, timeout: time.Second, code: "acp.start_timed_out"},
		{id: "C08", lines: []string{lInit, `{"agent":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{}}}}`},
			resume: "s1", code: "acp.resume_unsupported"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			r := newRuntime(t, Options{StartTimeout: tc.timeout})
			_, err := start(t, r, testLaunch(), tc.resume, fixture(t, tc.lines...))
			wantCode(t, err, tc.code, tc.params)
			xe, isExit := err.(hub.ExitError)
			if (tc.tail != "") != isExit || (isExit && xe.StderrTail() != tc.tail) {
				t.Fatalf("exit error %v (tail %q), want tail %q", isExit, tail(err), tc.tail)
			}
		})
	}
}

func tail(err error) string {
	if xe, ok := err.(hub.ExitError); ok {
		return xe.StderrTail()
	}
	return ""
}

// C06、C07：以 session/resume 接回；只有 session/load 時用它，丟掉重放的歷史。
func TestResume(t *testing.T) {
	t.Run("C06", func(t *testing.T) {
		r := newRuntime(t, Options{})
		o := mustStart(t, r, testLaunch(), "s1", fixture(t, lInit,
			`{"agent":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"resume":{}}}}}}`,
			`{"client":{"id":2,"method":"session/resume","params":{"sessionId":"s1","cwd":"<cwd>"}}}`,
			lResult(2, `{}`)))
		if o.conn.NativeID() != "s1" {
			t.Fatalf("NativeID %q", o.conn.NativeID())
		}
	})
	t.Run("C07", func(t *testing.T) {
		r := newRuntime(t, Options{})
		o := mustStart(t, r, testLaunch(), "s1", fixture(t, lInit, lInitOK,
			`{"client":{"id":2,"method":"session/load","params":{"sessionId":"s1","cwd":"<cwd>","mcpServers":[]}}}`,
			lUpdate(`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"old question"}}`),
			lChunk("old answer"),
			lUpdate(`{"sessionUpdate":"tool_call","toolCallId":"old","title":"old","kind":"read","status":"completed"}`),
			lResult(2, `{}`),
			lPrompt(3, "again"), lChunk("new answer"), lResult(3, `{"stopReason":"end_turn"}`)))
		o.prompt(t, task.Prompt{Text: "again"})
		o.rec.next(t, ends)
		var got []string
		for _, c := range o.rec.list[1:] {
			switch {
			case c.Output != nil:
				got = append(got, "output:"+c.Output.Text)
			case c.ToolStarted != nil:
				got = append(got, "tool:"+string(c.ToolStarted.ID))
			}
		}
		if !reflect.DeepEqual(got, []string{"output:new answer"}) {
			t.Fatalf("facts %v; the replayed history must be dropped", got)
		}
	})
}

// C09、C13–C21：一輪裡各種更新變成哪些事實，順序不變。
func TestUpdates(t *testing.T) {
	r := newRuntime(t, Options{})
	lines := append(append([]string{}, started...), lPrompt(3, "go"),
		`{"raw":"Starting agent v1 (not JSON)"}`,
		lUpdate(`{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"hmm"}}`),
		lChunk("hi"),
		lUpdate(`{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"AAAA","mimeType":"image/png"}}`),
		lUpdate(`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"git commit","kind":"execute","status":"pending","rawInput":{"command":["git","commit","-m","a b"]},"locations":[{"path":"/x/a.go"}]}`),
		lUpdate(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"out1"}}]}`),
		lUpdate(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","content":[{"type":"content","content":{"type":"text","text":"out2"}},{"type":"diff","path":"/x/a.go","newText":"x"}]}`),
		lUpdate(`{"sessionUpdate":"tool_call_update","toolCallId":"t1"}`),
		lUpdate(`{"sessionUpdate":"tool_call_update","toolCallId":"t2","title":"Read","kind":"read","status":"completed"}`),
		lUpdate(`{"sessionUpdate":"tool_call","toolCallId":"t3","title":"mode","kind":"switch_mode","status":"completed"}`),
		// 分類、標題與路徑在更新裡才給出（Grok 的錄製即如此）：tool_updated 帶上目前的值；標題裡行內代碼的反引號去掉（C17a）。
		lUpdate(`{"sessionUpdate":"tool_call","toolCallId":"t4","title":"\u0060read_file\u0060","status":"pending"}`),
		lUpdate(`{"sessionUpdate":"tool_call_update","toolCallId":"t4","title":"Read \u0060b.go\u0060","kind":"read","status":"completed","locations":[{"path":"/x/b.go"}]}`),
		lUpdate(`{"sessionUpdate":"plan","entries":[{"content":"a","priority":"high","status":"completed"},{"content":"b","priority":"low","status":"in_progress"},{"content":"c","priority":"low","status":"pending"}]}`),
		lUpdate(`{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"x","description":"y"}]}`),
		lUpdate(`{"sessionUpdate":"current_mode_update","currentModeId":"m"}`),
		lUpdate(`{"sessionUpdate":"session_info_update","title":"T"}`),
		lUpdate(`{"sessionUpdate":"usage_update","used":1,"size":2}`),
		lUpdate(`{"sessionUpdate":"something_new","x":1}`),
		`{"agent":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"other","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"not mine"}}}}}`,
		`{"agent":{"jsonrpc":"2.0","method":"_vendor/progress","params":{"x":1}}}`,
		lResult(3, `{"stopReason":"end_turn","usage":{"inputTokens":100,"cachedReadTokens":30,"cachedWriteTokens":10,"outputTokens":20,"thoughtTokens":5,"totalTokens":125}}`))
	o := mustStart(t, r, testLaunch(), "", fixture(t, lines...))
	o.prompt(t, task.Prompt{Text: "go"})
	o.rec.next(t, ends)
	input := `{"command":["git","commit","-m","a b"]}`
	want := []task.Command{
		{Kind: task.FactOutput, Output: &task.Output{Channel: task.ChannelThinking, Text: "hmm"}},
		{Kind: task.FactOutput, Output: &task.Output{Channel: task.ChannelText, Text: "hi"}},
		{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{ID: "t1", Tool: "execute", Kind: task.ToolExecute,
			Title: "git commit", Input: input, Paths: []string{"/x/a.go"}}},
		{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{ID: "t1", Status: task.ToolRunning, Kind: task.ToolExecute, Title: "git commit", Paths: []string{"/x/a.go"}, Output: "out1"}},
		{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{ID: "t1", Status: task.ToolRunning, Kind: task.ToolExecute, Title: "git commit", Paths: []string{"/x/a.go"}, Output: "out2"}},
		{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{ID: "t1", Status: task.ToolRunning, Kind: task.ToolExecute, Title: "git commit", Paths: []string{"/x/a.go"}, Output: "out2"}},
		{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{ID: "t2", Tool: "read", Kind: task.ToolRead, Title: "Read"}},
		{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{ID: "t2", Status: task.ToolCompleted, Kind: task.ToolRead, Title: "Read"}},
		{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{ID: "t3", Tool: "other", Kind: task.ToolOther, Title: "mode"}},
		{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{ID: "t3", Status: task.ToolCompleted, Kind: task.ToolOther, Title: "mode"}},
		{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{ID: "t4", Tool: "other", Kind: task.ToolOther, Title: "read_file"}},
		{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{ID: "t4", Status: task.ToolCompleted, Kind: task.ToolRead,
			Title: "Read b.go", Paths: []string{"/x/b.go"}}},
		{Kind: task.FactPlan, Plan: &task.PlanUpdate{Entries: []task.PlanEntry{
			{Text: "a", Status: task.PlanCompleted}, {Text: "b", Status: task.PlanInProgress}, {Text: "c", Status: task.PlanPending}}}},
		{Kind: task.FactUsage, Usage: &task.Usage{InputTokens: 60, CacheReadTokens: 30, CacheWriteTokens: 10, OutputTokens: 25}},
		{Kind: task.FactTurnEnded, TurnEnded: &task.TurnEnded{Result: task.TurnOK}},
	}
	got := o.rec.list[2:] // 略過啟動時的 model_reported 與 native_bound
	for i := range max(len(got), len(want)) {
		var g, w task.Command
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		if !reflect.DeepEqual(normalize(g), normalize(w)) {
			gj, _ := json.Marshal(g)
			wj, _ := json.Marshal(w)
			t.Fatalf("fact %d\n got %s\nwant %s", i, gj, wj)
		}
	}
}

// normalize 讓空的與 nil 的 Paths 相等。
func normalize(c task.Command) task.Command {
	if c.ToolStarted != nil && len(c.ToolStarted.Paths) == 0 {
		x := *c.ToolStarted
		x.Paths = nil
		c.ToolStarted = &x
	}
	return c
}

// C22、C23：stopReason 與 Prompt 回錯時這一輪的結局。
func TestTurnResults(t *testing.T) {
	for _, tc := range []struct {
		id, response string
		result       task.TurnResult
		code         string
		params       map[string]string
	}{
		{"end_turn", `{"result":{"stopReason":"end_turn"}}`, task.TurnOK, "", nil},
		{"cancelled", `{"result":{"stopReason":"cancelled"}}`, task.TurnCancelled, "", nil},
		{"max_tokens", `{"result":{"stopReason":"max_tokens"}}`, task.TurnError, "acp.max_tokens", nil},
		{"max_turn_requests", `{"result":{"stopReason":"max_turn_requests"}}`, task.TurnError, "acp.max_turn_requests", nil},
		{"refusal", `{"result":{"stopReason":"refusal"}}`, task.TurnError, "acp.refusal", nil},
		{"error", `{"error":{"code":-32603,"message":"Internal error","data":"serialization error"}}`, task.TurnError,
			"acp.prompt_failed", map[string]string{"detail": "Internal error: serialization error"}},
		{"auth", `{"error":{"code":-32000,"message":"Authentication required"}}`, task.TurnError,
			"acp.auth_required", map[string]string{"agent": "Test Agent"}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			r := newRuntime(t, Options{})
			resp := `{"agent":{"jsonrpc":"2.0","id":3,` + tc.response[1:] + `}`
			o := mustStart(t, r, testLaunch(), "", fixture(t, append(started, lPrompt(3, "go"), resp)...))
			o.prompt(t, task.Prompt{Text: "go"})
			c := o.rec.next(t, ends)
			if c.Kind != task.FactTurnEnded || c.TurnEnded.Result != tc.result {
				t.Fatalf("got %+v", c)
			}
			if tc.code == "" {
				if c.TurnEnded.Problem != nil {
					t.Fatalf("problem %+v", c.TurnEnded.Problem)
				}
				return
			}
			p := c.TurnEnded.Problem
			if p == nil || p.Code != tc.code || !reflect.DeepEqual(p.Params, tc.params) && tc.params != nil {
				t.Fatalf("problem %+v, want %s %v", p, tc.code, tc.params)
			}
		})
	}
}

// C24：一輪中途退出時只報 exited；退出前送出的更新都在它之前。
func TestExitMidTurn(t *testing.T) {
	r := newRuntime(t, Options{})
	exit3 := 3
	lines := append(append([]string{}, started...), lPrompt(3, "go"))
	for i := range 50 {
		lines = append(lines, lChunk(fmt.Sprint(i)))
	}
	boom, _ := json.Marshal(contract.Line{Stderr: "boom"})
	quit, _ := json.Marshal(contract.Line{Exit: &exit3})
	o := mustStart(t, r, testLaunch(), "", fixture(t, append(lines, string(boom), string(quit))...))
	o.prompt(t, task.Prompt{Text: "go"})
	c := o.rec.next(t, ends)
	if c.Kind != task.FactExited || c.Exited.Problem.Code != "proc.exited" || c.Exited.Problem.Params["status"] != "3" || c.Exited.StderrTail != "boom" {
		t.Fatalf("got %+v", c)
	}
	var texts []string
	for _, f := range o.rec.list {
		if f.Output != nil {
			texts = append(texts, f.Output.Text)
		}
		if f.Kind == task.FactTurnEnded {
			t.Fatal("turn_ended after the agent exited")
		}
	}
	if len(texts) != 50 || texts[49] != "49" {
		t.Fatalf("%d outputs before exited, want 50", len(texts))
	}
}

// C25：hub 處理得慢時讀取不停：agent 寫完全部更新（建立 done 檔）之前
// 事實一條都還沒交出去，之後依序全部交出。
func TestBurst(t *testing.T) {
	r := newRuntime(t, Options{})
	const n = 5000
	done := filepath.Join(t.TempDir(), "done")
	lines := append(append([]string{}, started...), lPrompt(3, "go"))
	for i := range n {
		lines = append(lines, lChunk(fmt.Sprint(i)))
	}
	touch, _ := json.Marshal(contract.Line{Touch: done})
	lines = append(lines, lResult(3, `{"stopReason":"end_turn"}`), string(touch))
	x := newRec()
	x.gate = make(chan struct{}) // 連 native_bound 都還沒交出去
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	o, err := startIn(t, r, testLaunch(), dir, "", x, replayEnv(t, fixture(t, lines...)))
	if err != nil {
		t.Fatal(err)
	}
	o.prompt(t, task.Prompt{Text: "go"})
	deadline := time.Now().Add(testWait)
	for {
		if _, err := os.Stat(done); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent was blocked while the hub was slow")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(x.gate)
	x.next(t, ends)
	if x.list[0].Kind != task.FactModelReported || x.list[1].Kind != task.FactNativeBound {
		t.Fatalf("first facts %s, %s", x.list[0].Kind, x.list[1].Kind)
	}
	i := 0
	for _, f := range x.list {
		if f.Output != nil {
			if f.Output.Text != fmt.Sprint(i) {
				t.Fatalf("output %d is %q", i, f.Output.Text)
			}
			i++
		}
	}
	if i != n {
		t.Fatalf("%d outputs, want %d", i, n)
	}
}

// C26：超過 10 MiB 的一行、或 stdout 關了行程還在，都是協議錯誤。
func TestProtocolErrors(t *testing.T) {
	long, _ := json.Marshal(contract.Line{Raw: strings.Repeat("x", maxLine+1)})
	closed, _ := json.Marshal(contract.Line{CloseStdout: true})
	for _, tc := range []struct{ id, line, detail string }{
		{"long line", string(long), "a line longer than 10 MiB"},
		{"stdout closed", string(closed), "stdout closed while the agent kept running"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			r := newRuntime(t, Options{})
			o := mustStart(t, r, testLaunch(), "", fixture(t, append(started, lPrompt(3, "go"), tc.line)...))
			o.prompt(t, task.Prompt{Text: "go"})
			c := o.rec.next(t, ends)
			if c.Kind != task.FactExited || c.Exited.Problem.Code != "acp.protocol_error" || c.Exited.Problem.Params["detail"] != tc.detail {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

// lAsk 是 agent 的第 id 個權限請求。
func lAsk(id, tool, options string) string {
	return fmt.Sprintf(`{"agent":{"jsonrpc":"2.0","id":%q,"method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":%q},"options":%s}}}`, id, tool, options)
}

// lAnswer 是 OrielHub 對權限請求 id 的回應；optionID 空表示 cancelled。
func lAnswer(id, optionID string) string {
	if optionID == "" {
		return fmt.Sprintf(`{"client":{"id":%q,"result":{"outcome":{"outcome":"cancelled"}}}}`, id)
	}
	return fmt.Sprintf(`{"client":{"id":%q,"result":{"outcome":{"outcome":"selected","optionId":%q}}}}`, id, optionID)
}

const (
	optAllowOnce    = `{"optionId":"once","name":"Allow","kind":"allow_once"}`
	optAllowAlways  = `{"optionId":"always","name":"Always","kind":"allow_always"}`
	optRejectOnce   = `{"optionId":"no","name":"Reject","kind":"reject_once"}`
	optRejectAlways = `{"optionId":"never","name":"Never","kind":"reject_always"}`
)

// C17、C27–C29、C31：權限請求成為 asked，回答選對選項（D99）。
func TestPermissions(t *testing.T) {
	r := newRuntime(t, Options{})
	lines := append(append([]string{}, started...), lPrompt(3, "go"),
		lUpdate(`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"rm","kind":"execute","rawInput":{"command":"rm -rf build"},"locations":[{"path":"/x/build"}]}`),
		lAsk("r1", "t1", "["+optAllowOnce+","+optAllowAlways+","+optRejectOnce+"]"),
		lAnswer("r1", "once"),
		lAsk("r2", "t1", "["+optAllowAlways+","+optRejectAlways+"]"),
		lAnswer("r2", "always"),
		lAsk("r3", "t1", "["+optRejectAlways+","+optAllowOnce+"]"),
		lAnswer("r3", "never"),
		lAsk("r4", "t1", "["+optAllowOnce+"]"),
		lAnswer("r4", ""),
		lAsk("r5", "t9", "["+optAllowOnce+"]"),
		lAnswer("r5", "once"),
		lResult(3, `{"stopReason":"end_turn"}`))
	o := mustStart(t, r, testLaunch(), "", fixture(t, lines...))
	o.prompt(t, task.Prompt{Text: "go"})
	ids := map[task.PermissionID]bool{}
	for _, v := range []task.Verdict{task.Allow, task.Allow, task.Deny, task.Deny} {
		a := o.rec.next(t, is(task.FactAsked))
		want := task.PermissionRequest{Tool: "execute", Kind: task.ToolExecute, Title: "rm", Command: "rm -rf build",
			Paths: []string{"/x/build"}, Input: `{"command":"rm -rf build"}`}
		if !reflect.DeepEqual(a.Asked.Request, want) || ids[a.Asked.ID] || a.Asked.ID == "" {
			t.Fatalf("asked %+v (seen %v)", a.Asked, ids)
		}
		ids[a.Asked.ID] = true
		if err := o.conn.Answer(context.Background(), a.Asked.ID, v); err != nil {
			t.Fatal(err)
		}
		if err := o.conn.Answer(context.Background(), a.Asked.ID, v); err != nil { // C31：已回答
			t.Fatal(err)
		}
	}
	// C17：沒出現過的工具呼叫先補 tool_started。
	s := o.rec.next(t, func(c task.Command) bool { return c.Kind == task.FactToolStarted || c.Kind == task.FactAsked })
	if s.Kind != task.FactToolStarted || s.ToolStarted.ID != "t9" || s.ToolStarted.Kind != task.ToolOther {
		t.Fatalf("got %+v, want tool_started t9", s)
	}
	a := o.rec.next(t, is(task.FactAsked))
	if err := o.conn.Answer(context.Background(), a.Asked.ID, task.Allow); err != nil {
		t.Fatal(err)
	}
	if c := o.rec.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnOK {
		t.Fatalf("got %+v; stderr may show a fixture mismatch", c)
	}
}

// C33–C35：取消時掛著的請求回 cancelled；agent 回錯也算取消；沒有一輪時無事。
func TestCancel(t *testing.T) {
	r := newRuntime(t, Options{})
	lines := append(append([]string{}, started...), lPrompt(3, "one"),
		lAsk("r1", "t1", "["+optAllowOnce+"]"),
		`{"client":{"method":"session/cancel","params":{"sessionId":"s1"}}}`,
		lAnswer("r1", ""),
		lResult(3, `{"stopReason":"cancelled"}`),
		lPrompt(4, "two"), lChunk("x"),
		`{"client":{"method":"session/cancel","params":{"sessionId":"s1"}}}`,
		`{"agent":{"jsonrpc":"2.0","id":4,"error":{"code":-32603,"message":"Internal error","data":"aborted"}}}`)
	o := mustStart(t, r, testLaunch(), "", fixture(t, lines...))
	if err := o.conn.Cancel(context.Background()); err != nil { // C35
		t.Fatal(err)
	}
	o.prompt(t, task.Prompt{Text: "one"})
	a := o.rec.next(t, is(task.FactAsked))
	if err := o.conn.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := o.rec.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnCancelled {
		t.Fatalf("got %+v", c)
	}
	if err := o.conn.Answer(context.Background(), a.Asked.ID, task.Allow); err != nil { // 已取消的請求
		t.Fatal(err)
	}
	o.prompt(t, task.Prompt{Text: "two"})
	o.rec.next(t, is(task.FactOutput))
	if err := o.conn.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := o.rec.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnCancelled {
		t.Fatalf("C34: got %+v", c)
	}
}

// C36：Close 結束行程，可以重複呼叫。
func TestClose(t *testing.T) {
	r := newRuntime(t, Options{})
	o := mustStart(t, r, testLaunch(), "", fixture(t, started...))
	c := o.conn.(*conn)
	o.conn.Close()
	select {
	case <-c.proc.Done():
	default:
		t.Fatal("Close returned before the agent process ended")
	}
	o.conn.Close()
}

// C11、C12：工作目錄裡的檔案作為 resource_link；blob 附件還不支援。
func TestAttachments(t *testing.T) {
	r := newRuntime(t, Options{})
	link := `{"client":{"id":3,"method":"session/prompt","params":{"sessionId":"s1","prompt":[` +
		`{"type":"text","text":"read"},` +
		`{"type":"resource_link","name":"notes.md","uri":"file://<cwd>/docs/notes.md","mimeType":"text/markdown"},` +
		`{"type":"resource_link","name":"b.go","uri":"file:///abs/b.go"}]}}}`
	o := mustStart(t, r, testLaunch(), "", fixture(t, append(started, link, lResult(3, `{"stopReason":"end_turn"}`))...))
	// blob 附件只讓這一輪失敗，行程照常，下一輪照樣送得出去。
	o.prompt(t, task.Prompt{Text: "x", Attachments: []task.Attachment{{Name: "a.png", Blob: "abc"}}})
	if c := o.rec.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnError ||
		c.TurnEnded.Problem.Code != "acp.attachment_unsupported" || c.TurnEnded.Problem.Params["name"] != "a.png" {
		t.Fatalf("got %+v", c)
	}
	o.prompt(t, task.Prompt{Text: "read", Attachments: []task.Attachment{
		{Name: "notes.md", MIME: "text/markdown", Path: "docs/notes.md"}, {Path: "/abs/b.go"}}})
	if c := o.rec.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnOK {
		t.Fatalf("got %+v", c)
	}
}

// C39：探測 initialize 回報的能力。
func TestCapabilities(t *testing.T) {
	for _, tc := range []struct {
		id, caps string
		resume   bool
	}{
		{"load", `{"loadSession":true}`, true},
		{"resume", `{"sessionCapabilities":{"resume":{}}}`, true},
		{"neither", `{"sessionCapabilities":{"list":{}}}`, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			r := newRuntime(t, Options{})
			f := fixture(t, lInit, `{"agent":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":`+tc.caps+`}}}`)
			caps, err := r.Capabilities(context.Background(), procCommand(t, f))
			if err != nil || caps != (task.Capabilities{Resume: tc.resume}) {
				t.Fatalf("caps %+v, %v", caps, err)
			}
		})
	}
	t.Run("protocol", func(t *testing.T) {
		r := newRuntime(t, Options{})
		f := fixture(t, lInit, `{"agent":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":3}}}`)
		_, err := r.Capabilities(context.Background(), procCommand(t, f))
		wantCode(t, err, "acp.protocol_version", map[string]string{"version": "3"})
	})
}

// C40、C41：OpenCode 的權限設定經環境變數；Grok 的會話帶 yoloMode false（D101）。
func TestAgentAsk(t *testing.T) {
	t.Run("C40", func(t *testing.T) {
		r := newRuntime(t, Options{})
		echo, _ := json.Marshal(contract.Line{EchoEnv: openCodeConfig})
		exit0 := 0
		quit, _ := json.Marshal(contract.Line{Exit: &exit0})
		l := testLaunch()
		l.Agent = openCodeID
		dir, _ := filepath.EvalSymlinks(t.TempDir())
		env := append(replayEnv(t, fixture(t, string(echo), string(quit))), openCodeConfig+`={"model":"m","permission":{"read":"allow","bash":"allow"}}`)
		_, err := startIn(t, r, l, dir, "", newRec(), env)
		got := strings.TrimPrefix(tail(err), openCodeConfig+"=")
		var cfg map[string]any
		if json.Unmarshal([]byte(got), &cfg) != nil || !reflect.DeepEqual(cfg, map[string]any{"model": "m",
			"permission": map[string]any{"read": "allow", "bash": "ask", "edit": "ask", "webfetch": "ask"}}) {
			t.Fatalf("agent got %s (%v)", got, err)
		}
	})
	t.Run("C41", func(t *testing.T) {
		r := newRuntime(t, Options{})
		l := testLaunch()
		l.Agent = grokID
		mustStart(t, r, l, "", fixture(t, lInit, lInitOK,
			`{"client":{"id":2,"method":"session/new","params":{"_meta":{"yoloMode":false},"cwd":"<cwd>"}}}`, lNewOK))
		mustStart(t, r, l, "s1", fixture(t, lInit,
			`{"agent":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}}}`,
			`{"client":{"id":2,"method":"session/resume","params":{"_meta":{"yoloMode":false},"sessionId":"s1"}}}`, lResult(2, `{}`)))
	})
}

func TestOpenCodeAsk(t *testing.T) {
	for _, tc := range []struct{ id, in, want, code string }{
		{"unset", "", `{"permission":{"bash":"ask","edit":"ask","webfetch":"ask"}}`, ""},
		{"blank", "  ", `{"permission":{"bash":"ask","edit":"ask","webfetch":"ask"}}`, ""},
		{"string permission", `{"permission":"allow"}`, `{"permission":{"bash":"ask","edit":"ask","webfetch":"ask"}}`, ""},
		{"not JSON", `{`, "", "acp.opencode_config"},
		{"null", `null`, "", "acp.opencode_config"},
		{"array", `[1]`, "", "acp.opencode_config"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			env := []string{"A=1"}
			if tc.in != "" {
				env = append(env, openCodeConfig+"="+tc.in)
			}
			got, err := openCodeAsk(env)
			if tc.code != "" {
				wantCode(t, err, tc.code, nil)
				return
			}
			if err != nil || got.Get(openCodeConfig) != tc.want || got.Get("A") != "1" {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

// C43：Prompt 在 session/prompt 寫出後才返回，緊接著的取消一定排在它後面。
func TestCancelRightAfterPrompt(t *testing.T) {
	for range 20 {
		r := newRuntime(t, Options{})
		o := mustStart(t, r, testLaunch(), "", fixture(t, append(started, lPrompt(3, "go"),
			`{"client":{"method":"session/cancel","params":{"sessionId":"s1"}}}`,
			lResult(3, `{"stopReason":"cancelled"}`))...))
		o.prompt(t, task.Prompt{Text: "go"})
		if err := o.conn.Cancel(context.Background()); err != nil {
			t.Fatal(err)
		}
		if c := o.rec.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnCancelled {
			t.Fatalf("got %+v; the cancel overtook the prompt", c)
		}
		o.conn.Close()
	}
}

// C44：agent 回應完這一輪就退出：這一輪照樣完成，之後才是 exited。
func TestExitRightAfterResponse(t *testing.T) {
	exit0 := 0
	quit, _ := json.Marshal(contract.Line{Exit: &exit0})
	for range 20 {
		r := newRuntime(t, Options{})
		o := mustStart(t, r, testLaunch(), "", fixture(t, append(started, lPrompt(3, "go"), lChunk("done"),
			`{"agent":{"jsonrpc":"2.0","method":"_vendor/progress","params":{"x":1}}}`,
			lResult(3, `{"stopReason":"end_turn"}`), string(quit))...))
		o.prompt(t, task.Prompt{Text: "go"})
		if c := o.rec.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnOK {
			t.Fatalf("got %s %+v %+v, want the completed turn first", c.Kind, c.TurnEnded, c.Exited)
		}
		if c := o.rec.next(t, ends); c.Kind != task.FactExited {
			t.Fatalf("got %+v", c)
		}
	}
}

// C03：agent 回了錯誤後立刻結束，原因仍是它回的錯誤。
func TestErrorThenExit(t *testing.T) {
	exit1 := 1
	quit, _ := json.Marshal(contract.Line{Exit: &exit1})
	for range 20 {
		r := newRuntime(t, Options{})
		_, err := start(t, r, testLaunch(), "", fixture(t, lInit, lInitOK, lNew,
			`{"agent":{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"Authentication required"}}}`, string(quit)))
		wantCode(t, err, "acp.auth_required", map[string]string{"agent": "Test Agent"})
	}
}

// C42：agent 結束了，離開行程群組的子孫卻還開著 stdout：寬限期後照樣記為
// 結束，不會一直等下去。
func TestDescendantHoldsStdout(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl not found")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	sh := filepath.Join(dir, "agent")
	// 子行程以 setsid 離開群組，繼承 stdout，killGroup 碰不到它。
	body := "#!/bin/sh\nperl -e 'use POSIX; POSIX::setsid(); open(F, \">\", $ARGV[0]); print F $$; close F; sleep 60' \"" + pidFile + "\" &\n" +
		"while [ ! -s \"" + pidFile + "\" ]; do sleep 0.01; done\nexit 1\n"
	if err := os.WriteFile(sh, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	r := newRuntime(t, Options{})
	l := testLaunch()
	l.Executable = sh
	_, err := start(t, r, l, "")
	wantCode(t, err, "proc.exited", map[string]string{"status": "1"})
	if _, ok := err.(hub.ExitError); !ok {
		t.Fatalf("got %v, want hub.ExitError", err)
	}
}

// C04：agent 答完 initialize 就退出：建會話的請求寫不進去或沒人回，都記為
// 行程結束並帶 stderr 尾部，不是 acp.request_failed。
func TestExitAfterInitialize(t *testing.T) {
	exit2 := 2
	bye, _ := json.Marshal(contract.Line{Stderr: "bye"})
	quit, _ := json.Marshal(contract.Line{Exit: &exit2})
	for range 20 {
		r := newRuntime(t, Options{})
		_, err := start(t, r, testLaunch(), "", fixture(t, lInit, lInitOK, string(bye), string(quit)))
		wantCode(t, err, "proc.exited", map[string]string{"status": "2"})
		if tail(err) != "bye" {
			t.Fatalf("tail %q", tail(err))
		}
	}
}

// C24：agent 在兩輪之間退出（stdout 還被離開群組的子孫開著，EOF 晚到），
// 下一個 Prompt 寫不進去：不能把上一輪的回應當成這一輪的，只報 exited。
func TestPromptWriteFailsAfterExit(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl not found")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	sh := filepath.Join(dir, "agent")
	body := "#!/bin/sh\n" +
		"read l; echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{}}}'\n" +
		"read l; echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"s1\"}}'\n" +
		"read l; echo '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"stopReason\":\"end_turn\",\"usage\":{\"inputTokens\":3,\"outputTokens\":1,\"totalTokens\":4}}}'\n" +
		"perl -e 'use POSIX; POSIX::setsid(); open(F, \">\", $ARGV[0]); print F $$; close F; sleep 60' \"" + pidFile + "\" &\n" +
		"while [ ! -s \"" + pidFile + "\" ]; do sleep 0.01; done\nexit 0\n"
	if err := os.WriteFile(sh, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	r := newRuntime(t, Options{})
	l := testLaunch()
	l.Executable = sh
	o := mustStart(t, r, l, "")
	o.prompt(t, task.Prompt{Text: "one"})
	if c := o.rec.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnOK {
		t.Fatalf("got %+v", c)
	}
	<-o.conn.(*conn).proc.Done() // agent 已結束，stdout 還開著
	o.prompt(t, task.Prompt{Text: "two"})
	if c := o.rec.next(t, ends); c.Kind != task.FactExited {
		t.Fatalf("got %s %+v, want exited", c.Kind, c.TurnEnded)
	}
	usage := 0
	for _, f := range o.rec.list {
		if f.Kind == task.FactUsage {
			usage++
		}
	}
	if usage != 1 {
		t.Fatalf("%d usage facts, want 1: the old reply was reused", usage)
	}
}

// C33：agent 在一輪中途退出（stdout 還被離開群組的子孫開著），取消寫不進去：
// Cancel 回 nil，由 exited 回報結束原因，不是 acp.request_failed。
func TestCancelWriteFailsAfterExit(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl not found")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	sh := filepath.Join(dir, "agent")
	body := "#!/bin/sh\n" +
		"read l; echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{}}}'\n" +
		"read l; echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"s1\"}}'\n" +
		"read l\n" + // 收到 Prompt，不回應就走
		"perl -e 'use POSIX; POSIX::setsid(); open(F, \">\", $ARGV[0]); print F $$; close F; sleep 60' \"" + pidFile + "\" &\n" +
		"while [ ! -s \"" + pidFile + "\" ]; do sleep 0.01; done\necho gone >&2\nexit 4\n"
	if err := os.WriteFile(sh, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	r := newRuntime(t, Options{})
	l := testLaunch()
	l.Executable = sh
	o := mustStart(t, r, l, "")
	o.prompt(t, task.Prompt{Text: "one"})
	<-o.conn.(*conn).proc.Done()
	if err := o.conn.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	c := o.rec.next(t, ends)
	if c.Kind != task.FactExited || c.Exited.Problem.Code != "proc.exited" || c.Exited.Problem.Params["status"] != "4" || c.Exited.StderrTail != "gone" {
		t.Fatalf("got %s %+v %+v", c.Kind, c.TurnEnded, c.Exited)
	}
}

// 各 agent 自己的擴充通知不交給 SDK：連發幾萬條，讀取照常，這一輪照常完成。
func TestVendorNotificationsDropped(t *testing.T) {
	r := newRuntime(t, Options{})
	lines := append(append([]string{}, started...), lPrompt(3, "go"))
	for i := range 20000 {
		lines = append(lines, fmt.Sprintf(`{"agent":{"jsonrpc":"2.0","method":"_vendor/progress","params":{"n":%d}}}`, i))
	}
	lines = append(lines, lChunk("done"), lResult(3, `{"stopReason":"end_turn"}`))
	o := mustStart(t, r, testLaunch(), "", fixture(t, lines...))
	o.prompt(t, task.Prompt{Text: "go"})
	if c := o.rec.next(t, ends); c.Kind != task.FactTurnEnded || c.TurnEnded.Result != task.TurnOK {
		t.Fatalf("got %+v", c)
	}
}

// C17a：標題裡 Markdown 行內代碼的反引號去掉，按 CommonMark 的 code span 配對。
func TestPlainTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Read b.go", "Read b.go"},
		{"Read `/Users/a/b.txt`", "Read /Users/a/b.txt"},
		{"`a` and `b`", "a and b"},
		{"``a`b``", "a`b"},
		{"`` `x` ``", "`x`"},
		{"it`s", "it`s"},
		{"`open ``", "`open ``"},
		{"run ` `", "run  "},
		{"Run \\`foo\\` now", "Run `foo` now"},
		{"`a\\`", "a\\"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := plainTitle(tc.in); got != tc.want {
			t.Errorf("plainTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// C17a：命令類的標題含著要執行的命令、命令自己用了反引號時，標題照原文。
func TestShownTitle(t *testing.T) {
	cases := []struct {
		title string
		kind  task.ToolKind
		input any
		want  string
	}{
		{"Execute `sleep 20`", task.ToolExecute, map[string]any{"command": "sleep 20"}, "Execute sleep 20"},
		{"ls `pwd`", task.ToolExecute, map[string]any{"command": "ls `pwd`"}, "ls `pwd`"},
		{"Execute `make`", task.ToolExecute, nil, "Execute make"},
		{"Read `a.go`", task.ToolRead, map[string]any{"command": "a.go"}, "Read a.go"},
	}
	for _, tc := range cases {
		st := &toolState{title: tc.title, kind: tc.kind, input: tc.input}
		if got := st.shownTitle(); got != tc.want {
			t.Errorf("shownTitle(%q, %s) = %q, want %q", tc.title, tc.kind, got, tc.want)
		}
	}
}
