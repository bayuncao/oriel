// Package usagetest 供各 Source 的測試用：在臨時的家目錄上以真的 Tracker 與
// MemStore 讀一輪，回傳存下的用量（按 Key 合併之後）與記錄來源。
package usagetest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/usage"
)

// Result 是讀一輪的結果。
type Result struct {
	// Total 是 30 天內四類的和。
	Total  usage.Tokens
	Source usage.SourceInfo
	Quotas []usage.Quota
	// Dirs、Models 是 30 天內按目錄、模型的和。
	Dirs   map[string]int64
	Models map[string]int64
	// Days 是最近 30 天每天的總數，最早的在前。
	Days []int64
}

// Home 是一個臨時家目錄；Write 在它下面寫檔案（自動建目錄）。
type Home struct {
	t    *testing.T
	Path string
}

func NewHome(t *testing.T) *Home { return &Home{t: t, Path: t.TempDir()} }

// Write 寫入 rel（相對於家目錄）並回傳完整路徑。
func (h *Home) Write(rel string, content []byte) string {
	h.t.Helper()
	p := filepath.Join(h.Path, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// Reader 是同一個家目錄上的 Tracker，可以讀多輪。
type Reader struct {
	t   *testing.T
	tr  *usage.Tracker
	now time.Time
}

// NewReader 以 src 與環境 env（家目錄之外的變數）建 Tracker；now 決定「今天」（UTC）。
func NewReader(t *testing.T, src usage.Source, home *Home, now time.Time, env ...string) *Reader {
	e := proc.Env(append([]string{"HOME=" + home.Path}, env...))
	tr := usage.New(usage.Options{Sources: []usage.Source{src}, Store: usage.NewMemStore(),
		Env:  func(context.Context) (proc.Env, error) { return e, nil },
		Now:  func() time.Time { return now },
		Zone: func() *time.Location { return time.UTC }})
	t.Cleanup(tr.Close)
	return &Reader{t: t, tr: tr, now: now}
}

// Read 讀一輪並回傳結果。
func (r *Reader) Read() Result {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.tr.Refresh(ctx); err != nil {
		r.t.Fatal(err)
	}
	s, err := r.tr.Summary(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	out := Result{Source: s.Sources[0], Quotas: s.Quotas, Dirs: map[string]int64{}, Models: map[string]int64{}}
	month := s.Periods[len(s.Periods)-1]
	out.Total = month.Total
	for _, d := range month.Dirs {
		out.Dirs[d.Name] += d.Tokens.Total()
	}
	for _, m := range month.Models {
		out.Models[m.Name] += m.Tokens.Total()
	}
	for _, d := range s.Days {
		var n int64
		for _, a := range d.Agents {
			n += a.Tokens.Total()
		}
		out.Days = append(out.Days, n)
	}
	return out
}
