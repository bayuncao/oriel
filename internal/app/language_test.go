package app

import (
	"os"
	"path/filepath"
	"testing"
)

var testLanguages = []string{"en", "zh-Hans"}

// LG1–LG4：讀記住的選擇；沒有、為空、或已不是 app 寫成的語言時跟隨系統。
func TestOpenLanguage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved *string // nil = 沒有檔案
		want  string
	}{
		{"LG1 沒有檔案", nil, ""},
		{"LG2 記住的語言", ptr("zh-Hans"), "zh-Hans"},
		{"LG3 空檔", ptr(""), ""},
		{"LG4 不再支援或寫到一半", ptr("zh"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.saved != nil {
				if err := os.WriteFile(filepath.Join(dir, languageFile), []byte(*tc.saved), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			l, err := OpenLanguage(dir, testLanguages)
			if err != nil {
				t.Fatal(err)
			}
			if got := l.Choice(); got != tc.want {
				t.Fatalf("choice %q, want %q", got, tc.want)
			}
		})
	}
}

// LG5：讀不了檔時跟隨系統，錯誤交給呼叫方記下。
func TestOpenLanguageUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, languageFile), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLanguage(dir, testLanguages)
	if err == nil || l == nil || l.Choice() != "" {
		t.Fatalf("language %v, err %v", l, err)
	}
}

// LG6–LG9：記住選擇；重開仍在（I07）；不支援的語言與寫不進去都回代碼，選擇不變。
func TestSaveLanguage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from     string
		tag      string
		readOnly bool
		code     Code // "" = 成功
		want     string
	}{
		{"LG6 選一種語言", "", "zh-Hans", false, "", "zh-Hans"},
		{"LG7 改回跟隨系統", "zh-Hans", "", false, "", ""},
		{"LG8 不是 app 寫成的語言", "en", "ja", false, CodeUnknownLanguage, "en"},
		{"LG9 寫不進去", "en", "zh-Hans", true, CodeLanguageSaveFailed, "en"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := OpenLanguage(dir, testLanguages)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.save(tc.from); err != nil {
				t.Fatal(err)
			}
			if tc.readOnly {
				// 目錄換成同名檔案：寫入一定失敗，不受執行身分影響。
				if err := os.Remove(filepath.Join(dir, languageFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, languageFile), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err = l.save(tc.tag)
			if tc.code == "" && err != nil {
				t.Fatal(err)
			}
			if tc.code != "" {
				ce, ok := err.(*codedError)
				if !ok || ce.code != tc.code {
					t.Fatalf("err %v, want %s", err, tc.code)
				}
				if tc.code == CodeUnknownLanguage && ce.params["tag"] != tc.tag {
					t.Fatalf("params %v", ce.params)
				}
			}
			if got := l.Choice(); got != tc.want {
				t.Fatalf("choice %q, want %q", got, tc.want)
			}
			if tc.readOnly {
				return
			}
			reopened, err := OpenLanguage(dir, testLanguages)
			if err != nil {
				t.Fatal(err)
			}
			if got := reopened.Choice(); got != tc.want {
				t.Fatalf("reopened %q, want %q", got, tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
