package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/transport/remote"
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
		{"KA01 an agent starts", func() { k.SetTasks(true, false) }, 1, 1},
		{"KA03 more work keeps one assertion", func() { k.SetTasks(true, false) }, 1, 1},
		{"KA02/KA04 nothing working", func() { k.SetTasks(false, true) }, 0, 1},
		{"KA07 switch off while idle", func() { mustSet(t, k, false) }, 0, 1},
		{"KA07 work while off", func() { k.SetTasks(true, false) }, 0, 1},
		{"KA07 switch on while working", func() { mustSet(t, k, true) }, 1, 2},
		{"KA07 switch off while working", func() { mustSet(t, k, false) }, 0, 2},
	} {
		s.do()
		if a.held != s.held || a.total != s.total {
			t.Fatalf("%s: held %d, acquired %d; want %d, %d", s.name, a.held, a.total, s.held, s.total)
		}
	}
}

// SL01–SL04、SL06、SL08：有遠程時，停在待答審批上也持有；從幹活變成等審批不先放再拿；
// 沒有遠程時照 KA04 釋放；開關是總開關。
func TestKeepAwakeRemote(t *testing.T) {
	var a acquisitions
	k, err := OpenKeepAwake(t.TempDir(), a.acquire)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		name        string
		do          func()
		held, total int
	}{
		{"SL03 waiting without remote", func() { k.SetTasks(false, true) }, 0, 0},
		{"KA01 an agent works", func() { k.SetTasks(true, false) }, 1, 1},
		{"SL03/KA04 waiting without remote releases", func() { k.SetTasks(false, true) }, 0, 1},
		{"SL04 remote appears while waiting", func() { k.SetRemote(true) }, 1, 2},
		{"SL01 back to work keeps it", func() { k.SetTasks(true, false) }, 1, 2},
		{"SL01 work to waiting keeps it, no flicker", func() { k.SetTasks(false, true) }, 1, 2},
		{"SL01 work and waiting at once", func() { k.SetTasks(true, true) }, 1, 2},
		{"SL04 remote goes away while waiting", func() { k.SetTasks(false, true); k.SetRemote(false) }, 0, 2},
		{"SL04 remote back", func() { k.SetRemote(true) }, 1, 3},
		{"SL02/SL06 round ends, nothing pending", func() { k.SetTasks(false, false) }, 0, 3},
		{"SL01 waiting again", func() { k.SetTasks(false, true) }, 1, 4},
		{"SL08 switch off", func() { mustSet(t, k, false) }, 0, 4},
		{"SL08 remote and waiting while off", func() { k.SetRemote(true); k.SetTasks(false, true) }, 0, 4},
		{"SL08 switch on again", func() { mustSet(t, k, true) }, 1, 5},
	} {
		s.do()
		if a.held != s.held || a.total != s.total {
			t.Fatalf("%s: held %d, acquired %d; want %d, %d", s.name, a.held, a.total, s.held, s.total)
		}
	}
}

// SL04、SL05：FollowRemote 跟著真的遠程控制器：開著且至少有一台設備才算有遠程；與中繼連不上、撤銷的不是最後
// 一台都不算變化；結束時放掉。每一步都等到 followRemote 處理完那個變化（observed 看到對應的狀態），再核對聲明，
// 所以“不該有的拿或放”也看得出來。
func TestFollowRemote(t *testing.T) {
	a, err := New(ctxb2(), Options{DataDir: t.TempDir(), Inherited: proc.Env{}, Root: t.TempDir(),
		Remote: true, Keychain: provider.NewMemKeychain()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	h, rc := a.Hub(), a.Remote()
	var acq acquisitions
	k, err := OpenKeepAwake(t.TempDir(), acq.acquire)
	if err != nil {
		t.Fatal(err)
	}
	k.SetTasks(false, true) // 有 Task 停在待答審批上
	type seen struct {
		status  remote.Status
		devices int
	}
	views := make(chan seen, 64)
	ctx, cancel := context.WithCancel(ctxb2())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- followRemote(ctx, rc, k, func(s remote.Status, n int) { views <- seen{s, n} })
	}()

	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// await 等到 followRemote 處理完滿足 cond 的推送，再核對聲明：持有幾條、一共拿過幾次。
	await := func(step string, cond func(seen) bool, held, total int) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case v := <-views:
				if !cond(v) {
					continue
				}
				k.mu.Lock() // acquire 與 release 在 k.mu 裡改 acq
				gotHeld, gotTotal := acq.held, acq.total
				k.mu.Unlock()
				if gotHeld != held || gotTotal != total {
					t.Fatalf("%s: held %d, acquired %d; want %d, %d", step, gotHeld, gotTotal, held, total)
				}
				return
			case <-deadline:
				t.Fatalf("%s: never saw the change", step)
			}
		}
	}
	pair := func(n byte) hub.Device {
		t.Helper()
		d, err := h.PairDevice(ctx, hub.LocalOwner, hub.DeviceKey{n}, "iPhone")
		ok(err)
		return d
	}
	devices := func(n int) func(seen) bool { return func(v seen) bool { return v.devices == n } }
	state := func(st remote.State) func(seen) bool { return func(v seen) bool { return v.status.State == st } }

	ok(rc.SetOn(true))
	await("SL04 on without devices", state(remote.StateNoRelay), 0, 0)
	first := pair(1)
	await("SL04 first device", devices(1), 1, 1)
	ok(rc.SetRelay("ws://127.0.0.1:1")) // 拒絕連線：退避重試
	await("SL05 relay unreachable", state(remote.StateRetrying), 1, 1)
	second := pair(2)
	await("SL04 second device", devices(2), 1, 1)
	ok(h.RevokeDevice(ctx, hub.LocalOwner, first.ID))
	await("SL04 one device left", devices(1), 1, 1)
	ok(h.RevokeDevice(ctx, hub.LocalOwner, second.ID))
	await("SL04 last device revoked", devices(0), 0, 1)
	pair(3)
	await("SL04 paired again", devices(1), 1, 2)
	ok(rc.SetOn(false))
	await("SL04 remote off", state(remote.StateOff), 0, 2)
	ok(rc.SetOn(true))
	await("SL04 remote on again", func(v seen) bool { return v.status.On }, 1, 3)

	cancel()
	deadline := time.After(5 * time.Second)
wait:
	for {
		select {
		case err := <-done:
			ok(err)
			break wait
		case <-views: // 結束前的推送不能讓 observed 堵住
		case <-deadline:
			t.Fatal("followRemote did not return")
		}
	}
	if acq.held != 0 || acq.total != 3 {
		t.Fatalf("after following ended: held %d, acquired %d; want 0, 3", acq.held, acq.total)
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
	k.SetTasks(true, false)
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
	k.SetTasks(true, false)
	k.Relabel()
	if a.held != 1 || a.total != 2 {
		t.Fatalf("relabel: held %d, acquired %d; want 1, 2", a.held, a.total)
	}
}

// 計數、遠程的 goroutine 與設定頁同時呼叫：聲明從不超過一條，最後與“開著，且在幹活或有遠程時在等審批”一致
// （-race 下跑）。
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
	for i := range 6 {
		wg.Go(func() {
			for j := range 200 {
				switch i % 3 {
				case 0:
					k.SetTasks(j%3 == 1, j%3 == 2)
				case 1:
					k.SetRemote(j%2 == 0)
				default:
					if err := k.Set(j%2 == 0); err != nil {
						t.Error(err)
					}
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
	if k.on && (k.working || k.remote && k.waiting) {
		want = 1
	}
	k.mu.Unlock()
	if a.held != want {
		t.Fatalf("held %d at the end, want %d", a.held, want)
	}
}

// 跟著 Task 列表的計數結束時（這裡是 hub 關閉）放掉聲明，不論先前是因為在幹活，還是有遠程時在等審批。
func TestFollowTaskCountsReleases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(k *KeepAwake)
	}{
		{"KA02 working", func(k *KeepAwake) { k.SetTasks(true, false) }},
		{"SL02 waiting with remote", func(k *KeepAwake) { k.SetRemote(true); k.SetTasks(false, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a acquisitions
			k, _ := OpenKeepAwake(t.TempDir(), a.acquire)
			tc.setup(k)
			if a.held != 1 {
				t.Fatalf("setup holds %d", a.held)
			}
			app, _ := newTestApp(t, proc.Env{}, t.TempDir())
			app.Close()
			if err := FollowTaskCounts(ctxb2(), app.Hub(), k, func(TaskCounts) {}); err != nil {
				t.Fatal(err)
			}
			if a.held != 0 {
				t.Fatalf("held %d after following ended", a.held)
			}
		})
	}
}
