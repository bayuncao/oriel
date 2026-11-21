//go:build unix

package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/runtime/contract"
	"github.com/bayuncao/oriel/internal/task"
)

// scriptedAgent 是 ORIELHUB_LIVE_AGENTS 裡開啟 TestScripted 的名字（PI52）。
const scriptedAgent = "pi-scripted"

// PI52：真實的 pi 載入 OrielHub 的審批擴充，配置目錄指向暫存目錄，模型是這裡按腳本回答的本機
// OpenAI 相容端點（不連網、不花額度）：允許與拒絕都走通，讀檔不詢問、但別的擴充覆寫的同名工具要詢問，
// 詢問掛著時 Cancel 是取消，
// 接回時是同一個會話。ORIELHUB_LIVE_AGENTS 含 pi-scripted 時才跑（要本機裝了 pi）。
func TestScripted(t *testing.T) {
	if !slices.Contains(strings.Split(os.Getenv(contract.EnvLive), ","), scriptedAgent) {
		t.Skipf("set %s=%s to run pi against a scripted model", contract.EnvLive, scriptedAgent)
	}
	l, ok := liveLaunch(t, "pi")
	if !ok {
		t.Skip("pi is not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(scriptedModel))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	writeJSON(t, filepath.Join(home, "models.json"), map[string]any{"providers": map[string]any{"scripted": map[string]any{
		"baseUrl": srv.URL + "/v1", "api": "openai-completions", "apiKey": "none",
		"models": []map[string]any{{"id": "scripted-1", "name": "Scripted", "reasoning": false, "input": []string{"text"},
			"contextWindow": 100000, "maxTokens": 1000, "cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}},
	}}})
	writeJSON(t, filepath.Join(home, "settings.json"), map[string]any{"defaultProvider": "scripted", "defaultModel": "scripted-1"})
	// 使用者自己的擴充覆寫了內建的 ls：同名不等於唯讀，要詢問（PI30）。
	if err := os.MkdirAll(filepath.Join(home, "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "extensions", "override-ls.ts"), []byte(overrideLs), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PI_CODING_AGENT_DIR="+home, "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1")
	r := newRuntime(t, Options{})
	open := func(resume task.NativeSessionID) (hub.Conn, *rec) {
		got := newRec()
		c, err := r.Start(context.Background(), hub.StartOptions{Launch: l, Workdir: dir, Resume: resume, Env: env, Emit: got.emit})
		if err != nil {
			if xe, ok := err.(hub.ExitError); ok {
				t.Fatalf("start: %v; stderr: %s", err, xe.StderrTail())
			}
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { c.Close() })
		return c, got
	}
	c, got := open("")
	if m := got.nextKind(t, task.FactModelReported); m.ModelReported.Setting.Current != "scripted/scripted-1" {
		t.Fatalf("model setting %+v", m.ModelReported.Setting)
	}
	marker := filepath.Join(dir, "orielhub-scripted.txt")

	t.Run("allow", func(t *testing.T) {
		prompt(t, c, "allow")
		a := got.nextKind(t, task.FactAsked).Asked
		if a.Request.Kind != task.ToolExecute || !strings.Contains(a.Request.Command, "orielhub-scripted") {
			t.Fatalf("asked %+v", a.Request)
		}
		if err := c.Answer(context.Background(), a.ID, task.Allow); err != nil {
			t.Fatal(err)
		}
		u := got.next(t, func(f task.Command) bool {
			return f.Kind == task.FactToolUpdated && f.ToolUpdated.Status != task.ToolRunning
		})
		if u.ToolUpdated.Status != task.ToolCompleted {
			t.Fatalf("tool %+v", u.ToolUpdated)
		}
		wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
		if b, err := os.ReadFile(marker); err != nil || string(b) != "ok" {
			t.Fatalf("the allowed command did not run: %q, %v", b, err)
		}
		os.Remove(marker)
	})
	t.Run("deny", func(t *testing.T) {
		prompt(t, c, "deny")
		a := got.nextKind(t, task.FactAsked).Asked
		if err := c.Answer(context.Background(), a.ID, task.Deny); err != nil {
			t.Fatal(err)
		}
		u := got.nextKind(t, task.FactToolUpdated)
		if u.ToolUpdated.Status != task.ToolFailed || !strings.Contains(u.ToolUpdated.Output, "Denied in OrielHub") {
			t.Fatalf("tool %+v", u.ToolUpdated)
		}
		wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("the denied command ran")
		}
	})
	t.Run("read", func(t *testing.T) {
		prompt(t, c, "read")
		end := got.turnEnd(t)
		wantTurn(t, end, task.TurnOK, "", nil)
		facts := got.all()
		i := slices.IndexFunc(facts, func(f task.Command) bool {
			return f.Kind == task.FactToolStarted && f.ToolStarted.Tool == "read"
		})
		if i < 0 || facts[i].ToolStarted.Kind != task.ToolRead || !slices.Equal(facts[i].ToolStarted.Paths, []string{filepath.Join(dir, "notes.txt")}) {
			t.Fatalf("no read tool in %v", got.kinds())
		}
		for _, f := range facts[i:] {
			if f.Kind == task.FactAsked {
				t.Fatal("reading a file asked OrielHub")
			}
		}
	})
	t.Run("overridden ls asks", func(t *testing.T) {
		prompt(t, c, "list")
		a := got.nextKind(t, task.FactAsked).Asked
		if a.Request.Tool != "ls" {
			t.Fatalf("asked %+v, want the overridden ls", a.Request)
		}
		if err := c.Answer(context.Background(), a.ID, task.Deny); err != nil {
			t.Fatal(err)
		}
		wantTurn(t, got.turnEnd(t), task.TurnOK, "", nil)
	})
	t.Run("cancel", func(t *testing.T) {
		prompt(t, c, "cancel")
		got.nextKind(t, task.FactAsked)
		if err := c.Cancel(context.Background()); err != nil {
			t.Fatal(err)
		}
		wantTurn(t, got.turnEnd(t), task.TurnCancelled, "", nil)
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("the command ran after Cancel")
		}
	})
	if !slices.Contains(got.kinds(), task.FactUsage) {
		t.Error("no usage reported")
	}
	t.Run("resume", func(t *testing.T) {
		id := c.NativeID()
		c.Close()
		c2, got2 := open(id)
		if b := got2.nextKind(t, task.FactNativeBound); b.Bound.NativeID != id {
			t.Fatalf("resumed as %q, want %q", b.Bound.NativeID, id)
		}
		prompt(t, c2, "hello")
		wantTurn(t, got2.turnEnd(t), task.TurnOK, "", nil)
	})
}

// overrideLs 是使用者自己的擴充：註冊一個同名的 ls 取代內建的（它的 sourceInfo 指向這個檔案）。
const overrideLs = `export default function (pi) {
	pi.registerTool({
		name: "ls",
		label: "ls",
		description: "List files (overridden)",
		parameters: { type: "object", properties: {} },
		async execute() {
			return { content: [{ type: "text", text: "overridden" }], details: undefined };
		},
	});
}
`

// scriptedModel 是按腳本回答的 OpenAI Chat Completions 串流端點：最後一則 user 訊息選腳本，有工具結果
// 時給最後的回答。
func scriptedModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var script string
	answered := false
	for _, m := range body.Messages {
		switch m.Role {
		case "user":
			script, answered = messageText(m.Content), false
		case "tool":
			answered = true
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	chunk := func(delta map[string]any, finish any, usage any) map[string]any {
		c := map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": "scripted-1",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			c["usage"] = usage
		}
		return c
	}
	usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
	var call map[string]any
	switch {
	case answered:
	case strings.Contains(script, "allow"), strings.Contains(script, "deny"), strings.Contains(script, "cancel"):
		call = map[string]any{"name": "bash", "arguments": `{"command":"printf ok > orielhub-scripted.txt"}`}
	case strings.Contains(script, "read"):
		call = map[string]any{"name": "read", "arguments": `{"path":"notes.txt"}`}
	case strings.Contains(script, "list"):
		call = map[string]any{"name": "ls", "arguments": `{}`}
	}
	if call != nil {
		send(chunk(map[string]any{"role": "assistant", "tool_calls": []map[string]any{
			{"index": 0, "id": "call_1", "type": "function", "function": call}}}, nil, nil))
		send(chunk(map[string]any{}, "tool_calls", usage))
	} else {
		send(chunk(map[string]any{"role": "assistant", "content": "done"}, nil, nil))
		send(chunk(map[string]any{}, "stop", usage))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// messageText 是訊息內容的文字（字串，或內容區塊的陣列）。
func messageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []content
	_ = json.Unmarshal(raw, &blocks)
	return text(blocks)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
