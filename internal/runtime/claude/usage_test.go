package claude

import (
	"strconv"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/usage"
	"github.com/bayuncao/oriel/internal/usage/usagetest"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// assistant 是一行帶用量的記錄，形狀取自真實記錄、去掉內容。
func assistant(id, req string, in, cw, cr, out int64, extra string) string {
	return `{"type":"assistant","sessionId":"s1","cwd":"/work/a","timestamp":"2026-11-02T09:00:00.000Z","requestId":"` + req +
		`","message":{"id":"` + id + `","model":"claude-x","role":"assistant","usage":{"input_tokens":` + itoa(in) +
		`,"cache_creation_input_tokens":` + itoa(cw) + `,"cache_read_input_tokens":` + itoa(cr) +
		`,"output_tokens":` + itoa(out) + extra + `}}}` + "\n"
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestUsage(t *testing.T) {
	cases := []struct {
		name  string
		lines string
		want  usage.Tokens
		bad   int
	}{
		// U02
		{"one response", assistant("m1", "r1", 10, 200, 3000, 50, ""),
			usage.Tokens{Input: 10, CacheRead: 3000, CacheWrite: 200, Output: 50}, 0},
		// U03：一次回應寫了 3 行，輸出逐行變大。
		{"several lines of one response", assistant("m1", "r1", 10, 200, 3000, 1, "") +
			assistant("m1", "r1", 10, 200, 3000, 20, "") + assistant("m1", "r1", 10, 200, 3000, 50, ""),
			usage.Tokens{Input: 10, CacheRead: 3000, CacheWrite: 200, Output: 50}, 0},
		// 頂層是 0、數字只在 iterations 裡的那種行（真機上見過）。
		{"only iterations", assistant("m1", "r1", 0, 0, 0, 0,
			`,"iterations":[{"type":"message","input_tokens":2,"cache_creation_input_tokens":906,"cache_read_input_tokens":101069,"output_tokens":516}]`),
			usage.Tokens{Input: 2, CacheRead: 101069, CacheWrite: 906, Output: 516}, 0},
		{"two responses", assistant("m1", "r1", 1, 0, 0, 1, "") + assistant("m2", "r2", 2, 0, 0, 2, ""),
			usage.Tokens{Input: 3, Output: 3}, 0},
		// U13：不是 JSON 的行與讀不懂的用量行；使用者的行不算。
		{"bad lines", "not json\n" + `{"type":"assistant","message":{"id":"m9","usage":{"input_tokens":"x"}}}` + "\n" +
			`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" + assistant("m1", "r1", 1, 0, 0, 1, ""),
			usage.Tokens{Input: 1, Output: 1}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := usagetest.NewHome(t)
			home.Write(".claude/projects/-work-a/s1.jsonl", []byte(c.lines))
			got := usagetest.NewReader(t, Usage(), home, now).Read()
			if got.Total != c.want || got.Source.Bad != c.bad {
				t.Fatalf("total %+v bad %d, want %+v bad %d", got.Total, got.Source.Bad, c.want, c.bad)
			}
		})
	}
}

// 會話分叉時歷史被複製進新檔案：同一次回應只算一次；子 agent 的記錄也讀。
func TestForkAndSubagentFiles(t *testing.T) {
	home := usagetest.NewHome(t)
	line := assistant("m1", "r1", 10, 0, 0, 5, "")
	home.Write(".claude/projects/-work-a/s1.jsonl", []byte(line))
	home.Write(".claude/projects/-work-a/s2.jsonl", []byte(line+assistant("m2", "r2", 1, 0, 0, 1, "")))
	home.Write(".claude/projects/-work-a/s1/subagents/agent-a.jsonl", []byte(assistant("m3", "r3", 3, 0, 0, 0, "")))
	got := usagetest.NewReader(t, Usage(), home, now).Read()
	if got.Total.Total() != 15+2+3 || got.Source.Files != 3 {
		t.Fatalf("total %+v files %d", got.Total, got.Source.Files)
	}
	if got.Dirs["/work/a"] != 20 || got.Models["claude-x"] != 20 {
		t.Fatalf("dirs %v models %v", got.Dirs, got.Models)
	}
}

// U10：CLAUDE_CONFIG_DIR 指到別處時讀那裡，記錄來源顯示它。
func TestConfigDir(t *testing.T) {
	home := usagetest.NewHome(t)
	home.Write("elsewhere/projects/-work-a/s1.jsonl", []byte(assistant("m1", "r1", 1, 0, 0, 1, "")))
	home.Write(".claude/projects/-work-a/s1.jsonl", []byte(assistant("m2", "r2", 100, 0, 0, 1, "")))
	got := usagetest.NewReader(t, Usage(), home, now, "CLAUDE_CONFIG_DIR="+home.Path+"/elsewhere").Read()
	if got.Total.Total() != 2 || len(got.Source.Roots) != 1 || got.Source.Roots[0] != home.Path+"/elsewhere/projects" {
		t.Fatalf("total %+v roots %v", got.Total, got.Source.Roots)
	}
}
