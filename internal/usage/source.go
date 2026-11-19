package usage

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// Source 讀一個 agent 在本機的用量記錄；由 runtime/<agent> 實作，記錄格式
// 只寫在那裡（規則 3）。實作不執行任何程式，也不讀登入憑據（D87）。
type Source interface {
	Agent() task.AgentID
	// Find 依環境快照找出記錄檔。roots 是找過的位置，給記錄來源顯示；
	// 位置不存在不是錯誤，讀不了（如沒有權限）才是。
	Find(env proc.Env) (roots []string, files []File, err error)
	// Parser 回傳從 state 接著解析 f 的 Parser；state 為 nil 表示從頭開始。
	// Format 為 Database 的檔案不用 Parser，見 DatabaseSource。
	Parser(f File, state []byte) (Parser, error)
	// Unreported 是這個 agent 不報的類別，頁面上顯示“不報”。
	Unreported() []Category
	// Notes 是記錄來源裡的說明。
	Notes() []Note
}

// DatabaseSource 是記錄在 SQLite 裡的 Source（OpenCode）：按自己記下的水位
// 查新增與更新過的行，資料庫被鎖住時回 ErrBusy。
type DatabaseSource interface {
	Source
	// ReadDatabase 從 state 讀一段，回傳讀到的用量與新的 state；more 表示還有下一段。
	ReadDatabase(ctx context.Context, f File, state []byte) (recs []Record, next []byte, more bool, err error)
}

// Parser 解析一個記錄檔，從頭或從存下的 state 接著解析。
type Parser interface {
	// Line 解析一行完整的行（不含換行；Whole 格式是整份內容）。bad 表示這行
	// 像是用量的記錄卻讀不懂，或根本不是 JSON 物件。
	Line(line []byte) (recs []Record, quotas []Quota, bad bool)
	// State 是目前的解析狀態，存進讀取位置；不得含對話內容（U17）。
	State() []byte
}

// Format 是記錄檔的讀法。
type Format string

const (
	// Lines 是只追加的 JSONL：從讀到的位置接著讀完整的行。
	Lines Format = "lines"
	// Compressed 是 zstd 壓縮的 JSONL（D91）：不會追加，變了就從頭解壓。
	Compressed Format = "zstd"
	// Whole 是整份改寫的 JSON 檔案：變了就整份重讀。
	Whole Format = "whole"
	// Database 是 SQLite 檔案（D90）：由 DatabaseSource 自己查。
	Database Format = "sqlite"
)

// File 是一個記錄檔。
type File struct {
	Path   string
	Format Format
	// Watch 是除了 Path 之外也要看大小與修改時間的檔案，如 SQLite 的 -wal。
	Watch []string
}

// Note 是記錄來源裡的說明，頁面翻譯它。
type Note string

const (
	// NoteNotRecorded：這個 agent 不在本機記 token（Cursor CLI）。
	NoteNotRecorded Note = "not_recorded"
	// NoteSessionEnd：會話結束時才記下用量，進行中的會話不計入（Copilot CLI）。
	NoteSessionEnd Note = "session_end"
	// NoteTurnEnd：一輪完成才記下用量，進行中的一輪不計入（Grok）。
	NoteTurnEnd Note = "turn_end"
	// NoteReportedOnly：只有經 OrielHub 運行的部分，來自 agent 每一輪的上報（Cursor CLI，m2.md U89）。
	NoteReportedOnly Note = "reported_only"
)

// ErrBusy 表示記錄被別的程式鎖住：這一輪跳過，下一輪再讀，不算讀取失敗（U45）。
var ErrBusy = errors.New("usage: records are locked by another program")

// QueryFunc 以唯讀方式對一個外部 SQLite 檔案執行一條查詢後立即關閉，回傳
// 結果行；值是 nil、int64、float64、string 或 []byte。庫被鎖住時回 ErrBusy。
// 由 store 提供、app 交給讀 SQLite 記錄的 Source（D90）。
type QueryFunc func(ctx context.Context, path, query string, args ...any) ([][]any, error)

// Walk 在 root 下找出 match 接受的檔案，依路徑排序；root 不存在時回空。
// 子目錄讀不了是錯誤（U14）。
func Walk(root string, format func(path string) (Format, bool)) ([]File, error) {
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if f, ok := format(path); ok {
				out = append(out, File{Path: path, Format: f})
			}
		}
		return nil
	})
	return out, err
}

// IsObject 回報一行看起來是完整的 JSON 物件（以 { 開頭、} 結尾）；不是的行
// 算沒讀懂。只看頭尾，不解析：大多數行是對話內容，不需要解析（U13）。
func IsObject(line []byte) bool {
	line = bytes.TrimSpace(line)
	return len(line) >= 2 && line[0] == '{' && line[len(line)-1] == '}'
}

// Root 是覆蓋變數的值；沒有設定時是家目錄下的 def。家目錄未知時回空。
func Root(env proc.Env, override, def string) string {
	if override != "" {
		if v := env.Get(override); v != "" {
			return v
		}
	}
	if h := env.Home(); h != "" {
		return filepath.Join(h, def)
	}
	return ""
}
