package pi

import (
	"strconv"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/usage"
	"github.com/bayuncao/oriel/internal/usage/usagetest"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// 形狀取自真機上的會話檔、去掉內容。
const header = `{"type":"session","version":3,"id":"0d0f6c1e-1c4b-4c39-9a3b-1b1b6c9f0e11","timestamp":"2026-11-02T08:00:00.000Z","cwd":"/work/a"}` + "\n"

func message(id string, ms int64, in, out, cr, cw, total int64) string {
	return `{"type":"message","id":"` + id + `","parentId":"x","timestamp":"2026-11-02T09:00:00.000Z","message":{"role":"assistant","api":"a","provider":"p","model":"m-1","usage":{"input":` +
		itoa(in) + `,"output":` + itoa(out) + `,"cacheRead":` + itoa(cr) + `,"cacheWrite":` + itoa(cw) +
		`,"totalTokens":` + itoa(total) + `,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":` + itoa(ms) + `}}` + "\n"
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

const ms = 1793091600000 // 2026-11-02T09:00:00Z

func TestUsage(t *testing.T) {
	cases := []struct {
		name  string
		lines string
		want  usage.Tokens
		bad   int
	}{
		// U31
		{"U31 one message", header + message("a1b2c3d4", ms, 100, 50, 2000, 300, 2450),
			usage.Tokens{Input: 100, CacheRead: 2000, CacheWrite: 300, Output: 50}, 0},
		{"total disagrees", header + message("a1b2c3d4", ms, 100, 50, 2000, 300, 9) + message("e5f6a7b8", ms+1, 1, 1, 0, 0, 2),
			usage.Tokens{Input: 1, Output: 1}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := usagetest.NewHome(t)
			home.Write(".pi/agent/sessions/--work-a--/2026-11-02T08-00-00-000Z_0d0f.jsonl", []byte(c.lines))
			got := usagetest.NewReader(t, Usage(), home, now).Read()
			if got.Total != c.want || got.Source.Bad != c.bad {
				t.Fatalf("total %+v bad %d, want %+v bad %d", got.Total, got.Source.Bad, c.want, c.bad)
			}
			if c.bad == 0 && (got.Dirs["/work/a"] != c.want.Total() || got.Models["m-1"] != c.want.Total()) {
				t.Fatalf("dirs %v models %v", got.Dirs, got.Models)
			}
		})
	}
}

// 分叉或複製出來的會話沿用同一項目（ID 與時間相同）時只算一次。
func TestCopiedEntriesCountOnce(t *testing.T) {
	home := usagetest.NewHome(t)
	m := message("a1b2c3d4", ms, 100, 50, 0, 0, 150)
	home.Write(".pi/agent/sessions/--work-a--/one.jsonl", []byte(header+m))
	home.Write(".pi/agent/sessions/--work-a--/two.jsonl", []byte(header+m+message("c3d4e5f6", ms+5, 1, 1, 0, 0, 2)))
	if got := usagetest.NewReader(t, Usage(), home, now).Read(); got.Total.Total() != 152 {
		t.Fatalf("total %+v", got.Total)
	}
}
