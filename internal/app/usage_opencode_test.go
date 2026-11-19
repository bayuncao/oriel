package app_test

// OpenCode 的 SQLite 記錄（D90）：runtime/acp 的 Source 經 store.QueryFile 唯讀查詢。
// 放在 app 的測試裡，因為建測試用的 SQLite 檔要用 mygo 的 SQLite 插件（規則 8）。

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"

	"github.com/bayuncao/oriel/internal/runtime/acp"
	"github.com/bayuncao/oriel/internal/store"
	"github.com/bayuncao/oriel/internal/usage"
	"github.com/bayuncao/oriel/internal/usage/usagetest"
)

var usageNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// openCodeDB 建一個 1.18 表結構（只取用到的欄）的 opencode.db；journal 是 "wal" 或 "delete"。
func openCodeDB(t *testing.T, home *usagetest.Home, journal string, stmts ...string) *sqlite.DB {
	t.Helper()
	path := filepath.Join(home.Path, ".local", "share", "opencode", "opencode.db")
	home.Write(".local/share/opencode/.keep", nil)
	ctx := context.Background()
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Query(ctx, "PRAGMA journal_mode="+journal); err != nil {
		t.Fatal(err)
	}
	all := append([]string{
		"CREATE TABLE session(id TEXT PRIMARY KEY, project_id TEXT NOT NULL, parent_id TEXT, directory TEXT NOT NULL, title TEXT NOT NULL)",
		"CREATE TABLE message(id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)",
		"CREATE TABLE session_message(id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL, seq INTEGER NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)",
		"INSERT INTO session VALUES('ses_1','p','','/work/o','t'), ('ses_2','p','ses_1','/work/o','fork')",
	}, stmts...)
	var batch []sqlite.Statement
	for _, s := range all {
		batch = append(batch, sqlite.Statement{SQL: s})
	}
	if _, err := db.Transaction(ctx, batch); err != nil {
		t.Fatal(err)
	}
	return db
}

func legacyRow(id, session string, created int64, provider string, in, out, reasoning, read, write int64) string {
	data := fmt.Sprintf(`{"role":"assistant","modelID":"m-1","providerID":%q,"cost":0,"tokens":{"total":%d,"input":%d,"output":%d,"reasoning":%d,"cache":{"read":%d,"write":%d}},"time":{"created":%d}}`,
		provider, in+out+reasoning+read+write, in, out, reasoning, read, write, created)
	return fmt.Sprintf("INSERT INTO message VALUES('%s', '%s', %d, %d, '%s')", id, session, created, created, data)
}

func v2Row(id, session string, seq int, created int64, in, out, reasoning, read, write int64) string {
	data := fmt.Sprintf(`{"model":{"id":"m-1","providerID":"anthropic","variant":"high"},"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":%d,"write":%d}},"time":{"created":%d}}`,
		in, out, reasoning, read, write, created)
	return fmt.Sprintf("INSERT INTO session_message VALUES('%s', '%s', 'assistant', %d, %d, %d, '%s')", id, session, seq, created, created, data)
}

const created = 1793091600000

func TestOpenCodeUsage(t *testing.T) {
	src := acp.OpenCodeUsage(store.QueryFile)
	want := usage.Tokens{Input: 100, CacheRead: 900, CacheWrite: 50, Output: 50}
	cases := []struct {
		name  string
		rows  []string
		total int64
	}{
		// U35：tokens 換算成四類，總數 1,100。
		{"U35 one message", []string{legacyRow("msg_1", "ses_1", created, "anthropic", 100, 40, 10, 900, 50)}, 1100},
		// U36：2.x 按原 ID 把 1.x 的訊息複製進新表：只算一次。
		{"U36 copied into v2", []string{legacyRow("msg_1", "ses_1", created, "anthropic", 100, 40, 10, 900, 50),
			v2Row("msg_1", "ses_1", 1, created, 100, 40, 10, 900, 50)}, 1100},
		// U37：claude-agent provider 的不算（它在 Claude Code 自己的記錄裡）。
		{"U37 claude-agent", []string{legacyRow("msg_1", "ses_1", created, "anthropic", 100, 40, 10, 900, 50),
			legacyRow("msg_2", "ses_1", created+5, "claude-agent", 7, 7, 0, 0, 0)}, 1100},
		// U38：分叉出來的會話換了訊息 ID 複製原會話的歷史：複製的只算一次，它自己的照算。
		{"U38 fork", []string{legacyRow("msg_1", "ses_1", created, "anthropic", 100, 40, 10, 900, 50),
			legacyRow("msg_9", "ses_2", created, "anthropic", 100, 40, 10, 900, 50),
			legacyRow("msg_10", "ses_2", created+60000, "anthropic", 1, 1, 0, 0, 0)}, 1102},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := usagetest.NewHome(t)
			// U35：庫正被另一個連線以 WAL 模式開著。
			openCodeDB(t, home, "wal", c.rows...)
			got := usagetest.NewReader(t, src, home, usageNow).Read()
			if got.Total.Total() != c.total || got.Source.Problem != nil {
				t.Fatalf("total %+v problem %+v, want %d", got.Total, got.Source.Problem, c.total)
			}
			if c.total == 1100 && got.Total != want {
				t.Fatalf("total %+v, want %+v", got.Total, want)
			}
			if got.Dirs["/work/o"] != c.total {
				t.Fatalf("dirs %v", got.Dirs)
			}
		})
	}
}

// 按水位只讀新增與更新過的訊息。
func TestOpenCodeReadsNewRows(t *testing.T) {
	home := usagetest.NewHome(t)
	db := openCodeDB(t, home, "wal", legacyRow("msg_1", "ses_1", created, "anthropic", 100, 40, 10, 900, 50))
	r := usagetest.NewReader(t, acp.OpenCodeUsage(store.QueryFile), home, usageNow)
	r.Read()
	if _, err := db.Execute(context.Background(), v2Row("msg_2", "ses_1", 2, created+1000, 1, 1, 0, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if got := r.Read(); got.Total.Total() != 1102 {
		t.Fatalf("total %+v", got.Total)
	}
}

// U45：庫被另一個連線一直拿著寫鎖：這一輪跳過、不報錯；放開後下一輪讀到。
func TestOpenCodeLockedDatabase(t *testing.T) {
	home := usagetest.NewHome(t)
	db := openCodeDB(t, home, "delete", legacyRow("msg_1", "ses_1", created, "anthropic", 100, 40, 10, 900, 50))
	ctx := context.Background()
	if _, err := db.Query(ctx, "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	// 排他模式下的寫入取得寫鎖後一直拿著，直到連線關閉。
	if _, err := db.Execute(ctx, "UPDATE session SET title='x'"); err != nil {
		t.Fatal(err)
	}
	r := usagetest.NewReader(t, acp.OpenCodeUsage(store.QueryFile), home, usageNow)
	if got := r.Read(); got.Total.Total() != 0 || got.Source.Problem != nil {
		t.Fatalf("locked: total %+v problem %+v", got.Total, got.Source.Problem)
	}
	db.Close()
	if got := r.Read(); got.Total.Total() != 1100 {
		t.Fatalf("after unlock: total %+v", got.Total)
	}
}
