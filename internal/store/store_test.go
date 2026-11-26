package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/egoist/mygo/plugins/sqlite"
)

// conformance 是兩種實現共用的介面。
type conformance interface {
	hub.Log
	hub.Config
	hub.Index
	ViewsOf(ctx context.Context, id task.TaskID) (hub.Views, error)
	TaskViews(ctx context.Context) ([]hub.TaskView, error)
}

func impls(t *testing.T) map[string]func(*testing.T) conformance {
	return map[string]func(*testing.T) conformance{
		"mem":    func(*testing.T) conformance { return hub.NewMemStore() },
		"sqlite": sqliteStore,
	}
}

func sqliteStore(t *testing.T) conformance {
	return openStore(t)
}

// openStore 回傳 *Store 給只測 SQLite 的情境。
func openStore(t *testing.T) *Store {
	s, err := Open(context.Background(),
		filepath.Join(t.TempDir(), "oriel.sqlite"), Options{})
	if err != nil {
		t.Fatal(err) // 原生庫是硬依賴：開不起來就失敗
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var at0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func rec(n task.Seq, text string) hub.Record {
	return hub.Record{Seq: n, Kind: task.MessageAppendedKind,
		At: at0.Add(time.Duration(n) * time.Second), Payload: []byte(text)}
}

func batch(recs ...hub.Record) hub.Batch { return hub.Batch{Records: recs} }

func storeError(t *testing.T, err error, code Code) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// --- Open / migrate ---

func TestOpenFresh(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("no db file")
	}
	ro, err := sqlite.Open(ctx, path, sqlite.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	v, _ := ro.Query(ctx, "PRAGMA user_version")
	if toInt(v.Values[0][0]) != int64(len(migrations)) {
		t.Fatalf("user_version %v", v.Values)
	}
	j, _ := ro.Query(ctx, "PRAGMA journal_mode")
	if j.Values[0][0] != "wal" {
		t.Fatalf("journal %v", j.Values)
	}
}

func TestReopenRetains(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, _ := Open(ctx, path, Options{})
	s.Append(ctx, "t-1", 0, batch(rec(1, "a")))
	s.Close()
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _ := s.Read(ctx, "t-1", 0, 0)
	if len(got) != 1 || string(got[0].Payload) != "a" {
		t.Fatalf("records %v", got)
	}
}

func TestNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, _ := Open(ctx, path, Options{})
	s.Close()
	rw, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rw.Execute(ctx, "PRAGMA user_version=99")
	rw.Close()
	_, err = Open(ctx, path, Options{})
	var se *Error
	if !errors.As(err, &se) || se.Code != CodeNewerSchema || se.Params["version"] != "99" {
		t.Fatalf("got %v", err)
	}
}

// S2：v1 的資料庫升到 v2：Task 列多出 failed（0），其餘欄位不變；
// 投影版本是 0（hub 據此重建）。
func TestMigrateFromV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	rw, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stmts := []sqlite.Statement{}
	for _, q := range migrations[0] {
		stmts = append(stmts, sqlite.Statement{SQL: q})
	}
	stmts = append(stmts,
		sqlite.Statement{SQL: "INSERT INTO tasks_view(id, title, workspace, status, archived, sessions, running, pending_asks, updated_at) VALUES('t-1','old','w-1','todo',0,2,1,3,?)",
			Args: []any{formatAt(at0)}},
		sqlite.Statement{SQL: "PRAGMA user_version = 1"})
	if _, err := rw.Transaction(ctx, stmts); err != nil {
		t.Fatal(err)
	}
	rw.Close()

	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, _ := s.r.Query(ctx, "PRAGMA user_version")
	if toInt(v.Values[0][0]) != int64(len(migrations)) {
		t.Fatalf("user_version %v", v.Values)
	}
	rows, err := s.TaskViews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := hub.TaskView{ID: "t-1", Title: "old", Workspace: "w-1", Status: task.TaskTodo,
		Sessions: 2, Running: 1, PendingAsks: 3, UpdatedAt: at0,
		Priority: task.PriorityNone, Labels: []task.LabelID{}, Agents: []hub.TaskAgent{}}
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("rows %+v", rows)
	}
	if ver, err := s.ViewsVersion(ctx); err != nil || ver != 0 {
		t.Fatalf("views version %d %v", ver, err)
	}
}

func TestOpenMissingParent(t *testing.T) {
	_, err := Open(context.Background(),
		filepath.Join(t.TempDir(), "nope", "x.sqlite"), Options{})
	storeError(t, err, CodeOpenFailed)
}

func TestClosedStore(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, filepath.Join(t.TempDir(), "x.sqlite"), Options{})
	s.Close()
	if err := s.Close(); err != nil {
		t.Fatal("double close")
	}
	checks := []error{
		s.Append(ctx, "t-1", 0, batch(rec(1, "a"))),
		s.Delete(ctx, "t-1"),
		s.PutWorkspace(ctx, hub.Workspace{ID: "w-1"}),
		s.DeleteRule(ctx, "r1"),
		s.PutSettings(ctx, hub.Settings{}),
		s.PutLabel(ctx, hub.Label{ID: "l-1"}),
		s.DeleteLabel(ctx, "l-1"),
		s.PutDevice(ctx, hub.Device{ID: "d-1"}),
		s.DeleteDevice(ctx, "d-1"),
		s.ClearDevices(ctx),
		s.SetViewsVersion(ctx, 1),
		s.Reproject(ctx, "t-1", hub.Views{}),
	}
	_, e0 := s.ViewsVersion(ctx)
	checks = append(checks, e0)
	_, e := s.Read(ctx, "t-1", 0, 0)
	checks = append(checks, e)
	_, e = s.Streams(ctx)
	checks = append(checks, e)
	_, e = s.Devices(ctx)
	checks = append(checks, e)
	_, e = s.Workspaces(ctx)
	checks = append(checks, e)
	_, e = s.Rules(ctx)
	checks = append(checks, e)
	_, e = s.Settings(ctx)
	checks = append(checks, e)
	_, e = s.ViewsOf(ctx, "t-1")
	checks = append(checks, e)
	_, e = s.TaskViews(ctx)
	checks = append(checks, e)
	for i, err := range checks {
		var se *Error
		if !errors.As(err, &se) || se.Code != CodeClosed {
			t.Fatalf("check %d: got %v", i, err)
		}
	}
}

// --- Log conformance ---

func TestAppendRead(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			r1 := rec(1, "a")
			r2 := rec(2, "b")
			r3 := rec(3, "nul")
			r3.Payload = []byte{'x', 0, 'y'}
			if err := s.Append(ctx, "t-1", 0, batch(r1, r2, r3)); err != nil {
				t.Fatal(err)
			}
			got, _ := s.Read(ctx, "t-1", 0, 0)
			if len(got) != 3 {
				t.Fatalf("got %d", len(got))
			}
			for i, want := range []hub.Record{r1, r2, r3} {
				if got[i].Seq != want.Seq || got[i].Kind != want.Kind ||
					!got[i].At.Equal(want.At) || string(got[i].Payload) != string(want.Payload) {
					t.Fatalf("rec %d: %+v vs %+v", i, got[i], want)
				}
			}
			got, _ = s.Read(ctx, "t-1", 1, 1)
			if len(got) != 1 || got[0].Seq != 2 {
				t.Fatalf("paged %v", got)
			}
			got, _ = s.Read(ctx, "t-1", 3, 0)
			if len(got) != 0 {
				t.Fatalf("tail %v", got)
			}
		})
	}
}

func TestAppendContinues(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			s.Append(ctx, "t-1", 0, batch(rec(1, "a")))
			if err := s.Append(ctx, "t-1", 1, batch(rec(2, "b"))); err != nil {
				t.Fatal(err)
			}
			s.Append(ctx, "t-2", 0, batch(rec(1, "x")))
			if err := s.Append(ctx, "t-2", 1, batch(rec(2, "y"))); err != nil {
				t.Fatal(err)
			}
			ids, _ := s.Streams(ctx)
			if len(ids) != 2 {
				t.Fatalf("streams %v", ids)
			}
			got, _ := s.Read(ctx, "t-2", 0, 0)
			if len(got) != 2 || got[1].Seq != 2 {
				t.Fatalf("t-2 %v", got)
			}
		})
	}
}

func TestAppendConflict(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			s.Append(ctx, "t-1", 0, batch(rec(1, "a"), rec(2, "b"), rec(3, "c")))
			bad := batch(rec(4, "d"))
			bad.NewWorkspace = &hub.Workspace{ID: "w-x", Path: "/x"}
			bad.Views.Task = &hub.TaskView{ID: "t-1", Title: "bad"}
			// behind：expected 1 但下一條已是 4 → seq 2,3 撞上 PK/trigger
			if err := s.Append(ctx, "t-1", 1, batch(rec(2, "z"))); err == nil {
				t.Fatal("behind accepted")
			}
			// ahead：expected 5 → 觸發器
			if err := s.Append(ctx, "t-1", 5, bad); err == nil {
				t.Fatal("ahead accepted")
			}
			// 批內斷號
			if err := s.Append(ctx, "t-1", 0,
				batch(rec(1, "a"), rec(2, "b"), rec(4, "d"))); err == nil {
				t.Fatal("gap accepted")
			}
			got, _ := s.Read(ctx, "t-1", 0, 0)
			if len(got) != 3 {
				t.Fatalf("records %v", got)
			}
			v, _ := s.ViewsOf(ctx, "t-1")
			if v.Task != nil {
				t.Fatalf("views %+v", v.Task)
			}
			ws, _ := s.Workspaces(ctx)
			if len(ws) != 0 {
				t.Fatalf("workspaces %v", ws)
			}
		})
	}
}

func TestAppendConflictCodes(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	s.Append(ctx, "t-1", 0, batch(rec(1, "a")))
	storeError(t, s.Append(ctx, "t-1", 0, batch(rec(1, "x"))), CodeConflict)
	storeError(t, s.Append(ctx, "t-1", 5, batch(rec(6, "x"))), CodeConflict)
	storeError(t, s.Append(ctx, "t-1", 1, batch(rec(1, "a"), rec(3, "x"))), CodeConflict)
}

func TestRedactAndUnsearch(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			b := batch(rec(1, "a"), rec(2, "b"), rec(3, "c"))
			b.Views.Search = []hub.SearchEntry{
				{Seq: 1, Text: "a"}, {Seq: 2, Text: "b"}, {Seq: 3, Text: "c"}}
			s.Append(ctx, "t-1", 0, b)
			b = batch(rec(4, "d"))
			b.Redact = []hub.Record{{Seq: 2, Kind: task.MessageAppendedKind,
				At: rec(2, "").At, Payload: []byte("redacted")}}
			b.Views.Unsearch = []task.Seq{2}
			if err := s.Append(ctx, "t-1", 3, b); err != nil {
				t.Fatal(err)
			}
			got, _ := s.Read(ctx, "t-1", 0, 0)
			if string(got[1].Payload) != "redacted" ||
				got[1].Kind != task.MessageAppendedKind || !got[1].At.Equal(rec(2, "").At) {
				t.Fatalf("redacted %+v", got[1])
			}
			v, _ := s.ViewsOf(ctx, "t-1")
			for _, e := range v.Search {
				if e.Seq == 2 {
					t.Fatal("seq 2 still searchable")
				}
			}
		})
	}
}

// 置頂的時間（D109）隨 Task 列存取；未置頂讀回零值。
func TestViewsPinnedAt(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			pinned := at0.Add(time.Minute)
			b := batch(rec(1, "a"))
			b.Views.Task = &hub.TaskView{ID: "t-1", UpdatedAt: at0, PinnedAt: pinned}
			if err := s.Append(ctx, "t-1", 0, b); err != nil {
				t.Fatal(err)
			}
			v, _ := s.ViewsOf(ctx, "t-1")
			rows, _ := s.TaskViews(ctx)
			if !v.Task.PinnedAt.Equal(pinned) || len(rows) != 1 || !rows[0].PinnedAt.Equal(pinned) {
				t.Fatalf("pinned: views %v, rows %+v", v.Task.PinnedAt, rows)
			}
			b = batch(rec(2, "b"))
			b.Views.Task = &hub.TaskView{ID: "t-1", UpdatedAt: at0}
			if err := s.Append(ctx, "t-1", 1, b); err != nil {
				t.Fatal(err)
			}
			v, _ = s.ViewsOf(ctx, "t-1")
			rows, _ = s.TaskViews(ctx)
			if !v.Task.PinnedAt.IsZero() || !rows[0].PinnedAt.IsZero() {
				t.Fatalf("unpinned: views %v, rows %v", v.Task.PinnedAt, rows[0].PinnedAt)
			}
		})
	}
}

func TestViewsReplace(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			b := batch(rec(1, "a"))
			b.Views.Task = &hub.TaskView{ID: "t-1", UpdatedAt: at0}
			b.Views.Confirmations = []hub.ConfirmationView{
				{ID: "c-1", Task: "t-1"}, {ID: "c-2", Task: "t-1"}}
			s.Append(ctx, "t-1", 0, b)
			v, _ := s.ViewsOf(ctx, "t-1")
			if len(v.Confirmations) != 2 {
				t.Fatalf("views %+v", v)
			}
			// 待確認數由確認列推出，不看批次 Task 列上的值。
			rows, _ := s.TaskViews(ctx)
			if v.Task.Confirmations != 2 || len(rows) != 1 || rows[0].Confirmations != 2 {
				t.Fatalf("confirmations count: views %d, rows %+v", v.Task.Confirmations, rows)
			}
			b = batch(rec(2, "b"))
			b.Views.Task = &hub.TaskView{ID: "t-1", UpdatedAt: at0}
			if err := s.Append(ctx, "t-1", 1, b); err != nil {
				t.Fatal(err)
			}
			v, _ = s.ViewsOf(ctx, "t-1")
			if v.Confirmations == nil || len(v.Confirmations) != 0 || v.Task.Confirmations != 0 {
				t.Fatalf("confirmations %v, count %d", v.Confirmations, v.Task.Confirmations)
			}
		})
	}
}

// S1：Task 列的掛起操作數照批次寫入，下一批整列替換。
func TestFailedCount(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			for i, n := range []int{2, 0} {
				b := batch(rec(task.Seq(i+1), "a"))
				b.Views.Task = &hub.TaskView{ID: "t-1", Failed: n, UpdatedAt: at0}
				if err := s.Append(ctx, "t-1", task.Seq(i), b); err != nil {
					t.Fatal(err)
				}
				v, _ := s.ViewsOf(ctx, "t-1")
				rows, _ := s.TaskViews(ctx)
				if v.Task.Failed != n || len(rows) != 1 || rows[0].Failed != n {
					t.Fatalf("batch %d: views %d, rows %+v", i, v.Task.Failed, rows)
				}
			}
		})
	}
}

// Reproject 整體替換一條 Stream 的投影（含檢索列），只動這一條；
// 做兩次結果相同（重建中途崩潰後重做，R4）。
func TestReproject(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			for _, id := range []task.TaskID{"t-1", "t-2"} {
				b := batch(rec(1, "a"), rec(2, "b"))
				b.Views.Task = &hub.TaskView{ID: id, Title: "old", UpdatedAt: at0}
				b.Views.Confirmations = []hub.ConfirmationView{{ID: "c-1", Task: id, RequestedAt: at0}}
				b.Views.Search = []hub.SearchEntry{{Seq: 1, Text: "stale label"}, {Seq: 2, Text: "kept"}}
				if err := s.Append(ctx, id, 0, b); err != nil {
					t.Fatal(err)
				}
			}
			other, _ := s.ViewsOf(ctx, "t-2")
			want := hub.Views{
				Task: &hub.TaskView{ID: "t-1", Title: "new", Sessions: 2, Failed: 1,
					Confirmations: 0, UpdatedAt: at0.Add(time.Second),
					Priority: task.PriorityHigh, Labels: []task.LabelID{"l-a", "l-b"},
					Agents: []hub.TaskAgent{{ID: "claude", Name: "Claude Code"}, {ID: "codex", Name: "Codex"}}},
				Confirmations: []hub.ConfirmationView{},
				Search:        []hub.SearchEntry{{Seq: 2, Text: "kept"}, {Seq: 3, Text: "new label"}},
			}
			for round := range 2 {
				if err := s.Reproject(ctx, "t-1", want); err != nil {
					t.Fatal(err)
				}
				got, _ := s.ViewsOf(ctx, "t-1")
				if !reflectEqual(got, want) {
					t.Fatalf("round %d: got %+v, want %+v", round, got, want)
				}
				hitsIn := func(q string) []hub.SearchHit {
					hits, _ := s.Search(ctx, q, 10)
					return slices.DeleteFunc(hits, func(h hub.SearchHit) bool { return h.Task != "t-1" })
				}
				if hits := hitsIn("stale"); len(hits) != 0 {
					t.Fatalf("round %d: stale search row %v", round, hits)
				}
				if hits := hitsIn("new label"); len(hits) != 1 {
					t.Fatalf("round %d: new label hits %v", round, hits)
				}
			}
			if got, _ := s.ViewsOf(ctx, "t-2"); !reflectEqual(got, other) {
				t.Fatalf("other stream changed: %+v", got)
			}
		})
	}
}

// 投影版本：從未記過是 0；記下後讀回同一個值，SQLite 重開後仍在。
func TestViewsVersion(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			if v, err := s.ViewsVersion(ctx); err != nil || v != 0 {
				t.Fatalf("fresh %d %v", v, err)
			}
			for _, n := range []int{3, 1} {
				if err := s.SetViewsVersion(ctx, n); err != nil {
					t.Fatal(err)
				}
				if v, _ := s.ViewsVersion(ctx); v != n {
					t.Fatalf("got %d, want %d", v, n)
				}
			}
		})
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, _ := Open(ctx, path, Options{})
	s.SetViewsVersion(ctx, 7)
	s.Close()
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.ViewsVersion(ctx); v != 7 {
		t.Fatalf("after reopen %d", v)
	}
}

// reflectEqual 比較兩份投影；時間以 Equal 比（SQLite 讀回的沒有單調時鐘）。
func reflectEqual(a, b hub.Views) bool {
	if (a.Task == nil) != (b.Task == nil) {
		return false
	}
	if a.Task != nil {
		x, y := *a.Task, *b.Task
		if !x.UpdatedAt.Equal(y.UpdatedAt) {
			return false
		}
		x.UpdatedAt, y.UpdatedAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return slices.EqualFunc(a.Confirmations, b.Confirmations, func(x, y hub.ConfirmationView) bool {
		eq := x.RequestedAt.Equal(y.RequestedAt)
		x.RequestedAt, y.RequestedAt = time.Time{}, time.Time{}
		return eq && x == y
	}) && slices.Equal(a.Search, b.Search)
}

func TestSearchAccumulates(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			b := batch(rec(1, "a"))
			b.Views.Search = []hub.SearchEntry{{Seq: 1, Text: "a"}}
			s.Append(ctx, "t-1", 0, b)
			b = batch(rec(2, "b"))
			b.Views.Search = []hub.SearchEntry{{Seq: 2, Text: "b"}}
			s.Append(ctx, "t-1", 1, b)
			b = batch(rec(3, "c"))
			b.Views.Search = []hub.SearchEntry{{Seq: 3, Text: "c"}}
			b.Views.Unsearch = []task.Seq{1}
			if err := s.Append(ctx, "t-1", 2, b); err != nil {
				t.Fatal(err)
			}
			v, _ := s.ViewsOf(ctx, "t-1")
			if len(v.Search) != 2 || v.Search[0].Seq != 2 || v.Search[1].Seq != 3 {
				t.Fatalf("search %v", v.Search)
			}
		})
	}
}

func TestNewWorkspaceAndRules(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			b := batch(rec(1, "a"))
			b.NewWorkspace = &hub.Workspace{ID: "w-1", Path: "/p", Name: "p"}
			b.NewRules = []task.Rule{{ID: "r-1", Scope: task.ScopeGlobal,
				Pattern: "rm", Verdict: task.Allow}}
			s.Append(ctx, "t-1", 0, b)
			b = batch(rec(2, "b"))
			b.NewRules = []task.Rule{{ID: "r-1", Scope: task.ScopeGlobal,
				Pattern: "mv", Verdict: task.Deny}}
			if err := s.Append(ctx, "t-1", 1, b); err != nil {
				t.Fatal(err)
			}
			ws, _ := s.Workspaces(ctx)
			if len(ws) != 1 || ws[0].Path != "/p" {
				t.Fatalf("workspaces %v", ws)
			}
			rules, _ := s.Rules(ctx)
			if len(rules) != 1 || rules[0].Verdict != task.Deny || rules[0].Pattern != "mv" {
				t.Fatalf("rules %v", rules)
			}
		})
	}
}

func TestDeleteStream(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			b := batch(rec(1, "a"))
			b.Views.Task = &hub.TaskView{ID: "t-1", UpdatedAt: at0}
			b.Views.Search = []hub.SearchEntry{{Seq: 1, Text: "a"}}
			s.Append(ctx, "t-1", 0, b)
			s.Append(ctx, "t-2", 0, batch(rec(1, "x")))
			if err := s.Delete(ctx, "t-1"); err != nil {
				t.Fatal(err)
			}
			got, _ := s.Read(ctx, "t-1", 0, 0)
			if len(got) != 0 {
				t.Fatalf("read %v", got)
			}
			ids, _ := s.Streams(ctx)
			if len(ids) != 1 || ids[0] != "t-2" {
				t.Fatalf("streams %v", ids)
			}
			v, _ := s.ViewsOf(ctx, "t-1")
			if v.Task != nil || len(v.Search) != 0 {
				t.Fatalf("views %+v", v)
			}
			tv, _ := s.TaskViews(ctx)
			for _, x := range tv {
				if x.ID == "t-1" {
					t.Fatal("deleted task in views")
				}
			}
			got, _ = s.Read(ctx, "t-2", 0, 0)
			if len(got) != 1 {
				t.Fatal("other stream damaged")
			}
		})
	}
}

// 用過的檔案（D107）：Append 按 (Task, Path) 併進已存的列（方式、時間與位置取後來的，
// 改過一直算改過）；Files 帶 Task 的 Workspace、不含已歸檔的 Task（F16）；Reproject 整體替換這一條；
// Delete 一起刪掉（F17）；其他 Task 不受影響。
func TestFiles(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			at1 := at0.Add(time.Second)
			row := func(id task.TaskID, ws task.WorkspaceID, archived bool) *hub.TaskView {
				return &hub.TaskView{ID: id, Title: "t", Workspace: ws, Status: task.TaskTodo, Archived: archived, UpdatedAt: at0}
			}
			b := batch(rec(1, "a"))
			b.Views.Task = row("t-1", "w1", false)
			b.Views.Files = []hub.FileUse{
				{Path: "a.go", Via: hub.FileEdit, Edited: true, At: at0},
				{Path: "/etc/hosts", Outside: true, Via: hub.FileRead, At: at0},
			}
			if err := s.Append(ctx, "t-1", 0, b); err != nil {
				t.Fatal(err)
			}
			b = batch(rec(2, "b"))
			b.Views.Task = row("t-1", "w1", false)
			b.Views.Files = []hub.FileUse{{Path: "a.go", Via: hub.FileRead, At: at1}}
			if err := s.Append(ctx, "t-1", 1, b); err != nil {
				t.Fatal(err)
			}
			b = batch(rec(1, "x"))
			b.Views.Task = row("t-2", "w2", false)
			b.Views.Files = []hub.FileUse{{Path: "a.go", Via: hub.FileAttach, At: at0}}
			if err := s.Append(ctx, "t-2", 0, b); err != nil {
				t.Fatal(err)
			}
			b = batch(rec(1, "y"))
			b.Views.Task = row("t-3", "w1", true)
			b.Views.Files = []hub.FileUse{{Path: "c.go", Via: hub.FileRead, At: at0}}
			if err := s.Append(ctx, "t-3", 0, b); err != nil {
				t.Fatal(err)
			}
			sorted := func() []hub.TaskFile {
				t.Helper()
				got, err := s.Files(ctx)
				if err != nil {
					t.Fatal(err)
				}
				slices.SortFunc(got, func(a, b hub.TaskFile) int {
					return cmp.Or(cmp.Compare(a.Task, b.Task), cmp.Compare(a.Use.Path, b.Use.Path))
				})
				return got
			}
			want := []hub.TaskFile{
				{Task: "t-1", Workspace: "w1", Use: hub.FileUse{Path: "/etc/hosts", Outside: true, Via: hub.FileRead, At: at0}},
				{Task: "t-1", Workspace: "w1", Use: hub.FileUse{Path: "a.go", Via: hub.FileRead, Edited: true, At: at1}},
				{Task: "t-2", Workspace: "w2", Use: hub.FileUse{Path: "a.go", Via: hub.FileAttach, At: at0}},
			}
			if got := sorted(); !slices.Equal(got, want) {
				t.Fatalf("merged %+v\nwant %+v", got, want)
			}
			v, _ := s.ViewsOf(ctx, "t-1")
			if len(v.Files) != 2 || v.Files[0].Path != "/etc/hosts" {
				t.Fatalf("views files %+v", v.Files)
			}

			if err := s.Reproject(ctx, "t-1", hub.Views{Task: row("t-1", "w1", false),
				Files: []hub.FileUse{{Path: "b.go", Via: hub.FileMove, Edited: true, At: at0}}}); err != nil {
				t.Fatal(err)
			}
			want = []hub.TaskFile{
				{Task: "t-1", Workspace: "w1", Use: hub.FileUse{Path: "b.go", Via: hub.FileMove, Edited: true, At: at0}},
				{Task: "t-2", Workspace: "w2", Use: hub.FileUse{Path: "a.go", Via: hub.FileAttach, At: at0}},
			}
			if got := sorted(); !slices.Equal(got, want) {
				t.Fatalf("reprojected %+v", got)
			}

			if err := s.Delete(ctx, "t-1"); err != nil {
				t.Fatal(err)
			}
			if got := sorted(); !slices.Equal(got, want[1:]) {
				t.Fatalf("F17 after delete %+v", got)
			}
		})
	}
}

// 結束的回合（D115，m2.md R01–R03）：Append 加進已存的列（按結束的順序），ViewsOf 照順序讀回、失敗的帶原因；
// Recap 只取 since 之後（含）、未歸檔的 Task，帶 Task 的標題與 Workspace；Reproject 整體替換這一條；Delete 一起刪掉。
func TestTurns(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			at1, at2 := at0.Add(time.Second), at0.Add(2*time.Second)
			row := func(id task.TaskID, archived bool) *hub.TaskView {
				return &hub.TaskView{ID: id, Title: "title " + string(id), Workspace: "w1", Status: task.TaskTodo, Archived: archived, UpdatedAt: at0}
			}
			failed := hub.TurnEnd{Session: "s1", Turn: "u1", Result: task.TurnError, Text: "fix it",
				Problem: &task.Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}, EndedAt: at0}
			done := hub.TurnEnd{Session: "s1", Turn: "u2", Result: task.TurnOK, Attachment: "a.go", EndedAt: at2}
			b := batch(rec(1, "a"))
			b.Views.Task = row("t-1", false)
			b.Views.Turns = []hub.TurnEnd{failed}
			if err := s.Append(ctx, "t-1", 0, b); err != nil {
				t.Fatal(err)
			}
			b = batch(rec(2, "b"))
			b.Views.Task = row("t-1", false)
			b.Views.Turns = []hub.TurnEnd{done}
			if err := s.Append(ctx, "t-1", 1, b); err != nil {
				t.Fatal(err)
			}
			other := hub.TurnEnd{Session: "s9", Turn: "u9", Result: task.TurnOK, Text: "x", EndedAt: at1}
			b = batch(rec(1, "c"))
			b.Views.Task = row("t-2", true)
			b.Views.Turns = []hub.TurnEnd{other}
			if err := s.Append(ctx, "t-2", 0, b); err != nil {
				t.Fatal(err)
			}

			v, err := s.ViewsOf(ctx, "t-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Turns) != 2 || !reflect.DeepEqual(v.Turns[0], failed) || !reflect.DeepEqual(v.Turns[1], done) {
				t.Fatalf("views turns %+v", v.Turns)
			}
			recap := func(since time.Time) []hub.RecapTurn {
				t.Helper()
				got, err := s.Recap(ctx, since)
				if err != nil {
					t.Fatal(err)
				}
				slices.SortFunc(got, func(a, b hub.RecapTurn) int { return a.EndedAt.Compare(b.EndedAt) })
				return got
			}
			want := []hub.RecapTurn{
				{Task: "t-1", TaskTitle: "title t-1", Workspace: "w1", TurnEnd: failed},
				{Task: "t-1", TaskTitle: "title t-1", Workspace: "w1", TurnEnd: done},
			}
			if got := recap(at0); !reflect.DeepEqual(got, want) {
				t.Fatalf("R02 recap %+v\nwant %+v", got, want)
			}
			if got := recap(at1); !reflect.DeepEqual(got, want[1:]) {
				t.Fatalf("R02 since %+v", got)
			}

			if err := s.Reproject(ctx, "t-1", hub.Views{Task: row("t-1", false), Turns: []hub.TurnEnd{done}}); err != nil {
				t.Fatal(err)
			}
			if got := recap(at0); !reflect.DeepEqual(got, want[1:]) {
				t.Fatalf("R03 reprojected %+v", got)
			}
			if err := s.Delete(ctx, "t-1"); err != nil {
				t.Fatal(err)
			}
			if got := recap(at0); len(got) != 0 {
				t.Fatalf("after delete %+v", got)
			}
		})
	}
}

func TestTaskViewsOrder(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			// 只在小數位不同的時間——詞序必須等於時序（fixed-width）。
			times := []time.Time{
				at0,                             // 整秒無小數
				at0.Add(50 * time.Millisecond),  // .05
				at0.Add(100 * time.Millisecond), // .1
			}
			ids := []task.TaskID{"t-1", "t-2", "t-3"}
			for i, tm := range times {
				b := batch(rec(1, "a"))
				b.Views.Task = &hub.TaskView{ID: ids[i], UpdatedAt: tm}
				if err := s.Append(ctx, ids[i], 0, b); err != nil {
					t.Fatal(err)
				}
			}
			tv, _ := s.TaskViews(ctx)
			if len(tv) != 3 || tv[0].ID != "t-3" || tv[1].ID != "t-2" || tv[2].ID != "t-1" {
				t.Fatalf("order %v", tv)
			}
		})
	}
}

func TestAppendCancelledCtx(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			s := mk(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.Append(ctx, "t-1", 0, batch(rec(1, "a"))); !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v", err)
			}
			got, _ := s.Read(context.Background(), "t-1", 0, 0)
			if len(got) != 0 {
				t.Fatal("written after cancel")
			}
		})
	}
}

// --- Config conformance ---

// 標籤目錄（D135）：存、改、刪，按 ID 排序；刪不存在的不算錯。
func TestLabels(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			if got, err := s.Labels(ctx); err != nil || len(got) != 0 {
				t.Fatalf("empty catalog %v %v", got, err)
			}
			b := hub.Label{ID: "l-b", Name: "后端", Tone: hub.ToneAmber}
			a := hub.Label{ID: "l-a", Name: "Frontend", Tone: hub.ToneSky}
			for _, l := range []hub.Label{b, a} {
				if err := s.PutLabel(ctx, l); err != nil {
					t.Fatal(err)
				}
			}
			a.Name, a.Tone = "UI", hub.ToneRose
			if err := s.PutLabel(ctx, a); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.Labels(ctx); !reflect.DeepEqual(got, []hub.Label{a, b}) {
				t.Fatalf("labels %v", got)
			}
			if err := s.DeleteLabel(ctx, "l-b"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteLabel(ctx, "l-gone"); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.Labels(ctx); !reflect.DeepEqual(got, []hub.Label{a}) {
				t.Fatalf("after delete %v", got)
			}
		})
	}
}

func TestDevices(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			if got, err := s.Devices(ctx); err != nil || len(got) != 0 {
				t.Fatalf("no devices: %v %v", got, err)
			}
			at := time.Date(2026, 11, 17, 9, 30, 0, 123456789, time.UTC)
			a := hub.Device{ID: "d-a", Name: "iPhone", Key: hub.DeviceKey{1}, PairedAt: at.Add(time.Minute)}
			b := hub.Device{ID: "d-b", Name: "iPad", Key: hub.DeviceKey{2}, PairedAt: at}
			for _, d := range []hub.Device{a, b} {
				if err := s.PutDevice(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
			a.Name, a.LastSeen = "May 的 iPhone", at.Add(time.Hour) // 從沒連過的 b 保持零值
			if err := s.PutDevice(ctx, a); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.Devices(ctx); !reflect.DeepEqual(got, []hub.Device{b, a}) {
				t.Fatalf("devices %+v", got)
			}
			if err := s.DeleteDevice(ctx, "d-b"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteDevice(ctx, "d-gone"); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.Devices(ctx); !reflect.DeepEqual(got, []hub.Device{a}) {
				t.Fatalf("after delete %+v", got)
			}
			if err := s.PutDevice(ctx, b); err != nil {
				t.Fatal(err)
			}
			if err := s.ClearDevices(ctx); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.Devices(ctx); len(got) != 0 {
				t.Fatalf("after clear %+v", got)
			}
		})
	}
}

func TestConfig(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			s.PutWorkspace(ctx, hub.Workspace{ID: "w-1", Path: "/a"})
			if err := s.PutWorkspace(ctx, hub.Workspace{
				ID: "w-1", Path: "/a", Name: "renamed"}); err != nil {
				t.Fatal(err)
			}
			ws, _ := s.Workspaces(ctx)
			if len(ws) != 1 || ws[0].Name != "renamed" {
				t.Fatalf("workspaces %v", ws)
			}
			r1 := task.Rule{ID: "r-1", Scope: task.ScopeGlobal, Verdict: task.Allow}
			r2 := task.Rule{ID: "r-2", Scope: task.ScopeWorkspace,
				ScopeID: "w-1", Verdict: task.Deny}
			b := batch(rec(1, "x"))
			b.NewRules = []task.Rule{r1, r2}
			if err := s.Append(ctx, "t-1", 0, b); err != nil {
				t.Fatal(err)
			}
			rules, _ := s.Rules(ctx)
			if len(rules) != 2 {
				t.Fatalf("rules %v", rules)
			}
			if err := s.DeleteRule(ctx, "r-1"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteRule(ctx, "r-nope"); err != nil {
				t.Fatal("unknown delete")
			}
			rules, _ = s.Rules(ctx)
			if len(rules) != 1 || rules[0].ID != "r-2" {
				t.Fatalf("rules %v", rules)
			}
			st, _ := s.Settings(ctx)
			if st != hub.DefaultSettings() {
				t.Fatalf("settings %+v", st)
			}
			put := hub.DefaultSettings()
			put.ReviewerProfile = "p-1"
			put.Notify.Completed = true
			put.Notify.Ask = false
			if err := s.PutSettings(ctx, put); err != nil {
				t.Fatal(err)
			}
			st, _ = s.Settings(ctx)
			if st.ReviewerProfile != "p-1" || st.Notify.Ask ||
				!st.Notify.Completed {
				t.Fatalf("settings %+v", st)
			}
		})
	}
}

// N-b：存過的 JSON 只寫了部分欄位 → 缺少的欄位取預設值（只 SQLite 有真 JSON 存取）。
func TestSettingsDefaultsMerge(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	rw, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Execute(ctx,
		`INSERT INTO settings(key, value_json) VALUES('hub','{"notify":{"completed":true}}')`); err != nil {
		t.Fatal(err)
	}
	rw.Close()
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	st, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := hub.DefaultSettings()
	want.Notify.Completed = true
	if st != want {
		t.Fatalf("%+v", st)
	}
}

// N-c：所有開關全關也能存取——明確的 false 不會被預設值蓋掉。
func TestSettingsAllOff(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			if err := s.PutSettings(ctx, hub.Settings{
				Notify: hub.NotifySettings{}}); err != nil {
				t.Fatal(err)
			}
			st, _ := s.Settings(ctx)
			if st.Notify != (hub.NotifySettings{}) {
				t.Fatalf("%+v", st.Notify)
			}
		})
	}
	// SQLite 重開之後還是全關。
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSettings(ctx, hub.Settings{
		Notify: hub.NotifySettings{}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	st, _ := s.Settings(ctx)
	if st.Notify != (hub.NotifySettings{}) {
		t.Fatalf("%+v", st.Notify)
	}
}

// --- SQLite 併發 ---

func TestConcurrentReadWrite(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	done := make(chan struct{})
	var wg sync.WaitGroup
	var failed atomic.Bool
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				got, err := s.Read(ctx, "t-1", 0, 0)
				if err != nil || len(got)%3 != 0 {
					failed.Store(true)
					return
				}
			}
		}()
	}
	for i := range 50 {
		if err := s.Append(ctx, "t-1", task.Seq(3*i), batch(
			rec(task.Seq(3*i+1), "a"), rec(task.Seq(3*i+2), "b"), rec(task.Seq(3*i+3), "c"))); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	wg.Wait()
	if failed.Load() {
		t.Fatal("reader saw partial batch or error")
	}
}

func TestCloseDuringReads(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	s.Append(ctx, "t-1", 0, batch(rec(1, "a")))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.Read(ctx, "t-1", 0, 0) // 錯誤可以接受：不能 panic
			}
		}()
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
}

// --- SQLite 專屬 ---

func TestRoundtrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, _ := Open(ctx, path, Options{})
	b := batch(rec(1, "a"), rec(2, "b"))
	b.Views.Task = &hub.TaskView{ID: "t-1", Title: "t", UpdatedAt: at0}
	b.Views.Search = []hub.SearchEntry{{Seq: 2, Text: "b"}}
	b.NewWorkspace = &hub.Workspace{ID: "w-1", Path: "/p", Name: "p"}
	b.NewRules = []task.Rule{{ID: "r-1", Verdict: task.Allow}}
	s.Append(ctx, "t-1", 0, b)
	s.PutSettings(ctx, hub.Settings{ReviewerProfile: "p-1"})
	s.Close()

	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _ := s.Read(ctx, "t-1", 0, 0)
	if len(got) != 2 {
		t.Fatalf("records %v", got)
	}
	v, _ := s.ViewsOf(ctx, "t-1")
	if v.Task == nil || v.Task.Title != "t" || len(v.Search) != 1 {
		t.Fatalf("views %+v", v)
	}
	ws, _ := s.Workspaces(ctx)
	rules, _ := s.Rules(ctx)
	st, _ := s.Settings(ctx)
	if len(ws) != 1 || len(rules) != 1 || st.ReviewerProfile != "p-1" {
		t.Fatal("config lost")
	}
}

func TestTriggerGuard(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, _ := Open(ctx, path, Options{})
	s.Append(ctx, "t-1", 0, batch(rec(1, "a"), rec(2, "b"), rec(3, "c")))
	rw, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	_, err = rw.Execute(ctx,
		"INSERT INTO events(stream, seq, kind, at, payload) VALUES('t-1', 5, 'k', '2026-01-01T00:00:00.000000000Z', x'00')")
	var se *sqlite.Error
	if !errors.As(err, &se) || se.Code != 19 {
		t.Fatalf("want code 19, got %v", err)
	}
	s.Close()
}

func TestFTS(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, _ := Open(ctx, path, Options{})
	b := batch(rec(1, "a"))
	b.Views.Search = []hub.SearchEntry{{Seq: 1, Text: "hello world"}}
	s.Append(ctx, "t-1", 0, b)
	ro, err := sqlite.Open(ctx, path, sqlite.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	const q = `SELECT s.seq FROM search_fts f JOIN search s ON s.id=f.rowid WHERE search_fts MATCH 'hello'`
	rows, err := ro.Query(ctx, q)
	if err != nil || len(rows.Values) != 1 {
		t.Fatalf("fts %v %v", rows.Values, err)
	}
	b = batch(rec(2, "x"))
	b.Views.Unsearch = []task.Seq{1}
	if err := s.Append(ctx, "t-1", 1, b); err != nil {
		t.Fatal(err)
	}
	rows, _ = ro.Query(ctx, q)
	if len(rows.Values) != 0 {
		t.Fatalf("fts after delete %v", rows.Values)
	}
	s.Close()
}

// --- Index conformance ---

func TestConfirmationsAcrossTasks(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			for i, id := range []task.TaskID{"t-1", "t-2"} {
				b := batch(rec(1, "a"))
				b.Views.Confirmations = []hub.ConfirmationView{{
					ID: task.ConfirmationID("c-x"), Task: id,
					Kind: "merge", RequestedBy: "d9",
					RequestedAt: at0.Add(time.Duration(i) * time.Hour)}}
				if err := s.Append(ctx, id, 0, b); err != nil {
					t.Fatal(err)
				}
			}
			cs, err := s.Confirmations(ctx)
			if err != nil || len(cs) != 2 {
				t.Fatalf("confirmations %v %v", cs, err)
			}
			if cs[0].Task != "t-1" || cs[1].Task != "t-2" {
				t.Fatalf("order %v", cs)
			}
		})
	}
}

// S6–S8：命中按 Task 更新時間新到舊、Task ID、訊息的 Seq 新到舊排序；limit 在排序之後。
// 兩種實作跑同一套斷言（S8：結果一致）。
func TestSearchOrdering(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			put := func(id task.TaskID, texts []string, updated time.Time) {
				t.Helper()
				b := batch(rec(1, "x"))
				b.Views.Task = &hub.TaskView{ID: id, UpdatedAt: updated}
				for i, text := range texts {
					b.Views.Search = append(b.Views.Search,
						hub.SearchEntry{Seq: task.Seq(i + 1), Text: text})
				}
				if err := s.Append(ctx, id, 0, b); err != nil {
					t.Fatal(err)
				}
			}

			// S6：B 較新排最前，A 的兩筆訊息新的在前（D70）。
			put("t-a", []string{"x old1", "x old2"}, at0)
			put("t-b", []string{"x new"}, at0.Add(time.Hour))
			hits, err := s.Search(ctx, "x", 10)
			if err != nil {
				t.Fatal(err)
			}
			want := []hub.SearchHit{
				{Task: "t-b", Seq: 1, Text: "x new"},
				{Task: "t-a", Seq: 2, Text: "x old2"},
				{Task: "t-a", Seq: 1, Text: "x old1"}}
			if !slices.Equal(hits, want) {
				t.Fatalf("hits %v", hits)
			}

			// 更新時間相同 → Task ID 升冪。
			put("t-d", []string{"y"}, at0)
			put("t-c", []string{"y"}, at0)
			hits, _ = s.Search(ctx, "y", 10)
			if len(hits) != 2 || hits[0].Task != "t-c" || hits[1].Task != "t-d" {
				t.Fatalf("equal-time hits %v", hits)
			}
		})
	}

	// S7：limit 取最新更新的 Task。
	for name, mk := range impls(t) {
		t.Run(name+"/limit", func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			for i, id := range []task.TaskID{"t-1", "t-2", "t-3"} {
				b := batch(rec(1, "x"))
				b.Views.Task = &hub.TaskView{ID: id,
					UpdatedAt: at0.Add(time.Duration(i) * time.Hour)}
				b.Views.Search = []hub.SearchEntry{{Seq: 1, Text: "x"}}
				if err := s.Append(ctx, id, 0, b); err != nil {
					t.Fatal(err)
				}
			}
			hits, err := s.Search(ctx, "x", 2)
			if err != nil || len(hits) != 2 ||
				hits[0].Task != "t-3" || hits[1].Task != "t-2" {
				t.Fatalf("hits %v, %v", hits, err)
			}
		})
	}
}

func TestSearchConformance(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			for i, id := range []task.TaskID{"t-1", "t-2"} {
				b := batch(rec(1, "a"))
				b.Views.Task = &hub.TaskView{ID: id, UpdatedAt: at0}
				b.Views.Search = []hub.SearchEntry{
					{Seq: 1, Text: "say hi"},
					{Seq: 2, Text: "修复登录问题"},
					{Seq: 3, Text: "HELLO there"},
					{Seq: 4, Text: "école française"}}
				b.Views.Unsearch = nil
				if i == 1 {
					b.Views.Unsearch = []task.Seq{3} // t-2 的 3 號一開始就移除
				}
				if err := s.Append(ctx, id, 0, b); err != nil {
					t.Fatal(err)
				}
			}
			hits := func(q string, lim int) []hub.SearchHit {
				t.Helper()
				h, err := s.Search(ctx, q, lim)
				if err != nil {
					t.Fatal(err)
				}
				return h
			}
			h := hits("hi", 10)
			if len(h) != 2 || h[0].Task != "t-1" || h[1].Task != "t-2" ||
				h[0].Seq != 1 || h[1].Seq != 1 {
				t.Fatalf("hits %v", h)
			}
			if h := hits("say hi", 10); len(h) != 2 {
				t.Fatalf("two-term %v", h)
			}
			if h := hits("say bye", 10); len(h) != 0 {
				t.Fatalf("two-term-miss %v", h)
			}
			if h := hits("hi", 1); len(h) != 1 {
				t.Fatalf("limit %v", h)
			}
			if h := hits("登录", 10); len(h) != 2 {
				t.Fatalf("cjk %v", h)
			}
			if h := hits("修复 登录", 10); len(h) != 2 {
				t.Fatalf("cjk-2term %v", h)
			}
			if h := hits("he", 10); len(h) != 1 { // 2-rune term
				t.Fatalf("short %v", h)
			}
			if h := hits("hello", 10); len(h) != 1 { // ASCII case-insensitive
				t.Fatalf("case %v", h)
			}
			// 非 ASCII 不折：É 打不中 é，反向也是
			if h := hits("É", 10); len(h) != 0 {
				t.Fatalf("nonascii %v", h)
			}
			if h := hits("é", 10); len(h) != 2 { // é 本身命中
				t.Fatalf("nonascii2 %v", h)
			}
			if h := hits("ÉCOLE", 10); len(h) != 0 { // 3+ 字非 ASCII 大小寫差不命中
				t.Fatalf("nonascii3 %v", h)
			}
			if h := hits("%_", 10); len(h) != 0 { // 不是萬用字元
				t.Fatalf("wildcard %v", h)
			}
			if h := hits("   ", 10); len(h) != 0 {
				t.Fatalf("ws %v", h)
			}
			// Unsearch：t-2 的 3 號不該出現
			for _, x := range hits("there", 10) {
				if x.Task == "t-2" {
					t.Fatal("unsearched hit")
				}
			}
		})
	}
}

// Q30–Q35、Q38（m2.md 第 32 節，D70）：標籤命中總在前；訊息命中每個 Task 最多 3 條、取最新的，
// 在 limit 之前篩；遮蔽了一條，下一條較舊的補上。來源與 agent 名稱原樣存取。兩種實作跑同一套斷言。
func TestSearchMessageCap(t *testing.T) {
	type hitKey struct {
		task task.TaskID
		seq  task.Seq
	}
	keys := func(hits []hub.SearchHit) []hitKey {
		var out []hitKey
		for _, h := range hits {
			out = append(out, hitKey{h.Task, h.Seq})
		}
		return out
	}
	// put 寫一個 Task：第 1 條是標籤，之後 messages 條訊息（奇數是使用者的、偶數是回答），
	// 文字都含 word；label 為真時標籤也含 word。
	put := func(t *testing.T, s conformance, id task.TaskID, updated time.Time, label bool, messages int, word string) {
		t.Helper()
		var recs []hub.Record
		for i := range messages + 1 {
			recs = append(recs, rec(task.Seq(i+1), "x"))
		}
		b := batch(recs...)
		b.Views.Task = &hub.TaskView{ID: id, UpdatedAt: updated}
		title := "other"
		if label {
			title = word + " title"
		}
		b.Views.Search = []hub.SearchEntry{{Seq: 1, Kind: hub.SearchLabel, Text: title}}
		for i := range messages {
			e := hub.SearchEntry{Seq: task.Seq(i + 2), Kind: hub.SearchPrompt, Ref: fmt.Sprintf("p-%d", i), Text: fmt.Sprintf("%s %d", word, i)}
			if i%2 == 1 {
				e.Kind, e.Ref, e.Agent = hub.SearchReply, fmt.Sprintf("m-%d", i), "Grok"
			}
			b.Views.Search = append(b.Views.Search, e)
		}
		if err := s.Append(context.Background(), id, 0, b); err != nil {
			t.Fatal(err)
		}
	}
	search := func(t *testing.T, s conformance, q string, limit int) []hub.SearchHit {
		t.Helper()
		hits, err := s.Search(context.Background(), q, limit)
		if err != nil {
			t.Fatal(err)
		}
		return hits
	}
	for name, mk := range impls(t) {
		t.Run(name+"/order", func(t *testing.T) {
			s := mk(t)
			put(t, s, "t-a", at0, true, 5, "登录")                  // Q31：標籤與 5 條訊息
			put(t, s, "t-b", at0.Add(time.Hour), false, 10, "登录") // Q30：10 條訊息，標籤不含
			hits := search(t, s, "登录", 20)
			want := []hitKey{{"t-a", 1}, // Q32：標籤在前，即使 A 較舊
				{"t-b", 11}, {"t-b", 10}, {"t-b", 9}, // Q30：最新的 3 條，新到舊
				{"t-a", 6}, {"t-a", 5}, {"t-a", 4}}
			if got := keys(hits); !slices.Equal(got, want) {
				t.Fatalf("hits %v, want %v", got, want)
			}
			if hits[0].Kind != hub.SearchLabel || hits[0].Ref != "" ||
				hits[1].Kind != hub.SearchReply || hits[1].Agent != "Grok" || hits[1].Ref != "m-9" ||
				hits[2].Kind != hub.SearchPrompt || hits[2].Agent != "" || hits[2].Ref != "p-8" {
				t.Fatalf("kinds %+v", hits[:3])
			}
			v, _ := s.ViewsOf(context.Background(), "t-b")
			if e := v.Search[2]; e.Kind != hub.SearchReply || e.Ref != "m-1" || e.Agent != "Grok" || e.Text != "登录 1" {
				t.Fatalf("stored entry %+v", e)
			}

			// Q35：遮蔽 B 最新的一條，較舊的一條補上。
			b := batch(rec(12, "x"))
			b.Views.Task = &hub.TaskView{ID: "t-b", UpdatedAt: at0.Add(time.Hour)}
			b.Views.Unsearch = []task.Seq{11}
			if err := s.Append(context.Background(), "t-b", 11, b); err != nil {
				t.Fatal(err)
			}
			want = []hitKey{{"t-a", 1}, {"t-b", 10}, {"t-b", 9}, {"t-b", 8},
				{"t-a", 6}, {"t-a", 5}, {"t-a", 4}}
			if got := keys(search(t, s, "登录", 20)); !slices.Equal(got, want) {
				t.Fatalf("after redaction %v, want %v", got, want)
			}
			// 只有一部分訊息含查詢的詞：上限按命中算，不按 Task 的訊息數。
			if got := keys(search(t, s, "登录 2", 20)); !slices.Equal(got, []hitKey{{"t-b", 4}, {"t-a", 4}}) {
				t.Fatalf("narrow %v", got)
			}
		})
		t.Run(name+"/labels first", func(t *testing.T) {
			// Q33：25 個標籤命中，上限 20：沒有訊息命中。
			s := mk(t)
			for i := range 25 {
				put(t, s, task.TaskID(fmt.Sprintf("t-%02d", i)), at0.Add(time.Duration(i)*time.Minute), true, 2, "登录")
			}
			hits := search(t, s, "登录", 20)
			if len(hits) != 20 {
				t.Fatalf("%d hits", len(hits))
			}
			for i, h := range hits {
				if h.Kind != hub.SearchLabel || h.Task != task.TaskID(fmt.Sprintf("t-%02d", 24-i)) {
					t.Fatalf("hit %d %+v", i, h)
				}
			}
		})
		t.Run(name+"/limit", func(t *testing.T) {
			// Q34：8 個 Task 各 5 條訊息，標籤不含。
			s := mk(t)
			for i := range 8 {
				put(t, s, task.TaskID(fmt.Sprintf("t-%d", i)), at0.Add(time.Duration(i)*time.Minute), false, 5, "登录")
			}
			count := func(hits []hub.SearchHit) map[task.TaskID]int {
				n := map[task.TaskID]int{}
				for _, h := range hits {
					n[h.Task]++
				}
				return n
			}
			n := count(search(t, s, "登录", 20))
			want := map[task.TaskID]int{"t-7": 3, "t-6": 3, "t-5": 3, "t-4": 3, "t-3": 3, "t-2": 3, "t-1": 2}
			if !maps.Equal(n, want) {
				t.Fatalf("limit 20: %v", n)
			}
			if hits := search(t, s, "登录", 500); len(hits) != 24 {
				t.Fatalf("limit 500: %d hits", len(hits))
			}
		})
	}
}

// Views.Secrets（m4.md AC37）：非 nil 時整組替換、nil 時不動、空的清掉；Reproject 整組；刪 Task 一起刪；按 agent 查。
func TestTaskSecrets(t *testing.T) {
	for name, mk := range impls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			used := func(agent task.AgentID) []string {
				t.Helper()
				keys, err := s.SecretsInUse(ctx, agent)
				if err != nil {
					t.Fatal(err)
				}
				slices.Sort(keys)
				return keys
			}
			b := batch(rec(1, "a"))
			b.Views.Secrets = []hub.TaskSecret{{Agent: "grok", Key: "k1"}, {Agent: "codex", Key: "k2"}}
			if err := s.Append(ctx, "t-1", 0, b); err != nil {
				t.Fatal(err)
			}
			b2 := batch(rec(1, "b"))
			b2.Views.Secrets = []hub.TaskSecret{{Agent: "grok", Key: "k3"}}
			if err := s.Append(ctx, "t-2", 0, b2); err != nil {
				t.Fatal(err)
			}
			if got := used("grok"); !slices.Equal(got, []string{"k1", "k3"}) {
				t.Fatalf("grok %v", got)
			}
			if got := used("codex"); !slices.Equal(got, []string{"k2"}) {
				t.Fatalf("codex %v", got)
			}
			if err := s.Append(ctx, "t-1", 1, batch(rec(2, "c"))); err != nil { // nil：不動
				t.Fatal(err)
			}
			if got := used("codex"); !slices.Equal(got, []string{"k2"}) {
				t.Fatalf("an append without secrets changed them: %v", got)
			}
			b3 := batch(rec(3, "d"))
			b3.Views.Secrets = []hub.TaskSecret{} // 空的：清掉
			if err := s.Append(ctx, "t-1", 2, b3); err != nil {
				t.Fatal(err)
			}
			if got := used("codex"); len(got) != 0 {
				t.Fatalf("an empty set kept %v", got)
			}
			if err := s.Reproject(ctx, "t-1", hub.Views{Secrets: []hub.TaskSecret{{Agent: "grok", Key: "k4"}}}); err != nil {
				t.Fatal(err)
			}
			if got := used("grok"); !slices.Equal(got, []string{"k3", "k4"}) {
				t.Fatalf("after reproject %v", got)
			}
			if err := s.Delete(ctx, "t-2"); err != nil {
				t.Fatal(err)
			}
			if got := used("grok"); !slices.Equal(got, []string{"k4"}) {
				t.Fatalf("after deleting t-2 %v", got)
			}
		})
	}
}
