package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// PF04、PF09：讀記住的外觀；沒有、為空或不認得時跟隨系統。
func TestOpenTheme(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved *string // nil = 沒有檔案
		want  string
	}{
		{"沒有檔案", nil, ""},
		{"PF04 記住的深色", ptr("dark"), "dark"},
		{"記住的淺色", ptr("light\n"), "light"},
		{"PF09 空檔", ptr(""), ""},
		{"PF09 不認得", ptr("sepia"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.saved != nil {
				if err := os.WriteFile(filepath.Join(dir, themeFile), []byte(*tc.saved), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			th, err := OpenTheme(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := th.Choice(); got != tc.want {
				t.Fatalf("choice %q, want %q", got, tc.want)
			}
		})
	}
}

// 讀不了檔時跟隨系統，錯誤交給呼叫方記下。
func TestOpenThemeUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, themeFile), 0o755); err != nil {
		t.Fatal(err)
	}
	th, err := OpenTheme(dir)
	if err == nil || th == nil || th.Choice() != "" {
		t.Fatalf("theme %v, err %v", th, err)
	}
}

// PF03、PF04、PF08：選了就生效並記住，重開仍在；不認得的值什麼都不變；
// 寫不進去時這次照樣生效、回代碼，重開後回到原來記住的。
func TestSetTheme(t *testing.T) {
	for _, tc := range []struct {
		name     string
		choice   string
		readOnly bool
		code     Code   // "" = 成功
		applied  string // 生效的；"-" = 沒有生效
		reopened string // 重開後的
	}{
		{"PF03 深色", "dark", false, "", "dark", "dark"},
		{"改回跟隨系統", "", false, "", "", ""},
		{"不認得的值", "sepia", false, CodeUnknownTheme, "-", "light"},
		{"PF08 寫不進去", "dark", true, CodeThemeSaveFailed, "dark", "light"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, themeFile), []byte("light"), 0o644); err != nil {
				t.Fatal(err)
			}
			th, err := OpenTheme(dir)
			if err != nil {
				t.Fatal(err)
			}
			applied := "-"
			th.apply = func(c string) { applied = c }
			if tc.readOnly {
				// 檔案換成同名目錄：寫入一定失敗，不受執行身分影響。
				if err := os.Remove(filepath.Join(dir, themeFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, themeFile), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err = th.Set(tc.choice)
			var ce *codedError
			switch {
			case tc.code == "" && err != nil:
				t.Fatalf("err %v", err)
			case tc.code != "" && (!errors.As(err, &ce) || ce.code != tc.code):
				t.Fatalf("err %v, want %s", err, tc.code)
			}
			if applied != tc.applied {
				t.Fatalf("applied %q, want %q", applied, tc.applied)
			}
			if tc.readOnly {
				if err := os.Remove(filepath.Join(dir, themeFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, themeFile), []byte("light"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			again, err := OpenTheme(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := again.Choice(); got != tc.reopened {
				t.Fatalf("reopened %q, want %q", got, tc.reopened)
			}
		})
	}
}
