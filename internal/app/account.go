package app

// account.go：還沒有賬號時，選了“無賬號使用”沒有（D112，docs/acceptance/m2.md NA）。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// accountFile 記住選擇：內容是 withoutAccount 時選過“無賬號使用”；沒有檔案、為空或
// 不認得是沒選過，啟動時先進登錄頁（NA01、NA04）。
const accountFile = "account"

const withoutAccount = "none"

// Account 是賬號的本機狀態。賬號（D112）做好之前只有一件事：選過“無賬號使用”沒有。
// 方法可從任何 goroutine 呼叫。
type Account struct {
	path string

	mu      sync.Mutex
	without bool
}

// OpenAccount 讀 dir 下記住的選擇。讀檔出錯時仍回傳沒選過的 Account，連同錯誤。
func OpenAccount(dir string) (*Account, error) {
	a := &Account{path: filepath.Join(dir, accountFile)}
	b, err := os.ReadFile(a.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return a, err
	}
	a.without = strings.TrimSpace(string(b)) == withoutAccount
	return a, nil
}

// WithoutAccount 回答是否選過“無賬號使用”。
func (a *Account) WithoutAccount() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.without
}

// ContinueWithout 記住“無賬號使用”（NA02、NA03）。寫不進去回 app.account_save_failed：
// 這次照樣算選過，重啟後再進登錄頁（NA05）。
func (a *Account) ContinueWithout() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.without = true
	if err := os.WriteFile(a.path, []byte(withoutAccount), 0o644); err != nil {
		return coded(CodeAccountSaveFailed, err, "detail", err.Error())
	}
	return nil
}
