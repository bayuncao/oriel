package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// testSource 是只認測試格式的真 Source：<root>/<agent>/ 下的 *.jsonl（Lines）、
// *.jsonl.zst（Compressed）、*.json（Whole，一個 JSON 陣列）與 *.db（Database：
// 每行一條，旁邊有 .lock 檔時回 ErrBusy）。一行是
// {"k":Key,"at":RFC3339,"s":會話,"d":目錄,"m":模型,"i":輸入,"o":輸出}，帶 "bad" 的
// 或不是 JSON 物件的算沒讀懂。gate 非 nil 時 Find 先等它（控制一輪的時序）。
type testSource struct {
	agent task.AgentID
	// gate 非 nil 時 Find 先送 entered、再等 gate：測試據此知道一輪已經開始。
	gate    chan struct{}
	entered chan struct{}

	mu    sync.Mutex
	finds int
	opens []string // Parser 打開的檔案與起始狀態
}

func (s *testSource) Agent() task.AgentID     { return s.agent }
func (s *testSource) Unreported() []Category  { return []Category{CacheWrite} }
func (s *testSource) Notes() []Note           { return []Note{NoteTurnEnd} }
func (s *testSource) dir(env proc.Env) string { return filepath.Join(env.Home(), string(s.agent)) }

func (s *testSource) Find(env proc.Env) ([]string, []File, error) {
	s.mu.Lock()
	s.finds++
	gate, entered := s.gate, s.entered
	s.mu.Unlock()
	if gate != nil {
		entered <- struct{}{}
		<-gate
	}
	root := s.dir(env)
	files, err := Walk(root, func(path string) (Format, bool) {
		switch {
		case strings.HasSuffix(path, ".jsonl"):
			return Lines, true
		case strings.HasSuffix(path, ".jsonl.zst"):
			return Compressed, true
		case strings.HasSuffix(path, ".json"):
			return Whole, true
		case strings.HasSuffix(path, ".db"):
			return Database, true
		}
		return "", false
	})
	return []string{root}, files, err
}

func (s *testSource) Parser(f File, state []byte) (Parser, error) {
	s.mu.Lock()
	s.opens = append(s.opens, filepath.Base(f.Path)+":"+string(state))
	s.mu.Unlock()
	p := &testParser{whole: f.Format == Whole}
	if len(state) > 0 {
		if err := json.Unmarshal(state, &p.n); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (s *testSource) ReadDatabase(_ context.Context, f File, state []byte) ([]Record, []byte, bool, error) {
	if _, err := os.Stat(f.Path + ".lock"); err == nil {
		return nil, state, false, ErrBusy
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, state, false, err
	}
	p := &testParser{}
	var recs []Record
	for line := range bytes.Lines(b) {
		r, _, _ := p.Line(bytes.TrimSpace(line))
		recs = append(recs, r...)
	}
	return recs, []byte(`"db"`), false, nil
}

func (s *testSource) opened() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.opens)
}

// testParser 的狀態是讀過的行數，用來確認接著讀時沿用存下的狀態。
type testParser struct {
	whole bool
	n     int
}

func (p *testParser) State() []byte { b, _ := json.Marshal(p.n); return b }

type testLine struct {
	K   string    `json:"k"`
	At  time.Time `json:"at"`
	S   string    `json:"s"`
	D   string    `json:"d"`
	M   string    `json:"m"`
	I   int64     `json:"i"`
	O   int64     `json:"o"`
	Bad bool      `json:"bad"`
}

func (l testLine) record() Record {
	return Record{Key: l.K, At: l.At, Session: l.S, Dir: l.D, Model: l.M, Tokens: Tokens{Input: l.I, Output: l.O}}
}

func (p *testParser) Line(b []byte) ([]Record, []Quota, bool) {
	p.n++
	if p.whole {
		var ls []testLine
		if json.Unmarshal(b, &ls) != nil {
			return nil, nil, true
		}
		var out []Record
		for _, l := range ls {
			out = append(out, l.record())
		}
		return out, nil, false
	}
	if !IsObject(b) {
		return nil, nil, true
	}
	var l testLine
	if json.Unmarshal(b, &l) != nil || l.Bad {
		return nil, nil, true
	}
	if l.K == "quota" {
		return nil, []Quota{{Bucket: l.S, At: l.At, Windows: []Limit{{Minutes: 300, UsedPercent: float64(l.I)}}}}, false
	}
	return []Record{l.record()}, nil, false
}

var day0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func ln(key string, at time.Time, in, out int64) string {
	b, _ := json.Marshal(testLine{K: key, At: at, S: "s1", I: in, O: out})
	return string(b) + "\n"
}

type fixture struct {
	t     *testing.T
	home  string
	store *MemStore
	src   *testSource
	opts  Options
}

func newFixture(t *testing.T) *fixture {
	home := t.TempDir()
	f := &fixture{t: t, home: home, store: NewMemStore(), src: &testSource{agent: "test"}}
	f.opts = Options{Sources: []Source{f.src}, Store: f.store,
		Env:  func(context.Context) (proc.Env, error) { return proc.Env{"HOME=" + home}, nil },
		Now:  func() time.Time { return day0 },
		Zone: func() *time.Location { return time.UTC }}
	if err := os.MkdirAll(filepath.Join(home, "test"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) path(name string) string { return filepath.Join(f.home, "test", name) }

func (f *fixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(f.path(name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) appendTo(name, content string) {
	f.t.Helper()
	fh, err := os.OpenFile(f.path(name), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(content); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) tracker() *Tracker {
	tr := New(f.opts)
	f.t.Cleanup(tr.Close)
	return tr
}

func (f *fixture) refresh(tr *Tracker) {
	f.t.Helper()
	if _, err := tr.Refresh(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// total 是存下的全部用量之和。
func (f *fixture) total() int64 {
	f.t.Helper()
	rows, err := f.store.UsageTotals(context.Background(), []time.Time{day0.AddDate(-1, 0, 0), day0.AddDate(1, 0, 0)})
	if err != nil {
		f.t.Fatal(err)
	}
	var n int64
	for _, r := range rows {
		n += r.Tokens.Total()
	}
	return n
}

func (f *fixture) source(tr *Tracker) SourceInfo {
	f.t.Helper()
	s, err := tr.Summary(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return s.Sources[0]
}

// U12：追加的內容刷新後才算上；沒有變化的檔案不重讀。
func TestRefreshReadsAppendedLines(t *testing.T) {
	f := newFixture(t)
	f.write("a.jsonl", ln("k1", day0, 10, 1))
	tr := f.tracker()
	f.refresh(tr)
	if got := f.total(); got != 11 {
		t.Fatalf("total %d", got)
	}
	f.refresh(tr)
	if got := len(f.src.opened()); got != 1 {
		t.Fatalf("unchanged file reread: %v", f.src.opened())
	}
	f.appendTo("a.jsonl", ln("k2", day0, 20, 2))
	f.refresh(tr)
	if got := f.total(); got != 33 {
		t.Fatalf("total %d", got)
	}
	// 接著讀：沿用存下的解析狀態（讀過 1 行），不從頭讀。
	if got := f.src.opened(); got[len(got)-1] != "a.jsonl:1" {
		t.Fatalf("opens %v", got)
	}
}

// 最後一行還沒寫完時停在它前面，寫完後再讀。
func TestIncompleteLastLineWaits(t *testing.T) {
	f := newFixture(t)
	full := ln("k2", day0, 20, 2)
	f.write("a.jsonl", ln("k1", day0, 10, 1)+full[:10])
	tr := f.tracker()
	f.refresh(tr)
	if got := f.total(); got != 11 {
		t.Fatalf("total %d", got)
	}
	f.appendTo("a.jsonl", full[10:])
	f.refresh(tr)
	if got, info := f.total(), f.source(tr); got != 33 || info.Bad != 0 {
		t.Fatalf("total %d bad %d", got, info.Bad)
	}
}

// 開頭一段變了（換成了別的檔案）：從頭重讀；同一 Key 合成一條，不多算。
func TestReplacedFileRereadsWithoutDoubleCounting(t *testing.T) {
	f := newFixture(t)
	f.write("a.jsonl", ln("k1", day0, 10, 1)+ln("k2", day0, 20, 2))
	tr := f.tracker()
	f.refresh(tr)
	f.write("a.jsonl", ln("k0", day0, 5, 0)+ln("k1", day0, 10, 1)+ln("k2", day0, 20, 2))
	f.refresh(tr)
	if got := f.total(); got != 38 {
		t.Fatalf("total %d", got)
	}
	if got := f.src.opened(); got[len(got)-1] != "a.jsonl:" {
		t.Fatalf("not reread from the start: %v", got)
	}
}

// U15：檔案被刪掉後讀取位置刪掉，用量保留；U13：沒讀懂的行計數，其餘照常。
func TestDeletedFileKeepsUsageAndBadLinesCount(t *testing.T) {
	f := newFixture(t)
	f.write("a.jsonl", ln("k1", day0, 10, 1)+"not json\n"+`{"k":"k2","bad":true}`+"\n"+ln("k3", day0, 1, 1))
	tr := f.tracker()
	f.refresh(tr)
	if got, info := f.total(), f.source(tr); got != 13 || info.Bad != 2 || info.Files != 1 {
		t.Fatalf("total %d info %+v", got, info)
	}
	if err := os.Remove(f.path("a.jsonl")); err != nil {
		t.Fatal(err)
	}
	f.refresh(tr)
	files, _ := f.store.UsageFiles(context.Background())
	if got, info := f.total(), f.source(tr); got != 13 || len(files) != 0 || info.Bad != 0 || info.Files != 0 {
		t.Fatalf("total %d files %v info %+v", got, files, info)
	}
}

// 讀到一半（崩潰）時存下的位置：下一次從那裡接著讀，不從頭。
func TestResumesFromStoredPosition(t *testing.T) {
	f := newFixture(t)
	first := ln("k1", day0, 10, 1)
	f.write("a.jsonl", first+ln("k2", day0, 20, 2))
	head, err := fingerprint(f.path("a.jsonl"), int64(len(first)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.store.ApplyUsage(ctx, Batch{Records: []Record{{Agent: "test", Key: "k1", At: day0, Tokens: Tokens{Input: 10, Output: 1}}},
		File: &FileState{Path: f.path("a.jsonl"), Agent: "test", ReadTo: int64(len(first)), Head: head, State: []byte("1")}}); err != nil {
		t.Fatal(err)
	}
	f.opts.ChunkRecords = 1
	tr := f.tracker()
	f.refresh(tr)
	if got := f.src.opened(); len(got) != 1 || got[0] != "a.jsonl:1" {
		t.Fatalf("opens %v", got)
	}
	if got := f.total(); got != 33 {
		t.Fatalf("total %d", got)
	}
}

func zst(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// U43、U44：壓縮的檔案照讀，最後一行沒有換行也算；變了從頭解壓，同一 Key 合成一條。
func TestCompressedFiles(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.path("a.jsonl.zst"), zst(t, ln("k1", day0, 10, 1)+strings.TrimSuffix(ln("k2", day0, 20, 2), "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := f.tracker()
	f.refresh(tr)
	if got := f.total(); got != 33 {
		t.Fatalf("total %d", got)
	}
	if err := os.WriteFile(f.path("a.jsonl.zst"), zst(t, ln("k1", day0, 10, 1)+ln("k2", day0, 20, 2)+ln("k3", day0, 3, 0)), 0o644); err != nil {
		t.Fatal(err)
	}
	f.refresh(tr)
	if got := f.total(); got != 36 {
		t.Fatalf("total %d", got)
	}
}

// 整份改寫的檔案：變了就整份重讀，之前的不重複算（U33）。
func TestWholeFileRereads(t *testing.T) {
	f := newFixture(t)
	one := testLine{K: "k1", At: day0, I: 10, O: 1}
	two := testLine{K: "k2", At: day0, I: 20, O: 2}
	b1, _ := json.Marshal([]testLine{one})
	f.write("a.json", string(b1))
	tr := f.tracker()
	f.refresh(tr)
	b2, _ := json.Marshal([]testLine{one, two})
	f.write("a.json", string(b2))
	f.refresh(tr)
	if got := f.total(); got != 33 {
		t.Fatalf("total %d", got)
	}
}

// U45：資料庫被鎖住時這一輪跳過、不報錯；放開後下一輪讀到。
func TestBusyDatabaseIsSkipped(t *testing.T) {
	f := newFixture(t)
	f.write("a.db", ln("k1", day0, 10, 1))
	f.write("a.db.lock", "")
	tr := f.tracker()
	f.refresh(tr)
	if got, info := f.total(), f.source(tr); got != 0 || info.Problem != nil {
		t.Fatalf("total %d problem %+v", got, info.Problem)
	}
	if err := os.Remove(f.path("a.db.lock")); err != nil {
		t.Fatal(err)
	}
	f.refresh(tr)
	if got := f.total(); got != 11 {
		t.Fatalf("total %d", got)
	}
}

// U14：位置讀不了時記錄來源有原因與路徑，其他 agent 照常。
func TestUnreadableLocation(t *testing.T) {
	f := newFixture(t)
	other := &testSource{agent: "other"}
	f.opts.Sources = append(f.opts.Sources, other)
	if err := os.MkdirAll(filepath.Join(f.home, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "other", "b.jsonl"), []byte(ln("k1", day0, 10, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := f.path("locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	tr := f.tracker()
	f.refresh(tr)
	s, err := tr.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := s.Sources[0].Problem
	if p == nil || p.Code != string(CodeReadFailed) || p.Params["path"] != locked || p.Params["agent"] != "test" {
		t.Fatalf("problem %+v", p)
	}
	if s.Sources[1].Problem != nil || f.total() != 11 {
		t.Fatalf("other agent: %+v total %d", s.Sources[1], f.total())
	}
}

// 額度：每個桶只留最新的一份。
func TestQuotasKeepTheLatest(t *testing.T) {
	f := newFixture(t)
	q := func(at time.Time, used int64) string {
		b, _ := json.Marshal(testLine{K: "quota", S: "b1", At: at, I: used})
		return string(b) + "\n"
	}
	f.write("a.jsonl", q(day0, 41)+q(day0.Add(-time.Hour), 30))
	tr := f.tracker()
	f.refresh(tr)
	s, err := tr.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Quotas) != 1 || s.Quotas[0].Agent != "test" || s.Quotas[0].Windows[0].UsedPercent != 41 {
		t.Fatalf("quotas %+v", s.Quotas)
	}
}

// U19：目錄解開符號連結合併；已不存在的目錄按原樣。
func TestDirectoriesResolveSymlinks(t *testing.T) {
	f := newFixture(t)
	real := filepath.Join(f.home, "repo")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.home, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realPath, _ := filepath.EvalSymlinks(real)
	line := func(k, dir string) string {
		b, _ := json.Marshal(testLine{K: k, At: day0, D: dir, I: 1})
		return string(b) + "\n"
	}
	f.write("a.jsonl", line("k1", real)+line("k2", link)+line("k3", "/gone/dir"))
	tr := f.tracker()
	f.refresh(tr)
	s, _ := tr.Summary(context.Background())
	dirs := map[string]int64{}
	for _, d := range s.Periods[0].Dirs {
		dirs[d.Name] = d.Tokens.Total()
	}
	if dirs[realPath] != 2 || dirs["/gone/dir"] != 1 || len(dirs) != 2 {
		t.Fatalf("dirs %v", dirs)
	}
}

// U08：今天、7 天、30 天按本機時區的日曆日；U09：換時區後按新時區重新分日。
func TestSummaryDays(t *testing.T) {
	f := newFixture(t)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 20, 0, 0, 0, shanghai)
	at := func(days int, hour, minute int) time.Time {
		return time.Date(2026, 11, 2-days, hour, minute, 0, 0, shanghai)
	}
	var content string
	for i, a := range []time.Time{at(0, 0, 10), at(1, 23, 50), at(6, 12, 0), at(7, 12, 0), at(29, 12, 0), at(30, 12, 0)} {
		content += ln("k"+string(rune('a'+i)), a, 1, 0)
	}
	f.write("a.jsonl", content)
	zone := shanghai
	f.opts.Now = func() time.Time { return now }
	f.opts.Zone = func() *time.Location { return zone }
	tr := f.tracker()
	f.refresh(tr)
	counts := func() []int64 {
		s, err := tr.Summary(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, p := range s.Periods {
			out = append(out, p.Total.Total())
		}
		if s.Days[len(s.Days)-1].Date != now.In(zone).Format(time.DateOnly) {
			t.Fatalf("last day %s", s.Days[len(s.Days)-1].Date)
		}
		return out
	}
	if got := counts(); !slices.Equal(got, []int64{1, 3, 5}) {
		t.Fatalf("periods %v", got)
	}
	// 往東調 3 小時：昨天 23:50 成了今天 02:50。
	zone = time.FixedZone("UTC+11", 11*3600)
	if got := counts(); got[0] != 2 {
		t.Fatalf("after zone change %v", got)
	}
}

// 讀的過程中來的刷新合成下一輪；Refresh 讀到呼叫之前的內容才返回（U12）。
func TestRefreshesCoalesce(t *testing.T) {
	f := newFixture(t)
	f.write("a.jsonl", ln("k1", day0, 1, 0))
	gate := make(chan struct{})
	f.src.gate, f.src.entered = gate, make(chan struct{}, 2)
	tr := f.tracker()
	tr.Start()
	<-f.src.entered // 第一輪已開始，卡在 Find
	errs := make(chan error, 2)
	for range 2 {
		go func() { _, err := tr.Refresh(context.Background()); errs <- err }()
	}
	// 等兩個刷新都登記了：要求的輪數到 3（Start 一輪、刷新兩輪）。
	for {
		tr.mu.Lock()
		want := tr.want
		changed := tr.changed
		tr.mu.Unlock()
		if want == 3 {
			break
		}
		<-changed
	}
	gate <- struct{}{} // 第一輪
	<-f.src.entered
	gate <- struct{}{} // 兩個刷新合成的第二輪
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	f.src.mu.Lock()
	finds := f.src.finds
	f.src.mu.Unlock()
	if finds != 2 {
		t.Fatalf("rounds %d, want 2", finds)
	}
}

// 排程：Start 讀一輪，之後每次讀完 24 小時再讀；醒來後按牆鐘重排（U16）。
func TestSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.write("a.jsonl", ln("k1", day0, 1, 0))
		wall := day0
		var mu sync.Mutex
		var resume func()
		f.opts.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return wall }
		f.opts.OnResume = func(fn func()) func() { resume = fn; return func() {} }
		tr := New(f.opts)
		defer tr.Close()
		finds := func() int { f.src.mu.Lock(); defer f.src.mu.Unlock(); return f.src.finds }
		tr.Start()
		synctest.Wait()
		if finds() != 1 {
			t.Fatalf("rounds after start %d", finds())
		}
		time.Sleep(23 * time.Hour)
		synctest.Wait()
		if finds() != 1 {
			t.Fatalf("read before a day passed")
		}
		// 電腦睡了一天：計時器停走，牆鐘往前；醒來後到期的立刻讀。
		mu.Lock()
		wall = wall.Add(30 * time.Hour)
		mu.Unlock()
		resume()
		synctest.Wait()
		if finds() != 2 {
			t.Fatalf("rounds after resume %d", finds())
		}
	})
}

// 一輪沒讀成（拿不到環境快照）：上一輪讀完的時間不變，一小時後再試。
func TestFailedRoundRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.write("a.jsonl", ln("k1", day0, 1, 0))
		home := f.home
		var mu sync.Mutex
		calls := 0
		f.opts.Env = func(context.Context) (proc.Env, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return nil, errors.New("no snapshot yet")
			}
			return proc.Env{"HOME=" + home}, nil
		}
		tr := New(f.opts)
		defer tr.Close()
		tr.Start()
		synctest.Wait()
		if s := tr.State(); !s.ReadAt.IsZero() || s.Reading || f.total() != 0 {
			t.Fatalf("after failed round: %+v total %d", s, f.total())
		}
		time.Sleep(59 * time.Minute)
		synctest.Wait()
		if f.total() != 0 {
			t.Fatal("retried before an hour")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if s := tr.State(); s.ReadAt.IsZero() || f.total() != 1 {
			t.Fatalf("after retry: %+v total %d", s, f.total())
		}
	})
}

// Close 之後 Refresh 回 usage.closed，Watch 回 nil。
func TestClose(t *testing.T) {
	f := newFixture(t)
	tr := f.tracker()
	tr.Close()
	if _, err := tr.Refresh(context.Background()); !errors.Is(err, errClosed) {
		t.Fatalf("refresh %v", err)
	}
	if err := tr.Watch(context.Background(), func(ReadState) error { return nil }); err != nil {
		t.Fatalf("watch %v", err)
	}
}

// TestCodes：每個代碼都有說明、帶 usage. 前綴、參數就是說明裡的佔位符。
func TestCodes(t *testing.T) {
	for _, c := range Codes() {
		if !strings.HasPrefix(string(c), "usage.") || codeText[c] == "" {
			t.Fatalf("code %q", c)
		}
	}
	if got := CodeReadFailed.Params(); !slices.Equal(got, []string{"agent", "path", "detail"}) {
		t.Fatalf("params %v", got)
	}
}

// U72、U75、U84、U85：每個 agent 帶經 OrielHub 的部分；經 OrielHub 的按 Task 拆分，
// Task 列出用到的 agent（多到少），按總計多到少。
func TestSummaryViaOrielHub(t *testing.T) {
	f := newFixture(t)
	line := func(k, session string, in int64) string {
		b, _ := json.Marshal(testLine{K: k, At: day0, S: session, I: in})
		return string(b) + "\n"
	}
	f.write("a.jsonl", line("k1", "g1", 600)+line("k2", "g1", 400)+line("k3", "g2", 500))
	f.store.LinkSession(SessionLink{Agent: "test", Native: "g1", Task: "t-a"})
	f.store.LinkSession(SessionLink{Agent: "other", Native: "c1", Task: "t-a"})
	f.store.LinkSession(SessionLink{Agent: "other", Native: "c2", Task: "t-b"})
	f.store.AddReported(Reported{Task: "t-a", Seq: 7, Agent: "other", Native: "c1", At: day0, Tokens: Tokens{Input: 100}})
	f.store.AddReported(Reported{Task: "t-b", Seq: 3, Agent: "other", Native: "c2", At: day0, Tokens: Tokens{Input: 2000}})
	tr := f.tracker()
	f.refresh(tr)
	s, err := tr.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := s.Periods[0]
	agents := map[task.AgentID][2]int64{}
	for _, a := range p.Agents {
		agents[a.Agent] = [2]int64{a.Tokens.Total(), a.Hub.Total()}
	}
	if agents["test"] != [2]int64{1500, 1000} || agents["other"] != [2]int64{2100, 2100} {
		t.Fatalf("agents %v", agents)
	}
	want := []TaskShare{
		{Task: "t-b", Agents: []task.AgentID{"other"}, Tokens: Tokens{Input: 2000}},
		{Task: "t-a", Agents: []task.AgentID{"test", "other"}, Tokens: Tokens{Input: 1100}},
	}
	if !reflect.DeepEqual(p.Tasks, want) {
		t.Fatalf("tasks %+v", p.Tasks)
	}
	if day := s.Days[len(s.Days)-1]; len(day.Agents) != 2 {
		t.Fatalf("today %+v", day)
	}
}

// U16：Start 登記醒來通知的途中 Tracker 被關掉：取消登記時不持鎖，取消登記的函式可以再用 Tracker。
func TestStartClosedWhileRegisteringResume(t *testing.T) {
	f := newFixture(t)
	var tr *Tracker
	cancelled := false
	f.opts.OnResume = func(func()) func() {
		tr.Close()
		return func() {
			tr.State()
			cancelled = true
		}
	}
	tr = New(f.opts)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Start()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Start is waiting on its own lock")
	}
	if !cancelled {
		t.Fatal("the wake notification stayed registered")
	}
}

// State 回傳目前的讀取狀態。
func (t *Tracker) State() ReadState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}
