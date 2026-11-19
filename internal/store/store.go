package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/egoist/mygo/plugins/sqlite"
)

// Options 是 Open 的參數。
type Options struct {
	// BusyTimeout 是鎖等待時間；0 用外掛預設（5 秒）。
	BusyTimeout time.Duration
}

// Store 是 SQLite 的 hub.Log + hub.Config 實現。
// 兩條連線：寫連線給 Append/Delete/寫 Config，唯讀連線給查詢
// （讀寫經 WAL 並行；新檔案直接 ReadOnly 可開）。
type Store struct {
	w      *sqlite.DB
	r      *sqlite.DB
	path   string
	closed atomic.Bool
}

var _ hub.Log = (*Store)(nil)
var _ hub.Config = (*Store)(nil)

// Open 打開（或建立）一個資料庫。
func Open(ctx context.Context, path string, o Options) (*Store, error) {
	w, err := sqlite.Open(ctx, filepath.Clean(path), sqlite.OpenOptions{
		BusyTimeout: o.BusyTimeout})
	if err != nil {
		return nil, fail(CodeOpenFailed, err, "detail", err.Error())
	}
	s := &Store{w: w, path: filepath.Clean(path)}
	if err := s.init(ctx, o); err != nil {
		w.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(ctx context.Context, o Options) error {
	rows, err := s.w.Query(ctx, "PRAGMA journal_mode=WAL")
	if err != nil {
		return fail(CodeOpenFailed, err, "detail", err.Error())
	}
	if len(rows.Values) == 0 || rows.Values[0][0] != "wal" {
		return fail(CodeOpenFailed, nil, "detail", "journal_mode is not wal")
	}
	if _, err := s.w.Execute(ctx, "PRAGMA synchronous=NORMAL"); err != nil {
		return fail(CodeOpenFailed, err, "detail", err.Error())
	}
	if err := s.migrate(ctx); err != nil {
		return err
	}
	r, err := sqlite.Open(ctx, s.path, sqlite.OpenOptions{
		ReadOnly: true, BusyTimeout: o.BusyTimeout})
	if err != nil {
		return fail(CodeOpenFailed, err, "detail", err.Error())
	}
	s.r = r
	return nil
}

// Close 關閉兩條連線；重複呼叫無害。
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	var err error
	if s.r != nil {
		err = s.r.Close()
	}
	if e := s.w.Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// check 是每個公開方法的前置：關閉後一律 store.closed。
func (s *Store) check() error {
	if s.closed.Load() {
		return fail(CodeClosed, nil)
	}
	return nil
}

// mapErr 把 sqlite/ctx 錯誤映射成 store 錯誤；ctx 錯誤原樣回傳。
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sqlite.ErrClosed) {
		return fail(CodeClosed, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		return fail(CodeFailed, err, "detail", se.Message)
	}
	return fail(CodeFailed, err, "detail", err.Error())
}

// conflictErr 是並行寫入/序號錯誤。
func conflictErr(id task.TaskID, cause error) error {
	return fail(CodeConflict, cause, "task", string(id))
}

func isConstraint(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code == 19
}
