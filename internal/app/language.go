package app

// language.go：用戶選單選的介面語言（D73，docs/acceptance/m1.md 語言一節）。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/egoist/mygo"
)

// languageFile 記住選擇：內容是語言標籤，沒有檔案或內容為空是跟隨系統。
// 寫到一半的檔案讀不出 app 寫成的語言，也就是跟隨系統，所以不需要原子寫入。
const languageFile = "language"

// Language 是用戶選單選的介面語言："" 跟隨系統，否則是 app 寫成的語言之一。
// MyGo 也保存 SetLocale 的選擇，但讀不出是否跟隨系統，所以 OrielHub 另存一份，
// 啟動時交還 MyGo（D73）。方法可從任何 goroutine 呼叫。
type Language struct {
	path      string
	supported []string

	mu     sync.Mutex
	choice string
}

// OpenLanguage 讀 dir 下記住的選擇；supported 是 app 寫成的語言
// （mygo.App.Languages）。記住的語言已不在其中時是跟隨系統。讀檔出錯時
// 仍回傳跟隨系統的 Language，連同錯誤。
func OpenLanguage(dir string, supported []string) (*Language, error) {
	l := &Language{path: filepath.Join(dir, languageFile), supported: slices.Clone(supported)}
	b, err := os.ReadFile(l.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return l, err
	}
	if tag := strings.TrimSpace(string(b)); slices.Contains(l.supported, tag) {
		l.choice = tag
	}
	return l, nil
}

// Choice 回傳目前的選擇，"" 是跟隨系統。
func (l *Language) Choice() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.choice
}

// Set 記住 tag，並讓 app 立刻改用它（mygo.App.SetLocale）。tag 不是 ""
// 也不在 supported 中回 app.unknown_language，寫不進去回
// app.language_save_failed，兩者都不改變選擇。
func (l *Language) Set(tag string) error {
	// 鎖到 SetLocale 之後：同時兩次 Set 時，記住的與顯示的是同一個。
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.save(tag); err != nil {
		return err
	}
	mygo.App.SetLocale(tag)
	return nil
}

// save 檢查並寫下 tag；呼叫方持有 mu。
func (l *Language) save(tag string) error {
	if tag != "" && !slices.Contains(l.supported, tag) {
		return coded(CodeUnknownLanguage, nil, "tag", tag)
	}
	if err := os.WriteFile(l.path, []byte(tag), 0o644); err != nil {
		return coded(CodeLanguageSaveFailed, err, "detail", err.Error())
	}
	l.choice = tag
	return nil
}
