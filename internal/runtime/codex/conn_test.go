//go:build unix

package codex

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
)

// CX01、CX03：審批經 OrielHub 時，會話不是以 untrusted 開、或審批不交給 user 就不啟動；原生寬鬆模式不核對。
func TestStartChecksApprovalPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   string
		approval task.ApprovalMode
		policy   string // 空：啟動成功
	}{
		{name: "asks", result: `{"thread":{"id":"thr_1"},"approvalPolicy":"untrusted","approvalsReviewer":"user"}`, approval: task.ApprovalOrielHub},
		{name: "model decides", result: `{"thread":{"id":"thr_1"},"approvalPolicy":"on-request","approvalsReviewer":"user"}`, approval: task.ApprovalOrielHub, policy: "on-request"},
		{name: "granular", result: `{"thread":{"id":"thr_1"},"approvalPolicy":{"granular":{"rules":true,"sandbox_approval":true,"mcp_elicitations":true}}}`, approval: task.ApprovalOrielHub, policy: "granular"},
		{name: "auto review", result: `{"thread":{"id":"thr_1"},"approvalPolicy":"untrusted","approvalsReviewer":"auto_review"}`, approval: task.ApprovalOrielHub, policy: "untrusted (approvals reviewed by auto_review)"},
		{name: "native", result: `{"thread":{"id":"thr_1"},"approvalPolicy":"never"}`, approval: task.ApprovalNative},
		{name: "policy not reported", result: `{"thread":{"id":"thr_1"}}`, approval: task.ApprovalOrielHub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := testLaunch()
			l.Approval = tc.approval
			lines := startedWith(tc.result)
			if tc.approval == task.ApprovalNative {
				lines[5] = `{"client":{"id":3,"method":"thread/start","params":{"cwd":"<cwd>"}}}`
			}
			_, _, err := start(t, Options{}, l, "", fixture(t, lines...))
			if tc.policy == "" {
				if err != nil {
					t.Fatalf("start: %v", err)
				}
				return
			}
			var e *Error
			if !errors.As(err, &e) || e.Code != CodeApprovalPolicy || e.Params["policy"] != tc.policy {
				t.Fatalf("start: %v, want %s{policy: %s}", err, CodeApprovalPolicy, tc.policy)
			}
		})
	}
}

// CX02：接回以 thread/resume{threadId, excludeTurns}，native_bound 是同一個 ID。
func TestResume(t *testing.T) {
	c, got, err := start(t, Options{}, testLaunch(), "thr_9", fixture(t, lInit, lInitOK, lInited, lModels, lModelsOK,
		`{"client":{"id":3,"method":"thread/resume","params":{"threadId":"thr_9","excludeTurns":true,"approvalPolicy":"untrusted","approvalsReviewer":"user"}}}`,
		`{"agent":{"id":3,"result":{"thread":{"id":"thr_9"},"model":"gpt-a","approvalPolicy":"untrusted","approvalsReviewer":"user"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if b := got.nextKind(t, task.FactNativeBound); b.Bound.NativeID != "thr_9" || c.NativeID() != "thr_9" {
		t.Fatalf("native_bound %q, NativeID %q, want thr_9", b.Bound.NativeID, c.NativeID())
	}
}

// CX04、CX05：建好會話前結束是 hub.ExitError；沒按時建好是 codex.start_timed_out；請求回錯誤是 codex.request_failed。
func TestStartFailures(t *testing.T) {
	t.Run("exits", func(t *testing.T) {
		_, _, err := start(t, Options{}, testLaunch(), "", fixture(t, lInit, `{"stderr":"no such config"}`, `{"exit":2}`))
		var xe hub.ExitError
		if !errors.As(err, &xe) || xe.StderrTail() != "no such config" {
			t.Fatalf("start: %v, want an ExitError with the stderr tail", err)
		}
	})
	t.Run("times out", func(t *testing.T) {
		_, _, err := start(t, Options{StartTimeout: time.Second}, testLaunch(), "", fixture(t, lInit))
		if !errors.Is(err, fail(CodeStartTimedOut, nil)) {
			t.Fatalf("start: %v, want %s", err, CodeStartTimedOut)
		}
	})
	t.Run("refused", func(t *testing.T) {
		_, _, err := start(t, Options{}, testLaunch(), "", fixture(t, lInit, lInitOK, lInited, lModels, lModelsOK, lStart,
			`{"agent":{"id":3,"error":{"code":-32600,"message":"bad cwd"}}}`))
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeRequestFailed || e.Params["method"] != "thread/start" || e.Params["detail"] != "bad cwd" {
			t.Fatalf("start: %v, want %s{thread/start, bad cwd}", err, CodeRequestFailed)
		}
	})
}

// CX06、CX07：可選的模型（不含 hidden）在 native_bound 之前報，目前的值取會話回應的 model；SetModel 不發
// 請求，下一輪的 turn/start 帶上它。
func TestModels(t *testing.T) {
	c, got := mustStart(t, append(started,
		`{"client":{"id":4,"method":"turn/start","params":{"threadId":"thr_1","model":"gpt-b"}}}`,
		`{"agent":{"id":4,"result":{"turn":{"id":"turn_1"}}}}`, lOK(1))...)
	first := got.next(t, func(task.Command) bool { return true })
	s := first.ModelReported
	if first.Kind != task.FactModelReported || !s.Start || s.Setting.Current != "gpt-a" || len(s.Setting.Choices) != 2 ||
		s.Setting.Choices[1] != (task.ModelChoice{Value: "gpt-b", Name: "GPT B", Description: "Quick"}) {
		t.Fatalf("first fact %s %+v", first.Kind, s)
	}
	set, err := c.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "gpt-b"})
	if err != nil || set.Current != "gpt-b" || len(set.Choices) != 2 {
		t.Fatalf("SetModel: %+v, %v", set, err)
	}
	prompt(t, c, "hi")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
}

// CX11、CX12：工作目錄裡的圖片是 localImage，其他檔案寫成一段路徑文字；blob 只讓這一輪失敗。交接內容接在文字之後（HO18）。
func TestAttachments(t *testing.T) {
	c, got := mustStart(t, append(started,
		`{"client":{"id":4,"method":"turn/start","params":{"threadId":"thr_1","input":[{"type":"text","text":"look\n\nhandoff"},`+
			`{"type":"localImage","path":"<cwd>/shot.png"},{"type":"text","text":"Attached file: <cwd>/notes.md"}]}}}`,
		`{"agent":{"id":4,"result":{"turn":{"id":"turn_1"}}}}`, lOK(1))...)
	if err := c.Prompt(context.Background(), task.Prompt{ID: "a", Text: "look", Context: "handoff", Attachments: []task.Attachment{
		{Name: "shot.png", MIME: "image/png", Path: "shot.png"}, {Name: "notes.md", MIME: "text/markdown", Path: "notes.md"}}}); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	if err := c.Prompt(context.Background(), task.Prompt{ID: "b", Text: "see",
		Attachments: []task.Attachment{{Name: "blob.png", MIME: "image/png", Blob: "sha256-x"}}}); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnError, string(CodeAttachmentUnsupported), map[string]string{"name": "blob.png"})
}

// CX13–CX17、CX20：文字與推理的增量；沒以增量送過的條目在完成時補報；命令依 Codex 的解析分類，被拒記為
// failed；修改檔案、MCP 與網頁搜尋；計畫；一輪之外的不報。
func TestItems(t *testing.T) {
	c, got := mustStart(t, append(append(started,
		lItem(false, `{"type":"agentMessage","id":"early","text":""}`)), // 一輪之外
		append(lTurn(4, 1),
			lNote("item/agentMessage/delta", `{"turnId":"turn_1","itemId":"m1","delta":"Hel"}`),
			lNote("item/agentMessage/delta", `{"turnId":"turn_1","itemId":"m1","delta":"lo"}`),
			lItem(true, `{"type":"agentMessage","id":"m1","text":"Hello"}`),
			lNote("item/reasoning/summaryTextDelta", `{"turnId":"turn_1","itemId":"r1","delta":"thinking"}`),
			lItem(true, `{"type":"agentMessage","id":"m2","text":"unstreamed"}`),
			lItem(false, `{"type":"commandExecution","id":"c1","command":"cat a.go","commandActions":[{"type":"read","command":"cat a.go","name":"a.go","path":"/x/a.go"}],"status":"inProgress"}`),
			lItem(true, `{"type":"commandExecution","id":"c1","command":"cat a.go","commandActions":[{"type":"read","command":"cat a.go","name":"a.go","path":"/x/a.go"}],"status":"completed","aggregatedOutput":"package a"}`),
			lItem(false, `{"type":"commandExecution","id":"c2","command":"rg TODO","commandActions":[{"type":"search","command":"rg TODO","query":"TODO"}],"status":"inProgress"}`),
			lItem(true, `{"type":"commandExecution","id":"c3","command":"make","commandActions":[{"type":"unknown","command":"make"}],"status":"declined"}`),
			lItem(false, `{"type":"fileChange","id":"f1","changes":[{"path":"/x/a.go","kind":{"type":"update"},"diff":"@@"},{"path":"/x/b.go","kind":{"type":"update"},"diff":"@@"}],"status":"inProgress"}`),
			lItem(true, `{"type":"fileChange","id":"f2","changes":[{"path":"/x/old.go","kind":{"type":"delete"},"diff":""}],"status":"completed"}`),
			lItem(true, `{"type":"mcpToolCall","id":"p1","server":"github","tool":"create_issue","arguments":{"title":"x"},"status":"failed","error":{"message":"no token"}}`),
			lItem(true, `{"type":"webSearch","id":"w1","query":"go generics"}`),
			lNote("turn/plan/updated", `{"turnId":"turn_1","plan":[{"step":"Read","status":"completed"},{"step":"Fix","status":"inProgress"}]}`),
			lOK(1))...)...)
	prompt(t, c, "go")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	var outs, started, updated []string
	var plan *task.PlanUpdate
	for _, f := range got.all() {
		switch f.Kind {
		case task.FactOutput:
			outs = append(outs, string(f.Output.Channel)+":"+f.Output.Text)
		case task.FactToolStarted:
			s := f.ToolStarted
			started = append(started, string(s.ID)+" "+s.Tool+" "+string(s.Kind)+" "+s.Title+" "+strings.Join(s.Paths, ","))
		case task.FactToolUpdated:
			u := f.ToolUpdated
			updated = append(updated, string(u.ID)+" "+string(u.Status)+" "+u.Output)
		case task.FactPlan:
			plan = f.Plan
		}
	}
	check := func(what string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", what, got, want)
		}
	}
	check("outputs", outs, []string{"text:Hel", "text:lo", "thinking:thinking", "text:unstreamed"})
	check("tool_started", started, []string{
		"c1 shell read cat a.go /x/a.go", "c2 shell search rg TODO ", "c3 shell execute make ",
		"f1 apply_patch edit Edit /x/a.go and 1 more /x/a.go,/x/b.go", "f2 apply_patch delete Edit /x/old.go /x/old.go",
		"p1 github.create_issue other github.create_issue ", "w1 web_search fetch go generics "})
	check("tool_updated", updated, []string{"c1 completed package a", "c3 failed ", "f2 completed ", "p1 failed no token", "w1 completed "})
	want := []task.PlanEntry{{Text: "Read", Status: task.PlanCompleted}, {Text: "Fix", Status: task.PlanInProgress}}
	if plan == nil || !slices.Equal(plan.Entries, want) {
		t.Errorf("plan %+v, want %+v", plan, want)
	}
}

// CX18：用量是這一輪開始到結束之間累計的增量（輸入扣掉快取讀），先於結局報。
func TestUsageIsTheTurnsShare(t *testing.T) {
	c, got := mustStart(t, append(append(started, lUsage(1000, 400, 100, 1100)),
		append(lTurn(4, 1), lUsage(1300, 500, 150, 1450), lOK(1))...)...)
	prompt(t, c, "go")
	u := got.next(t, func(f task.Command) bool { return f.Kind == task.FactUsage || f.Kind == task.FactTurnEnded })
	if u.Kind != task.FactUsage || *u.Usage != (task.Usage{InputTokens: 200, CacheReadTokens: 100, OutputTokens: 50}) {
		t.Fatalf("got %s %+v, want usage {in 200, cache read 100, out 50}", u.Kind, u.Usage)
	}
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
}

// CX19：失敗的原因依 codexErrorInfo；失敗之後行程照常，下一輪照樣送。
func TestTurnFailures(t *testing.T) {
	lines := append([]string(nil), started...)
	cases := []struct {
		turn   string
		code   Code
		params map[string]string
	}{
		{`"status":"failed","error":{"message":"401","codexErrorInfo":"unauthorized"}`, CodeAuthRequired, nil},
		{`"status":"failed","error":{"message":"limit","codexErrorInfo":"usageLimitExceeded"}`, CodeUsageLimit, nil},
		{`"status":"failed","error":{"message":"too long","codexErrorInfo":"contextWindowExceeded"}`, CodeContextWindow, nil},
		{`"status":"failed","error":{"message":"upstream 502","codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":502}}}`, CodeTurnFailed, map[string]string{"detail": "upstream 502"}},
		// 上游 API 的錯誤回應原樣轉來：取 error.message，沒有就取頂層的 message；都沒有時原樣。
		{`"status":"failed","error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'x' model is not supported.\",\"code\":\"model_not_found\"}}"}`, CodeTurnFailed, map[string]string{"detail": "The 'x' model is not supported."}},
		{`"status":"failed","error":{"message":"{\"message\":\"rate limited\"}"}`, CodeTurnFailed, map[string]string{"detail": "rate limited"}},
		{`"status":"failed","error":{"message":"{\"error\":\"unauthorized\",\"message\":\"Invalid API key\"}"}`, CodeTurnFailed, map[string]string{"detail": "Invalid API key"}},
		{`"status":"failed","error":{"message":"{\"error\":{\"code\":500}}"}`, CodeTurnFailed, map[string]string{"detail": `{"error":{"code":500}}`}},
		{`"status":"failed","error":{"message":"{not json"}`, CodeTurnFailed, map[string]string{"detail": "{not json"}},
	}
	for i, tc := range cases {
		lines = append(lines, append(lTurn(4+i, i+1), lDone(i+1, tc.turn))...)
	}
	c, got := mustStart(t, lines...)
	for _, tc := range cases {
		prompt(t, c, "go")
		wantTurn(t, got.turnEnd(t), task.TurnError, string(tc.code), tc.params)
	}
}

// CX30、CX31、CX33、CX36：命令與修改檔案的審批報成 asked；Codex 清掉的請求之後回答不送任何東西；其他
// 請求各有回覆。
func TestAsk(t *testing.T) {
	c, got := mustStart(t, append(started, append(lTurn(4, 1),
		lItem(false, `{"type":"fileChange","id":"f1","changes":[{"path":"/x/a.go","kind":{"type":"update"},"diff":"@@"}],"status":"inProgress"}`),
		lAgent(`{"id":100,"method":"item/fileChange/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"f1","reason":"Fix the bug"}}`),
		`{"client":{"id":100,"result":{"decision":"accept"}}}`,
		lAgent(`{"id":101,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"c9","command":"rm -rf build","cwd":"/x","commandActions":[{"type":"unknown","command":"rm -rf build"}]}}`),
		lAgent(`{"id":102,"method":"mcpServer/elicitation/request","params":{"threadId":"thr_1","serverName":"s","mode":"form"}}`),
		lAgent(`{"id":103,"method":"item/permissions/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"p","permissions":{}}}`),
		lAgent(`{"id":104,"method":"item/tool/requestUserInput","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"q","isBlocking":true}}`),
		`{"client":{"id":102,"result":{"action":"decline"}}}`,
		`{"client":{"id":103,"result":{"permissions":{}}}}`,
		`{"client":{"id":104,"error":{"code":-32601}}}`,
		lNote("serverRequest/resolved", `{"threadId":"thr_1","requestId":101}`),
		lNote("item/agentMessage/delta", `{"turnId":"turn_1","itemId":"m","delta":"resolved"}`),
		// 回答已清掉的 101 若送了東西，這裡等到的就不是 turn/interrupt。
		`{"client":{"id":5,"method":"turn/interrupt","params":{"threadId":"thr_1","turnId":"turn_1"}}}`,
		`{"agent":{"id":5,"result":{}}}`, lDone(1, `"status":"interrupted"`))...)...)
	prompt(t, c, "fix")
	a := got.nextKind(t, task.FactAsked)
	if r := a.Asked.Request; r.Tool != "apply_patch" || r.Kind != task.ToolEdit || r.Title != "Fix the bug" || !slices.Equal(r.Paths, []string{"/x/a.go"}) {
		t.Fatalf("file change asked %+v", r)
	}
	if err := c.Answer(context.Background(), a.Asked.ID, task.Allow); err != nil {
		t.Fatal(err)
	}
	s := got.next(t, func(f task.Command) bool {
		return f.Kind == task.FactToolStarted && f.ToolStarted.ID == "c9" || f.Kind == task.FactAsked
	})
	if s.Kind != task.FactToolStarted {
		t.Fatalf("got %s before the command ask, want tool_started for c9", s.Kind)
	}
	a2 := got.nextKind(t, task.FactAsked)
	if r := a2.Asked.Request; r.Tool != "shell" || r.Kind != task.ToolExecute || r.Command != "rm -rf build" || r.Title != "rm -rf build" {
		t.Fatalf("command asked %+v", r)
	}
	got.next(t, func(f task.Command) bool { return f.Kind == task.FactOutput && f.Output.Text == "resolved" })
	if err := c.Answer(context.Background(), a2.Asked.ID, task.Deny); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnCancelled, "", nil)
}

// CX35：一輪之外的審批請求直接 decline。夾具收到 decline 才結束；沒回的話它一直等。
func TestAskDeclinedOutsideTurn(t *testing.T) {
	_, got := mustStart(t, append(started,
		lAgent(`{"id":200,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr_1","turnId":"turn_0","itemId":"c0","command":"ls"}}`),
		`{"client":{"id":200,"result":{"decision":"decline"}}}`, `{"exit":0}`)...)
	e := got.nextKind(t, task.FactExited)
	if e.Exited.Problem.Params["status"] != "0" {
		t.Fatalf("exited %+v, want status 0 after the decline", e.Exited)
	}
	for _, f := range got.all() {
		if f.Kind == task.FactAsked {
			t.Fatalf("asked outside a turn: %+v", f.Asked)
		}
	}
}

// CX35、CX40：中斷之後才到的審批請求直接 decline，不問使用者。
func TestAskAfterCancel(t *testing.T) {
	c, got := mustStart(t, append(started, append(lTurn(4, 1),
		`{"client":{"id":5,"method":"turn/interrupt","params":{"threadId":"thr_1","turnId":"turn_1"}}}`,
		lAgent(`{"id":201,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"c1","command":"rm -rf /"}}`),
		`{"client":{"id":201,"result":{"decision":"decline"}}}`,
		`{"agent":{"id":5,"result":{}}}`, lDone(1, `"status":"interrupted"`))...)...)
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

// CX40、CX41：中斷寫不進去而行程還在時回錯誤，下一次照樣再送；沒有一輪時中斷不送。
func TestCancelRetriesAfterFailedWrite(t *testing.T) {
	c, got, err := start(t, Options{ExitGrace: time.Second}, testLaunch(), "", fixture(t, append(started, append(lTurn(4, 1),
		`{"closeStdin":true}`, lNote("item/agentMessage/delta", `{"turnId":"turn_1","itemId":"m","delta":"stdin closed"}`), `{"hold":true}`)...)...))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(context.Background()); err != nil { // 沒有一輪在跑
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

// CX40：這一輪的 ID 還沒到就要求中斷時，ID 一到就送。turn/start 的回應不帶 ID，turn/started 要等
// OrielHub 回答了審批才送：中斷一定是在 ID 到之前要求的。
func TestCancelBeforeTurnID(t *testing.T) {
	c, got := mustStart(t, append(started,
		`{"client":{"id":4,"method":"turn/start","params":{"threadId":"thr_1"}}}`, `{"agent":{"id":4,"result":{}}}`,
		lAgent(`{"id":301,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"c1","command":"make"}}`),
		`{"client":{"id":301,"result":{"decision":"decline"}}}`,
		lNote("turn/started", `{"threadId":"thr_1","turn":{"id":"turn_1","status":"inProgress"}}`),
		`{"client":{"id":5,"method":"turn/interrupt","params":{"threadId":"thr_1","turnId":"turn_1"}}}`,
		`{"agent":{"id":5,"result":{}}}`, lDone(1, `"status":"interrupted"`))...)
	prompt(t, c, "go")
	a := got.nextKind(t, task.FactAsked)
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Answer(context.Background(), a.Asked.ID, task.Deny); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnCancelled, "", nil)
}

// CX32：回答寫不進去而行程還在時回錯誤（hub 不能以為 Codex 收到了）。
func TestAnswerWriteFails(t *testing.T) {
	c, got, err := start(t, Options{ExitGrace: time.Second}, testLaunch(), "", fixture(t, append(started, append(lTurn(4, 1),
		lAgent(`{"id":302,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr_1","turnId":"turn_1","itemId":"c1","command":"make"}}`),
		`{"closeStdin":true}`, lNote("item/agentMessage/delta", `{"turnId":"turn_1","itemId":"m","delta":"stdin closed"}`), `{"hold":true}`)...)...))
	if err != nil {
		t.Fatal(err)
	}
	prompt(t, c, "go")
	a := got.nextKind(t, task.FactAsked)
	got.next(t, func(f task.Command) bool { return f.Kind == task.FactOutput && f.Output.Text == "stdin closed" })
	var e *Error
	if err := c.Answer(context.Background(), a.Asked.ID, task.Allow); !errors.As(err, &e) || e.Code != CodeRequestFailed {
		t.Fatalf("Answer: %v, want %s", err, CodeRequestFailed)
	}
}

// CX13：推理摘要的段落之間空一行；有摘要的推理不再送原始文字。
func TestReasoning(t *testing.T) {
	c, got := mustStart(t, append(started, append(lTurn(4, 1),
		lNote("item/reasoning/summaryTextDelta", `{"turnId":"turn_1","itemId":"r1","delta":"First"}`),
		lNote("item/reasoning/summaryPartAdded", `{"turnId":"turn_1","itemId":"r1","summaryIndex":1}`),
		lNote("item/reasoning/summaryTextDelta", `{"turnId":"turn_1","itemId":"r1","delta":"Second"}`),
		lNote("item/reasoning/textDelta", `{"turnId":"turn_1","itemId":"r1","delta":"raw"}`),
		lNote("item/reasoning/textDelta", `{"turnId":"turn_1","itemId":"r2","delta":"only raw"}`),
		lOK(1))...)...)
	prompt(t, c, "go")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	var outs []string
	for _, f := range got.all() {
		if f.Kind == task.FactOutput {
			outs = append(outs, f.Output.Text)
		}
	}
	if want := []string{"First", "\n\n", "Second", "only raw"}; !slices.Equal(outs, want) {
		t.Fatalf("thinking %q, want %q", outs, want)
	}
}

// CX21：超過 10 MiB 的一行結束行程，原因是 codex.protocol_error。
func TestLongLine(t *testing.T) {
	c, got := mustStart(t, append(started, append(lTurn(4, 1), `{"raw":"`+strings.Repeat("x", maxLine+1)+`"}`)...)...)
	prompt(t, c, "go")
	e := got.turnEnd(t)
	if e.Kind != task.FactExited || e.Exited.Problem.Code != string(CodeProtocolError) {
		t.Fatalf("got %s %+v, want exited %s", e.Kind, e.Exited, CodeProtocolError)
	}
}

// CX42：Close 可以重複呼叫。
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
