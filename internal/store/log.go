package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/egoist/mygo/plugins/sqlite"
)

const timeFmt = "2006-01-02T15:04:05.000000000Z07:00"

func formatAt(t time.Time) string { return t.UTC().Format(timeFmt) }

// formatOptionalAt 同 formatAt，零值存成空字串（如未置頂的 pinned_at）。
func formatOptionalAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatAt(t)
}

// Append 是 Log.Append：一批記錄、遮蔽、投影與新設定一筆寫入。
func (s *Store) Append(ctx context.Context, id task.TaskID, expected task.Seq, b hub.Batch) error {
	if err := s.check(); err != nil {
		return err
	}
	// Go 側先驗：批次為空或第一筆不是 expected+1、或批內斷號。
	if len(b.Records) == 0 || b.Records[0].Seq != expected+1 {
		return conflictErr(id, nil)
	}
	for i := 1; i < len(b.Records); i++ {
		if b.Records[i].Seq != b.Records[i-1].Seq+1 {
			return conflictErr(id, nil)
		}
	}
	var stmts []sqlite.Statement
	for _, r := range b.Records {
		stmts = append(stmts, sqlite.Statement{
			SQL:  "INSERT INTO events(stream, seq, kind, at, payload) VALUES(?,?,?,?,?)",
			Args: []any{string(id), int64(r.Seq), string(r.Kind), formatAt(r.At), r.Payload},
		})
	}
	for _, r := range b.Redact {
		stmts = append(stmts, sqlite.Statement{
			SQL:  "UPDATE events SET payload=? WHERE stream=? AND seq=?",
			Args: []any{r.Payload, string(id), int64(r.Seq)},
		})
	}
	stmts = append(stmts, replaceViews(id, b.Views)...)
	stmts = append(stmts, insertSearch(id, b.Views.Search)...)
	stmts = append(stmts, upsertFiles(id, b.Views.Files)...)
	stmts = append(stmts, insertUsage(b.Views)...)
	turns, err := insertTurns(id, b.Views.Turns)
	if err != nil {
		return err
	}
	stmts = append(stmts, turns...)
	for _, seq := range b.Views.Unsearch {
		stmts = append(stmts, sqlite.Statement{
			SQL:  "DELETE FROM search WHERE stream=? AND seq=?",
			Args: []any{string(id), int64(seq)},
		})
	}
	if w := b.NewWorkspace; w != nil {
		stmts = append(stmts, sqlite.Statement{
			SQL:  "INSERT OR REPLACE INTO workspaces(id, path, name, policy, last_used_at) VALUES(?,?,?,?,?)",
			Args: []any{string(w.ID), w.Path, w.Name, string(w.Policy), formatAt(w.LastUsedAt)},
		})
	}
	for _, r := range b.NewRules {
		stmts = append(stmts, sqlite.Statement{
			SQL: "INSERT OR REPLACE INTO rules(id, scope, scope_id, agent, match, tool, pattern, verdict) VALUES(?,?,?,?,?,?,?,?)",
			Args: []any{string(r.ID), string(r.Scope), r.ScopeID, string(r.Agent),
				string(r.Match), r.Tool, r.Pattern, string(r.Verdict)},
		})
	}
	if _, err := s.w.Transaction(ctx, stmts); err != nil {
		if isConstraint(err) {
			return conflictErr(id, err)
		}
		return mapErr(err)
	}
	return nil
}

// replaceViews 是全集替換的投影：Task 列（有才寫）與確認列。
// Append 與 Reproject 共用。
func replaceViews(id task.TaskID, v hub.Views) []sqlite.Statement {
	var stmts []sqlite.Statement
	if t := v.Task; t != nil {
		stmts = append(stmts, sqlite.Statement{
			SQL: "INSERT OR REPLACE INTO tasks_view(id, title, workspace, status, archived, sessions, running, pending_asks, failed, updated_at, version, pinned_at, priority, labels, agents) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			Args: []any{string(t.ID), t.Title, string(t.Workspace), string(t.Status),
				boolInt(t.Archived), int64(t.Sessions), int64(t.Running),
				int64(t.PendingAsks), int64(t.Failed), formatAt(t.UpdatedAt), int64(t.Version),
				formatOptionalAt(t.PinnedAt), string(t.Priority), jsonList(t.Labels), jsonList(t.Agents)},
		})
	}
	stmts = append(stmts, sqlite.Statement{
		SQL: "DELETE FROM confirmations_view WHERE task_id=?", Args: []any{string(id)}})
	for _, x := range v.Confirmations {
		stmts = append(stmts, sqlite.Statement{
			SQL: "INSERT INTO confirmations_view(id, task_id, kind, requested_by, requested_at) VALUES(?,?,?,?,?)",
			Args: []any{string(x.ID), string(x.Task), string(x.Kind),
				string(x.RequestedBy), formatAt(x.RequestedAt)},
		})
	}
	return stmts
}

// insertSearch 新增檢索列。純 INSERT：REPLACE 的隱含 delete
// 不會觸發 ad trigger。
func insertSearch(id task.TaskID, entries []hub.SearchEntry) []sqlite.Statement {
	stmts := make([]sqlite.Statement, 0, len(entries))
	for _, e := range entries {
		stmts = append(stmts, sqlite.Statement{
			SQL:  "INSERT INTO search(stream, seq, kind, ref, agent, text) VALUES(?,?,?,?,?,?)",
			Args: []any{string(id), int64(e.Seq), string(e.Kind), e.Ref, e.Agent, e.Text},
		})
	}
	return stmts
}

// upsertFiles 把本批用過的檔案併進已存的列（hub.Views.Files）：方式、時間與位置
// 取本批的，改過則一直算改過。
func upsertFiles(id task.TaskID, files []hub.FileUse) []sqlite.Statement {
	stmts := make([]sqlite.Statement, 0, len(files))
	for _, f := range files {
		stmts = append(stmts, sqlite.Statement{
			SQL: "INSERT INTO files_view(task_id, path, outside, via, at, edited) VALUES(?,?,?,?,?,?) " +
				"ON CONFLICT(task_id, path) DO UPDATE SET outside=excluded.outside, via=excluded.via, " +
				"at=excluded.at, edited=MAX(edited, excluded.edited)",
			Args: []any{string(id), f.Path, boolInt(f.Outside), string(f.Via), formatAt(f.At), boolInt(f.Edited)},
		})
	}
	return stmts
}

// insertTurns 新增結束的回合（hub.Views.Turns），按結束的順序（rowid 跟著）。
func insertTurns(id task.TaskID, turns []hub.TurnEnd) ([]sqlite.Statement, error) {
	stmts := make([]sqlite.Statement, 0, len(turns))
	for _, t := range turns {
		problem := ""
		if t.Problem != nil {
			b, err := json.Marshal(t.Problem)
			if err != nil {
				return nil, fail(CodeFailed, err, "detail", err.Error())
			}
			problem = string(b)
		}
		stmts = append(stmts, sqlite.Statement{
			SQL: "INSERT INTO turns_view(task_id, session, turn, result, text, attachment, problem, ended_at) VALUES(?,?,?,?,?,?,?,?)",
			Args: []any{string(id), string(t.Session), string(t.Turn), string(t.Result), t.Text, t.Attachment,
				problem, formatAt(t.EndedAt)},
		})
	}
	return stmts, nil
}

// Reproject 是 Log.Reproject：一個交易整體替換一條 Stream 的投影。
func (s *Store) Reproject(ctx context.Context, id task.TaskID, v hub.Views) error {
	if err := s.check(); err != nil {
		return err
	}
	stmts := replaceViews(id, v)
	stmts = append(stmts, sqlite.Statement{
		SQL: "DELETE FROM search WHERE stream=?", Args: []any{string(id)}})
	stmts = append(stmts, insertSearch(id, v.Search)...)
	stmts = append(stmts, sqlite.Statement{
		SQL: "DELETE FROM files_view WHERE task_id=?", Args: []any{string(id)}})
	stmts = append(stmts, upsertFiles(id, v.Files)...)
	stmts = append(stmts, sqlite.Statement{
		SQL: "DELETE FROM turns_view WHERE task_id=?", Args: []any{string(id)}})
	turns, err := insertTurns(id, v.Turns)
	if err != nil {
		return err
	}
	stmts = append(stmts, turns...)
	stmts = append(stmts, insertUsage(v)...)
	if _, err := s.w.Transaction(ctx, stmts); err != nil {
		return mapErr(err)
	}
	return nil
}

// insertUsage 寫入會話歸屬與上報的用量（hub.Views.Links、Reported）：只增不刪，已有的鍵照舊
// （內容由事件決定，重寫的相同），所以 Append 與 Reproject 都用它，Delete 不碰（DL12）。
func insertUsage(v hub.Views) []sqlite.Statement {
	stmts := make([]sqlite.Statement, 0, len(v.Links)+len(v.Reported))
	for _, l := range v.Links {
		stmts = append(stmts, sqlite.Statement{
			SQL:  "INSERT OR IGNORE INTO usage_sessions(agent, native, task_id, session) VALUES(?,?,?,?)",
			Args: []any{string(l.Agent), string(l.Native), string(l.Task), string(l.Session)},
		})
	}
	for _, r := range v.Reported {
		u := r.Usage
		stmts = append(stmts, sqlite.Statement{
			SQL: "INSERT OR IGNORE INTO usage_reported(task_id, seq, agent, native, at, model, dir, input, cache_read, cache_write, output) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
			Args: []any{string(r.Task), int64(r.Seq), string(r.Agent), string(r.Native), formatAt(r.At), r.Model, r.Dir,
				u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens},
		})
	}
	return stmts
}

// ViewsVersion 是 Log.ViewsVersion：meta 表裡沒有就是 0。
func (s *Store) ViewsVersion(ctx context.Context) (int, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	rows, err := s.r.Query(ctx, "SELECT value FROM meta WHERE key='views_version'")
	if err != nil {
		return 0, mapErr(err)
	}
	if len(rows.Values) == 0 {
		return 0, nil
	}
	return int(toInt(rows.Values[0][0])), nil
}

// SetViewsVersion 是 Log.SetViewsVersion。
func (s *Store) SetViewsVersion(ctx context.Context, v int) error {
	if err := s.check(); err != nil {
		return err
	}
	if _, err := s.w.Transaction(ctx, []sqlite.Statement{{
		SQL:  "INSERT OR REPLACE INTO meta(key, value) VALUES('views_version', ?)",
		Args: []any{int64(v)}}}); err != nil {
		return mapErr(err)
	}
	return nil
}

// Read 依序回傳 Seq > from 的記錄；limit <= 0 不限筆數。
func (s *Store) Read(ctx context.Context, id task.TaskID, from task.Seq, limit int) ([]hub.Record, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	sql := "SELECT seq, kind, at, payload FROM events WHERE stream=? AND seq>? ORDER BY seq"
	args := []any{string(id), int64(from)}
	if limit > 0 {
		sql += " LIMIT ?"
		args = append(args, int64(limit))
	}
	rows, err := s.r.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]hub.Record, 0, len(rows.Values))
	for _, v := range rows.Values {
		at, err := parseAt(toStr(v[2]))
		if err != nil {
			return nil, err
		}
		out = append(out, hub.Record{
			Seq: task.Seq(toInt(v[0])), Kind: task.EventKind(toStr(v[1])),
			At: at, Payload: toBytes(v[3]),
		})
	}
	return out, nil
}

// Delete 移除一個 stream 及其所有投影。
func (s *Store) Delete(ctx context.Context, id task.TaskID) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Transaction(ctx, []sqlite.Statement{
		{SQL: "DELETE FROM events WHERE stream=?", Args: []any{string(id)}},
		{SQL: "DELETE FROM tasks_view WHERE id=?", Args: []any{string(id)}},
		{SQL: "DELETE FROM confirmations_view WHERE task_id=?", Args: []any{string(id)}},
		{SQL: "DELETE FROM search WHERE stream=?", Args: []any{string(id)}},
		{SQL: "DELETE FROM files_view WHERE task_id=?", Args: []any{string(id)}},
		{SQL: "DELETE FROM turns_view WHERE task_id=?", Args: []any{string(id)}},
	})
	if err != nil {
		return mapErr(err)
	}
	return nil
}

// Streams 回傳所有 stream ID。
func (s *Store) Streams(ctx context.Context) ([]task.TaskID, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx, "SELECT DISTINCT stream FROM events")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]task.TaskID, 0, len(rows.Values))
	for _, v := range rows.Values {
		out = append(out, task.TaskID(toStr(v[0])))
	}
	return out, nil
}
