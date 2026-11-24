package app

// keepawake.go：Agent 運行時防止電腦空閒睡眠（D80，m2.md 第 30 節 KA01–KA16；打開遠程後見 D146，
// remote.md 第 7 節 SL01–SL12）。開關是本機的偏好，與介面語言一樣記在資料目錄；開著時，有 agent 在幹活
// （TaskCounts.Working），或有遠程（開著且至少配對了一台設備）而有 Task 在等人處理（TaskCounts.Waiting：
// 停在待答審批上或有待確認），就經注入的
// acquire 拿一條防止空閒睡眠的聲明（正式是 mygo.Power.KeepAwake），否則釋放。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/transport/remote"
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
	working bool   // 有 agent 在幹活
	waiting bool   // 有 Task 在等人處理：停在待答審批上或有待確認（手機可能正要處理，D146）
	remote  bool   // 遠程開著且至少配對了一台設備
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

// SetTasks 告訴它現在有沒有 agent 在幹活（KA01–KA04），有沒有 Task 在等人處理（SL01–SL03、SL11）。
// 兩者一起給：從幹活變成等審批時，有遠程就一直持有，不先放再拿（SL01）。
func (k *KeepAwake) SetTasks(working, waiting bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.working, k.waiting = working, waiting
	k.applyLocked()
}

// SetRemote 告訴它有沒有遠程：開著且至少配對了一台設備（SL04、SL05）。
func (k *KeepAwake) SetRemote(remote bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.remote = remote
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

// applyLocked 讓聲明與“開著，且有 agent 在幹活或有遠程時有 Task 在等審批”一致：始終最多一條（KA03）。
// 開關是總開關（SL08）。
func (k *KeepAwake) applyLocked() {
	want := k.on && (k.working || k.remote && k.waiting)
	switch {
	case want && k.release == nil:
		k.release = k.acquire()
	case !want && k.release != nil:
		k.release()
		k.release = nil
	}
}

// FollowRemote 把“有遠程”（開著且至少配對了一台設備，D146）交給 awake：只看開關與設備，不看此刻與中繼的
// 連線和設備在不在線（SL05），網路抖動時聲明不一放一拿；只在有沒有遠程變了時才告訴它。正式版沒有遠程
// （D149），不呼叫它（SL10）。ctx 結束、rc 或 hub 關閉時回 nil；不論為什麼結束，結束時都告訴 awake 沒有遠程。
// 不能在主線程上呼叫。
func FollowRemote(ctx context.Context, rc *remote.Control, awake *KeepAwake) error {
	return followRemote(ctx, rc, awake, nil)
}

// followRemote 同 FollowRemote；observed（測試用，可為 nil）在每次推送交給 awake 之後呼叫，帶著當下的狀態與設備數，
// 測試據此等到某個變化已經處理完，再看聲明。
func followRemote(ctx context.Context, rc *remote.Control, awake *KeepAwake, observed func(remote.Status, int)) error {
	defer awake.SetRemote(false)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// 兩個觀察者各在一個 goroutine 上；mu 讓兩邊的更新依序處理，後到的總是看到兩邊最新的值。
	var (
		mu      sync.Mutex
		status  remote.Status
		devices int
		has     bool
	)
	update := func(change func()) error {
		mu.Lock()
		defer mu.Unlock()
		change()
		if now := status.On && devices > 0; now != has {
			has = now
			awake.SetRemote(has)
		}
		if observed != nil {
			observed(status, devices)
		}
		return nil
	}
	errs := make(chan error, 2)
	go func() {
		errs <- rc.Watch(ctx, func(s remote.Status) error { return update(func() { status = s }) })
	}()
	go func() {
		err := rc.WatchDevices(ctx, func(d []remote.DeviceRow) error { return update(func() { devices = len(d) }) })
		if errors.Is(err, hub.ErrClosed) {
			err = nil // 應用退出
		}
		errs <- err
	}()
	// 一邊結束（多半是應用退出）就停下另一邊。
	err := <-errs
	cancel()
	return errors.Join(err, <-errs)
}

// SystemKeepAwake 用 mygo.Power.KeepAwake 拿一條只防空閒睡眠的聲明（不保持螢幕常亮，KA01、KA05）；
// 系統列出聲明時寫的原因用當下的介面語言。不能在主線程上呼叫（它等主線程）。
func SystemKeepAwake() (release func()) {
	return mygo.Power.KeepAwake(locales.Catalog.Format(mygo.App.Locale(), "keep_awake_reason", nil), false)
}
