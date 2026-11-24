package app

// theme.go：介面的淺色、深色或跟隨系統（D158，docs/acceptance/settings.md 第 2 節）。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/egoist/mygo"
)

// themeFile 記住選擇：內容是 "light" 或 "dark"；沒有檔案、為空或不認得是跟隨系統（PF09）。
const themeFile = "theme"

// Theme 是記住的外觀："" 跟隨系統，"light" 或 "dark" 固定。生效靠 MyGo 的
// Theme.SetSource：原生視窗、菜單欄面板與頁面的 prefers-color-scheme 一起換，
// 啟動時在開窗之前交給它，就不會先閃一下另一種（PF04）。方法可從任何 goroutine 呼叫。
type Theme struct {
	path string
	// apply 讓 app 立刻改用 choice；nil 是 mygo.Theme.SetSource。測試的接縫，建構時設定。
	apply func(choice string)

	// setMu 讓兩次 Set 依序生效與寫入：記住的與顯示的是同一個。生效要等主線程，
	// 所以 mu 不跨過它：主線程上讀 Choice 不會等一個在等主線程的 Set。
	setMu sync.Mutex

	mu     sync.Mutex
	choice string
}

// OpenTheme 讀 dir 下記住的選擇。讀檔出錯時仍回傳跟隨系統的 Theme，連同錯誤。
func OpenTheme(dir string) (*Theme, error) {
	t := &Theme{path: filepath.Join(dir, themeFile)}
	b, err := os.ReadFile(t.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return t, err
	}
	if c := strings.TrimSpace(string(b)); validTheme(c) {
		t.choice = c
	}
	return t, nil
}

func validTheme(c string) bool { return c == "" || c == "light" || c == "dark" }

// Choice 回傳目前的選擇，"" 是跟隨系統。
func (t *Theme) Choice() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.choice
}

// Apply 讓 app 用目前的選擇；啟動時在開窗之前呼叫。
func (t *Theme) Apply() {
	t.setMu.Lock()
	defer t.setMu.Unlock()
	t.applyChoice(t.Choice())
}

// Set 讓 app 立刻改用 choice 並記住它。choice 不認得回 app.unknown_theme，什麼都不變；
// 寫不進去回 app.theme_save_failed，但這次照樣生效，重啟後回到原來記住的（PF08）。
func (t *Theme) Set(choice string) error {
	if !validTheme(choice) {
		return coded(CodeUnknownTheme, nil, "theme", choice)
	}
	t.setMu.Lock()
	defer t.setMu.Unlock()
	t.mu.Lock()
	t.choice = choice
	t.mu.Unlock()
	t.applyChoice(choice)
	if err := os.WriteFile(t.path, []byte(choice), 0o644); err != nil {
		return coded(CodeThemeSaveFailed, err, "detail", err.Error())
	}
	return nil
}

// applyChoice 交給 MyGo；它等主線程做完，呼叫時不可拿著 mu。
func (t *Theme) applyChoice(choice string) {
	if t.apply != nil {
		t.apply(choice)
		return
	}
	mygo.Theme.SetSource(mygo.ThemeSource(choice)) // "" 是跟隨系統
}
