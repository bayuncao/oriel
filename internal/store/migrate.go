package store

import (
	"context"
	"fmt"

	"github.com/egoist/mygo/plugins/sqlite"
)

// migrations 是各版本的語句清單；索引+1 即版本號。
// 每個版本在一個 Transaction 裡跑完再寫 user_version。
var migrations = [][]string{
	{ // v1
		`CREATE TABLE events(stream TEXT NOT NULL, seq INTEGER NOT NULL, kind TEXT NOT NULL, at TEXT NOT NULL, payload BLOB NOT NULL, PRIMARY KEY(stream, seq)) WITHOUT ROWID`,
		`CREATE TRIGGER events_contiguous BEFORE INSERT ON events
		 WHEN NEW.seq != (SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE stream = NEW.stream)
		 BEGIN SELECT RAISE(ABORT, 'events: seq gap'); END`,
		`CREATE TABLE tasks_view(id TEXT PRIMARY KEY, title TEXT NOT NULL, workspace TEXT NOT NULL, status TEXT NOT NULL, archived INTEGER NOT NULL, sessions INTEGER NOT NULL, running INTEGER NOT NULL, pending_asks INTEGER NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE sessions_view(id TEXT NOT NULL, task_id TEXT NOT NULL, pos INTEGER NOT NULL, agent TEXT NOT NULL, profile TEXT NOT NULL, role TEXT NOT NULL, status TEXT NOT NULL, updated_at TEXT NOT NULL, PRIMARY KEY(task_id, id))`,
		`CREATE TABLE confirmations_view(id TEXT NOT NULL, task_id TEXT NOT NULL, kind TEXT NOT NULL, requested_by TEXT NOT NULL, requested_at TEXT NOT NULL, PRIMARY KEY(task_id, id))`,
		`CREATE TABLE search(id INTEGER PRIMARY KEY, stream TEXT NOT NULL, seq INTEGER NOT NULL, text TEXT NOT NULL, UNIQUE(stream, seq))`,
		`CREATE VIRTUAL TABLE search_fts USING fts5(text, content='search', content_rowid='id', tokenize='trigram')`,
		`CREATE TRIGGER search_ai AFTER INSERT ON search BEGIN INSERT INTO search_fts(rowid, text) VALUES (new.id, new.text); END`,
		`CREATE TRIGGER search_ad AFTER DELETE ON search BEGIN INSERT INTO search_fts(search_fts, rowid, text) VALUES ('delete', old.id, old.text); END`,
		`CREATE TABLE workspaces(id TEXT PRIMARY KEY, path TEXT NOT NULL, name TEXT NOT NULL, policy TEXT NOT NULL, last_used_at TEXT NOT NULL)`,
		`CREATE INDEX workspaces_path ON workspaces(path)`,
		`CREATE TABLE rules(id TEXT PRIMARY KEY, scope TEXT NOT NULL, scope_id TEXT NOT NULL, agent TEXT NOT NULL, match TEXT NOT NULL, tool TEXT NOT NULL, pattern TEXT NOT NULL, verdict TEXT NOT NULL)`,
		`CREATE TABLE settings(key TEXT PRIMARY KEY, value_json TEXT NOT NULL)`,
	},
	{ // v2：Task 列的掛起操作數（D71）與投影版本（hub 據此重建投影）
		`ALTER TABLE tasks_view ADD COLUMN failed INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE meta(key TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
		// 本機用量（usage.Store，docs/architecture/store.md）。開發階段直接加在最新的
		// 版本裡，不另開遷移版本（ADR-36）：這之前建的開發庫沒有這三張表，要刪掉重建。
		`CREATE TABLE usage_records(agent TEXT NOT NULL, key TEXT NOT NULL, at TEXT NOT NULL, session TEXT NOT NULL, parent TEXT NOT NULL, model TEXT NOT NULL, dir TEXT NOT NULL, input INTEGER NOT NULL, cache_read INTEGER NOT NULL, cache_write INTEGER NOT NULL, output INTEGER NOT NULL, PRIMARY KEY(agent, key)) WITHOUT ROWID`,
		`CREATE INDEX usage_records_at ON usage_records(at)`,
		`CREATE TABLE usage_files(path TEXT PRIMARY KEY, agent TEXT NOT NULL, size INTEGER NOT NULL, mod_time TEXT NOT NULL, head TEXT NOT NULL, read_to INTEGER NOT NULL, state BLOB, bad INTEGER NOT NULL, done INTEGER NOT NULL)`,
		`CREATE TABLE usage_quotas(agent TEXT NOT NULL, bucket TEXT NOT NULL, name TEXT NOT NULL, plan TEXT NOT NULL, windows TEXT NOT NULL, at TEXT NOT NULL, PRIMARY KEY(agent, bucket))`,
	},
	{ // v3：用過的檔案（D107）與 Task 列的 Version。staging 已發布，所以另開版本；兩者都由 hub 的投影重建（viewsVersion 3）填入
		`ALTER TABLE tasks_view ADD COLUMN version INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE files_view(task_id TEXT NOT NULL, path TEXT NOT NULL, outside INTEGER NOT NULL, via TEXT NOT NULL, at TEXT NOT NULL, edited INTEGER NOT NULL, PRIMARY KEY(task_id, path)) WITHOUT ROWID`,
	},
	{ // v4：Task 列的置頂時間（D109），空字串 = 未置頂；由 hub 的投影重建（viewsVersion 5）填入
		`ALTER TABLE tasks_view ADD COLUMN pinned_at TEXT NOT NULL DEFAULT ''`,
	},
	{ // v5：結束的回合（D115），problem 是 JSON（沒有時是空字串）；rowid 是結束的順序；由 hub 的投影重建（viewsVersion 6）填入
		`CREATE TABLE turns_view(task_id TEXT NOT NULL, session TEXT NOT NULL, turn TEXT NOT NULL, result TEXT NOT NULL, text TEXT NOT NULL, attachment TEXT NOT NULL, problem TEXT NOT NULL, ended_at TEXT NOT NULL, UNIQUE(task_id, session, turn))`,
		`CREATE INDEX turns_view_ended_at ON turns_view(ended_at)`,
	},
	{ // v6：Provider keys 的記錄（provider.KeyStore，D132）；鑰匙本身在系統鑰匙串，不進資料庫
		`CREATE TABLE provider_keys(provider TEXT PRIMARY KEY, last4 TEXT NOT NULL, status TEXT NOT NULL, checked_at TEXT NOT NULL, detail TEXT NOT NULL)`,
	},
	{ // v7：鑰匙有名稱與廠商圖標，同一家可以存好幾把，不再驗證（D134）。舊記錄以廠商為 ID（鑰匙串裡的名字不變），
		// 名稱是廠商名，圖標是廠商（xAI 不在圖標清單裡，留空），添加時間取上次測試的時間。
		`CREATE TABLE provider_keys_v7(id TEXT PRIMARY KEY, name TEXT NOT NULL, icon TEXT NOT NULL, last4 TEXT NOT NULL, added_at TEXT NOT NULL)`,
		`INSERT INTO provider_keys_v7(id, name, icon, last4, added_at) SELECT provider,
			CASE provider WHEN 'anthropic' THEN 'Anthropic' WHEN 'openai' THEN 'OpenAI' WHEN 'google' THEN 'Google AI' WHEN 'mistral' THEN 'Mistral' WHEN 'xai' THEN 'xAI' ELSE provider END,
			CASE provider WHEN 'xai' THEN '' ELSE provider END,
			last4, checked_at FROM provider_keys`,
		`DROP TABLE provider_keys`,
		`ALTER TABLE provider_keys_v7 RENAME TO provider_keys`,
	},
	{ // v8：標籤目錄與 Task 列的優先級、標籤、agent（D135）；labels、agents 是 JSON 陣列，由 hub 的投影重建（viewsVersion 7）填入
		`CREATE TABLE labels(id TEXT PRIMARY KEY, name TEXT NOT NULL, tone TEXT NOT NULL)`,
		`ALTER TABLE tasks_view ADD COLUMN priority TEXT NOT NULL DEFAULT 'none'`,
		`ALTER TABLE tasks_view ADD COLUMN labels TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE tasks_view ADD COLUMN agents TEXT NOT NULL DEFAULT '[]'`,
	},
	{ // v9：鑰匙的 base URL（D136）；空字串 = 廠商的官方地址，舊記錄都是空的（PK35）
		`ALTER TABLE provider_keys ADD COLUMN base_url TEXT NOT NULL DEFAULT ''`,
	},
	{ // v10：檢索列的來源、命中的訊息與回答的 agent 名稱（D70）；由 hub 的投影重建（viewsVersion 8）填入
		`ALTER TABLE search ADD COLUMN kind TEXT NOT NULL DEFAULT 'label'`,
		`ALTER TABLE search ADD COLUMN ref TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE search ADD COLUMN agent TEXT NOT NULL DEFAULT ''`,
	},
	{ // v11：OrielHub 跑的原生會話屬於哪個 Task、agent 上報的用量（m2.md 第 33 節）；由 hub 隨事件寫入，
		// 投影重建（viewsVersion 9）時補齊，刪除 Task 時不刪（DL12）
		`CREATE TABLE usage_sessions(agent TEXT NOT NULL, native TEXT NOT NULL, task_id TEXT NOT NULL, session TEXT NOT NULL, PRIMARY KEY(agent, native)) WITHOUT ROWID`,
		`CREATE TABLE usage_reported(task_id TEXT NOT NULL, seq INTEGER NOT NULL, agent TEXT NOT NULL, native TEXT NOT NULL, at TEXT NOT NULL, model TEXT NOT NULL, dir TEXT NOT NULL, input INTEGER NOT NULL, cache_read INTEGER NOT NULL, cache_write INTEGER NOT NULL, output INTEGER NOT NULL, PRIMARY KEY(task_id, seq)) WITHOUT ROWID`,
		`CREATE INDEX usage_reported_at ON usage_reported(at)`,
		`CREATE INDEX usage_records_session ON usage_records(agent, session)`,
	},
	{ // v12：配對過的遠程設備（D145）；key 是手機 Noise 公鑰的 base64url，last_seen 從沒連過是空字串
		`CREATE TABLE devices(id TEXT PRIMARY KEY, name TEXT NOT NULL, key TEXT NOT NULL UNIQUE, paired_at TEXT NOT NULL, last_seen TEXT NOT NULL)`,
	},
	{ // v13：Session 列的投影沒有讀者（Task 列已帶 Session 數、運行數與 agent），刪掉
		`DROP TABLE sessions_view`,
	},
}

// migrate 依序套用比 user_version 新的版本。
func (s *Store) migrate(ctx context.Context) error {
	rows, err := s.w.Query(ctx, "PRAGMA user_version")
	if err != nil {
		return fail(CodeOpenFailed, err, "detail", err.Error())
	}
	var v int64
	if len(rows.Values) > 0 {
		v = toInt(rows.Values[0][0])
	}
	if v > int64(len(migrations)) {
		return fail(CodeNewerSchema, nil, "version", fmt.Sprint(v))
	}
	for i := int(v); i < len(migrations); i++ {
		stmts := make([]sqlite.Statement, 0, len(migrations[i])+1)
		for _, sql := range migrations[i] {
			stmts = append(stmts, sqlite.Statement{SQL: sql})
		}
		// PRAGMA 不接受綁定參數。
		stmts = append(stmts, sqlite.Statement{
			SQL: fmt.Sprintf("PRAGMA user_version = %d", i+1)})
		if _, err := s.w.Transaction(ctx, stmts); err != nil {
			return fail(CodeOpenFailed, err, "detail", err.Error())
		}
	}
	return nil
}
