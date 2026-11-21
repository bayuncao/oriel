package mygoipc

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/internal/transport/remote"
)

// Remote 是 設置 › 遠程 的 IPC 服務（BindAs "Remote"，D145，remote.md DV）：開關、中繼
// 地址、連線狀態、配對連結與設備列表，都轉給 remote.Control。每個渠道都綁它，所以生成
// 的 src/mygo.ts 不隨渠道而變（DV19）；沒有遠程的構建由 DisableRemote 停用它。
type Remote struct {
	late[*remote.Control]
	off atomic.Bool
}

// ConnectRemote 給 Remote 服務接上後端（D61：包級函式，不是頁面可呼叫的方法）。
func ConnectRemote(s *Remote, c *remote.Control, log *slog.Logger) { s.connect(c, log) }

// DisableRemote 標明這個構建沒有遠程（正式版，D149）：之後每個方法立即回
// mygoipc.remote_unavailable，不回 mygoipc.not_ready，頁面不會當作還在啟動而一直重試（DV18）。
func DisableRemote(s *Remote) { s.off.Store(true) }

// get 是 late.get，停用時改回 mygoipc.remote_unavailable。
func (s *Remote) get() (*remote.Control, *slog.Logger, *transport.Error) {
	if s.off.Load() {
		e, _ := transport.Wire(errRemoteUnavailable)
		return nil, nil, e
	}
	return s.late.get()
}

// Status 回目前的開關、中繼地址與連線狀態。
func (s *Remote) Status() (remote.Status, error) {
	c, _, e := s.get()
	if e != nil {
		return remote.Status{}, e
	}
	return c.Status(), nil
}

// Watch 先推目前的狀態，之後每次變化推一次；頁面離開或應用退出時結束。
func (s *Remote) Watch(ctx context.Context, states *mygo.Channel[remote.Status]) error {
	c, log, e := s.get()
	if e != nil {
		return e
	}
	if err := pageLeft(ctx, c.Watch(ctx, states.Send)); err != nil {
		return wire(log, "Watch", err)
	}
	return nil
}

// SetOn 打開或關掉遠程（DV02、DV07、DV11）。
func (s *Remote) SetOn(on bool) error {
	c, log, e := s.get()
	if e != nil {
		return e
	}
	if err := c.SetOn(on); err != nil {
		return wire(log, "SetOn", err)
	}
	return nil
}

// SetRelay 換中繼地址；空字串清掉（DV08、DV15、DV16）。
func (s *Remote) SetRelay(relay string) error {
	c, log, e := s.get()
	if e != nil {
		return e
	}
	if err := c.SetRelay(relay); err != nil {
		return wire(log, "SetRelay", err)
	}
	return nil
}

// Retry 立即重連（DV10）。
func (s *Remote) Retry() error {
	c, _, e := s.get()
	if e != nil {
		return e
	}
	c.Retry()
	return nil
}

// Offer 產生新的配對碼，回二維碼的內容與期限（PA02、DV13）。
func (s *Remote) Offer() (remote.PairingLink, error) {
	c, log, e := s.get()
	if e != nil {
		return remote.PairingLink{}, e
	}
	l, err := c.Offer()
	if err != nil {
		return remote.PairingLink{}, wire(log, "Offer", err)
	}
	return l, nil
}

// CancelOffer 作廢顯示中的配對碼（關掉了二維碼）。
func (s *Remote) CancelOffer() error {
	c, _, e := s.get()
	if e != nil {
		return e
	}
	c.CancelOffer()
	return nil
}

// Reset 重置遠程：換掉房間與密鑰，撤銷所有設備（DV06）。
func (s *Remote) Reset(ctx context.Context) error {
	c, log, e := s.get()
	if e != nil {
		return e
	}
	if err := c.Reset(ctx); err != nil {
		return wire(log, "Reset", err)
	}
	return nil
}

// WatchDevices 先推設備列表，之後設備或在線的設備變了就再推（DV03、DV14）。
func (s *Remote) WatchDevices(ctx context.Context, rows *mygo.Channel[[]remote.DeviceRow]) error {
	c, log, e := s.get()
	if e != nil {
		return e
	}
	if err := pageLeft(ctx, c.WatchDevices(ctx, rows.Send)); err != nil {
		return wire(log, "WatchDevices", err)
	}
	return nil
}

// Rename 改設備的名字（DV04）。
func (s *Remote) Rename(ctx context.Context, id task.DeviceID, name string) (hub.Device, error) {
	c, log, e := s.get()
	if e != nil {
		return hub.Device{}, e
	}
	d, err := c.Rename(ctx, id, name)
	if err != nil {
		return hub.Device{}, wire(log, "Rename", err)
	}
	return d, nil
}

// Revoke 撤銷設備，它立即斷開（DV05）。
func (s *Remote) Revoke(ctx context.Context, id task.DeviceID) error {
	c, log, e := s.get()
	if e != nil {
		return e
	}
	if err := c.Revoke(ctx, id); err != nil {
		return wire(log, "Revoke", err)
	}
	return nil
}
