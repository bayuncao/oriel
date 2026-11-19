package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
)

// confirmationCount 是 Task 列的待確認數：由 confirmations_view 推出，不另存一欄。
const confirmationCount = "(SELECT COUNT(*) FROM confirmations_view c WHERE c.task_id = tasks_view.id)"

// taskColumns 是 Task 列的欄位，taskRow 依這個順序讀。
const taskColumns = "id, title, workspace, status, archived, sessions, running, pending_asks, updated_at, " + confirmationCount + ", failed, version, pinned_at, priority, labels, agents"

// taskRow 從 taskColumns 的一列還原 Task 列。
func taskRow(r []any) (hub.TaskView, error) {
	t := hub.TaskView{
		ID: task.TaskID(toStr(r[0])), Title: toStr(r[1]),
		Workspace: task.WorkspaceID(toStr(r[2])), Status: task.TaskStatus(toStr(r[3])),
		Archived: toInt(r[4]) != 0, Sessions: int(toInt(r[5])),
		Running: int(toInt(r[6])), PendingAsks: int(toInt(r[7])),
		Confirmations: int(toInt(r[9])), Failed: int(toInt(r[10])),
		Version: task.Seq(toInt(r[11])), Priority: task.TaskPriority(toStr(r[13])),
	}
	var err error
	if t.UpdatedAt, err = parseAt(toStr(r[8])); err != nil {
		return t, err
	}
	if t.PinnedAt, err = parseOptionalAt(toStr(r[12])); err != nil {
		return t, err
	}
	if t.Labels, err = parseList[task.LabelID](toStr(r[14])); err != nil {
		return t, err
	}
	if t.Agents, err = parseList[hub.TaskAgent](toStr(r[15])); err != nil {
		return t, err
	}
	return t, nil
}

// jsonList 把切片存成 JSON 陣列；nil 存成 []。
func jsonList[T any](xs []T) string {
	if xs == nil {
		return "[]"
	}
	b, err := json.Marshal(xs)
	if err != nil {
		panic("store: encode list: " + err.Error()) // 只有字串與純字串的結構，不會失敗
	}
	return string(b)
}

// parseList 是 jsonList 的反向，回傳非 nil 的切片。
func parseList[T any](s string) ([]T, error) {
	out := []T{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fail(CodeFailed, err, "detail", err.Error())
	}
	if out == nil {
		out = []T{}
	}
	return out, nil
}

// ViewsOf 回傳某個 stream 最新的投影；沒有該 stream 是零值。
func (s *Store) ViewsOf(ctx context.Context, id task.TaskID) (hub.Views, error) {
	var v hub.Views
	if err := s.check(); err != nil {
		return v, err
	}
	rows, err := s.r.Query(ctx, "SELECT "+taskColumns+" FROM tasks_view WHERE id=?", string(id))
	if err != nil {
		return v, mapErr(err)
	}
	if len(rows.Values) > 0 {
		t, err := taskRow(rows.Values[0])
		if err != nil {
			return v, err
		}
		v.Task = &t
	}
	v.Confirmations = []hub.ConfirmationView{}
	cr, err := s.r.Query(ctx,
		"SELECT id, kind, requested_by, requested_at FROM confirmations_view WHERE task_id=? ORDER BY id",
		string(id))
	if err != nil {
		return v, mapErr(err)
	}
	for _, r := range cr.Values {
		v.Confirmations = append(v.Confirmations, hub.ConfirmationView{
			ID: task.ConfirmationID(toStr(r[0])), Task: id,
			Kind: task.CommandKind(toStr(r[1])), RequestedBy: task.DeviceID(toStr(r[2]))})
		at, err := parseAt(toStr(r[3]))
		if err != nil {
			return v, err
		}
		v.Confirmations[len(v.Confirmations)-1].RequestedAt = at
	}
	fr, err := s.r.Query(ctx,
		"SELECT seq, kind, ref, agent, text FROM search WHERE stream=? ORDER BY seq", string(id))
	if err != nil {
		return v, mapErr(err)
	}
	for _, r := range fr.Values {
		v.Search = append(v.Search, hub.SearchEntry{
			Seq: task.Seq(toInt(r[0])), Kind: hub.SearchKind(toStr(r[1])),
			Ref: toStr(r[2]), Agent: toStr(r[3]), Text: toStr(r[4])})
	}
	frows, err := s.r.Query(ctx, "SELECT path, outside, via, at, edited FROM files_view WHERE task_id=? ORDER BY path", string(id))
	if err != nil {
		return v, mapErr(err)
	}
	for _, r := range frows.Values {
		u, err := fileUse(r)
		if err != nil {
			return v, err
		}
		v.Files = append(v.Files, u)
	}
	trows, err := s.r.Query(ctx, "SELECT session, turn, result, text, attachment, problem, ended_at FROM turns_view WHERE task_id=? ORDER BY rowid", string(id))
	if err != nil {
		return v, mapErr(err)
	}
	for _, r := range trows.Values {
		t, err := turnEnd(r)
		if err != nil {
			return v, err
		}
		v.Turns = append(v.Turns, t)
	}
	return v, nil
}

// Recap 是 Index.Recap：未歸檔的 Task 在 since 之後（含）結束的回合，連同 Task 的標題與 Workspace
// （在 SQL 裡篩，走 ended_at 的索引）。
func (s *Store) Recap(ctx context.Context, since time.Time) ([]hub.RecapTurn, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx, "SELECT r.task_id, t.title, t.workspace, r.session, r.turn, r.result, r.text, r.attachment, r.problem, r.ended_at "+
		"FROM turns_view r JOIN tasks_view t ON t.id = r.task_id WHERE r.ended_at >= ? AND t.archived = 0", formatAt(since))
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]hub.RecapTurn, 0, len(rows.Values))
	for _, r := range rows.Values {
		t, err := turnEnd(r[3:])
		if err != nil {
			return nil, err
		}
		out = append(out, hub.RecapTurn{Task: task.TaskID(toStr(r[0])), TaskTitle: toStr(r[1]),
			Workspace: task.WorkspaceID(toStr(r[2])), TurnEnd: t})
	}
	return out, nil
}

// turnEnd 讀一列 session, turn, result, text, attachment, problem, ended_at。
func turnEnd(r []any) (hub.TurnEnd, error) {
	at, err := parseAt(toStr(r[6]))
	if err != nil {
		return hub.TurnEnd{}, err
	}
	t := hub.TurnEnd{Session: task.SessionID(toStr(r[0])), Turn: task.TurnID(toStr(r[1])),
		Result: task.TurnResult(toStr(r[2])), Text: toStr(r[3]), Attachment: toStr(r[4]), EndedAt: at}
	if p := toStr(r[5]); p != "" {
		t.Problem = new(task.Problem)
		if err := json.Unmarshal([]byte(p), t.Problem); err != nil {
			return hub.TurnEnd{}, fail(CodeFailed, err, "detail", err.Error())
		}
	}
	return t, nil
}

// Files 是 Index.Files：未歸檔的 Task 用過的檔案，連同 Task 的 Workspace（在 SQL 裡篩，
// 不讀已歸檔的列）。
func (s *Store) Files(ctx context.Context) ([]hub.TaskFile, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx, "SELECT f.task_id, t.workspace, f.path, f.outside, f.via, f.at, f.edited "+
		"FROM files_view f JOIN tasks_view t ON t.id = f.task_id WHERE t.archived = 0")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]hub.TaskFile, 0, len(rows.Values))
	for _, r := range rows.Values {
		u, err := fileUse(r[2:])
		if err != nil {
			return nil, err
		}
		out = append(out, hub.TaskFile{Task: task.TaskID(toStr(r[0])), Workspace: task.WorkspaceID(toStr(r[1])), Use: u})
	}
	return out, nil
}

// fileUse 讀一列 path, outside, via, at, edited。
func fileUse(r []any) (hub.FileUse, error) {
	at, err := parseAt(toStr(r[3]))
	if err != nil {
		return hub.FileUse{}, err
	}
	return hub.FileUse{Path: toStr(r[0]), Outside: toInt(r[1]) != 0, Via: hub.FileVia(toStr(r[2])),
		At: at, Edited: toInt(r[4]) != 0}, nil
}

// Confirmations 回傳全部 Task 的待確認，按請求時間、Task、ID 升冪。
func (s *Store) Confirmations(ctx context.Context) ([]hub.ConfirmationView, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx,
		"SELECT id, task_id, kind, requested_by, requested_at FROM confirmations_view ORDER BY requested_at ASC, task_id ASC, id ASC")
	if err != nil {
		return nil, mapErr(err)
	}
	out := []hub.ConfirmationView{}
	for _, r := range rows.Values {
		v := hub.ConfirmationView{
			ID: task.ConfirmationID(toStr(r[0])), Task: task.TaskID(toStr(r[1])),
			Kind: task.CommandKind(toStr(r[2])), RequestedBy: task.DeviceID(toStr(r[3]))}
		at, err := parseAt(toStr(r[4]))
		if err != nil {
			return nil, err
		}
		v.RequestedAt = at
		out = append(out, v)
	}
	return out, nil
}

// Search：每個詞都是 text 的子字串，僅 ASCII 大小寫不敏感。
// 每個詞配兩個 LIKE：f.text 讓 trigram 索引剪枝，s.text 以核心
// LIKE 執行精確語義（FTS 欄位會折 Unicode，核心 LIKE 只折 ASCII，
// 兩者都要才不受索引啟用與否影響）。% _ \ 以 ESCAPE 跳脫，
// 不開放 FTS 語法。limit 由呼叫方保證大於 0。先是標籤命中，再是訊息命中；
// 訊息命中每個 Task 最多 hub.SearchMessagesPerTask 條、取最新的（D70），在 limit 之前篩。
func (s *Store) Search(ctx context.Context, q string, limit int) ([]hub.SearchHit, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	var args []any
	for _, t := range hub.SearchTerms(q) {
		var b strings.Builder
		for _, c := range t {
			if c == '\\' || c == '%' || c == '_' {
				b.WriteByte('\\')
			}
			b.WriteRune(c)
		}
		esc := "%" + b.String() + "%"
		args = append(args, esc, esc)
	}
	terms := len(args) / 2
	if terms == 0 {
		return nil, nil
	}
	sql := "SELECT stream, seq, kind, ref, agent, text FROM (" +
		"SELECT s.stream, s.seq, s.kind, s.ref, s.agent, s.text, v.updated_at, " +
		"ROW_NUMBER() OVER (PARTITION BY s.stream, s.kind = 'label' ORDER BY s.seq DESC) AS n " +
		"FROM search_fts f JOIN search s ON s.id = f.rowid " +
		"JOIN tasks_view v ON v.id = s.stream WHERE " +
		strings.TrimSuffix(strings.Repeat(
			"f.text LIKE ? ESCAPE '\\' AND s.text LIKE ? ESCAPE '\\' AND ",
			terms), " AND ") +
		") WHERE kind = 'label' OR n <= ? " +
		"ORDER BY kind <> 'label', updated_at DESC, stream, seq DESC LIMIT ?"
	args = append(args, int64(hub.SearchMessagesPerTask), int64(limit))
	rows, err := s.r.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	var out []hub.SearchHit
	for _, r := range rows.Values {
		out = append(out, hub.SearchHit{
			Task: task.TaskID(toStr(r[0])), Seq: task.Seq(toInt(r[1])),
			Kind: hub.SearchKind(toStr(r[2])), Ref: toStr(r[3]), Agent: toStr(r[4]), Text: toStr(r[5])})
	}
	return out, nil
}

// TaskViews 回傳所有 Task 列，新的在前。
func (s *Store) TaskViews(ctx context.Context) ([]hub.TaskView, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx, "SELECT "+taskColumns+" FROM tasks_view ORDER BY updated_at DESC, id ASC")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]hub.TaskView, 0, len(rows.Values))
	for _, r := range rows.Values {
		t, err := taskRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}
