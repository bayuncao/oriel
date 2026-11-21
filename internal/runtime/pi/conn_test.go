//go:build unix

package pi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/runtime/contract"
	"github.com/bayuncao/oriel/internal/task"
)

// workdir 是連線的工作目錄（夾具的 <cwd>）。
func workdir(c hub.Conn) string { return c.(*conn).workdir }

// PI01、PI03、PI06、PI42：可選的模型在 native_bound 之前報（值是 provider/id，按提供方分組，目前的值取
// get_state）；會話 ID 取 pi 報的；審批擴充寫在只有本使用者能讀寫的暫存檔，Close 時刪掉。
func TestStart(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	c, got := mustStart(t, started...)
	first := got.next(t, func(task.Command) bool { return true })
	s := first.ModelReported
	want := []task.ModelChoice{{Value: "anthropic/claude-a", Name: "Claude A", Group: "anthropic"},
		{Value: "openai/gpt-b", Name: "GPT B", Group: "openai"}}
	if first.Kind != task.FactModelReported || !s.Start || s.Setting.Current != "anthropic/claude-a" || !slices.Equal(s.Setting.Choices, want) {
		t.Fatalf("first fact %s %+v", first.Kind, s)
	}
	if b := got.nextKind(t, task.FactNativeBound); b.Bound.NativeID != contract.ReplaySession || c.NativeID() != contract.ReplaySession {
		t.Fatalf("native_bound %q, NativeID %q", b.Bound.NativeID, c.NativeID())
	}
	ext := c.(*conn).ext
	if filepath.Dir(ext) != tmp || !strings.HasSuffix(ext, ".ts") {
		t.Fatalf("extension at %q, want a .ts file in %s", ext, tmp)
	}
	info, err := os.Stat(ext)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("extension file: %v, mode %v", err, info.Mode())
	}
	if b, _ := os.ReadFile(ext); !bytes.Equal(b, approvals) {
		t.Fatal("the extension file is not the embedded extension")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ext); !os.IsNotExist(err) {
		t.Fatalf("after Close the extension file is still there: %v", err)
	}
}

// PI03、PI04：審批經 OrielHub 而 get_commands 裡沒有擴充的命令時不啟動（暫存檔也刪掉）；原生寬鬆模式不加
// 擴充、不核對。
func TestStartChecksApprovals(t *testing.T) {
	t.Run("not loaded", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		lines := slices.Clone(started)
		lines[5] = lResponse(3, "get_commands", `{"commands":[{"name":"other","source":"extension"}]}`)
		_, _, err := start(t, Options{}, testLaunch(), "", fixture(t, lines...))
		if !errors.Is(err, fail(CodeApprovalsUnavailable, nil)) {
			t.Fatalf("start: %v, want %s", err, CodeApprovalsUnavailable)
		}
		if left, _ := os.ReadDir(tmp); len(left) != 0 {
			t.Fatalf("left %d files in the temporary directory", len(left))
		}
	})
	t.Run("native", func(t *testing.T) {
		l := testLaunch()
		l.Approval = task.ApprovalNative
		c, _, err := start(t, Options{}, l, "", fixture(t, lState, lStateOK, lModels, lModelsOK))
		if err != nil {
			t.Fatal(err)
		}
		if c.(*conn).ext != "" {
			t.Fatal("the native mode wrote the approvals extension")
		}
	})
}

// PI02：接回時 native_bound 是 pi 報的同一個 ID。
func TestResume(t *testing.T) {
	c, got, err := start(t, Options{}, testLaunch(), contract.ReplaySession, fixture(t, started...))
	if err != nil {
		t.Fatal(err)
	}
	if b := got.nextKind(t, task.FactNativeBound); b.Bound.NativeID != contract.ReplaySession || c.NativeID() != contract.ReplaySession {
		t.Fatalf("native_bound %q", b.Bound.NativeID)
	}
}

// PI05：建好會話前結束是 hub.ExitError；沒按時建好是 pi.start_timed_out；命令被拒是 pi.request_failed；
// get_state 沒有 sessionId 是 pi.protocol_error。
func TestStartFailures(t *testing.T) {
	t.Run("exits", func(t *testing.T) {
		_, _, err := start(t, Options{}, testLaunch(), "", fixture(t, lState, `{"stderr":"No session found"}`, `{"exit":1}`))
		var xe hub.ExitError
		if !errors.As(err, &xe) || xe.StderrTail() != "No session found" {
			t.Fatalf("start: %v, want an ExitError with the stderr tail", err)
		}
	})
	t.Run("times out", func(t *testing.T) {
		_, _, err := start(t, Options{StartTimeout: time.Second}, testLaunch(), "", fixture(t, lState))
		if !errors.Is(err, fail(CodeStartTimedOut, nil)) {
			t.Fatalf("start: %v, want %s", err, CodeStartTimedOut)
		}
	})
	t.Run("refused", func(t *testing.T) {
		_, _, err := start(t, Options{}, testLaunch(), "", fixture(t, lState,
			`{"agent":{"id":"oriel-1","type":"response","command":"get_state","success":false,"error":"no session"}}`))
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeRequestFailed || e.Params["command"] != "get_state" || e.Params["detail"] != "no session" {
			t.Fatalf("start: %v, want %s{get_state, no session}", err, CodeRequestFailed)
		}
	})
	t.Run("no session ID", func(t *testing.T) {
		_, _, err := start(t, Options{}, testLaunch(), "", fixture(t, lState, lResponse(1, "get_state", `{}`)))
		if !errors.Is(err, fail(CodeProtocolError, nil)) {
			t.Fatalf("start: %v, want %s", err, CodeProtocolError)
		}
	})
}

// PI06：pi 沒選模型（unknown 的佔位）、或選的不在可用的模型裡時不報目前的值；沒有可用的模型時不報模型設置。
func TestStartModels(t *testing.T) {
	t.Run("no current model", func(t *testing.T) {
		_, got := mustStart(t, lState, lResponse(1, "get_state",
			`{"sessionId":"s","model":{"provider":"unknown","id":"unknown","name":"unknown"}}`), lModels, lModelsOK, lCommands, lCommandsOK)
		if m := got.nextKind(t, task.FactModelReported); m.ModelReported.Setting.Current != "" || len(m.ModelReported.Setting.Choices) != 2 {
			t.Fatalf("model setting %+v", m.ModelReported.Setting)
		}
	})
	t.Run("current model not offered", func(t *testing.T) {
		_, got := mustStart(t, lState, lResponse(1, "get_state",
			`{"sessionId":"s","model":{"provider":"openai-codex","id":"gpt-5.5","name":"GPT-5.5"}}`), lModels, lModelsOK, lCommands, lCommandsOK)
		if m := got.nextKind(t, task.FactModelReported); m.ModelReported.Setting.Current != "" {
			t.Fatalf("current %q, want none: it is not one of the choices", m.ModelReported.Setting.Current)
		}
	})
	t.Run("no models", func(t *testing.T) {
		_, got := mustStart(t, lState, lStateOK, lModels, lResponse(2, "get_available_models", `{"models":[]}`), lCommands, lCommandsOK)
		if f := got.next(t, func(task.Command) bool { return true }); f.Kind != task.FactNativeBound {
			t.Fatalf("first fact %s, want native_bound", f.Kind)
		}
	})
}

// PI07：SetModel 送 set_model{provider, modelId}；被拒是 pi.request_failed；不是 provider/id 的值不送；
// 時限內沒回是 pi.set_model_timed_out。
func TestSetModel(t *testing.T) {
	t.Run("switches", func(t *testing.T) {
		c, _ := mustStart(t, append(slices.Clone(started),
			`{"client":{"id":"oriel-4","type":"set_model","provider":"openai","modelId":"gpt-b"}}`,
			lResponse(4, "set_model", `{"provider":"openai","id":"gpt-b"}`))...)
		set, err := c.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "openai/gpt-b"})
		if err != nil || set.Current != "openai/gpt-b" || len(set.Choices) != 2 {
			t.Fatalf("SetModel: %+v, %v", set, err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		c, _ := mustStart(t, append(slices.Clone(started),
			`{"client":{"id":"oriel-4","type":"set_model","provider":"openai","modelId":"gpt-b"}}`,
			`{"agent":{"id":"oriel-4","type":"response","command":"set_model","success":false,"error":"Model not found: openai/gpt-b"}}`)...)
		_, err := c.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "openai/gpt-b"})
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeRequestFailed || e.Params["command"] != "set_model" || e.Params["detail"] != "Model not found: openai/gpt-b" {
			t.Fatalf("SetModel: %v, want %s", err, CodeRequestFailed)
		}
	})
	t.Run("not a provider/model value", func(t *testing.T) {
		c, _ := mustStart(t, started...)
		var e *Error
		if _, err := c.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "gpt-b"}); !errors.As(err, &e) || e.Code != CodeRequestFailed {
			t.Fatalf("SetModel: %v, want %s", err, CodeRequestFailed)
		}
	})
	t.Run("times out", func(t *testing.T) {
		c, _, err := start(t, Options{SetModelTimeout: time.Second}, testLaunch(), "", fixture(t, append(slices.Clone(started),
			`{"client":{"id":"oriel-4","type":"set_model","provider":"openai","modelId":"gpt-b"}}`)...))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "openai/gpt-b"}); !errors.Is(err, fail(CodeSetModelTimedOut, nil)) {
			t.Fatalf("SetModel: %v, want %s", err, CodeSetModelTimedOut)
		}
	})
}

// lRejected 是 pi 拒絕 oriel-<n> 的 prompt。
func lRejected(n int, detail string) string {
	return fmt.Sprintf(`{"agent":{"id":"oriel-%d","type":"response","command":"prompt","success":false,"error":"%s"}}`, n, detail)
}

// PI10：prompt 被拒時這一輪失敗（沒有密鑰是 pi.auth_required）；擴充命令吃掉（handled）時這一輪完成；
// 已有一輪在跑時回 pi.turn_failed。
func TestPrompt(t *testing.T) {
	c, got := mustStart(t, append(slices.Clone(started),
		`{"client":{"id":"oriel-4","type":"prompt","message":"one"}}`, lRejected(4, `No API key found for the selected model.\n\nUse /login`),
		`{"client":{"id":"oriel-5","type":"prompt","message":"two"}}`, lRejected(5, "Agent is already streaming"),
		`{"client":{"id":"oriel-6","type":"prompt","message":"/cmd"}}`, lResponse(6, "prompt", `{"disposition":"handled"}`),
		`{"client":{"id":"oriel-7","type":"prompt","message":"four"}}`)...)
	prompt(t, c, "one")
	wantTurn(t, got.turnEnd(t), task.TurnError, string(CodeAuthRequired), nil)
	prompt(t, c, "two")
	wantTurn(t, got.turnEnd(t), task.TurnError, string(CodeRequestFailed), map[string]string{"command": "prompt", "detail": "Agent is already streaming"})
	prompt(t, c, "/cmd")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	prompt(t, c, "four")
	var e *Error
	if err := c.Prompt(context.Background(), task.Prompt{ID: "x", Text: "five"}); !errors.As(err, &e) || e.Code != CodeTurnFailed {
		t.Fatalf("second Prompt during a turn: %v, want %s", err, CodeTurnFailed)
	}
}

// PI11、PI12：附件以絕對路徑寫成一段文字；blob 只讓這一輪失敗。
func TestAttachments(t *testing.T) {
	c, got := mustStart(t, append(append(slices.Clone(started),
		lPrompt(4, "look\n\nAttached file: <cwd>/shot.png\n\nAttached file: <cwd>/notes.md")...), lSettled(false))...)
	if err := c.Prompt(context.Background(), task.Prompt{ID: "a", Text: "look", Attachments: []task.Attachment{
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

// PI13–PI17、PI19：文字與思考的增量；工具依 pi 的內建工具分類，路徑按工作目錄補全；「執行中」只報一次、
// 不帶輸出，結果在結束時報；用量
// 是這一輪 assistant 與工具結果的總和，先於結局報；agent_end 不算結束；一輪之外的不報。
func TestEvents(t *testing.T) {
	c, got := mustStart(t, append(append(slices.Clone(started), lPrompt(4, "go")...),
		lEvent("agent_start", ""),
		lDelta("text_delta", "Hel"), lDelta("text_delta", "lo"), lDelta("thinking_delta", "hmm"),
		lEvent("message_update", `"assistantMessageEvent":{"type":"text_end","contentIndex":0,"content":"Hello"}`),
		lAssistant("toolUse", `"usage":{"input":100,"output":10,"cacheRead":50,"cacheWrite":5,"totalTokens":165,"cost":{"total":0.01}}`),
		lEvent("tool_execution_start", `"toolCallId":"t1","toolName":"bash","args":{"command":"ls -la\necho hi"}`),
		lEvent("tool_execution_start", `"toolCallId":"t2","toolName":"read","args":{"path":"a.go"}`),
		lEvent("tool_execution_start", `"toolCallId":"t3","toolName":"edit","args":{"path":"/abs/b.go","edits":[]}`),
		lEvent("tool_execution_start", `"toolCallId":"t4","toolName":"write","args":{"path":"c.go","content":"x"}`),
		lEvent("tool_execution_start", `"toolCallId":"t5","toolName":"grep","args":{"pattern":"TODO"}`),
		lEvent("tool_execution_start", `"toolCallId":"t6","toolName":"find","args":{"pattern":"*.go","path":"src"}`),
		lEvent("tool_execution_start", `"toolCallId":"t7","toolName":"ls","args":{}`),
		lEvent("tool_execution_start", `"toolCallId":"t8","toolName":"mcp__docs__search","args":{"q":"x"}`),
		lEvent("tool_execution_update", `"toolCallId":"t1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"par"}]}`),
		lEvent("tool_execution_update", `"toolCallId":"t1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"partial"}]}`),
		lEvent("tool_execution_end", `"toolCallId":"t1","toolName":"bash","result":{"content":[{"type":"text","text":"out"}]},"isError":false`),
		lEvent("tool_execution_end", `"toolCallId":"t2","toolName":"read","result":{"content":[{"type":"text","text":"ENOENT"}]},"isError":true`),
		lEvent("message_end", `"message":{"role":"toolResult","toolCallId":"t8","usage":{"input":7,"output":3,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.002}}}`),
		lAssistant("stop", `"usage":{"input":20,"output":2,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.003}}`),
		lEvent("agent_end", `"messages":[],"willRetry":false`),
		lDelta("text_delta", "!"), lSettled(false),
		lDelta("text_delta", "outside"), lEvent("tool_execution_start", `"toolCallId":"t9","toolName":"bash","args":{"command":"x"}`),
		`{"client":{"id":"oriel-5","type":"set_model","provider":"openai","modelId":"gpt-b"}}`,
		lResponse(5, "set_model", `{}`))...)
	prompt(t, c, "go")
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	// 一輪之外的記錄在 set_model 的回應之前寫出：回應到了，它們都已讀過。
	if _, err := c.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "openai/gpt-b"}); err != nil {
		t.Fatal(err)
	}
	dir := workdir(c)
	var text, thinking string
	var tools []task.ToolCall
	var updates []task.ToolUpdate
	var usage *task.Usage
	facts := got.all()
	endAt := slices.IndexFunc(facts, func(f task.Command) bool { return f.Kind == task.FactTurnEnded })
	for i, f := range facts {
		switch f.Kind {
		case task.FactOutput:
			if i > endAt {
				t.Fatalf("output after the turn ended: %q", f.Output.Text)
			}
			if f.Output.Channel == task.ChannelThinking {
				thinking += f.Output.Text
			} else {
				text += f.Output.Text
			}
		case task.FactToolStarted:
			tools = append(tools, *f.ToolStarted)
		case task.FactToolUpdated:
			updates = append(updates, *f.ToolUpdated)
		case task.FactUsage:
			if i != endAt-1 {
				t.Fatalf("usage at %d, want right before the turn ended (%d)", i, endAt)
			}
			usage = f.Usage
		}
	}
	if text != "Hello!" || thinking != "hmm" {
		t.Fatalf("text %q, thinking %q", text, thinking)
	}
	type tool struct {
		name  string
		kind  task.ToolKind
		title string
		paths []string
	}
	want := []tool{
		{"bash", task.ToolExecute, "ls -la …", []string{}},
		{"read", task.ToolRead, "Read a.go", []string{filepath.Join(dir, "a.go")}},
		{"edit", task.ToolEdit, "Edit /abs/b.go", []string{"/abs/b.go"}},
		{"write", task.ToolEdit, "Write c.go", []string{filepath.Join(dir, "c.go")}},
		{"grep", task.ToolSearch, "Search TODO", []string{}},
		{"find", task.ToolSearch, "Find *.go", []string{filepath.Join(dir, "src")}},
		{"ls", task.ToolSearch, "List .", []string{}},
		{"mcp__docs__search", task.ToolOther, "mcp__docs__search", []string{}},
	}
	if len(tools) != len(want) {
		t.Fatalf("%d tools started, want %d (none outside the turn)", len(tools), len(want))
	}
	for i, w := range want {
		s := tools[i]
		if s.Tool != w.name || s.Kind != w.kind || s.Title != w.title || !slices.Equal(s.Paths, w.paths) {
			t.Errorf("tool %d: %+v, want %+v", i, s, w)
		}
	}
	if !strings.Contains(tools[0].Input, `"command"`) {
		t.Errorf("bash input %q", tools[0].Input)
	}
	if len(updates) != 3 || updates[0].Status != task.ToolRunning || updates[0].Output != "" ||
		updates[1].Status != task.ToolCompleted || updates[1].Output != "out" ||
		updates[2].Status != task.ToolFailed || updates[2].Output != "ENOENT" || updates[2].Kind != task.ToolRead {
		t.Fatalf("tool updates %+v", updates)
	}
	if usage == nil || usage.InputTokens != 127 || usage.OutputTokens != 15 || usage.CacheReadTokens != 50 ||
		usage.CacheWriteTokens != 5 || usage.CostUSD < 0.0149 || usage.CostUSD > 0.0151 {
		t.Fatalf("usage %+v", usage)
	}
}

// PI17、PI18：最後一條 assistant 是 error 時失敗（自動重試失敗時取它的原因）；pi 報 aborted 是取消；
// assistant 報出另一個可選的模型時報 model_reported；沒有用量時不報 usage。
func TestTurnOutcomes(t *testing.T) {
	lines := slices.Clone(started)
	lines = append(lines, lPrompt(4, "a")...)
	lines = append(lines, lAssistant("error", `"errorMessage":"rate limited"`), lSettled(false))
	lines = append(lines, lPrompt(5, "b")...)
	lines = append(lines, lEvent("auto_retry_end", `"success":false,"attempt":3,"finalError":"529 overloaded"`),
		lAssistant("error", `"errorMessage":"529 overloaded (final)"`), lSettled(false))
	lines = append(lines, lPrompt(6, "c")...)
	lines = append(lines, lEvent("message_end", `"message":{"role":"assistant","provider":"openai","model":"gpt-b","stopReason":"stop"}`),
		lSettled(true))
	c, got := mustStart(t, lines...)
	prompt(t, c, "a")
	wantTurn(t, got.turnEnd(t), task.TurnError, string(CodeTurnFailed), map[string]string{"detail": "rate limited"})
	prompt(t, c, "b")
	wantTurn(t, got.turnEnd(t), task.TurnError, string(CodeTurnFailed), map[string]string{"detail": "529 overloaded"})
	prompt(t, c, "c")
	m := got.next(t, func(f task.Command) bool { return f.Kind == task.FactModelReported && !f.ModelReported.Start })
	if m.ModelReported.Setting.Current != "openai/gpt-b" {
		t.Fatalf("model reported %+v", m.ModelReported.Setting)
	}
	wantTurn(t, got.turnEnd(t), task.TurnCancelled, "", nil)
	if slices.Contains(got.kinds(), task.FactUsage) {
		t.Fatal("usage reported for turns without any")
	}
}

// PI30–PI34：審批擴充的詢問報成 asked（沒出現過的工具先補 tool_started）；允許與拒絕是 confirmed
// true / false，已回答的不再送；讀不懂的詢問與一輪之外的詢問直接回不允許；別的擴充的對話回 cancelled，
// 不等回應的不理。
func TestApprovals(t *testing.T) {
	c, got := mustStart(t, append(append(slices.Clone(started), lPrompt(4, "go")...),
		lEvent("tool_execution_start", `"toolCallId":"c1","toolName":"bash","args":{"command":"make test"}`),
		lAsk("ui-1", "c1", "bash", `{"command":"make test"}`), lAnswer("ui-1", "confirmed", true),
		lAsk("ui-2", "c2", "write", `{"path":"out.txt","content":"x"}`), lAnswer("ui-2", "confirmed", false),
		lEvent("extension_ui_request", `"id":"ui-3","method":"select","title":"Pick","options":["a","b"]`), lAnswer("ui-3", "cancelled", true),
		lEvent("extension_ui_request", `"id":"ui-4","method":"notify","message":"hi"`),
		lEvent("extension_ui_request", `"id":"ui-5","method":"confirm","title":"orielhub.permission","message":"not json"`), lAnswer("ui-5", "confirmed", false),
		lSettled(false),
		lAsk("ui-6", "c3", "bash", `{"command":"late"}`), lAnswer("ui-6", "confirmed", false), `{"exit":0}`)...)
	prompt(t, c, "go")
	a := got.nextKind(t, task.FactAsked).Asked
	if r := a.Request; r.Tool != "bash" || r.Kind != task.ToolExecute || r.Command != "make test" || r.Title != "make test" ||
		r.Input != `{"command":"make test"}` || r.Paths == nil {
		t.Fatalf("asked %+v", r)
	}
	if err := c.Answer(context.Background(), a.ID, task.Allow); err != nil {
		t.Fatal(err)
	}
	s := got.nextKind(t, task.FactToolStarted)
	if s.ToolStarted.ID != "c2" || s.ToolStarted.Kind != task.ToolEdit {
		t.Fatalf("tool_started for the unseen call: %+v", s.ToolStarted)
	}
	b := got.nextKind(t, task.FactAsked).Asked
	if b.Request.Title != "Write out.txt" || b.ID == a.ID {
		t.Fatalf("second ask %+v", b)
	}
	for range 2 { // 第二次不再送：否則夾具下一行（ui-3 的 cancelled）對不上
		if err := c.Answer(context.Background(), b.ID, task.Deny); err != nil {
			t.Fatal(err)
		}
	}
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	// 夾具核對到最後一行（ui-6 的回答）才以 0 結束；對不上時是 97。
	if e := got.nextKind(t, task.FactExited); e.Exited.Problem.Params["status"] != "0" {
		t.Fatalf("exited %+v: the answers did not match the fixture", e.Exited)
	}
	if n := len(slices.DeleteFunc(got.kinds(), func(k task.CommandKind) bool { return k != task.FactAsked })); n != 2 {
		t.Fatalf("%d asks, want 2", n)
	}
}

// PI40、PI41：Cancel 先把掛著的詢問回成不允許，再送 abort；之後的 agent_settled 是取消；再 Cancel 與之後
// 的 Answer 都不送；沒有一輪時 Cancel 不送。
func TestCancel(t *testing.T) {
	c, got := mustStart(t, append(append(slices.Clone(started), lPrompt(4, "go")...),
		lEvent("tool_execution_start", `"toolCallId":"c1","toolName":"bash","args":{"command":"make"}`),
		lAsk("ui-1", "c1", "bash", `{"command":"make"}`),
		lAnswer("ui-1", "confirmed", false), `{"client":{"id":"oriel-5","type":"abort"}}`,
		lEvent("tool_execution_end", `"toolCallId":"c1","toolName":"bash","result":{"content":[{"type":"text","text":"Operation aborted"}]},"isError":true`),
		lAssistant("error", `"errorMessage":"This operation was aborted"`), lSettled(true), lResponse(5, "abort", `{}`),
		`{"client":{"id":"oriel-6","type":"prompt","message":"next"}}`, lResponse(6, "prompt", `{"disposition":"handled"}`))...)
	if err := c.Cancel(context.Background()); err != nil { // 沒有一輪在跑：不送
		t.Fatal(err)
	}
	prompt(t, c, "go")
	a := got.nextKind(t, task.FactAsked)
	for range 2 {
		if err := c.Cancel(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Answer(context.Background(), a.Asked.ID, task.Allow); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnCancelled, "", nil)
	prompt(t, c, "next") // 多送的 abort 或回答會讓夾具的這一行對不上
	wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
}

// PI40：prompt 還沒回應就要求中斷時，回應一到就送 abort。pi 要等讀到 set_model 才回應 prompt，所以
// 中斷一定是在回應之前要求的。
func TestCancelBeforePromptAnswered(t *testing.T) {
	c, got := mustStart(t, append(slices.Clone(started),
		`{"client":{"id":"oriel-4","type":"prompt","message":"go"}}`,
		`{"client":{"id":"oriel-5","type":"set_model","provider":"openai","modelId":"gpt-b"}}`,
		lResponse(4, "prompt", `{"disposition":"started"}`),
		`{"client":{"id":"oriel-6","type":"abort"}}`,
		lResponse(5, "set_model", `{}`), lSettled(true))...)
	prompt(t, c, "go")
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "openai/gpt-b"}); err != nil {
		t.Fatal(err)
	}
	wantTurn(t, got.turnEnd(t), task.TurnCancelled, "", nil)
}

// PI40：abort 寫不進去而行程還在時回 pi.request_failed，下一次 Cancel 照樣再送。
func TestCancelRetriesAfterFailedWrite(t *testing.T) {
	c, got, err := start(t, Options{ExitGrace: time.Second}, testLaunch(), "", fixture(t, append(append(slices.Clone(started),
		lPrompt(4, "go")...), `{"closeStdin":true}`, lDelta("text_delta", "stdin closed"), `{"hold":true}`)...))
	if err != nil {
		t.Fatal(err)
	}
	prompt(t, c, "go")
	got.next(t, func(f task.Command) bool { return f.Kind == task.FactOutput && f.Output.Text == "stdin closed" })
	for i := range 2 {
		var e *Error
		if err := c.Cancel(context.Background()); !errors.As(err, &e) || e.Code != CodeRequestFailed {
			t.Fatalf("Cancel #%d: %v, want %s (the abort was tried again)", i+1, err, CodeRequestFailed)
		}
	}
}

// PI40：Cancel 回掉詢問時寫不進去而行程還在：詢問放回去，hub 之後的 Answer 照樣送（同樣寫不進去，回錯誤，
// 而不是以為詢問已不在而回 nil）。
func TestCancelKeepsAsksWhenWriteFails(t *testing.T) {
	c, got, err := start(t, Options{ExitGrace: time.Second}, testLaunch(), "", fixture(t, append(append(slices.Clone(started),
		lPrompt(4, "go")...), lAsk("ui-1", "c1", "bash", `{"command":"make"}`),
		`{"closeStdin":true}`, lDelta("text_delta", "stdin closed"), `{"hold":true}`)...))
	if err != nil {
		t.Fatal(err)
	}
	prompt(t, c, "go")
	a := got.nextKind(t, task.FactAsked)
	got.next(t, func(f task.Command) bool { return f.Kind == task.FactOutput && f.Output.Text == "stdin closed" })
	var e *Error
	if err := c.Cancel(context.Background()); !errors.As(err, &e) || e.Code != CodeRequestFailed {
		t.Fatalf("Cancel: %v, want %s", err, CodeRequestFailed)
	}
	if err := c.Answer(context.Background(), a.Asked.ID, task.Deny); !errors.As(err, &e) || e.Code != CodeRequestFailed {
		t.Fatalf("Answer after a failed Cancel: %v, want %s (the ask was put back)", err, CodeRequestFailed)
	}
}

// PI32：回答寫不進去而行程還在時回錯誤（hub 不能以為 pi 收到了）。
func TestAnswerWriteFails(t *testing.T) {
	c, got, err := start(t, Options{ExitGrace: time.Second}, testLaunch(), "", fixture(t, append(append(slices.Clone(started),
		lPrompt(4, "go")...), lAsk("ui-1", "c1", "bash", `{"command":"make"}`),
		`{"closeStdin":true}`, lDelta("text_delta", "stdin closed"), `{"hold":true}`)...))
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

// PI20：一輪中途行程結束只報 exited；一行超過 10 MiB、stdout 關了而行程還在是 pi.protocol_error。
func TestExits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		code  string
	}{
		{"exits", []string{`{"stderr":"boom"}`, `{"exit":3}`}, "proc.exited"},
		{"long line", []string{`{"raw":"` + strings.Repeat("x", maxLine+1) + `"}`}, string(CodeProtocolError)},
		{"stdout closed", []string{`{"closeStdout":true}`, `{"hold":true}`}, string(CodeProtocolError)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, got, err := start(t, Options{ExitGrace: time.Second}, testLaunch(), "",
				fixture(t, append(append(slices.Clone(started), lPrompt(4, "go")...), tc.lines...)...))
			if err != nil {
				t.Fatal(err)
			}
			prompt(t, c, "go")
			e := got.turnEnd(t)
			if e.Kind != task.FactExited || e.Exited.Problem.Code != tc.code {
				t.Fatalf("got %s %+v, want exited %s", e.Kind, e.Exited, tc.code)
			}
		})
	}
}

// PI42：Close 可以重複呼叫。
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
