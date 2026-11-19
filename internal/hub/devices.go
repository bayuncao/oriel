package hub

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bayuncao/oriel/internal/task"
)

// Device 是配對過的手機（ADR-37、D145）。它是設定，經 Config 存取；身份就是
// 手機的 Noise 靜態公鑰，沒有任何 token。
type Device struct {
	ID       task.DeviceID `json:"id"`
	Name     string        `json:"name"`
	Key      DeviceKey     `json:"key"`
	PairedAt time.Time     `json:"pairedAt"`
	// LastSeen 是最後一次連上或斷開的時間；從沒連過是零值。
	LastSeen time.Time `json:"lastSeen,omitzero"`
}

// DeviceKey 是手機的 Noise 靜態公鑰（即 relay.PublicKey；hub 不導入 relay）。
// 文字形式是 base64url。
type DeviceKey [32]byte

func (k DeviceKey) String() string { return base64.RawURLEncoding.EncodeToString(k[:]) }

func (k DeviceKey) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

func (k *DeviceKey) UnmarshalText(b []byte) error {
	if base64.RawURLEncoding.EncodedLen(len(k)) != len(b) {
		return errors.New("hub: bad device key")
	}
	if _, err := base64.RawURLEncoding.Decode(k[:], b); err != nil {
		return errors.New("hub: bad device key")
	}
	return nil
}

// ID 是這把公鑰的 DeviceID："d-" 加公鑰 SHA-256 的前 16 位 base32（80 位元，
// 不同的公鑰不會撞上）。同一把公鑰
// 再配對是同一台設備（PA08），App 重裝換了密鑰是新設備（PA11）。
func (k DeviceKey) ID() task.DeviceID {
	h := sha256.Sum256(k[:])
	return task.DeviceID("d-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h[:]))[:16])
}

// DeviceNameMax 是設備名稱最多的字數（Unicode 字元）。
const DeviceNameMax = 64

// deviceName 正規化設備名稱：同 Workspace 名稱經 task.Label，另限 DeviceNameMax。
func deviceName(name string) (string, error) {
	n, ok := task.Label(name)
	if !ok || utf8.RuneCountInString(n) > DeviceNameMax {
		return "", fail(CodeInvalidField, nil, "field", "name")
	}
	return n, nil
}

// Devices 是配對過的設備，按配對時間（同刻按 ID）。只有桌面能看（PA09）。
func (h *Hub) Devices(ctx context.Context, p Principal) ([]Device, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	if err := h.needsDesktop(p); err != nil {
		return nil, err
	}
	return h.deviceList(), nil
}

// DeviceByKey 按公鑰找設備；沒登記或已撤銷時 ok 為 false（CN02、CN03）。
// transport/remote 每次握手以 LocalOwner 呼叫，找到了才構造 Principal{Device}。
func (h *Hub) DeviceByKey(ctx context.Context, p Principal, key DeviceKey) (d Device, ok bool, err error) {
	if err := h.alive(); err != nil {
		return Device{}, false, err
	}
	if err := h.needsDesktop(p); err != nil {
		return Device{}, false, err
	}
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	d, ok = h.cfg.devices[key.ID()]
	if !ok || d.Key != key {
		return Device{}, false, nil
	}
	return d, true, nil
}

// PairDevice 登記配對完成的設備（PA03）：transport/remote 驗過一次性密鑰之後
// 以 LocalOwner 呼叫。名稱去首尾空白，不能為空、不能含換行、最多
// DeviceNameMax 字（hub.invalid_field{name}）。同一把公鑰已登記時只更新名稱，
// DeviceID 與配對時間不變（PA08）。
func (h *Hub) PairDevice(ctx context.Context, p Principal, key DeviceKey, name string) (Device, error) {
	if err := h.alive(); err != nil {
		return Device{}, err
	}
	if err := h.needsDesktop(p); err != nil {
		return Device{}, err
	}
	name, err := deviceName(name)
	if err != nil {
		return Device{}, err
	}
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	h.cfg.mu.Lock()
	d, ok := h.cfg.devices[key.ID()]
	h.cfg.mu.Unlock()
	if !ok || d.Key != key {
		d = Device{ID: key.ID(), Key: key, PairedAt: h.now()}
	}
	d.Name = name
	if err := h.putDevice(ctx, d); err != nil {
		return Device{}, err
	}
	return d, nil
}

// RenameDevice 改設備的名稱（DV04），規則同 PairDevice；不在是
// hub.unknown_device{device}。只有桌面能用。
func (h *Hub) RenameDevice(ctx context.Context, p Principal, id task.DeviceID, name string) (Device, error) {
	if err := h.alive(); err != nil {
		return Device{}, err
	}
	if err := h.needsDesktop(p); err != nil {
		return Device{}, err
	}
	name, err := deviceName(name)
	if err != nil {
		return Device{}, err
	}
	return h.updateDevice(ctx, id, func(d *Device) { d.Name = name })
}

// DeviceSeen 記下設備連上或斷開的時間（CN01）。transport/remote 只在連上與
// 斷開時以 LocalOwner 呼叫，不隨每個請求。已撤銷的是 hub.unknown_device{device}。
func (h *Hub) DeviceSeen(ctx context.Context, p Principal, id task.DeviceID) error {
	if err := h.alive(); err != nil {
		return err
	}
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	_, err := h.updateDevice(ctx, id, func(d *Device) { d.LastSeen = h.now() })
	return err
}

// RevokeDevice 撤銷一台設備（CN04、DV05）：從設定刪掉，WatchDevices 的觀察者
// （transport/remote）隨即斷開它。它發起、還在等的 Confirmation 不動，留給
// 桌面（CN09）。不在是 hub.unknown_device{device}。只有桌面能用。
func (h *Hub) RevokeDevice(ctx context.Context, p Principal, id task.DeviceID) error {
	if err := h.alive(); err != nil {
		return err
	}
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	h.cfg.mu.Lock()
	_, ok := h.cfg.devices[id]
	h.cfg.mu.Unlock()
	if !ok {
		return fail(CodeUnknownDevice, nil, "device", string(id))
	}
	if err := h.config.DeleteDevice(ctx, id); err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	delete(h.cfg.devices, id)
	h.cfg.mu.Unlock()
	h.devicesChanged()
	return nil
}

// RevokeDevices 撤銷所有設備：重置遠程時呼叫（CN05、DV06），密鑰與房間由
// transport/remote 換新。只有桌面能用。
func (h *Hub) RevokeDevices(ctx context.Context, p Principal) error {
	if err := h.alive(); err != nil {
		return err
	}
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	if err := h.config.ClearDevices(ctx); err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	clear(h.cfg.devices)
	h.cfg.mu.Unlock()
	h.devicesChanged()
	return nil
}

// WatchDevices 推送設備列表：先給目前的全集，之後每次變化（配對、改名、
// 最後在線、撤銷）再給一次全集；接連的變化可能合成一次。桌面的列表與
// transport/remote（斷開被撤銷的設備）用它。sink 在呼叫者的 goroutine 上
// 執行；ctx 結束回 nil，sink 出錯原樣返回，Close 回 hub.closed。只有桌面能用。
func (h *Hub) WatchDevices(ctx context.Context, p Principal, sink func([]Device) error) error {
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	w := &deviceWatcher{ch: make(chan struct{}, 1)}
	// 登記與 closed 檢查在同一把鎖下：Close 遍歷觀察者也拿這把鎖。
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	h.deviceWatchers[w] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.deviceWatchers, w)
		h.mu.Unlock()
	}()
	// 登記後才取列表，不會漏掉之間的變化（可能多推一次）。
	for {
		if err := sink(h.deviceList()); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-w.ch:
			if !ok {
				return ErrClosed
			}
		}
	}
}

// deviceWatcher 是一個設備列表的觀察者。ch 只是「有變化」的訊號，容量 1：
// 還沒處理的變化合成一次，觀察者永遠拿全集，所以不必踢出。Close 關閉 ch。
type deviceWatcher struct {
	ch chan struct{}
}

// devicesChanged 通知觀察者；絕不阻塞。
func (h *Hub) devicesChanged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for w := range h.deviceWatchers {
		select {
		case w.ch <- struct{}{}:
		default: // 已有一個沒處理的訊號
		}
	}
}

// updateDevice 在 cfgWrite 下改一台設備並寫入。
func (h *Hub) updateDevice(ctx context.Context, id task.DeviceID, change func(*Device)) (Device, error) {
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	h.cfg.mu.Lock()
	d, ok := h.cfg.devices[id]
	h.cfg.mu.Unlock()
	if !ok {
		return Device{}, fail(CodeUnknownDevice, nil, "device", string(id))
	}
	change(&d)
	if err := h.putDevice(ctx, d); err != nil {
		return Device{}, err
	}
	return d, nil
}

// putDevice 寫入、更新快取並通知觀察者。持有 cfgWrite。
func (h *Hub) putDevice(ctx context.Context, d Device) error {
	if err := h.config.PutDevice(ctx, d); err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	h.cfg.devices[d.ID] = d
	h.cfg.mu.Unlock()
	h.devicesChanged()
	return nil
}

func (h *Hub) deviceList() []Device {
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	return sortedDevices(h.cfg.devices)
}

func sortedDevices(m map[task.DeviceID]Device) []Device {
	return slices.SortedFunc(maps.Values(m), func(a, b Device) int {
		return cmp.Or(a.PairedAt.Compare(b.PairedAt), cmp.Compare(a.ID, b.ID))
	})
}
