package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// NA01、NA03、NA04：讀記住的“無賬號使用”；沒有檔案（全新安裝、舊版本升級）、為空或不認得時是沒選過。
func TestOpenAccount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved *string // nil = 沒有檔案
		want  bool
	}{
		{"NA01 全新安裝", nil, false},
		{"NA03 選過", ptr("none"), true},
		{"選過，帶換行", ptr("none\n"), true},
		{"空檔", ptr(""), false},
		{"不認得", ptr("someone"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.saved != nil {
				if err := os.WriteFile(filepath.Join(dir, accountFile), []byte(*tc.saved), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			a, err := OpenAccount(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := a.WithoutAccount(); got != tc.want {
				t.Fatalf("WithoutAccount = %v, want %v", got, tc.want)
			}
		})
	}
}

// 讀不了檔時當沒選過（再進一次登錄頁），錯誤交給呼叫方記下。
func TestOpenAccountUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, accountFile), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := OpenAccount(dir)
	if err == nil || a == nil || a.WithoutAccount() {
		t.Fatalf("account %v, err %v", a, err)
	}
}

// NA02、NA03、NA05：選了就記住，重開仍在；寫不進去時這次照樣算選過、回代碼，重開後又是沒選過。
func TestContinueWithoutAccount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readOnly bool
		code     Code // "" = 成功
		reopened bool
	}{
		{"NA02 記住", false, "", true},
		{"NA05 寫不進去", true, CodeAccountSaveFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a, err := OpenAccount(dir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.readOnly {
				// 檔案的位置換成同名目錄：寫入一定失敗，不受執行身分影響。
				if err := os.Mkdir(filepath.Join(dir, accountFile), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err = a.ContinueWithout()
			var ce *codedError
			switch {
			case tc.code == "" && err != nil:
				t.Fatalf("err %v", err)
			case tc.code != "" && (!errors.As(err, &ce) || ce.code != tc.code || ce.params["detail"] == ""):
				t.Fatalf("err %v, want %s with detail", err, tc.code)
			}
			if !a.WithoutAccount() {
				t.Fatal("not counted as chosen for this run")
			}
			if tc.readOnly {
				if err := os.Remove(filepath.Join(dir, accountFile)); err != nil {
					t.Fatal(err)
				}
			}
			again, err := OpenAccount(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := again.WithoutAccount(); got != tc.reopened {
				t.Fatalf("after reopening: %v, want %v", got, tc.reopened)
			}
		})
	}
}
