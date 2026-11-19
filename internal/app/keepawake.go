package app

// keepawake.go：Agent 運行時防止電腦空閒睡眠（D80，m2.md 第 30 節 KA01–KA15）。開關是本機的偏好，
// 與介面語言一樣記在資料目錄；有 agent 在幹活（TaskCounts.Working）又開著時，經注入的 acquire
// 拿一條防止空閒睡眠的聲明（正式是 mygo.Power.KeepAwake），兩者有一個不成立就釋放。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/locales"
)

// keepAwakeFile 記住開關：內容是 off 時關，沒有檔案或其他內容都是開（預設開，KA10）。
// 寫到一半的檔案讀不出 off，也就是開，所以不需要原子寫入。
const keepAwakeFile = "keep-awake"

// KeepAwake 是開關與目前的聲明。方法可從任何 goroutine 呼叫，但不能在主線程上：
// acquire 與 release 在持鎖時呼叫，正式的實作要等主線程（mygo 的 onMain）。
type KeepAwake struct {
	path    string
	acquire func() (release func())

	mu      sync.Mutex
	on      bool
	working bool
	release func() // 持有的聲明；nil = 沒有
}

// OpenKeepAwake 讀 dir 下記住的開關；acquire 拿一條聲明並回傳釋放它的函式（正式是
// SystemKeepAwake）。讀檔出錯時仍回傳開著的 KeepAwake，連同錯誤。
func OpenKeepAwake(dir string, acquire func() (release func())) (*KeepAwake, error) {
	k := &KeepAwake{path: filepath.Join(dir, keepAwakeFile), acquire: acquire, on: true}
	b, err := os.ReadFile(k.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return k, err
	}
	k.on = strings.TrimSpace(string(b)) != "off"
	return k, nil
}

// On 回答開關是否開著。
func (k *KeepAwake) On() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.on
}

// Set 記住開關並立刻生效（KA07）；寫不進去回 app.keep_awake_save_failed，不改變開關。
func (k *KeepAwake) Set(on bool) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	v := "on"
	if !on {
		v = "off"
	}
	if err := os.WriteFile(k.path, []byte(v), 0o644); err != nil {
		return coded(CodeKeepAwakeSaveFailed, err, "detail", err.Error())
	}
	k.on = on
	k.applyLocked()
	return nil
}

// SetWorking 告訴它現在有沒有 agent 在幹活（KA01–KA04）。
func (k *KeepAwake) SetWorking(working bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.working = working
	k.applyLocked()
}

// Relabel 在介面語言換了之後重拿持有的聲明，系統列出的原因換成新的語言：先拿新的再放舊的，中間不留空檔。
// 沒有持有時不做什麼。
func (k *KeepAwake) Relabel() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.release == nil {
		return
	}
	old := k.release
	k.release = k.acquire()
	old()
}

// applyLocked 讓聲明與“開著且有 agent 在幹活”一致：始終最多一條（KA03）。
func (k *KeepAwake) applyLocked() {
	want := k.on && k.working
	switch {
	case want && k.release == nil:
		k.release = k.acquire()
	case !want && k.release != nil:
		k.release()
		k.release = nil
	}
}

// SystemKeepAwake 用 mygo.Power.KeepAwake 拿一條只防空閒睡眠的聲明（不保持螢幕常亮，KA01、KA05）；
// 系統列出聲明時寫的原因用當下的介面語言。不能在主線程上呼叫（它等主線程）。
func SystemKeepAwake() (release func()) {
	return mygo.Power.KeepAwake(locales.Catalog.Format(mygo.App.Locale(), "keep_awake_reason", nil), false)
}
