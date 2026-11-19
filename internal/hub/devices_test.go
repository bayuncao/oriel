package hub

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// 遠程設備的驗收場景（docs/acceptance/remote.md 第 2、3 節）裡 hub 負責的部分。

func pairedDevice(t *testing.T, env *testEnv, key DeviceKey, name string) Device {
	t.Helper()
	d, err := env.h.PairDevice(ctxb(), LocalOwner, key, name)
	if err != nil {
		t.Fatalf("PairDevice: %v", err)
	}
	return d
}

func TestPairDevice(t *testing.T) {
	env := newTestHub(t, nil)
	phone := DeviceKey{1}
	first := pairedDevice(t, env, phone, "  May 的 iPhone ")

	// PA03：登記名字、公鑰與配對時間；ID 由公鑰派生。
	want := Device{ID: phone.ID(), Name: "May 的 iPhone", Key: phone, PairedAt: env.clock.Now()}
	if first != want {
		t.Fatalf("PA03 paired %+v, want %+v", first, want)
	}
	if !strings.HasPrefix(string(first.ID), "d-") || len(first.ID) != 18 {
		t.Fatalf("device id %q", first.ID)
	}

	// PA08：同一台手機再配對，不新建，只改名字；ID 與配對時間不變。
	env.clock.Advance(time.Hour)
	again := pairedDevice(t, env, phone, "iPhone 16")
	if again.ID != first.ID || again.PairedAt != first.PairedAt || again.Name != "iPhone 16" {
		t.Fatalf("PA08 re-paired %+v", again)
	}

	// PA11：重裝 App 換了密鑰，是新設備；舊的留在列表裡。
	reinstalled := pairedDevice(t, env, DeviceKey{2}, "iPhone 16")
	if reinstalled.ID == first.ID {
		t.Fatal("PA11 reinstalled phone reused the old device id")
	}
	got, err := env.h.Devices(ctxb(), LocalOwner)
	if err != nil || !reflect.DeepEqual(got, []Device{again, reinstalled}) {
		t.Fatalf("devices %+v, %v", got, err)
	}

	for _, tc := range []struct {
		name string
		in   string
	}{
		{"empty name", ""},
		{"only spaces", "   "},
		{"two lines", "a\nb"},
		{"65 characters", strings.Repeat("机", DeviceNameMax+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.h.PairDevice(ctxb(), LocalOwner, DeviceKey{9}, tc.in)
			if errCode(err) != string(CodeInvalidField) {
				t.Fatalf("got %v, want %s", err, CodeInvalidField)
			}
			if _, ok, _ := env.h.DeviceByKey(ctxb(), LocalOwner, DeviceKey{9}); ok {
				t.Fatal("invalid name registered a device")
			}
		})
	}
	if _, err := env.h.PairDevice(ctxb(), LocalOwner, DeviceKey{9}, strings.Repeat("机", DeviceNameMax)); err != nil {
		t.Fatalf("%d characters: %v", DeviceNameMax, err)
	}
}

func TestDeviceByKey(t *testing.T) {
	env := newTestHub(t, nil)
	d := pairedDevice(t, env, DeviceKey{1}, "iPhone")

	// CN01：認得已配對的公鑰。
	if got, ok, err := env.h.DeviceByKey(ctxb(), LocalOwner, DeviceKey{1}); err != nil || !ok || got != d {
		t.Fatalf("CN01 lookup %+v %v %v", got, ok, err)
	}
	// CN02：沒登記的公鑰。
	if _, ok, err := env.h.DeviceByKey(ctxb(), LocalOwner, DeviceKey{2}); err != nil || ok {
		t.Fatalf("CN02 unknown key found: %v %v", ok, err)
	}
	// CN03：撤銷之後同一把公鑰。
	if err := env.h.RevokeDevice(ctxb(), LocalOwner, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := env.h.DeviceByKey(ctxb(), LocalOwner, DeviceKey{1}); err != nil || ok {
		t.Fatalf("CN03 revoked key found: %v %v", ok, err)
	}
}

func TestDeviceSeenAndRename(t *testing.T) {
	env := newTestHub(t, nil)
	d := pairedDevice(t, env, DeviceKey{1}, "iPhone")
	if !d.LastSeen.IsZero() {
		t.Fatal("never-connected device has a last-seen time")
	}

	// CN01：連上時記下最後在線。
	env.clock.Advance(time.Minute)
	if err := env.h.DeviceSeen(ctxb(), LocalOwner, d.ID); err != nil {
		t.Fatal(err)
	}
	// DV04：改名。
	renamed, err := env.h.RenameDevice(ctxb(), LocalOwner, d.ID, " 工作手機 ")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "工作手機" || renamed.LastSeen != env.clock.Now() || renamed.PairedAt != d.PairedAt {
		t.Fatalf("renamed %+v", renamed)
	}
	if _, err := env.h.RenameDevice(ctxb(), LocalOwner, d.ID, "\n"); errCode(err) != string(CodeInvalidField) {
		t.Fatalf("blank rename: %v", err)
	}

	for name, call := range map[string]func() error{
		"seen":   func() error { return env.h.DeviceSeen(ctxb(), LocalOwner, "d-gone") },
		"rename": func() error { _, err := env.h.RenameDevice(ctxb(), LocalOwner, "d-gone", "x"); return err },
		"revoke": func() error { return env.h.RevokeDevice(ctxb(), LocalOwner, "d-gone") },
	} {
		t.Run(name, func(t *testing.T) {
			var he *Error
			if err := call(); !errors.As(err, &he) || he.Code != CodeUnknownDevice || he.Params["device"] != "d-gone" {
				t.Fatalf("got %v, want %s{device=d-gone}", err, CodeUnknownDevice)
			}
		})
	}
}

// PA09：設備管理只有桌面能用，繞過 transport 的白名單直接以 Principal{Device}
// 呼叫 hub 也被拒絕。
func TestDevicesNeedDesktop(t *testing.T) {
	env := newTestHub(t, nil)
	d := pairedDevice(t, env, DeviceKey{1}, "iPhone")
	remote := Principal{Device: d.ID}
	for name, call := range map[string]func() error{
		"list":   func() error { _, err := env.h.Devices(ctxb(), remote); return err },
		"lookup": func() error { _, _, err := env.h.DeviceByKey(ctxb(), remote, DeviceKey{1}); return err },
		"pair":   func() error { _, err := env.h.PairDevice(ctxb(), remote, DeviceKey{2}, "x"); return err },
		"rename": func() error { _, err := env.h.RenameDevice(ctxb(), remote, d.ID, "x"); return err },
		"seen":   func() error { return env.h.DeviceSeen(ctxb(), remote, d.ID) },
		"revoke": func() error { return env.h.RevokeDevice(ctxb(), remote, d.ID) },
		"reset":  func() error { return env.h.RevokeDevices(ctxb(), remote) },
		"watch":  func() error { return env.h.WatchDevices(ctxb(), remote, func([]Device) error { return nil }) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); errCode(err) != string(CodeNeedsDesktop) {
				t.Fatalf("got %v, want %s", err, CodeNeedsDesktop)
			}
		})
	}
	if got, _ := env.h.Devices(ctxb(), LocalOwner); !reflect.DeepEqual(got, []Device{d}) {
		t.Fatalf("devices changed: %+v", got)
	}
}

// PA10：重啟後從設定取回設備。
func TestDevicesSurviveRestart(t *testing.T) {
	env := newTestHub(t, nil)
	d := pairedDevice(t, env, DeviceKey{1}, "iPhone")
	env.h.Close()

	h := New(Options{Log: env.store, Config: env.store, Index: env.store, Now: env.clock.Now, Timer: env.clock.Timer})
	t.Cleanup(h.Close)
	if err := h.Start(ctxb()); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := h.DeviceByKey(ctxb(), LocalOwner, DeviceKey{1}); err != nil || !ok || got != d {
		t.Fatalf("after restart %+v %v %v", got, ok, err)
	}
}

// deviceLists 在背景跑 WatchDevices，把每次推來的列表送出來。
func deviceLists(t *testing.T, h *Hub) (<-chan []Device, <-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctxb())
	lists := make(chan []Device, 64)
	done := make(chan error, 1)
	go func() {
		done <- h.WatchDevices(ctx, LocalOwner, func(ds []Device) error {
			lists <- ds
			return nil
		})
	}()
	t.Cleanup(cancel)
	return lists, done, cancel
}

// nextList 等到推來的列表滿足 ok；接連的變化可能合成一次，所以只看到達的狀態。
func nextList(t *testing.T, lists <-chan []Device, ok func([]Device) bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ds := <-lists:
			if ok(ds) {
				return
			}
		case <-timeout:
			t.Fatal("device list never reached the expected state")
		}
	}
}

func TestWatchDevices(t *testing.T) {
	env := newTestHub(t, nil)
	kept := pairedDevice(t, env, DeviceKey{1}, "iPad")
	lists, _, _ := deviceLists(t, env.h)
	nextList(t, lists, func(ds []Device) bool { return reflect.DeepEqual(ds, []Device{kept}) })

	// PA03：配對後桌面的列表立刻出現。
	env.clock.Advance(time.Second)
	d := pairedDevice(t, env, DeviceKey{2}, "iPhone")
	nextList(t, lists, func(ds []Device) bool { return reflect.DeepEqual(ds, []Device{kept, d}) })

	// CN01、DV04：最後在線與改名也推。
	env.clock.Advance(time.Minute)
	if err := env.h.DeviceSeen(ctxb(), LocalOwner, d.ID); err != nil {
		t.Fatal(err)
	}
	nextList(t, lists, func(ds []Device) bool { return len(ds) == 2 && ds[1].LastSeen.Equal(env.clock.Now()) })
	if _, err := env.h.RenameDevice(ctxb(), LocalOwner, d.ID, "手機"); err != nil {
		t.Fatal(err)
	}
	nextList(t, lists, func(ds []Device) bool { return len(ds) == 2 && ds[1].Name == "手機" })

	// CN04：撤銷後觀察者（transport/remote）看到它不在了，據此斷開。
	if err := env.h.RevokeDevice(ctxb(), LocalOwner, d.ID); err != nil {
		t.Fatal(err)
	}
	nextList(t, lists, func(ds []Device) bool { return reflect.DeepEqual(ds, []Device{kept}) })

	// CN05：重置遠程撤銷所有設備。
	if err := env.h.RevokeDevices(ctxb(), LocalOwner); err != nil {
		t.Fatal(err)
	}
	nextList(t, lists, func(ds []Device) bool { return len(ds) == 0 })
	if _, ok, _ := env.h.DeviceByKey(ctxb(), LocalOwner, DeviceKey{1}); ok {
		t.Fatal("CN05 device still known after reset")
	}
}

func TestWatchDevicesEnds(t *testing.T) {
	env := newTestHub(t, nil)
	lists, done, cancel := deviceLists(t, env.h)
	<-lists
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ctx done: %v", err)
	}

	sinkErr := errors.New("page went away")
	if err := env.h.WatchDevices(ctxb(), LocalOwner, func([]Device) error { return sinkErr }); err != sinkErr {
		t.Fatalf("sink error: %v", err)
	}

	lists, done, _ = deviceLists(t, env.h)
	<-lists
	env.h.Close()
	if err := <-done; errCode(err) != string(CodeClosed) {
		t.Fatalf("after Close: %v", err)
	}
	if err := env.h.WatchDevices(ctxb(), LocalOwner, func([]Device) error { return nil }); errCode(err) != string(CodeClosed) {
		t.Fatalf("watch on a closed hub: %v", err)
	}
}

// CN09：撤銷設備不動它發起、還在等的 Confirmation，桌面照常處理。
func TestRevokeKeepsConfirmations(t *testing.T) {
	env := newTestHub(t, nil)
	dir := gitRepo(t)
	env.store.PutWorkspace(ctxb(), Workspace{ID: "w-1", Path: realPath(t, dir), Policy: task.PolicyAlways})
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	id := env.create(t, dir, "", testLaunch())
	d := pairedDevice(t, env, DeviceKey{1}, "iPhone")
	if _, err := env.h.Command(ctxb(), Principal{Device: d.ID}, id, task.Command{
		Kind:  task.CmdMerge,
		Merge: &task.Merge{Target: task.Target{Level: task.TargetWorktree}}}); err != nil {
		t.Fatal(err)
	}

	if err := env.h.RevokeDevice(ctxb(), LocalOwner, d.ID); err != nil {
		t.Fatal(err)
	}
	cs, err := env.h.Confirmations(ctxb(), LocalOwner)
	if err != nil || len(cs) != 1 || cs[0].RequestedBy != d.ID {
		t.Fatalf("confirmations after revoke %+v %v", cs, err)
	}
	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind:    task.CmdConfirm,
		Confirm: &task.ConfirmationRef{Confirmation: cs[0].ID}}); err != nil {
		t.Fatalf("desktop confirm: %v", err)
	}
}
