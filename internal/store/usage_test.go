package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// usageImpls 是 usage.Store 的兩種實現：一致性測試同時跑兩者。
func usageImpls(t *testing.T) map[string]usage.Store {
	return map[string]usage.Store{"mem": usage.NewMemStore(), "sqlite": openStore(t)}
}

var u0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func TestUsageRecordsMerge(t *testing.T) {
	for name, s := range usageImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			first := usage.Record{Agent: "claude", Key: "k1", At: u0.Add(time.Minute), Session: "s1", Model: "m",
				Tokens: usage.Tokens{Input: 10, CacheRead: 3000, CacheWrite: 200, Output: 1}}
			// 同一次回應的另一份：輸出更大、時間更早；先寫入的會話與模型保留，空的目錄補上。
			second := usage.Record{Agent: "claude", Key: "k1", At: u0, Session: "s2", Model: "other", Dir: "/work",
				Tokens: usage.Tokens{Input: 10, CacheRead: 3000, CacheWrite: 200, Output: 50}}
			// 時間不知道的一份不影響時間。
			third := usage.Record{Agent: "claude", Key: "k1", Tokens: usage.Tokens{Output: 2}}
			other := usage.Record{Agent: "codex", Key: "k1", At: u0, Model: "m", Tokens: usage.Tokens{Input: 5}}
			for _, r := range []usage.Record{first, second, third, other} {
				if err := s.ApplyUsage(ctx, usage.Batch{Records: []usage.Record{r}}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.UsageTotals(ctx, []time.Time{u0.Add(-time.Hour), u0.Add(time.Hour), u0.Add(2 * time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			want := []usage.Total{
				{Day: 0, Agent: "claude", Model: "m", Dir: "/work", Tokens: usage.Tokens{Input: 10, CacheRead: 3000, CacheWrite: 200, Output: 50}},
				{Day: 0, Agent: "codex", Model: "m", Tokens: usage.Tokens{Input: 5}},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("totals\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestUsageTotalsGroupsByDay(t *testing.T) {
	for name, s := range usageImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			recs := []usage.Record{
				{Agent: "a", Key: "1", At: u0, Model: "m2", Tokens: usage.Tokens{Input: 1}},
				{Agent: "a", Key: "2", At: u0.Add(time.Hour), Model: "m1", Tokens: usage.Tokens{Input: 2}},
				{Agent: "a", Key: "3", At: u0.Add(25 * time.Hour), Model: "m1", Tokens: usage.Tokens{Output: 4}},
				{Agent: "a", Key: "4", At: u0.Add(-25 * time.Hour), Model: "m1", Tokens: usage.Tokens{Output: 8}}, // 範圍外
				{Agent: "a", Key: "5", At: u0.Add(48 * time.Hour), Model: "m1", Tokens: usage.Tokens{Output: 16}}, // 上界不含
			}
			if err := s.ApplyUsage(ctx, usage.Batch{Records: recs}); err != nil {
				t.Fatal(err)
			}
			day := u0.Truncate(24 * time.Hour)
			got, err := s.UsageTotals(ctx, []time.Time{day, day.Add(24 * time.Hour), day.Add(48 * time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			want := []usage.Total{
				{Day: 0, Agent: "a", Model: "m1", Tokens: usage.Tokens{Input: 2}},
				{Day: 0, Agent: "a", Model: "m2", Tokens: usage.Tokens{Input: 1}},
				{Day: 1, Agent: "a", Model: "m1", Tokens: usage.Tokens{Output: 4}},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("totals\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestUsageQuotasKeepTheLatest(t *testing.T) {
	for name, s := range usageImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			newer := usage.Quota{Agent: "codex", Bucket: "codex", Plan: "pro", At: u0,
				Windows: []usage.Limit{{Minutes: 300, UsedPercent: 41, ResetsAt: u0.Add(2 * time.Hour)}, {Minutes: 10080, UsedPercent: 12}}}
			older := usage.Quota{Agent: "codex", Bucket: "codex", Plan: "plus", At: u0.Add(-time.Hour),
				Windows: []usage.Limit{{Minutes: 300, UsedPercent: 99}}}
			other := usage.Quota{Agent: "codex", Bucket: "a_other", Name: "Other", At: u0, Windows: []usage.Limit{{Minutes: 60, UsedPercent: 5}}}
			for _, b := range []usage.Batch{{Quotas: []usage.Quota{newer}}, {Quotas: []usage.Quota{older, other}}} {
				if err := s.ApplyUsage(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.UsageQuotas(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, []usage.Quota{other, newer}) {
				t.Fatalf("quotas\n got %+v\nwant %+v", got, []usage.Quota{other, newer})
			}
		})
	}
}

func TestUsageFiles(t *testing.T) {
	for name, s := range usageImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a := usage.FileState{Path: "/r/a.jsonl", Agent: "claude", Size: 10, ModTime: u0, Head: "h", ReadTo: 9,
				State: []byte(`{"n":1}`), Bad: 2, Done: true}
			b := usage.FileState{Path: "/r/b.jsonl", Agent: "codex", State: []byte{}}
			for _, f := range []usage.FileState{b, a} {
				if err := s.ApplyUsage(ctx, usage.Batch{File: &f}); err != nil {
					t.Fatal(err)
				}
			}
			a.ReadTo = 10
			if err := s.ApplyUsage(ctx, usage.Batch{File: &a}); err != nil {
				t.Fatal(err)
			}
			got, err := s.UsageFiles(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, []usage.FileState{a, b}) {
				t.Fatalf("files\n got %+v\nwant %+v", got, []usage.FileState{a, b})
			}
			if err := s.DeleteUsageFiles(ctx, []string{a.Path}); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.UsageFiles(ctx); len(got) != 1 || got[0].Path != b.Path {
				t.Fatalf("after delete %+v", got)
			}
		})
	}
}

// QueryFile：唯讀查詢外部 SQLite 檔；寫入被拒，不存在的檔案是錯誤（不是 ErrBusy）。
func TestQueryFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ext.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSettings(ctx, hub.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	rows, err := QueryFile(ctx, path, "SELECT count(*) FROM settings WHERE key = ?", "hub")
	if err != nil || len(rows) != 1 || rows[0][0] != int64(1) {
		t.Fatalf("rows %v err %v", rows, err)
	}
	if _, err := QueryFile(ctx, path, "DELETE FROM settings"); err == nil || errors.Is(err, usage.ErrBusy) {
		t.Fatalf("write through a read-only connection: %v", err)
	}
	if _, err := QueryFile(ctx, filepath.Join(t.TempDir(), "missing.sqlite"), "SELECT 1"); err == nil || errors.Is(err, usage.ErrBusy) {
		t.Fatalf("missing file: %v", err)
	}
}

// usageWriter 把 hub 寫下的會話歸屬與上報放進一種實現：SQLite 經 Append（與 hub 一樣隨事件寫入），
// MemStore 經它的 LinkSession、AddReported。
type usageWriter struct {
	s    usage.Store
	seqs map[task.TaskID]task.Seq
}

func (w *usageWriter) write(t *testing.T, id task.TaskID, v hub.Views) {
	t.Helper()
	switch s := w.s.(type) {
	case *usage.MemStore:
		for _, l := range v.Links {
			s.LinkSession(usage.SessionLink{Agent: l.Agent, Native: string(l.Native), Task: l.Task})
		}
		for _, r := range v.Reported {
			s.AddReported(usage.Reported{Task: r.Task, Seq: int64(r.Seq), Agent: r.Agent, Native: string(r.Native),
				At: r.At, Model: r.Model, Dir: r.Dir, Tokens: usage.Tokens{Input: r.Usage.InputTokens,
					CacheRead: r.Usage.CacheReadTokens, CacheWrite: r.Usage.CacheWriteTokens, Output: r.Usage.OutputTokens}})
		}
	case *Store:
		next := w.seqs[id] + 1
		b := hub.Batch{Records: []hub.Record{{Seq: next, Kind: task.MessageAppendedKind, At: u0, Payload: []byte("x")}}, Views: v}
		if err := s.Append(context.Background(), id, w.seqs[id], b); err != nil {
			t.Fatal(err)
		}
		w.seqs[id] = next
	}
}

// remove 刪掉一個 Task 的事件與投影（DL12）；MemStore 沒有 Task，不用刪。
func (w *usageWriter) remove(t *testing.T, id task.TaskID) {
	t.Helper()
	if s, ok := w.s.(*Store); ok {
		if err := s.Delete(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}

// U72–U82（m2.md 第 33 節）：本機記錄的會話或父會話是 OrielHub 跑的就歸到那個 Task；上報只在那個
// 原生會話沒有任何本機記錄時算；刪除 Task 不影響。U83：兩種實現跑同一套斷言。
func TestUsageAttribution(t *testing.T) {
	bounds := []time.Time{u0.Add(-time.Hour), u0.Add(time.Hour)}
	reported := func(id task.TaskID, seq task.Seq, agent task.AgentID, native string, in, out int64) hub.ReportedUsage {
		return hub.ReportedUsage{Task: id, Seq: seq, Agent: agent, Native: task.NativeSessionID(native), At: u0,
			Model: "m", Dir: "/ws", Usage: task.Usage{InputTokens: in, OutputTokens: out}}
	}
	link := func(agent task.AgentID, native string, id task.TaskID) hub.SessionLink {
		return hub.SessionLink{Agent: agent, Native: task.NativeSessionID(native), Task: id, Session: "s1"}
	}
	rec := func(agent task.AgentID, key, session, parent string, in int64) usage.Record {
		return usage.Record{Agent: agent, Key: key, At: u0, Session: session, Parent: parent, Model: "m", Dir: "/ws",
			Tokens: usage.Tokens{Input: in}}
	}
	for name, s := range usageImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			w := &usageWriter{s: s, seqs: map[task.TaskID]task.Seq{}}
			w.write(t, "t-a", hub.Views{Links: []hub.SessionLink{link("grok", "g1", "t-a")},
				Reported: []hub.ReportedUsage{reported("t-a", 5, "grok", "g1", 800, 0)}}) // U76：g1 有本機記錄，上報不算
			w.write(t, "t-b", hub.Views{Links: []hub.SessionLink{link("opencode", "o1", "t-b")}})
			w.write(t, "t-c", hub.Views{Links: []hub.SessionLink{link("cursor", "c1", "t-c")},
				Reported: []hub.ReportedUsage{reported("t-c", 7, "cursor", "c1", 100, 50), reported("t-c", 9, "cursor", "c1", 200, 20)}})
			// 同一歸屬再寫一次（重建、再次綁定）：照舊，不重複。
			w.write(t, "t-a", hub.Views{Links: []hub.SessionLink{link("grok", "g1", "t-a")},
				Reported: []hub.ReportedUsage{reported("t-a", 5, "grok", "g1", 800, 0)}})
			records := []usage.Record{
				rec("grok", "1", "g1", "", 600), rec("grok", "2", "g1", "", 400), // U72：經 OrielHub
				rec("grok", "3", "g2", "", 500),      // U72：直接使用
				rec("opencode", "4", "o2", "o1", 70), // U74：子會話看父會話
				rec("codex", "5", "g1", "", 9),       // U82：同一個 ID 在別的 agent 下不串
			}
			if err := s.ApplyUsage(ctx, usage.Batch{Records: records}); err != nil {
				t.Fatal(err)
			}
			want := []usage.Total{
				{Agent: "codex", Model: "m", Dir: "/ws", Tokens: usage.Tokens{Input: 9}},
				{Agent: "cursor", Model: "m", Dir: "/ws", Task: "t-c", Tokens: usage.Tokens{Input: 300, Output: 70}}, // U75
				{Agent: "grok", Model: "m", Dir: "/ws", Tokens: usage.Tokens{Input: 500}},
				{Agent: "grok", Model: "m", Dir: "/ws", Task: "t-a", Tokens: usage.Tokens{Input: 1000}},
				{Agent: "opencode", Model: "m", Dir: "/ws", Task: "t-b", Tokens: usage.Tokens{Input: 70}},
			}
			totals := func() []usage.Total {
				t.Helper()
				got, err := s.UsageTotals(ctx, bounds)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			if got := totals(); !reflect.DeepEqual(got, want) {
				t.Fatalf("totals\n got %+v\nwant %+v", got, want)
			}
			// U77：上報先到、本機記錄還沒寫時按上報算；記錄寫下後換成記錄，不重複。
			w.write(t, "t-d", hub.Views{Links: []hub.SessionLink{link("grok", "g3", "t-d")},
				Reported: []hub.ReportedUsage{reported("t-d", 3, "grok", "g3", 800, 0)}})
			if got := totals(); !slices.Contains(got, usage.Total{Agent: "grok", Model: "m", Dir: "/ws", Task: "t-d", Tokens: usage.Tokens{Input: 800}}) {
				t.Fatalf("reported before the record: %+v", got)
			}
			if err := s.ApplyUsage(ctx, usage.Batch{Records: []usage.Record{rec("grok", "6", "g3", "", 1000)}}); err != nil {
				t.Fatal(err)
			}
			if got := totals(); !slices.Contains(got, usage.Total{Agent: "grok", Model: "m", Dir: "/ws", Task: "t-d", Tokens: usage.Tokens{Input: 1000}}) {
				t.Fatalf("record replaces the report: %+v", got)
			}
			// U79：刪除 Task 之後，它的用量照舊歸到它的 ID。
			before := totals()
			w.remove(t, "t-a")
			w.remove(t, "t-c")
			if got := totals(); !reflect.DeepEqual(got, before) {
				t.Fatalf("after delete\n got %+v\nwant %+v", got, before)
			}
		})
	}
}
