package app

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bayuncao/oriel/internal/proc"
)

// acquisitions 記下聲明的拿與放：held 是目前持有的條數，total 是拿過幾次。
type acquisitions struct{ held, total int }

func (a *acquisitions) acquire() func() {
	a.held++
	a.total++
	return func() { a.held-- }
}

// KA01–KA04、KA07：開著且有 agent 在幹活時持有一條聲明，否則釋放；重複的通知不多拿。
func TestKeepAwakeHolds(t *testing.T) {
	var a acquisitions
	k, err := OpenKeepAwake(t.TempDir(), a.acquire)
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		name  string
		do    func()
		held  int
		total int
	}
	for _, s := range []step{
		{"KA10 default on, nothing running", func() {}, 0, 0},
		{"KA01 an agent starts", func() { k.SetWorking(true) }, 1, 1},
		{"KA03 more work keeps one assertion", func() { k.SetWorking(true) }, 1, 1},
		{"KA02/KA04 nothing working", func() { k.SetWorking(false) }, 0, 1},
		{"KA07 switch off while idle", func() { mustSet(t, k, false) }, 0, 1},
		{"KA07 work while off", func() { k.SetWorking(true) }, 0, 1},
		{"KA07 switch on while working", func() { mustSet(t, k, true) }, 1, 2},
		{"KA07 switch off while working", func() { mustSet(t, k, false) }, 0, 2},
	} {
		s.do()
		if a.held != s.held || a.total != s.total {
			t.Fatalf("%s: held %d, acquired %d; want %d, %d", s.name, a.held, a.total, s.held, s.total)
		}
	}
}

func mustSet(t *testing.T, k *KeepAwake, on bool) {
	t.Helper()
	if err := k.Set(on); err != nil {
		t.Fatal(err)
	}
}

// KA10：沒有檔案是開；關掉後重新打開仍是關；壞掉的內容當成開。
func TestKeepAwakeRemembered(t *testing.T) {
	dir := t.TempDir()
	none := func() func() { return func() {} }
	k, err := OpenKeepAwake(dir, none)
	if err != nil || !k.On() {
		t.Fatalf("fresh: on = %v, %v", k.On(), err)
	}
	mustSet(t, k, false)
	k, err = OpenKeepAwake(dir, none)
	if err != nil || k.On() {
		t.Fatalf("after off: on = %v, %v", k.On(), err)
	}
	if err := os.WriteFile(filepath.Join(dir, keepAwakeFile), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if k, _ = OpenKeepAwake(dir, none); !k.On() {
		t.Fatal("unknown content should read as on")
	}
}

// KA14：寫不進去時回 app.keep_awake_save_failed，開關與聲明都不變。
func TestKeepAwakeSaveFailed(t *testing.T) {
	var a acquisitions
	dir := filepath.Join(t.TempDir(), "missing") // 不存在的目錄：寫檔失敗
	k, err := OpenKeepAwake(dir, a.acquire)
	if err != nil {
		t.Fatal(err)
	}
	k.SetWorking(true)
	err = k.Set(false)
	if c, ok := err.(interface{ ErrorCode() string }); !ok || c.ErrorCode() != string(CodeKeepAwakeSaveFailed) {
		t.Fatalf("Set error = %v", err)
	}
	if !k.On() || a.held != 1 {
		t.Fatalf("a failed save changed things: on = %v, held = %d", k.On(), a.held)
	}
}

// 換了介面語言：持有的聲明換一條新的（先拿後放），沒有持有時不拿。
func TestKeepAwakeRelabel(t *testing.T) {
	var a acquisitions
	k, _ := OpenKeepAwake(t.TempDir(), a.acquire)
	k.Relabel()
	if a.total != 0 {
		t.Fatalf("relabel while idle acquired %d", a.total)
	}
	k.SetWorking(true)
	k.Relabel()
	if a.held != 1 || a.total != 2 {
		t.Fatalf("relabel: held %d, acquired %d; want 1, 2", a.held, a.total)
	}
}

// 計數的 goroutine 與設定頁同時呼叫：聲明從不超過一條，最後與“開著且在幹活”一致（-race 下跑）。
func TestKeepAwakeConcurrent(t *testing.T) {
	var a acquisitions
	maxHeld := 0
	acquire := func() func() {
		r := a.acquire()
		maxHeld = max(maxHeld, a.held) // acquire 與 release 都在 k.mu 裡呼叫
		return r
	}
	k, _ := OpenKeepAwake(t.TempDir(), acquire)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for j := range 200 {
				if i%2 == 0 {
					k.SetWorking(j%3 != 0)
				} else if err := k.Set(j%2 == 0); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if maxHeld > 1 {
		t.Fatalf("held %d assertions at once", maxHeld)
	}
	k.mu.Lock()
	want := 0
	if k.on && k.working {
		want = 1
	}
	k.mu.Unlock()
	if a.held != want {
		t.Fatalf("held %d at the end, want %d", a.held, want)
	}
}

// 跟著 Task 列表的計數結束時（這裡是 hub 關閉）放掉聲明，不論先前是否在幹活。
func TestFollowTaskCountsReleases(t *testing.T) {
	var a acquisitions
	k, _ := OpenKeepAwake(t.TempDir(), a.acquire)
	k.SetWorking(true) // 模擬結束前正在幹活
	app, _ := newTestApp(t, proc.Env{}, t.TempDir())
	app.Close()
	if err := FollowTaskCounts(ctxb2(), app.Hub(), k, func(TaskCounts) {}); err != nil {
		t.Fatal(err)
	}
	if a.held != 0 {
		t.Fatalf("held %d after following ended", a.held)
	}
}
