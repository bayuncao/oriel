package codex

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/bayuncao/oriel/internal/usage"
	"github.com/bayuncao/oriel/internal/usage/usagetest"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// 以下的行形狀取自真機上的 rollout、去掉內容。

func metaLine(id, parent string, start int) string {
	extra := ""
	if parent != "" {
		extra = fmt.Sprintf(`,"parent_thread_id":%q,"subagent_history_start_ordinal":%d`, parent, start)
	}
	return fmt.Sprintf(`{"timestamp":"2026-11-02T08:00:00.000Z","ordinal":0,"type":"session_meta","payload":{"id":%q,"cwd":"/work/a","cli_version":"0.166.0"%s}}`, id, extra) + "\n"
}

func contextLine(ord int, model string) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-02T08:00:01.000Z","ordinal":%d,"type":"turn_context","payload":{"cwd":"/work/a","model":%q}}`, ord, model) + "\n"
}

type u struct{ in, cached, out, reasoning int64 }

func (x u) json() string {
	return fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":%d,"total_tokens":%d}`,
		x.in, x.cached, x.out, x.reasoning, x.in+x.out)
}

func (x u) plus(y u) u {
	return u{x.in + y.in, x.cached + y.cached, x.out + y.out, x.reasoning + y.reasoning}
}

const limits = `{"limit_id":"codex","limit_name":null,"plan_type":"pro","primary":{"used_percent":41.0,"window_minutes":300,"resets_at":1793000000},"secondary":{"used_percent":12.0,"window_minutes":10080,"resets_at":1793400000}}`

func countLine(ord int, total, last u) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-02T09:00:00.000Z","ordinal":%d,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":%s,"last_token_usage":%s,"model_context_window":258400},"rate_limits":%s}}`,
		ord, total.json(), last.json(), limits) + "\n"
}

func recordLine(ord int, resp string, use u) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-02T09:00:00.000Z","ordinal":%d,"type":"token_usage_record","payload":{"thread_id":"t1","turn_id":"x","session_id":"t1","root_turn_id":"x","response_id":%q,"usage":%s,"turn_token_usage":%s,"thread_token_usage":%s}}`,
		ord, resp, use.json(), use.json(), use.json()) + "\n"
}

var (
	call1 = u{1000, 800, 100, 40}
	call2 = u{1500, 1200, 60, 0}
	// twoCalls 是 U04 的兩次呼叫：輸入 500、緩存讀取 2,000、輸出 160。
	twoCalls = metaLine("t1", "", 0) + contextLine(1, "gpt-x") + countLine(2, call1, call1) + countLine(3, call1.plus(call2), call2)
)

func zst(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf)
	w.Write([]byte(s))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUsage(t *testing.T) {
	twoCallsTokens := usage.Tokens{Input: 500, CacheRead: 2000, Output: 160}
	child := u{100, 0, 10, 0}
	cases := []struct {
		name  string
		files map[string]string
		want  usage.Tokens
		bad   int
	}{
		{"U04 token_count increments", map[string]string{"sessions/2026/11/02/rollout-a.jsonl": twoCalls}, twoCallsTokens, 0},
		// U05：只有額度變化的 token_count 重複上一次的 last，累計不變，不算。
		{"U05 rate limits only", map[string]string{"sessions/2026/11/02/rollout-a.jsonl": twoCalls +
			countLine(4, call1.plus(call2), call2)}, twoCallsTokens, 0},
		// U06：子線程開頭重放父線程的記錄（ordinal 在 subagent_history_start_ordinal 之前），不算。
		{"U06 subagent replay", map[string]string{
			"sessions/2026/11/02/rollout-a.jsonl": twoCalls,
			"sessions/2026/11/02/rollout-b.jsonl": metaLine("t2", "t1", 10) + metaLine("t1", "", 0) + countLine(2, call1, call1) +
				countLine(3, call1.plus(call2), call2) + contextLine(10, "gpt-x") + countLine(11, call1.plus(call2).plus(child), child)},
			usage.Tokens{Input: 600, CacheRead: 2000, Output: 170}, 0},
		// U07：同一個 rollout 在 sessions/ 與 archived_sessions/ 各一份。
		{"U07 archived copy", map[string]string{"sessions/2026/11/02/rollout-a.jsonl": twoCalls,
			"archived_sessions/rollout-a.jsonl": twoCalls}, twoCallsTokens, 0},
		// 0.153 起的 token_usage_record：從它起只算它（它寫在自己的 token_count 之前）。
		{"records replace counts", map[string]string{"sessions/2026/11/02/rollout-a.jsonl": metaLine("t1", "", 0) +
			countLine(1, call1, call1) + recordLine(2, "resp_2", call2) + countLine(3, call1.plus(call2), call2) +
			recordLine(4, "resp_compact", u{50, 0, 5, 0})},
			usage.Tokens{Input: 200 + 300 + 50, CacheRead: 2000, Output: 165}, 0},
		// U42：總數與輸入加輸出不等的那一條不算，算沒讀懂。
		{"U42 totals disagree", map[string]string{"sessions/2026/11/02/rollout-a.jsonl": metaLine("t1", "", 0) +
			`{"timestamp":"2026-11-02T09:00:00.000Z","ordinal":1,"type":"token_usage_record","payload":{"thread_id":"t1","response_id":"r1","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5,"total_tokens":25}}}` + "\n" +
			recordLine(2, "r2", call1)}, usage.Tokens{Input: 200, CacheRead: 800, Output: 100}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := usagetest.NewHome(t)
			for rel, content := range c.files {
				home.Write(".codex/"+rel, []byte(content))
			}
			got := usagetest.NewReader(t, Usage(), home, now).Read()
			if got.Total != c.want || got.Source.Bad != c.bad {
				t.Fatalf("total %+v bad %d, want %+v bad %d", got.Total, got.Source.Bad, c.want, c.bad)
			}
			if got.Models["gpt-x"] == 0 && c.name != "records replace counts" && c.name != "U42 totals disagree" {
				t.Fatalf("models %v", got.Models)
			}
		})
	}
}

// U43：壓縮的歸檔照讀；同一目錄裡有未壓縮的那份時只讀它。
// U44：讀過之後又追加、隨後被壓縮（未壓縮的那份不在了）：追加的也算，之前的不重複算。
func TestCompressed(t *testing.T) {
	home := usagetest.NewHome(t)
	home.Write(".codex/archived_sessions/rollout-a.jsonl.zst", zst(t, twoCalls))
	other := metaLine("t9", "", 0) + contextLine(1, "gpt-x") + countLine(2, call1, call1)
	home.Write(".codex/archived_sessions/rollout-b.jsonl", []byte(other))
	home.Write(".codex/archived_sessions/rollout-b.jsonl.zst", zst(t, other))
	r := usagetest.NewReader(t, Usage(), home, now)
	got := r.Read()
	if got.Total.Total() != 2660+1100 || got.Source.Files != 2 {
		t.Fatalf("total %+v files %d", got.Total, got.Source.Files)
	}

	home2 := usagetest.NewHome(t)
	plain := home2.Write(".codex/sessions/2026/11/02/rollout-c.jsonl", []byte(twoCalls))
	r2 := usagetest.NewReader(t, Usage(), home2, now)
	r2.Read()
	call3 := u{10, 0, 1, 0}
	appended := twoCalls + countLine(4, call1.plus(call2).plus(call3), call3)
	home2.Write(".codex/sessions/2026/11/02/rollout-c.jsonl.zst", zst(t, appended))
	removeFile(t, plain)
	if got := r2.Read(); got.Total.Total() != 2660+11 {
		t.Fatalf("total %+v", got.Total)
	}
}

// U21、U23：額度取最近的快照，每個額度桶一組，主、次兩個窗口。
func TestQuota(t *testing.T) {
	home := usagetest.NewHome(t)
	other := `{"timestamp":"2026-11-02T10:00:00.000Z","ordinal":5,"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex_other","limit_name":"Other model","plan_type":"pro","primary":{"used_percent":5,"window_minutes":300,"resets_at":1793000000},"secondary":null}}}` + "\n"
	home.Write(".codex/sessions/2026/11/02/rollout-a.jsonl", []byte(twoCalls+other))
	got := usagetest.NewReader(t, Usage(), home, now).Read()
	if len(got.Quotas) != 2 {
		t.Fatalf("quotas %+v", got.Quotas)
	}
	q := got.Quotas[0]
	if q.Bucket != "codex" || q.Plan != "pro" || len(q.Windows) != 2 || q.Windows[0].Minutes != 300 ||
		q.Windows[0].UsedPercent != 41 || q.Windows[1].Minutes != 10080 || !q.Windows[0].ResetsAt.Equal(time.Unix(1793000000, 0)) {
		t.Fatalf("quota %+v", q)
	}
	if got.Quotas[1].Bucket != "codex_other" || got.Quotas[1].Name != "Other model" || len(got.Quotas[1].Windows) != 1 {
		t.Fatalf("second bucket %+v", got.Quotas[1])
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
