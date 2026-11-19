package acp

// usage_opencode.go：讀 OpenCode 在本機的用量記錄（docs/architecture/runtime.md
// 「本機用量記錄」、D90）。1.14 起記錄在 SQLite（opencode.db），經 store 提供的
// 唯讀查詢讀；查這些表的 SQL 是 OpenCode 的私有格式，所以寫在這裡（規則 3）。
// 本機的庫是 1.18 的表結構但沒有用量，token 欄位取自 ccusage#966 與
// omacom/omarchy#13893 的說明，待真機核實。

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// pageRows 是一次查詢至多取的行數。
const pageRows = 2000

// OpenCodeUsage 是讀 OpenCode 本機用量記錄的 usage.Source；query 是 store 提供的
// 唯讀查詢（usage.QueryFunc，由 app 交來）。
func OpenCodeUsage(query usage.QueryFunc) usage.Source { return openCodeSource{query: query} }

type openCodeSource struct{ query usage.QueryFunc }

var _ usage.DatabaseSource = openCodeSource{}

func (openCodeSource) Agent() task.AgentID          { return openCodeID }
func (openCodeSource) Unreported() []usage.Category { return nil }
func (openCodeSource) Notes() []usage.Note          { return nil }

// Find 找 $OPENCODE_DATA_DIR（預設 $XDG_DATA_HOME/opencode，即 ~/.local/share/opencode）
// 下的 opencode.db，以及 1.14 之前的 storage/message/**/*.json。
func (openCodeSource) Find(env proc.Env) ([]string, []usage.File, error) {
	root := env.Get("OPENCODE_DATA_DIR")
	if root == "" {
		data := env.Get("XDG_DATA_HOME")
		if data == "" {
			if home := env.Home(); home != "" {
				data = filepath.Join(home, ".local", "share")
			}
		}
		if data == "" {
			return nil, nil, nil
		}
		root = filepath.Join(data, "opencode")
	}
	var files []usage.File
	db := filepath.Join(root, "opencode.db")
	if _, err := os.Stat(db); err == nil {
		files = append(files, usage.File{Path: db, Format: usage.Database, Watch: []string{db + "-wal"}})
	} else if !errors.Is(err, os.ErrNotExist) {
		return []string{root}, nil, err
	}
	legacy, err := usage.Walk(filepath.Join(root, "storage", "message"), func(path string) (usage.Format, bool) {
		return usage.Whole, strings.HasSuffix(path, ".json")
	})
	return []string{root}, append(files, legacy...), err
}

// Parser 讀 1.14 之前一則訊息一個的 JSON 檔：storage/message/<會話 ID>/msg_<ID>.json。
func (openCodeSource) Parser(f usage.File, _ []byte) (usage.Parser, error) {
	return openCodeFileParser{session: filepath.Base(filepath.Dir(f.Path))}, nil
}

type openCodeFileParser struct{ session string }

func (openCodeFileParser) State() []byte { return nil }

func (p openCodeFileParser) Line(b []byte) ([]usage.Record, []usage.Quota, bool) {
	if !usage.IsObject(b) {
		return nil, nil, true
	}
	var d openCodeData
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, nil, true
	}
	r, ok, bad := d.record(d.ID, cmp.Or(d.SessionID, p.session), "", "")
	if !ok {
		return nil, nil, bad
	}
	return []usage.Record{r}, nil, false
}

// openCodeData 是 message.data（1.x）或 session_message.data（2.x）的 JSON：1.x 的
// 角色與模型在頂層，2.x 的模型在 model 裡、角色在 type 欄。
type openCodeData struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionID"`
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Model      *struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Tokens *struct {
		Total     int64 `json:"total"`
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Time struct {
		Created int64 `json:"created"` // 毫秒
	} `json:"time"`
	Path struct {
		Cwd string `json:"cwd"`
	} `json:"path"`
}

// record 換算一則 assistant 訊息：輸入不含緩存，推理另外算進輸出；有總數時四類
// 之和必須等於它。用 claude-agent provider 的訊息不算，它們在 Claude Code 自己的
// 記錄裡（U37）。Key 是建立時間、provider 與模型：分叉出來的會話複製原會話的歷史
// 時換了訊息 ID，但建立時間不變，所以複製的只算一次（U38）；2.x 升級時按原 ID 複製
// 1.x 的訊息，同樣合成一條（U36）。
func (d openCodeData) record(id, session, dir, parent string) (usage.Record, bool, bool) {
	if d.Tokens == nil || (d.Role != "" && d.Role != "assistant") {
		return usage.Record{}, false, false
	}
	provider, model := d.ProviderID, d.ModelID
	if d.Model != nil {
		provider, model = cmp.Or(d.Model.ProviderID, provider), cmp.Or(d.Model.ID, model)
	}
	if provider == "claude-agent" {
		return usage.Record{}, false, false
	}
	t := d.Tokens
	tokens := usage.Tokens{Input: t.Input, CacheRead: t.Cache.Read, CacheWrite: t.Cache.Write, Output: t.Output + t.Reasoning}
	if !tokens.Valid() || (t.Total != 0 && tokens.Total() != t.Total) {
		return usage.Record{}, false, true
	}
	key := "id:" + id
	if d.Time.Created > 0 {
		key = fmt.Sprintf("%d|%s|%s", d.Time.Created, provider, model)
	} else if id == "" {
		return usage.Record{}, false, true
	}
	at := time.Time{}
	if d.Time.Created > 0 {
		at = time.UnixMilli(d.Time.Created).UTC()
	}
	return usage.Record{Key: key, At: at, Session: session, Parent: parent, Model: model,
		Dir: cmp.Or(dir, d.Path.Cwd), Tokens: tokens}, true, false
}

// openCodeState 是兩張訊息表各自讀到的 (time_updated, id)。
type openCodeState struct {
	Legacy mark `json:"legacy"`
	V2     mark `json:"v2"`
}

type mark struct {
	At int64  `json:"at"`
	ID string `json:"id"`
}

// ReadDatabase 按水位讀新增與更新過的訊息；每次先查 sqlite_master 認出是哪一代
// 表結構。資料庫被鎖住時 query 回 usage.ErrBusy，這一輪跳過（U45）。
func (s openCodeSource) ReadDatabase(ctx context.Context, f usage.File, state []byte) ([]usage.Record, []byte, bool, error) {
	if s.query == nil {
		return nil, state, false, errors.New("acp: no SQLite reader")
	}
	var st openCodeState
	if len(state) > 0 {
		if err := json.Unmarshal(state, &st); err != nil {
			st = openCodeState{}
		}
	}
	rows, err := s.query(ctx, f.Path, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, state, false, err
	}
	tables := map[string]bool{}
	for _, r := range rows {
		if name, ok := r[0].(string); ok {
			tables[name] = true
		}
	}
	join, err := s.sessionJoin(ctx, f.Path, tables)
	if err != nil {
		return nil, state, false, err
	}
	var recs []usage.Record
	more := false
	for _, t := range []struct {
		table string
		where string
		m     *mark
	}{
		{"message", "", &st.Legacy},
		{"session_message", "m.type = 'assistant' AND ", &st.V2},
	} {
		if !tables[t.table] {
			continue
		}
		q := fmt.Sprintf(`SELECT m.id, m.session_id, m.time_updated, m.data, %s FROM %s m %s
			WHERE %s(m.time_updated > ? OR (m.time_updated = ? AND m.id > ?))
			ORDER BY m.time_updated, m.id LIMIT %d`, join.columns, t.table, join.clause, t.where, pageRows)
		page, err := s.query(ctx, f.Path, q, t.m.At, t.m.At, t.m.ID)
		if err != nil {
			return nil, state, false, err
		}
		more = more || len(page) == pageRows
		for _, row := range page {
			id, _ := row[0].(string)
			session, _ := row[1].(string)
			updated, _ := row[2].(int64)
			data, _ := row[3].(string)
			dir, _ := row[4].(string)
			parent, _ := row[5].(string)
			t.m.At, t.m.ID = updated, id
			var d openCodeData
			if json.Unmarshal([]byte(data), &d) != nil {
				continue
			}
			if r, ok, _ := d.record(id, session, dir, parent); ok {
				recs = append(recs, r)
			}
		}
	}
	next, err := json.Marshal(st)
	if err != nil {
		return nil, state, false, err
	}
	return recs, next, more, nil
}

type sessionJoin struct{ columns, clause string }

// sessionJoin 找出會話表（2.x 早期是 session_v2）與它的 directory、parent_id 欄；
// 沒有時目錄取自訊息裡的 path.cwd。
func (s openCodeSource) sessionJoin(ctx context.Context, path string, tables map[string]bool) (sessionJoin, error) {
	none := sessionJoin{columns: "'' AS directory, '' AS parent_id"}
	table := ""
	for _, t := range []string{"session_v2", "session"} {
		if tables[t] {
			table = t
			break
		}
	}
	if table == "" {
		return none, nil
	}
	rows, err := s.query(ctx, path, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return none, err
	}
	var cols []string
	for _, r := range rows {
		if name, ok := r[0].(string); ok {
			cols = append(cols, name)
		}
	}
	if !slices.Contains(cols, "id") {
		return none, nil
	}
	dir, parent := "''", "''"
	if slices.Contains(cols, "directory") {
		dir = "COALESCE(s.directory, '')"
	}
	if slices.Contains(cols, "parent_id") {
		parent = "COALESCE(s.parent_id, '')"
	}
	return sessionJoin{columns: dir + " AS directory, " + parent + " AS parent_id",
		clause: "LEFT JOIN " + table + " s ON s.id = m.session_id"}, nil
}
