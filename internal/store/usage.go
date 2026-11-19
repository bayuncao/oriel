package store

// usage.go：本機用量的三張表（usage.Store，docs/architecture/store.md）與讀外部
// SQLite 檔案的唯讀查詢（D90）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
	"github.com/egoist/mygo/plugins/sqlite"
)

var _ usage.Store = (*Store)(nil)

// UsageFiles 回傳存下的全部讀取位置，依路徑排序。
func (s *Store) UsageFiles(ctx context.Context) ([]usage.FileState, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx,
		"SELECT path, agent, size, mod_time, head, read_to, state, bad, done FROM usage_files ORDER BY path")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]usage.FileState, 0, len(rows.Values))
	for _, v := range rows.Values {
		mod, err := parseOptionalAt(toStr(v[3]))
		if err != nil {
			return nil, err
		}
		out = append(out, usage.FileState{Path: toStr(v[0]), Agent: task.AgentID(toStr(v[1])),
			Size: toInt(v[2]), ModTime: mod, Head: toStr(v[4]), ReadTo: toInt(v[5]),
			State: toBytes(v[6]), Bad: int(toInt(v[7])), Done: toInt(v[8]) != 0})
	}
	return out, nil
}

// mergeRecord 是同一 (agent, key) 的合併：每類取較大值，時間取較早的，會話、
// 父會話、模型、目錄保留先寫入的（usage.Record.Merge 的 SQL 版）。
const mergeRecord = `INSERT INTO usage_records(agent, key, at, session, parent, model, dir, input, cache_read, cache_write, output)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(agent, key) DO UPDATE SET
  at = CASE WHEN at = '' THEN excluded.at WHEN excluded.at = '' THEN at ELSE min(at, excluded.at) END,
  session = CASE WHEN session = '' THEN excluded.session ELSE session END,
  parent = CASE WHEN parent = '' THEN excluded.parent ELSE parent END,
  model = CASE WHEN model = '' THEN excluded.model ELSE model END,
  dir = CASE WHEN dir = '' THEN excluded.dir ELSE dir END,
  input = max(input, excluded.input),
  cache_read = max(cache_read, excluded.cache_read),
  cache_write = max(cache_write, excluded.cache_write),
  output = max(output, excluded.output)`

// ApplyUsage 在一個事務裡寫入一段讀取結果。
func (s *Store) ApplyUsage(ctx context.Context, b usage.Batch) error {
	if err := s.check(); err != nil {
		return err
	}
	stmts := make([]sqlite.Statement, 0, len(b.Records)+len(b.Quotas)+1)
	for _, r := range b.Records {
		stmts = append(stmts, sqlite.Statement{SQL: mergeRecord, Args: []any{
			string(r.Agent), r.Key, formatOptionalAt(r.At), r.Session, r.Parent, r.Model, r.Dir,
			r.Tokens.Input, r.Tokens.CacheRead, r.Tokens.CacheWrite, r.Tokens.Output}})
	}
	for _, q := range b.Quotas {
		windows, err := json.Marshal(q.Windows)
		if err != nil {
			return fail(CodeFailed, err, "detail", err.Error())
		}
		stmts = append(stmts, sqlite.Statement{
			SQL: `INSERT INTO usage_quotas(agent, bucket, name, plan, windows, at) VALUES(?,?,?,?,?,?)
ON CONFLICT(agent, bucket) DO UPDATE SET name = excluded.name, plan = excluded.plan,
  windows = excluded.windows, at = excluded.at WHERE excluded.at >= usage_quotas.at`,
			Args: []any{string(q.Agent), q.Bucket, q.Name, q.Plan, string(windows), formatOptionalAt(q.At)}})
	}
	if f := b.File; f != nil {
		state := f.State
		if state == nil {
			state = []byte{}
		}
		stmts = append(stmts, sqlite.Statement{
			SQL: "INSERT OR REPLACE INTO usage_files(path, agent, size, mod_time, head, read_to, state, bad, done) VALUES(?,?,?,?,?,?,?,?,?)",
			Args: []any{f.Path, string(f.Agent), f.Size, formatOptionalAt(f.ModTime), f.Head, f.ReadTo,
				state, int64(f.Bad), boolInt(f.Done)}})
	}
	if len(stmts) == 0 {
		return nil
	}
	_, err := s.w.Transaction(ctx, stmts)
	return mapErr(err)
}

// DeleteUsageFiles 刪掉這些檔案的讀取位置；已讀到的用量保留（D86）。
func (s *Store) DeleteUsageFiles(ctx context.Context, paths []string) error {
	if err := s.check(); err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	stmts := make([]sqlite.Statement, len(paths))
	for i, p := range paths {
		stmts[i] = sqlite.Statement{SQL: "DELETE FROM usage_files WHERE path=?", Args: []any{p}}
	}
	_, err := s.w.Transaction(ctx, stmts)
	return mapErr(err)
}

// UsageTotals 按 usage 算好的日界分組加總；時間存成固定寬度的 UTC 文字，詞序即時序。
func (s *Store) UsageTotals(ctx context.Context, bounds []time.Time) ([]usage.Total, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	if len(bounds) < 2 {
		return nil, nil
	}
	var values []string
	var args []any
	for i := 0; i+1 < len(bounds); i++ {
		values = append(values, "(?, ?, ?)")
		args = append(args, int64(i), formatAt(bounds[i]), formatAt(bounds[i+1]))
	}
	// 本機記錄的會話（或父會話）是 OrielHub 跑的就歸到那個 Task；上報的用量只在那個原生會話
	// 沒有任何本機記錄時算（m2.md 第 33 節）。
	q := fmt.Sprintf(`WITH b(i, lo, hi) AS (VALUES %s)
SELECT i, agent, model, dir, task_id, sum(input), sum(cache_read), sum(cache_write), sum(output) FROM (
  SELECT b.i, r.agent, r.model, r.dir, coalesce(s1.task_id, s2.task_id, '') AS task_id,
    r.input, r.cache_read, r.cache_write, r.output
  FROM usage_records r JOIN b ON r.at >= b.lo AND r.at < b.hi
  LEFT JOIN usage_sessions s1 ON r.session <> '' AND s1.agent = r.agent AND s1.native = r.session
  LEFT JOIN usage_sessions s2 ON r.parent <> '' AND s2.agent = r.agent AND s2.native = r.parent
  UNION ALL
  SELECT b.i, p.agent, p.model, p.dir, p.task_id, p.input, p.cache_read, p.cache_write, p.output
  FROM usage_reported p JOIN b ON p.at >= b.lo AND p.at < b.hi
  WHERE NOT EXISTS (SELECT 1 FROM usage_records r WHERE r.agent = p.agent AND r.session = p.native)
)
GROUP BY i, agent, model, dir, task_id
ORDER BY i, agent, model, dir, task_id`, strings.Join(values, ", "))
	rows, err := s.r.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]usage.Total, 0, len(rows.Values))
	for _, v := range rows.Values {
		out = append(out, usage.Total{Day: int(toInt(v[0])), Agent: task.AgentID(toStr(v[1])),
			Model: toStr(v[2]), Dir: toStr(v[3]), Task: task.TaskID(toStr(v[4])), Tokens: usage.Tokens{Input: toInt(v[5]),
				CacheRead: toInt(v[6]), CacheWrite: toInt(v[7]), Output: toInt(v[8])}})
	}
	return out, nil
}

// UsageQuotas 回傳各額度桶最新的快照，依 agent、桶排序。
func (s *Store) UsageQuotas(ctx context.Context) ([]usage.Quota, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx,
		"SELECT agent, bucket, name, plan, windows, at FROM usage_quotas ORDER BY agent, bucket")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]usage.Quota, 0, len(rows.Values))
	for _, v := range rows.Values {
		at, err := parseOptionalAt(toStr(v[5]))
		if err != nil {
			return nil, err
		}
		q := usage.Quota{Agent: task.AgentID(toStr(v[0])), Bucket: toStr(v[1]), Name: toStr(v[2]),
			Plan: toStr(v[3]), At: at}
		if err := json.Unmarshal([]byte(toStr(v[4])), &q.Windows); err != nil {
			return nil, fail(CodeFailed, err, "detail", err.Error())
		}
		out = append(out, q)
	}
	return out, nil
}

// queryFileWait 是外部資料庫被鎖住時等待的時間：擋得住短暫的 checkpoint，等不到就
// 回 usage.ErrBusy，下一輪再讀。
const queryFileWait = 2 * time.Second

// QueryFile 以唯讀方式打開一個外部 SQLite 檔案（agent 的記錄，D90），執行一條查詢
// 後立即關閉，回傳結果行；不執行 PRAGMA，不開長事務，所以不妨礙那個程式寫入與
// checkpoint。庫被鎖住時回 usage.ErrBusy。它滿足 usage.QueryFunc，由 app 交給讀
// SQLite 記錄的 Source；查詢的 SQL 屬於擁有那個格式的 runtime/<agent>（規則 3）。
func QueryFile(ctx context.Context, path, query string, args ...any) ([][]any, error) {
	db, err := sqlite.Open(ctx, filepath.Clean(path), sqlite.OpenOptions{ReadOnly: true, BusyTimeout: queryFileWait})
	if err != nil {
		return nil, busyOr(err)
	}
	defer db.Close()
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, busyOr(err)
	}
	return rows.Values, nil
}

// busyOr 把 SQLITE_BUSY、SQLITE_LOCKED 換成 usage.ErrBusy，其餘原樣回傳。
func busyOr(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) && (se.Code == 5 || se.Code == 6) {
		return fmt.Errorf("%w: %v", usage.ErrBusy, err)
	}
	return err
}
