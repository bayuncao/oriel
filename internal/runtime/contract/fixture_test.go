package contract

import (
	"encoding/json"
	"testing"
)

// K02：回放時 OrielHub 送來的訊息只核對夾具寫出的欄位。
func TestMatches(t *testing.T) {
	got := map[string]any{"method": "session/new", "params": map[string]any{
		"cwd": "/w", "mcpServers": []any{}, "_meta": map[string]any{"yoloMode": false}}}
	for _, tc := range []struct {
		want string
		ok   bool
	}{
		{`{"method":"session/new"}`, true},
		{`{"method":"session/new","params":{"cwd":"/w"}}`, true},
		{`{"method":"session/new","params":{"cwd":"<any>"}}`, true},
		{`{"method":"session/new","params":{"_meta":{"yoloMode":false}}}`, true},
		{`{"method":"session/new","params":{"_meta":{"yoloMode":true}}}`, false},
		{`{"method":"session/load"}`, false},
		{`{"method":"session/new","params":{"mcpServers":[{}]}}`, false},
		{`{"method":"session/new","params":{"missing":1}}`, false},
	} {
		var w any
		if err := json.Unmarshal([]byte(tc.want), &w); err != nil {
			t.Fatal(err)
		}
		if matches(w, got) != tc.ok {
			t.Errorf("matches(%s) = %v", tc.want, !tc.ok)
		}
	}
}

// K04：錄下的訊息換成佔位值，去掉 agent 的 _meta、擴充通知與帶使用者設定的清單，
// OrielHub 送出的只留要核對的欄位。
func TestScrub(t *testing.T) {
	in, err := ParseFixture([]byte(`
{"client":{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/home/u/w","mcpServers":[],"_meta":{"yoloMode":false},"extra":1}}}
{"agent":{"jsonrpc":"2.0","id":2,"result":{"sessionId":"abc","models":{"x":1},"_meta":{"host":"mac"}}}}
{"agent":{"jsonrpc":"2.0","method":"_x.ai/sessions/changed","params":{"cwd":"/home/u/w"}}}
{"agent":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"abc","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"secret-skill"}]}}}}
{"agent":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"abc","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"in /home/u/w and /home/u"}}}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Scrub(in, [][2]string{{"abc", PlaceSession}, {"/home/u/w", PlaceCwd}, {"/home/u", PlaceHome}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"id":2,"method":"session/new","params":{"_meta":{"yoloMode":false},"cwd":"<cwd>","mcpServers":[]}}`,
		`{"id":2,"jsonrpc":"2.0","result":{"sessionId":"<session>"}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"<session>","update":{"availableCommands":[],"sessionUpdate":"available_commands_update"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"<session>","update":{"content":{"text":"in <cwd> and <home>","type":"text"},"sessionUpdate":"agent_message_chunk"}}}`,
	}
	if len(out) != len(want) {
		t.Fatalf("%d lines, want %d", len(out), len(want))
	}
	for i, l := range out {
		got := string(l.Client)
		if l.Agent != nil {
			got = string(l.Agent)
		}
		if got != want[i] {
			t.Errorf("line %d\n got %s\nwant %s", i, got, want[i])
		}
	}
}
