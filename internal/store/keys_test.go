package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/provider"
	"github.com/egoist/mygo/plugins/sqlite"
)

// provider.KeyStore 的一致性測試：記憶體與 SQLite 兩種實現行為相同。
func TestKeyStore(t *testing.T) {
	for name, s := range map[string]provider.KeyStore{"mem": provider.NewMemKeyStore(), "sqlite": openStore(t)} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			at := time.Date(2026, 11, 13, 9, 30, 0, 123_000_000, time.UTC)
			// 加入的順序與添加時間相反，讀出時依添加時間排（PK03）；同一時間依 ID。
			late := provider.StoredKey{ID: "c", Name: "Personal", Icon: "openai", Last4: "9c01", AddedAt: at.Add(time.Minute)}
			b := provider.StoredKey{ID: "b", Name: "Work", Icon: "anthropic", Last4: "4f2a", AddedAt: at}
			a := provider.StoredKey{ID: "a", Name: "Work", Icon: "anthropic", Last4: "h7Qd", AddedAt: at, BaseURL: "https://proxy.example.com/v1"}
			gone := provider.StoredKey{ID: "d", Name: "Old", Icon: "", Last4: "", AddedAt: at}
			for _, k := range []provider.StoredKey{late, b, a, gone} {
				if err := s.PutKey(ctx, k); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.DeleteKey(ctx, "d"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteKey(ctx, "missing"); err != nil {
				t.Fatalf("deleting a missing record: %v", err)
			}
			got, err := s.Keys(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for i := range got {
				got[i].AddedAt = got[i].AddedAt.UTC()
			}
			if want := []provider.StoredKey{a, b, late}; !reflect.DeepEqual(got, want) {
				t.Errorf("keys = %+v\nwant %+v", got, want)
			}
			// PK28、PK29、PK32：只換掉一把的 base URL（改或清空），其他欄位與順序不變；沒有這一把時回 false。
			if found, err := s.SetKeyBaseURL(ctx, "missing", "https://x.com"); err != nil || found {
				t.Fatalf("SetKeyBaseURL(missing) = %v, %v", found, err)
			}
			for _, base := range []string{"http://localhost:4000", ""} {
				changed := a
				changed.BaseURL = base
				if found, err := s.SetKeyBaseURL(ctx, a.ID, base); err != nil || !found {
					t.Fatalf("SetKeyBaseURL(%q) = %v, %v", base, found, err)
				}
				got, err := s.Keys(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for i := range got {
					got[i].AddedAt = got[i].AddedAt.UTC()
				}
				if want := []provider.StoredKey{changed, b, late}; !reflect.DeepEqual(got, want) {
					t.Errorf("after setting %q: %+v\nwant %+v", base, got, want)
				}
			}
		})
	}
}

// v6 的鑰匙記錄升到 v7（D134）：以廠商為 ID（鑰匙串裡的名字不變），名稱是廠商名，圖標是廠商，
// xAI 不在圖標清單裡留空；添加時間取上次測試的時間。
func TestMigrateKeysFromV6(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	rw, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stmts := []sqlite.Statement{}
	for _, m := range migrations[:6] {
		for _, q := range m {
			stmts = append(stmts, sqlite.Statement{SQL: q})
		}
	}
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	stmts = append(stmts,
		sqlite.Statement{SQL: "INSERT INTO provider_keys(provider, last4, status, checked_at, detail) VALUES('anthropic','4f2a','connected',?,'')", Args: []any{formatAt(at)}},
		sqlite.Statement{SQL: "INSERT INTO provider_keys(provider, last4, status, checked_at, detail) VALUES('xai','9c01','invalid',?,'bad key')", Args: []any{formatAt(at.Add(time.Minute))}},
		sqlite.Statement{SQL: "PRAGMA user_version = 6"})
	if _, err := rw.Transaction(ctx, stmts); err != nil {
		t.Fatal(err)
	}
	rw.Close()

	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Keys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		got[i].AddedAt = got[i].AddedAt.UTC()
	}
	want := []provider.StoredKey{
		{ID: "anthropic", Name: "Anthropic", Icon: "anthropic", Last4: "4f2a", AddedAt: at},
		{ID: "xai", Name: "xAI", Icon: "", Last4: "9c01", AddedAt: at.Add(time.Minute)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %+v\nwant %+v", got, want)
	}
}

// PK35：v8 的鑰匙記錄升到 v9（D136），每把照舊在，base URL 是空的。
func TestMigrateKeysFromV8(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	rw, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stmts := []sqlite.Statement{}
	for _, m := range migrations[:8] {
		for _, q := range m {
			stmts = append(stmts, sqlite.Statement{SQL: q})
		}
	}
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	stmts = append(stmts,
		sqlite.Statement{SQL: "INSERT INTO provider_keys(id, name, icon, last4, added_at) VALUES('k1','Work','openai','4f2a',?)", Args: []any{formatAt(at)}},
		sqlite.Statement{SQL: "PRAGMA user_version = 8"})
	if _, err := rw.Transaction(ctx, stmts); err != nil {
		t.Fatal(err)
	}
	rw.Close()

	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Keys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		got[i].AddedAt = got[i].AddedAt.UTC()
	}
	want := []provider.StoredKey{{ID: "k1", Name: "Work", Icon: "openai", Last4: "4f2a", AddedAt: at, BaseURL: ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %+v\nwant %+v", got, want)
	}
}
