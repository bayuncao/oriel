//go:build unix

package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// CL01、CL02：新會話以 --session-id=<UUID> 啟動、native_bound 報出它；恢復時以 --resume=<ID> 接回同一個
// ID。參數用等號的寫法。看參數的方法：一個記下參數再換成測試二進位的腳本。
func TestStartSessionArgs(t *testing.T) {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for _, tc := range []struct {
		name   string
		resume task.NativeSessionID
		want   func(args []string, id task.NativeSessionID) bool
	}{
		{name: "new session", want: func(args []string, id task.NativeSessionID) bool {
			return uuid.MatchString(string(id)) && slices.Contains(args, "--session-id="+string(id)) &&
				!slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--resume") })
		}},
		{name: "resume", resume: "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b", want: func(args []string, id task.NativeSessionID) bool {
			return id == "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b" && slices.Contains(args, "--resume="+string(id)) &&
				!slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--session-id") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "args")
			script := filepath.Join(dir, "claude")
			body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\nexec '" + os.Args[0] + "'\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			l := testLaunch()
			l.Executable, l.Args = script, []string{"-p", "--permission-mode", "default"}
			c, got, err := start(t, Options{}, l, tc.resume, fixture(t, started...))
			if err != nil {
				t.Fatal(err)
			}
			b := got.nextKind(t, task.FactNativeBound)
			if b.Bound.NativeID != c.NativeID() {
				t.Fatalf("native_bound %q, NativeID %q", b.Bound.NativeID, c.NativeID())
			}
			raw, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if !tc.want(args, c.NativeID()) {
				t.Fatalf("NativeID %q, args %q", c.NativeID(), args)
			}
		})
	}
}

// CL01：審批經 OrielHub 時，initialize 報的起始模式不是每次都詢問的那個就不啟動；舊版不報時無從核對。
// 原生寬鬆模式不核對。
func TestStartChecksPermissionMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		approval task.ApprovalMode
		code     string
	}{
		{name: "plan mode", response: `{"current_permission_mode":"plan"}`, approval: task.ApprovalOrielHub, code: string(CodePermissionMode)},
		{name: "asks", response: `{"current_permission_mode":"default"}`, approval: task.ApprovalOrielHub},
		{name: "asks under its newer name", response: `{"current_permission_mode":"manual"}`, approval: task.ApprovalOrielHub},
		{name: "not reported", response: `{}`, approval: task.ApprovalOrielHub},
		{name: "native", response: `{"current_permission_mode":"bypassPermissions"}`, approval: task.ApprovalNative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := testLaunch()
			l.Approval = tc.approval
			_, _, err := start(t, Options{}, l, "", fixture(t, lInit, lInitWith(tc.response)))
			if tc.code == "" {
				if err != nil {
					t.Fatalf("start: %v", err)
				}
				return
			}
			var e *Error
			if !errors.As(err, &e) || string(e.Code) != tc.code || e.Params["mode"] != "plan" {
				t.Fatalf("start: %v, want %s{mode: plan}", err, tc.code)
			}
		})
	}
}

// CL03、CL04：建好會話前結束是 hub.ExitError（帶 stderr 尾部）；initialize 沒回應是 claude.start_timed_out；
// 回錯誤是 claude.request_failed。
func TestStartFailures(t *testing.T) {
	t.Run("exits", func(t *testing.T) {
		_, _, err := start(t, Options{}, testLaunch(), "", fixture(t, lInit, `{"stderr":"not logged in"}`, `{"exit":2}`))
		var xe hub.ExitError
		if !errors.As(err, &xe) || xe.StderrTail() != "not logged in" {
			t.Fatalf("start: %v, want an ExitError with the stderr tail", err)
		}
		if code := xe.(interface{ ErrorCode() string }).ErrorCode(); code != "proc.exited" {
			t.Fatalf("code %s, want proc.exited", code)
		}
	})
	t.Run("times out", func(t *testing.T) {
		_, _, err := start(t, Options{StartTimeout: time.Second}, testLaunch(), "", fixture(t, lInit))
		if !errors.Is(err, fail(CodeStartTimedOut, nil)) {
			t.Fatalf("start: %v, want %s", err, CodeStartTimedOut)
		}
	})
	t.Run("refused", func(t *testing.T) {
		_, _, err := start(t, Options{}, testLaunch(), "", fixture(t, lInit,
			lAgent(`{"type":"control_response","response":{"subtype":"error","request_id":"oriel-1","error":"bad hooks"}}`)))
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeRequestFailed || e.Params["request"] != "initialize" || e.Params["detail"] != "bad hooks" {
			t.Fatalf("start: %v, want %s{initialize, bad hooks}", err, CodeRequestFailed)
		}
	})
}

// CL06：initialize 報的可選模型在 native_bound 之前報成 model_reported{start}，目前的值是 default；
// system init 報的實際模型對上另一個選項時再報一次（不是 start）。沒有可選的模型時不報。
func TestModelReported(t *testing.T) {
	c, got := mustStart(t, append(started, lUser,
		lAgent(`{"type":"system","subtype":"init","model":"claude-b"}`), lOK)...)
	first := got.next(t, func(task.Command) bool { return true })
	if first.Kind != task.FactModelReported || !first.ModelReported.Start {
		t.Fatalf("first fact %s, want model_reported{start}", first.Kind)
	}
	s := first.ModelReported.Setting
	if s.Option != "model" || s.Current != "default" || len(s.Choices) != 2 ||
		s.Choices[1] != (task.ModelChoice{Value: "fast", Name: "Fast", Description: "Quick"}) {
		t.Fatalf("setting %+v", s)
	}
	if got.next(t, func(task.Command) bool { return true }).Kind != task.FactNativeBound {
		t.Fatal("native_bound does not follow the model report")
	}
	prompt(t, c, "hi")
	m := got.nextKind(t, task.FactModelReported)
	if m.ModelReported.Start || m.ModelReported.Setting.Current != "fast" {
		t.Fatalf("after system init %+v, want current fast", m.ModelReported)
	}
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)

	_, got = mustStart(t, lInit, lInitWith(`{"current_permission_mode":"default"}`))
	if f := got.next(t, func(task.Command) bool { return true }); f.Kind != task.FactNativeBound {
		t.Fatalf("without models the first fact is %s", f.Kind)
	}
}

// CL07：set_model 在時限內沒回應是 claude.set_model_timed_out，連線照舊。
func TestSetModelTimesOut(t *testing.T) {
	c, got, err := start(t, Options{SetModelTimeout: time.Second}, testLaunch(), "", fixture(t, append(started,
		`{"client":{"type":"control_request","request_id":"oriel-2","request":{"subtype":"set_model","model":"fast"}}}`,
		lUser, lOK)...))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "fast"})
	if !errors.Is(err, fail(CodeSetModelTimedOut, nil)) {
		t.Fatalf("SetModel: %v, want %s", err, CodeSetModelTimedOut)
	}
	prompt(t, c, "still there?")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
}

// CL11、CL12：工作目錄裡的檔案以絕對路徑告訴 Claude；blob 只讓這一輪失敗，下一輪照常。
func TestAttachments(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := newRuntime(t, Options{})
	got := newRec()
	c, err := r.Start(context.Background(), hub.StartOptions{Launch: testLaunch(), Workdir: dir, Emit: got.emit,
		Env: replayEnv(t, fixture(t, append(started,
			`{"client":{"type":"user","message":{"content":[{"type":"text","text":"look"},{"type":"text","text":"Attached file: <cwd>/notes.md"}]}}}`,
			lOK, lUserText("again"), lOK)...))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Prompt(context.Background(), task.Prompt{ID: "a", Text: "look",
		Attachments: []task.Attachment{{Name: "notes.md", Path: "notes.md"}}}); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	if err := c.Prompt(context.Background(), task.Prompt{ID: "b", Text: "see",
		Attachments: []task.Attachment{{Name: "shot.png", MIME: "image/png", Blob: "sha256-x"}}}); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnError, string(CodeAttachmentUnsupported), map[string]string{"name": "shot.png"})
	prompt(t, c, "again")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
}

// CL13：文字與推理的增量各是一個 output；串流送過的訊息，整則訊息裡的文字不再報；沒串流的訊息照報。
func TestOutput(t *testing.T) {
	c, got := mustStart(t, append(started, lUser,
		lStart("m1"), lDelta("text_delta", "text", "Hel"), lDelta("text_delta", "text", "lo"),
		lDelta("thinking_delta", "thinking", "hmm"),
		lAssistant("m1", `[{"type":"thinking","thinking":"hmm","signature":"s"},{"type":"text","text":"Hello"}]`),
		lAssistant("m2", `[{"type":"text","text":"unstreamed"}]`), lOK)...)
	prompt(t, c, "hi")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	var outs []task.Output
	for _, f := range got.all() {
		if f.Kind == task.FactOutput {
			outs = append(outs, *f.Output)
		}
	}
	want := []task.Output{{Channel: task.ChannelText, Text: "Hel"}, {Channel: task.ChannelText, Text: "lo"},
		{Channel: task.ChannelThinking, Text: "hmm"}, {Channel: task.ChannelText, Text: "unstreamed"}}
	if !slices.Equal(outs, want) {
		t.Fatalf("outputs %+v, want %+v", outs, want)
	}
}

// CL14–CL16、CL20、CL34：工具呼叫報 tool_started 與 tool_updated（沒經審批就執行的也照實報，沒有 asked）；
// TodoWrite 報 plan；子 agent 的文字不進主對話，它的工具呼叫照報。
func TestTools(t *testing.T) {
	c, got := mustStart(t, append(started, lUser,
		lAssistant("m1", `[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"/x/a.go"}}]`),
		lAgent(`{"type":"user","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"r1","is_error":true,"content":[{"type":"text","text":"no such file"}]}]}}`),
		lAssistant("m2", `[{"type":"tool_use","id":"td","name":"TodoWrite","input":{"todos":[{"content":"Read a.go","status":"completed","activeForm":"Reading"},{"content":"Fix it","status":"in_progress","activeForm":"Fixing"}]}}]`),
		lAgent(`{"type":"stream_event","parent_tool_use_id":"task_1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"sub agent thinking aloud"}}}`),
		lAgent(`{"type":"assistant","parent_tool_use_id":"task_1","message":{"id":"m3","content":[{"type":"text","text":"sub agent text"},{"type":"tool_use","id":"g1","name":"Grep","input":{"pattern":"TODO","path":"/x"}}]}}`),
		lOK)...)
	prompt(t, c, "fix a.go")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	var started, updated []string
	var plan *task.PlanUpdate
	for _, f := range got.all() {
		switch f.Kind {
		case task.FactToolStarted:
			s := f.ToolStarted
			started = append(started, string(s.ID)+" "+s.Tool+" "+string(s.Kind)+" "+s.Title+" "+strings.Join(s.Paths, ","))
		case task.FactToolUpdated:
			u := f.ToolUpdated
			updated = append(updated, string(u.ID)+" "+string(u.Status)+" "+u.Output)
		case task.FactPlan:
			plan = f.Plan
		case task.FactOutput:
			t.Errorf("output %q: no text was in the main conversation", f.Output.Text)
		case task.FactAsked:
			t.Errorf("asked %+v without a can_use_tool (CL34)", f.Asked)
		}
	}
	if want := []string{"r1 Read read /x/a.go /x/a.go", "g1 Grep search TODO /x"}; !slices.Equal(started, want) {
		t.Errorf("tool_started %q, want %q", started, want)
	}
	if want := []string{"r1 failed no such file"}; !slices.Equal(updated, want) {
		t.Errorf("tool_updated %q, want %q", updated, want)
	}
	wantPlan := []task.PlanEntry{{Text: "Read a.go", Status: task.PlanCompleted}, {Text: "Fix it", Status: task.PlanInProgress}}
	if plan == nil || !slices.Equal(plan.Entries, wantPlan) {
		t.Errorf("plan %+v, want %+v", plan, wantPlan)
	}
}

// CL17：工具名稱對應的分類。
func TestKindOf(t *testing.T) {
	for name, want := range map[string]task.ToolKind{
		"Bash": task.ToolExecute, "Read": task.ToolRead, "Edit": task.ToolEdit, "Write": task.ToolEdit,
		"MultiEdit": task.ToolEdit, "NotebookEdit": task.ToolEdit, "Glob": task.ToolSearch, "Grep": task.ToolSearch,
		"WebFetch": task.ToolFetch, "WebSearch": task.ToolFetch, "Task": task.ToolOther, "mcp__github__create_issue": task.ToolOther,
	} {
		if got := kindOf(name); got != want {
			t.Errorf("kindOf(%s) = %s, want %s", name, got, want)
		}
	}
}

// CL18、CL19：成功時先報用量再報完成；各種失敗的原因；失敗之後行程照常，下一輪照樣送。
func TestResults(t *testing.T) {
	c, got := mustStart(t, append(started,
		lUser, lResult(`"subtype":"success","is_error":false,"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":7}`),
		lUser, lResult(`"subtype":"error_max_turns","is_error":true`),
		lUser, lResult(`"subtype":"error_during_execution","is_error":true,"errors":["boom"]`),
		lUser, lResult(`"subtype":"success","is_error":true,"result":"Not logged in · Please run /login"`),
	)...)
	prompt(t, c, "1")
	u := got.next(t, func(f task.Command) bool { return f.Kind == task.FactUsage || f.Kind == task.FactTurnEnded })
	if u.Kind != task.FactUsage || *u.Usage != (task.Usage{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 5, CacheWriteTokens: 7}) {
		t.Fatalf("got %s %+v, want the usage first", u.Kind, u.Usage)
	}
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	prompt(t, c, "2")
	wantTurn(t, got.turnEnd(t), task.TurnError, string(CodeMaxTurns), nil)
	prompt(t, c, "3")
	wantTurn(t, got.turnEnd(t), task.TurnError, string(CodeExecutionError), map[string]string{"detail": "boom"})
	prompt(t, c, "4")
	wantTurn(t, got.turnEnd(t), task.TurnError, string(CodeTurnFailed), map[string]string{"detail": "Not logged in · Please run /login"})
}

// CL30–CL33：can_use_tool 報成 asked；允許時原樣帶回 input；沒見過的工具呼叫先補 tool_started；Claude 撤回的
// 請求之後再回答不送任何東西；hook 等其他請求回錯誤。
func TestAsk(t *testing.T) {
	c, got := mustStart(t, append(started, lUser,
		lAssistant("m1", `[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"rm -rf build","description":"Clean"}}]`),
		lAgent(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf build","description":"Clean"},"tool_use_id":"b1","title":"Delete build","blocked_path":"/outside"}}`),
		`{"client":{"type":"control_response","response":{"subtype":"success","request_id":"req-1","response":{"behavior":"allow","updatedInput":{"command":"rm -rf build","description":"Clean"}}}}}`,
		lAgent(`{"type":"control_request","request_id":"req-3","request":{"subtype":"hook_callback","callback_id":"hook_0"}}`),
		`{"client":{"type":"control_response","response":{"subtype":"error","request_id":"req-3"}}}`,
		lAgent(`{"type":"control_request","request_id":"req-2","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/x/b.txt","content":"hi"},"tool_use_id":"w1"}}`),
		lAgent(`{"type":"control_cancel_request","request_id":"req-2"}`),
		lDelta("text_delta", "text", "withdrawn"),
		// 回答撤回的 req-2 若送了東西，這裡等到的就不是 interrupt。
		`{"client":{"type":"control_request","request_id":"oriel-2","request":{"subtype":"interrupt"}}}`,
		lAgent(`{"type":"control_response","response":{"subtype":"success","request_id":"oriel-2","response":{}}}`),
		lResult(`"subtype":"error_during_execution","is_error":true`), lUserText("next"), lOK)...)
	prompt(t, c, "clean up")
	a := got.nextKind(t, task.FactAsked)
	want := task.PermissionRequest{Tool: "Bash", Kind: task.ToolExecute, Title: "Delete build", Command: "rm -rf build",
		Paths: []string{"/outside"}, Input: `{"command":"rm -rf build","description":"Clean"}`}
	if r := a.Asked.Request; r.Tool != want.Tool || r.Kind != want.Kind || r.Title != want.Title || r.Command != want.Command ||
		!slices.Equal(r.Paths, want.Paths) || r.Input != want.Input {
		t.Fatalf("asked %+v, want %+v", r, want)
	}
	if err := c.Answer(context.Background(), a.Asked.ID, task.Allow); err != nil {
		t.Fatal(err)
	}
	s := got.next(t, func(f task.Command) bool {
		return f.Kind == task.FactToolStarted && f.ToolStarted.ID == "w1" || f.Kind == task.FactAsked
	})
	if s.Kind != task.FactToolStarted || s.ToolStarted.Kind != task.ToolEdit {
		t.Fatalf("got %s before the second ask, want tool_started for w1", s.Kind)
	}
	a2 := got.nextKind(t, task.FactAsked)
	if a2.Asked.ID == a.Asked.ID || a2.Asked.Request.Title != "/x/b.txt" {
		t.Fatalf("second asked %+v", a2.Asked)
	}
	// 撤回之後的文字到了，撤回就已處理過（同一個讀取迴圈依序處理）。
	got.next(t, func(f task.Command) bool { return f.Kind == task.FactOutput && f.Output.Text == "withdrawn" })
	if err := c.Answer(context.Background(), a2.Asked.ID, task.Deny); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnCancelled, "", nil)
	prompt(t, c, "next")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
}

// CL40–CL42：中斷先拒絕掛著的審批再送 interrupt，之後的 result 不論 subtype 都是取消；同一輪再中斷、
// 沒有一輪時中斷都不送任何東西。
func TestCancel(t *testing.T) {
	c, got := mustStart(t, append(started, lUser,
		lAssistant("m1", `[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"make"}}]`),
		lAgent(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"make"},"tool_use_id":"b1"}}`),
		`{"client":{"type":"control_response","response":{"subtype":"success","request_id":"req-1","response":{"behavior":"deny","message":"Denied in OrielHub"}}}}`,
		`{"client":{"type":"control_request","request_id":"oriel-2","request":{"subtype":"interrupt"}}}`,
		lAgent(`{"type":"control_response","response":{"subtype":"success","request_id":"oriel-2","response":{}}}`),
		lResult(`"subtype":"error_during_execution","is_error":true,"errors":["Request was aborted."]`),
		lUserText("next"), lOK)...)
	prompt(t, c, "build")
	got.nextKind(t, task.FactAsked)
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(context.Background()); err != nil { // 這一輪已要求中斷
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnCancelled, "", nil)
	if err := c.Cancel(context.Background()); err != nil { // 沒有一輪在跑
		t.Fatal(err)
	}
	prompt(t, c, "next")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
}

// CL05、CL23：不是 JSON 的行跳過；超過 10 MiB 的一行結束行程，原因是 claude.protocol_error。
func TestProtocolLines(t *testing.T) {
	c, got := mustStart(t, append(started, `{"raw":"not json at all"}`, lUser, lOK)...)
	prompt(t, c, "hi")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)

	c, got = mustStart(t, append(started, lUser, `{"raw":"`+strings.Repeat("x", maxLine+1)+`"}`)...)
	prompt(t, c, "hi")
	e := got.turnEnd(t)
	if e.Kind != task.FactExited || e.Exited.Problem.Code != string(CodeProtocolError) {
		t.Fatalf("got %s %+v, want exited %s", e.Kind, e.Exited, CodeProtocolError)
	}
}

// CL21：一輪之外的內容不報（如接回會話時重放的歷史）：native_bound 仍是第一條事實，之後也沒有 output。
func TestNothingOutsideTurn(t *testing.T) {
	c, got := mustStart(t, append(started,
		lAssistant("old", `[{"type":"text","text":"history"},{"type":"tool_use","id":"h1","name":"Read","input":{"file_path":"/old"}}]`),
		lUser, lOK)...)
	if f := got.next(t, func(f task.Command) bool { return f.Kind != task.FactModelReported }); f.Kind != task.FactNativeBound {
		t.Fatalf("first fact after the model is %s, want native_bound", f.Kind)
	}
	prompt(t, c, "hi")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	for _, f := range got.all() {
		if f.Kind == task.FactOutput || f.Kind == task.FactToolStarted {
			t.Fatalf("%s from outside a turn: %+v", f.Kind, f)
		}
	}
}

// CL36：不在一輪裡的 can_use_tool 回 deny，不讓 Claude 一直等。夾具收到 deny 才以 0 結束；沒回的話它一直等，
// 這裡等不到 exited。
func TestAskOutsideTurn(t *testing.T) {
	_, got := mustStart(t, append(started,
		// 回覆依 Claude 發出請求的順序寫出（先 hook 的錯誤，再 deny）。
		lAgent(`{"type":"control_request","request_id":"req-h","request":{"subtype":"hook_callback","callback_id":"hook_0"}}`),
		lAgent(`{"type":"control_request","request_id":"req-0","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"b0"}}`),
		`{"client":{"type":"control_response","response":{"subtype":"error","request_id":"req-h"}}}`,
		`{"client":{"type":"control_response","response":{"subtype":"success","request_id":"req-0","response":{"behavior":"deny"}}}}`,
		`{"exit":0}`)...)
	e := got.nextKind(t, task.FactExited)
	if e.Exited.Problem.Code != "proc.exited" || e.Exited.Problem.Params["status"] != "0" {
		t.Fatalf("exited %+v, want status 0 after the deny", e.Exited)
	}
	for _, f := range got.all() {
		if f.Kind == task.FactAsked {
			t.Fatalf("asked outside a turn: %+v", f.Asked)
		}
	}
}

// CL40：已要求中斷之後才到的 can_use_tool 直接回 deny，不再問使用者。
func TestAskAfterCancel(t *testing.T) {
	c, got := mustStart(t, append(started, lUser,
		`{"client":{"type":"control_request","request_id":"oriel-2","request":{"subtype":"interrupt"}}}`,
		lAgent(`{"type":"control_request","request_id":"req-5","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /"},"tool_use_id":"b5"}}`),
		`{"client":{"type":"control_response","response":{"subtype":"success","request_id":"req-5","response":{"behavior":"deny"}}}}`,
		lAgent(`{"type":"control_response","response":{"subtype":"success","request_id":"oriel-2","response":{}}}`),
		lResult(`"subtype":"error_during_execution","is_error":true`))...)
	prompt(t, c, "go")
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnCancelled, "", nil)
	for _, f := range got.all() {
		if f.Kind == task.FactAsked {
			t.Fatalf("asked after Cancel: %+v", f.Asked)
		}
	}
}

// CL40：中斷沒送到而行程還在時回錯誤，下一次 Cancel 照樣再送（不會以為已經要求過）。夾具關掉 stdin 後
// 停著不結束：寫入失敗，行程仍在。
func TestCancelRetriesAfterFailedWrite(t *testing.T) {
	c, got, err := start(t, Options{ExitGrace: time.Second}, testLaunch(), "", fixture(t, append(started, lUser,
		`{"closeStdin":true}`, lDelta("text_delta", "text", "stdin closed"), `{"hold":true}`)...))
	if err != nil {
		t.Fatal(err)
	}
	prompt(t, c, "go")
	got.next(t, func(f task.Command) bool { return f.Kind == task.FactOutput && f.Output.Text == "stdin closed" })
	for i := range 2 {
		var e *Error
		if err := c.Cancel(context.Background()); !errors.As(err, &e) || e.Code != CodeRequestFailed {
			t.Fatalf("Cancel #%d: %v, want %s (the interrupt was tried again)", i+1, err, CodeRequestFailed)
		}
	}
}

// 一輪結束後工具的記錄清掉：下一輪才到的舊工具結果不報（也不會越積越多）。
func TestToolsForgottenAfterTurn(t *testing.T) {
	c, got := mustStart(t, append(started, lUser,
		lAssistant("m1", `[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"/x/a.go"}}]`), lOK,
		lUserText("again"),
		lAgent(`{"type":"user","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"r1","content":"late"}]}}`),
		lOK)...)
	prompt(t, c, "read")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	prompt(t, c, "again")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	for _, f := range got.all() {
		if f.Kind == task.FactToolUpdated {
			t.Fatalf("tool_updated for a tool of the previous turn: %+v", f.ToolUpdated)
		}
	}
}

// CL43：Close 結束行程並等讀取結束，可以重複呼叫。
func TestCloseTwice(t *testing.T) {
	c, got := mustStart(t, started...)
	got.nextKind(t, task.FactNativeBound)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	got.nextKind(t, task.FactExited)
}

// lRateLimit 是一則 rate_limit_event，rate_limit_info 由呼叫者給（欄位取自 Claude Code 2.1.311 的 schema）。
func lRateLimit(info string) string {
	return lAgent(`{"type":"rate_limit_event","rate_limit_info":` + info + `,"uuid":"u-rl","session_id":"<session>"}`)
}

// CQ01–CQ07：rate_limit_event 的 unifiedWindows 記成 Claude 的額度快照（5 小時與 7 天），時間是收到的時間；
// 沒有可記的窗口時不記，讀不懂的窗口跳過，讀不懂的事件丟掉；都不產生事實，這一輪照常。
func TestRateLimit(t *testing.T) {
	const t1, t2 = 1795000000, 1795500000
	var mu sync.Mutex
	var quotas []usage.Quota
	o := Options{Quota: func(q usage.Quota) {
		mu.Lock()
		defer mu.Unlock()
		quotas = append(quotas, q)
	}}
	before := time.Now().UTC()
	c, got, err := start(t, o, testLaunch(), "", fixture(t, append(started, lUser,
		// CQ01、CQ04：兩個窗口；超額的窗口與頂層欄位不讀。
		lRateLimit(`{"status":"allowed_warning","rateLimitType":"seven_day_overage_included","utilization":0.9,"resetsAt":1,`+
			`"unifiedWindows":{"five_hour":{"utilization":0.25,"resetsAt":1795000000,"status":"allowed"},`+
			`"seven_day":{"utilization":0.5,"resetsAt":1795500000},`+
			`"seven_day_overage_included":{"utilization":0.9,"resetsAt":1}},"overageStatus":"rejected","isUsingOverage":false}`),
		// CQ02、CQ05：只有 5 小時，超過上限。
		lRateLimit(`{"status":"rejected","unifiedWindows":{"five_hour":{"utilization":1.25,"resetsAt":1795000000}}}`),
		// CQ03：API key 的會話沒有 unifiedWindows；只有超額窗口也不記。
		lRateLimit(`{"status":"allowed"}`),
		lRateLimit(`{"status":"allowed","unifiedWindows":{"seven_day_overage_included":{"utilization":0.1,"resetsAt":1}}}`),
		// CQ06：讀不懂的窗口跳過，另一個照記；兩個都讀不懂時不記。
		lRateLimit(`{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":"high","resetsAt":1795000000},`+
			`"seven_day":{"utilization":0.5,"resetsAt":1795500000}}}`),
		lRateLimit(`{"status":"allowed","unifiedWindows":{"five_hour":{"resetsAt":1795000000},"seven_day":{"utilization":0.5}}}`),
		// CQ07：rate_limit_info 不是物件。
		lRateLimit(`"soon"`),
		lOK)...))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	prompt(t, c, "hi")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	after := time.Now().UTC()

	five := func(used float64) usage.Limit {
		return usage.Limit{Minutes: 300, UsedPercent: used, ResetsAt: time.Unix(t1, 0).UTC()}
	}
	seven := usage.Limit{Minutes: 10080, UsedPercent: 50, ResetsAt: time.Unix(t2, 0).UTC()}
	want := [][]usage.Limit{{five(25), seven}, {five(125)}, {seven}}
	mu.Lock()
	defer mu.Unlock()
	if len(quotas) != len(want) {
		t.Fatalf("quotas %+v, want %d", quotas, len(want))
	}
	for i, q := range quotas {
		if q.Agent != "claude" || q.Bucket != "claude" || q.At.Before(before) || q.At.After(after) || !slices.Equal(q.Windows, want[i]) {
			t.Fatalf("quota %d: %+v, want windows %+v at [%v, %v]", i, q, want[i], before, after)
		}
	}
	for _, f := range got.all() {
		if f.Kind != task.FactNativeBound && f.Kind != task.FactModelReported && f.Kind != task.FactUsage &&
			f.Kind != task.FactTurnEnded {
			t.Fatalf("unexpected fact %s: %+v", f.Kind, f)
		}
	}
}

// CQ14：呼叫過模型而成功結束的一輪之後，最近的額度以當時的時間再報一次；沒有用量的一輪、失敗的一輪不報。
func TestQuotaConfirmedAtTurnEnd(t *testing.T) {
	var mu sync.Mutex
	var quotas []usage.Quota
	o := Options{Quota: func(q usage.Quota) {
		mu.Lock()
		defer mu.Unlock()
		quotas = append(quotas, q)
	}}
	used := `"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`
	c, got, err := start(t, o, testLaunch(), "", fixture(t, append(started,
		lUser, lOK, // 還沒收到過額度：沒有可再報的
		lUser, lRateLimit(`{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.25,"resetsAt":1795000000}}}`),
		lResult(`"subtype":"success","is_error":false,`+used),
		lUser, lOK, // 沒有用量
		lUser, lResult(`"subtype":"error_during_execution","is_error":true,`+used),
		lUser, lResult(`"subtype":"success","is_error":false,`+used))...))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for range 5 {
		prompt(t, c, "hi")
		got.turnEnd(t)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(quotas) != 3 {
		t.Fatalf("quotas %+v, want the event and two confirmations", quotas)
	}
	for i := 1; i < len(quotas); i++ {
		if !slices.Equal(quotas[i].Windows, quotas[0].Windows) || quotas[i].At.Before(quotas[i-1].At) {
			t.Fatalf("confirmation %d: %+v after %+v", i, quotas[i], quotas[i-1])
		}
	}
}
