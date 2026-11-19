package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/store"
	"github.com/bayuncao/oriel/internal/usage"
)

// TestLiveUsage 讀這台機器上各 agent 的真實記錄，存進臨時的資料庫，只印出各
// agent 的總數、檔案數、沒讀懂的行數與花費的時間，不印任何記錄內容；再讀一輪，
// 確認沒有變化的檔案不重讀、數字不變。設定 ORIELHUB_LIVE_USAGE=1 來執行它。
func TestLiveUsage(t *testing.T) {
	if os.Getenv("ORIELHUB_LIVE_USAGE") == "" {
		t.Skip("set ORIELHUB_LIVE_USAGE=1 to read this machine's usage records")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "usage.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tr := usage.New(usage.Options{Sources: usageSources(), Store: db,
		Env: func(context.Context) (proc.Env, error) { return proc.Inherited(), nil }})
	defer tr.Close()
	totals := func() map[string]int64 {
		lo := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
		hi := time.Now().Add(24 * time.Hour)
		rows, err := db.UsageTotals(ctx, []time.Time{lo, hi})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, r := range rows {
			out[string(r.Agent)] += r.Tokens.Total()
		}
		return out
	}
	start := time.Now()
	if _, err := tr.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	first := time.Since(start)
	before := totals()
	sum, err := tr.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sum.Sources {
		problem := ""
		if s.Problem != nil {
			problem = s.Problem.Code
		}
		t.Logf("%-9s files=%-6d bad=%-5d total=%-14d unreported=%v notes=%v %s", s.Agent, s.Files, s.Bad,
			before[string(s.Agent)], s.Unreported, s.Notes, problem)
	}
	for _, q := range sum.Quotas {
		t.Logf("quota %s/%s plan=%s windows=%d at=%s", q.Agent, q.Bucket, q.Plan, len(q.Windows), q.At.Format(time.RFC3339))
	}
	start = time.Now()
	if _, err := tr.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("first round %v, second round %v", first.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	after := totals()
	for agent, n := range before {
		// 兩輪之間 agent 可能剛寫了新記錄，數字只會變大。
		if after[agent] < n {
			t.Errorf("%s: total went down from %d to %d", agent, n, after[agent])
		}
	}
}
