package acp

import (
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/usage"
	"github.com/bayuncao/oriel/internal/usage/usagetest"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// 以下的行形狀：Grok 取自真機上的記錄；Gemini、Qwen、Copilot、OpenCode 取自
// docs/research/usage.md 引的說明，待真機核實。全都去掉了內容。

func grokTurn(prompt string, models string) string {
	return fmt.Sprintf(`{"timestamp":1793091600,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":%q,"stop_reason":"end_turn","usage":{"inputTokens":0,"outputTokens":0,"totalTokens":0,"cachedReadTokens":0,"cacheCreationTokens":0,"reasoningTokens":0,"modelCalls":1,"apiDurationMs":10,"modelUsage":{%s},"numTurns":1},"elapsed_ms":10},"_meta":{"eventId":"e","agentTimestampMs":1793091600000}}}`, prompt, models) + "\n"
}

func grokModel(name string, in, out, cr, cc int64) string {
	return fmt.Sprintf(`%q:{"inputTokens":%d,"outputTokens":%d,"totalTokens":%d,"cachedReadTokens":%d,"cacheCreationTokens":%d,"reasoningTokens":0,"modelCalls":1,"apiDurationMs":10,"costUsdTicks":0}`,
		name, in, out, in+out, cr, cc)
}

// U41：一輪的用量按模型；沒完成的一輪不算；目錄從路徑解出。
func TestGrok(t *testing.T) {
	home := usagetest.NewHome(t)
	dir := url.PathEscape("/work/a b")
	unfinished := `{"timestamp":1793091700,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk"}}}` + "\n"
	home.Write(".grok/sessions/"+dir+"/s1/updates.jsonl", []byte(
		grokTurn("p1", grokModel("grok-4.7", 1000, 60, 700, 100))+unfinished+
			grokTurn("p2", grokModel("grok-4.7", 10, 1, 0, 0)+","+grokModel("grok-mini", 5, 1, 0, 0))+
			grokTurn("p2", grokModel("grok-4.7", 10, 1, 0, 0)+","+grokModel("grok-mini", 5, 1, 0, 0))))
	got := usagetest.NewReader(t, GrokUsage(), home, now).Read()
	want := usage.Tokens{Input: 200 + 10 + 5, CacheRead: 700, CacheWrite: 100, Output: 60 + 1 + 1}
	if got.Total != want || got.Dirs["/work/a b"] != want.Total() || got.Models["grok-mini"] != 6 {
		t.Fatalf("total %+v dirs %v models %v", got.Total, got.Dirs, got.Models)
	}
	if !slices.Equal(got.Source.Notes, []usage.Note{usage.NoteTurnEnd}) {
		t.Fatalf("notes %v", got.Source.Notes)
	}
}

func geminiLine(id string, in, cached, tool, thoughts, out int64) string {
	return fmt.Sprintf(`{"id":%q,"timestamp":"2026-11-02T09:00:00.000Z","type":"gemini","content":"","model":"gemini-x","tokens":{"input":%d,"output":%d,"cached":%d,"thoughts":%d,"tool":%d,"total":%d}}`,
		id, in, out, cached, thoughts, tool, in+out+thoughts+tool)
}

// U32、U33：整份改寫的會話檔變了就整份重讀，之前的不重複算；Gemini 不報緩存寫入。
func TestGemini(t *testing.T) {
	home := usagetest.NewHome(t)
	path := ".gemini/tmp/abc123/chats/session-1.json"
	one := geminiLine("m1", 1000, 600, 50, 30, 100)
	home.Write(path, []byte(`{"sessionId":"g1","projectHash":"abc123","messages":[{"id":"u1","type":"user","content":""},`+one+`]}`))
	r := usagetest.NewReader(t, GeminiUsage(), home, now)
	got := r.Read()
	if want := (usage.Tokens{Input: 450, CacheRead: 600, Output: 130}); got.Total != want {
		t.Fatalf("total %+v, want %+v", got.Total, want)
	}
	if !slices.Equal(got.Source.Unreported, []usage.Category{usage.CacheWrite}) {
		t.Fatalf("unreported %v", got.Source.Unreported)
	}
	home.Write(path, []byte(`{"sessionId":"g1","messages":[`+one+`,`+geminiLine("m2", 500, 400, 0, 0, 20)+`]}`))
	if got := r.Read(); got.Total.Total() != 1180+520 {
		t.Fatalf("after rewrite %+v", got.Total)
	}
}

// U34：prompt 含 cached，thoughts 另外算進輸出。
func TestQwen(t *testing.T) {
	home := usagetest.NewHome(t)
	line := `{"uuid":"q1","parentUuid":null,"sessionId":"s1","timestamp":"2026-11-02T09:00:00.000Z","type":"assistant","cwd":"/work/q","model":"qwen-x","message":{},"usageMetadata":{"promptTokenCount":800,"candidatesTokenCount":60,"thoughtsTokenCount":20,"cachedContentTokenCount":500,"totalTokenCount":880}}` + "\n"
	user := `{"uuid":"q0","sessionId":"s1","timestamp":"2026-11-02T08:59:00.000Z","type":"user","cwd":"/work/q","message":{}}` + "\n"
	home.Write(".qwen/projects/-work-q/chats/s1.jsonl", []byte(user+line+line))
	got := usagetest.NewReader(t, QwenUsage(), home, now).Read()
	if want := (usage.Tokens{Input: 300, CacheRead: 500, Output: 80}); got.Total != want || got.Dirs["/work/q"] != 880 {
		t.Fatalf("total %+v dirs %v", got.Total, got.Dirs)
	}
}

func shutdown(at string, in, out, cr, cw int64) string {
	return fmt.Sprintf(`{"type":"session.shutdown","id":"e","timestamp":%q,"data":{"modelMetrics":{"claude-x":{"requests":{"count":1,"cost":1},"usage":{"inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d}}}}}`,
		at, in, out, cr, cw) + "\n"
}

// U39：恢復後的 shutdown 是累計數，減去上一次的；U40：還沒 shutdown 的會話不算。
func TestCopilot(t *testing.T) {
	home := usagetest.NewHome(t)
	home.Write(".copilot/session-state/c1/events.jsonl", []byte(
		shutdown("2026-11-01T09:00:00.000Z", 1000, 50, 600, 100)+shutdown("2026-11-02T09:00:00.000Z", 1800, 90, 1200, 100)))
	home.Write(".copilot/session-state/c2/events.jsonl", []byte(`{"type":"session.start","id":"e","timestamp":"2026-11-02T09:00:00.000Z","data":{}}`+"\n"))
	got := usagetest.NewReader(t, CopilotUsage(), home, now).Read()
	if want := (usage.Tokens{Input: 300 + 200, CacheRead: 600 + 600, CacheWrite: 100, Output: 50 + 40}); got.Total != want {
		t.Fatalf("total %+v, want %+v", got.Total, want)
	}
	if days := got.Days; days[len(days)-1] != 840 || days[len(days)-2] != 1050 {
		t.Fatalf("days %v", days[len(days)-2:])
	}
	if !slices.Equal(got.Source.Notes, []usage.Note{usage.NoteSessionEnd}) {
		t.Fatalf("notes %v", got.Source.Notes)
	}
}

// U20、U89：Cursor 不在本機記 token：不讀任何檔案，只有說明（用量只有經 OrielHub 的上報）。
func TestCursor(t *testing.T) {
	got := usagetest.NewReader(t, CursorUsage(), usagetest.NewHome(t), now).Read()
	if got.Total.Total() != 0 || got.Source.Files != 0 || !slices.Equal(got.Source.Notes, []usage.Note{usage.NoteNotRecorded, usage.NoteReportedOnly}) {
		t.Fatalf("%+v", got)
	}
}

// 1.14 之前一則訊息一個 JSON 檔；用 claude-agent provider 的不算（U37）。
func TestOpenCodeLegacyFiles(t *testing.T) {
	home := usagetest.NewHome(t)
	msg := func(id, provider string) string {
		return fmt.Sprintf(`{"id":%q,"sessionID":"ses_1","role":"assistant","modelID":"m-1","providerID":%q,"cost":0,"tokens":{"input":100,"output":40,"reasoning":10,"cache":{"read":900,"write":50}},"time":{"created":1793091600000},"path":{"cwd":"/work/o","root":"/work/o"}}`, id, provider)
	}
	home.Write(".local/share/opencode/storage/message/ses_1/msg_1.json", []byte(msg("msg_1", "anthropic")))
	home.Write(".local/share/opencode/storage/message/ses_1/msg_2.json", []byte(msg("msg_2", "claude-agent")))
	got := usagetest.NewReader(t, OpenCodeUsage(nil), home, now).Read()
	if want := (usage.Tokens{Input: 100, CacheRead: 900, CacheWrite: 50, Output: 50}); got.Total != want || got.Dirs["/work/o"] != 1100 {
		t.Fatalf("total %+v dirs %v", got.Total, got.Dirs)
	}
}
